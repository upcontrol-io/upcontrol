package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	apigen "go.upcontrol.io/back/gen/api"
	"go.upcontrol.io/back/internal/storage/pgstore"
)

func TestValidateSeries_AcceptsAWellFormedBatch(t *testing.T) {
	msg := validateSeries([]seriesQuery{
		{ID: "w1:0", Source: "logs", Range: "24h", Where: map[string]string{"service": "api", "level": "warn"}},
		{ID: "w2:0", Source: "check", Range: "7d", Name: "uptime", Where: map[string]string{"check": "abc"}},
		{ID: "w3:0", Source: "metric", Range: "365d", Name: "funnel"},
	})
	if msg != "" {
		t.Fatalf("a valid batch must be accepted; got %q", msg)
	}
}

func TestValidateSeries_RejectsBeyondTheBatchCap(t *testing.T) {
	// The cap is what keeps one board's render from being an unbounded fan-out
	// of queries; the message has to say which limit was passed.
	qs := make([]seriesQuery, maxSeriesQueries+1)
	for i := range qs {
		qs[i] = seriesQuery{ID: fmt.Sprintf("w%d", i), Source: "logs", Range: "1h"}
	}
	if msg := validateSeries(qs); msg == "" {
		t.Fatalf("%d queries must be rejected", len(qs))
	}
	if msg := validateSeries(qs[:maxSeriesQueries]); msg != "" {
		t.Fatalf("exactly %d queries must be accepted; got %q", maxSeriesQueries, msg)
	}
}

func TestValidateSeries_RejectsWhatCannotBeAnswered(t *testing.T) {
	cases := []struct {
		name string
		qs   []seriesQuery
	}{
		{"empty batch", nil},
		{"no id", []seriesQuery{{Source: "logs", Range: "1h"}}},
		{"duplicate id", []seriesQuery{
			{ID: "a", Source: "logs", Range: "1h"},
			{ID: "a", Source: "event", Range: "1h", Name: "signup"},
		}},
		{"unknown source", []seriesQuery{{ID: "a", Source: "traces", Range: "1h"}}},
		// A range outside the enum has no step, so answering it would mean
		// inventing a resolution the client never asked for.
		{"unknown range", []seriesQuery{{ID: "a", Source: "logs", Range: "90d"}}},
	}
	for _, c := range cases {
		if msg := validateSeries(c.qs); msg == "" {
			t.Fatalf("%s must be rejected", c.name)
		}
	}
}

// labels narrow the two reads of one event name over time, within the
// contract's caps, and every refusal names the query it is about.
func TestValidateSeries_Labels(t *testing.T) {
	pro := map[string]string{"plan": "pro"}
	for _, ok := range []seriesQuery{
		{ID: "ev", Source: "event", Range: "24h", Name: "purchase", Labels: pro},
		{ID: "bare", Source: "people", Range: "24h", Name: "purchase", Labels: pro},
		{ID: "caps", Source: "event", Range: "24h", Name: "purchase", Labels: map[string]string{
			"a": "1", "b": "2", strings.Repeat("k", 60): strings.Repeat("v", 256)}},
		// An empty object filters nothing, so it is nobody's business which read
		// carries it.
		{ID: "none", Source: "logs", Range: "24h", Labels: map[string]string{}},
	} {
		if msg := validateSeries([]seriesQuery{ok}); msg != "" {
			t.Fatalf("%s must be accepted; got %q", ok.ID, msg)
		}
	}
	for _, bad := range []seriesQuery{
		{ID: "logs", Source: "logs", Range: "24h", Labels: pro},
		{ID: "check", Source: "check", Range: "24h", Name: "uptime", Labels: pro},
		{ID: "metric", Source: "metric", Range: "24h", Name: "load", Labels: pro},
		{ID: "funnel", Source: "people", Range: "24h", Steps: []string{"visit", "signup"}, Labels: pro},
		{ID: "retention", Source: "people", Range: "24h", Cohort: "week", Labels: pro},
		{ID: "breakdown", Source: "people", Range: "24h", Name: "purchase", Group: []string{"plan"}, Labels: pro},
		{ID: "four", Source: "event", Range: "24h", Name: "purchase", Labels: map[string]string{
			"a": "1", "b": "2", "c": "3", "d": "4"}},
		{ID: "emptykey", Source: "event", Range: "24h", Name: "purchase", Labels: map[string]string{"": "x"}},
		{ID: "longkey", Source: "event", Range: "24h", Name: "purchase", Labels: map[string]string{strings.Repeat("k", 61): "x"}},
		{ID: "longvalue", Source: "event", Range: "24h", Name: "purchase", Labels: map[string]string{"k": strings.Repeat("v", 257)}},
		// Postgres refuses a NUL inside jsonb with a 500 for the whole board.
		{ID: "nulvalue", Source: "event", Range: "24h", Name: "purchase", Labels: map[string]string{"plan": "pro\x00"}},
		{ID: "ctrlkey", Source: "people", Range: "24h", Name: "purchase", Labels: map[string]string{"pl\nan": "pro"}},
	} {
		msg := validateSeries([]seriesQuery{bad})
		if msg == "" || !strings.HasSuffix(msg, " in "+bad.ID) {
			t.Fatalf("%s must be refused by name; got %q", bad.ID, msg)
		}
	}
}

// A people query naming one event and nothing else is a shape of its own now;
// the people queries that name nothing at all are still refused, and so is a
// row count on anything but a one-key fold.
func TestValidateSeries_BarePeopleName(t *testing.T) {
	if msg := validateSeries([]seriesQuery{{ID: "a", Source: "people", Range: "7d", Name: "signup"}}); msg != "" {
		t.Fatalf("a bare people name must be accepted; got %q", msg)
	}
	for _, bad := range []seriesQuery{
		{ID: "nothing", Source: "people", Range: "7d"},
		{ID: "unnamed", Source: "people", Range: "7d", Group: []string{"plan"}},
		{ID: "rows", Source: "people", Range: "7d", Name: "signup", Count: "events"},
	} {
		if msg := validateSeries([]seriesQuery{bad}); msg == "" || !strings.HasSuffix(msg, " in "+bad.ID) {
			t.Fatalf("%s must still be refused by name; got %q", bad.ID, msg)
		}
	}
}

// A bucket keeps the range's span and must leave more than one bucket and no
// more than a 31-day month of hours.
func TestValidateSeries_Bucket(t *testing.T) {
	for _, ok := range []struct{ rng, bucket string }{
		{"4h", "hour"}, {"24h", "hour"}, {"7d", "hour"}, {"31d", "hour"},
		{"7d", "day"}, {"31d", "day"}, {"365d", "day"},
	} {
		q := seriesQuery{ID: "a", Source: "event", Range: ok.rng, Name: "signup", Bucket: ok.bucket}
		if msg := validateSeries([]seriesQuery{q}); msg != "" {
			t.Fatalf("%s on %s must be accepted; got %q", ok.bucket, ok.rng, msg)
		}
	}
	for _, bad := range []struct{ rng, bucket string }{
		{"1h", "hour"}, {"365d", "hour"}, {"1h", "day"}, {"12h", "day"}, {"24h", "day"}, {"7d", "week"},
	} {
		q := seriesQuery{ID: "q1", Source: "event", Range: bad.rng, Name: "signup", Bucket: bad.bucket}
		if msg := validateSeries([]seriesQuery{q}); msg == "" || !strings.HasSuffix(msg, " in q1") {
			t.Fatalf("%s on %s must be refused by name; got %q", bad.bucket, bad.rng, msg)
		}
	}
	// A fold has no time axis to cut, so a bucket on one is refused, never
	// ignored; the bare people name keeps one and keeps its bucket.
	if msg := validateSeries([]seriesQuery{{ID: "a", Source: "people", Range: "7d", Name: "signup", Bucket: "day"}}); msg != "" {
		t.Fatalf("a bare people name in day buckets must be accepted; got %q", msg)
	}
	for _, bad := range []seriesQuery{
		{ID: "grouped", Source: "metric", Range: "7d", Name: "load", Group: []string{"host"}, Bucket: "day"},
		{ID: "breakdown", Source: "people", Range: "7d", Name: "purchase", Group: []string{"plan"}, Bucket: "day"},
		{ID: "funnel", Source: "people", Range: "7d", Steps: []string{"visit", "signup"}, Bucket: "day"},
		{ID: "retention", Source: "people", Range: "7d", Cohort: "week", Bucket: "day"},
	} {
		if msg := validateSeries([]seriesQuery{bad}); msg != "bucket needs a time axis in "+bad.ID {
			t.Fatalf("%s must be refused for want of a time axis; got %q", bad.ID, msg)
		}
	}
}

// A bucket re-lands the window on a UTC hour or a UTC midnight and keeps the
// span, so the history gate, which reads the span, answers as it did.
func TestRebucket_KeepsTheSpanOnTheBucketsOwnBoundary(t *testing.T) {
	now := time.Date(2026, 9, 6, 13, 7, 23, 0, time.UTC)
	for _, c := range []struct {
		rng, bucket string
		step, n     int
		to          time.Time
	}{
		{"24h", "hour", 3600, 24, time.Date(2026, 9, 6, 14, 0, 0, 0, time.UTC)},
		{"7d", "hour", 3600, 168, time.Date(2026, 9, 6, 14, 0, 0, 0, time.UTC)},
		{"7d", "day", 86400, 7, time.Date(2026, 9, 7, 0, 0, 0, 0, time.UTC)},
		{"31d", "day", 86400, 31, time.Date(2026, 9, 7, 0, 0, 0, 0, time.UTC)},
	} {
		own := seriesRanges[c.rng]
		r := rebucket(own, c.bucket)
		if r.step != c.step || r.buckets != c.n {
			t.Fatalf("%s in %s buckets = %d of %ds, want %d of %ds", c.rng, c.bucket, r.buckets, r.step, c.n, c.step)
		}
		from, to := r.window(now)
		if !to.Equal(c.to) || to.Sub(from) != time.Duration(own.step*own.buckets)*time.Second {
			t.Fatalf("%s in %s spans %s .. %s, want a whole %s up to %s", c.rng, c.bucket, from, to, c.rng, c.to)
		}
	}
	if r := rebucket(seriesRanges["24h"], ""); r != seriesRanges["24h"] {
		t.Fatalf("no bucket is the range's own step; got %+v", r)
	}
}

// The rev a save checks its base against is the document's, not its spelling:
// postgres hands a jsonb layout back with its own key order and spacing, and
// a save answered with one rev must find the same rev on the next read.
func TestBoardRev_IsTheDocumentNotItsSpelling(t *testing.T) {
	ours := `{"version":2,"widgets":[{"id":"w_1","kind":"stat","title":"<Signups>","metrics":[{"source":"event","name":"signup"}],"x":0,"y":0,"w":4,"h":4}]}`
	theirs := `{"version": 2, "widgets": [{"h": 4, "w": 4, "x": 0, "y": 0, "id": "w_1", "kind": "stat", "title": "<Signups>", "metrics": [{"name": "signup", "source": "event"}]}]}`
	rev := boardRev([]byte(ours))
	if len(rev) != 16 || strings.Trim(rev, "0123456789abcdef") != "" {
		t.Fatalf("a rev is 16 hex characters; got %q", rev)
	}
	if got := boardRev([]byte(theirs)); got != rev {
		t.Fatalf("two spellings of one layout must be one rev; got %s and %s", rev, got)
	}
	if got := boardRev([]byte(strings.Replace(ours, `"h":4`, `"h":5`, 1))); got == rev {
		t.Fatal("a different layout must be a different rev")
	}
	// Pinned: the empty board is what every never-saved alias is served as, so
	// a change in the canonical form would refuse the first save of every tab
	// open across a deploy.
	if got := boardRev(emptyLayout); got != "4ce8b0daa99532bd" || got != boardRev([]byte(`{"widgets": [], "version": 2}`)) {
		t.Fatalf("the empty board's rev drifted; got %s", got)
	}
}

func TestSeriesWindow_LandsOnBucketBoundaries(t *testing.T) {
	// 13:07:23 is mid-bucket: `to` must round UP, so the last bucket is the
	// current, partial one rather than a bucket that already closed.
	now := time.Date(2026, 9, 6, 13, 7, 23, 0, time.UTC)
	r := seriesRanges["24h"]
	if r.step != 1800 || r.buckets != 48 {
		t.Fatalf("24h is 48 buckets of 1800s; got %d of %d", r.buckets, r.step)
	}
	from, to := r.window(now)
	if to.Unix()%int64(r.step) != 0 || from.Unix()%int64(r.step) != 0 {
		t.Fatalf("both ends must sit on a bucket boundary; got %s .. %s", from, to)
	}
	if !to.After(now) || to.Sub(now) > 30*time.Minute {
		t.Fatalf("to must be the NEXT boundary after now; got %s for %s", to, now)
	}
	if to.Sub(from) != 24*time.Hour {
		t.Fatalf("the window must span the range; got %s", to.Sub(from))
	}
}

func TestSeriesWindow_EveryRangeSpansItsOwnName(t *testing.T) {
	// step × buckets IS the span: a drift here would draw a 7d chart over
	// six days and label it seven.
	spans := map[string]time.Duration{
		"1h": time.Hour, "4h": 4 * time.Hour, "12h": 12 * time.Hour,
		"24h": 24 * time.Hour, "7d": 7 * 24 * time.Hour,
		"31d": 31 * 24 * time.Hour, "365d": 365 * 24 * time.Hour,
	}
	now := time.Date(2026, 9, 6, 13, 7, 23, 0, time.UTC)
	for name, want := range spans {
		r, ok := seriesRanges[name]
		if !ok {
			t.Fatalf("%s must be a known range", name)
		}
		if from, to := r.window(now); to.Sub(from) != want {
			t.Fatalf("%s spans %s, want %s", name, to.Sub(from), want)
		}
	}
}

func TestCountPoints_EmptyBucketIsAMeasuredZero(t *testing.T) {
	// Zero is silence for a gauge, not for a count: nothing happened in that
	// minute is a fact, and drawing a gap there would hide it.
	points, total := countPoints([]pgstore.Bucket{{Index: 0, Count: 3}, {Index: 2, Count: 1}},
		seriesRange{step: 60, buckets: 4}, time.Time{}, nil)
	if total != int64(4) {
		t.Fatalf("total must sum the buckets; got %v", total)
	}
	for i, want := range []int64{3, 0, 1, 0} {
		if points[i] != want {
			t.Fatalf("bucket %d = %v, want %d", i, points[i], want)
		}
	}
}

func TestRangeDays_IsTheDepthThePlanSells(t *testing.T) {
	// The gate compares this against plan_entitlement.history_days, so every
	// sub-day range has to land on 1: a 1h chart asks the store for no more
	// depth than a 24h one, and gating it would be a wall nobody crossed.
	want := map[string]int{"1h": 1, "4h": 1, "12h": 1, "24h": 1, "7d": 7, "31d": 31, "365d": 365}
	for name, days := range want {
		if got := rangeDays(seriesRanges[name]); got != days {
			t.Fatalf("rangeDays(%s) = %d, want %d", name, got, days)
		}
	}
}

func TestHistoryLabelAndReason_AreTheCopyTheFrontSpells(t *testing.T) {
	for days, want := range map[int]string{1: "24 hours", 7: "7 days", 31: "31 days", 365: "12 months"} {
		if got := historyLabel(days); got != want {
			t.Fatalf("historyLabel(%d) = %q, want %q", days, got, want)
		}
	}
	if got := historyReason(31, "Growth"); got != "31 days of history is on Growth and up. It counts from the day you switch." {
		t.Fatalf("the wall's copy drifted; got %q", got)
	}
	// The top of the ladder drops " and up": there is nothing above it to buy.
	if got := historyReason(365, "Agency"); got != "12 months of history is on Agency. It counts from the day you switch." {
		t.Fatalf("the top rung must not offer a rung above it; got %q", got)
	}
}

func TestCountPoints_BeforeTheOldestRowIsNullNotZero(t *testing.T) {
	from := time.Date(2026, 9, 6, 0, 0, 0, 0, time.UTC)
	r := seriesRange{step: 3600, buckets: 4}
	// The store's first row sits inside bucket 2, so buckets 0 and 1 end at or
	// before it: nothing was kept there, which is not a count of zero.
	o := &oldest{at: from.Add(2*time.Hour + 30*time.Minute), found: true}
	points, total := countPoints([]pgstore.Bucket{{Index: 2, Count: 5}}, r, from, o)
	if points[0] != nil || points[1] != nil {
		t.Fatalf("a bucket ending at or before the oldest row must be null; got %v", points[:2])
	}
	if points[2] != int64(5) || points[3] != int64(0) {
		t.Fatalf("from the oldest row on, an empty bucket is a measured 0; got %v", points[2:])
	}
	if total != int64(5) {
		t.Fatalf("total sums the measured buckets only; got %v", total)
	}

	// A bucket ending EXACTLY on the oldest row held nothing either: the row
	// is the next bucket's first.
	edge, _ := countPoints(nil, r, from, &oldest{at: from.Add(2 * time.Hour), found: true})
	if edge[1] != nil || edge[2] != int64(0) {
		t.Fatalf("the boundary belongs to the bucket that starts on it; got %v", edge)
	}

	// An empty store measured nothing anywhere, so there is no total either.
	empty, none := countPoints(nil, r, from, &oldest{})
	for i, p := range empty {
		if p != nil {
			t.Fatalf("an empty store draws nothing; bucket %d = %v", i, p)
		}
	}
	if none != nil {
		t.Fatalf("a total over no measured bucket is null, not 0; got %v", none)
	}
}

func TestPreviousTotal_OnlyWhenTheWholeSpanWasStored(t *testing.T) {
	from := time.Date(2026, 9, 6, 0, 0, 0, 0, time.UTC)
	span := 24 * time.Hour
	rows := []pgstore.Bucket{{Index: 0, Count: 9}}
	// The previous span starts a day before `from`; an oldest row inside it
	// would make the comparison a shorter span against a full one.
	if got := previousTotal(rows, from, span, &oldest{at: from.Add(-2 * time.Hour), found: true}); got != nil {
		t.Fatalf("a partly stored previous span has no total; got %v", got)
	}
	if got := previousTotal(rows, from, span, &oldest{}); got != nil {
		t.Fatalf("an empty store has no previous total; got %v", got)
	}
	if got := previousTotal(rows, from, span, &oldest{at: from.Add(-span), found: true}); got != int64(9) {
		t.Fatalf("a span starting exactly on the oldest row counts; got %v", got)
	}
	if got := previousTotal(rows, from, span, nil); got != int64(9) {
		t.Fatalf("an axis with no floor always counts; got %v", got)
	}
}

func TestCheckValue_NoProbeIsNullNotZero(t *testing.T) {
	// 100% uptime for a check that never ran is the one lie a monitoring
	// product may not tell, and 0 ms is the same lie about latency.
	if v := checkValue("uptime", pgstore.CheckBucket{}); v != nil {
		t.Fatalf("uptime with no probe must be null; got %v", v)
	}
	if v := checkValue("response", pgstore.CheckBucket{Total: 2}); v != nil {
		t.Fatalf("response with no OK probe must be null; got %v", v)
	}
	if v := checkValue("uptime", pgstore.CheckBucket{OK: 3, Total: 4}); v != 75.0 {
		t.Fatalf("uptime must be the share of ok probes; got %v", v)
	}
	if v := checkValue("response", pgstore.CheckBucket{SumMs: 300, OK: 2, Total: 3}); v != 150.0 {
		t.Fatalf("response must average the ok probes only; got %v", v)
	}
}

func TestLogsFilter_ReadsTheKnownKeysAndIgnoresTheRest(t *testing.T) {
	f := logsFilter(map[string]string{
		"service":     "api",
		"level":       "warn",
		"fingerprint": "18446744073709551615", // above MaxInt64: it wraps, as the column does
		"q":           "webhook",
		"attr.route":  "/checkout",
		"unknown":     "ignored",
		"attr.":       "ignored",
	})
	if f.Service == nil || *f.Service != "api" || f.Level != "warn" || f.Search != "webhook" {
		t.Fatalf("the known keys must land on the filter; got %+v", f)
	}
	if f.Fingerprint == nil || *f.Fingerprint != -1 {
		t.Fatalf("the decimal text must invert the int64 wrap InsertLogs stores; got %v", f.Fingerprint)
	}
	if len(f.Attrs) != 1 || f.Attrs["route"] != "/checkout" {
		t.Fatalf("only attr.<key> becomes an attribute filter; got %v", f.Attrs)
	}
}

func TestLogsFilter_AbsentServiceIsEveryServiceAndEmptyIsTheUnlabelledOne(t *testing.T) {
	if f := logsFilter(nil); f.Service != nil {
		t.Fatalf("an absent service must not filter; got %v", *f.Service)
	}
	f := logsFilter(map[string]string{"service": ""})
	if f.Service == nil || *f.Service != "" {
		t.Fatalf("the empty string is the unlabelled service, a real name; got %v", f.Service)
	}
}

func TestReadsRollup_WholeHoursWithoutTextFilters(t *testing.T) {
	plain := logsFilter(nil)
	if readsRollup(seriesRanges["24h"], plain) {
		t.Fatal("a sub-day step reads the ring")
	}
	for _, rng := range []string{"7d", "31d", "365d"} {
		if !readsRollup(seriesRanges[rng], plain) {
			t.Fatalf("%s steps in whole hours and reads the rollup", rng)
		}
	}
	if readsRollup(seriesRanges["7d"], logsFilter(map[string]string{"q": "timeout"})) {
		t.Fatal("a substring filter has to see the lines")
	}
	if readsRollup(seriesRanges["31d"], logsFilter(map[string]string{"attr.route": "/x"})) {
		t.Fatal("an attribute filter has to see the lines")
	}
	if !readsRollup(seriesRanges["7d"], logsFilter(map[string]string{"service": "api", "level": "warn", "fingerprint": "7"})) {
		t.Fatal("service, level and fingerprint are the rollup's own key")
	}
}

// The board's envelope. validateLayout is the only thing standing between a
// front's document and the store, and it deliberately checks the envelope
// alone: the refs inside a widget are the front's to interpret.

// widget is the shortest way to spell a valid widget in a table row; each case
// below mutates the one field it is about.
func widget(id string) apigen.DashboardWidget {
	rng := apigen.DashboardWidgetRangeN24h
	return apigen.DashboardWidget{
		Id: id, Kind: apigen.DashboardWidgetKindLine, Title: "Errors by level",
		Metrics: []apigen.DashboardMetricRef{{Source: apigen.DashboardMetricRefSourceLogs}},
		Range:   &rng, X: 0, Y: 0, W: 6, H: 4,
	}
}

func TestValidateLayout(t *testing.T) {
	badRange := apigen.DashboardWidgetRange("2h")
	cases := []struct {
		name string
		doc  apigen.DashboardLayout
		ok   bool
	}{
		{"a board the front would send", apigen.DashboardLayout{
			Version: 1, Widgets: []apigen.DashboardWidget{widget("w_1"), func() apigen.DashboardWidget {
				b := widget("w_2")
				b.X, b.W = 6, 6
				return b
			}()},
		}, true},
		{"an empty board", apigen.DashboardLayout{Version: 1, Widgets: []apigen.DashboardWidget{}}, true},
		{"a widget with no range", apigen.DashboardLayout{
			Version: 1, Widgets: []apigen.DashboardWidget{func() apigen.DashboardWidget {
				b := widget("w_1")
				b.Kind, b.Range = apigen.DashboardWidgetKindStatus, nil
				return b
			}()},
		}, true},
		// 2 is the halved-row unit and stores like any other; 3 is nothing yet, and a server
		// that took it would be storing a board no front can read back.
		{"the halved-row unit stores", apigen.DashboardLayout{
			Version: 2, Widgets: []apigen.DashboardWidget{widget("w_1")},
		}, true},
		{"a version this server does not store", apigen.DashboardLayout{
			Version: 3, Widgets: []apigen.DashboardWidget{widget("w_1")},
		}, false},
		{"an empty id", apigen.DashboardLayout{
			Version: 1, Widgets: []apigen.DashboardWidget{widget("")},
		}, false},
		{"the same id twice", apigen.DashboardLayout{
			Version: 1, Widgets: []apigen.DashboardWidget{widget("w_1"), widget("w_1")},
		}, false},
		{"an unknown kind", apigen.DashboardLayout{
			Version: 1, Widgets: []apigen.DashboardWidget{func() apigen.DashboardWidget {
				b := widget("w_1")
				b.Kind = apigen.DashboardWidgetKind("sparkline")
				return b
			}()},
		}, false},
		{"a range outside the six", apigen.DashboardLayout{
			Version: 1, Widgets: []apigen.DashboardWidget{func() apigen.DashboardWidget {
				b := widget("w_1")
				b.Range = &badRange
				return b
			}()},
		}, false},
		{"a widget with no width", apigen.DashboardLayout{
			Version: 1, Widgets: []apigen.DashboardWidget{func() apigen.DashboardWidget {
				b := widget("w_1")
				b.W = 0
				return b
			}()},
		}, false},
		{"a widget bound to nothing", apigen.DashboardLayout{
			Version: 1, Widgets: []apigen.DashboardWidget{func() apigen.DashboardWidget {
				b := widget("w_1")
				b.Metrics = nil
				return b
			}()},
		}, false},
		{"a widget running past the grid", apigen.DashboardLayout{
			Version: 1, Widgets: []apigen.DashboardWidget{func() apigen.DashboardWidget {
				b := widget("w_1")
				b.X, b.W = 7, 6
				return b
			}()},
		}, false},
		{"a widget above the grid", apigen.DashboardLayout{
			Version: 1, Widgets: []apigen.DashboardWidget{func() apigen.DashboardWidget {
				b := widget("w_1")
				b.Y = -1
				return b
			}()},
		}, false},
		{"a widget left of the grid", apigen.DashboardLayout{
			Version: 1, Widgets: []apigen.DashboardWidget{func() apigen.DashboardWidget {
				b := widget("w_1")
				b.X = -1
				return b
			}()},
		}, false},
		{"a widget with no height", apigen.DashboardLayout{
			Version: 1, Widgets: []apigen.DashboardWidget{func() apigen.DashboardWidget {
				b := widget("w_1")
				b.H = 0
				return b
			}()},
		}, false},
		{"a ref with an unknown source", apigen.DashboardLayout{
			Version: 1, Widgets: []apigen.DashboardWidget{func() apigen.DashboardWidget {
				b := widget("w_1")
				b.Metrics = []apigen.DashboardMetricRef{{Source: apigen.DashboardMetricRefSource("guesswork")}}
				return b
			}()},
		}, false},
		// Every source the catalog hands the picker must survive a save. `retention` was
		// missing from the enum, so a board carrying a retention card was refused whole.
		{"a ref for every source the catalog mints", apigen.DashboardLayout{
			Version: 1, Widgets: []apigen.DashboardWidget{func() apigen.DashboardWidget {
				b := widget("w_1")
				b.Metrics = nil
				for _, source := range []apigen.DashboardMetricRefSource{
					apigen.DashboardMetricRefSourceLogs, apigen.DashboardMetricRefSourceCheck,
					apigen.DashboardMetricRefSourceEvent, apigen.DashboardMetricRefSourceMetric,
					apigen.DashboardMetricRefSourceService, apigen.DashboardMetricRefSourceFunnel,
					apigen.DashboardMetricRefSourceExperiment, apigen.DashboardMetricRefSourceRetention,
					apigen.DashboardMetricRefSourceDimension,
				} {
					b.Metrics = append(b.Metrics, apigen.DashboardMetricRef{Source: source})
				}
				return b
			}()},
		}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			reason := validateLayout(c.doc)
			if c.ok && reason != "" {
				t.Fatalf("this layout must be stored; refused with %q", reason)
			}
			if !c.ok && reason == "" {
				t.Fatal("this layout must be refused, and the refusal must say why")
			}
		})
	}
}

// A ratio is a count over what it is out of: two refs, and any other number
// has nothing to divide.
func TestValidateLayout_ARatioReadsExactlyTwoRefs(t *testing.T) {
	ref := apigen.DashboardMetricRef{Source: apigen.DashboardMetricRefSourceEvent}
	refused := `widget "w_1": a ratio reads exactly two refs, a count and what it is out of`
	for n, want := range map[int]string{1: refused, 2: "", 3: refused} {
		b := widget("w_1")
		b.Kind, b.Metrics = apigen.DashboardWidgetKindRatio, slices.Repeat([]apigen.DashboardMetricRef{ref}, n)
		if got := validateLayout(apigen.DashboardLayout{Version: 2, Widgets: []apigen.DashboardWidget{b}}); got != want {
			t.Fatalf("a ratio of %d refs = %q, want %q", n, got, want)
		}
	}
}

// A count the contract does not name never reaches storage: the app drops a ref
// with an unknown count on read, so a typo an agent sent through the CLI would
// otherwise vanish from the board on its next Save.
func TestValidateLayout_RefusesACountTheContractDoesNotName(t *testing.T) {
	for count, want := range map[string]string{
		"people": "",
		"events": "",
		"users":  `widget "w_1" counts an unknown "users"`,
	} {
		c := apigen.DashboardMetricRefCount(count)
		b := widget("w_1")
		b.Metrics = []apigen.DashboardMetricRef{{Source: apigen.DashboardMetricRefSourcePeople, Count: &c}}
		if got := validateLayout(apigen.DashboardLayout{Version: 2, Widgets: []apigen.DashboardWidget{b}}); got != want {
			t.Fatalf("count %q = %q, want %q", count, got, want)
		}
	}
}

// The empty answer is the contract's own literal, and its version is the
// CURRENT one: an empty board has no heights, so the field says nothing except
// which unit the next writer should use. It read 1 until 2026-09-08, which
// invited an agent to fill it in and send it back with version-2 sizes under a
// version-1 label.
func TestEmptyLayoutIsTheDocumentedEmptyBoard(t *testing.T) {
	if string(emptyLayout) != `{"version":2,"widgets":[]}` {
		t.Fatalf("the empty board is %s", emptyLayout)
	}
	var doc apigen.DashboardLayout
	if err := json.Unmarshal(emptyLayout, &doc); err != nil {
		t.Fatalf("the empty board must parse as a layout: %v", err)
	}
	if reason := validateLayout(doc); reason != "" {
		t.Fatalf("the empty board must be a layout this server would store; got %q", reason)
	}
}

// The board walls' copy, which the front spells identically. Both sentences are
// built from the entitlement rows, so a limit that moves in a migration moves
// here without a code edit — the numbers below are the arguments, not the table.
func TestBoardsReason_IsTheCopyTheFrontSpells(t *testing.T) {
	if got := boardsReason("Free", 1, "Growth", 3); got != "Your plan carries 1 dashboard per project. Growth carries 3." {
		t.Fatalf("the create wall's copy drifted; got %q", got)
	}
	// Plural once the reader's own plan carries more than one.
	if got := boardsReason("Growth", 3, "Agency", 5); got != "Your plan carries 3 dashboards per project. Agency carries 5." {
		t.Fatalf("the create wall must count in the plural; got %q", got)
	}
	// The top of the ladder has nothing to sell: a sentence, and the front
	// shows it instead of a modal whose every card would say Downgrade.
	if got := boardsReason("Agency", 5, "", 0); got != "Agency carries 5 dashboards per project." {
		t.Fatalf("the top rung must not offer a rung above it; got %q", got)
	}
	if got := frozenReason("Growth", 3); got != "This dashboard is kept, not running. Growth carries 3 per project." {
		t.Fatalf("the frozen wall's copy drifted; got %q", got)
	}
	// Kept is kept, whether or not there is a plan that would run it.
	if got := frozenReason("", 0); got != "This dashboard is kept, not running." {
		t.Fatalf("a frozen board with nothing to buy still says it is kept; got %q", got)
	}
}

// The {id} of a per-board path, with the sub-path taken off first. Reading the
// last segment would make /v1/dashboards/{id}/proposal a board called
// "proposal", which resolves to nothing and 404s a door that exists.
func TestBoardPathID_ReadsTheBoardNotTheSubPath(t *testing.T) {
	for path, want := range map[string]string{
		"/v1/dashboards/main":          "main",
		"/v1/dashboards/ab12":          "ab12",
		"/v1/dashboards/ab12/proposal": "ab12",
		"/v1/dashboards/ab12/widgets":  "ab12",
	} {
		if got := boardPathID(path); got != want {
			t.Fatalf("boardPathID(%q) = %q, want %q", path, got, want)
		}
	}
}

// A name is what a tab prints. A control character is refused as a bad name
// here, where postgres would refuse a NUL with a 500 after the alias was laid.
func TestBoardName_RefusesControlCharacters(t *testing.T) {
	for raw, want := range map[string]string{
		"  Sales  ":  "Sales",
		"Ops · EU":   "Ops · EU",
		"ops\x00":    "",
		"o\nps":      "",
		"ops\u0085x": "",
	} {
		w := httptest.NewRecorder()
		got, ok := boardName(w, raw)
		if got != want || ok != (want != "") {
			t.Fatalf("boardName(%q) = %q, %v; want %q", raw, got, ok, want)
		}
		if !ok && (w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "bad_name")) {
			t.Fatalf("boardName(%q) must answer 400 bad_name; got %d %s", raw, w.Code, w.Body.String())
		}
	}
}

// Which board paths the KEY door takes, and which it deliberately does not.
// The handler tests drive the handlers, and routes_test reads the mux, so this
// is the only place the dispatch itself is pinned: a key on a session-only
// path must fall through to the session gate rather than be served, and an
// {id} arm that swallowed a sub-path would hand the agent the board instead of
// refusing it. With no resolver wired, reaching the key door IS the 401
// bad_key; everything that falls through is the session's own no_session.
func TestTheKeyDoorTakesTheBoardPathsItOwnsAndNoOthers(t *testing.T) {
	h := &writeAPI{}
	for _, door := range []struct{ method, path, want string }{
		{http.MethodGet, "/v1/dashboard", "bad_key"},
		{http.MethodPut, "/v1/dashboard", "bad_key"},
		{http.MethodPost, "/v1/dashboard/widgets", "bad_key"},
		{http.MethodGet, "/v1/dashboards", "bad_key"},
		{http.MethodPost, "/v1/dashboards", "bad_key"},
		{http.MethodGet, "/v1/dashboards/ab12", "bad_key"},
		{http.MethodPut, "/v1/dashboards/ab12", "bad_key"},
		{http.MethodPost, "/v1/dashboards/ab12/widgets", "bad_key"},
		// A key names no board, saves no batch and reads no proposal.
		{http.MethodPatch, "/v1/dashboards/ab12", "no_session"},
		{http.MethodDelete, "/v1/dashboards/ab12", "no_session"},
		{http.MethodPut, "/v1/dashboards", "no_session"},
		{http.MethodGet, "/v1/dashboards/ab12/proposal", "no_session"},
		{http.MethodDelete, "/v1/dashboards/ab12/proposal", "no_session"},
	} {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(door.method, door.path, strings.NewReader("{}"))
		r.Header.Set("X-Upcontrol-Key", "uc_live_deadbeefdeadbeefdeadbeefdeadbeef")
		h.ServeHTTP(w, r)
		if w.Code != http.StatusUnauthorized || !strings.Contains(w.Body.String(), door.want) {
			t.Fatalf("%s %s with a key = %d %s, want 401 %s", door.method, door.path, w.Code, w.Body.String(), door.want)
		}
	}
}

// A board is STORED as DashboardMetricRef and the write decodes it strictly, so a field a card
// can carry but the schema does not list is a 400 on a document the server itself just served.
// That is not hypothetical: the people source shipped with the reads and not with this schema,
// and every board holding a funnel, a retention grid, a breakdown or an A/B test was refused by
// its own Save — in the app and through a key alike — until 0.26.1. The round trip is asserted,
// not just the decode: a field the struct drops on the way out is stored and never served.
func TestReadLayout_KeepsEveryRefACardCanCarry(t *testing.T) {
	for _, one := range []struct{ name, ref, token string }{
		{"a funnel's journey", `{"source":"people","steps":["visit","signup"]}`, `"steps":["visit","signup"]`},
		{"a retention grid", `{"source":"people","cohort":"week"}`, `"cohort":"week"`},
		{"a breakdown's field", `{"source":"people","name":"page","field":"value"}`, `"field":"value"`},
		{"an A/B test's event", `{"source":"people","name":"checkout"}`, `"source":"people"`},
		{"a narrowed log pick", `{"source":"logs","where":{"service":"api"},"label":"api errors"}`, `"label":"api errors"`},
		{"a counted feed from before", `{"source":"dimension","name":"page"}`, `"source":"dimension"`},
	} {
		t.Run(one.name, func(t *testing.T) {
			body := fmt.Sprintf(
				`{"version":2,"widgets":[{"id":"w1","kind":"funnel","title":"T","metrics":[%s],"x":0,"y":0,"w":6,"h":12}]}`,
				one.ref)
			r := httptest.NewRequest(http.MethodPut, "/v1/dashboard", strings.NewReader(body))
			w := httptest.NewRecorder()
			doc, ok := readLayout(w, r)
			if !ok {
				t.Fatalf("%s must be storable; got %d %s", one.name, w.Code, w.Body.String())
			}
			out, err := json.Marshal(doc)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if !strings.Contains(string(out), one.token) {
				t.Fatalf("%s must survive the round trip; %s is missing from %s", one.name, one.token, out)
			}
		})
	}
}
