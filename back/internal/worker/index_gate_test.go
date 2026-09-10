//go:build integration

// The index gate (plan part 4) and the DNS token jobs (part 2's removal,
// part 4's verification): qualification, the ramp's ceilings, hysteresis,
// and the TXT-driven state changes. Real DNS is untestable - the resolvers
// are package vars, overridden here. Own database per test, like the api's
// lanes. Run with -tags=integration, UC_TEST_POSTGRES set.
package worker

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"go.upcontrol.io/back/internal/platform/config"
	"go.upcontrol.io/back/internal/storage/pg"
)

func newGateWorld(t *testing.T) *pg.Pool {
	t.Helper()
	dsn := os.Getenv("UC_TEST_POSTGRES")
	if dsn == "" {
		t.Skip("UC_TEST_POSTGRES not set; skipping index-gate integration test")
	}
	base, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("uc_g2_gate_%d", time.Now().UnixNano())
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

// seedGatePage builds one host page with a root target and 72 h of history
// at the asked cadence and quality. failRate is the share of measured rows
// that failed; unmeasured is the share of could-not-measure rows.
func seedGatePage(t *testing.T, pool *pg.Pool, suffix string, failRate, unmeasured float64, firstOK bool) (pageID, targetID int64, host string) {
	t.Helper()
	ctx := context.Background()
	host = fmt.Sprintf("%s-%d.example.com", suffix, time.Now().UnixNano()%100000)
	if err := pool.Raw().QueryRow(ctx,
		`INSERT INTO tenant (public_id, name, claim_token_hash)
		 SELECT gen_random_uuid(), $1, decode(md5($1::text), 'hex') RETURNING id`, host).Scan(&pageID); err != nil {
		t.Fatalf("tenant %s: %v", host, err)
	}
	// pageID is the tenant id here; the page comes below.
	var tenantID int64
	tenantID = pageID
	var projectID int64
	if err := pool.Raw().QueryRow(ctx,
		`INSERT INTO project (public_id, tenant_id, domain) VALUES (gen_random_uuid(), $1, $2) RETURNING id`,
		tenantID, host).Scan(&projectID); err != nil {
		t.Fatalf("project %s: %v", host, err)
	}
	if err := pool.Raw().QueryRow(ctx,
		`INSERT INTO probe_target (key, kind, url, first_ok_at)
		 VALUES ($1, 'website', $2, CASE WHEN $3 THEN now() - interval '3 days' ELSE NULL END) RETURNING id`,
		"website\x1fhttps://"+host+"\x1f", "https://"+host, firstOK).Scan(&targetID); err != nil {
		t.Fatal(err)
	}
	if err := pool.Raw().QueryRow(ctx,
		`INSERT INTO status_page (tenant_id, project_id, slug, title, root_target_id, is_host_page, minted_source)
		 VALUES ($1, $2, $3, $3, $4, true, $5) RETURNING id`,
		tenantID, projectID, slugify(host), targetID, pageSource(suffix)).Scan(&pageID); err != nil {
		t.Fatalf("status_page %s: %v", host, err)
	}
	// 72 hours of 300 s checks: 864 rows.
	rows := make([][]any, 0, 864)
	for i := 863; i >= 0; i-- {
		ok := true
		if float64(i%100) < failRate*100 {
			ok = false
		}
		errClass := ""
		code := 200
		if !ok {
			errClass, code = "connect", 0
		}
		if float64(i%100) < unmeasured*100 {
			ok, errClass, code = false, "status", 403
		}
		rows = append(rows, []any{targetID, 300,
			time.Now().Add(-time.Duration(i) * 300 * time.Second),
			"default", ok, code, errClass, 120})
	}
	for _, r := range rows {
		if _, err := pool.Raw().Exec(ctx,
			`INSERT INTO checks (target_id, interval_sec, ts, region, ok, status_code, error_class, total_ms)
			 VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
			r[0], r[1], r[2], r[3], r[4], r[5], r[6], r[7]); err != nil {
			t.Fatalf("seed check row: %v", err)
		}
	}
	return pageID, targetID, host
}

// pageSource maps the test suffix to a minted_source: "seed-*" pages are
// seeded pages for the origin rule.
func pageSource(suffix string) string {
	if len(suffix) > 5 && suffix[:5] == "seed-" {
		return "seed"
	}
	return "watch"
}

func slugify(host string) string {
	var b []byte
	prevDash := true
	for _, r := range host {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			b = append(b, byte(r))
			prevDash = false
		case !prevDash:
			b = append(b, '-')
			prevDash = true
		}
	}
	return string(b)
}

// withResolvers swaps the package's DNS probes for fakes and restores them.
// The default host fake: the gate's random-label wildcard probes answer
// NXDOMAIN (no host is wildcard), and bare hosts answer a non-NotFound
// failure, so the NXDOMAIN half of hysteresis stays silent unless a test
// says otherwise.
func withResolvers(t *testing.T, hostFn func(host string) ([]string, error), txts map[string][]string) {
	t.Helper()
	oldHost := lookupHost
	oldTXT := lookupTXT
	lookupHost = func(host string) ([]string, error) {
		if hostFn != nil {
			return hostFn(host)
		}
		if isRandomLabelProbe(host) {
			return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
		}
		return nil, errors.New("servfail")
	}
	lookupTXT = func(ctx context.Context, name string) ([]string, error) {
		if v, ok := txts[name]; ok {
			return v, nil
		}
		return nil, errors.New("no txt")
	}
	t.Cleanup(func() { lookupHost, lookupTXT = oldHost, oldTXT })
}

// isRandomLabelProbe answers whether the name is the gate's wildcard probe:
// a bare 8-hex-char label in front of the host.
func isRandomLabelProbe(host string) bool {
	i := strings.IndexByte(host, '.')
	if i != 8 {
		return false
	}
	for j := 0; j < 8; j++ {
		c := host[j]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// A page with clean 72 h continuity qualifies and is stamped; a
// 60%-failure page does not.
func TestGateStampsOnlyContinuousPages(t *testing.T) {
	pool := newGateWorld(t)
	goodID, _, goodHost := seedGatePage(t, pool, "watch", 0, 0, true)
	badID, _, _ := seedGatePage(t, pool, "watch", 0.6, 0, true)
	withResolvers(t, nil, nil)

	g := newIndexGate(config.StatusPageKnobs{IndexRampPerDay: 25, IndexMaxPages: 300})
	g.tick(context.Background(), pool, quietLogger)

	ctx := context.Background()
	var stamped int
	_ = pool.Raw().QueryRow(ctx, `SELECT count(*) FROM status_page WHERE id = $1 AND indexed_at IS NOT NULL`, goodID).Scan(&stamped)
	if stamped != 1 {
		t.Fatalf("the continuous page (%s) was not stamped", goodHost)
	}
	_ = pool.Raw().QueryRow(ctx, `SELECT count(*) FROM status_page WHERE id = $1 AND indexed_at IS NOT NULL`, badID).Scan(&stamped)
	if stamped != 0 {
		t.Fatalf("a 60%% failure page was stamped: continuity was not consulted")
	}
}

// A wildcard-DNS host does not qualify even with clean continuity; a
// claimed page without host_verified_at is not even a candidate.
func TestGateRefusesWildcardDNSAndUnverifiedClaims(t *testing.T) {
	pool := newGateWorld(t)
	ctx := context.Background()
	wildID, _, wildHost := seedGatePage(t, pool, "watch", 0, 0, true)
	// Wildcard: every name under the host resolves, the random-label probe
	// included.
	withResolvers(t, func(host string) ([]string, error) {
		if strings.HasSuffix(host, wildHost) {
			return []string{"192.0.2.1"}, nil
		}
		return nil, errors.New("servfail")
	}, nil)

	g := newIndexGate(config.StatusPageKnobs{IndexRampPerDay: 25, IndexMaxPages: 300})
	g.tick(context.Background(), pool, quietLogger)
	var stamped int
	_ = pool.Raw().QueryRow(ctx, `SELECT count(*) FROM status_page WHERE id = $1 AND indexed_at IS NOT NULL`, wildID).Scan(&stamped)
	if stamped != 0 {
		t.Fatal("a wildcard-DNS host was stamped")
	}

	// A claimed, opted-in page without the TXT proof is not a candidate at
	// all: the stamp never lands however clean its continuity is.
	claimID, targetID, claimHost := seedGatePage(t, pool, "watch", 0, 0, true)
	var tenantID int64
	_ = pool.Raw().QueryRow(ctx, `SELECT tenant_id FROM status_page WHERE id = $1`, claimID).Scan(&tenantID)
	if _, err := pool.Raw().Exec(ctx,
		`UPDATE tenant SET claim_token_hash = NULL WHERE id = $1`, tenantID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Raw().Exec(ctx,
		`UPDATE status_page SET index_opt_in = true, is_host_page = false WHERE id = $1`, claimID); err != nil {
		t.Fatal(err)
	}
	_ = targetID
	g.tick(context.Background(), pool, quietLogger)
	_ = pool.Raw().QueryRow(ctx, `SELECT count(*) FROM status_page WHERE id = $1 AND indexed_at IS NOT NULL`, claimID).Scan(&stamped)
	if stamped != 0 {
		t.Fatalf("a claimed page without host_verified_at was stamped (%s)", claimHost)
	}
}

// A seeded page qualifies only with interaction: without any it stays
// unindexed; a last_seen_at stamp (a visit) qualifies it.
func TestSeedPagesNeedInteraction(t *testing.T) {
	pool := newGateWorld(t)
	ctx := context.Background()
	untouchedID, _, _ := seedGatePage(t, pool, "seed-cold", 0, 0, true)
	visitedID, _, _ := seedGatePage(t, pool, "seed-warm", 0, 0, true)
	if _, err := pool.Raw().Exec(ctx,
		`UPDATE status_page SET last_seen_at = now() WHERE id = $1`, visitedID); err != nil {
		t.Fatal(err)
	}
	withResolvers(t, nil, nil)

	g := newIndexGate(config.StatusPageKnobs{IndexRampPerDay: 25, IndexMaxPages: 300})
	g.tick(context.Background(), pool, quietLogger)

	var stamped int
	_ = pool.Raw().QueryRow(ctx, `SELECT count(*) FROM status_page WHERE id = $1 AND indexed_at IS NOT NULL`, untouchedID).Scan(&stamped)
	if stamped != 0 {
		t.Fatal("a seeded page nobody touched was stamped")
	}
	_ = pool.Raw().QueryRow(ctx, `SELECT count(*) FROM status_page WHERE id = $1 AND indexed_at IS NOT NULL`, visitedID).Scan(&stamped)
	if stamped != 1 {
		t.Fatal("a seeded page with a visit (last_seen_at) was not stamped")
	}
}

// The ramp stamps at most IndexRampPerDay a day and never past
// IndexMaxPages; a hold blocks re-entry until the operator clears it.
func TestGateRampAndHolds(t *testing.T) {
	pool := newGateWorld(t)
	ctx := context.Background()
	// Three qualifying pages, ramp of 2: two stamped today, one left.
	ids := make([]int64, 0, 3)
	for i := 0; i < 3; i++ {
		id, _, _ := seedGatePage(t, pool, "watch", 0, 0, true)
		ids = append(ids, id)
	}
	withResolvers(t, nil, nil)

	g := newIndexGate(config.StatusPageKnobs{IndexRampPerDay: 2, IndexMaxPages: 300})
	g.tick(context.Background(), pool, quietLogger)
	var total int
	_ = pool.Raw().QueryRow(ctx, `SELECT count(*) FROM status_page WHERE indexed_at IS NOT NULL`).Scan(&total)
	if total != 2 {
		t.Fatalf("stamped = %d, want 2 (the day's ramp)", total)
	}

	// A second run the same day stamps nothing more.
	g.tick(context.Background(), pool, quietLogger)
	_ = pool.Raw().QueryRow(ctx, `SELECT count(*) FROM status_page WHERE indexed_at IS NOT NULL`).Scan(&total)
	if total != 2 {
		t.Fatalf("stamped after the second same-day run = %d, want still 2", total)
	}

	// The instance ceiling: with the cap equal to the stamped count, no new
	// page enters even on a fresh day.
	id4, _, _ := seedGatePage(t, pool, "watch", 0, 0, true)
	g2 := newIndexGate(config.StatusPageKnobs{IndexRampPerDay: 25, IndexMaxPages: 2})
	g2.tick(context.Background(), pool, quietLogger)
	var stamped4 int
	_ = pool.Raw().QueryRow(ctx, `SELECT count(*) FROM status_page WHERE id = $1 AND indexed_at IS NOT NULL`, id4).Scan(&stamped4)
	if stamped4 != 0 {
		t.Fatal("a page was stamped past IndexMaxPages")
	}

	// A hold blocks re-entry: set one, clear the stamp's day by hand, and
	// the ramp still skips the page.
	if _, err := pool.Raw().Exec(ctx,
		`UPDATE status_page SET indexed_at = NULL, reindex_hold = true WHERE id = $1`, ids[0]); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Raw().Exec(ctx,
		`UPDATE status_page SET indexed_at = now() - interval '2 days' WHERE indexed_at IS NOT NULL`); err != nil {
		t.Fatal(err)
	}
	g3 := newIndexGate(config.StatusPageKnobs{IndexRampPerDay: 25, IndexMaxPages: 300})
	g3.tick(context.Background(), pool, quietLogger)
	var held int
	_ = pool.Raw().QueryRow(ctx, `SELECT count(*) FROM status_page WHERE id = $1 AND indexed_at IS NOT NULL`, ids[0]).Scan(&held)
	if held != 0 {
		t.Fatal("a held page re-entered the index without the operator clearing the hold")
	}
}

// Hysteresis: an indexed page whose continuity broke for 7 days leaves the
// index with a hold; an NXDOMAIN host leaves too.
func TestGateHysteresisTakesBrokenPagesOut(t *testing.T) {
	pool := newGateWorld(t)
	ctx := context.Background()
	healthyID, _, _ := seedGatePage(t, pool, "watch", 0, 0, true)
	if _, err := pool.Raw().Exec(ctx,
		`UPDATE status_page SET indexed_at = now() WHERE id = $1`, healthyID); err != nil {
		t.Fatal(err)
	}
	// A broken page: indexed, but its history is 7 days of failure.
	brokenID, brokenTarget, _ := seedGatePage(t, pool, "watch", 0, 0, true)
	if _, err := pool.Raw().Exec(ctx, `DELETE FROM checks WHERE target_id = $1`, brokenTarget); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 7; i++ {
		if _, err := pool.Raw().Exec(ctx,
			`INSERT INTO checks (target_id, interval_sec, ts, region, ok, status_code, error_class, total_ms)
			 VALUES ($1, 300, now() - make_interval(days => $2::int), 'default', false, 0, 'connect', 0)`,
			brokenTarget, i); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pool.Raw().Exec(ctx,
		`UPDATE status_page SET indexed_at = now() WHERE id = $1`, brokenID); err != nil {
		t.Fatal(err)
	}
	// An NXDOMAIN host: indexed and continuous, but the host is gone.
	goneID, _, goneHost := seedGatePage(t, pool, "watch", 0, 0, true)
	if _, err := pool.Raw().Exec(ctx,
		`UPDATE status_page SET indexed_at = now() WHERE id = $1`, goneID); err != nil {
		t.Fatal(err)
	}
	withResolvers(t, func(host string) ([]string, error) {
		if host == goneHost || len(host) > len(goneHost) && host[len(host)-len(goneHost):] == goneHost {
			return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
		}
		return nil, errors.New("servfail") // other hosts: not found-by-name, not NXDOMAIN-clean either
	}, nil)

	g := newIndexGate(config.StatusPageKnobs{IndexRampPerDay: 25, IndexMaxPages: 300})
	g.tick(context.Background(), pool, quietLogger)

	var healthy int
	_ = pool.Raw().QueryRow(ctx, `SELECT count(*) FROM status_page WHERE id = $1 AND indexed_at IS NOT NULL AND NOT reindex_hold`, healthyID).Scan(&healthy)
	if healthy != 1 {
		t.Fatal("a healthy indexed page was taken out of the index")
	}
	for _, id := range []int64{brokenID, goneID} {
		var out, held int
		_ = pool.Raw().QueryRow(ctx, `SELECT count(*) FROM status_page WHERE id = $1 AND indexed_at IS NULL`, id).Scan(&out)
		_ = pool.Raw().QueryRow(ctx, `SELECT count(*) FROM status_page WHERE id = $1 AND reindex_hold`, id).Scan(&held)
		if out != 1 || held != 1 {
			t.Fatalf("page %d: left = %d held = %d, want 1/1", id, out, held)
		}
	}
}

// TXT verification: a claimed page whose _upcontrol-verify record carries
// the token is stamped and the token cleared; a removal record takes the
// page out, deletes the project's subscriptions on the root and blocks the
// host.
func TestDNSTokensVerifyAndRemove(t *testing.T) {
	pool := newGateWorld(t)
	ctx := context.Background()
	verifyID, _, verifyHost := seedGatePage(t, pool, "watch", 0, 0, true)
	if _, err := pool.Raw().Exec(ctx,
		`UPDATE status_page SET verification_token = 'tok-verify-1' WHERE id = $1`, verifyID); err != nil {
		t.Fatal(err)
	}
	removeID, removeTarget, _ := seedGatePage(t, pool, "watch", 0, 0, true)
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
	withResolvers(t, nil, map[string][]string{
		"_upcontrol-verify.example.com": {"upcontrol-verify=tok-verify-1"},
		"_upcontrol-remove.example.com": {"upcontrol-remove=tok-remove-1"},
	})

	if err := verifyHostTokens(ctx, pool, quietLogger); err != nil {
		t.Fatalf("verifyHostTokens: %v", err)
	}
	var verified, tokenCleared int
	_ = pool.Raw().QueryRow(ctx,
		`SELECT count(*) FROM status_page WHERE id = $1 AND host_verified_at IS NOT NULL`, verifyID).Scan(&verified)
	_ = pool.Raw().QueryRow(ctx,
		`SELECT count(*) FROM status_page WHERE id = $1 AND verification_token IS NULL`, verifyID).Scan(&tokenCleared)
	if verified != 1 || tokenCleared != 1 {
		t.Fatalf("verification: verified = %d token cleared = %d, want 1/1 (%s)", verified, tokenCleared, verifyHost)
	}

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
