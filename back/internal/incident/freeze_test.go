package incident

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	sqlc "go.upcontrol.io/back/gen/pg"
	"go.upcontrol.io/back/internal/deliver"
	"go.upcontrol.io/back/internal/storage/pg"
)

// scanID runs a seeding INSERT ... RETURNING id.
func scanID(t *testing.T, pool *pg.Pool, what, sql string, args ...any) int64 {
	t.Helper()
	var id int64
	if err := pool.Raw().QueryRow(context.Background(), sql, args...).Scan(&id); err != nil {
		t.Fatalf("seed %s: %v", what, err)
	}
	return id
}

// The plan's Telegram arithmetic: seats go to the oldest destinations, one
// per person however many channels they hold, and a room the plan does not
// carry is muted without taking a seat.
func TestMutedTelegram(t *testing.T) {
	person := func(id int64) *int64 { return &id }
	tg := func(id int64, who *int64) sqlc.ListChannelsByProjectRow {
		return sqlc.ListChannelsByProjectRow{ID: id, Kind: "telegram", RecipientPersonID: who}
	}
	mail := sqlc.ListChannelsByProjectRow{ID: 99, Kind: "email"}
	for _, tc := range []struct {
		name  string
		chans []sqlc.ListChannelsByProjectRow
		seats int
		rooms bool
		muted []int64
	}{
		{"four people on three seats: the newest is muted",
			[]sqlc.ListChannelsByProjectRow{tg(1, person(10)), tg(2, person(20)), mail, tg(3, person(30)), tg(4, person(40))},
			3, true, []int64{4}},
		{"one person with two channels holds one seat",
			[]sqlc.ListChannelsByProjectRow{tg(1, person(10)), tg(2, person(10)), tg(3, person(20)), tg(4, person(30))},
			3, true, nil},
		{"a room takes a seat when the plan carries rooms",
			[]sqlc.ListChannelsByProjectRow{tg(1, nil), tg(2, person(10)), tg(3, person(20)), tg(4, person(30))},
			3, true, []int64{4}},
		{"a room the plan does not carry is muted and takes no seat",
			[]sqlc.ListChannelsByProjectRow{tg(1, nil), tg(2, person(10)), tg(3, person(20)), tg(4, person(30))},
			3, false, []int64{1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := mutedTelegram(tc.chans, tc.seats, tc.rooms)
			var ids []int64
			for id := range got {
				ids = append(ids, id)
			}
			slices.Sort(ids)
			if !slices.Equal(ids, tc.muted) {
				t.Fatalf("muted = %v, want %v", ids, tc.muted)
			}
		})
	}
}

// An unreadable plan mutes nothing: the wall stood at connect time, and a
// lost page is the worse error.
func TestMutedByPlanFailsOpen(t *testing.T) {
	chans := []sqlc.ListChannelsByProjectRow{{ID: 1, Kind: "telegram"}}
	if got := MutedByPlan(context.Background(), sqlc.New(failDB{}), 1, chans); len(got) != 0 {
		t.Fatalf("muted = %v on a failed plan read, want none", got)
	}
}

var errFailDB = errors.New("db down")

type failDB struct{}

func (failDB) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, errFailDB
}
func (failDB) Query(context.Context, string, ...any) (pgx.Rows, error) { return nil, errFailDB }
func (failDB) QueryRow(context.Context, string, ...any) pgx.Row        { return failRow{} }

type failRow struct{}

func (failRow) Scan(...any) error { return errFailDB }

// A frozen project is left out of every reader that would run, page or land
// on it, while its incidents still record. The frozen project gets the LOWER
// id, so every min(id) fallback that forgot the predicate would pick it.
// UC_TEST_POSTGRES unset = skip.
func TestFrozenProjectIsExcluded(t *testing.T) {
	ctx := context.Background()
	pool := openTestDB(t, "frozen exclusion test")
	q := pool.Queries()
	uniq := time.Now().UnixNano()

	personID := scanID(t, pool, "person", `INSERT INTO person (public_id, email, name) VALUES (gen_random_uuid(), $1, 'Freeze') RETURNING id`,
		fmt.Sprintf("freeze-%d@example.com", uniq))
	tenantID := scanID(t, pool, "tenant", `INSERT INTO tenant (public_id, name, plan, owner_person_id) VALUES (gen_random_uuid(), $1, 'Free', $2) RETURNING id`,
		fmt.Sprintf("freeze-%d", uniq), personID)
	frozen := scanID(t, pool, "frozen project", `INSERT INTO project (public_id, tenant_id, domain, frozen_at) VALUES (gen_random_uuid(), $1, 'frozen.example', now()) RETURNING id`, tenantID)
	live := scanID(t, pool, "live project", `INSERT INTO project (public_id, tenant_id, domain) VALUES (gen_random_uuid(), $1, 'live.example') RETURNING id`, tenantID)
	monitor := func(projectID int64, kind, key string) int64 {
		t.Helper()
		targetKind := "website"
		if kind == "heartbeat" {
			targetKind = "heartbeat"
		}
		target := scanID(t, pool, "target", `INSERT INTO probe_target (key, kind, url) VALUES ($1, $2, $1) RETURNING id`, key, targetKind)
		if _, err := pool.Raw().Exec(ctx,
			`INSERT INTO target_schedule (target_id, next_due_at) VALUES ($1, now() - interval '5 minutes')`, target); err != nil {
			t.Fatalf("seed schedule: %v", err)
		}
		return scanID(t, pool, "monitor", `INSERT INTO monitor (public_id, tenant_id, project_id, target_id, kind, name, target, interval_sec)
			VALUES (gen_random_uuid(), $1, $2, $3, $4, $5, $5, 60) RETURNING id`, tenantID, projectID, target, kind, key)
	}
	frozenCheck := monitor(frozen, "http", fmt.Sprintf("https://frozen-%d.example", uniq))
	frozenBeat := monitor(frozen, "heartbeat", fmt.Sprintf("hb-frozen-%d", uniq))
	monitor(live, "http", fmt.Sprintf("https://live-%d.example", uniq))
	if _, err := pool.Raw().Exec(ctx,
		`INSERT INTO alert_channel (public_id, tenant_id, project_id, kind, target, notify)
		 VALUES (gen_random_uuid(), $1, $2, 'email', $3, '{"websiteDown": true, "errorLogs": true}')`,
		tenantID, frozen, fmt.Sprintf("frozen-%d@example.com", uniq)); err != nil {
		t.Fatalf("seed channel: %v", err)
	}
	token := []byte(fmt.Sprintf("freeze-session-%d", uniq))
	hash := sha256.Sum256(token)
	if _, err := pool.Raw().Exec(ctx,
		`INSERT INTO session (token_hash, person_id, tenant_id, project_id, expires_at)
		 VALUES ($1, $2, $3, $4, now() + interval '1 hour')`, hash[:], personID, tenantID, frozen); err != nil {
		t.Fatalf("seed session: %v", err)
	}

	for _, tc := range []struct {
		name  string
		check func() error
	}{
		{"ListMissedHeartbeats skips the frozen heartbeat", func() error {
			rows, err := q.ListMissedHeartbeats(ctx)
			if err != nil {
				return err
			}
			if slices.ContainsFunc(rows, func(r sqlc.ListMissedHeartbeatsRow) bool { return r.ID == frozenBeat }) {
				return errors.New("the frozen project's heartbeat is listed as missed")
			}
			return nil
		}},
		{"ListErrorSubscribedChannels skips the frozen project", func() error {
			rows, err := q.ListErrorSubscribedChannels(ctx)
			if err != nil {
				return err
			}
			if slices.ContainsFunc(rows, func(r sqlc.ListErrorSubscribedChannelsRow) bool { return r.ProjectID == frozen }) {
				return errors.New("the frozen project's channel is subscribed to error logs")
			}
			return nil
		}},
		{"ReachableProjectInTenant never resolves onto it", func() error {
			got, err := q.ReachableProjectInTenant(ctx, sqlc.ReachableProjectInTenantParams{
				Pick: frozen, TenantID: tenantID, PersonID: &personID,
			})
			if err != nil {
				return err
			}
			if got != live {
				return fmt.Errorf("resolved %d, want the live project %d", got, live)
			}
			return nil
		}},
		{"GetMe names the live project", func() error {
			row, err := q.GetMe(ctx, hash[:])
			if err != nil {
				return err
			}
			if row.ProjectID == nil || *row.ProjectID != live {
				return fmt.Errorf("project = %v, want the live project %d", row.ProjectID, live)
			}
			return nil
		}},
		{"the check count leaves its checks out", func() error {
			n, err := q.CountMonitorsByTenant(ctx, tenantID)
			if err != nil {
				return err
			}
			if n != 1 {
				return fmt.Errorf("check count = %d, want 1", n)
			}
			return nil
		}},
		{"ActivateInvites leaves its invite pending", func() error {
			invitee := scanID(t, pool, "invitee", `INSERT INTO person (public_id, email, name) VALUES (gen_random_uuid(), $1, 'Invitee') RETURNING id`,
				fmt.Sprintf("freeze-invitee-%d@example.com", uniq))
			if _, err := pool.Raw().Exec(ctx,
				`INSERT INTO project_member (project_id, person_id, tenant_id, role, status)
				 VALUES ($1, $3, $4, 'notify', 'pending'), ($2, $3, $4, 'notify', 'pending')`,
				frozen, live, invitee, tenantID); err != nil {
				return err
			}
			rows, err := q.ActivateInvites(ctx, invitee)
			if err != nil {
				return err
			}
			if len(rows) != 1 || rows[0].ProjectID != live {
				return fmt.Errorf("activated %v, want the live project %d alone", rows, live)
			}
			return nil
		}},
		{"an incident still records but delivers nothing", func() error {
			incidentID, created, err := New(pool, nil).Open(ctx, frozenCheck, "frozen.example is down", 0)
			if err != nil || !created {
				return fmt.Errorf("open: created=%v err=%w", created, err)
			}
			var queued int
			if err := pool.Raw().QueryRow(ctx,
				`SELECT count(*) FROM delivery_queue WHERE incident_id = $1`, incidentID).Scan(&queued); err != nil {
				return err
			}
			if queued != 0 {
				return fmt.Errorf("%d deliveries queued from a frozen project", queued)
			}
			return nil
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.check(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// fakeMail is the delivery worker's email sender in a test: every send
// succeeds and nothing leaves the process.
type fakeMail struct{}

func (fakeMail) Kind() string { return "email" }
func (fakeMail) Send(context.Context, string, deliver.AlertPayload) (int, error) {
	return http.StatusOK, nil
}

// What was queued before a change of plan meets that change at send time: a
// frozen project's page goes dead while the owner's test still sends, and a
// plan_paused close kills the 15-minute follow-up instead of letting it read
// as a recovery. UC_TEST_POSTGRES unset = skip.
func TestQueuedDeliveriesMeetThePlan(t *testing.T) {
	ctx := context.Background()
	pool := openTestDB(t, "queued deliveries test")
	uniq := time.Now().UnixNano()
	// Indie: the follow-up is paid only.
	tenantID := scanID(t, pool, "tenant", `INSERT INTO tenant (public_id, name, plan) VALUES (gen_random_uuid(), $1, 'Indie') RETURNING id`,
		fmt.Sprintf("queued-%d", uniq))
	projectID := scanID(t, pool, "project", `INSERT INTO project (public_id, tenant_id, domain) VALUES (gen_random_uuid(), $1, 'queued.example') RETURNING id`, tenantID)
	channelID := scanID(t, pool, "channel", `INSERT INTO alert_channel (public_id, tenant_id, project_id, kind, target, notify)
		VALUES (gen_random_uuid(), $1, $2, 'email', $3, '{"websiteDown": true, "resolveFollowUp": true}') RETURNING id`,
		tenantID, projectID, fmt.Sprintf("queued-%d@example.com", uniq))
	monitor := func(name string) int64 {
		t.Helper()
		url := fmt.Sprintf("https://%s-%d.example", name, uniq)
		target := scanID(t, pool, "target", `INSERT INTO probe_target (key, kind, url) VALUES ($1, 'website', $1) RETURNING id`, url)
		return scanID(t, pool, "monitor", `INSERT INTO monitor (public_id, tenant_id, project_id, target_id, kind, name, target, interval_sec)
			VALUES (gen_random_uuid(), $1, $2, $3, 'http', $4, $5, 60) RETURNING id`, tenantID, projectID, target, name, url)
	}
	l := New(pool, nil)
	open := func(monitorID int64) int64 {
		t.Helper()
		id, created, err := l.Open(ctx, monitorID, "down", 0)
		if err != nil || !created {
			t.Fatalf("open: created=%v err=%v", created, err)
		}
		return id
	}
	dw := deliver.NewWorker(pool, slog.New(slog.DiscardHandler), "queued-test")
	dw.RegisterChannel(fakeMail{})
	// The queue is shared with the package's other tests: tick until none of
	// this channel's rows that are due is still pending.
	drain := func() {
		t.Helper()
		for range 50 {
			var pending int
			if err := pool.Raw().QueryRow(ctx,
				`SELECT count(*) FROM delivery_queue WHERE channel_id = $1 AND state = 'pending' AND next_try_at <= now()`,
				channelID).Scan(&pending); err != nil {
				t.Fatalf("count pending: %v", err)
			}
			if pending == 0 {
				return
			}
			if err := dw.Tick(ctx); err != nil {
				t.Fatalf("tick: %v", err)
			}
		}
		t.Fatal("the delivery worker never drained the queue")
	}
	state := func(where string, args ...any) string {
		t.Helper()
		var st string
		var reason *string
		if err := pool.Raw().QueryRow(ctx,
			`SELECT state, dead_reason FROM delivery_queue WHERE `+where, args...).Scan(&st, &reason); err != nil {
			t.Fatalf("read queue row: %v", err)
		}
		if reason != nil {
			st += "/" + *reason
		}
		return st
	}

	// The budget pauses a down check: its incident closes as plan_paused on
	// every run until it is closed, and its queued follow-up goes dead.
	paused := monitor("paused")
	pausedIncident := open(paused)
	if _, err := pool.Raw().Exec(ctx,
		`UPDATE monitor SET paused = true, paused_by = 'plan' WHERE id = $1`, paused); err != nil {
		t.Fatalf("plan-pause: %v", err)
	}
	for range 2 {
		if err := l.ClosePlanPaused(ctx); err != nil {
			t.Fatalf("close plan-paused: %v", err)
		}
	}
	var reason string
	if err := pool.Raw().QueryRow(ctx,
		`SELECT close_reason FROM incident WHERE id = $1 AND resolved_at IS NOT NULL`, pausedIncident).Scan(&reason); err != nil || reason != ReasonPlanPaused {
		t.Fatalf("plan-paused incident close_reason=%q err=%v, want %s", reason, err, ReasonPlanPaused)
	}
	if _, err := pool.Raw().Exec(ctx,
		`UPDATE delivery_queue SET next_try_at = now() WHERE incident_id = $1 AND class = 'followup'`, pausedIncident); err != nil {
		t.Fatalf("bring the follow-up due: %v", err)
	}
	drain()
	if got := state(`incident_id = $1 AND class = 'followup'`, pausedIncident); got != "dead/closed_unmeasured" {
		t.Fatalf("follow-up after a plan_paused close = %s, want dead/closed_unmeasured", got)
	}
	if got := state(`incident_id = $1 AND class = 'page'`, pausedIncident); got != "sent" {
		t.Fatalf("the outage page = %s, want sent", got)
	}

	// The project freezes with a page and a test in the queue.
	frozenIncident := open(monitor("frozen"))
	if err := pool.Queries().EnqueueDelivery(ctx, sqlc.EnqueueDeliveryParams{
		TenantID: tenantID, ChannelID: channelID, Class: "test",
		IdemKey: fmt.Sprintf("queued-test-%d", uniq), Payload: []byte(`{"title":"test"}`),
	}); err != nil {
		t.Fatalf("enqueue test: %v", err)
	}
	if _, err := pool.Raw().Exec(ctx, `UPDATE project SET frozen_at = now() WHERE id = $1`, projectID); err != nil {
		t.Fatalf("freeze: %v", err)
	}
	drain()
	if got := state(`incident_id = $1 AND class = 'page'`, frozenIncident); got != "dead/project_frozen" {
		t.Fatalf("a frozen project's queued page = %s, want dead/project_frozen", got)
	}
	if got := state(`idem_key = $1`, fmt.Sprintf("queued-test-%d", uniq)); got != "sent" {
		t.Fatalf("the owner's test send = %s, want sent", got)
	}
}
