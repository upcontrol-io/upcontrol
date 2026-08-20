package shutdown

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestShutdownAllFinish(t *testing.T) {
	c := New(nil)
	var mu sync.Mutex
	ran := make(map[string]bool)
	c.Register(Task{"a", func(context.Context) error { mu.Lock(); ran["a"] = true; mu.Unlock(); return nil }})
	c.Register(Task{"b", func(context.Context) error { mu.Lock(); ran["b"] = true; mu.Unlock(); return nil }})
	if err := c.Shutdown(context.Background(), time.Second); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	if !ran["a"] || !ran["b"] {
		t.Errorf("ran = %v, want both tasks", ran)
	}
}

func TestShutdownUnfinishedReported(t *testing.T) {
	c := New(nil)
	// `slow` blocks longer than the budget; it must be reported unfinished.
	c.Register(Task{"slow", func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	}})
	c.Register(Task{"quick", func(context.Context) error { return nil }})
	err := c.Shutdown(context.Background(), 30*time.Millisecond)
	if err == nil {
		t.Fatal("expected unfinished error, got nil")
	}
	if !strings.Contains(err.Error(), "slow") {
		t.Errorf("error should name the slow task: %v", err)
	}
	if strings.Contains(err.Error(), "quick") {
		t.Errorf("quick task should not be flagged unfinished: %v", err)
	}
}

func TestShutdownTaskErrorLogged(t *testing.T) {
	c := New(nil)
	c.Register(Task{"boom", func(context.Context) error { return errors.New("nope") }})
	// A task that returns an error but finishes in time is NOT unfinished; the
	// error is logged, Shutdown returns nil.
	if err := c.Shutdown(context.Background(), time.Second); err != nil {
		t.Errorf("erroring-but-finished task should not fail shutdown: %v", err)
	}
}
