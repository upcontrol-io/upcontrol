package health

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestCheckerOK(t *testing.T) {
	c := New(time.Second)
	c.Register("pg", func(context.Context) error { return nil })
	c.Register("ch", func(context.Context) error { return nil })
	c.Run(context.Background(), time.Second)
	got := c.Snapshot()
	if got.Status != StatusOK {
		t.Fatalf("status = %v, want ok", got.Status)
	}
	if got.Checks["pg"] != StatusOK || got.Checks["ch"] != StatusOK {
		t.Errorf("checks = %v", got.Checks)
	}
}

func TestCheckerDown(t *testing.T) {
	c := New(time.Second)
	c.Register("pg", func(context.Context) error { return errors.New("nope") })
	c.Run(context.Background(), time.Second)
	got := c.Snapshot()
	if got.Status != StatusDown {
		t.Fatalf("status = %v, want down", got.Status)
	}
	if got.Checks["pg"] != StatusDown {
		t.Errorf("pg = %v, want down", got.Checks["pg"])
	}
}

func TestCheckerPanicIsDown(t *testing.T) {
	c := New(time.Second)
	c.Register("flaky", func(context.Context) error { panic("kaboom") })
	c.Run(context.Background(), time.Second)
	got := c.Snapshot()
	if got.Checks["flaky"] != StatusDown {
		t.Errorf("panicking check should be down: %v", got.Checks)
	}
}

func TestCheckerHandlerCodes(t *testing.T) {
	c := New(time.Second)
	c.Register("pg", func(context.Context) error { return nil })
	c.Run(context.Background(), time.Second)
	rr := httptest.NewRecorder()
	c.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/health", nil))
	if rr.Code != http.StatusOK {
		t.Errorf("ok -> %d, want 200", rr.Code)
	}

	c2 := New(time.Second)
	c2.Register("pg", func(context.Context) error { return errors.New("down") })
	c2.Run(context.Background(), time.Second)
	rr2 := httptest.NewRecorder()
	c2.Handler().ServeHTTP(rr2, httptest.NewRequest(http.MethodGet, "/health", nil))
	if rr2.Code != http.StatusServiceUnavailable {
		t.Errorf("down -> %d, want 503", rr2.Code)
	}
}

func TestCheckerSlowProbeTimesOut(t *testing.T) {
	c := New(time.Second)
	c.Register("slow", func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	})
	// probeTimeout of 20ms forces the slow probe to report its context error.
	c.Run(context.Background(), 20*time.Millisecond)
	got := c.Snapshot()
	if got.Checks["slow"] != StatusDown {
		t.Errorf("slow probe should be down after timeout: %v", got.Checks)
	}
}
