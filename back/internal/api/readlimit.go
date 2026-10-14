package api

import (
	"context"
	"sync"
)

// readSlots bounds the board reads that run at once, process-wide, so a burst
// of heavy boards cannot take the connections the ingest flushes need: the pool
// holds 16 (storage/pg/pool.go) and one board read holds at most one of them at
// a time. A read waits for a slot; it is never refused for being busy, because a
// refusal would draw a failed board where a slower one was the honest answer.
// ponytail: one queue across every tenant; per-tenant fairness when the
// slow-read log shows one tenant's boards holding the rest back.
var readSlots = make(chan struct{}, 4)

// takeReadSlot waits for a slot or for ctx to end. The returned func gives the
// slot back, and is safe to call more than once and when ctx ended first: a
// caller defers it and also calls it the moment its reads end, so a client
// slow to take the body holds no slot while the answer is written.
func takeReadSlot(ctx context.Context) (release func(), err error) {
	select {
	case readSlots <- struct{}{}:
		return sync.OnceFunc(func() { <-readSlots }), nil
	case <-ctx.Done():
		return func() {}, ctx.Err()
	}
}
