package ch

import (
	"context"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
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
	return insert(ctx, c, "INSERT INTO metrics "+
		"(tenant_id, project_id, ts, name, labels, value)",
		rows, func(b driver.Batch, r MetricRow) error {
			return b.Append(r.TenantID, r.ProjectID, r.TS, r.Name, r.Labels, r.Value)
		})
}
