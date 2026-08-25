// Package shutdown orchestrates graceful teardown: tasks run concurrently
// under a shared deadline; a missed deadline exits non-zero.
package shutdown

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"time"
)

// Task is one teardown step: a name (for logging) and a stop function; the
// Coordinator imposes no per-task timeout, the shared deadline is the budget.
type Task struct {
	Name string
	Stop func(ctx context.Context) error
}

// Coordinator runs registered Tasks against a shared deadline.
type Coordinator struct {
	mu     sync.Mutex
	tasks  []Task
	logger *slog.Logger
}

// New builds a Coordinator that logs unfinished tasks to logger.
func New(logger *slog.Logger) *Coordinator {
	if logger == nil {
		logger = slog.Default()
	}
	return &Coordinator{logger: logger}
}

// Register appends a teardown task. Registering after Shutdown has begun is a
// no-op (the run has already snapshotted the list).
func (c *Coordinator) Register(t Task) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.tasks = append(c.tasks, t)
}

// Shutdown runs every registered task concurrently, bounded by timeout; a
// non-nil return lists the unfinished task names and maps to a non-zero exit.
func (c *Coordinator) Shutdown(ctx context.Context, timeout time.Duration) error {
	c.mu.Lock()
	tasks := make([]Task, len(c.tasks))
	copy(tasks, c.tasks)
	c.mu.Unlock()

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var wg sync.WaitGroup
	type result struct {
		name string
		err  error
	}
	results := make(chan result, len(tasks))

	for _, t := range tasks {
		wg.Add(1)
		go func(t Task) {
			defer wg.Done()
			err := t.Stop(ctx)
			results <- result{t.Name, err}
			if err != nil {
				c.logger.Warn("shutdown task returned error", "task", t.Name, "err", err)
			}
		}(t)
	}

	// Wait for all tasks to finish or the deadline; `done` closes when every
	// task reported, then drain stragglers that wrote after the timeout.
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	finishedNames := map[string]bool{}

	// drain collects results that have already arrived without blocking. A task
	// that has not reported by the time we snapshot is, by definition, unfinished.
	drain := func() {
		for {
			select {
			case r := <-results:
				finishedNames[r.name] = true
			default:
				return
			}
		}
	}

	select {
	case <-done:
		// All finished; every task sent exactly one result into the buffered
		// channel, so a single drain captures them all.
		drain()
	case <-ctx.Done():
		// Deadline hit. Snapshot who finished; the rest are abandoned and named.
		drain()
	}

	var unfinished []string
	for _, t := range tasks {
		if !finishedNames[t.Name] {
			unfinished = append(unfinished, t.Name)
		}
	}

	if len(unfinished) > 0 {
		return errors.New("shutdown: tasks did not finish within deadline: " + strings.Join(unfinished, ", "))
	}
	return nil
}
