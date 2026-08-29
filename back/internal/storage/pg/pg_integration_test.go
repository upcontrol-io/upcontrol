//go:build integration

// Integration test for storage/pg against a real Postgres (DSN: UC_TEST_POSTGRES);
// key resolver, idempotency, seq leaser. Run: go test -tags=integration ./internal/storage/pg/...
package pg

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib"

	sqlc "go.upcontrol.io/back/gen/pg"
)

func startPostgres(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("UC_TEST_POSTGRES")
	if dsn == "" {
		t.Skip("UC_TEST_POSTGRES not set; skipping pg integration test")
	}
	return dsn
}

// applyMigrations runs every goose migration in db/postgres; idempotent via
// ON CONFLICT for the seed rows. A fresh DB is the operator's responsibility.
func applyMigrations(t *testing.T, dsn string) {
	t.Helper()
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	dir := filepath.Join("..", "..", "..", "..", "db", "postgres")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		body, _ := os.ReadFile(filepath.Join(dir, e.Name()))
		up := splitPGUp(string(body))
		for _, stmt := range splitPGStatements(up) {
			stmt = strings.TrimSpace(stmt)
			if stmt == "" {
				continue
			}
			if _, err := db.Exec(stmt); err != nil {
				t.Fatalf("migrate %s: %v", e.Name(), err)
			}
		}
	}
}

func splitPGUp(s string) string {
	i := strings.Index(s, "-- +goose StatementBegin")
	if i < 0 {
		return s
	}
	rest := s[i+len("-- +goose StatementBegin"):]
	j := strings.Index(rest, "-- +goose Down")
	if j < 0 {
		return rest
	}
	return rest[:j]
}

// splitPGStatements splits on `;`, except where one sits inside a dollar-quoted
// block, which is body text rather than a terminator. 001_init.sql bootstraps
// its log partitions in a DO $$ ... $$ holding four.
func splitPGStatements(s string) []string {
	var keep []string
	for _, line := range strings.Split(s, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "--") {
			continue
		}
		keep = append(keep, line)
	}
	s = strings.Join(keep, "\n")

	var out []string
	start, tag := 0, ""
	for i := 0; i < len(s); i++ {
		if tag != "" {
			if strings.HasPrefix(s[i:], tag) {
				i += len(tag) - 1
				tag = ""
			}
			continue
		}
		if t := dollarTag(s[i:]); t != "" {
			tag = t
			i += len(t) - 1
			continue
		}
		if s[i] == ';' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	return append(out, s[start:])
}

// dollarTag returns the $$ or $tag$ opener at the head of s, or "". A bare
// placeholder like $1 is not one: a tag may not start with a digit.
func dollarTag(s string) string {
	if len(s) < 2 || s[0] != '$' {
		return ""
	}
	for i := 1; i < len(s); i++ {
		c := s[i]
		if c == '$' {
			return s[:i+1]
		}
		letter := c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
		digit := i > 1 && c >= '0' && c <= '9'
		if !letter && !digit {
			return ""
		}
	}
	return ""
}

// openPool builds a *Pool with the generated Queries wired in.
func openPool(t *testing.T, dsn string) *Pool {
	t.Helper()
	db, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
	return &Pool{db: db, q: sqlc.New(db)}
}

func seedKey(t *testing.T, db *pgxpool.Pool, fullKey, state string, rotatingUntil *time.Time) {
	t.Helper()
	prefix, ok := extractPrefix(fullKey)
	if !ok {
		t.Fatalf("bad test key: %s", fullKey)
	}
	sum := sha256.Sum256([]byte(fullKey))
	ctx := context.Background()
	_, _ = db.Exec(ctx, `INSERT INTO tenant (public_id, name) VALUES ('018f0000-0000-7000-8000-000000000001','acme') ON CONFLICT DO NOTHING`)
	_, _ = db.Exec(ctx, `INSERT INTO project (public_id, tenant_id, domain) VALUES ('018f0000-0000-7000-8000-000000000002', 1, 'example.com') ON CONFLICT DO NOTHING`)
	var rt any
	if rotatingUntil != nil {
		rt = *rotatingUntil
	}
	_, err := db.Exec(ctx,
		`INSERT INTO api_key (tenant_id, project_id, prefix, secret_hash, state, rotating_until)
		 VALUES (1, 1, $1, $2, $3, $4)`,
		prefix, sum[:], state, rt)
	if err != nil {
		t.Fatalf("seed api_key: %v", err)
	}
}

func TestResolveActiveKey(t *testing.T) {
	dsn := startPostgres(t)
	applyMigrations(t, dsn)
	pool := openPool(t, dsn)
	r := NewKeyResolver(pool, nil)
	full := "uc_live_8f2ac41d9b0eSecret123456"
	seedKey(t, pool.db, full, "active", nil)

	tenant, err := r.Resolve(context.Background(), full)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if tenant.TenantID != 1 || tenant.ProjectID != 1 {
		t.Errorf("tenant = %+v, want {1 1}", tenant)
	}
}

func TestResolveWrongSecretFails(t *testing.T) {
	dsn := startPostgres(t)
	pool := openPool(t, dsn)
	r := NewKeyResolver(pool, nil)
	full := "uc_live_aaaaaaaaaaaCorrectSecret"
	seedKey(t, pool.db, full, "active", nil)
	if _, err := r.Resolve(context.Background(), "uc_live_aaaaaaaaaaaWrongSecret"); err == nil {
		t.Error("wrong secret should fail")
	}
}

func TestResolveRevokedFails(t *testing.T) {
	dsn := startPostgres(t)
	pool := openPool(t, dsn)
	r := NewKeyResolver(pool, nil)
	full := "uc_live_bbbbbbbbbbbCorrectSecret"
	seedKey(t, pool.db, full, "revoked", nil)
	if _, err := r.Resolve(context.Background(), full); err == nil {
		t.Error("revoked key should fail")
	}
}

func TestResolveRotatingWithinWindow(t *testing.T) {
	dsn := startPostgres(t)
	pool := openPool(t, dsn)
	r := NewKeyResolver(pool, nil)
	full := "uc_live_cccccccccccCorrectSecret"
	future := time.Now().Add(24 * time.Hour)
	seedKey(t, pool.db, full, "rotating", &future)
	if _, err := r.Resolve(context.Background(), full); err != nil {
		t.Errorf("rotating within window should succeed: %v", err)
	}
}

func TestResolveRotatingExpiredFails(t *testing.T) {
	dsn := startPostgres(t)
	pool := openPool(t, dsn)
	r := NewKeyResolver(pool, nil)
	full := "uc_live_dddddddddddCorrectSecret"
	past := time.Now().Add(-1 * time.Hour)
	seedKey(t, pool.db, full, "rotating", &past)
	if _, err := r.Resolve(context.Background(), full); err == nil {
		t.Error("expired rotating key should fail")
	}
}

func TestIdempotencyReplay(t *testing.T) {
	dsn := startPostgres(t)
	pool := openPool(t, dsn)
	idm := NewIdempotency(pool)
	ctx := context.Background()
	r1, a1, err := idm.Claim(ctx, "batch-1", []byte("bodyhash"), 42)
	if err != nil {
		t.Fatal(err)
	}
	if r1 || a1 != 42 {
		t.Errorf("first claim: replay=%v accepted=%d", r1, a1)
	}
	r2, a2, err := idm.Claim(ctx, "batch-1", []byte("bodyhash"), 99)
	if err != nil {
		t.Fatal(err)
	}
	if !r2 || a2 != 42 {
		t.Errorf("replay: replay=%v accepted=%d, want true/42", r2, a2)
	}
}

func TestSeqLeaseDisjoint(t *testing.T) {
	dsn := startPostgres(t)
	pool := openPool(t, dsn)
	_, err := pool.db.Exec(context.Background(),
		`INSERT INTO project_seq (project_id, next) VALUES (1, 1) ON CONFLICT DO NOTHING`)
	if err != nil {
		t.Fatal(err)
	}
	sl := NewSeqLeaser(pool)
	ctx := context.Background()
	s1, err := sl.LeaseSeqBlock(ctx, 1, 10000)
	if err != nil {
		t.Fatal(err)
	}
	s2, err := sl.LeaseSeqBlock(ctx, 1, 10000)
	if err != nil {
		t.Fatal(err)
	}
	if s1+10000 != s2 {
		t.Errorf("blocks not disjoint: %d, %d", s1, s2)
	}
}
