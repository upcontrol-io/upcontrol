package ingest

import (
	"bytes"
	"encoding/json"
	"time"

	"go.upcontrol.io/back/internal/ingest/decode"
)

// MetricLine is one metric reading off the wire, before storage.
type MetricLine struct {
	TS     time.Time
	Name   string
	Value  float64
	Labels map[string]string
}

// MetricEnvelope is the metric row handed to the batcher and the WAL — the
// metric twin of RowEnvelope. The CH sink adapter decodes it into ch.MetricRow.
type MetricEnvelope struct {
	TenantID  int64             `json:"tenant_id"`
	ProjectID int64             `json:"project_id"`
	TS        string            `json:"ts,omitempty"`
	Name      string            `json:"name"`
	Value     float64           `json:"value"`
	Labels    map[string]string `json:"labels,omitempty"`
}

// metricWire mirrors MetricLine in JSON. Value is a *float64 so an absent
// value is distinguishable from a real 0 — "signups" with no number is not a
// measurement, and storing 0 would be a reading nobody took.
type metricWire struct {
	TS     *string           `json:"ts"`
	Metric string            `json:"metric"`
	Value  *float64          `json:"value"`
	Labels map[string]string `json:"labels"`
}

// ParseMetric recognises a metric line: JSON carrying both a non-empty
// `metric` string and a *present* numeric `value`. The bool is false when the
// line is not a metric, which means it is a log line and travels the existing
// path — a log line mistaken for a metric would silently empty the stream.
//
// `ts` is honoured when present (the customer's clock is the reading's clock);
// zero means the caller stamps now, same contract as a log line's ts_absent.
func ParseMetric(raw []byte) (MetricLine, bool) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return MetricLine{}, false
	}
	var w metricWire
	if err := json.Unmarshal(trimmed, &w); err != nil {
		return MetricLine{}, false
	}
	if w.Metric == "" || w.Value == nil {
		return MetricLine{}, false
	}
	m := MetricLine{Name: w.Metric, Value: *w.Value, Labels: w.Labels}
	if w.TS != nil {
		if ts, err := time.Parse(time.RFC3339Nano, *w.TS); err == nil {
			m.TS = ts
		}
	}
	return m, true
}

// splitMetrics partitions decoded records into log records and serialised
// MetricEnvelopes. A record whose Raw (set by the JSON-decoder forms) parses as
// a metric leaves the log path — or the stream would silently empty, a reading
// rendered as a message. Timestamps prefer the decode pass (ts normalisation
// lives there, incl. the ts_absent warning); a metric whose own ts survived
// only in Raw keeps that; otherwise now.
func (h *Ingester) splitMetrics(t Tenant, recs []decode.Record) ([]decode.Record, [][]byte) {
	logs := make([]decode.Record, 0, len(recs))
	var metrics [][]byte
	for _, rec := range recs {
		if len(rec.Raw) > 0 {
			if m, ok := ParseMetric(rec.Raw); ok {
				ts := rec.Time
				if ts.IsZero() {
					ts = m.TS
				}
				if ts.IsZero() {
					ts = time.Now().UTC()
				}
				env := MetricEnvelope{
					TenantID:  t.TenantID,
					ProjectID: t.ProjectID,
					TS:        ts.UTC().Format("2006-01-02T15:04:05.000Z07:00"),
					Name:      m.Name,
					Value:     m.Value,
					Labels:    m.Labels,
				}
				b, _ := json.Marshal(env)
				metrics = append(metrics, b)
				continue
			}
		}
		logs = append(logs, rec)
	}
	return logs, metrics
}
