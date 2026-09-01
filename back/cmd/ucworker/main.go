// Command ucworker is the background driver: delivery dispatch, purge jobs,
// compaction. Advisory locks make it N-replica safe.
package main

import (
	"context"
	"net/http"
	"os"

	"go.upcontrol.io/back/internal/platform/app"
	"go.upcontrol.io/back/internal/platform/shutdown"
	"go.upcontrol.io/back/internal/storage/pg"
	"go.upcontrol.io/back/internal/worker"
)

func main() {
	os.Exit(app.Run("ucworker", setup))
}

func setup(ctx context.Context, d app.Deps) (func() error, error) {
	mux := http.NewServeMux()
	mux.Handle("GET /health", d.Health.Handler())

	if d.Config.PostgresURL != "" {
		pool, err := pg.Open(ctx, d.Config.PostgresURL)
		if err == nil {
			// The pool is owned here: the jobs take it already open, so ucapi
			// can hand them its own without a second connection.
			d.Health.Register("postgres", pool.Ping)
			d.Shutdown.Register(shutdown.Task{Name: "pg-pool", Stop: func(context.Context) error { pool.Close(); return nil }})
			err = worker.Start(ctx, d, pool)
		}
		if err != nil {
			d.Logger.Error("ucworker wiring failed; serving /health only", "err", err)
		}
	}

	return app.ServeHTTP(d.Config.HTTPAddr, mux, d)
}
