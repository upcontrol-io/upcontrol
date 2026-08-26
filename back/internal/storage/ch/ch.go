// Package ch is the ClickHouse storage layer and the ONLY writer of the logs
// and events tables; ucprobe must not import it (depguard-enforced).
package ch

import (
	"context"
	"errors"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// Conn is a pooled ClickHouse native-protocol client.
type Conn struct {
	db driver.Conn
}

// Options configures the connection pool.
type Options struct {
	Addr         []string // host:port (native 9000)
	Database     string
	Username     string
	Password     string
	MaxOpenConns int
}

// Open connects and pings. A failure to reach ClickHouse at startup is fatal —
// the ingest path has nowhere to put rows without it.
func Open(ctx context.Context, opt Options) (*Conn, error) {
	if len(opt.Addr) == 0 {
		return nil, errors.New("ch: no addresses")
	}
	if opt.MaxOpenConns == 0 {
		opt.MaxOpenConns = 8
	}
	db, err := clickhouse.Open(&clickhouse.Options{
		Addr: opt.Addr,
		Auth: clickhouse.Auth{
			Database: opt.Database,
			Username: opt.Username,
			Password: opt.Password,
		},
		MaxOpenConns: opt.MaxOpenConns,
		Settings: clickhouse.Settings{
			"max_execution_time": 60,
		},
		DialTimeout: 5 * time.Second,
	})
	if err != nil {
		return nil, err
	}
	if err := db.Ping(ctx); err != nil {
		return nil, err
	}
	return &Conn{db: db}, nil
}

// Ping is the health probe (registered with platform/health as "clickhouse").
func (c *Conn) Ping(ctx context.Context) error { return c.db.Ping(ctx) }

// Close releases the pool.
func (c *Conn) Close() error { return c.db.Close() }

// LogRow is one row of the logs table; TenantID/ProjectID/Seq are added by the
// ingest layer, the rest comes from the decoder (cardinality already applied).
type LogRow struct {
	TenantID    uint64
	ProjectID   uint64
	TS          time.Time
	Seq         uint64
	Source      string
	Service     string
	Host        string
	Level       string
	Message     string
	Fingerprint uint64
	Attrs       map[string]string
}

// insert runs the shared batch-insert shape: no-op on empty rows, one
// PrepareBatch, one app call per row, one Send.
func insert[T any](ctx context.Context, c *Conn, sql string, rows []T, app func(driver.Batch, T) error) error {
	if len(rows) == 0 {
		return nil
	}
	batch, err := c.db.PrepareBatch(ctx, sql)
	if err != nil {
		return err
	}
	for _, r := range rows {
		if err := app(batch, r); err != nil {
			return err
		}
	}
	return batch.Send()
}

// InsertLogs writes a batch of log rows via the native batch protocol. The
// batcher calls this per flush; one INSERT per call → one ClickHouse part.
func (c *Conn) InsertLogs(ctx context.Context, rows []LogRow) error {
	return insert(ctx, c, "INSERT INTO logs "+
		"(tenant_id, project_id, ts, seq, source, service, host, level, message, fingerprint, attrs)",
		rows, func(b driver.Batch, r LogRow) error {
			return b.Append(
				r.TenantID, r.ProjectID, r.TS, r.Seq,
				r.Source, r.Service, r.Host, r.Level, r.Message, r.Fingerprint, r.Attrs,
			)
		})
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

// InsertEvents writes a batch of event rows.
func (c *Conn) InsertEvents(ctx context.Context, rows []EventRow) error {
	return insert(ctx, c, "INSERT INTO events "+
		"(tenant_id, project_id, ts, name, labels, amount_minor, currency)",
		rows, func(b driver.Batch, r EventRow) error {
			return b.Append(r.TenantID, r.ProjectID, r.TS, r.Name, r.Labels, r.AmountMinor, r.Currency)
		})
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

// InsertWebEvents writes a batch of web event rows. is_app is MATERIALIZED in
// the table (startsWith(path, '/app')) and therefore not part of the insert.
func (c *Conn) InsertWebEvents(ctx context.Context, rows []WebEventRow) error {
	return insert(ctx, c, "INSERT INTO web_events "+
		"(visitor_id, person_id, tenant_id, ts, name, path, title, referrer, "+
		"utm_source, utm_medium, utm_campaign, country, ip_hash, device, os, browser, props)",
		rows, func(b driver.Batch, r WebEventRow) error {
			return b.Append(
				r.VisitorID, r.PersonID, r.TenantID, r.TS, r.Name, r.Path, r.Title, r.Referrer,
				r.UTMSource, r.UTMMedium, r.UTMCampaign, r.Country, r.IPHash, r.Device, r.OS, r.Browser, r.Props,
			)
		})
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
	BodyHash   uint64
}

// InsertChecks writes a batch of check rows via the native batch protocol.
func (c *Conn) InsertChecks(ctx context.Context, rows []CheckRow) error {
	return insert(ctx, c, "INSERT INTO checks "+
		"(tenant_id, monitor_id, ts, region, ok, status_code, error_class, "+
		"dns_ms, connect_ms, tls_ms, ttfb_ms, total_ms, body_hash)",
		rows, func(b driver.Batch, r CheckRow) error {
			return b.Append(
				r.TenantID, r.MonitorID, r.TS, r.Region, r.OK, r.StatusCode, r.ErrorClass,
				r.DNSMs, r.ConnectMs, r.TLSMs, r.TTFBMs, r.TotalMs, r.BodyHash,
			)
		})
}

// Raw exposes the underlying driver for callers that need arbitrary SQL; logs
// reads stay behind ring.QueryBuilder; this is the seam.
func (c *Conn) Raw() driver.Conn { return c.db }
