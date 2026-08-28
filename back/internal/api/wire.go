// Package api wires the ingest pipeline into the ucapi HTTP server: adapters
// bridge the handler's interfaces to storage and the supporting packages.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"os"
	"sync"
	"time"

	"go.upcontrol.io/back/internal/ingest"
	"go.upcontrol.io/back/internal/ingest/batcher"
	"go.upcontrol.io/back/internal/ingest/cardinality"
	"go.upcontrol.io/back/internal/ring/seq"
	"go.upcontrol.io/back/internal/storage/ch"
	"go.upcontrol.io/back/internal/storage/pg"
)

// chLogSink adapts batcher.Sink: decoded RowEnvelopes batch-insert into
// ClickHouse. The JSON roundtrip is the seam that keeps the batcher CH-agnostic.
type chLogSink struct {
	conn *ch.Conn
}

func (s *chLogSink) Flush(ctx context.Context, key string, rows [][]byte) error {
	if len(rows) == 0 {
		return nil
	}
	// The batcher keys by table; metrics leave via their own column form.
	if key == "metrics" {
		return s.flushMetrics(ctx, rows)
	}
	logRows, eventRows := decodeRows(rows)
	logErr := s.conn.InsertLogs(ctx, logRows)
	var evErr error
	if len(eventRows) > 0 {
		evErr = s.conn.InsertEvents(ctx, eventRows)
	}
	// The batcher swaps the batch out before calling Flush and never retries a
	// failed flush, so a partial insert cannot duplicate on a later attempt.
	return errors.Join(logErr, evErr)
}

// decodeRows decodes RowEnvelopes into LogRows plus promoted EventRows; an
// event-named row is double-written. Labels are cloned: sha aliasing stays out.
func decodeRows(rows [][]byte) ([]ch.LogRow, []ch.EventRow) {
	logRows := make([]ch.LogRow, 0, len(rows))
	var eventRows []ch.EventRow
	for _, raw := range rows {
		var env ingest.RowEnvelope
		if err := json.Unmarshal(raw, &env); err != nil {
			continue // a corrupt row is dropped, not fatal
		}
		lr := ch.LogRow{
			TenantID:    uint64(env.TenantID),
			ProjectID:   uint64(env.ProjectID),
			Seq:         uint64(env.Seq),
			Source:      "sdk",
			Service:     env.Service,
			Host:        env.Host,
			Level:       env.Level,
			LevelRaw:    env.LevelRaw,
			Message:     env.Message,
			Fingerprint: env.Fingerprint,
			Attrs:       env.Attrs,
		}
		if env.TS != "" {
			lr.TS, _ = time.Parse(time.RFC3339Nano, env.TS)
		}
		if lr.TS.IsZero() {
			lr.TS = time.Now().UTC()
		}
		logRows = append(logRows, lr)
		if env.Event != "" {
			labels := maps.Clone(env.Attrs)
			// Ingest carries the deploy sha as commit_sha; read_api's eventText
			// reads Labels["sha"] (read_api.go:741) — alias when sha is empty.
			if labels["sha"] == "" && labels["commit_sha"] != "" {
				labels["sha"] = labels["commit_sha"]
			}
			eventRows = append(eventRows, ch.EventRow{
				TenantID:    uint64(env.TenantID),
				ProjectID:   uint64(env.ProjectID),
				TS:          lr.TS,
				Name:        env.Event,
				Labels:      labels,
				AmountMinor: 0,
				Currency:    "",
			})
		}
	}
	return logRows, eventRows
}

// flushMetrics decodes MetricEnvelopes into ch.MetricRows — the metric twin of
// the log path above.
func (s *chLogSink) flushMetrics(ctx context.Context, rows [][]byte) error {
	metricRows := make([]ch.MetricRow, 0, len(rows))
	for _, raw := range rows {
		var env ingest.MetricEnvelope
		if err := json.Unmarshal(raw, &env); err != nil {
			continue // a corrupt row is dropped, not fatal
		}
		mr := ch.MetricRow{
			TenantID:  uint64(env.TenantID),
			ProjectID: uint64(env.ProjectID),
			Name:      env.Name,
			Labels:    env.Labels,
			Value:     env.Value,
		}
		if env.TS != "" {
			mr.TS, _ = time.Parse(time.RFC3339Nano, env.TS)
		}
		if mr.TS.IsZero() {
			mr.TS = time.Now().UTC()
		}
		metricRows = append(metricRows, mr)
	}
	return s.conn.InsertMetrics(ctx, metricRows)
}

// batcherSink wraps ingest/batcher.Batcher to satisfy ingest.BatchSink.
type batcherSink struct {
	b *batcher.Batcher
}

func (s *batcherSink) Add(ctx context.Context, table string, row []byte) error {
	return s.b.Add(ctx, table, row)
}

// dirSpoolFiller computes the spool fill percentage from the directory size.
type dirSpoolFiller struct {
	dir string
	max int64
}

func (f *dirSpoolFiller) FillPercent(_ context.Context) (int, error) {
	var size int64
	entries, err := os.ReadDir(f.dir)
	if err != nil {
		return 0, nil // directory missing → 0% (not an error condition)
	}
	for _, e := range entries {
		info, _ := e.Info()
		size += info.Size()
	}
	if f.max <= 0 {
		return 0, nil
	}
	pct := int(size * 100 / f.max)
	if pct > 100 {
		pct = 100
	}
	return pct, nil
}

// WireIngest builds the full POST /i pipeline. The returned Batcher must be
// Tick'd periodically and Close'd on shutdown.

// Batcher is re-exported so cmd/ucapi can name the ticker's type without
// importing batcher directly.

type Batcher = batcher.Batcher

func WireIngest(spoolDir string, scrubOff bool, pgPool *pg.Pool, chConn *ch.Conn) (*ingest.Ingester, *Batcher, error) {
	// Batcher: flushes to ClickHouse on 8 MiB / 200 ms / 1-sec-per-key.
	bs := batcher.New(&chLogSink{conn: chConn}, nil, batcher.Options{})

	// Seq allocators: one per project, created on first use, each leasing
	// 10k-value blocks from its own project_seq row.
	allocs := &seqAllocators{
		leaser:    pg.NewSeqLeaser(pgPool),
		byProject: map[int64]*seq.Allocator{},
	}

	ing := ingest.New(ingest.Deps{
		Keys:  pg.NewKeyResolver(pgPool, nil),
		Seq:   allocs,
		Sink:  &batcherSink{b: bs},
		Idem:  pg.NewIdempotency(pgPool),
		Spool: &dirSpoolFiller{dir: spoolDir, max: 1 << 30},
		Card:  cardinality.New(1000),
		// Self-host only; config refuses the switch on the hosted service.
		ScrubOff: scrubOff,
	})

	return ing, bs, nil
}

// seqAllocators holds one ring/seq.Allocator per project: two projects must
// never draw from one counter, and a failed lease collapses seq to 0.
type seqAllocators struct {
	leaser seq.BlockLeaser

	mu        sync.Mutex
	byProject map[int64]*seq.Allocator
}

func (s *seqAllocators) Next(ctx context.Context, projectID int64) (int64, error) {
	s.mu.Lock()
	a := s.byProject[projectID]
	if a == nil {
		a = seq.New(projectID, 10000, s.leaser)
		s.byProject[projectID] = a
	}
	s.mu.Unlock()
	return a.Next(ctx)
}
