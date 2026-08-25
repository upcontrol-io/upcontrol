package seq

import (
	"context"
	"errors"
	"sync"
	"testing"
)

// fakeStore models the atomic Postgres UPDATE: each LeaseSeqBlock advances
// `next` by blockSize under the mutex; two allocators see disjoint blocks.
type fakeStore struct {
	mu   sync.Mutex
	next int64
}

func (f *fakeStore) LeaseSeqBlock(_ context.Context, _, blockSize int64) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	start := f.next
	f.next += blockSize
	return start, nil
}

func TestAllocatorSequential(t *testing.T) {
	a := New(1, 100, &fakeStore{})
	var got []int64
	for i := 0; i < 250; i++ {
		v, err := a.Next(context.Background())
		if err != nil {
			t.Fatalf("Next %d: %v", i, err)
		}
		got = append(got, v)
	}
	// 250 values across blocks of 100: 0..249, contiguous (within a single
	// allocator there are no holes — the block is consumed in order).
	for i, v := range got {
		if v != int64(i) {
			t.Fatalf("seq[%d] = %d, want %d", i, v, i)
		}
	}
	if a.Remaining() != 50 {
		t.Errorf("Remaining = %d, want 50 (250 used of 300 leased)", a.Remaining())
	}
}

func TestAllocatorLeasesAtBoundary(t *testing.T) {
	store := &fakeStore{}
	a := New(1, 10, store)
	for i := 0; i < 10; i++ {
		if _, err := a.Next(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	// 10 used: the first block [0,10) is exhausted, Remaining must be 0 before
	// the next call leases again.
	if a.Remaining() != 0 {
		t.Errorf("Remaining = %d, want 0 at block boundary", a.Remaining())
	}
	v, err := a.Next(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if v != 10 {
		t.Errorf("first value of next block = %d, want 10", v)
	}
	// store advanced by two blocks of 10.
	store.mu.Lock()
	got := store.next
	store.mu.Unlock()
	if got != 20 {
		t.Errorf("store.next = %d, want 20", got)
	}
}

// Two allocator instances (two ucapi processes) sharing one project_seq must
// NEVER hand out the same seq.
func TestTwoInstancesNoOverlap(t *testing.T) {
	const blockSize = 1000
	const perInstance = 5000
	store := &fakeStore{}
	a := New(42, blockSize, store)
	b := New(42, blockSize, store) // same project, "second process"

	var wg sync.WaitGroup
	var mu sync.Mutex
	fromA := make(map[int64]bool, perInstance)
	fromB := make(map[int64]bool, perInstance)

	alloc := func(alloc *Allocator, into *map[int64]bool) {
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
			if (*into)[v] {
				t.Errorf("duplicate within one instance: %d", v)
			}
			(*into)[v] = true
		}
		mu.Unlock()
	}

	wg.Add(2)
	go alloc(a, &fromA)
	go alloc(b, &fromB)
	wg.Wait()

	// The intersection must be empty: no seq handed out by both instances.
	overlap := 0
	for v := range fromA {
		if fromB[v] {
			overlap++
		}
	}
	if overlap != 0 {
		t.Fatalf("two instances overlapped on %d sequence values (invariant 5/6)", overlap)
	}

	// 10000 values issued total across the two instances.
	if len(fromA)+len(fromB) != 2*perInstance {
		t.Errorf("issued %d, want %d", len(fromA)+len(fromB), 2*perInstance)
	}
}

func TestNilLeaserErrors(t *testing.T) {
	a := New(1, 100, nil)
	if _, err := a.Next(context.Background()); !errors.Is(err, ErrNoLeaser) {
		t.Errorf("err = %v, want ErrNoLeaser", err)
	}
}
