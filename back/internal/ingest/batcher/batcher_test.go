package batcher

import (
	"bytes"
	"context"
	"sync"
	"testing"
	"time"
)

// fakeSink records every flush's row count (a proxy for ClickHouse parts).
type fakeSink struct {
	mu      sync.Mutex
	flushes map[string][]int // key -> per-flush row counts
}

func newFakeSink() *fakeSink { return &fakeSink{flushes: map[string][]int{}} }

func (s *fakeSink) Flush(_ context.Context, key string, rows [][]byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.flushes[key] = append(s.flushes[key], len(rows))
	return nil
}

func (s *fakeSink) count(key string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.flushes[key])
}

func (s *fakeSink) rows() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, counts := range s.flushes {
		for _, c := range counts {
			n += c
		}
	}
	return n
}

type fakeClock struct{ t time.Time }

func (f *fakeClock) Now() time.Time      { return f.t }
func (f *fakeClock) Add(d time.Duration) { f.t = f.t.Add(d) }

// TestSizeThresholdFlushesImmediately: crossing BatchBytes flushes at once,
// ignoring MinInterval (a full batch cannot wait).
func TestSizeThresholdFlushesImmediately(t *testing.T) {
	sink := newFakeSink()
	clk := &fakeClock{t: time.Unix(1000, 0)}
	b := New(sink, clk, Options{BatchBytes: 16, BatchAge: time.Hour, MinInterval: time.Hour})
	row := bytes.Repeat([]byte("x"), 8)
	// Two 8-byte rows cross the 16-byte threshold.
	b.Add(context.Background(), "logs", row)
	if sink.count("logs") != 0 {
		t.Fatal("first row should not flush")
	}
	b.Add(context.Background(), "logs", row)
	if sink.count("logs") != 1 {
		t.Fatalf("expected 1 size-flush, got %d", sink.count("logs"))
	}
	if sink.rows() != 2 {
		t.Errorf("flushed %d rows, want 2", sink.rows())
	}
}

// TestGate1RowPerSecIs1PartPerSec is the §3.7 gate: feeding one row per second
// and Ticking must produce at most one flush per second per key — i.e. ≤ elapsed
// seconds of flushes (parts), not one flush per row.
func TestGate1RowPerSecIs1PartPerSec(t *testing.T) {
	sink := newFakeSink()
	clk := &fakeClock{t: time.Unix(0, 0)}
	// Tight age so every Tick ages out; MinInterval 1s is the floor under test.
	b := New(sink, clk, Options{BatchBytes: 1 << 20, BatchAge: 50 * time.Millisecond, MinInterval: 1 * time.Second})

	const seconds = 10
	for s := 0; s < seconds; s++ {
		b.Add(context.Background(), "logs", []byte("row"))
		// Advance the clock one second and Tick a few times within that second —
		// MinInterval must collapse those Ticks into a single flush.
		clk.Add(1 * time.Second)
		for i := 0; i < 4; i++ {
			b.Tick(context.Background())
			clk.Add(200 * time.Millisecond)
		}
	}
	flushes := sink.count("logs")
	// At most one flush per second per key (allow exactly seconds; a couple of
	// missed ticks could make it less, but never more).
	if flushes > seconds {
		t.Fatalf("§3.7 gate: %d flushes for 1 row/sec over %ds, want <= %d (one part/sec)", flushes, seconds, seconds)
	}
	if sink.rows() != seconds {
		t.Errorf("total rows flushed = %d, want %d (no rows lost)", sink.rows(), seconds)
	}
}

// TestCloseFlushesEverything: Close drains pending rows regardless of thresholds.
func TestCloseFlushesEverything(t *testing.T) {
	sink := newFakeSink()
	clk := &fakeClock{t: time.Unix(0, 0)}
	b := New(sink, clk, Options{BatchBytes: 1 << 30, BatchAge: time.Hour, MinInterval: time.Hour})
	for i := 0; i < 5; i++ {
		b.Add(context.Background(), "logs", []byte("r"))
	}
	if sink.count("logs") != 0 {
		t.Fatal("nothing should flush before Close")
	}
	if err := b.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if sink.count("logs") != 1 || sink.rows() != 5 {
		t.Errorf("Close flushed %d/%d, want 1 flush of 5 rows", sink.count("logs"), sink.rows())
	}
}

// TestMetricRowsReachTheirOwnKey: metric envelopes ride the same batcher under
// the "metrics" key, so they land in the metrics table (the sink routes by key)
// and never mix into a logs part.
func TestMetricRowsReachTheirOwnKey(t *testing.T) {
	sink := newFakeSink()
	clk := &fakeClock{t: time.Unix(0, 0)}
	b := New(sink, clk, Options{BatchBytes: 1 << 30, BatchAge: 10 * time.Millisecond, MinInterval: time.Second})
	b.Add(context.Background(), "logs", []byte(`{"message":"hi","level":"info"}`))
	b.Add(context.Background(), "metrics", []byte(`{"name":"signups","value":31}`))
	clk.Add(2 * time.Second)
	if err := b.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if sink.count("logs") != 1 || sink.count("metrics") != 1 {
		t.Fatalf("logs=%d metrics=%d, want each flushed once under its own key",
			sink.count("logs"), sink.count("metrics"))
	}
}

// TestKeysAreIndependent: the 1/sec floor is per key, so two keys each flush.
func TestKeysAreIndependent(t *testing.T) {
	sink := newFakeSink()
	clk := &fakeClock{t: time.Unix(0, 0)}
	b := New(sink, clk, Options{BatchBytes: 1 << 20, BatchAge: 10 * time.Millisecond, MinInterval: 1 * time.Second})
	b.Add(context.Background(), "logs", []byte("a"))
	b.Add(context.Background(), "events", []byte("b"))
	clk.Add(2 * time.Second)
	b.Tick(context.Background())
	if sink.count("logs") != 1 || sink.count("events") != 1 {
		t.Errorf("logs=%d events=%d, want each flushed once", sink.count("logs"), sink.count("events"))
	}
}
