//go:build integration

// The DNS token job (part 2's self-serve removal): the TXT-driven state
// change. Real DNS is untestable - the resolver is a
// package var, overridden here. Own database per test, like the api's lanes.
// Run with -tags=integration, UC_TEST_POSTGRES set.
package worker

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"go.upcontrol.io/back/internal/storage/pg"
)

func newWorkerDB(t *testing.T) *pg.Pool {
	t.Helper()
	dsn := os.Getenv("UC_TEST_POSTGRES")
	if dsn == "" {
		t.Skip("UC_TEST_POSTGRES not set; skipping dns-token integration test")
	}
	base, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("uc_g2_tokens_%d", time.Now().UnixNano())
	admin, err := pgx.Connect(context.Background(), base.String())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(context.Background(), "CREATE DATABASE "+name); err != nil {
		t.Fatalf("create database: %v", err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(), "DROP DATABASE "+name+" WITH (FORCE)")
		_ = admin.Close(context.Background())
	})
	mine := *base
	mine.Path = "/" + name
	db, err := sql.Open("pgx", mine.String())
	if err != nil {
		t.Fatal(err)
	}
	if err := goose.SetDialect("postgres"); err != nil {
		t.Fatal(err)
	}
	if err := goose.UpContext(context.Background(), db, "../../../db/postgres"); err != nil {
		t.Fatalf("goose up: %v", err)
	}
	_ = db.Close()
	pool, err := pg.Open(context.Background(), mine.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

var quietLogger = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

// seedTokenPage builds one unclaimed host page with its root target.
func seedTokenPage(t *testing.T, pool *pg.Pool, suffix string) (pageID, targetID int64, host string) {
	t.Helper()
	ctx := context.Background()
	host = fmt.Sprintf("%s-%d.example.com", suffix, time.Now().UnixNano()%100000)
	var tenantID, projectID int64
	if err := pool.Raw().QueryRow(ctx,
		`INSERT INTO tenant (public_id, name, claim_token_hash)
		 SELECT gen_random_uuid(), $1, decode(md5($1::text), 'hex') RETURNING id`, host).Scan(&tenantID); err != nil {
		t.Fatalf("tenant %s: %v", host, err)
	}
	if err := pool.Raw().QueryRow(ctx,
		`INSERT INTO project (public_id, tenant_id, domain) VALUES (gen_random_uuid(), $1, $2) RETURNING id`,
		tenantID, host).Scan(&projectID); err != nil {
		t.Fatalf("project %s: %v", host, err)
	}
	if err := pool.Raw().QueryRow(ctx,
		`INSERT INTO probe_target (key, kind, url, first_ok_at)
		 VALUES ($1, 'website', $2, now() - interval '3 days') RETURNING id`,
		"website\x1fhttps://"+host+"\x1f", "https://"+host).Scan(&targetID); err != nil {
		t.Fatal(err)
	}
	if err := pool.Raw().QueryRow(ctx,
		`INSERT INTO status_page (tenant_id, project_id, slug, title, root_target_id, is_host_page, minted_source)
		 VALUES ($1, $2, $3, $4, $5, true, 'watch') RETURNING id`,
		tenantID, projectID, suffix+fmt.Sprint(time.Now().UnixNano()), host, targetID).Scan(&pageID); err != nil {
		t.Fatalf("status_page %s: %v", host, err)
	}
	return pageID, targetID, host
}

// withTXT swaps the package's TXT resolver for a fake and restores it.
func withTXT(t *testing.T, txts map[string][]string) {
	t.Helper()
	old := lookupTXT
	lookupTXT = func(ctx context.Context, name string) ([]string, error) {
		if v, ok := txts[name]; ok {
			return v, nil
		}
		return nil, errors.New("no txt")
	}
	t.Cleanup(func() { lookupTXT = old })
}

// A removal record takes the page out, deletes the project's subscriptions
// on the root and blocks the host.
func TestDNSTokenRemovesThePage(t *testing.T) {
	pool := newWorkerDB(t)
	ctx := context.Background()
	removeID, removeTarget, _ := seedTokenPage(t, pool, "remove")
	if _, err := pool.Raw().Exec(ctx,
		`UPDATE status_page SET removal_token = 'tok-remove-1' WHERE id = $1`, removeID); err != nil {
		t.Fatal(err)
	}
	var removeProject int64
	_ = pool.Raw().QueryRow(ctx, `SELECT project_id FROM status_page WHERE id = $1`, removeID).Scan(&removeProject)
	if _, err := pool.Raw().Exec(ctx,
		`INSERT INTO monitor (public_id, tenant_id, project_id, kind, name, target, interval_sec, target_id)
		 SELECT gen_random_uuid(), tenant_id, project_id, 'website', 'Root', 'x', 300, $2 FROM status_page WHERE id = $1`,
		removeID, removeTarget); err != nil {
		t.Fatal(err)
	}
	withTXT(t, map[string][]string{
		"_upcontrol-remove.example.com": {"upcontrol-remove=tok-remove-1"},
	})

	if err := removeByToken(ctx, pool, quietLogger); err != nil {
		t.Fatalf("removeByToken: %v", err)
	}
	var removed, rootDropped, blocked int
	_ = pool.Raw().QueryRow(ctx,
		`SELECT count(*) FROM status_page WHERE id = $1 AND removed_at IS NOT NULL AND root_target_id IS NULL`, removeID).Scan(&removed)
	_ = pool.Raw().QueryRow(ctx,
		`SELECT count(*) FROM monitor WHERE project_id = $1 AND target_id = $2`, removeProject, removeTarget).Scan(&rootDropped)
	_ = pool.Raw().QueryRow(ctx,
		`SELECT count(*) FROM blocked_host WHERE domain = 'example.com' AND reason = 'self-serve TXT removal'`).Scan(&blocked)
	if removed != 1 {
		t.Fatal("the page was not removed by its TXT record")
	}
	if rootDropped != 0 {
		t.Fatal("the removed page's project kept its subscription on the root target")
	}
	if blocked != 1 {
		t.Fatal("the eTLD+1 was not written to blocked_host")
	}
}
