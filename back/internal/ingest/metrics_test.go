package ingest

import "testing"

// ParseMetric's contract, and the one rule that matters most: a log line must
// never be mistaken for a metric, or the stream silently empties.
func TestParseMetricRecognisesAMetricAndLeavesLogsAlone(t *testing.T) {
	m, ok := ParseMetric([]byte(`{"metric":"signups","value":31,"labels":{"plan":"free"}}`))
	if !ok {
		t.Fatal("a line with metric+value is a metric")
	}
	if m.Name != "signups" || m.Value != 31 {
		t.Fatalf("got %+v", m)
	}
	if m.Labels["plan"] != "free" {
		t.Fatalf("labels lost: %+v", m.Labels)
	}

	if _, ok := ParseMetric([]byte(`{"message":"user signed up","level":"info"}`)); ok {
		t.Fatal("a log line was taken for a metric")
	}
	// A metric with no value is not a measurement: storing 0 would be a
	// reading nobody took.
	if _, ok := ParseMetric([]byte(`{"metric":"signups"}`)); ok {
		t.Fatal("a metric with no value was accepted")
	}
	// Nor plain text.
	if _, ok := ParseMetric([]byte(`nginx upstream timed out`)); ok {
		t.Fatal("plain text was taken for a metric")
	}
}

func TestParseMetricKeepsItsTimestamp(t *testing.T) {
	m, ok := ParseMetric([]byte(`{"metric":"latency_ms","value":182.5,"ts":"2026-08-15T00:37:23Z"}`))
	if !ok {
		t.Fatal("a metric with ts should parse")
	}
	if m.TS.IsZero() {
		t.Fatal("ts was dropped: the customer's clock is the reading's clock")
	}
	if m.Value != 182.5 {
		t.Fatalf("float value lost: %v", m.Value)
	}
}
