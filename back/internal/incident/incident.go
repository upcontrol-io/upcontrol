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
	"go.upcontrol.io/back/internal/deliver"
	"go.upcontrol.io/back/internal/ring/query"
	"go.upcontrol.io/back/internal/storage/ch"
	"go.upcontrol.io/back/internal/storage/pg"
)

// detectStatus is what a detection incident opens as, on the row AND in the
// alert — the two must agree or the dashboard shows red for the incident
// Telegram showed orange. Not "down": these detectors read the log stream and
// never look at a monitor, so they can report degradation but have not earned
// the availability verdict. The API counts it as ongoing either way.
const detectStatus = "check"

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
		// The checks failed, so this one has earned the word.
		Status: "down",
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

	// The facts the incident already holds, so the alert can name what broke
	// instead of only that something did. Only what was measured: a field the
	// row does not carry is omitted, never sent as an empty string for a
	// renderer to draw as a blank row.
	//
	// Built once, outside the channel loop: one outage has one start time, and
	// computing it per channel would tell email and telegram different minutes
	// for the same incident.
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

	l.notifyChannels(ctx, mon.TenantID, notifySpec{
		incidentID: row.ID,
		payload:    payload,
		wants:      func(s notifysettings.Settings) bool { return s.WebsiteDown },
		followup:   fuPayload,
	})

	return row.ID, true, nil
}

// notifySpec is one incident's side of a notification: the message every
// interested channel receives, and the question "is this channel interested"
// — availability asks websiteDown, detection asks the error axis.
type notifySpec struct {
	incidentID int64
	payload    []byte
	wants      func(notifysettings.Settings) bool
	// followup is the 15-minute resolve follow-up's payload, or nil when this
	// kind of incident has none: a detector incident has no monitor to report
	// back up, and GetIncidentForFollowUp would find nothing to compose from.
	followup []byte
}

// notifyChannels enqueues one delivery per interested channel. EnqueueDelivery
// dedupes on idem_key, so replaying an open is a no-op — these are the rows the
// delivery worker (ucworker) picks up. Without them an open incident never
// reaches delivery_queue.
func (l *Lifecycle) notifyChannels(ctx context.Context, tenantID int64, n notifySpec) {
	q := l.pool.Queries()
	chans, err := q.ListChannelsByTenant(ctx, tenantID)
	if err != nil {
		return
	}
	// The follow-up is PAID ONLY (every paid plan): the stored flag alone does
	// not schedule one — a tenant that downgraded keeps the setting but stops
	// getting what it bought. Read only when a follow-up is possible at all.
	plan := ""
	if n.followup != nil {
		plan, _ = q.GetTenantPlan(ctx, tenantID)
	}

	sent := 0
	for _, ch := range chans {
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
		// The 15-minute resolve follow-up: enqueued now with next_try_at in the
		// future, COMPOSED at send time from the incident's then-current state
		// (deliver.Worker, class "followup") — recovered → "back up", still open
		// → "still down". Either way, so the reader knows whether to keep
		// running for a laptop.
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
	// The mark says an alert was QUEUED for at least one channel — the worker
	// sends later and may still dead-letter it, so this is not proof of
	// delivery and MTTD read off it is a queue time. It is written only when a
	// channel actually took one: the detect gate defaults OFF, so most detect
	// incidents enqueue nothing, and stamping those would put a notification
	// time on an incident nobody was told about.
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
		// removing the check, not a recovery, and the timeline may not imply one.
		text = "Monitor deleted"
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
	// Summary and Fields are the alert's measured content — the caller owns
	// them because only the detector knows what it measured. Both may be
	// empty: a renderer draws what it is given and invents nothing.
	Summary string
	Fields  []deliver.Field
}

// OpenDetect opens a project-scoped incident for a detector verdict.
// Deduplication is by fingerprint: an already-open incident is returned as-is.
//
// NAMED REVERSAL of D4 (Aug 24, 2026). D4 said detection incidents were
// dashboard-visible only and enqueued nothing. They now notify through the
// same helper availability incidents use, gated by the channel's ERROR axis
// (errorLogs / repeatingErrorLogs — not websiteDown), which is off by default:
// a channel that never opened its settings keeps today's silence. There is no
// follow-up (no monitor to report back up), and notified_at is stamped only if
// a delivery was actually enqueued. Do not "fix this back".
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

	// Timeline: the detector's own reason for firing (D13) — what the z-score
	// actually said, not a generic "spike detected" line.
	_ = q.AddIncidentUpdate(ctx, sqlc.AddIncidentUpdateParams{
		IncidentID: row.ID,
		Kind:       "opened",
		Text:       p.OpenedText,
	})

	// Same frozen-slice reasoning as Open, best-effort for the same reasons.
	_ = l.freezeSlice(ctx, row.ID, p.TenantID, p.ProjectID)

	// One line from the slice just frozen, so the alert carries the evidence
	// and not only the verdict. A slice that cannot be read is no slice: the
	// alert goes out without the lines section rather than not at all.
	slice, serr := q.ListIncidentSlice(ctx, row.ID)
	if serr != nil {
		slice = nil
	}
	l.notifyChannels(ctx, p.TenantID, notifySpec{
		incidentID: row.ID,
		payload:    detectAlertPayload(p, uuidStr(row.PublicID), slice),
		wants: func(s notifysettings.Settings) bool {
			return s.ErrorLogs || s.RepeatingErrorLogs
		},
	})

	return row.ID, true, nil
}

// detectAlertPayload is the detection alert on the wire. Every key here must
// match a deliver.AlertPayload json tag exactly — the two sides meet through a
// queue, so a typo does not fail, it silently drops the field (and dropping
// "detector" alone turns the mail's badge back into "Down" and puts a Resolve
// button on an incident that refuses it).
//
// The slice arrives oldest-first, so the newest line is the LAST one; an empty
// slice draws no lines section at all rather than a heading over nothing.
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
