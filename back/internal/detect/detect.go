// Package detect is the orchestrator that turns the pure detection core
// (detectors + suppression) into incidents (docs/plans/detect-errorrate-v1.md).
// Every ucworker tick it walks the projects, reads the ClickHouse rollups
// through the storage layer (invariants 2/4: aggregates only, never raw logs),
// asks the ErrorRate detector for a Decision, runs suppression on a fire, and
// opens or closes a fingerprint-keyed incident accordingly.
//
// v1 wires ErrorRate only (D3). The thresholds live in the detector code; this
// package owns the plumbing — windows, baselines, the decision-to-action map,
// and the per-project error isolation (one project's ClickHouse hiccup must
// not stop the next project's scan).
package detect

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"

	sqlc "go.upcontrol.io/back/gen/pg"
	"go.upcontrol.io/back/internal/deliver"
	detector "go.upcontrol.io/back/internal/detect/detectors"
	"go.upcontrol.io/back/internal/detect/suppression"
	"go.upcontrol.io/back/internal/incident"
	"go.upcontrol.io/back/internal/storage/ch"
	"go.upcontrol.io/back/internal/storage/pg"
)

// madScale converts a MAD into a standard-deviation equivalent for the
// detector's z-score. Fixed by plan: no env knobs (D8).
const madScale = 1.4826

// scanWindow is the span windowBounds scores, named because the alert says it
// out loud ("in the last 5 minutes") — the sentence and the query must not be
// able to drift apart.
const scanWindow = 5 * time.Minute

// Scanner wires the pure decisions to Postgres + ClickHouse. One
// implementation, no interface (D11): the mock would be a second product.
type Scanner struct {
	pool *pg.Pool
	ch   *ch.Conn
	inc  *incident.Lifecycle
	log  *slog.Logger
}

// New builds a Scanner.
func New(pool *pg.Pool, chConn *ch.Conn, lc *incident.Lifecycle, log *slog.Logger) *Scanner {
	return &Scanner{pool: pool, ch: chConn, inc: lc, log: log}
}

// windowBounds is the current window the ErrorRate detector scores: the last
// five completed minutes ([to-5m, to), to = now truncated to the minute).
// Recovery uses the same window (D7): five clean minutes close the incident,
// so the window IS the smoothing — no separate M constant.
func windowBounds(now time.Time) (from, to time.Time) {
	to = now.Truncate(time.Minute)
	return to.Add(-scanWindow), to
}

// baselineBounds is the week of history the MAD baseline is computed over,
// ending exactly where the current window begins — no overlap, so the spike
// being scored never inflates its own baseline.
func baselineBounds(now time.Time) (from, to time.Time) {
	wFrom, _ := windowBounds(now)
	return wFrom.Add(-7 * 24 * time.Hour), wFrom
}

// errorRateSummary is the alert's one measured sentence. The baseline clause
// is the half that can lie: a project younger than a week has no history,
// ErrorRateBaseline reports that as (0, 0), and the detector falls back to its
// flat 10% rule. "The weekly baseline is 0.0%" would then assert a measurement
// nobody made, so the baseline is quoted only when the detector actually had
// one to compare against — a MAD above zero is exactly that condition.
func errorRateSummary(errs, total uint64, median, mad float64) string {
	rate := float64(errs) / float64(total)
	s := fmt.Sprintf("Error and fatal lines are %.1f%% of the log stream in the last %d minutes.",
		rate*100, int(scanWindow.Minutes()))
	if mad > 0 {
		s += fmt.Sprintf(" The weekly baseline is %.1f%%.", median*100)
	}
	return s
}

// action is what a tick must do with a project after decide().
type action int

const (
	actNone  action = iota // no fire, nothing open — leave it alone
	actOpen                // fire survived suppression — open an incident
	actClose               // no fire while an incident is open — recover it
)

// decide maps (Decision, suppression, openness) to the act. Pure: every
// combination is unit-tested, including the ones the live loop cannot produce
// (an unsuppressed fire with an incident already open — dedup makes that
// unreachable in Tick, but decide must not care).
func decide(dec detector.Decision, suppressed, hasOpen bool) action {
	if dec.Fire && !suppressed {
		return actOpen
	}
	if !dec.Fire && hasOpen {
		return actClose
	}
	return actNone
}

// Tick runs one pass over every project. The project list itself failing is a
// tick-level error (returned); any per-project failure is logged and the loop
// CONTINUES — detection is best-effort per project, and one broken project
// must not blind the rest.
func (s *Scanner) Tick(ctx context.Context) error {
	projects, err := s.pool.Queries().ListProjectsForDetect(ctx)
	if err != nil {
		return fmt.Errorf("detect: list projects: %w", err)
	}
	for _, proj := range projects {
		if err := s.scanProject(ctx, proj, time.Now()); err != nil {
			s.log.Warn("detect: project scan failed", "project_id", proj.ID, "err", err)
		}
	}
	return nil
}

// scanProject runs the detect loop for one project: window → decision →
// suppression → act. The fingerprint is stable per (project, detector), so
// open, cooldown and recovery all key the same incident.
func (s *Scanner) scanProject(ctx context.Context, proj sqlc.ListProjectsForDetectRow, now time.Time) error {
	q := s.pool.Queries()
	fp := incident.KeyFingerprint(fmt.Sprintf("project:%d:errorrate", proj.ID))

	wFrom, wTo := windowBounds(now)
	errs, total, err := s.ch.ErrorWindow(ctx, proj.TenantID, proj.ID, wFrom, wTo)
	if err != nil {
		return fmt.Errorf("error window: %w", err)
	}

	open := false
	if _, err := q.GetOpenIncidentByFingerprint(ctx, sqlc.GetOpenIncidentByFingerprintParams{
		TenantID:    proj.TenantID,
		Fingerprint: fp,
	}); err == nil {
		open = true
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("open incident lookup: %w", err)
	}

	// Under the total>=10 floor there is nothing to score — and no reason to
	// pay the two baseline queries either (the detector would NoFire anyway).
	// median and mad outlive the branch because the alert quotes them.
	var dec detector.Decision
	var median, mad float64
	if total < 10 {
		dec = detector.NoFire()
	} else {
		bFrom, bTo := baselineBounds(now)
		m, d, berr := s.ch.ErrorRateBaseline(ctx, proj.TenantID, proj.ID, bFrom, bTo)
		if berr != nil {
			return fmt.Errorf("error rate baseline: %w", berr)
		}
		median, mad = m, d
		dec = detector.ErrorRate(int(errs), int(total), m, d, madScale)
	}

	// Suppression only matters on a fire (test-pinned order: post-deploy → maintenance
	// → dedup → cooldown). A suppressed fire is logged, never extended.
	suppressed := false
	if dec.Fire {
		var lastFire time.Time
		if st, err := q.GetErrorAlertState(ctx, sqlc.GetErrorAlertStateParams{
			TenantID:    proj.TenantID,
			Fingerprint: fp,
			Kind:        "detect:errorrate",
		}); err == nil {
			lastFire = st.Time
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("alert state lookup: %w", err)
		}
		lastDeploy, err := s.ch.LastDeployAt(ctx, proj.TenantID, proj.ID)
		if err != nil {
			return fmt.Errorf("last deploy: %w", err)
		}
		sup := suppression.Evaluate(suppression.Input{
			Fingerprint:     fp,
			LastFireAt:      lastFire,
			HasOpenIncident: open,
			InMaintenance:   false,
			LastDeployAt:    lastDeploy,
			Now:             now,
		})
		if sup.Suppress {
			suppressed = true
			s.log.Info("detect: fire suppressed", "project_id", proj.ID, "reason", sup.Reason)
		}
	}

	switch decide(dec, suppressed, open) {
	case actOpen:
		_, created, err := s.inc.OpenDetect(ctx, incident.DetectOpen{
			TenantID:    proj.TenantID,
			ProjectID:   proj.ID,
			MonitorID:   nil,
			Detector:    "errorrate",
			Fingerprint: fp,
			Title:       "Error rate spike on " + proj.Domain,
			OpenedText:  dec.Reason,
			Summary:     errorRateSummary(errs, total, median, mad),
			Fields: []deliver.Field{
				{Label: "Detected", Value: now.UTC().Format("2 Jan 2006, 15:04") + " UTC"},
				{Label: "Error lines", Value: fmt.Sprintf("%d of %d lines", errs, total)},
				// Not "Deviation": with no baseline the detector never computes
				// one, it fires on a flat threshold, and dec.Reason then says
				// so. One label that is true of both rules.
				{Label: "Why it fired", Value: dec.Reason},
			},
		})
		if err != nil {
			return fmt.Errorf("open detect incident: %w", err)
		}
		// D6: the cooldown starts only when an incident actually opened. A
		// suppressed fire never reaches this line, so it cannot extend the
		// cooldown it is being suppressed by.
		if created {
			if err := q.UpsertErrorAlertState(ctx, sqlc.UpsertErrorAlertStateParams{
				TenantID:    proj.TenantID,
				Fingerprint: fp,
				Kind:        "detect:errorrate",
			}); err != nil {
				return fmt.Errorf("upsert alert state: %w", err)
			}
		}
	case actClose:
		if err := s.inc.CloseByFingerprint(ctx, proj.TenantID, fp,
			incident.ReasonRecovered, "Error rate back to normal"); err != nil {
			return fmt.Errorf("close detect incident: %w", err)
		}
	}
	return nil
}
