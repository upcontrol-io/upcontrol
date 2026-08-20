// Package pg is the Postgres storage layer: the api_key resolver (ingest auth),
// the seq-block leaser adapter for ring/seq, and the ingest-batch idempotency
// store. It wraps a pgxpool and the sqlc-generated Queries. Every query is
// tenant-scoped by construction (the key's tenant_id), and invariant 3 holds at
// the SQL level.
package pg

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	sqlc "go.upcontrol.io/back/gen/pg"
)

// Pool is a thin wrapper over pgxpool.Pool, exposing the sqlc Queries and the
// raw pool for the few things sqlc does not cover (advisory locks, COPY).
type Pool struct {
	db *pgxpool.Pool
	q  *sqlc.Queries
}

// Open builds a pgxpool against databaseURL and pings. A failure at startup is
// fatal (the process has nothing to serve).
func Open(ctx context.Context, databaseURL string) (*Pool, error) {
	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("pg: parse %q: %w", databaseURL, err)
	}
	cfg.MinConns = 2
	cfg.MaxConns = 16
	db, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("pg: connect: %w", err)
	}
	if err := db.Ping(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("pg: ping: %w", err)
	}
	return &Pool{db: db, q: sqlc.New(db)}, nil
}

// Ping is the health probe (registered with platform/health as "postgres").
func (p *Pool) Ping(ctx context.Context) error { return p.db.Ping(ctx) }

// Close releases the pool.
func (p *Pool) Close() { p.db.Close() }

// Queries exposes the sqlc-generated query interface. Callers reach the pool's
// raw Conn via the generated db.go's WithTx; for advisory locks use Exec below.
func (p *Pool) Queries() *sqlc.Queries { return p.q }

// Exec runs a raw SQL statement (for advisory locks and other non-query SQL).
func (p *Pool) Exec(ctx context.Context, sql string, args ...any) error {
	_, err := p.db.Exec(ctx, sql, args...)
	return err
}

// Raw exposes the underlying pool for code that needs direct pgx access.
func (p *Pool) Raw() *pgxpool.Pool { return p.db }
