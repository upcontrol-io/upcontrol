package api

import (
	"strings"
	"testing"

	"go.upcontrol.io/back/internal/storage/ch"
)

// The product tiles are only as honest as their note: a bare number is exactly
// what "numbers ship with their normal range, never bare" forbids. A metric
// with under 7 days of history has no computable range and must produce no
// tile at all — a tile with no note is a bare number wearing a label.
func TestMetricTilesCarryARangeOrDontShip(t *testing.T) {
	stats := []ch.MetricStat{
		{Name: "signups", Latest: 31, P10: 28, P90: 35, Days: 7, Spark: []float64{29, 30, 31}},
		{Name: "checkout_latency_ms", Latest: 182.5, P10: 170, P90: 210, Days: 9, Spark: []float64{190, 185, 182.5}},
		{Name: "brand_new_metric", Latest: 5, P10: 0, P90: 0, Days: 1, Spark: []float64{5}},
	}
	tiles := metricTiles(stats)
	if len(tiles) != 2 {
		t.Fatalf("got %d tiles, want 2 — under 7 days of history ships no tile", len(tiles))
	}
	for _, tile := range tiles {
		note, _ := tile["note"].(string)
		if !strings.HasPrefix(note, "usually ") {
			t.Fatalf("tile %v carries no range: %q", tile["label"], note)
		}
	}
	signups := tiles[0]
	if signups["label"] != "Sign-ups today" {
		t.Fatalf("label = %v, want Sign-ups today", signups["label"])
	}
	if !strings.Contains(signups["note"].(string), "28–35") {
		t.Fatalf("signups note lost its range: %v", signups["note"])
	}
	latency := tiles[1]
	if !strings.Contains(latency["value"].(string), "ms") {
		t.Fatalf("latency value carries no unit: %v", latency["value"])
	}
	if !strings.Contains(latency["note"].(string), "170") || !strings.Contains(latency["note"].(string), "210") {
		t.Fatalf("latency note lost its range: %v", latency["note"])
	}
}

func TestMetricTilesOfNothingIsNothing(t *testing.T) {
	if got := metricTiles(nil); len(got) != 0 {
		t.Fatalf("got %d tiles for no metrics, want 0", len(got))
	}
}
