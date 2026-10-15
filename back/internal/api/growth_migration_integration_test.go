//go:build integration

// Migration 013 backfills what already happened, on a database of its own
// migrated to 012 first: a person who has signed in has proven the address,
// or their next sign-in would count as a new account; a project that already
// sent install_verified is stamped, or its next SDK start would count as its
// first install; a workspace that already holds a check is stamped, or its
// next check would count as its first.
package api

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
)

func TestGrowthMigrationBackfillsWhatAlreadyHappened(t *testing.T) {
	dsn := os.Getenv("UC_TEST_POSTGRES")
	if dsn == "" {
		t.Skip("UC_TEST_POSTGRES not set; skipping migration integration test")
	}
	ctx := context.Background()
	base, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("uc_growth_mig_%d", time.Now().UnixNano())
	admin, err := pgx.Connect(ctx, base.String())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
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
	t.Cleanup(func() { _ = db.Close() })
	if err := goose.SetDialect("postgres"); err != nil {
		t.Fatal(err)
	}
	const dir = "../../../db/postgres"
	if err := goose.UpToContext(ctx, db, dir, 12); err != nil {
		t.Fatalf("goose up to 012: %v", err)
	}

	one := func(q string, args ...any) int64 {
		t.Helper()
		var id int64
		if err := db.QueryRowContext(ctx, q, args...).Scan(&id); err != nil {
			t.Fatalf("%q: %v", q, err)
		}
		return id
	}
	person := func(email string) int64 {
		return one(`INSERT INTO person (public_id, email, name, created_at)
		             VALUES (gen_random_uuid(), $1, $1, now() - interval '3 days') RETURNING id`, email)
	}
	signedIn, tracked, never := person("session@example.com"), person("event@example.com"), person("never@example.com")
	tenant := one(`INSERT INTO tenant (public_id, name, owner_person_id) VALUES (gen_random_uuid(), 'w', $1) RETURNING id`, signedIn)
	one(`INSERT INTO session (token_hash, person_id, tenant_id, expires_at)
	     VALUES (decode(md5('growth'), 'hex'), $1, $2, now() + interval '1 day') RETURNING id`, signedIn, tenant)
	if _, err := db.ExecContext(ctx,
		`INSERT INTO web_events (person_id, ts, name) VALUES ($1, now(), 'signed_in')`, tracked); err != nil {
		t.Fatal(err)
	}
	installed := one(`INSERT INTO project (public_id, tenant_id, domain) VALUES (gen_random_uuid(), $1, 'a.example.com') RETURNING id`, tenant)
	quiet := one(`INSERT INTO project (public_id, tenant_id, domain) VALUES (gen_random_uuid(), $1, 'b.example.com') RETURNING id`, tenant)
	for _, age := range []string{"2 days", "1 day"} {
		if _, err := db.ExecContext(ctx,
			`INSERT INTO events (tenant_id, project_id, ts, name, labels, actor)
			 VALUES ($1, $2, now() - $3::interval, 'install_verified', '{}', '')`, tenant, installed, age); err != nil {
			t.Fatal(err)
		}
	}
	target := one(`INSERT INTO probe_target (key, kind, url) VALUES ('growth-mig', 'website', 'https://a.example.com') RETURNING id`)
	one(`INSERT INTO monitor (public_id, tenant_id, project_id, kind, name, target, interval_sec, target_id, created_at)
	     VALUES (gen_random_uuid(), $1, $2, 'website', 'A', 'https://a.example.com', 300, $3, now() - interval '2 days')
	     RETURNING id`, tenant, installed, target)
	checkless := one(`INSERT INTO tenant (public_id, name, owner_person_id) VALUES (gen_random_uuid(), 'c', $1) RETURNING id`, tracked)

	if err := goose.UpContext(ctx, db, dir); err != nil {
		t.Fatalf("goose up through 013: %v", err)
	}
	is := func(q string, args ...any) bool {
		t.Helper()
		var ok bool
		if err := db.QueryRowContext(ctx, q, args...).Scan(&ok); err != nil {
			t.Fatalf("%q: %v", q, err)
		}
		return ok
	}
	for who, id := range map[string]int64{"a person with a session": signedIn, "a person who signed_in": tracked} {
		if !is(`SELECT email_verified_at IS NOT DISTINCT FROM created_at FROM person WHERE id = $1`, id) {
			t.Errorf("%s was not backfilled as verified", who)
		}
	}
	if !is(`SELECT email_verified_at IS NULL FROM person WHERE id = $1`, never) {
		t.Error("a person who never signed in was marked verified")
	}
	if !is(`SELECT install_verified_at IS NOT DISTINCT FROM
	               (SELECT min(ts) FROM events WHERE project_id = $1 AND name = 'install_verified')
	          FROM project WHERE id = $1`, installed) {
		t.Error("the installed project's install_verified_at is not its first install_verified")
	}
	if !is(`SELECT install_verified_at IS NULL FROM project WHERE id = $1`, quiet) {
		t.Error("a project that never sent install_verified was stamped")
	}
	if !is(`SELECT first_check_at IS NOT DISTINCT FROM (SELECT min(created_at) FROM monitor WHERE tenant_id = $1)
	          FROM tenant WHERE id = $1`, tenant) {
		t.Error("the workspace holding a check is not stamped with its oldest")
	}
	if !is(`SELECT first_check_at IS NULL FROM tenant WHERE id = $1`, checkless) {
		t.Error("a workspace with no check was stamped")
	}
}
