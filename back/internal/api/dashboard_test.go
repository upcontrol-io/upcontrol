package api

import (
	"fmt"
	"testing"
	"time"

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
		seriesRange{step: 60, buckets: 4})
	if total != 4 {
		t.Fatalf("total must sum the buckets; got %d", total)
	}
	for i, want := range []int64{3, 0, 1, 0} {
		if points[i] != want {
			t.Fatalf("bucket %d = %v, want %d", i, points[i], want)
		}
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
