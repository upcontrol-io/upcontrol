package api

import (
	"context"
	"testing"
	"time"
)

func TestTakeReadSlotQueuesPastTheCap(t *testing.T) {
	ctx := context.Background()
	var releases []func()
	for range cap(readSlots) {
		release, err := takeReadSlot(ctx)
		if err != nil {
			t.Fatalf("slot under the cap refused: %v", err)
		}
		releases = append(releases, release)
	}

	// Past the cap a read waits, and ends with its context rather than failing early.
	short, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
	defer cancel()
	release, err := takeReadSlot(short)
	if err == nil {
		t.Fatal("a fifth read got a slot while four were held")
	}
	release() // a no-op on the refused path, and must not panic

	// Giving one back twice frees that one slot only, and lets the next read in.
	releases[0]()
	releases[0]()
	if held := len(readSlots); held != cap(readSlots)-1 {
		t.Fatalf("a release called twice left %d of %d slots held, want %d", held, cap(readSlots), cap(readSlots)-1)
	}
	waited, err := takeReadSlot(ctx)
	if err != nil {
		t.Fatalf("a freed slot was not handed on: %v", err)
	}
	waited()
	for _, r := range releases[1:] {
		r()
	}
	if len(readSlots) != 0 {
		t.Fatalf("slots leaked: %d still held", len(readSlots))
	}
}
