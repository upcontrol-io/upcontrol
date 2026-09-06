//go:build integration

// Integration test for the board's reads against a real Postgres with 001
// applied: the catalog of what a project sends and the bucketed series a
// widget draws. The logs half goes through ring.QueryBuilder, which is the
// only path to that table. Three tests, so three containers rather than one
// per assertion group.
// Run: go test -tags=integration ./internal/storage/pgstore/...
package pgstore

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"go.upcontrol.io/back/internal/ring/query"
)

// One tenant, two projects: the catalog is asserted on the first and the
// bucket arithmetic on the second, so neither seed can bend the other's counts.
const (
	seriesTenant   = 91
	catalogProject = 92
	bucketProject  = 93
)

// wrappedFingerprint is above MaxInt64, so it lands in the bigint column as
// -1: the catalog has to hand it back as the decimal text of the uint64 it was.
const wrappedFingerprint = uint64(18446744073709551615)

// runLogQuery executes a builder's query the way the API layer does.
func runLogQuery(t *testing.T, pool *pgxpool.Pool, lq query.LogQuery) [][]any {
	t.Helper()
	rows, err := pool.Query(context.Background(), lq.SQL, lq.Args...)
	if err != nil {
		t.Fatalf("query: %v\n%s", err, lq.SQL)
	}
	defer rows.Close()
	var out [][]any
	for rows.Next() {
		vals, err := rows.Values()
		if err != nil {
			t.Fatalf("values: %v", err)
		}
		out = append(out, vals)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	return out
}

func TestCatalogAndSeriesOverLogs(t *testing.T) {
	s, pool := openStore(t)
	ctx := context.Background()
	// A run just after UTC midnight would otherwise write into a day the
	// install-day partitions do not cover, and one such row fails the COPY.
	if _, _, err := s.RollLogPartitions(ctx, time.Now(), 1, 24*time.Hour); err != nil {
		t.Fatalf("roll partitions: %v", err)
	}

	at := time.Now().UTC().Add(-5 * time.Minute)
	catalog := []LogRow{
		{ProjectID: catalogProject, Service: "api", Level: "error", Message: "boom 1", Fingerprint: 7,
			Attrs: map[string]string{"route": "/checkout"}},
		{ProjectID: catalogProject, Service: "api", Level: "error", Message: "boom 2", Fingerprint: 7,
			Attrs: map[string]string{"route": "/checkout"}},
		{ProjectID: catalogProject, Service: "api", Level: "warn", Message: "slow query 412ms", Fingerprint: wrappedFingerprint,
			Attrs: map[string]string{"route": "/cart"}},
		// debug is neither error nor warn, so the catalog files it under info.
		{ProjectID: catalogProject, Service: "api", Level: "debug", Message: "tick", Fingerprint: 9},
		{ProjectID: catalogProject, Service: "worker", Level: "error", Message: "job failed", Fingerprint: 11},
	}
	for i := range catalog {
		catalog[i].TenantID, catalog[i].Source = seriesTenant, "sdk"
		catalog[i].TS = at.Add(time.Duration(i) * time.Second)
		catalog[i].Seq = uint64(i + 1)
	}
	if err := s.InsertLogs(ctx, catalog); err != nil {
		t.Fatalf("insert catalog logs: %v", err)
	}

	qb := query.New(seriesTenant, catalogProject)

	// --- groups ------------------------------------------------------------
	got := runLogQuery(t, pool, qb.Groups(7*24*time.Hour, 8))
	if len(got) != 4 {
		t.Fatalf("four (service, level, fingerprint) groups were written; got %d: %v", len(got), got)
	}
	type key struct{ service, level, fingerprint string }
	lines := map[key]int64{}
	samples := map[key]string{}
	for _, row := range got {
		k := key{row[0].(string), row[1].(string), strconv.FormatUint(uint64(row[2].(int64)), 10)}
		lines[k] = row[3].(int64)
		samples[k] = row[4].(string)
	}
	if n := lines[key{"api", "error", "7"}]; n != 2 {
		t.Fatalf("the two api errors share a fingerprint; got %d (%v)", n, lines)
	}
	// debug is not a bucket the API knows: it has to arrive as info, or a group
	// listed under a level cannot be filtered back out at that level.
	if n := lines[key{"api", "info", "9"}]; n != 1 {
		t.Fatalf("debug must be folded into info; got %v", lines)
	}
	warn := key{"api", "warn", strconv.FormatUint(wrappedFingerprint, 10)}
	if lines[warn] != 1 {
		t.Fatalf("a fingerprint above MaxInt64 must survive the int64 wrap; got %v", lines)
	}
	if samples[warn] != "slow query 412ms" {
		t.Fatalf("the newest message is the sample; got %q", samples[warn])
	}

	// --- attribute pairs ---------------------------------------------------
	pairs := map[string]int64{}
	for _, row := range runLogQuery(t, pool, qb.AttrPairs(7*24*time.Hour, 20000, 10)) {
		pairs[row[0].(string)+" "+row[1].(string)+"="+row[2].(string)] = row[3].(int64)
	}
	if pairs["api route=/checkout"] != 2 || pairs["api route=/cart"] != 1 {
		t.Fatalf("attribute pairs must count the lines that carry them; got %v", pairs)
	}
	// The bounded scan is a LIMIT inside a subquery: a project with nothing in
	// it must answer an empty list, not fail on the bound.
	if rows := runLogQuery(t, pool, query.New(seriesTenant, 999).AttrPairs(7*24*time.Hour, 20000, 10)); len(rows) != 0 {
		t.Fatalf("an empty project has no attribute pairs; got %v", rows)
	}

	// --- buckets -----------------------------------------------------------
	from := time.Now().UTC().Add(-10 * time.Minute).Truncate(time.Minute)
	series := []LogRow{
		{Service: "api", Level: "error", Message: "a", Fingerprint: 7, TS: from.Add(10 * time.Second),
			Attrs: map[string]string{"route": "/checkout"}},
		{Service: "api", Level: "error", Message: "b", Fingerprint: 7, TS: from.Add(20 * time.Second)},
		{Service: "worker", Level: "warn", Message: "c", Fingerprint: 8, TS: from.Add(70 * time.Second)},
	}
	for i := range series {
		series[i].TenantID, series[i].ProjectID = seriesTenant, bucketProject
		series[i].Seq, series[i].Source = uint64(i+1), "sdk"
	}
	if err := s.InsertLogs(ctx, series); err != nil {
		t.Fatalf("insert series logs: %v", err)
	}

	sqb := query.New(seriesTenant, bucketProject)
	within := query.Range{From: from, To: from.Add(10 * time.Minute)}
	counts := map[int64]int64{}
	for _, row := range runLogQuery(t, pool, sqb.SeriesBuckets(within, 60, query.SeriesFilter{})) {
		counts[row[0].(int64)] = row[1].(int64)
	}
	if counts[0] != 2 || counts[1] != 1 {
		t.Fatalf("buckets are indexed from the range start; got %v", counts)
	}
	service := "api"
	filtered := runLogQuery(t, pool, sqb.SeriesBuckets(within, 60, query.SeriesFilter{
		Service: &service, Level: "error", Attrs: map[string]string{"route": "/checkout"},
	}))
	if len(filtered) != 1 || filtered[0][1].(int64) != 1 {
		t.Fatalf("service + level + attribute must narrow to the one line; got %v", filtered)
	}
}

func TestCheckAndEventSeries(t *testing.T) {
	s, _ := openStore(t)
	ctx := context.Background()
	from := time.Now().UTC().Add(-10 * time.Minute).Truncate(time.Minute)

	probes := []CheckRow{
		{TS: from.Add(10 * time.Second), OK: true, TotalMs: 100},
		{TS: from.Add(20 * time.Second), OK: true, TotalMs: 300},
		// The third minute fails: uptime is a share, so a failed probe is not
		// a missing one.
		{TS: from.Add(140 * time.Second), OK: false, TotalMs: 900},
	}
	for i := range probes {
		probes[i].TenantID, probes[i].MonitorID, probes[i].Region = seriesTenant, 5, "ams"
	}
	if err := s.InsertChecks(ctx, probes); err != nil {
		t.Fatalf("insert checks: %v", err)
	}

	got, err := s.CheckBuckets(ctx, seriesTenant, 5, from, from.Add(10*time.Minute), 60)
	if err != nil {
		t.Fatalf("check buckets: %v", err)
	}
	byIndex := map[int64]CheckBucket{}
	for _, b := range got {
		byIndex[b.Index] = b
	}
	if len(byIndex) != 2 {
		t.Fatalf("only the minutes a probe ran in answer; got %v", byIndex)
	}
	if first := byIndex[0]; first.OK != 2 || first.Total != 2 || first.SumMs != 400 {
		t.Fatalf("the ok probes must sum, so an average over any span is one division; got %+v", first)
	}
	// Bucket 1 held no probe at all: absent, so the caller draws null rather
	// than 100% uptime for a minute nobody measured.
	if _, ok := byIndex[1]; ok {
		t.Fatalf("a minute without a probe must not appear; got %v", byIndex)
	}
	if third := byIndex[2]; third.OK != 0 || third.Total != 1 {
		t.Fatalf("a failed probe still counts towards the share; got %+v", third)
	}
	// An unknown monitor is an empty series, never an error: the widget names
	// a check that used to exist and the chart says so.
	if empty, err := s.CheckBuckets(ctx, seriesTenant, 4242, from, from.Add(10*time.Minute), 60); err != nil || len(empty) != 0 {
		t.Fatalf("an unknown monitor answers nothing; got %v %v", empty, err)
	}

	events := []EventRow{
		{Name: "signup", TS: from.Add(5 * time.Second)},
		{Name: "signup", TS: from.Add(15 * time.Second)},
		{Name: "checkout", TS: from.Add(75 * time.Second)},
	}
	for i := range events {
		events[i].TenantID, events[i].ProjectID = seriesTenant, catalogProject
	}
	if err := s.InsertEvents(ctx, events); err != nil {
		t.Fatalf("insert events: %v", err)
	}
	cat, err := s.CatalogEvents(ctx, seriesTenant, catalogProject, from.Add(-time.Hour))
	if err != nil {
		t.Fatalf("catalog events: %v", err)
	}
	if len(cat) != 2 || cat[0].Name != "signup" || cat[0].Times != 2 {
		t.Fatalf("the catalog lists each name once, most seen first; got %+v", cat)
	}
	buckets, err := s.EventBuckets(ctx, seriesTenant, catalogProject, "signup", from, from.Add(10*time.Minute), 60)
	if err != nil {
		t.Fatalf("event buckets: %v", err)
	}
	if len(buckets) != 1 || buckets[0].Index != 0 || buckets[0].Count != 2 {
		t.Fatalf("both signups sit in the first minute; got %+v", buckets)
	}
}

func TestMetricCatalogAndFunnelFold(t *testing.T) {
	s, _ := openStore(t)
	ctx := context.Background()
	from := time.Now().UTC().Add(-10 * time.Minute).Truncate(time.Minute)
	paid := map[string]string{"funnel": "visit to paid", "step": "paid", "i": "2"}
	rows := []MetricRow{
		{Name: "response_ms", TS: from, Labels: map[string]string{"route": "/checkout"}, Value: 120},
		{Name: "response_ms", TS: from.Add(time.Second), Labels: map[string]string{"route": "/cart"}, Value: 90},
		// The seed sits before the window; the fold needs it for the first delta.
		{Name: "funnel", TS: from.Add(-2 * time.Minute), Labels: paid, Value: 10},
		{Name: "funnel", TS: from.Add(time.Minute), Labels: paid, Value: 12},
		{Name: "funnel", TS: from.Add(2 * time.Minute), Labels: paid, Value: 3},
		{Name: "funnel", TS: from.Add(3 * time.Minute), Labels: paid, Value: 5},
		// The other steps arrive out of order on purpose: `i` is what orders them.
		{Name: "funnel", TS: from, Labels: map[string]string{"funnel": "visit to paid", "step": "visit", "i": "0"}, Value: 900},
		{Name: "funnel", TS: from, Labels: map[string]string{"funnel": "visit to paid", "step": "signup", "i": "1"}, Value: 40},
	}
	for i := range rows {
		rows[i].TenantID, rows[i].ProjectID = seriesTenant, catalogProject
	}
	if err := s.InsertMetrics(ctx, rows); err != nil {
		t.Fatalf("insert metrics: %v", err)
	}

	metrics, err := s.CatalogMetrics(ctx, seriesTenant, catalogProject, from.Add(-time.Hour))
	if err != nil {
		t.Fatalf("catalog metrics: %v", err)
	}
	if len(metrics) != 1 || metrics[0].Name != "response_ms" || metrics[0].Readings != 2 {
		t.Fatalf("funnel is a shape of its own and never a metric row; got %+v", metrics)
	}
	if len(metrics[0].Labels) != 1 || metrics[0].Labels[0] != "route" {
		t.Fatalf("the label keys are what a widget can narrow by; got %v", metrics[0].Labels)
	}

	funnels, err := s.CatalogFunnels(ctx, seriesTenant, catalogProject, from.Add(-time.Hour))
	if err != nil {
		t.Fatalf("catalog funnels: %v", err)
	}
	if len(funnels) != 1 || funnels[0].Name != "visit to paid" {
		t.Fatalf("one funnel was written; got %+v", funnels)
	}
	for i, step := range []string{"visit", "signup", "paid"} {
		if funnels[0].Steps[i] != step {
			t.Fatalf("steps must follow the `i` label; got %v", funnels[0].Steps)
		}
	}

	step := map[string]string{"funnel": "visit to paid", "step": "paid"}
	// All four readings inside one bucket. The first has nothing to be
	// measured against and so counts as nothing; 12 is +2, 3 is a reset worth
	// its own 3, and 5 is +2.
	whole, err := s.FunnelBuckets(ctx, seriesTenant, catalogProject, "funnel", step,
		from.Add(-5*time.Minute), from.Add(10*time.Minute), 900)
	if err != nil {
		t.Fatalf("funnel buckets: %v", err)
	}
	if len(whole) != 1 || whole[0].Index != 0 || whole[0].Sum != 7 {
		t.Fatalf("the fold is 2 + the reset's own 3 + 2 = 7; got %+v", whole)
	}

	// The same readings with the window starting after the first: 10 stays out
	// of the answer but seeds the delta, so the first bucket inside is +2 and
	// not the whole running total of 12. The other steps never appear — the
	// label filter is containment, not a prefix.
	folded, err := s.FunnelBuckets(ctx, seriesTenant, catalogProject, "funnel", step, from, from.Add(10*time.Minute), 60)
	if err != nil {
		t.Fatalf("funnel buckets: %v", err)
	}
	sums := map[int64]float64{}
	for _, b := range folded {
		sums[b.Index] = b.Sum
	}
	if len(sums) != 3 || sums[1] != 2 || sums[2] != 3 || sums[3] != 2 {
		t.Fatalf("the seed before the window is what the first delta is measured against; got %+v", folded)
	}

	// A span nothing was written into is an empty read, which the API renders
	// as zeros: no rows is an answer, not a failure.
	quiet, err := s.FunnelBuckets(ctx, seriesTenant, catalogProject, "funnel", step,
		from.Add(-2*time.Hour), from.Add(-time.Hour), 60)
	if err != nil || len(quiet) != 0 {
		t.Fatalf("a span with no readings answers nothing; got %+v %v", quiet, err)
	}

	// A read that could not run is an error, never an empty fold: zeros drawn
	// from a dead read are a measurement nobody took.
	dead, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := s.FunnelBuckets(dead, seriesTenant, catalogProject, "funnel", step, from, from.Add(10*time.Minute), 60); err == nil {
		t.Fatal("a failed read must return an error, not an empty fold")
	}

	if v, ok, err := s.MetricLast(ctx, seriesTenant, catalogProject, "funnel", step, from); err != nil || !ok || v != 10 {
		t.Fatalf("the seed is the last value before the window; got %v %v %v", v, ok, err)
	}
	if _, ok, _ := s.MetricLast(ctx, seriesTenant, catalogProject, "funnel", step, from.Add(-time.Hour)); ok {
		t.Fatal("a metric with no reading yet has no value, not 0")
	}

	buckets, err := s.MetricBuckets(ctx, seriesTenant, catalogProject, "funnel", step, from, from.Add(10*time.Minute), 60)
	if err != nil {
		t.Fatalf("metric buckets: %v", err)
	}
	if len(buckets) != 3 || buckets[0].Index != 1 || buckets[0].Avg != 12 || buckets[0].Last != 12 {
		t.Fatalf("one bucket per minute a reading arrived in; got %+v", buckets)
	}
}
