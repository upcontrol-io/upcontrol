// Package errorlog is the notification scanner behind the "Error logs" and
// "Repeating error logs" channel settings (docs/plans/channel-notify-settings.md).
// Every 60 seconds (a ucworker job, behind an advisory lock) it aggregates the
// recent error lines by fingerprint — aggregates, never raw rows (invariant 2),
// through ring.QueryBuilder (invariant 4) — and enqueues class `ticket`
// deliveries for the channels that subscribed:
//
//   - errorLogs: "an error APPEARED", never "a line arrived" — a fingerprint
//     alerts at most once per NewErrorCooldown, whatever its line count;
//   - repeatingErrorLogs: the same fingerprint counted ≥ RepeatThreshold times
//     inside the channel's window, re-firing no sooner than one window later.
//
// error_alert_state (Postgres) is the scanner's memory of what it already
// alerted; without it a persisting error would page again on every tick.
package errorlog

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	sqlc "go.upcontrol.io/back/gen/pg"
	notifysettings "go.upcontrol.io/back/internal/channel/notify"
	"go.upcontrol.io/back/internal/deliver"
	"go.upcontrol.io/back/internal/ring/query"
	"go.upcontrol.io/back/internal/storage/ch"
	"go.upcontrol.io/back/internal/storage/pg"
)

const (
	// NewErrorCooldown is how long a fingerprint stays quiet after an
	// "error appeared" alert.
	NewErrorCooldown = 30 * time.Minute
	// RepeatThreshold is how many lines inside the window make an error
	// "repeating".
	RepeatThreshold = 2
	// Lookback is how far behind now() the "appeared" check reaches — wide
	// enough to overlap two 60s ticks, narrow enough that an error from this
	// morning is not "new".
	Lookback = 2 * time.Minute
	// maxTitle keeps a log message from becoming a page-long subject line.
	maxTitle = 120
)

// Group is one fingerprint's recent error activity, as aggregated by
// ring.QueryBuilder.ErrorGroups.
type Group struct {
	Fingerprint uint64
	Count       uint64
	Service     string
	Message     string
	LastTS      time.Time
}

// ShouldFireNew decides the errorLogs category: activity inside the lookback,
// and the fingerprint not alerted within the cooldown. lastAlerted is nil when
// the fingerprint never alerted.
func ShouldFireNew(g Group, lastAlerted *time.Time, now time.Time) bool {
	if now.Sub(g.LastTS) > Lookback {
		return false // old noise, not something that just appeared
	}
	return lastAlerted == nil || now.Sub(*lastAlerted) >= NewErrorCooldown
}

// ShouldFireRepeat decides the repeatingErrorLogs category: the count crossed
// the threshold inside the channel's window, and the last repeat alert is at
// least one window old — so a steadily-repeating error pages once per window,
// not once per tick.
func ShouldFireRepeat(g Group, lastAlerted *time.Time, window time.Duration, now time.Time) bool {
	if g.Count < RepeatThreshold {
		return false
	}
	return lastAlerted == nil || now.Sub(*lastAlerted) >= window
}

// NewErrorTitle is the errorLogs alert's one line.
func NewErrorTitle(g Group) string {
	msg := truncate(g.Message, maxTitle)
	if g.Service != "" {
		return "Error in " + g.Service + ": " + msg
	}
	return "Error: " + msg
}

// RepeatTitle is the repeatingErrorLogs alert's one line — the count and the
// window are the fact that made it fire, so they are in the title.
func RepeatTitle(g Group, windowMin int) string {
	return fmt.Sprintf("Repeating error (%d× in %d min): %s", g.Count, windowMin, truncate(g.Message, maxTitle))
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	// Cut on a rune boundary — a multi-byte character split in half renders as
	// mojibake in every channel.
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// Scanner wires the pure decisions above to Postgres + ClickHouse.
type Scanner struct {
	pool *pg.Pool
	ch   *ch.Conn
	log  *slog.Logger
}

// New builds a Scanner.
func New(pool *pg.Pool, chConn *ch.Conn, log *slog.Logger) *Scanner {
	return &Scanner{pool: pool, ch: chConn, log: log}
}

// subscription is one channel's resolved interest, grouped per tenant.
type subscription struct {
	channelID int64
	settings  notifysettings.Settings
}

// Tick runs one scan pass over every tenant that has at least one subscribed
// channel. Tenants without subscriptions cost nothing — not even a ClickHouse
// query.
func (s *Scanner) Tick(ctx context.Context) error {
	q := s.pool.Queries()
	rows, err := q.ListErrorSubscribedChannels(ctx)
	if err != nil {
		return fmt.Errorf("errorlog: list subscribed: %w", err)
	}
	byTenant := map[int64][]subscription{}
	for _, row := range rows {
		byTenant[row.TenantID] = append(byTenant[row.TenantID], subscription{
			channelID: row.ID,
			settings:  notifysettings.Resolve(row.Notify),
		})
	}
	for tenantID, subs := range byTenant {
		if err := s.scanTenant(ctx, tenantID, subs); err != nil {
			s.log.Warn("errorlog: tenant scan failed", "tenant_id", tenantID, "err", err)
		}
	}
	return nil
}

func (s *Scanner) scanTenant(ctx context.Context, tenantID int64, subs []subscription) error {
	now := time.Now()
	q := s.pool.Queries()

	// What this tenant's channels actually asked for: the distinct repeat
	// windows (each gets its own aggregate query, so a channel's count is
	// measured inside ITS window) and whether anyone wants the "appeared" pass.
	windows := map[int]bool{}
	needNew := false
	for _, sub := range subs {
		if sub.settings.ErrorLogs {
			needNew = true
		}
		if sub.settings.RepeatingErrorLogs {
			windows[sub.settings.RepeatWindowMin] = true
		}
	}

	// The scanner's memory: fingerprint+kind → when it last alerted.
	stateRows, err := q.ListErrorAlertState(ctx, tenantID)
	if err != nil {
		return err
	}
	type stateKey struct {
		fp   int64
		kind string
	}
	state := map[stateKey]time.Time{}
	for _, row := range stateRows {
		state[stateKey{row.Fingerprint, row.Kind}] = row.LastAlerted.Time
	}
	lastFor := func(fp uint64, kind string) *time.Time {
		if t, ok := state[stateKey{int64(fp), kind}]; ok {
			return &t
		}
		return nil
	}

	// The "appeared" pass: one lookback-sized query.
	if needNew {
		groups, gerr := s.errorGroups(ctx, tenantID, now.Add(-Lookback))
		if gerr != nil {
			return gerr
		}
		for _, g := range groups {
			if !ShouldFireNew(g, lastFor(g.Fingerprint, "error"), now) {
				continue
			}
			for _, sub := range subs {
				if !sub.settings.ErrorLogs {
					continue
				}
				s.enqueue(ctx, tenantID, sub.channelID, g, "error", NewErrorTitle(g), 0, now)
			}
			_ = q.UpsertErrorAlertState(ctx, sqlc.UpsertErrorAlertStateParams{
				TenantID: tenantID, Fingerprint: int64(g.Fingerprint), Kind: "error",
			})
		}
	}

	// The "repeating" pass: one query per distinct window among this tenant's
	// channels (usually one), so each channel's count is measured inside its
	// own window.
	for windowMin := range windows {
		window := time.Duration(windowMin) * time.Minute
		groups, gerr := s.errorGroups(ctx, tenantID, now.Add(-window))
		if gerr != nil {
			return gerr
		}
		for _, g := range groups {
			if !ShouldFireRepeat(g, lastFor(g.Fingerprint, "repeat"), window, now) {
				continue
			}
			for _, sub := range subs {
				if !sub.settings.RepeatingErrorLogs || sub.settings.RepeatWindowMin != windowMin {
					continue
				}
				s.enqueue(ctx, tenantID, sub.channelID, g, "repeat", RepeatTitle(g, windowMin), windowMin, now)
			}
			_ = q.UpsertErrorAlertState(ctx, sqlc.UpsertErrorAlertStateParams{
				TenantID: tenantID, Fingerprint: int64(g.Fingerprint), Kind: "repeat",
			})
		}
	}
	return nil
}

// errorGroups aggregates error lines by fingerprint across every project of
// the tenant, each behind its own ring cutoff (invariant 4: a displaced line
// must not page anybody).
func (s *Scanner) errorGroups(ctx context.Context, tenantID int64, since time.Time) ([]Group, error) {
	projRows, err := s.pool.Raw().Query(ctx, `SELECT id FROM project WHERE tenant_id = $1`, tenantID)
	if err != nil {
		return nil, err
	}
	var projectIDs []int64
	for projRows.Next() {
		var id int64
		if err := projRows.Scan(&id); err == nil {
			projectIDs = append(projectIDs, id)
		}
	}
	projRows.Close()

	merged := map[uint64]Group{}
	for _, projectID := range projectIDs {
		// No window row yet means nothing was displaced: cutoff 0.
		var cutoff int64
		if info, werr := s.pool.Queries().GetProjectWindowInfo(ctx, projectID); werr == nil {
			cutoff = info.CutoffSeq
		}
		lq := query.New(tenantID, projectID, cutoff).ErrorGroups(since)
		rows, qerr := s.ch.Raw().Query(ctx, lq.SQL, lq.Args...)
		if qerr != nil {
			return nil, qerr
		}
		for rows.Next() {
			var g Group
			if err := rows.Scan(&g.Fingerprint, &g.Count, &g.Service, &g.Message, &g.LastTS); err != nil {
				continue
			}
			if have, ok := merged[g.Fingerprint]; ok {
				g.Count += have.Count
				if have.LastTS.After(g.LastTS) {
					g.LastTS, g.Service, g.Message = have.LastTS, have.Service, have.Message
				}
			}
			merged[g.Fingerprint] = g
		}
		_ = rows.Close()
	}
	groups := make([]Group, 0, len(merged))
	for _, g := range merged {
		groups = append(groups, g)
	}
	return groups, nil
}

// enqueue puts one class-`ticket` delivery on the queue — a log alert is not a
// page. Idem-keyed per channel+fingerprint+kind+minute, so a replayed tick
// collapses instead of double-sending.
//
// The group travels with the alert, not just its title: the title is truncated
// to fit one line on a lock screen, and the reader who opens the alert wants
// the service, the count and the message in full. Every field here is one the
// scanner measured — windowMin is 0 for the "appeared" pass, which has no
// window, and the row is omitted rather than sent as "0 min".
func (s *Scanner) enqueue(ctx context.Context, tenantID, channelID int64, g Group, kind, title string, windowMin int, now time.Time) {
	fields := []deliver.Field{}
	if g.Service != "" {
		fields = append(fields, deliver.Field{Label: "Service", Value: g.Service})
	}
	if windowMin > 0 {
		fields = append(fields, deliver.Field{
			Label: "Occurrences",
			Value: fmt.Sprintf("%d in %d min", g.Count, windowMin),
		})
	}
	if !g.LastTS.IsZero() {
		fields = append(fields, deliver.Field{
			Label: "Last seen",
			Value: g.LastTS.UTC().Format("2 Jan 2006, 15:04") + " UTC",
		})
	}
	fields = append(fields, deliver.Field{
		Label: "Fingerprint",
		Value: fmt.Sprintf("%x", g.Fingerprint),
		Mono:  true,
	})

	body := map[string]any{
		"title":  title,
		"status": "check",
		"fields": fields,
	}
	if g.Message != "" {
		body["lines"] = []string{g.Message}
		body["lines_label"] = "The error"
	}
	payload, _ := json.Marshal(body)
	_ = s.pool.Queries().EnqueueDelivery(ctx, sqlc.EnqueueDeliveryParams{
		TenantID:  tenantID,
		ChannelID: channelID,
		IdemKey:   fmt.Sprintf("errlog:%d:%d:%s:%d", channelID, g.Fingerprint, kind, now.Unix()/60),
		Class:     "ticket",
		Payload:   payload,
	})
}
