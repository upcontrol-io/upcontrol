package api

import (
	"encoding/json"
	"fmt"
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

func TestSeriesWindow_LandsOnBucketBoundaries(t *testing.T) {
	// 13:07:23 is mid-bucket: `to` must round UP, so the last bucket is the
	// current, partial one rather than a bucket that already closed.
	now := time.Date(2026, 9, 6, 13, 7, 23, 0, time.UTC)
	from, to, r, ok := seriesWindow("24h", now)
	if !ok {
		t.Fatal("24h must be a known range")
	}
	if r.step != 1800 || r.buckets != 48 {
		t.Fatalf("24h is 48 buckets of 1800s; got %d of %d", r.buckets, r.step)
	}
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
		from, to, _, ok := seriesWindow(name, now)
		if !ok {
			t.Fatalf("%s must be a known range", name)
		}
		if to.Sub(from) != want {
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
		{"a version this server does not store", apigen.DashboardLayout{
			Version: 2, Widgets: []apigen.DashboardWidget{widget("w_1")},
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

// The empty answer is the contract's own literal: the front compares against
// it, and a re-spelling here (a space, "widgets" first) would break that.
func TestEmptyLayoutIsTheDocumentedEmptyBoard(t *testing.T) {
	if string(emptyLayout) != `{"version":1,"widgets":[]}` {
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
