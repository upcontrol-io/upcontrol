// Package incident implements the incident lifecycle (plan §5.8). It connects
// the availability detector's Open/Close outcomes to the incident table and the
// incident_update timeline.
//
// Open: insert a new incident row + an "opened" timeline entry. Deduplicates by
//
//	checking for an already-open incident on the same monitor.
//
// Close: update the incident's resolved_at + close_reason + a "resolved" entry.
//
//	The four marks (detected_at, notified_at, acked_at, resolved_at) give
//	MTTD/MTTA/MTTR.
//
// Phase 1 of the frozen log slice runs at open (freezeSlice, via
// ring.QueryBuilder — the only permitted path to the logs table). Phase 2 at
// +15m and the deploy join-at-open are still TODO: they need the events table
// and a scheduled second pass in ucworker.
package incident

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	sqlc "go.upcontrol.io/back/gen/pg"
	notifysettings "go.upcontrol.io/back/internal/channel/notify"
	"go.upcontrol.io/back/internal/ring/query"
	"go.upcontrol.io/back/internal/storage/ch"
	"go.upcontrol.io/back/internal/storage/pg"
)

// CloseReason enumerates the incident close reasons (plan §5.8).
const (
	ReasonRecovered     = "recovered"
	ReasonMaintenance   = "maintenance"
	ReasonMonitorDelete = "monitor_deleted"
	ReasonByHuman       = "by_human"
	ReasonAbsorbed      = "absorbed"
	ReasonDetectorOff   = "detector_off"
)

// Lifecycle manages incident open/close against the Postgres tables.
type Lifecycle struct {
	pool *pg.Pool
	// ch is optional: without it the incident still opens, it just carries no
	// frozen log slice. The probe path always has one; tests do not.
	ch *ch.Conn
}

// New builds a Lifecycle bound to a pg.Pool and (optionally) ClickHouse, which
// is what the frozen slice is read from.
func New(p *pg.Pool, chConn *ch.Conn) *Lifecycle { return &Lifecycle{pool: p, ch: chConn} }

// Open creates an incident for a monitor that just crossed the availability
// threshold. It deduplicates: if an incident is already open for this monitor,
// it returns that incident's ID without creating a duplicate.
func (l *Lifecycle) Open(ctx context.Context, monitorID int64, title string) (incidentID int64, created bool, err error) {
	q := l.pool.Queries()

	// Check for an already-open incident.
	if existing, e := q.GetOpenIncident(ctx, &monitorID); e == nil {
		return existing.ID, false, nil
	}

	// Get the monitor's tenant/project for the incident row.
	mon, e := q.GetMonitorForIncident(ctx, monitorID)
	if e != nil {
		return 0, false, fmt.Errorf("incident: monitor %d: %w", monitorID, e)
	}

	// Fingerprint: hash(monitor_id + "availability") — repeated outages of the
	// same monitor share a fingerprint for grouping in the history.
	fp := int64(fingerprint(monitorID, "availability"))

	row, err := q.OpenIncident(ctx, sqlc.OpenIncidentParams{
		PublicID:    newUUID(),
		TenantID:    mon.TenantID,
		ProjectID:   mon.ProjectID,
		MonitorID:   &monitorID,
		Detector:    "availability",
		Fingerprint: fp,
		Title:       title,
	})
	if err != nil {
		return 0, false, fmt.Errorf("incident: open: %w", err)
	}

	// Timeline entry: "opened". The text describes the event, not the incident —
	// the card already carries the title in its header, and an entry repeating it
	// verbatim is a line that tells the reader nothing they have not just read.
	_ = q.AddIncidentUpdate(ctx, sqlc.AddIncidentUpdateParams{
		IncidentID: row.ID,
		Kind:       "opened",
		Text:       mon.Target + " stopped responding",
	})

	// Freeze the log slice while the lines are still inside the ring window
	// (§5.8 phase 1). Best effort — a project that sends no logs simply has none,
	// and a ClickHouse hiccup must not block the incident or its alert — but the
	// error is returned rather than swallowed inside, so a caller with a logger
	// can say why a card came back without evidence.
	_ = l.freezeSlice(ctx, row.ID, mon.TenantID, mon.ProjectID)

	// Notify every connected channel (§5.8) whose settings still ask for the
	// page — websiteDown defaults on, so a channel that never opened its
	// settings behaves exactly as before. EnqueueDelivery dedupes on idem_key,
	// so replaying the open is a no-op — this is the row the delivery worker
	// (ucworker) picks up. Without it an open incident never reaches
	// delivery_queue (block 2: EnqueueDelivery was never called).
	if chans, cerr := q.ListChannelsByTenant(ctx, mon.TenantID); cerr == nil {
		// The follow-up is PAID ONLY (every paid plan): the stored flag alone
		// does not schedule one — a tenant that downgraded keeps the setting
		// but stops getting what it bought.
		plan, _ := q.GetTenantPlan(ctx, mon.TenantID)
		for _, ch := range chans {
			settings := notifysettings.Resolve(ch.Notify)
			if !settings.WebsiteDown {
				continue
			}
			payload, _ := json.Marshal(map[string]any{
				"title":        title,
				"status":       "down",
				"incident_id":  uuidStr(row.PublicID),
				"monitor_name": mon.Name,
			})
			_ = q.EnqueueDelivery(ctx, sqlc.EnqueueDeliveryParams{
				TenantID:   mon.TenantID,
				IncidentID: &row.ID,
				ChannelID:  ch.ID,
				IdemKey:    fmt.Sprintf("incident:%d:channel:%d:opened", row.ID, ch.ID),
				Class:      "page",
				Payload:    payload,
			})
			// The 15-minute resolve follow-up: enqueued now with next_try_at in
			// the future, COMPOSED at send time from the incident's then-current
			// state (deliver.Worker, class "followup") — recovered → "back up",
			// still open → "still down". Either way, so the reader knows whether
			// to keep running for a laptop.
			if settings.ResolveFollowUp && plan != "" && plan != "Free" {
				fuPayload, _ := json.Marshal(map[string]any{
					"incident_id":  uuidStr(row.PublicID),
					"monitor_name": mon.Name,
				})
				_ = q.EnqueueDeliveryAt(ctx, sqlc.EnqueueDeliveryAtParams{
					TenantID:   mon.TenantID,
					IncidentID: &row.ID,
					ChannelID:  ch.ID,
					IdemKey:    fmt.Sprintf("incident:%d:channel:%d:followup", row.ID, ch.ID),
					Class:      "followup",
					Payload:    fuPayload,
					NextTryAt:  pgtype.Timestamptz{Time: time.Now().Add(notifysettings.FollowUpDelay), Valid: true},
				})
			}
		}
		_ = q.TouchIncidentNotified(ctx, row.ID)
	}

	return row.ID, true, nil
}

// Close resolves the open incident for a monitor with the given reason.
func (l *Lifecycle) Close(ctx context.Context, monitorID int64, reason string) error {
	q := l.pool.Queries()

	// Find the open incident to add a timeline entry.
	existing, err := q.GetOpenIncident(ctx, &monitorID)
	if err != nil {
		return nil // no open incident — nothing to close
	}

	reasonPtr := reason
	if err := q.CloseIncident(ctx, sqlc.CloseIncidentParams{
		CloseReason: &reasonPtr,
		MonitorID:   &monitorID,
	}); err != nil {
		return fmt.Errorf("incident: close: %w", err)
	}

	// Timeline entry: "resolved". Same rule as "opened": say what happened.
	text := "Closed: " + reason
	if reason == ReasonRecovered {
		text = "Checks are passing again"
	}
	_ = q.AddIncidentUpdate(ctx, sqlc.AddIncidentUpdateParams{
		IncidentID: existing.ID,
		Kind:       "resolved",
		Text:       text,
	})

	return nil
}

// DetectOpen is the project-scoped opener's payload (D1): detection keys
// incidents by a fingerprint over the detected key, not by a monitor —
// MonitorID is nil for every v1 detector (ErrorRate is project-scoped).
type DetectOpen struct {
	TenantID    int64
	ProjectID   int64
	MonitorID   *int64
	Detector    string
	Fingerprint int64
	Title       string
	OpenedText  string
}

// OpenDetect opens a project-scoped incident for a detector verdict. Unlike
// Open it enqueues NO deliveries and never touches notified_at (D4): detection
// incidents are dashboard-visible only — the error-log scanner already alerts
// on error spikes, and a second notification class would be a second product.
// Deduplication is by fingerprint: an already-open incident is returned as-is.
func (l *Lifecycle) OpenDetect(ctx context.Context, p DetectOpen) (incidentID int64, created bool, err error) {
	q := l.pool.Queries()

	// The verdict re-fires on every tick; only the first one lands.
	existing, err := q.GetOpenIncidentByFingerprint(ctx, sqlc.GetOpenIncidentByFingerprintParams{
		TenantID:    p.TenantID,
		Fingerprint: p.Fingerprint,
	})
	if err == nil {
		return existing.ID, false, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return 0, false, fmt.Errorf("incident: detect lookup: %w", err)
	}

	row, err := q.OpenIncident(ctx, sqlc.OpenIncidentParams{
		PublicID:    newUUID(),
		TenantID:    p.TenantID,
		ProjectID:   p.ProjectID,
		MonitorID:   p.MonitorID,
		Detector:    p.Detector,
		Fingerprint: p.Fingerprint,
		Title:       p.Title,
	})
	if err != nil {
		return 0, false, fmt.Errorf("incident: detect open: %w", err)
	}

	// Timeline: the detector's own reason for firing (D13) — what the z-score
	// actually said, not a generic "spike detected" line.
	_ = q.AddIncidentUpdate(ctx, sqlc.AddIncidentUpdateParams{
		IncidentID: row.ID,
		Kind:       "opened",
		Text:       p.OpenedText,
	})

	// Same frozen-slice reasoning as Open, best-effort for the same reasons.
	_ = l.freezeSlice(ctx, row.ID, p.TenantID, p.ProjectID)

	return row.ID, true, nil
}

// CloseByFingerprint resolves the open incident behind a detector key. The
// resolvedText is the caller's to word (D12): the availability close's "Checks
// are passing again" would be a lie for a log-driven incident. Unlike Close,
// a lookup failure other than "nothing open" is returned, not swallowed —
// a broken close must be heard.
func (l *Lifecycle) CloseByFingerprint(ctx context.Context, tenantID, fp int64, reason, resolvedText string) error {
	q := l.pool.Queries()

	existing, err := q.GetOpenIncidentByFingerprint(ctx, sqlc.GetOpenIncidentByFingerprintParams{
		TenantID:    tenantID,
		Fingerprint: fp,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil // no open incident — nothing to close
		}
		return fmt.Errorf("incident: detect lookup: %w", err)
	}

	if err := q.CloseIncidentByFingerprint(ctx, sqlc.CloseIncidentByFingerprintParams{
		CloseReason: &reason,
		TenantID:    tenantID,
		Fingerprint: fp,
	}); err != nil {
		return fmt.Errorf("incident: detect close: %w", err)
	}

	_ = q.AddIncidentUpdate(ctx, sqlc.AddIncidentUpdateParams{
		IncidentID: existing.ID,
		Kind:       "resolved",
		Text:       resolvedText,
	})

	return nil
}

// MarkNotified sets the notified_at timestamp when the first alert is sent.
func (l *Lifecycle) MarkNotified(ctx context.Context, incidentID int64) error {
	return l.pool.Queries().TouchIncidentNotified(ctx, incidentID)
}

// sliceLines is how many log lines the frozen slice keeps. The card shows a
// handful and says "trimmed"; more than this is an explorer, which this product
// deliberately is not.
const sliceLines = 12

// freezeSlice copies the tail of the project's visible log window into
// incident_slice. It reads through ring.QueryBuilder because that is the only
// permitted path to the logs table (invariant 4, enforced by depguard), and it
// runs at open time because the ring displaces lines: an incident read tomorrow
// must still show the lines that were on screen when it fired.
func (l *Lifecycle) freezeSlice(ctx context.Context, incidentID, tenantID, projectID int64) error {
	if l.ch == nil {
		return nil
	}
	q := l.pool.Queries()
	var cutoff int64
	if win, werr := q.GetProjectWindowInfo(ctx, projectID); werr == nil {
		cutoff = win.CutoffSeq
	}
	// The tail of the visible window IS the slice: those are the lines that were
	// on screen when the incident fired. Deliberately not a [from, to) seq range
	// derived from project_seq — the allocator hands out blocks and its counter
	// is not a row count, so arithmetic on it points at seqs that never existed.
	qb := query.New(tenantID, projectID, cutoff)
	// No range: the incident's slice is bounded by seq, not by the clock.
	// Evidence, not the bare tail: the failing lines fill the budget first and
	// ordinary traffic tops up the rest, so an incident on a busy project stops
	// freezing twelve lines of healthy traffic around one failure.
	lq := qb.Evidence(sliceLines)
	rows, err := l.ch.Raw().Query(ctx, lq.SQL, lq.Args...)
	if err != nil {
		return fmt.Errorf("incident: slice query: %w", err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		// seq is UInt64 in ClickHouse and bigint in Postgres; the driver will not
		// scan it into an int64, and a silent scan error here is how the slice
		// came back empty the first time.
		var seq uint64
		var ts time.Time
		var level, service, message string
		if err := rows.Scan(&seq, &ts, &level, &service, &message); err != nil {
			return fmt.Errorf("incident: slice scan: %w", err)
		}
		_ = q.AddIncidentSliceLine(ctx, sqlc.AddIncidentSliceLineParams{
			IncidentID: incidentID,
			Seq:        int64(seq),
			Ts:         pgtype.Timestamptz{Time: ts, Valid: true},
			Level:      level,
			Service:    service,
			Message:    message,
		})
	}
	return rows.Err()
}

// KeyFingerprint is the exported, key-based sibling of fingerprint: the same
// fnv64a family over a caller-composed key ("project:<id>:errorrate") instead
// of (monitorID, detector). Detection keys are not monitor IDs, but grouping
// needs the same thing — one stable int64 per detected thing.
func KeyFingerprint(key string) int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(key))
	return int64(h.Sum64())
}

// fingerprint computes a stable int64 hash for incident grouping.
func fingerprint(monitorID int64, detector string) uint64 {
	h := fnv.New64a()
	_, _ = fmt.Fprintf(h, "%d:%s", monitorID, detector)
	return h.Sum64()
}

// newUUID generates a v4 UUID for the incident's public_id.
func newUUID() pgtype.UUID {
	var u [16]byte
	_, _ = rand.Read(u[:])
	u[6] = (u[6] & 0x0f) | 0x40
	u[8] = (u[8] & 0x3f) | 0x80
	return pgtype.UUID{Bytes: u, Valid: true}
}

// uuidStr renders a pgtype.UUID as lowercase hex without dashes (matches the
// public_id format used across the API).
func uuidStr(u pgtype.UUID) string { return fmt.Sprintf("%x", u.Bytes[:]) }
