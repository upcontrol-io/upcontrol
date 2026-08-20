// Package api wires the ingest pipeline into the ucapi HTTP server. The four
// adapters here bridge the handler's interfaces (defined in internal/ingest) to
// the real storage layer (storage/pg, storage/ch) and the supporting packages
// (ingest/wal, ingest/batcher, ingest/cardinality, ring/seq).
package api

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"sync"
	"time"

	"go.upcontrol.io/back/internal/ingest"
	"go.upcontrol.io/back/internal/ingest/batcher"
	"go.upcontrol.io/back/internal/ingest/cardinality"
	"go.upcontrol.io/back/internal/ingest/wal"
	"go.upcontrol.io/back/internal/ring/seq"
	"go.upcontrol.io/back/internal/storage/ch"
	"go.upcontrol.io/back/internal/storage/pg"
)

// chLogSink adapts batcher.Sink: when the batcher flushes, it decodes each JSON
// RowEnvelope back into a ch.LogRow and batch-inserts into ClickHouse. The JSON
// roundtrip is the seam that keeps the batcher CH-agnostic; a future optimisation
// can pass typed rows through a typed batcher to avoid it.
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

// decodeRows is the log path's decode step: JSON RowEnvelopes in, ch.LogRows
// plus the promoted ch.EventRows out. A row the classifier marked with an
// event name (env.Event) is double-written: the log line stays in logs, and a
// copy goes to events — the feed LastDeployAt, the incident timeline's deploy
// join and the absence detector all live on events. Labels are a copy: the sha
// aliasing below must never leak back into env.Attrs (the LogRow shares it).
func decodeRows(rows [][]byte) ([]ch.LogRow, []ch.EventRow) {
	logRows := make([]ch.LogRow, 0, len(rows))
	var eventRows []ch.EventRow
	for _, raw := range rows {
		var env ingest.RowEnvelope
		if err := json.Unmarshal(raw, &env); err != nil {
			continue // a corrupt row is dropped, not fatal
		}
		lr := ch.LogRow{
			TenantID:  uint64(env.TenantID),
			ProjectID: uint64(env.ProjectID),
			Seq:       uint64(env.Seq),
			Source:    "sdk",
			Service:   env.Service,
			Host:      env.Host,
			Level:     env.Level,
			Message:   env.Message,
			Attrs:     env.Attrs,
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

// walAdapter wraps ingest/wal.WAL to satisfy ingest.WALAppender.
type walAdapter struct {
	w *wal.WAL
}

func (a *walAdapter) AppendSync(_ context.Context, payload []byte) error {
	if _, _, err := a.w.Append(payload); err != nil {
		return err
	}
	_, err := a.w.Sync()
	return err
}

// batcherSink wraps ingest/batcher.Batcher to satisfy ingest.BatchSink.
type batcherSink struct {
	b *batcher.Batcher
}

func (s *batcherSink) Add(ctx context.Context, table string, row []byte) error {
	return s.b.Add(ctx, table, row)
}

// dirSpoolFiller computes the WAL/spool fill percentage from the directory size.
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

// WireIngest builds the full POST /i pipeline from real dependencies. The
// returned Ingester's Handle method is the POST /i http.HandlerFunc; the returned
// Batcher must be Tick'd periodically and Close'd on shutdown.

// Type aliases re-export the concrete types so cmd/ucapi wires through the api
// package without importing ingest or batcher directly.

type Ingester = ingest.Ingester
type Batcher = batcher.Batcher

func WireIngest(spoolDir string, pgPool *pg.Pool, chConn *ch.Conn) (*Ingester, *Batcher, error) {
	// WAL: durable append+fsync before receipt.
	w, err := wal.Open(filepath.Join(spoolDir, "ingest.wal"))
	if err != nil {
		return nil, nil, err
	}

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
		WAL:   &walAdapter{w: w},
		Card:  cardinality.New(1000),
	})

	return ing, bs, nil
}

// seqAllocators satisfies ingest.SeqAllocator with one ring/seq.Allocator per
// project. Keying by project matters twice over: two projects must never draw
// from one counter, and a lease against a project id that has no project_seq
// row errors — which ingest records as seq 0 on every line, collapsing the
// ring's order (and the /app log selection keyed on seq) for the whole window.
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
