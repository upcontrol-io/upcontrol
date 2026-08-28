// Package detect turns the pure detection core (detectors + suppression) into
// incidents; per-project errors are isolated so one cannot blind the rest.
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
	"go.upcontrol.io/back/internal/storage/pg"
	"go.upcontrol.io/back/internal/storage/pgstore"
)

// madScale converts a MAD into a standard-deviation equivalent for the
// detector's z-score; fixed, no env knobs.
const madScale = 1.4826

// scanWindow is the span windowBounds scores; the alert says it out loud
// ("in the last 5 minutes"), so the sentence and the query must not drift.
const scanWindow = 5 * time.Minute

// Scanner wires the pure decisions to Postgres + its telemetry store; one
// implementation, no interface (the mock would be a second product).
type Scanner struct {
	pool *pg.Pool
	pgs  *pgstore.Store
	inc  *incident.Lifecycle
	log  *slog.Logger
}

func New(pool *pg.Pool, pgs *pgstore.Store, lc *incident.Lifecycle, log *slog.Logger) *Scanner {
	return &Scanner{pool: pool, pgs: pgs, inc: lc, log: log}
}

// windowBounds is the window the ErrorRate detector scores: [to-5m, to) with
// to = now truncated to the minute. Recovery uses the same window.
func windowBounds(now time.Time) (from, to time.Time) {
	to = now.Truncate(time.Minute)
	return to.Add(-scanWindow), to
}

// baselineBounds is the week of history the MAD baseline is computed over,
// ending where the current window begins so the spike never inflates it.
func baselineBounds(now time.Time) (from, to time.Time) {
	wFrom, _ := windowBounds(now)
	return wFrom.Add(-7 * 24 * time.Hour), wFrom
}

// errorRateSummary is the alert's one measured sentence; the baseline is
// quoted only when the detector had one (a MAD above zero is that condition).
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

// decide maps (Decision, suppression, openness) to the act; pure, so every
// combination is unit-tested, including ones the live loop cannot produce.
func decide(dec detector.Decision, suppressed, hasOpen bool) action {
	if dec.Fire && !suppressed {
		return actOpen
	}
	if !dec.Fire && hasOpen {
		return actClose
	}
	return actNone
}

// Tick runs one pass over every project; a per-project failure is logged and
// the loop continues; one broken project must not blind the rest.
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
// suppression → act; the fingerprint is stable per (project, detector).
func (s *Scanner) scanProject(ctx context.Context, proj sqlc.ListProjectsForDetectRow, now time.Time) error {
	q := s.pool.Queries()
	fp := incident.KeyFingerprint(fmt.Sprintf("project:%d:errorrate", proj.ID))

	wFrom, wTo := windowBounds(now)
	errs, total, err := s.pgs.ErrorWindow(ctx, proj.TenantID, proj.ID, wFrom, wTo)
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

	// Under the total>=10 floor there is nothing to score and no reason to pay
	// the two baseline queries; median and mad outlive the branch (the alert quotes them).
	var dec detector.Decision
	var median, mad float64
	if total < 10 {
		dec = detector.NoFire()
	} else {
		bFrom, bTo := baselineBounds(now)
		m, d, berr := s.pgs.ErrorRateBaseline(ctx, proj.TenantID, proj.ID, bFrom, bTo)
		if berr != nil {
			return fmt.Errorf("error rate baseline: %w", berr)
		}
		median, mad = m, d
		dec = detector.ErrorRate(int(errs), int(total), m, d, madScale)
	}

	// Suppression only matters on a fire (pinned order: post-deploy →
	// maintenance → dedup → cooldown); a suppressed fire is logged, never extended.
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
		lastDeploy, err := s.pgs.LastDeployAt(ctx, proj.TenantID, proj.ID)
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
				// With no baseline the detector fires on a flat threshold and
				// dec.Reason says so; one label true of both rules.
				{Label: "Why it fired", Value: dec.Reason},
			},
		})
		if err != nil {
			return fmt.Errorf("open detect incident: %w", err)
		}
		// The cooldown starts only when an incident actually opened; a
		// suppressed fire never reaches here, so it extends no cooldown.
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
