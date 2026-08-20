// Package seq allocates monotonically increasing sequence numbers per project
// from Postgres `project_seq`. The allocator leases blocks of `blockSize` values
// in one atomic UPDATE (plan ring/seq §2.3.1):
//
//	UPDATE project_seq SET next = next + $1 WHERE project_id = $2
//	RETURNING (next - $1)::bigint
//
// and hands them out from memory until the block is exhausted, then leases the
// next. Because the lease is a single atomic statement, two allocator instances
// (two ucapi processes) never receive overlapping blocks — that is the two-
// instance guarantee (invariant 5/6, §3.6).
//
// On shutdown the unused tail of the current block is NOT returned. The v4 doc
// (§2.3.1) is explicit: holes in seq are acceptable because seq is an order
// marker, not a row counter, and returning a block races with concurrent
// leases. (The build plan's "return the tail on shutdown" wording conflicts;
// the v4 reasoning is architecturally correct — recorded in plan §9.)
package seq

import (
	"context"
	"errors"
	"sync"
)

// BlockLeaser leases a contiguous block of sequence values for a project. The
// real implementation wraps the sqlc-generated LeaseSeqBlock query; tests pass a
// fake that models the atomic UPDATE. The interface is int64 so the allocator is
// decoupled from sqlc's type inference and safe for high-volume tenants whose
// seq exceeds int32.
type BlockLeaser interface {
	LeaseSeqBlock(ctx context.Context, projectID, blockSize int64) (startSeq int64, err error)
}

// Allocator hands out per-project sequence numbers, leasing blocks as needed.
// It is safe for concurrent use: a mutex guards the in-memory cursor; the lease
// itself is atomic in Postgres.
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

// Next returns the next sequence number for the project, leasing a fresh block
// when the current one is exhausted. It blocks on the leaser only at block
// boundaries — the common path is an in-memory increment under the mutex.
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
	// Slow path: lease a new block. Done outside the lock so two concurrent
	// goroutines do not serialize on Postgres for unrelated projects — but we
	// re-check under the lock after, so only one lease wins.
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

// Remaining reports how many values the current block still holds. For tests and
// observability; not for production decisions.
func (a *Allocator) Remaining() int64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.end - a.next
}
