//go:build integration

// Naming a sign-in project re-claims its service slug (prj-N) from the domain;
// hand-picked slugs never rewrite. Run with -tags=integration, UC_TEST_POSTGRES set.
package api

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"go.upcontrol.io/back/internal/migrate"
	"go.upcontrol.io/back/internal/storage/pg"
)

func TestNamingReclaimsTheServiceSlug(t *testing.T) {
	dsn := os.Getenv("UC_TEST_POSTGRES")
	if dsn == "" {
		t.Skip("UC_TEST_POSTGRES not set; skipping slug integration test")
	}
	ctx := context.Background()
	// back/internal/api → three levels up is the repo root (same depth the HA
	// test uses from back/test/ha).
	if err := migrate.Run(ctx, dsn, "", "", "", "", "../../../db/postgres", ""); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}
	pool, err := pg.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	t.Cleanup(pool.Close)

	// A sign-in-shaped account: tenant + project with no domain, and a page row
	// still on the service slug (what a first save before any check produces).
	uniq := time.Now().UnixNano()
	tenant := fmt.Sprintf("slugtest-%d", uniq)
	domain := fmt.Sprintf("example-%d.com", uniq%100000)
	var tenantID, projectID int64
	// public_id is a uuid column: mint it in the database, never hand-typed
	// (hand-built strings are not uuid syntax).
	if err := pool.Raw().QueryRow(ctx,
		`INSERT INTO tenant (public_id, name) VALUES (gen_random_uuid(), $1) RETURNING id`,
		tenant).Scan(&tenantID); err != nil {
		t.Fatalf("tenant: %v", err)
	}
	if err := pool.Raw().QueryRow(ctx,
		`INSERT INTO project (public_id, tenant_id, domain) VALUES (gen_random_uuid(), $1, '') RETURNING id`,
		tenantID).Scan(&projectID); err != nil {
		t.Fatalf("project: %v", err)
	}
	service := fmt.Sprintf("prj-%d", projectID)
	if _, err := pool.Raw().Exec(ctx,
		`INSERT INTO status_page (tenant_id, project_id, slug, title) VALUES ($1, $2, $3, $4)`,
		tenantID, projectID, service, domain); err != nil {
		t.Fatalf("status_page: %v", err)
	}

	h := &Monitors{pool: pool}
	slugOf := func() string {
		var slug string
		if err := pool.Raw().QueryRow(ctx,
			`SELECT slug FROM status_page WHERE project_id = $1`, projectID).Scan(&slug); err != nil {
			t.Fatalf("read slug: %v", err)
		}
		return slug
	}

	// The first website check names the project and re-claims the slug.
	h.nameProjectIfUnnamed(ctx, projectID, "website", "https://"+domain+"/checkout")
	if got := slugOf(); got == service || got == "" {
		t.Fatalf("slug after naming = %q, want the domain-based slug (was %q)", got, service)
	}
	var named string
	_ = pool.Raw().QueryRow(ctx, `SELECT domain FROM project WHERE id = $1`, projectID).Scan(&named)
	if named != domain {
		t.Fatalf("project domain = %q, want %q", named, domain)
	}
	// The old service address still resolves to the same tenant — that is the
	// alias publicStatus gives prj-N for free (project-id fallback).
	var viaFallback int64
	if err := pool.Raw().QueryRow(ctx,
		`SELECT tenant_id FROM project WHERE id = $1`, projectID).Scan(&viaFallback); err != nil || viaFallback != tenantID {
		t.Fatalf("prj-N fallback: err=%v tenant=%d want=%d", err, viaFallback, tenantID)
	}

	// A second naming of an already-named project is a no-op on both rows.
	h.nameProjectIfUnnamed(ctx, projectID, "website", "https://other.example.com")
	if got := slugOf(); got == service {
		t.Fatal("a second naming must not touch the slug")
	}
	_ = pool.Raw().QueryRow(ctx, `SELECT domain FROM project WHERE id = $1`, projectID).Scan(&named)
	if named != domain {
		t.Fatalf("domain after second naming = %q, want unchanged %q", named, domain)
	}
}

// The sign-in door creates no status_page row: naming must CREATE it under
// the domain slug, or a fresh account keeps handing out prj-N.
func TestNamingCreatesThePageRowWhenNoneExists(t *testing.T) {
	dsn := os.Getenv("UC_TEST_POSTGRES")
	if dsn == "" {
		t.Skip("UC_TEST_POSTGRES not set; skipping slug integration test")
	}
	ctx := context.Background()
	if err := migrate.Run(ctx, dsn, "", "", "", "", "../../../db/postgres", ""); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}
	pool, err := pg.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	t.Cleanup(pool.Close)

	// A sign-in-shaped account: unnamed project and NO status_page row, what
	// /v1/auth/magic-link provisions.
	uniq := time.Now().UnixNano()
	domain := fmt.Sprintf("fresh-%d.com", uniq%100000)
	var tenantID, projectID int64
	if err := pool.Raw().QueryRow(ctx,
		`INSERT INTO tenant (public_id, name) VALUES (gen_random_uuid(), 'fresh-account') RETURNING id`,
	).Scan(&tenantID); err != nil {
		t.Fatalf("tenant: %v", err)
	}
	if err := pool.Raw().QueryRow(ctx,
		`INSERT INTO project (public_id, tenant_id, domain) VALUES (gen_random_uuid(), $1, '') RETURNING id`,
		tenantID).Scan(&projectID); err != nil {
		t.Fatalf("project: %v", err)
	}

	h := &Monitors{pool: pool}
	h.nameProjectIfUnnamed(ctx, projectID, "website", "https://"+domain)

	var slug, title string
	if err := pool.Raw().QueryRow(ctx,
		`SELECT slug, title FROM status_page WHERE project_id = $1`, projectID).Scan(&slug, &title); err != nil {
		t.Fatalf("naming a fresh account must create its status_page row: %v", err)
	}
	if service := fmt.Sprintf("prj-%d", projectID); slug == service || slug == "" {
		t.Fatalf("created slug = %q, want the domain-based one", slug)
	}
	if title != domain {
		t.Fatalf("created title = %q, want the host %q", title, domain)
	}
}

// A hand-picked slug is never rewritten: the UPDATE matches only the service
// slug, so a page somebody has already shared under its own name stays put.
func TestNamingNeverRewritesAHandPickedSlug(t *testing.T) {
	dsn := os.Getenv("UC_TEST_POSTGRES")
	if dsn == "" {
		t.Skip("UC_TEST_POSTGRES not set; skipping slug integration test")
	}
	ctx := context.Background()
	if err := migrate.Run(ctx, dsn, "", "", "", "", "../../../db/postgres", ""); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}
	pool, err := pg.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	t.Cleanup(pool.Close)

	uniq := time.Now().UnixNano()
	var tenantID, projectID int64
	if err := pool.Raw().QueryRow(ctx,
		`INSERT INTO tenant (public_id, name) VALUES (gen_random_uuid(), 'keeps-slug') RETURNING id`,
	).Scan(&tenantID); err != nil {
		t.Fatalf("tenant: %v", err)
	}
	if err := pool.Raw().QueryRow(ctx,
		`INSERT INTO project (public_id, tenant_id, domain) VALUES (gen_random_uuid(), $1, '') RETURNING id`,
		tenantID).Scan(&projectID); err != nil {
		t.Fatalf("project: %v", err)
	}
	chosen := fmt.Sprintf("chosen-%d", uniq%100000)
	if _, err := pool.Raw().Exec(ctx,
		`INSERT INTO status_page (tenant_id, project_id, slug, title) VALUES ($1, $2, $3, 'mine')`,
		tenantID, projectID, chosen); err != nil {
		t.Fatalf("status_page: %v", err)
	}

	h := &Monitors{pool: pool}
	h.nameProjectIfUnnamed(ctx, projectID, "website", "https://picked.example.com")

	var slug string
	if err := pool.Raw().QueryRow(ctx,
		`SELECT slug FROM status_page WHERE project_id = $1`, projectID).Scan(&slug); err != nil {
		t.Fatalf("read slug: %v", err)
	}
	if slug != chosen {
		t.Fatalf("slug = %q, want the hand-picked %q — a shared address must not be renamed under its owner", slug, chosen)
	}
}
