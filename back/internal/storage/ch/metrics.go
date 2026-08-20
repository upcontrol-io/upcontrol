package ch

import (
	"context"
	"time"
)

// MetricRow is one row of the metrics table, written by the ingest batcher
// when a POST /i line carries metric+value.
type MetricRow struct {
	TenantID  uint64
	ProjectID uint64
	TS        time.Time
	Name      string
	Labels    map[string]string
	Value     float64
}

// InsertMetrics writes a batch of metric rows, mirroring InsertEvents.
func (c *Conn) InsertMetrics(ctx context.Context, rows []MetricRow) error {
	if len(rows) == 0 {
		return nil
	}
	batch, err := c.db.PrepareBatch(ctx, "INSERT INTO metrics "+
		"(tenant_id, project_id, ts, name, labels, value)")
	if err != nil {
		return err
	}
	for _, r := range rows {
		if err := batch.Append(r.TenantID, r.ProjectID, r.TS, r.Name, r.Labels, r.Value); err != nil {
			return err
		}
	}
	return batch.Send()
}
