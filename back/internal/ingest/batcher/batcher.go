// Package batcher accumulates decoded rows and flushes them to ClickHouse in
// batches; the 1/sec-per-key flush floor caps part count (one part per INSERT).
package batcher

import (
	"context"
	"sync"
	"time"
)

// Sink receives a flushed batch for one key (table×bucket).
type Sink interface {
	Flush(ctx context.Context, key string, rows [][]byte) error
}

// Clock is the minimal time surface the batcher needs; tests pass a fake.
type Clock interface {
	Now() time.Time
}

// Options configures the flush policy; zero-value fields fall back to the
// 8 MiB / 200 ms / 1-per-second defaults.
type Options struct {
	BatchBytes    int
	BatchAge      time.Duration
	MinInterval   time.Duration           // 1/sec-per-key floor; 0 means BatchAge alone
	FlushCallback func(key string, n int) // optional, for tests/metrics
}

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

// New builds a Batcher; clk nil means the real clock.
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

// Add appends a row under key, flushing at once if the batch crosses
// BatchBytes (the size threshold ignores MinInterval).
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

// Tick flushes batches older than BatchAge whose key is past MinInterval;
// callers drive it on a short ticker.
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

	// Swap-and-flush: take the batch out and replace it, so a row added between
	// the snapshot and the flush is never lost or double-flushed.
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

// Pending reports the total pending rows across all keys.
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
