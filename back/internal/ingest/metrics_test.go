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

// A funnel reading from the SDK is a metric with its labels, so it leaves the
// log path: the funnel must never show up in the log window.
func TestParseMetricTakesAFunnelReading(t *testing.T) {
	m, ok := ParseMetric([]byte(`{"ts":"2026-09-06T12:00:00.000Z","metric":"funnel","value":14135,"labels":{"funnel":"visit to paid","step":"visit","i":"01"}}`))
	if !ok {
		t.Fatal("a funnel reading is a metric")
	}
	if m.Name != "funnel" || m.Value != 14135 {
		t.Fatalf("got %+v", m)
	}
	if m.Labels["funnel"] != "visit to paid" || m.Labels["step"] != "visit" || m.Labels["i"] != "01" {
		t.Fatalf("labels lost: %+v", m.Labels)
	}
	if m.TS.IsZero() {
		t.Fatal("ts was dropped")
	}
}
