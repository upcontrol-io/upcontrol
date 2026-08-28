//go:build integration

// Integration test for storage/pgstore: real Postgres via testcontainers,
// apply the single 001_init migration and round-trip every writer.
// Run: go test -tags=integration ./internal/storage/pgstore/...
package pgstore

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

const (
	pgUser = "upcontrol"
	pgPass = "secret"
	pgDB   = "upcontrol"
)

// startPostgres starts one postgres:18.6 per test and returns its host:port.
func startPostgres(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	req := testcontainers.ContainerRequest{
		Image:        "postgres:18.6",
		ExposedPorts: []string{"5432/tcp"},
		Env: map[string]string{
			"POSTGRES_USER":     pgUser,
			"POSTGRES_PASSWORD": pgPass,
			"POSTGRES_DB":       pgDB,
		},
		// The line is printed twice on first boot: once by the temporary
		// initdb server, once by the real one. The second means ready.
		WaitingFor: wait.ForLog("database system is ready to accept connections").
			WithOccurrence(2).WithStartupTimeout(180 * time.Second),
	}
	c, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: req, Started: true,
	})
	if err != nil {
		t.Fatalf("start postgres: %v", err)
	}
	t.Cleanup(func() { _ = c.Terminate(ctx) })
	endpoint, err := c.PortEndpoint(ctx, "5432", "")
	if err != nil {
		t.Fatal(err)
	}
	return endpoint
}

// applyMigration execs db/postgres/001_init.sql statement by statement, the
// whole schema in one file since the 28-migration collapse.
func applyMigration(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	path := filepath.Join("..", "..", "..", "..", "db", "postgres", "001_init.sql")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read migration: %v", err)
	}
	for _, stmt := range splitStatements(splitUp(string(body))) {
		stmt = strings.TrimSpace(stmt)
		if stmt == "" {
			continue
		}
		if _, err := pool.Exec(context.Background(), stmt); err != nil {
			t.Fatalf("exec 001_init statement (%s…): %v", firstLine(stmt), err)
		}
	}
}

func splitUp(s string) string {
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

func splitStatements(s string) []string {
	// Strip whole comment lines first: the migration's header comment contains
	// ';', which would split mid-comment.
	var keep []string
	for _, line := range strings.Split(s, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "--") {
			continue
		}
		keep = append(keep, line)
	}
	// Split on ';' but not inside a $$ dollar-quoted body (the partition DO
	// block carries semicolons of its own).
	s = strings.Join(keep, "\n")
	var stmts []string
	start, inDollar := 0, false
	for i := 0; i < len(s); i++ {
		switch {
		case strings.HasPrefix(s[i:], "$$"):
			inDollar = !inDollar
			i++
		case !inDollar && s[i] == ';':
			stmts = append(stmts, s[start:i])
			start = i + 1
		}
	}
	return append(stmts, s[start:])
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

func openStore(t *testing.T) (*Store, *pgxpool.Pool) {
	t.Helper()
	pool, err := pgxpool.New(context.Background(),
		fmt.Sprintf("postgres://%s:%s@%s/%s", pgUser, pgPass, startPostgres(t), pgDB))
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := pool.Ping(context.Background()); err != nil {
		t.Fatalf("ping: %v", err)
	}
	applyMigration(t, pool)
	return New(pool), pool
}

func TestInsertLogsRoundTrip(t *testing.T) {
	s, pool := openStore(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)
	rows := []LogRow{
		{TenantID: 1, ProjectID: 1, TS: now, Seq: 1, Source: "sdk", Service: "app",
			Host: "h1", Level: "error", LevelRaw: "FATAL", Message: "boom",
			Fingerprint: 42, Attrs: map[string]string{"k": "v"}},
		{TenantID: 1, ProjectID: 1, TS: now, Seq: 2, Source: "sdk", Service: "app",
			Host: "h1", Level: "info", Message: "ok"},
	}
	if err := s.InsertLogs(ctx, rows); err != nil {
		t.Fatalf("InsertLogs: %v", err)
	}

	var n int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM logs WHERE tenant_id=1").Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 2 {
		t.Fatalf("row count = %d, want 2", n)
	}

	var msg, lvl, lvlRaw string
	var fp int64
	var attrsRaw []byte
	if err := pool.QueryRow(ctx,
		"SELECT message, level, level_raw, fingerprint, attrs FROM logs WHERE seq=1").
		Scan(&msg, &lvl, &lvlRaw, &fp, &attrsRaw); err != nil {
		t.Fatalf("select seq=1: %v", err)
	}
	if msg != "boom" || lvl != "error" {
		t.Errorf("seq=1 got msg=%q lvl=%q", msg, lvl)
	}
	// The client's own spelling is annotated, never rewritten over.
	if lvlRaw != "FATAL" || fp != 42 {
		t.Errorf("seq=1 got level_raw=%q fingerprint=%d, want FATAL/42", lvlRaw, fp)
	}
	var attrs map[string]string
	if err := json.Unmarshal(attrsRaw, &attrs); err != nil {
		t.Fatalf("attrs not json: %v", err)
	}
	if attrs["k"] != "v" {
		t.Errorf("attrs = %v, want k=v", attrs)
	}
}

func TestBumpSeriesAccumulates(t *testing.T) {
	s, pool := openStore(t)
	ctx := context.Background()
	minute := time.Now().UTC().Truncate(time.Minute)
	first := SeriesBump{TenantID: 1, ProjectID: 1, Minute: minute, Source: "sdk",
		Level: "error", Lines: 10, Bytes: 4096}
	// Two keys in one call: the multi-row builder's placeholder numbering must
	// line up with its args, which a single-row call never exercises.
	other := SeriesBump{TenantID: 1, ProjectID: 1, Minute: minute.Add(time.Minute),
		Source: "sdk", Level: "info", Lines: 3, Bytes: 100}
	if err := s.BumpSeries(ctx, []SeriesBump{first, other}); err != nil {
		t.Fatalf("BumpSeries 1: %v", err)
	}
	if err := s.BumpSeries(ctx, []SeriesBump{first}); err != nil {
		t.Fatalf("BumpSeries 2: %v", err)
	}

	// Same key twice must ACCUMULATE, not overwrite — the one property the
	// old materialized view guaranteed.
	var lines, bytes int64
	if err := pool.QueryRow(ctx,
		"SELECT lines, bytes FROM series_1m WHERE tenant_id=1 AND project_id=1 AND minute=$1 AND source='sdk' AND level='error'",
		minute).Scan(&lines, &bytes); err != nil {
		t.Fatalf("select series_1m: %v", err)
	}
	if lines != 20 || bytes != 8192 {
		t.Errorf("accumulated lines=%d bytes=%d, want 20/8192", lines, bytes)
	}
	var n int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM series_1m").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("series_1m rows = %d, want 2 (second key landed once)", n)
	}
}

func TestInsertChecksAndWebEventsRoundTrip(t *testing.T) {
	s, pool := openStore(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)

	if err := s.InsertChecks(ctx, []CheckRow{{
		TenantID: 1, MonitorID: 5, TS: now, Region: "eu", OK: true, StatusCode: 200,
		ErrorClass: "", DNSMs: 1, ConnectMs: 2, TLSMs: 3, TTFBMs: 4, TotalMs: 10,
		BodyHash: 0xdeadbeef,
	}}); err != nil {
		t.Fatalf("InsertChecks: %v", err)
	}
	var ok bool
	var statusCode, totalMs int
	var bodyHash int64
	if err := pool.QueryRow(ctx,
		"SELECT ok, status_code, total_ms, body_hash FROM checks WHERE tenant_id=1 AND monitor_id=5").
		Scan(&ok, &statusCode, &totalMs, &bodyHash); err != nil {
		t.Fatalf("select checks: %v", err)
	}
	if !ok || statusCode != 200 || totalMs != 10 || bodyHash != 0xdeadbeef {
		t.Errorf("checks got ok=%v status=%d total=%d hash=%d", ok, statusCode, totalMs, bodyHash)
	}

	ip := [8]byte{1, 2, 3, 4, 5, 6, 7, 8}
	if err := s.InsertWebEvents(ctx, []WebEventRow{{
		TS: now, VisitorID: 7, PersonID: 3, TenantID: 1, Name: "pageview",
		Path: "/app/logs", Title: "Logs", Country: "DE", IPHash: ip,
		Device: "desktop", OS: "windows", Browser: "firefox",
		Props: map[string]string{"a": "b"},
	}}); err != nil {
		t.Fatalf("InsertWebEvents: %v", err)
	}
	var visitorID int64
	var ipBack []byte
	var propsRaw []byte
	if err := pool.QueryRow(ctx,
		"SELECT visitor_id, ip_hash, props FROM web_events WHERE tenant_id=1").
		Scan(&visitorID, &ipBack, &propsRaw); err != nil {
		t.Fatalf("select web_events: %v", err)
	}
	if visitorID != 7 || !bytes.Equal(ipBack, ip[:]) {
		t.Errorf("web_events visitor=%d ip=%v, want 7 %v", visitorID, ipBack, ip)
	}
	var props map[string]string
	if err := json.Unmarshal(propsRaw, &props); err != nil {
		t.Fatalf("props not json: %v", err)
	}
	if props["a"] != "b" {
		t.Errorf("props = %v, want a=b", props)
	}
}

func TestEventsAroundPicksClosestAndReturnsTimeOrder(t *testing.T) {
	s, _ := openStore(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)
	rows := []EventRow{
		// 7 minutes from the pivot: inside the window, but only second-closest.
		{TenantID: 1, ProjectID: 1, TS: now.Add(-9 * time.Minute), Name: "deploy.succeeded",
			Labels: map[string]string{"sha": "a1b2c3d", "service": "api"}},
		// 1 minute from the pivot: the closest, so first by distance.
		{TenantID: 1, ProjectID: 1, TS: now.Add(-1 * time.Minute), Name: "payment_failed"},
		// Another tenant's event: must not leak into the answer.
		{TenantID: 2, ProjectID: 1, TS: now, Name: "payment_failed"},
		// Outside the window entirely.
		{TenantID: 1, ProjectID: 1, TS: now.Add(-30 * time.Minute), Name: "deploy.ancient"},
	}
	if err := s.InsertEvents(ctx, rows); err != nil {
		t.Fatalf("InsertEvents: %v", err)
	}
	pivot := now.Add(-2 * time.Minute)
	got, err := s.EventsAround(ctx, 1, now.Add(-10*time.Minute), now, pivot, 5)
	if err != nil {
		t.Fatalf("EventsAround: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d events, want 2 (tenant+window scoped): %+v", len(got), got)
	}
	// Time order, not distance order: the chronology renders, the pivot only
	// spent the budget.
	if got[0].Name != "deploy.succeeded" || got[1].Name != "payment_failed" {
		t.Fatalf("order = [%s, %s], want deploy before payment", got[0].Name, got[1].Name)
	}
	// labels ride as jsonb and must come back as the map the card reads.
	if got[0].Labels["sha"] != "a1b2c3d" {
		t.Errorf("labels = %v, want sha=a1b2c3d", got[0].Labels)
	}
}

func TestLastDeployAt(t *testing.T) {
	s, _ := openStore(t)
	ctx := context.Background()
	deployAt := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Microsecond)
	rows := []EventRow{
		{TenantID: 1, ProjectID: 1, TS: deployAt, Name: "deploy.succeeded"},
		// Newer, but not a deploy: must not win.
		{TenantID: 1, ProjectID: 1, TS: time.Now().UTC(), Name: "payment_succeeded"},
		// A deploy of the same tenant, another project: must not leak.
		{TenantID: 1, ProjectID: 2, TS: deployAt.Add(time.Hour), Name: "deployment_created"},
	}
	if err := s.InsertEvents(ctx, rows); err != nil {
		t.Fatalf("InsertEvents: %v", err)
	}
	got, err := s.LastDeployAt(ctx, 1, 1)
	if err != nil {
		t.Fatalf("LastDeployAt: %v", err)
	}
	if !got.Equal(deployAt) {
		t.Errorf("LastDeployAt = %v, want %v", got, deployAt)
	}
	// No deploy at all: max() is NULL over an empty set — the zero time, not
	// an error and not the Unix epoch.
	none, err := s.LastDeployAt(ctx, 7, 7)
	if err != nil {
		t.Fatalf("LastDeployAt (empty): %v", err)
	}
	if !none.IsZero() {
		t.Errorf("LastDeployAt (empty) = %v, want zero time", none)
	}
}

func TestMetricSummaryShipsOnlySevenDayNames(t *testing.T) {
	s, _ := openStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	// signups: 8 days of span, three readings — 20 and 30 are inside the 7-day
	// range window, 10 fell out of it.
	rows := []MetricRow{
		{TenantID: 1, ProjectID: 1, TS: now.Add(-8 * 24 * time.Hour), Name: "signups", Value: 10},
		{TenantID: 1, ProjectID: 1, TS: now.Add(-4 * 24 * time.Hour), Name: "signups", Value: 20},
		{TenantID: 1, ProjectID: 1, TS: now.Add(-1 * time.Hour), Name: "signups", Value: 30},
		// A young name: one day of history produces no tile.
		{TenantID: 1, ProjectID: 1, TS: now.Add(-1 * time.Hour), Name: "brand_new", Value: 5},
	}
	if err := s.InsertMetrics(ctx, rows); err != nil {
		t.Fatalf("InsertMetrics: %v", err)
	}
	stats, err := s.MetricSummary(ctx, 1)
	if err != nil {
		t.Fatalf("MetricSummary: %v", err)
	}
	if len(stats) != 1 || stats[0].Name != "signups" {
		t.Fatalf("stats = %+v, want exactly signups (under 7 days ships nothing)", stats)
	}
	st := stats[0]
	if st.Latest != 30 {
		t.Errorf("Latest = %v, want 30 (the max-ts row's value)", st.Latest)
	}
	if st.Days < 8 {
		t.Errorf("Days = %d, want >= 8 (8-day span plus one)", st.Days)
	}
	// p10/p90 of [20, 30] interpolate linearly: 21 and 29.
	if math.Abs(st.P10-21) > 1e-6 || math.Abs(st.P90-29) > 1e-6 {
		t.Errorf("P10/P90 = %v/%v, want 21/29", st.P10, st.P90)
	}
	// The 12-hour spark holds the one hourly mean that exists: [30].
	if len(st.Spark) != 1 || st.Spark[0] != 30 {
		t.Errorf("Spark = %v, want [30]", st.Spark)
	}
}

func TestErrorWindowSumsTheRollup(t *testing.T) {
	s, _ := openStore(t)
	ctx := context.Background()
	minute := time.Now().UTC().Truncate(time.Minute)
	bumps := []SeriesBump{
		{TenantID: 1, ProjectID: 1, Minute: minute, Source: "sdk", Level: "error", Lines: 3},
		{TenantID: 1, ProjectID: 1, Minute: minute, Source: "sdk", Level: "info", Lines: 7},
		// The next minute is OUTSIDE [from, to): must not count.
		{TenantID: 1, ProjectID: 1, Minute: minute.Add(time.Minute), Source: "sdk", Level: "error", Lines: 2},
	}
	if err := s.BumpSeries(ctx, bumps); err != nil {
		t.Fatalf("BumpSeries: %v", err)
	}
	errs, total, err := s.ErrorWindow(ctx, 1, 1, minute.Add(-time.Minute), minute.Add(time.Minute))
	if err != nil {
		t.Fatalf("ErrorWindow: %v", err)
	}
	if errs != 3 || total != 10 {
		t.Errorf("ErrorWindow = %d/%d, want 3 error of 10 total", errs, total)
	}
	// An empty window is (0, 0, nil) — the detector's floor reads it, not an error.
	errs, total, err = s.ErrorWindow(ctx, 9, 9, minute, minute.Add(time.Minute))
	if err != nil || errs != 0 || total != 0 {
		t.Errorf("ErrorWindow (empty) = %d/%d/%v, want 0/0/<nil>", errs, total, err)
	}
}

func TestErrorRateBaselineMedianAndMADByHand(t *testing.T) {
	s, _ := openStore(t)
	ctx := context.Background()
	start := time.Now().UTC().Truncate(time.Minute).Add(-20 * time.Minute)
	// Three 5-minute buckets with hand-computable rates 0.1 / 0.3 / 0.5:
	// each bucket is 10 lines, 1/3/5 of them errors.
	var bumps []SeriesBump
	for i, errLines := range []int64{1, 3, 5} {
		m := start.Add(time.Duration(i) * 5 * time.Minute)
		bumps = append(bumps,
			SeriesBump{TenantID: 1, ProjectID: 1, Minute: m, Source: "sdk", Level: "error", Lines: errLines},
			SeriesBump{TenantID: 1, ProjectID: 1, Minute: m, Source: "sdk", Level: "info", Lines: 10 - errLines},
		)
	}
	// A traffic-free bucket inside the range: no rate exists, the HAVING must
	// drop it — keeping it would drag the median toward an unmeasured number.
	free := start.Add(17 * time.Minute)
	bumps = append(bumps, SeriesBump{TenantID: 1, ProjectID: 1, Minute: free, Source: "sdk", Level: "info", Lines: 0})
	if err := s.BumpSeries(ctx, bumps); err != nil {
		t.Fatalf("BumpSeries: %v", err)
	}

	median, mad, err := s.ErrorRateBaseline(ctx, 1, 1, start, start.Add(20*time.Minute))
	if err != nil {
		t.Fatalf("ErrorRateBaseline: %v", err)
	}
	// median of {0.1, 0.3, 0.5} is 0.3; deviations {0.2, 0, 0.2} give MAD 0.2.
	// A percentile_cont dialect mistake answers something plausible — these
	// exact numbers are what make it fail loudly instead.
	if math.Abs(median-0.3) > 1e-9 {
		t.Errorf("median = %v, want 0.3", median)
	}
	if math.Abs(mad-0.2) > 1e-9 {
		t.Errorf("mad = %v, want 0.2", mad)
	}

	// An empty baseline is the (0, 0, nil) contract, not NaN and not an error.
	median, mad, err = s.ErrorRateBaseline(ctx, 9, 9, start, start.Add(time.Hour))
	if err != nil || median != 0 || mad != 0 {
		t.Errorf("empty baseline = %v/%v/%v, want 0/0/<nil>", median, mad, err)
	}
}
