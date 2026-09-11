// Package errorlog scans aggregated error fingerprints for the "Error logs"
// and "Repeating error logs" channels; error_alert_state is what it already alerted.
package errorlog

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"time"

	sqlc "go.upcontrol.io/back/gen/pg"
	notifysettings "go.upcontrol.io/back/internal/channel/notify"
	"go.upcontrol.io/back/internal/deliver"
	"go.upcontrol.io/back/internal/incident"
	"go.upcontrol.io/back/internal/ring/query"
	"go.upcontrol.io/back/internal/storage/pg"
	"go.upcontrol.io/back/internal/storage/pgstore"
)

const (
	// NewErrorCooldown is how long a fingerprint stays quiet after an
	// "error appeared" alert.
	NewErrorCooldown = 30 * time.Minute
	// RepeatThreshold is how many lines inside the window make an error
	// "repeating".
	RepeatThreshold = 2
	// Lookback is how far behind now() the "appeared" check reaches: wide
	// enough to overlap two 60s ticks, narrow enough for "new" to mean new.
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

// ShouldFireNew decides the errorLogs category: activity inside the lookback
// and the fingerprint not alerted within the cooldown (nil = never alerted).
func ShouldFireNew(g Group, lastAlerted *time.Time, now time.Time) bool {
	if now.Sub(g.LastTS) > Lookback {
		return false // old noise, not something that just appeared
	}
	return lastAlerted == nil || now.Sub(*lastAlerted) >= NewErrorCooldown
}

// ShouldFireRepeat decides the repeatingErrorLogs category: count crossed
// the threshold inside the window and the last alert is one window old or more.
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

// Scanner wires the pure decisions above to Postgres + its telemetry store.
type Scanner struct {
	pool *pg.Pool
	pgs  *pgstore.Store
	log  *slog.Logger
}

// New builds a Scanner.
func New(pool *pg.Pool, pgs *pgstore.Store, log *slog.Logger) *Scanner {
	return &Scanner{pool: pool, pgs: pgs, log: log}
}

// subscription is one channel's resolved interest, grouped per project.
type subscription struct {
	channelID int64
	settings  notifysettings.Settings
}

// scope is the pair a scan pass runs over: a channel subscribes for its own
// project, and the scanner's memory is keyed the same way.
type scope struct {
	tenantID  int64
	projectID int64
}

// Tick runs one scan pass over every project with at least one subscribed
// channel; projects without subscriptions cost nothing, not even a query.
func (s *Scanner) Tick(ctx context.Context) error {
	q := s.pool.Queries()
	rows, err := q.ListErrorSubscribedChannels(ctx)
	if err != nil {
		return fmt.Errorf("errorlog: list subscribed: %w", err)
	}
	byProject := map[scope][]subscription{}
	for _, row := range rows {
		key := scope{tenantID: row.TenantID, projectID: row.ProjectID}
		byProject[key] = append(byProject[key], subscription{
			channelID: row.ID,
			settings:  notifysettings.Resolve(row.Notify),
		})
	}
	for key, subs := range byProject {
		if err := s.scanProject(ctx, key, subs); err != nil {
			s.log.Warn("errorlog: project scan failed",
				"tenant_id", key.tenantID, "project_id", key.projectID, "err", err)
		}
	}
	return nil
}

func (s *Scanner) scanProject(ctx context.Context, sc scope, subs []subscription) error {
	now := time.Now()
	q := s.pool.Queries()

	// The plan's Telegram seats and rooms mute here exactly as they mute an
	// incident page. A failed read keeps every subscriber.
	if chans, err := q.ListChannelsByProject(ctx, sc.projectID); err == nil {
		muted := incident.MutedByPlan(ctx, q, sc.tenantID, chans)
		subs = slices.DeleteFunc(subs, func(sub subscription) bool { return muted[sub.channelID] })
	}

	// What this project's channels asked for: the distinct repeat windows (each
	// gets its own aggregate query) and whether anyone wants the "appeared" pass.
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

	// The scanner's memory: fingerprint+kind → when it last alerted. Per
	// project: the same error in a sibling project is a separate alert.
	stateRows, err := q.ListErrorAlertState(ctx, sqlc.ListErrorAlertStateParams{
		TenantID: sc.tenantID, ProjectID: sc.projectID,
	})
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
		groups, gerr := s.errorGroups(ctx, sc, now.Add(-Lookback))
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
				s.enqueue(ctx, sc.tenantID, sub.channelID, g, "error", NewErrorTitle(g), 0, now)
			}
			_ = q.UpsertErrorAlertState(ctx, sqlc.UpsertErrorAlertStateParams{
				TenantID: sc.tenantID, ProjectID: sc.projectID,
				Fingerprint: int64(g.Fingerprint), Kind: "error",
			})
		}
	}

	// The "repeating" pass: one query per distinct window among this project's
	// channels, so each channel's count is measured inside its own window.
	for windowMin := range windows {
		window := time.Duration(windowMin) * time.Minute
		groups, gerr := s.errorGroups(ctx, sc, now.Add(-window))
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
				s.enqueue(ctx, sc.tenantID, sub.channelID, g, "repeat", RepeatTitle(g, windowMin), windowMin, now)
			}
			_ = q.UpsertErrorAlertState(ctx, sqlc.UpsertErrorAlertStateParams{
				TenantID: sc.tenantID, ProjectID: sc.projectID,
				Fingerprint: int64(g.Fingerprint), Kind: "repeat",
			})
		}
	}
	return nil
}

// errorGroups aggregates one project's error lines by fingerprint, behind its
// own ring cutoff (a displaced line must not page).
func (s *Scanner) errorGroups(ctx context.Context, sc scope, since time.Time) ([]Group, error) {
	lq := query.New(sc.tenantID, sc.projectID).ErrorGroups(since)
	rows, err := s.pgs.Raw().Query(ctx, lq.SQL, lq.Args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var groups []Group
	for rows.Next() {
		// fingerprint is a wrapped bigint: scan signed, wrap in Go — pgx
		// refuses a negative int8 into uint64, and half the hash space is.
		var g Group
		var fp int64
		if err := rows.Scan(&fp, &g.Count, &g.Service, &g.Message, &g.LastTS); err != nil {
			continue
		}
		g.Fingerprint = uint64(fp)
		groups = append(groups, g)
	}
	return groups, rows.Err()
}

// enqueue puts one class-`ticket` delivery on the queue (a log alert is not
// a page), idem-keyed per channel+fingerprint+kind+minute so a replay collapses.
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
