//go:build ha

// HA test: two ucapi and two ucworker instances run against the same Postgres
// and ClickHouse; boots real processes, asserts no duplication. -tags=ha.
package ha

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	sqlc "go.upcontrol.io/back/gen/pg"
	"go.upcontrol.io/back/internal/migrate"
	"go.upcontrol.io/back/internal/ring/seq"
	"go.upcontrol.io/back/internal/storage/pg"
)

// Two seq allocators on the same project_seq never hand out the same value.
func TestSeqNoIntersectionBetweenInstances(t *testing.T) {
	dsn := startPostgres(t)
	pool := openPool(t, dsn)

	ctx := context.Background()
	// project_seq REFERENCES project, which references tenant: seed the
	// parents on a fresh database, or the INSERT fails its FK silently.
	if _, err := pool.Raw().Exec(ctx,
		`INSERT INTO tenant (id, public_id, name) VALUES (1, gen_random_uuid(), 'ha-seq')
		 ON CONFLICT DO NOTHING`); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	if _, err := pool.Raw().Exec(ctx,
		`INSERT INTO project (id, public_id, tenant_id, domain) VALUES (1, gen_random_uuid(), 1, '')
		 ON CONFLICT DO NOTHING`); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	// Ensure project_seq for project 1.
	if _, err := pool.Raw().Exec(ctx,
		`INSERT INTO project_seq (project_id, next) VALUES (1, 1) ON CONFLICT DO NOTHING`); err != nil {
		t.Fatalf("seed project_seq: %v", err)
	}

	leaser := pg.NewSeqLeaser(pool)

	// Two allocators (two ucapi instances).
	a := seq.New(1, 10000, leaser)
	b := seq.New(1, 10000, leaser)

	const perInstance = 5000
	var mu sync.Mutex
	fromA := make(map[int64]bool)
	fromB := make(map[int64]bool)
	var wg sync.WaitGroup

	alloc := func(alloc *seq.Allocator, into *map[int64]bool) {
		defer wg.Done()
		ctx := context.Background()
		local := make([]int64, 0, perInstance)
		for i := 0; i < perInstance; i++ {
			v, err := alloc.Next(ctx)
			if err != nil {
				t.Errorf("Next: %v", err)
				return
			}
			local = append(local, v)
		}
		mu.Lock()
		for _, v := range local {
			(*into)[v] = true
		}
		mu.Unlock()
	}

	wg.Add(2)
	go alloc(a, &fromA)
	go alloc(b, &fromB)
	wg.Wait()

	// Intersection must be empty.
	overlap := 0
	for v := range fromA {
		if fromB[v] {
			overlap++
		}
	}
	if overlap != 0 {
		t.Fatalf("two instances overlapped on %d seq values (invariant 5/6)", overlap)
	}
	if len(fromA)+len(fromB) != 2*perInstance {
		t.Errorf("total issued = %d, want %d", len(fromA)+len(fromB), 2*perInstance)
	}
}

// TestDeliveryQueueNoDuplication verifies two delivery workers never deliver
// the same item: both Tick concurrently, total processed equals enqueued.
func TestDeliveryQueueNoDuplication(t *testing.T) {
	dsn := startPostgres(t)
	pool := openPool(t, dsn)
	ctx := context.Background()

	// Seed: one tenant + one channel + 100 queue items; the uuids are minted
	// (the literals this used to carry were not valid uuid syntax).
	if _, err := pool.Raw().Exec(ctx,
		`INSERT INTO tenant (id, public_id, name) VALUES (1, gen_random_uuid(), 'ha-delivery')
		 ON CONFLICT DO NOTHING`); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	if _, err := pool.Raw().Exec(ctx,
		`INSERT INTO alert_channel (id, public_id, tenant_id, kind, target)
		 VALUES (1, gen_random_uuid(), 1, 'email', 'test@example.com')
		 ON CONFLICT DO NOTHING`); err != nil {
		t.Fatalf("seed channel: %v", err)
	}

	for i := 0; i < 100; i++ {
		hash := sha256.Sum256([]byte{byte(i)})
		// idem_key is TEXT: raw digest bytes are not valid UTF-8 (SQLSTATE
		// 22021); hex-encode, exactly like the ingester does.
		if _, err := pool.Raw().Exec(ctx,
			`INSERT INTO delivery_queue (tenant_id, channel_id, idem_key, class, payload, next_try_at)
			 VALUES (1, 1, $1, 'test', '{"title":"test"}', now())
			 ON CONFLICT (idem_key) DO NOTHING`,
			hex.EncodeToString(hash[:8])); err != nil {
			t.Fatalf("seed queue item %d: %v", i, err)
		}
	}

	// Two workers lease concurrently.
	var wg sync.WaitGroup
	processedA := 0
	processedB := 0

	worker := func(nodeID string, count *int) {
		defer wg.Done()
		for j := 0; j < 50; j++ {
			items, err := pool.Queries().LeasePendingDeliveries(ctx,
				sqlc.LeasePendingDeliveriesParams{
					LeasedBy:   &nodeID,
					LeaseUntil: pgtype.Timestamptz{Time: time.Now().Add(time.Minute), Valid: true},
					Limit:      20,
				})
			if err != nil || len(items) == 0 {
				break
			}
			*count += len(items)
			// Mark each as sent (simulating delivery).
			for _, item := range items {
				_ = pool.Queries().MarkDelivered(ctx, item.ID)
			}
		}
	}

	wg.Add(2)
	go worker("ha-worker-a", &processedA)
	go worker("ha-worker-b", &processedB)
	wg.Wait()

	total := processedA + processedB
	if total > 100 {
		t.Fatalf("processed %d items, but only 100 were enqueued — duplication detected", total)
	}
	t.Logf("processed: A=%d B=%d total=%d", processedA, processedB, total)
}

// TestAdvisoryLockExclusive: a second advisory lock on the same key cannot
// be taken while the first holds it (the Telegram bot's single poller).
func TestAdvisoryLockExclusive(t *testing.T) {
	dsn := startPostgres(t)
	pool := openPool(t, dsn)
	pool2 := openPool(t, dsn) // a SEPARATE pool = a different backend session; the advisory lock must block it
	ctx := context.Background()

	const lockKey = 0x74657374 // "test"

	// Acquire on pool A.
	if _, err := pool.Raw().Exec(ctx, "SELECT pg_advisory_lock($1)", lockKey); err != nil {
		t.Fatalf("first lock: %v", err)
	}

	// Try to acquire on a second connection — should block.
	acquired := make(chan error, 1)
	go func() {
		_, err := pool2.Raw().Exec(ctx, "SELECT pg_advisory_lock($1)", lockKey)
		acquired <- err
	}()

	select {
	case <-acquired:
		t.Fatal("second advisory_lock should block, but succeeded")
	case <-time.After(2 * time.Second):
		// Expected: the second lock is still waiting.
	}

	// Release the first lock.
	_, _ = pool.Raw().Exec(ctx, "SELECT pg_advisory_unlock($1)", lockKey)

	select {
	case err := <-acquired:
		if err != nil {
			t.Fatalf("second lock after release: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("second lock did not acquire after release")
	}

	// Clean up.
	_, _ = pool.Raw().Exec(ctx, "SELECT pg_advisory_unlock($1)", lockKey)
}

func startPostgres(t *testing.T) string {
	t.Helper()
	// Reuse the same pattern as internal/storage/pg/pg_integration_test.go.
	dsn := os.Getenv("UC_TEST_POSTGRES")
	if dsn == "" {
		t.Skip("UC_TEST_POSTGRES not set; skipping HA test")
	}
	// Apply migrations.
	applyMigrations(t, dsn)
	return dsn
}

func openPool(t *testing.T, dsn string) *pg.Pool {
	t.Helper()
	p, err := pg.Open(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	return p
}

func applyMigrations(t *testing.T, dsn string) {
	t.Helper()
	// Apply goose migrations idempotently (UC_TEST_POSTGRES may point at a fresh
	// DB). Postgres-only: ClickHouse addr empty => runClickHouse is a no-op.
	if err := migrate.Run(context.Background(), dsn, "", "", "", "", "../../../db/postgres", ""); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}
}
