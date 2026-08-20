// Package batcher accumulates decoded log rows and flushes them to ClickHouse
// in batches. The flush policy (plan §4.3) is the only thing between ingest
// throughput and ClickHouse's part count:
//
//   - flush at batchBytes (default 8 MiB), OR
//   - flush when a batch is older than batchAge (default 200 ms) BUT not more
//     often than once per second per key (the 1/sec-per-(table×bucket) floor),
//   - and always flush everything on Close.
//
// The §3.7 gate is "1 row/sec for an hour → ≤ 3600 parts": a part is born per
// INSERT, so capping inserts at one per second per key caps parts at 3600/hour.
//
// The batcher owns no ClickHouse client; it calls a Sink, so the flush policy is
// unit-testable with a fake sink and a fake clock (synctest-friendly).
package batcher

import (
	"context"
	"sync"
	"time"
)

// Sink receives a flushed batch for one key (table×bucket). The implementation
// (storage/ch) does the actual batch INSERT.
type Sink interface {
	Flush(ctx context.Context, key string, rows [][]byte) error
}

// Clock is the minimal time surface the batcher needs; tests pass a fake so the
// 200 ms / 1 s thresholds are deterministic under testing/synctest.
type Clock interface {
	Now() time.Time
}

// Options configures the flush policy. Zero values fall back to the plan's
// 8 MiB / 200 ms / 1-per-second defaults.
type Options struct {
	BatchBytes    int
	BatchAge      time.Duration
	MinInterval   time.Duration           // 1/sec-per-key floor; 0 means BatchAge alone
	FlushCallback func(key string, n int) // optional, for tests/metrics
}

// Batcher accumulates rows per key and flushes per the policy.
type Batcher struct {
	sink Sink
	clk  Clock
	opt  Options

	mu      sync.Mutex
	pending map[string]*batch
}

type batch struct {
	rows      [][]byte
	bytes     int
	oldestAt  time.Time // when the first pending row arrived
	lastFlush time.Time // used to enforce MinInterval
}

const (
	defaultBatchBytes  = 8 << 20
	defaultBatchAge    = 200 * time.Millisecond
	defaultMinInterval = 1 * time.Second
)

// New builds a Batcher. clk may be nil (real clock).
func New(sink Sink, clk Clock, opt Options) *Batcher {
	if opt.BatchBytes <= 0 {
		opt.BatchBytes = defaultBatchBytes
	}
	if opt.BatchAge <= 0 {
		opt.BatchAge = defaultBatchAge
	}
	if opt.MinInterval <= 0 {
		opt.MinInterval = defaultMinInterval
	}
	if clk == nil {
		clk = sysClock{}
	}
	return &Batcher{sink: sink, clk: clk, opt: opt, pending: map[string]*batch{}}
}

// Add appends a row under key. If the key's batch crosses BatchBytes it is
// flushed immediately (size threshold ignores MinInterval). Returns nil once the
// row is accepted (it may or may not be flushed yet).
func (b *Batcher) Add(ctx context.Context, key string, row []byte) error {
	flushNow := false
	var toFlush *batch

	b.mu.Lock()
	ba := b.pending[key]
	if ba == nil {
		ba = &batch{}
		b.pending[key] = ba
	}
	if len(ba.rows) == 0 {
		ba.oldestAt = b.clk.Now()
	}
	ba.rows = append(ba.rows, row)
	ba.bytes += len(row)
	if ba.bytes >= b.opt.BatchBytes {
		toFlush = ba
		flushNow = true
	}
	b.mu.Unlock()

	if flushNow {
		return b.flush(ctx, key, toFlush)
	}
	return nil
}

// Tick ages out batches: any key whose batch is older than BatchAge AND whose
// last flush was at least MinInterval ago is flushed. ucworker/ucapi drives this
// on a short ticker (e.g. every 50 ms).
func (b *Batcher) Tick(ctx context.Context) error {
	now := b.clk.Now()
	// Snapshot the keys eligible for flush under the lock, then flush outside.
	type pending struct {
		key string
		ba  *batch
	}
	var elig []pending
	b.mu.Lock()
	for k, ba := range b.pending {
		if len(ba.rows) == 0 {
			continue
		}
		aged := now.Sub(ba.oldestAt) >= b.opt.BatchAge
		paceOK := ba.lastFlush.IsZero() || now.Sub(ba.lastFlush) >= b.opt.MinInterval
		if aged && paceOK {
			elig = append(elig, pending{k, ba})
		}
	}
	b.mu.Unlock()

	// Flush each eligible batch. To avoid flushing a batch that grew between the
	// snapshot and the flush, swap-and-flush: take the batch out, replace with a
	// fresh empty one.
	var firstErr error
	for _, p := range elig {
		b.mu.Lock()
		current := b.pending[p.key]
		if current == nil || len(current.rows) == 0 {
			b.mu.Unlock()
			continue
		}
		// Re-check pace under the lock (another Tick may have flushed).
		if !current.lastFlush.IsZero() && now.Sub(current.lastFlush) < b.opt.MinInterval {
			b.mu.Unlock()
			continue
		}
		b.pending[p.key] = &batch{lastFlush: now}
		b.mu.Unlock()
		if err := b.sink.Flush(ctx, p.key, current.rows); err != nil {
			firstErr = err
		} else if b.opt.FlushCallback != nil {
			b.opt.FlushCallback(p.key, len(current.rows))
		}
	}
	return firstErr
}

// Close flushes every pending batch regardless of thresholds.
func (b *Batcher) Close(ctx context.Context) error {
	b.mu.Lock()
	snap := b.pending
	b.pending = map[string]*batch{}
	b.mu.Unlock()
	var firstErr error
	for k, ba := range snap {
		if len(ba.rows) == 0 {
			continue
		}
		if err := b.sink.Flush(ctx, k, ba.rows); err != nil {
			firstErr = err
		} else if b.opt.FlushCallback != nil {
			b.opt.FlushCallback(k, len(ba.rows))
		}
	}
	return firstErr
}

// flush sends one batch's rows to the sink and resets that key's batch.
func (b *Batcher) flush(ctx context.Context, key string, ba *batch) error {
	b.mu.Lock()
	// Take the batch out and replace with a fresh one that inherits lastFlush.
	now := b.clk.Now()
	b.pending[key] = &batch{lastFlush: now}
	rows := ba.rows
	b.mu.Unlock()
	if err := b.sink.Flush(ctx, key, rows); err != nil {
		return err
	}
	if b.opt.FlushCallback != nil {
		b.opt.FlushCallback(key, len(rows))
	}
	return nil
}

// Pending reports the total rows across all keys (for tests/observability).
func (b *Batcher) Pending() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := 0
	for _, ba := range b.pending {
		n += len(ba.rows)
	}
	return n
}

type sysClock struct{}

func (sysClock) Now() time.Time { return time.Now() }
