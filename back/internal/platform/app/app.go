// Package app is the shared process bootstrap: load config, build logger,
// health and shutdown, install signal handlers, run Setup, tear down in budget.
package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"go.upcontrol.io/back/internal/platform/config"
	"go.upcontrol.io/back/internal/platform/health"
	"go.upcontrol.io/back/internal/platform/logging"
	"go.upcontrol.io/back/internal/platform/shutdown"
)

// Deps is everything Setup needs; tests substitute fakes at this one seam.
type Deps struct {
	Config   config.Config
	Logger   *slog.Logger
	Health   *health.Checker
	Shutdown *shutdown.Coordinator
}

// Setup wires a binary's components and may return a blocking run function
// (e.g. Serve); it must not block, long work goes in a registered goroutine.
type Setup func(ctx context.Context, d Deps) (run func() error, err error)

// Run is the process entrypoint. name is the binary name for logs.
func Run(name string, setup Setup) int {
	// Exit codes: 0 clean, 1 shutdown exceeded the budget, 2 never started.
	cfg, err := config.Load(name)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: config: %v\n", name, err)
		return 2
	}
	log := logging.New(logging.Options{Level: cfg.LogLevel, Format: cfg.LogFormat})
	log = log.With("svc", name)
	// Config warnings were collected before this logger existed; replay them
	// so a JSON shipper in prod can index them.
	for _, w := range cfg.Warnings {
		log.Warn("config: " + w)
	}
	h := health.New(5 * time.Second)
	sd := shutdown.New(log)

	d := Deps{Config: cfg, Logger: log, Health: h, Shutdown: sd}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Background health-probe loop: refresh cached probe results on a cadence
	// so /health is always a cheap cache read.
	probeCtx, probeCancel := context.WithCancel(context.Background())
	go runHealthProbes(probeCtx, h, 5*time.Second, 2*time.Second, log)
	sd.Register(shutdown.Task{Name: "health-probes", Stop: func(context.Context) error { probeCancel(); return nil }})

	run, err := setup(ctx, d)
	if err != nil {
		log.Error("startup failed", "err", err)
		return 2
	}

	// If Setup returned a blocking run (an HTTP server), drive it on the main
	// goroutine and shut it down on signal. Otherwise wait directly on ctx.
	serveErr := make(chan error, 1)
	if run != nil {
		go func() { serveErr <- run() }()
	}

	select {
	case <-ctx.Done():
		log.Info("shutting down", "reason", ctx.Err())
	case err := <-serveErr:
		log.Info("run returned", "err", err)
	}

	shutdownErr := sd.Shutdown(context.Background(), cfg.ShutdownTimeout)
	if shutdownErr != nil {
		log.Error("graceful shutdown exceeded budget — exiting non-zero", "err", shutdownErr)
		return 1
	}
	log.Info("shutdown complete")
	return 0
}

// runHealthProbes refreshes every registered probe until ctx is cancelled.
func runHealthProbes(ctx context.Context, h *health.Checker, every, probeTimeout time.Duration, log *slog.Logger) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			// Run swallows per-probe panics; a slow probe is bounded inside.
			func() {
				defer func() {
					if r := recover(); r != nil {
						log.Warn("health probe loop panicked", "err", fmt.Sprint(r))
					}
				}()
				h.Run(ctx, probeTimeout)
			}()
		}
	}
}

// ServeHTTP is a Setup helper: serve an http.Server, register its graceful
// Stop as a shutdown task, return the blocking Serve call.
func ServeHTTP(addr string, handler http.Handler, d Deps) (func() error, error) {
	srv := &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
	}
	d.Shutdown.Register(shutdown.Task{
		Name: "http:" + addr,
		Stop: func(ctx context.Context) error {
			err := srv.Shutdown(ctx)
			// Serve returns ErrServerClosed after Shutdown; treat that as clean.
			if err != nil && !errors.Is(err, http.ErrServerClosed) {
				return err
			}
			return nil
		},
	})
	return func() error {
		d.Logger.Info("http listening", "addr", addr)
		err := srv.ListenAndServe()
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}, nil
}
