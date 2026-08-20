// ingest-batch idempotency — implements ingest.Idempotency over ingest_batch so
// a replay of the same body returns the first accept's count and never
// double-writes (plan §3.8).

package pg

import (
	"context"

	sqlc "go.upcontrol.io/back/gen/pg"
)

// Idempotency implements ingest.Idempotency over the ingest_batch table.
type Idempotency struct {
	pool *Pool
}

// NewIdempotency builds the store.
func NewIdempotency(p *Pool) *Idempotency { return &Idempotency{pool: p} }

// Claim inserts the batch_key (sha256 of the body) unless it already exists. On
// a replay it returns replay=true and the count stored on the first accept.
func (i *Idempotency) Claim(ctx context.Context, batchKey string, bodyHash []byte, accepted int) (bool, int, error) {
	row, err := i.pool.Queries().ClaimIngestBatch(ctx, sqlc.ClaimIngestBatchParams{
		BatchKey: batchKey,
		BodyHash: bodyHash,
		Accepted: int32(accepted),
	})
	if err != nil {
		return false, 0, err
	}
	return !row.Inserted, int(row.Accepted), nil
}
