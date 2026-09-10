// Package incident implements the incident lifecycle: Open/Close against the
// incident table and its timeline; the four marks give MTTD/MTTA/MTTR.
package incident

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	sqlc "go.upcontrol.io/back/gen/pg"
	notifysettings "go.upcontrol.io/back/internal/channel/notify"
	"go.upcontrol.io/back/internal/deliver"
	"go.upcontrol.io/back/internal/ring/query"
	"go.upcontrol.io/back/internal/storage/pg"
	"go.upcontrol.io/back/internal/storage/pgstore"
)

// detectStatus is what a detection incident opens as, on the row AND in the
// alert; not "down" (a log-stream detector has not earned the down verdict).
const detectStatus = "check"

// CloseReason enumerates the incident close reasons.
const (
	ReasonRecovered     = "recovered"
	ReasonMonitorDelete = "monitor_deleted"
	ReasonByHuman       = "by_human"
)

// Lifecycle manages incident open/close against the Postgres tables.
type Lifecycle struct {
	pool *pg.Pool
	// pgs is optional: without it the incident still opens, it just carries no
	// frozen log slice. The probe path always has one; tests do not.
	pgs *pgstore.Store
}

// New builds a Lifecycle; pgs is optional (no store, no frozen slice).
func New(p *pg.Pool, pgs *pgstore.Store) *Lifecycle { return &Lifecycle{pool: p, pgs: pgs} }

// Open creates an incident for a monitor that crossed the availability
// threshold; an already-open incident is returned, not duplicated.
// effectiveIntervalSec is the cadence the target was checked at when the
// incident fired (review decision 14: the interval fold is recorded, not
// undone); 0 means unknown/legacy and writes nothing.
func (l *Lifecycle) Open(ctx context.Context, monitorID int64, title string, effectiveIntervalSec int32) (incidentID int64, created bool, err error) {
	return l.open(ctx, l.pool.Queries(), monitorID, title, effectiveIntervalSec, true)
}

// OpenOnTx is Open on a caller-owned transaction: the subscription and its
// first incident land atomically, so a subscriber who joins an outage gets
// its incident with the insert, not one check later. The frozen log slice is
// NOT taken here: it is written through the pool, and the incident row is
// invisible to other connections until the caller's transaction commits —
// incident_slice's FK would refuse it. The caller freezes after commit via
// FreezeOpenIncident.
func (l *Lifecycle) OpenOnTx(ctx context.Context, q *sqlc.Queries, monitorID int64, title string, effectiveIntervalSec int32) (incidentID int64, created bool, err error) {
	return l.open(ctx, q, monitorID, title, effectiveIntervalSec, false)
}

// FreezeOpenIncident takes the evidence slice of the monitor's open incident
// — the step OpenOnTx deferred past the commit. No open incident: a no-op.
func (l *Lifecycle) FreezeOpenIncident(ctx context.Context, monitorID int64) {
	if l.pgs == nil {
		return
	}
	existing, err := l.pool.Queries().GetOpenIncident(ctx, &monitorID)
	if err != nil {
		return
	}
	mon, err := l.pool.Queries().GetMonitorForIncident(ctx, monitorID)
	if err != nil {
		return
	}
	_ = l.freezeSlice(ctx, existing.ID, mon.TenantID, mon.ProjectID)
}

func (l *Lifecycle) open(ctx context.Context, q *sqlc.Queries, monitorID int64, title string, effectiveIntervalSec int32, freeze bool) (incidentID int64, created bool, err error) {

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
		// The checks failed, so this one has earned the word.
		Status: "down",
	})
	if err != nil {
		return 0, false, fmt.Errorf("incident: open: %w", err)
	}
	// The effective interval at opening, on the row (NULL for legacy callers
	// that pass 0; the column says what cadence the outage was measured at).
	if effectiveIntervalSec > 0 {
		_ = q.SetIncidentEffectiveInterval(ctx, sqlc.SetIncidentEffectiveIntervalParams{
			ID: row.ID, EffectiveIntervalSec: effectiveIntervalSec,
		})
	}

	// Timeline entry: "opened"; the text describes the event, not the
	// incident (the card header already carries the title). A heartbeat has
	// no target that stopped responding, so the line names the monitor.
	text := mon.Target + " stopped responding"
	if mon.Kind == "heartbeat" {
		text = mon.Name + " has not checked in"
	}
	_ = q.AddIncidentUpdate(ctx, sqlc.AddIncidentUpdateParams{
		IncidentID: row.ID,
		Kind:       "opened",
		Text:       text,
	})

	// Freeze the log slice while the lines are still inside the ring window;
	// best effort (a CH hiccup must not block the incident), error returned.
	// Skipped on the caller-owned-transaction path (see OpenOnTx).
	if freeze {
		_ = l.freezeSlice(ctx, row.ID, mon.TenantID, mon.ProjectID)
	}

	// The facts the row holds, so the alert names what broke; a field the row
	// does not carry is omitted, never sent as blank. Built once, not per channel.
	fields := []deliver.Field{}
	if mon.Target != "" {
		fields = append(fields, deliver.Field{Label: "Target", Value: mon.Target, Mono: true})
	}
	fields = append(fields, deliver.Field{
		Label: "Down since",
		Value: time.Now().UTC().Format("2 Jan 2006, 15:04") + " UTC",
	})
	payload, _ := json.Marshal(map[string]any{
		"title":        title,
		"status":       "down",
		"incident_id":  uuidStr(row.PublicID),
		"monitor_name": mon.Name,
		"fields":       fields,
	})
	fuPayload, _ := json.Marshal(map[string]any{
		"incident_id":  uuidStr(row.PublicID),
		"monitor_name": mon.Name,
	})

	l.notifyChannels(ctx, q, mon.TenantID, mon.ProjectID, notifySpec{
		incidentID: row.ID,
		payload:    payload,
		wants:      func(s notifysettings.Settings) bool { return s.WebsiteDown },
		followup:   fuPayload,
	})

	return row.ID, true, nil
}

// notifySpec is one incident's side of a notification: the payload every
// interested channel receives, and the wants() interest question.
type notifySpec struct {
	incidentID int64
	payload    []byte
	wants      func(notifysettings.Settings) bool
	// followup is the 15-minute resolve follow-up's payload, or nil when this
	// kind of incident has none (a detector has no monitor to report back up).
	followup []byte
}

// notifyChannels enqueues one delivery per interested channel; EnqueueDelivery
// dedupes on idem_key, so replaying an open is a no-op. The Queries argument is
// the caller's transaction when the open rides one: the delivery lands with
// the incident or not at all.
func (l *Lifecycle) notifyChannels(ctx context.Context, q *sqlc.Queries, tenantID, projectID int64, n notifySpec) {
	// A project's incident reaches that project's destinations and nobody
	// else's — a sibling project of the same workspace is not an audience.
	chans, err := q.ListChannelsByProject(ctx, projectID)
	if err != nil {
		return
	}
	// The follow-up is PAID ONLY: the stored flag alone does not schedule one;
	// a downgraded tenant keeps the setting but stops getting what it bought.
	plan := ""
	if n.followup != nil {
		plan, _ = q.GetTenantPlan(ctx, tenantID)
	}

	// The telegram_recipients axis at DELIVERY time (docs/plans/trial-and-
	// freeze.md): adding a recipient is 402-gated, but a downgrade (or a trial's
	// end) leaves more connected than the plan carries, and a seat beyond the
	// limit must not page. One seat per distinct linked person, one per
	// group/channel destination (recipient_person_id NULL) — the channel-set
	// shadow of countTelegramRecipients. Rows arrive created_at-first, so the
	// oldest connections stay audible.
	// ponytail: channel-set approximation of the workspace-wide person count;
	// move to that count per enqueue if a discrepancy is ever observed.
	tgMax := -1
	tgSeats := 0
	tgSeen := make(map[int64]bool)
	sent := 0
	for _, ch := range chans {
		if ch.Kind == "telegram" {
			if tgMax < 0 { // loaded once, on the first telegram channel
				tgMax = 0
				if plan, err := q.GetTenantPlan(ctx, tenantID); err == nil {
					if ent, err := q.GetPlanEntitlement(ctx, plan); err == nil {
						tgMax = int(ent.TelegramRecipients)
					}
				}
			}
			// A person's seat keys on their id (one seat however many
			// channels), a group/channel destination on its own channel id —
			// negative so the two key spaces never collide.
			seat := -ch.ID
			if ch.RecipientPersonID != nil {
				seat = *ch.RecipientPersonID
			}
			if _, seen := tgSeen[seat]; !seen {
				if tgSeats >= tgMax {
					continue // beyond the plan's seats: mute, oldest first kept
				}
				tgSeen[seat] = true
				tgSeats++
			}
		}
		settings := notifysettings.Resolve(ch.Notify)
		if !n.wants(settings) {
			continue
		}
		if err := q.EnqueueDelivery(ctx, sqlc.EnqueueDeliveryParams{
			TenantID:   tenantID,
			IncidentID: &n.incidentID,
			ChannelID:  ch.ID,
			IdemKey:    fmt.Sprintf("incident:%d:channel:%d:opened", n.incidentID, ch.ID),
			Class:      "page",
			Payload:    n.payload,
		}); err == nil {
			sent++
		}
		// The 15-minute follow-up: enqueued now, COMPOSED at send time from the
		// then-current state ("back up" / "still down").
		if n.followup != nil && settings.ResolveFollowUp && plan != "" && plan != "Free" {
			_ = q.EnqueueDeliveryAt(ctx, sqlc.EnqueueDeliveryAtParams{
				TenantID:   tenantID,
				IncidentID: &n.incidentID,
				ChannelID:  ch.ID,
				IdemKey:    fmt.Sprintf("incident:%d:channel:%d:followup", n.incidentID, ch.ID),
				Class:      "followup",
				Payload:    n.followup,
				NextTryAt:  pgtype.Timestamptz{Time: time.Now().Add(notifysettings.FollowUpDelay), Valid: true},
			})
		}
	}
	// The mark says an alert was QUEUED, not delivered (MTTD off it is a queue
	// time), and is written only when a channel actually took one.
	if sent > 0 {
		_ = q.TouchIncidentNotified(ctx, n.incidentID)
	}
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
	switch reason {
	case ReasonRecovered:
		text = "Checks are passing again"
	case ReasonMonitorDelete:
		// The incident is real history and stays; what ended it was the owner
		// removing the check, so the timeline may not imply a recovery.
		text = "Monitor deleted"
	}
	_ = q.AddIncidentUpdate(ctx, sqlc.AddIncidentUpdateParams{
		IncidentID: existing.ID,
		Kind:       "resolved",
		Text:       text,
	})

	return nil
}

// DetectOpen is the project-scoped opener's payload: detection keys incidents
// by a fingerprint over the detected key, not by a monitor (MonitorID nil).
type DetectOpen struct {
	TenantID    int64
	ProjectID   int64
	MonitorID   *int64
	Detector    string
	Fingerprint int64
	Title       string
	OpenedText  string
	// Summary and Fields are the alert's measured content, caller-owned (only
	// the detector knows what it measured); both may be empty.
	Summary string
	Fields  []deliver.Field
}

// OpenDetect opens a project-scoped incident, deduplicated by fingerprint;
// notification is gated by the channel's ERROR axis (off by default).
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
		// detectStatus, not "down": the row has to say what the alert says, or
		// the dashboard shows red for the incident Telegram showed orange.
		Status: detectStatus,
	})
	if err != nil {
		return 0, false, fmt.Errorf("incident: detect open: %w", err)
	}

	// Timeline: the detector's own reason for firing, not a generic line.
	_ = q.AddIncidentUpdate(ctx, sqlc.AddIncidentUpdateParams{
		IncidentID: row.ID,
		Kind:       "opened",
		Text:       p.OpenedText,
	})

	// Same frozen-slice reasoning as Open, best-effort for the same reasons.
	_ = l.freezeSlice(ctx, row.ID, p.TenantID, p.ProjectID)

	// One line from the slice just frozen, so the alert carries the evidence;
	// an unreadable slice means no lines section, not a dropped alert.
	slice, serr := q.ListIncidentSlice(ctx, row.ID)
	if serr != nil {
		slice = nil
	}
	l.notifyChannels(ctx, l.pool.Queries(), p.TenantID, p.ProjectID, notifySpec{
		incidentID: row.ID,
		payload:    detectAlertPayload(p, uuidStr(row.PublicID), slice),
		wants: func(s notifysettings.Settings) bool {
			return s.ErrorLogs || s.RepeatingErrorLogs
		},
	})

	return row.ID, true, nil
}

// detectAlertPayload is the detection alert on the wire; every key must match
// a deliver.AlertPayload json tag exactly or the field silently drops.
func detectAlertPayload(p DetectOpen, publicID string, slice []sqlc.ListIncidentSliceRow) []byte {
	alert := map[string]any{
		"title":       p.Title,
		"status":      detectStatus,
		"incident_id": publicID,
		"detector":    p.Detector,
		"summary":     p.Summary,
		"fields":      p.Fields,
	}
	if len(slice) > 0 {
		alert["lines"] = []string{slice[len(slice)-1].Message}
		alert["lines_label"] = "From the logs when it fired"
	}
	payload, _ := json.Marshal(alert)
	return payload
}

// CloseByFingerprint resolves the open incident behind a detector key; the
// resolvedText is the caller's. A broken close is returned, not swallowed.
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

// sliceLines is how many log lines the frozen slice keeps; more than this is
// an explorer, which this product deliberately is not.
const sliceLines = 12

// freezeSlice copies the tail of the project's lines into incident_slice via
// ring.QueryBuilder; it runs at open so the slice is the state at detection.
func (l *Lifecycle) freezeSlice(ctx context.Context, incidentID, tenantID, projectID int64) error {
	if l.pgs == nil {
		return nil
	}
	q := l.pool.Queries()
	// The tail IS the slice. Not a [from, to) range from project_seq: its
	// counter is not a row count, so arithmetic on it points at nothing.
	qb := query.New(tenantID, projectID)
	// The slice is bounded by seq, not the clock, and Evidence fills the budget
	// with failing lines first instead of the tail's healthy traffic.
	lq := qb.Evidence(sliceLines)
	rows, err := l.pgs.Raw().Query(ctx, lq.SQL, lq.Args...)
	if err != nil {
		return fmt.Errorf("incident: slice query: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		// seq is a bigint in Postgres and always positive; a silent scan error
		// here once returned an empty slice, so errors are returned now.
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

// KeyFingerprint is the key-based sibling of fingerprint: the same fnv64a over
// a caller-composed key ("project:<id>:errorrate"), one stable int64 per thing.
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
	return pgtype.UUID{Bytes: uuid.New(), Valid: true}
}

// uuidStr renders a pgtype.UUID as lowercase hex without dashes (matches the
// public_id format used across the API).
func uuidStr(u pgtype.UUID) string { return fmt.Sprintf("%x", u.Bytes[:]) }
