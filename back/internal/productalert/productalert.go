// Package productalert evaluates the product alert rules: one event of a
// project (its count, or the money it carried in one currency) compared over
// the last window_hours against the same hours a week ago, or watched for
// silence. Each rule keeps its episode on its row, so a channel hears once
// when it starts and once when it ends.
package productalert

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	sqlc "go.upcontrol.io/back/gen/pg"
	"go.upcontrol.io/back/internal/deliver"
	"go.upcontrol.io/back/internal/incident"
	"go.upcontrol.io/back/internal/storage/pg"
	"go.upcontrol.io/back/internal/storage/pgstore"
)

const (
	week = 7 * 24 * time.Hour
	// minBaseEvents is the guard: a week-ago window holding fewer events is
	// too thin to compare with, and the rule stays learning.
	minBaseEvents = 20
	maxLines      = 5

	StateOK       = "ok"
	StateFiring   = "firing"
	StateLearning = "learning"

	SendFired     = "fired"
	SendRecovered = "recovered"
)

// Rule is one product_alert row the evaluator reads.
type Rule struct {
	ID, TenantID, ProjectID int64
	Name, Event             string
	Labels                  map[string]string
	Measure, Currency       string
	Kind, Direction         string
	Percent, WindowHours    int
	State                   string
	LastFiredAt             *time.Time
}

// Reading is what one evaluation measured. Cur and Base are the rule's value
// (a count, or minor units of money) over the window and the same hours a
// week ago; BaseEvents is the week-ago event count the guard reads even for
// money; CurEvents and History (events in the 7 days before the window) are
// what a silence rule reads.
type Reading struct {
	Cur, Base, BaseEvents, CurEvents, History int64
}

func (rd Reading) pct() float64 {
	return float64(rd.Cur-rd.Base) / float64(rd.Base) * 100
}

// Condition answers whether the rule's condition holds now (StateFiring), does
// not (StateOK), or cannot be measured against enough history (StateLearning).
func Condition(r Rule, rd Reading) string {
	if r.Kind == "silence" {
		switch {
		case rd.CurEvents > 0:
			return StateOK
		case rd.History >= int64((168+r.WindowHours-1)/r.WindowHours):
			return StateFiring
		default:
			return StateLearning
		}
	}
	if rd.BaseEvents < minBaseEvents || rd.Base == 0 {
		return StateLearning
	}
	pct, p := rd.pct(), float64(r.Percent)
	var holds bool
	switch r.Direction {
	case "down":
		holds = pct <= -p
	case "up":
		holds = pct >= p
	default:
		holds = math.Abs(pct) >= p
	}
	if holds {
		return StateFiring
	}
	return StateOK
}

// Step moves the episode: the state the row keeps next, and the message to
// send ("" for none). A firing rule repeats nothing; it recovers once.
func Step(state, cond string) (next, send string) {
	switch {
	case cond == StateFiring && state != StateFiring:
		return StateFiring, SendFired
	case state == StateFiring && cond != StateFiring:
		return cond, SendRecovered
	}
	return cond, ""
}

// subject names the rule in a title: its name when it has one, else the event,
// and money says so.
func subject(r Rule, money bool) string {
	if r.Name != "" {
		return r.Name
	}
	if money {
		return r.Event + " revenue"
	}
	return r.Event
}

func (r Rule) money() bool { return r.Kind == "change" && r.Measure == "amount" }

func (r Rule) value(n int64) string {
	if r.money() {
		return formatMoney(n, r.Currency)
	}
	return group(n)
}

// Title is the message's one line.
func Title(r Rule, rd Reading, send string) string {
	switch {
	case send == SendRecovered:
		return subject(r, r.money()) + " is back to normal"
	case r.Kind == "silence":
		return fmt.Sprintf("%s has been silent for %dh", subject(r, false), r.WindowHours)
	}
	pct := rd.pct()
	verb := "rose"
	if pct < 0 {
		verb = "dropped"
	}
	return fmt.Sprintf("%s %s %d%% vs the same %dh last week", subject(r, r.money()), verb, int(math.Round(math.Abs(pct))), r.WindowHours)
}

// Payload builds the delivery body in the shape the channels render; lines are
// the deploys and releases inside the window, newest first.
func Payload(r Rule, rd Reading, send string, lines []string) []byte {
	var fields []deliver.Field
	now := rd.Cur
	if r.Kind == "silence" {
		now = rd.CurEvents
	}
	switch {
	case send == SendRecovered:
		fields = append(fields, deliver.Field{Label: "Now", Value: r.value(now)})
	case r.Kind == "silence":
		usual := int64(math.Round(float64(rd.History) * float64(r.WindowHours) / 168))
		fields = append(fields, deliver.Field{Label: "Usually", Value: fmt.Sprintf("about %s in %dh", group(usual), r.WindowHours)})
	default:
		verb := map[string]string{"down": "drops", "up": "rises"}[r.Direction]
		if verb == "" {
			verb = "moves"
		}
		fields = append(fields,
			deliver.Field{Label: "Now", Value: r.value(rd.Cur)},
			deliver.Field{Label: "Week ago", Value: r.value(rd.Base)},
			deliver.Field{Label: "Rule", Value: fmt.Sprintf("%s %d%% in %dh", verb, r.Percent, r.WindowHours)})
	}
	if send == SendFired && len(r.Labels) > 0 {
		pairs := make([]string, 0, len(r.Labels))
		for k, v := range r.Labels {
			pairs = append(pairs, k+"="+v)
		}
		slices.Sort(pairs)
		fields = append(fields, deliver.Field{Label: "Filter", Value: strings.Join(pairs, ", ")})
	}
	status := "check"
	if send == SendRecovered {
		status = "ok"
	}
	body := map[string]any{
		"title":    Title(r, rd, send),
		"status":   status,
		"fields":   fields,
		"detector": deliver.DetectorProductAlert,
	}
	if len(lines) > 0 {
		body["lines"] = lines
		body["lines_label"] = "Deploys in this window"
	} else {
		body["summary"] = "No deploys or releases in this window."
	}
	payload, _ := json.Marshal(body)
	return payload
}

// markLines words the window's deploys and releases, newest first, at most
// maxLines of them.
func markLines(deploys, releases []pgstore.MarkRow) []string {
	type mark struct {
		at   time.Time
		text string
	}
	var marks []mark
	for _, d := range deploys {
		parts := []string{"deploy"}
		if d.Name != "deploy" {
			parts = append(parts, d.Name)
		}
		if d.SHA != "" {
			parts = append(parts, d.SHA[:min(7, len(d.SHA))])
		}
		marks = append(marks, mark{d.At, strings.Join(parts, " ")})
	}
	for _, rel := range releases {
		marks = append(marks, mark{rel.At, "release " + rel.Name})
	}
	slices.SortStableFunc(marks, func(a, b mark) int { return b.at.Compare(a.at) })
	lines := []string{}
	for _, m := range marks[:min(maxLines, len(marks))] {
		lines = append(lines, m.text+" at "+m.at.UTC().Format("15:04")+" UTC")
	}
	return lines
}

// ponytail: the zero-decimal currencies are this list; a currency table comes
// when a customer sends one that is missing.
var noMinor = map[string]bool{"JPY": true, "KRW": true, "VND": true, "CLP": true, "ISK": true}

// formatMoney prints minor units in the currency's major unit with its code.
func formatMoney(minor int64, currency string) string {
	currency = strings.ToUpper(currency)
	sign := ""
	if minor < 0 {
		sign, minor = "-", -minor
	}
	if noMinor[currency] {
		return strings.TrimSpace(sign + group(minor) + " " + currency)
	}
	return strings.TrimSpace(fmt.Sprintf("%s%s.%02d %s", sign, group(minor/100), minor%100, currency))
}

// group writes a non-negative integer with thousands separators.
func group(n int64) string {
	s := strconv.FormatInt(n, 10)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}

// Evaluator wires the decisions above to Postgres.
type Evaluator struct {
	pool *pg.Pool
	pgs  *pgstore.Store
	log  *slog.Logger
	now  func() time.Time
}

// New builds an Evaluator on the wall clock.
func New(pool *pg.Pool, pgs *pgstore.Store, log *slog.Logger) *Evaluator {
	return &Evaluator{pool: pool, pgs: pgs, log: log, now: time.Now}
}

// liveRules: not paused, the project not frozen, and inside the workspace's
// plan count, ranked oldest first (NULL or an unknown plan carries them all).
const liveRules = `
	SELECT r.id, r.tenant_id, r.project_id, r.name, r.event, r.labels, r.measure, r.currency,
	       r.kind, r.direction, r.percent, r.window_hours, r.state, r.last_fired_at
	  FROM (SELECT a.*, row_number() OVER (PARTITION BY a.tenant_id ORDER BY a.created_at, a.id) AS rank
	          FROM product_alert a) r
	  JOIN tenant t ON t.id = r.tenant_id
	  JOIN project pr ON pr.id = r.project_id
	  LEFT JOIN plan_entitlement p ON p.plan = t.plan
	 WHERE NOT r.paused AND pr.frozen_at IS NULL
	   AND (p.product_alerts IS NULL OR r.rank <= p.product_alerts)
	 ORDER BY r.id`

// Tick evaluates every live rule once; one rule's failure is logged and the
// others still run.
func (e *Evaluator) Tick(ctx context.Context) error {
	rows, err := e.pool.Raw().Query(ctx, liveRules)
	if err != nil {
		return fmt.Errorf("productalert: list rules: %w", err)
	}
	var rules []Rule
	for rows.Next() {
		var r Rule
		var labels []byte
		if err := rows.Scan(&r.ID, &r.TenantID, &r.ProjectID, &r.Name, &r.Event, &labels, &r.Measure, &r.Currency,
			&r.Kind, &r.Direction, &r.Percent, &r.WindowHours, &r.State, &r.LastFiredAt); err != nil {
			rows.Close()
			return fmt.Errorf("productalert: scan rule: %w", err)
		}
		// A filter the evaluator cannot read is that rule's failure, not the tick's.
		if err := json.Unmarshal(labels, &r.Labels); err != nil {
			e.log.Warn("productalert: unreadable labels", "rule_id", r.ID, "err", err)
			continue
		}
		rules = append(rules, r)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("productalert: list rules: %w", err)
	}
	now := e.now()
	for _, r := range rules {
		if err := e.evaluate(ctx, r, now); err != nil {
			e.log.Warn("productalert: rule evaluation failed", "rule_id", r.ID, "project_id", r.ProjectID, "err", err)
		}
	}
	return nil
}

// read measures the rule's windows: [now-W, now) and the same hours a week
// earlier, each read as one bucket as wide as its span.
func (e *Evaluator) read(ctx context.Context, r Rule, now time.Time) (Reading, error) {
	var rd Reading
	var err error
	span := func(from, to time.Time) int { return int(to.Sub(from) / time.Second) }
	count := func(from, to time.Time) int64 {
		if err != nil {
			return 0
		}
		var bs []pgstore.Bucket
		bs, err = e.pgs.EventBuckets(ctx, r.TenantID, r.ProjectID, r.Event, r.Labels, false, from, to, span(from, to))
		return total(bs)
	}
	amount := func(from, to time.Time) int64 {
		if err != nil {
			return 0
		}
		var bs []pgstore.Bucket
		bs, err = e.pgs.AmountBuckets(ctx, r.TenantID, r.ProjectID, r.Event, strings.ToUpper(r.Currency), r.Labels, from, to, span(from, to))
		return total(bs)
	}
	from := now.Add(-time.Duration(r.WindowHours) * time.Hour)
	if r.Kind == "silence" {
		rd.CurEvents = count(from, now)
		rd.History = count(from.Add(-week), from)
		return rd, err
	}
	rd.BaseEvents = count(from.Add(-week), now.Add(-week))
	if r.money() {
		rd.Cur = amount(from, now)
		rd.Base = amount(from.Add(-week), now.Add(-week))
	} else {
		rd.Cur = count(from, now)
		rd.Base = rd.BaseEvents
	}
	return rd, err
}

func total(bs []pgstore.Bucket) int64 {
	var n int64
	for _, b := range bs {
		n += b.Count
	}
	return n
}

func (e *Evaluator) evaluate(ctx context.Context, r Rule, now time.Time) error {
	rd, err := e.read(ctx, r, now)
	if err != nil {
		return fmt.Errorf("read: %w", err)
	}
	next, send := Step(r.State, Condition(r, rd))
	firedAt := r.LastFiredAt
	if send == SendFired {
		firedAt = &now
	}

	var payload []byte
	var chans []sqlc.ListChannelsByProjectRow
	if send != "" {
		from := now.Add(-time.Duration(r.WindowHours) * time.Hour)
		deploys, err := e.pgs.DeployMarks(ctx, r.TenantID, r.ProjectID, from, now)
		if err != nil {
			return fmt.Errorf("deploy marks: %w", err)
		}
		releases, err := e.pgs.ReleaseMarks(ctx, r.TenantID, r.ProjectID, from, now)
		if err != nil {
			return fmt.Errorf("release marks: %w", err)
		}
		payload = Payload(r, rd, send, markLines(deploys, releases))
		q := e.pool.Queries()
		if chans, err = q.ListChannelsByProject(ctx, r.ProjectID); err != nil {
			return fmt.Errorf("channels: %w", err)
		}
		muted := incident.MutedByPlan(ctx, q, r.TenantID, chans)
		chans = slices.DeleteFunc(chans, func(ch sqlc.ListChannelsByProjectRow) bool { return muted[ch.ID] })
	}

	// The deliveries and the episode land together or not at all, so a failed
	// write never leaves a sent message behind a row that will send it again.
	tx, err := e.pool.Raw().Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := e.pool.Queries().WithTx(tx)
	var firedUnix int64
	if firedAt != nil {
		firedUnix = firedAt.Unix()
	}
	for _, ch := range chans {
		if err := q.EnqueueDelivery(ctx, sqlc.EnqueueDeliveryParams{
			TenantID:  r.TenantID,
			ChannelID: ch.ID,
			IdemKey:   fmt.Sprintf("palert:%d:%d:%s:%d", r.ID, ch.ID, send, firedUnix),
			Class:     "ticket",
			Payload:   payload,
		}); err != nil {
			return fmt.Errorf("enqueue: %w", err)
		}
	}
	if _, err := tx.Exec(ctx,
		`UPDATE product_alert SET state = $2, last_fired_at = $3, last_checked_at = $4 WHERE id = $1`,
		r.ID, next, firedAt, now); err != nil {
		return fmt.Errorf("update: %w", err)
	}
	return tx.Commit(ctx)
}
