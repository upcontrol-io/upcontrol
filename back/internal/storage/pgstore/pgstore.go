// Package pgstore owns the telemetry Postgres holds: logs, events, checks,
// metrics, web analytics, the per-minute counts the detector's baseline reads,
// and the read half that replaced the ClickHouse connection. It replaces the
// ClickHouse writer of the same shape.
package pgstore

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Store is the Postgres telemetry writer; the pool is owned and closed elsewhere.
type Store struct{ pool *pgxpool.Pool }

// New wraps an existing pool.
func New(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

// LogRow is one row of the logs table, mirroring ch.LogRow: TenantID/ProjectID/
// Seq are added by the ingest layer, the rest comes from the decoder
// (cardinality already applied).
type LogRow struct {
	TenantID    uint64
	ProjectID   uint64
	TS          time.Time
	Seq         uint64
	Source      string
	Service     string
	Host        string
	Level       string
	LevelRaw    string
	Message     string
	Fingerprint uint64 // stored as signed bigint via int64(v): a hash above MaxInt64 wraps, which is fine — it is an opaque identity, never compared for order
	Attrs       map[string]string
}

// EventRow is one row of the events table (never displaced by the ring).
type EventRow struct {
	TenantID    uint64
	ProjectID   uint64
	TS          time.Time
	Name        string
	Labels      map[string]string
	AmountMinor int64
	Currency    string
}

// CheckRow is one row of the checks table: the availability detector's history
// and the public status page; written once per SubmitResults batch.
type CheckRow struct {
	TenantID   uint64
	MonitorID  uint64
	TS         time.Time
	Region     string
	OK         bool
	StatusCode uint16
	ErrorClass string
	DNSMs      uint32
	ConnectMs  uint32
	TLSMs      uint32
	TTFBMs     uint32
	TotalMs    uint32
	BodyHash   uint64 // same int64 wrap as LogRow.Fingerprint
}

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

// WebEventRow is one row of web_events; analytics is the only writer. IPHash
// is sha256(client IP) truncated to 8 bytes; a full IP is never stored.
type WebEventRow struct {
	TS          time.Time
	VisitorID   uint64
	PersonID    uint64
	TenantID    uint64
	Name        string
	Path        string
	Title       string
	Referrer    string
	UTMSource   string
	UTMMedium   string
	UTMCampaign string
	Country     string
	IPHash      [8]byte
	Device      string
	OS          string
	Browser     string
	Props       map[string]string
}

// SeriesBump is one per-minute (source, level) count for a flushed batch; the
// caller aggregates per minute before calling, so keys are unique within a call.
type SeriesBump struct {
	TenantID  uint64
	ProjectID uint64
	Minute    time.Time
	Source    string
	Level     string
	Lines     int64
	Bytes     int64
}

// InsertLogs writes a batch of log rows via COPY: logs is the hot path and the
// batcher delivers rows pre-grouped, one COPY per flush.
func (s *Store) InsertLogs(ctx context.Context, rows []LogRow) error {
	return copyFrom(ctx, s.pool, "logs",
		[]string{"tenant_id", "project_id", "ts", "seq", "source", "service",
			"host", "level", "level_raw", "message", "fingerprint", "attrs"},
		rows, func(r LogRow) []any {
			return []any{int64(r.TenantID), int64(r.ProjectID), r.TS, int64(r.Seq),
				r.Source, r.Service, r.Host, r.Level, r.LevelRaw, r.Message,
				int64(r.Fingerprint), jsonb(r.Attrs)}
		})
}

// InsertEvents writes a batch of event rows.
func (s *Store) InsertEvents(ctx context.Context, rows []EventRow) error {
	return copyFrom(ctx, s.pool, "events",
		[]string{"tenant_id", "project_id", "ts", "name", "labels", "amount_minor", "currency"},
		rows, func(r EventRow) []any {
			return []any{int64(r.TenantID), int64(r.ProjectID), r.TS, r.Name,
				jsonb(r.Labels), r.AmountMinor, r.Currency}
		})
}

// InsertChecks writes a batch of check rows.
func (s *Store) InsertChecks(ctx context.Context, rows []CheckRow) error {
	return copyFrom(ctx, s.pool, "checks",
		[]string{"tenant_id", "monitor_id", "ts", "region", "ok", "status_code",
			"error_class", "dns_ms", "connect_ms", "tls_ms", "ttfb_ms", "total_ms", "body_hash"},
		rows, func(r CheckRow) []any {
			// int columns take a Go int (not int64): binary COPY encodes each
			// value against the column's own OID.
			return []any{int64(r.TenantID), int64(r.MonitorID), r.TS, r.Region, r.OK,
				int(r.StatusCode), r.ErrorClass, int(r.DNSMs), int(r.ConnectMs),
				int(r.TLSMs), int(r.TTFBMs), int(r.TotalMs), int64(r.BodyHash)}
		})
}

// InsertMetrics writes a batch of metric rows.
func (s *Store) InsertMetrics(ctx context.Context, rows []MetricRow) error {
	return copyFrom(ctx, s.pool, "metrics",
		[]string{"tenant_id", "project_id", "ts", "name", "labels", "value"},
		rows, func(r MetricRow) []any {
			return []any{int64(r.TenantID), int64(r.ProjectID), r.TS, r.Name,
				jsonb(r.Labels), r.Value}
		})
}

// InsertWebEvents writes a batch of web event rows.
func (s *Store) InsertWebEvents(ctx context.Context, rows []WebEventRow) error {
	return copyFrom(ctx, s.pool, "web_events",
		[]string{"visitor_id", "person_id", "tenant_id", "ts", "name", "path", "title",
			"referrer", "utm_source", "utm_medium", "utm_campaign", "country",
			"ip_hash", "device", "os", "browser", "props"},
		rows, func(r WebEventRow) []any {
			return []any{int64(r.VisitorID), int64(r.PersonID), int64(r.TenantID), r.TS,
				r.Name, r.Path, r.Title, r.Referrer, r.UTMSource, r.UTMMedium, r.UTMCampaign,
				r.Country, r.IPHash[:], r.Device, r.OS, r.Browser, jsonb(r.Props)}
		})
}

// BumpSeries adds the per-minute line and byte counts for a flushed batch, as
// one statement: the upsert that replaces ClickHouse's series_1m_mv. All rows
// of one flush go in together, so contention on a hot row is bounded by the
// flush interval, not by the line count.
func (s *Store) BumpSeries(ctx context.Context, rows []SeriesBump) error {
	if len(rows) == 0 {
		return nil
	}
	var q strings.Builder
	args := make([]any, 0, len(rows)*7)
	q.WriteString("INSERT INTO series_1m (tenant_id, project_id, minute, source, level, lines, bytes) VALUES ")
	for i, r := range rows {
		if i > 0 {
			q.WriteByte(',')
		}
		b := i * 7
		fmt.Fprintf(&q, "($%d,$%d,$%d,$%d,$%d,$%d,$%d)", b+1, b+2, b+3, b+4, b+5, b+6, b+7)
		args = append(args, int64(r.TenantID), int64(r.ProjectID), r.Minute,
			r.Source, r.Level, r.Lines, r.Bytes)
	}
	q.WriteString(" ON CONFLICT (tenant_id, project_id, minute, source, level)" +
		" DO UPDATE SET lines = series_1m.lines + EXCLUDED.lines," +
		" bytes = series_1m.bytes + EXCLUDED.bytes")
	_, err := s.pool.Exec(ctx, q.String(), args...)
	return err
}

// copyFrom runs the shared batch-insert shape, mirroring ch.insert: no-op on
// empty rows, one COPY per call.
func copyFrom[T any](ctx context.Context, pool *pgxpool.Pool, table string, cols []string, rows []T, vals func(T) []any) error {
	if len(rows) == 0 {
		return nil
	}
	_, err := pool.CopyFrom(ctx, pgx.Identifier{table}, cols, &copySource[T]{rows: rows, vals: vals})
	return err
}

type copySource[T any] struct {
	rows []T
	i    int
	vals func(T) []any
}

func (s *copySource[T]) Next() bool { s.i++; return s.i <= len(s.rows) }

func (s *copySource[T]) Values() ([]any, error) { return s.vals(s.rows[s.i-1]), nil }

func (s *copySource[T]) Err() error { return nil }

// jsonb marshals a map for a jsonb column as pre-encoded JSON (pgx passes
// []byte through verbatim). nil is the empty object, matching the ClickHouse
// writer where a nil Map inserts empty.
func jsonb(m map[string]string) []byte {
	if len(m) == 0 {
		return []byte("{}")
	}
	b, err := json.Marshal(m)
	if err != nil { // map[string]string: unreachable
		return []byte("{}")
	}
	return b
}
