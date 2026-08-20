// Package health exposes a liveness/readiness endpoint. A process is "live" if
// the event loop is running; "ready" if every registered dependency reports OK.
// The checker does NOT run expensive probes on every /health hit — each check
// caches its result with a TTL and is re-run by a background goroutine, so a
// health hit under load is a map lookup, not a DB round trip.
package health

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"time"
)

// Check is a single named dependency probe (e.g. "postgres", "clickhouse"). It
// must be cheap and time-bounded by the caller.
type Check func(ctx context.Context) error

// Status is the per-check outcome.
type Status string

const (
	StatusOK   Status = "ok"
	StatusDown Status = "down"
)

// Result is what /health renders.
type Result struct {
	Status Status            `json:"status"`
	Checks map[string]Status `json:"checks"`
}

// Checker registers dependency probes and serves a combined Result. The zero
// value is a checker with no probes (always OK), suitable for a skeleton boot.
type Checker struct {
	ttl    time.Duration
	mu     sync.RWMutex
	checks map[string]Check
	cache  map[string]cacheEntry
	clock  func() time.Time
}

type cacheEntry struct {
	at  time.Time
	err error
}

// New builds a Checker whose cached probe results live for ttl.
func New(ttl time.Duration) *Checker {
	if ttl <= 0 {
		ttl = 5 * time.Second
	}
	return &Checker{
		ttl:    ttl,
		checks: map[string]Check{},
		cache:  map[string]cacheEntry{},
		clock:  time.Now,
	}
}

// Register adds (or replaces) a named probe. Probes are run by Run; /health
// reads their cached results.
func (c *Checker) Register(name string, fn Check) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.checks[name] = fn
}

// Run refreshes every probe's cached result. It is the background loop the
// process drives; each probe gets its own short timeout so one slow dependency
// cannot starve the rest.
func (c *Checker) Run(ctx context.Context, probeTimeout time.Duration) {
	c.mu.RLock()
	names := make([]string, 0, len(c.checks))
	for name := range c.checks {
		names = append(names, name)
	}
	c.mu.RUnlock()

	for _, name := range names {
		c.mu.RLock()
		fn := c.checks[name]
		c.mu.RUnlock()
		if fn == nil {
			continue
		}
		err := c.runOne(ctx, fn, probeTimeout)
		c.mu.Lock()
		c.cache[name] = cacheEntry{at: c.clock(), err: err}
		c.mu.Unlock()
	}
}

func (c *Checker) runOne(ctx context.Context, fn Check, timeout time.Duration) error {
	if timeout <= 0 {
		timeout = 2 * time.Second
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				done <- errPanic{r}
			}
		}()
		done <- fn(cctx)
	}()
	select {
	case err := <-done:
		return err
	case <-cctx.Done():
		return cctx.Err()
	}
}

// Snapshot returns the current cached result. Stale entries (older than ttl)
// count as OK to avoid flapping when the background loop is briefly delayed;
// a genuine outage refreshes them on the next tick.
func (c *Checker) Snapshot() Result {
	c.mu.RLock()
	defer c.mu.RUnlock()
	now := c.clock()
	out := Result{Status: StatusOK, Checks: make(map[string]Status, len(c.cache))}
	for name, e := range c.cache {
		st := StatusOK
		if e.err != nil {
			st = StatusDown
		}
		out.Checks[name] = st
		if e.err != nil && now.Sub(e.at) <= c.ttl {
			out.Status = StatusDown
		}
	}
	return out
}

// Handler returns an http.Handler writing the Snapshot as JSON. It is 200 when
// overall OK, 503 when any check is down — the contract a load balancer wants.
func (c *Checker) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		res := c.Snapshot()
		code := http.StatusOK
		if res.Status == StatusDown {
			code = http.StatusServiceUnavailable
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(res)
	})
}

type errPanic struct{ v any }

func (e errPanic) Error() string { return "health check panicked" }
