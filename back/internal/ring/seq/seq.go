// Package seq allocates monotonic per-project sequence numbers by leasing
// blocks from Postgres in one atomic UPDATE; the unused tail is never returned.
package seq

import (
	"context"
	"errors"
	"sync"
)

// BlockLeaser leases a contiguous block of sequence values; int64 so the
// allocator stays decoupled from sqlc's type inference.
type BlockLeaser interface {
	LeaseSeqBlock(ctx context.Context, projectID, blockSize int64) (startSeq int64, err error)
}

// Allocator hands out per-project sequence numbers, leasing blocks as needed;
// a mutex guards the in-memory cursor, the lease is atomic in Postgres.
type Allocator struct {
	projectID int64
	blockSize int64
	leaser    BlockLeaser

	mu   sync.Mutex
	next int64 // next seq to hand out (inclusive)
	end  int64 // end of current block (exclusive); next >= end means "lease again"
}

// New builds an Allocator that leases in blockSize increments for projectID.
func New(projectID, blockSize int64, leaser BlockLeaser) *Allocator {
	if blockSize <= 0 {
		blockSize = 10000
	}
	return &Allocator{projectID: projectID, blockSize: blockSize, leaser: leaser, end: 0}
}

// ErrNoLeaser is returned when Next is called on an Allocator whose leaser is nil
// (programmer error caught at the call site, not panic).
var ErrNoLeaser = errors.New("seq: block leaser is nil")

// Next returns the next sequence number, leasing a fresh block when the current
// one is exhausted; the common path is an in-memory increment under the mutex.
func (a *Allocator) Next(ctx context.Context) (int64, error) {
	if a.leaser == nil {
		return 0, ErrNoLeaser
	}
	a.mu.Lock()
	// Fast path: hand out the next value from the current block.
	if a.next < a.end {
		v := a.next
		a.next++
		a.mu.Unlock()
		return v, nil
	}
	a.mu.Unlock()
	// Slow path: lease outside the lock (no serializing on Postgres), then
	// re-check under the lock so only one lease wins.
	start, err := a.leaser.LeaseSeqBlock(ctx, a.projectID, a.blockSize)
	if err != nil {
		return 0, err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	// Another goroutine may have leased and advanced next while we waited on
	// the leaser; if so, serve from its block instead of clobbering.
	if a.next < a.end {
		v := a.next
		a.next++
		return v, nil
	}
	a.next = start
	a.end = start + a.blockSize
	v := a.next
	a.next++
	return v, nil
}
