//go:build integration

// One product-alerts episode end to end: a drop fires once per channel, a
// second tick repeats nothing, the recovery is sent once, a rule past Free's
// two is not evaluated, and a thin week ago keeps a rule learning and quiet.
// Run with -tags=integration, UC_TEST_POSTGRES set.
package productalert

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"go.upcontrol.io/back/internal/migrate"
	"go.upcontrol.io/back/internal/storage/pg"
	"go.upcontrol.io/back/internal/storage/pgstore"
)

func TestTickEpisode(t *testing.T) {
	dsn := os.Getenv("UC_TEST_POSTGRES")
	if dsn == "" {
		t.Skip("UC_TEST_POSTGRES not set; skipping product alerts integration test")
	}
	ctx := context.Background()
	if err := migrate.Run(ctx, dsn, "../../../db/postgres"); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}
	pool, err := pg.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	t.Cleanup(pool.Close)

	uniq := time.Now().UnixNano()
	now := time.Now().UTC().Truncate(time.Second)
	scan := func(what, sql string, args ...any) int64 {
		t.Helper()
		var id int64
		if err := pool.Raw().QueryRow(ctx, sql, args...).Scan(&id); err != nil {
			t.Fatalf("seed %s: %v", what, err)
		}
		return id
	}
	exec := func(what, sql string, args ...any) {
		t.Helper()
		if _, err := pool.Raw().Exec(ctx, sql, args...); err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	}
	tenantID := scan("tenant", `INSERT INTO tenant (public_id, name, plan) VALUES (gen_random_uuid(), $1, 'Free') RETURNING id`,
		fmt.Sprintf("palert-%d", uniq))
	projectID := scan("project", `INSERT INTO project (public_id, tenant_id, domain) VALUES (gen_random_uuid(), $1, $2) RETURNING id`,
		tenantID, fmt.Sprintf("palert-%d.example", uniq))
	for i := range 2 {
		exec("seed channel", `INSERT INTO alert_channel (public_id, tenant_id, project_id, kind, target)
			VALUES (gen_random_uuid(), $1, $2, 'email', $3)`, tenantID, projectID, fmt.Sprintf("palert-%d-%d@example.com", uniq, i))
	}
	rule := func(event string, age time.Duration) int64 {
		return scan("rule", `INSERT INTO product_alert (tenant_id, project_id, event, kind, direction, percent, window_hours, created_at)
			VALUES ($1, $2, $3, 'change', 'down', 50, 6, $4) RETURNING id`, tenantID, projectID, event, now.Add(-age))
	}
	events := func(event string, n int, at time.Time) {
		exec("seed events", `INSERT INTO events (tenant_id, project_id, ts, name, labels)
			SELECT $1, $2, $3::timestamptz + make_interval(secs => g), $4, '{}' FROM generate_series(1, $5) g`,
			tenantID, projectID, at, event, n)
	}
	weekAgo := now.Add(-week - 3*time.Hour)

	signup := fmt.Sprintf("signup_%d", uniq)
	thin := fmt.Sprintf("thin_%d", uniq)
	extra := fmt.Sprintf("extra_%d", uniq)
	dropRule := rule(signup, 3*time.Hour)
	thinRule := rule(thin, 2*time.Hour)
	extraRule := rule(extra, time.Hour) // the third: past Free's two
	events(signup, 40, weekAgo)
	events(signup, 10, now.Add(-3*time.Hour))
	events(thin, 19, weekAgo)
	events(extra, 40, weekAgo)

	e := New(pool, pgstore.New(pool.Raw()), slog.New(slog.DiscardHandler))
	e.now = func() time.Time { return now }
	queued := func(ruleID int64, kind string) int {
		t.Helper()
		var n int
		if err := pool.Raw().QueryRow(ctx, `SELECT count(*) FROM delivery_queue WHERE idem_key LIKE $1`,
			fmt.Sprintf("palert:%d:%%:%s:%%", ruleID, kind)).Scan(&n); err != nil {
			t.Fatalf("count queued: %v", err)
		}
		return n
	}
	tick := func() {
		t.Helper()
		if err := e.Tick(ctx); err != nil {
			t.Fatalf("tick: %v", err)
		}
	}

	tick()
	if n := queued(dropRule, SendFired); n != 2 {
		t.Fatalf("%d fired rows, want one per channel (2)", n)
	}
	var title string
	if err := pool.Raw().QueryRow(ctx, `SELECT payload->>'title' FROM delivery_queue WHERE idem_key LIKE $1 LIMIT 1`,
		fmt.Sprintf("palert:%d:%%", dropRule)).Scan(&title); err != nil || !strings.HasPrefix(title, signup+" dropped 75%") {
		t.Fatalf("title %q err %v", title, err)
	}

	e.now = func() time.Time { return now.Add(5 * time.Minute) }
	tick()
	if n := queued(dropRule, SendFired); n != 2 {
		t.Fatalf("%d fired rows after a second tick, want still 2", n)
	}

	events(signup, 30, now.Add(-2*time.Hour))
	tick()
	tick()
	if n := queued(dropRule, SendRecovered); n != 2 {
		t.Fatalf("%d recovered rows, want one per channel (2)", n)
	}

	var state string
	if err := pool.Raw().QueryRow(ctx, `SELECT state FROM product_alert WHERE id = $1`, thinRule).Scan(&state); err != nil || state != StateLearning {
		t.Fatalf("thin rule state %q err %v, want learning", state, err)
	}
	if n := queued(thinRule, "%"); n != 0 {
		t.Fatalf("thin rule queued %d rows", n)
	}

	var checked *time.Time
	if err := pool.Raw().QueryRow(ctx, `SELECT last_checked_at FROM product_alert WHERE id = $1`, extraRule).Scan(&checked); err != nil || checked != nil {
		t.Fatalf("the third rule was evaluated: last_checked_at %v err %v", checked, err)
	}
	if n := queued(extraRule, "%"); n != 0 {
		t.Fatalf("the third rule queued %d rows", n)
	}
}
