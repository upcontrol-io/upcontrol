package errorlog

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

var now = time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC)

func recent() Group {
	return Group{Fingerprint: 42, Count: 1, Service: "api", Message: "boom", LastTS: now.Add(-30 * time.Second)}
}

func TestShouldFireNew_FirstAppearanceFires(t *testing.T) {
	if !ShouldFireNew(recent(), nil, now) {
		t.Fatal("a fingerprint that never alerted must fire")
	}
}

func TestShouldFireNew_CooldownHolds(t *testing.T) {
	last := now.Add(-NewErrorCooldown / 2)
	if ShouldFireNew(recent(), &last, now) {
		t.Fatal("inside the cooldown a persisting error must stay quiet")
	}
	old := now.Add(-NewErrorCooldown)
	if !ShouldFireNew(recent(), &old, now) {
		t.Fatal("after the cooldown the fingerprint may fire again")
	}
}

func TestShouldFireNew_OldNoiseDoesNotFire(t *testing.T) {
	g := recent()
	g.LastTS = now.Add(-Lookback - time.Minute)
	if ShouldFireNew(g, nil, now) {
		t.Fatal("an error last seen before the lookback did not just appear")
	}
}

func TestShouldFireRepeat_ThresholdAndWindow(t *testing.T) {
	window := 5 * time.Minute
	g := recent()
	g.Count = 1
	if ShouldFireRepeat(g, nil, window, now) {
		t.Fatal("a single line is not repeating")
	}
	g.Count = RepeatThreshold
	if !ShouldFireRepeat(g, nil, window, now) {
		t.Fatal("threshold crossed with no prior alert must fire")
	}
	last := now.Add(-window / 2)
	if ShouldFireRepeat(g, &last, window, now) {
		t.Fatal("a steadily-repeating error pages once per window, not per tick")
	}
	old := now.Add(-window)
	if !ShouldFireRepeat(g, &old, window, now) {
		t.Fatal("one full window after the last alert it may fire again")
	}
}

func TestTitles(t *testing.T) {
	g := recent()
	if got := NewErrorTitle(g); got != "Error in api: boom" {
		t.Fatalf("NewErrorTitle = %q", got)
	}
	g.Service = ""
	if got := NewErrorTitle(g); got != "Error: boom" {
		t.Fatalf("NewErrorTitle without service = %q", got)
	}
	g.Count = 7
	if got := RepeatTitle(g, 5); got != "Repeating error (7× in 5 min): boom" {
		t.Fatalf("RepeatTitle = %q", got)
	}
}

func TestTitleTruncatesOnRuneBoundary(t *testing.T) {
	g := recent()
	g.Message = strings.Repeat("é", 200) // 2 bytes per rune: byte-slicing would split one
	title := NewErrorTitle(g)
	if !strings.HasSuffix(title, "…") {
		t.Fatalf("long message must be truncated, got %q", title)
	}
	if strings.ContainsRune(title, '�') {
		t.Fatal("truncation split a rune")
	}
}

// Two projects of one workspace carrying the SAME fingerprint alert
// independently: the scanner's memory is keyed per project, so one project
// remembering an error must not silence the other. UC_TEST_POSTGRES unset = skip.
func TestTick_ProjectsRememberSeparately(t *testing.T) {
	dsn := os.Getenv("UC_TEST_POSTGRES")
	if dsn == "" {
		t.Skip("UC_TEST_POSTGRES not set; skipping per-project scan test")
	}
	ctx := context.Background()
	pool, err := pg.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	t.Cleanup(pool.Close)
	// Under an advisory lock: `go test ./...` runs packages in parallel and two
	// goose runs against one fresh database collide on the objects they are
	// both creating.
	conn, err := pool.Raw().Acquire(ctx)
	if err != nil {
		t.Fatalf("lock connection: %v", err)
	}
	defer conn.Release()
	const migrateLock = 0x75636d67 // "ucmg"
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock($1)", migrateLock); err != nil {
		t.Fatalf("migration lock: %v", err)
	}
	if err := migrate.Run(ctx, dsn, "../../../../db/postgres"); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_unlock($1)", migrateLock); err != nil {
		t.Fatalf("migration unlock: %v", err)
	}

	var tenantID int64
	if err := pool.Raw().QueryRow(ctx,
		`INSERT INTO tenant (public_id, name) VALUES (gen_random_uuid(), $1) RETURNING id`,
		fmt.Sprintf("errlog-%d", time.Now().UnixNano())).Scan(&tenantID); err != nil {
		t.Fatalf("tenant: %v", err)
	}
	// One fingerprint, both projects: the same error, twice over.
	fp := time.Now().UnixNano()
	seed := func(domain string) (projectID, channelID int64) {
		if err := pool.Raw().QueryRow(ctx,
			`INSERT INTO project (public_id, tenant_id, domain) VALUES (gen_random_uuid(), $1, $2) RETURNING id`,
			tenantID, domain).Scan(&projectID); err != nil {
			t.Fatalf("project %s: %v", domain, err)
		}
		if err := pool.Raw().QueryRow(ctx,
			`INSERT INTO alert_channel (public_id, tenant_id, project_id, kind, target, notify)
			 VALUES (gen_random_uuid(), $1, $2, 'email', $3, '{"errorLogs":true}'::jsonb) RETURNING id`,
			tenantID, projectID, "errlog-"+domain).Scan(&channelID); err != nil {
			t.Fatalf("channel %s: %v", domain, err)
		}
		if _, err := pool.Raw().Exec(ctx,
			`INSERT INTO logs (tenant_id, project_id, ts, seq, source, service, host, level, message, fingerprint)
			 VALUES ($1, $2, now(), 1, 'test', 'api', 'h', 'error', 'boom', $3)`,
			tenantID, projectID, fp); err != nil {
			t.Fatalf("log line for %s: %v", domain, err)
		}
		return projectID, channelID
	}
	oneProject, oneChannel := seed("one.example")
	twoProject, twoChannel := seed("two.example")

	s := New(pool, pgstore.New(pool.Raw()), slog.New(slog.DiscardHandler))
	if err := s.Tick(ctx); err != nil {
		t.Fatalf("tick: %v", err)
	}

	for _, tc := range []struct {
		name                 string
		projectID, channelID int64
	}{
		{"one.example", oneProject, oneChannel},
		{"two.example", twoProject, twoChannel},
	} {
		var states, deliveries int
		if err := pool.Raw().QueryRow(ctx,
			`SELECT count(*) FROM error_alert_state
			  WHERE tenant_id = $1 AND project_id = $2 AND fingerprint = $3 AND kind = 'error'`,
			tenantID, tc.projectID, fp).Scan(&states); err != nil {
			t.Fatalf("read %s state: %v", tc.name, err)
		}
		if states != 1 {
			t.Fatalf("%s alert-state rows = %d, want 1 — the memory is per project, not per workspace", tc.name, states)
		}
		if err := pool.Raw().QueryRow(ctx,
			`SELECT count(*) FROM delivery_queue WHERE channel_id = $1`, tc.channelID).Scan(&deliveries); err != nil {
			t.Fatalf("read %s queue: %v", tc.name, err)
		}
		if deliveries != 1 {
			t.Fatalf("%s deliveries = %d, want 1 — a sibling's alert must neither silence nor page this project", tc.name, deliveries)
		}
	}
}
