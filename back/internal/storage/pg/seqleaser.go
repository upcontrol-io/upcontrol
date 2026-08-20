// ring/seq adapter — bridges the sqlc-generated LeaseSeqBlock query to the
// ring/seq.BlockLeaser interface so the per-project Allocator can lease blocks.

package pg

import (
	"context"

	"go.upcontrol.io/back/gen/pg"
)

// SeqLeaser adapts Pool to ring/seq.BlockLeaser.
type SeqLeaser struct {
	pool *Pool
}

// NewSeqLeaser builds the adapter.
func NewSeqLeaser(p *Pool) *SeqLeaser { return &SeqLeaser{pool: p} }

// LeaseSeqBlock runs the atomic UPDATE that reserves [start, start+blockSize).
// Two SeqLeaser instances on the same Pool (two ucapi processes) get disjoint
// blocks — that is the §3.6 guarantee, enforced by the Postgres row lock.
func (s *SeqLeaser) LeaseSeqBlock(ctx context.Context, projectID, blockSize int64) (int64, error) {
	return s.pool.Queries().LeaseSeqBlock(ctx, pg.LeaseSeqBlockParams{
		BlockSize: blockSize,
		ProjectID: projectID,
	})
}
