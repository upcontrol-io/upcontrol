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

// MetricEnvelope is the metric row handed to the batcher — the metric twin
// of RowEnvelope. The CH sink adapter decodes it into ch.MetricRow.
type MetricEnvelope struct {
	TenantID  int64             `json:"tenant_id"`
	ProjectID int64             `json:"project_id"`
	TS        string            `json:"ts,omitempty"`
	Name      string            `json:"name"`
	Value     float64           `json:"value"`
	Labels    map[string]string `json:"labels,omitempty"`
}

// metricWire mirrors MetricLine in JSON; Value is *float64 so an absent value
// is distinguishable from a real 0 ("signups" with no number is no reading).
type metricWire struct {
	TS     *string           `json:"ts"`
	Metric string            `json:"metric"`
	Value  *float64          `json:"value"`
	Labels map[string]string `json:"labels"`
}

// ParseMetric recognises a metric line: JSON with a non-empty `metric` and a
// *present* numeric `value`; false = a log line (strictness keeps the stream).
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

// splitMetrics partitions records into logs and serialised MetricEnvelopes; a
// metric record leaves the log path. Timestamp: decode pass, else own, else now.
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
