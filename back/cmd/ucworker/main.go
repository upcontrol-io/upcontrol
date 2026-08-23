// Command ucworker is the background driver: ring/cutoff recomputation,
// delivery dispatch (when ucapi's inline worker is overloaded), purge jobs
// (expired batches, stale sessions, old incidents past plan retention), and
// the night-only ClickHouse compaction safety net. Every job takes a
// pg_advisory_lock so two ucworker instances never duplicate work (invariant
// 5/6). That is what lets the fleet run N replicas safely.
package main

import (
	"context"
	"math/big"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	sqlc "go.upcontrol.io/back/gen/pg"
	"go.upcontrol.io/back/internal/deliver"
	"go.upcontrol.io/back/internal/detect"
	"go.upcontrol.io/back/internal/detect/errorlog"
	"go.upcontrol.io/back/internal/incident"
	"go.upcontrol.io/back/internal/notify/mailer"
	"go.upcontrol.io/back/internal/platform/app"
	"go.upcontrol.io/back/internal/platform/config"
	"go.upcontrol.io/back/internal/platform/shutdown"
	"go.upcontrol.io/back/internal/ring/cutoff"
	"go.upcontrol.io/back/internal/storage/ch"
	"go.upcontrol.io/back/internal/storage/pg"
)

func main() {
	os.Exit(app.Run("ucworker", setup))
}

func setup(ctx context.Context, d app.Deps) (func() error, error) {
	mux := http.NewServeMux()
	mux.Handle("GET /health", d.Health.Handler())

	if d.Config.PostgresURL != "" {
		if err := wireJobs(ctx, d); err != nil {
			d.Logger.Error("ucworker wiring failed; serving /health only", "err", err)
		}
	}

	return app.ServeHTTP(d.Config.HTTPAddr, mux, d)
}

func wireJobs(ctx context.Context, d app.Deps) error {
	pool, err := pg.Open(ctx, d.Config.PostgresURL)
	if err != nil {
		return err
	}
	d.Health.Register("postgres", pool.Ping)
	d.Shutdown.Register(shutdown.Task{Name: "pg-pool", Stop: func(context.Context) error { pool.Close(); return nil }})

	// --- Delivery worker (queue → channels → telegram/email/discord/slack) ---
	// Lives in ucworker, NOT ucapi: two ucapi replicas with an inline worker
	// would duplicate deliveries. The advisory lock in each Tick's lease query
	// ensures even two ucworker instances never double-deliver.
	// The bot token resolves per send: a token pasted into the Settings
	// screen (instance_setting, sealed under UC_SECRET_KEY_HEX) wins over
	// UC_TELEGRAM_BOT_TOKEN, and alerts start flowing without a restart.
	var openFn func([]byte) ([]byte, error)
	if d.Config.SecretKeyHex != "" {
		if k, kerr := config.SecretKeyFromHex(d.Config.SecretKeyHex); kerr == nil {
			openFn = k.Open
		}
	}
	tgToken := func(c context.Context) string {
		return pool.InstanceValue(c, openFn, "telegram_bot_token", d.Config.TelegramBotToken)
	}
	dw := deliver.NewWorker(pool, d.Logger, "ucworker")
	dw.RegisterChannel(&deliver.TelegramChannel{Token: tgToken})
	dw.RegisterChannel(&deliver.DiscordChannel{})
	dw.RegisterChannel(&deliver.SlackChannel{})
	// Alert email goes through the email agent only when UC_EMAIL_URL is set;
	// without the agent, a configured SMTP relay carries the alerts directly
	// (the self-host path — same precedence as ucapi's magic-link mailer).
	if d.Config.EmailURL != "" {
		dw.RegisterChannel(&deliver.EmailChannel{
			APIURL: d.Config.EmailURL,
			APIKey: d.Config.EmailAPIKey,
			// Where the mail's button sends the reader. The agent has no idea
			// what origin this deployment answers on, so it travels per send.
			AppURL: strings.TrimRight(d.Config.PublicOrigin, "/") + "/app",
		})
	} else {
		// SMTP resolves per send (a relay saved in Settings wins over
		// UC_SMTP_*): alerts start flowing the moment one is saved, no
		// restart. An unconfigured relay errors per delivery with the fix in
		// the message — the Telegram channel's missing-token contract. The
		// boot refusal survives for the env half: UC_SMTP_HOST with no From
		// is still a config error.
		if d.Config.SMTPHost != "" && d.Config.SMTPFrom == "" {
			d.Logger.Error("mailer: refusing to start", "err", "UC_SMTP_HOST set but UC_SMTP_FROM empty")
			os.Exit(1)
		}
		smtpCfg := func(c context.Context) mailer.Config {
			port := d.Config.SMTPPort
			if p := pool.InstanceValue(c, openFn, "smtp_port", ""); p != "" {
				if n, perr := strconv.Atoi(p); perr == nil {
					port = n
				}
			}
			return mailer.Config{
				Host:     pool.InstanceValue(c, openFn, "smtp_host", d.Config.SMTPHost),
				Port:     port,
				Username: pool.InstanceValue(c, openFn, "smtp_username", d.Config.SMTPUsername),
				Password: pool.InstanceValue(c, openFn, "smtp_password", d.Config.SMTPPassword),
				From:     pool.InstanceValue(c, openFn, "smtp_from", d.Config.SMTPFrom),
				FromName: d.Config.SMTPFromName,
			}
		}
		dw.RegisterChannel(&deliver.SMTPChannel{Mailer: mailer.NewDynamic(smtpCfg, d.Logger)})
	}
	go dw.Run(ctx, 2*time.Second)

	// --- Cutoff recompute: every 1 minute per project ---
	go runWithLock(ctx, pool, d, "cutoff", time.Minute, func(ctx context.Context) {
		recomputeCutoff(ctx, pool, d)
	})

	// --- Purge expired ingest batches: every 5 minutes ---
	go runWithLock(ctx, pool, d, "purge-batches", 5*time.Minute, func(ctx context.Context) {
		_ = pool.Queries().PurgeExpiredBatches(ctx)
	})

	// --- Error-log notification scanner: every 60 seconds ---
	// The first ucworker job that reads ClickHouse (through ring.QueryBuilder
	// aggregates — invariants 2/4). It backs the per-channel "Error logs" /
	// "Repeating error logs" settings (docs/plans/channel-notify-settings.md).
	// Without ClickHouse configured the job simply does not start: a scanner
	// with nothing to scan is not an error, it is a smaller deployment.
	jobs := "delivery+cutoff+purge"
	if d.Config.ClickHouseAddr != "" {
		chConn, cherr := ch.Open(ctx, ch.Options{
			Addr: []string{d.Config.ClickHouseAddr}, Database: d.Config.ClickHouseDB,
			Username: d.Config.ClickHouseUser, Password: d.Config.ClickHousePass,
		})
		if cherr != nil {
			d.Logger.Error("ucworker: clickhouse unreachable; error-log scanner disabled", "err", cherr)
		} else {
			d.Shutdown.Register(shutdown.Task{Name: "ch-conn", Stop: func(context.Context) error { return chConn.Close() }})
			scanner := errorlog.New(pool, chConn, d.Logger)
			go runWithLock(ctx, pool, d, "errorlog-scan", time.Minute, func(ctx context.Context) {
				if err := scanner.Tick(ctx); err != nil {
					d.Logger.Warn("errorlog scan tick error", "err", err)
				}
			})
			jobs += "+errorlog"
			// --- Detection (error-rate incidents): every minute ---
			// The orchestrator that wires detectors + suppression to incidents
			// (docs/plans/detect-errorrate-v1.md). Same advisory-lock pattern as
			// every other job, so two ucworkers never double-open. On by default
			// with ClickHouse present; UC_DETECT_ENABLED=0 is the kill switch.
			if d.Config.DetectEnabled {
				lc := incident.New(pool, chConn)
				det := detect.New(pool, chConn, lc, d.Logger)
				go runWithLock(ctx, pool, d, "detect", time.Minute, func(ctx context.Context) {
					_ = det.Tick(ctx)
				})
				jobs += "+detect"
			}
		}
	}

	d.Logger.Info("ucworker jobs started", "jobs", jobs)
	return nil
}

// runWithLock acquires a named advisory lock, runs fn, then releases. If the
// lock is held by another instance, it skips (non-blocking). This is the
// invariant-5/6 mechanism: the advisory lock is the single-writer guarantee.
func runWithLock(ctx context.Context, pool *pg.Pool, _ app.Deps, jobName string, every time.Duration, fn func(context.Context)) {
	lockKey := hashJobName(jobName)
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			// Advisory locks are session-scoped: the try-lock, fn and unlock
			// must ride ONE pooled connection. Routing the unlock through the
			// pool let it land on a different session — the unlock answered
			// false (ignored), the lock stayed with its idle session forever,
			// and every later tick answered "held by another instance". All
			// runWithLock jobs went silently dead once the pool churned.
			conn, err := pool.Raw().Acquire(ctx)
			if err != nil {
				continue
			}
			// Try to acquire the lock (non-blocking — skip if held).
			var got bool
			if err := conn.QueryRow(ctx,
				"SELECT pg_try_advisory_lock($1)", lockKey).Scan(&got); err != nil || !got {
				conn.Release()
				continue // another instance is handling it
			}
			func() {
				defer func() {
					_, _ = conn.Exec(ctx, "SELECT pg_advisory_unlock($1)", lockKey)
					conn.Release()
				}()
				fn(ctx)
			}()
		}
	}
}

// recomputeCutoff walks every project's ledger and updates project_window.
func recomputeCutoff(ctx context.Context, pool *pg.Pool, d app.Deps) {
	// Get all projects.
	rows, err := pool.Raw().Query(ctx, `SELECT id, tenant_id FROM project`)
	if err != nil {
		d.Logger.Warn("cutoff: list projects", "err", err)
		return
	}
	type proj struct{ id, tenantID int64 }
	var projects []proj
	for rows.Next() {
		var p proj
		if err := rows.Scan(&p.id, &p.tenantID); err != nil {
			continue
		}
		projects = append(projects, p)
	}
	rows.Close()

	for _, p := range projects {
		// Get the tenant's plan.
		var plan string
		_ = pool.Raw().QueryRow(ctx,
			`SELECT t.plan FROM tenant t JOIN project p ON p.tenant_id = t.id WHERE p.id = $1`,
			p.id).Scan(&plan)
		if plan == "" {
			plan = "Free"
		}
		ent, ok := cutoff.PlanEntitlements[plan]
		if !ok {
			ent = cutoff.PlanEntitlements["Free"]
		}

		result, err := cutoff.Recompute(ctx, pool, p.id, ent)
		if err != nil {
			d.Logger.Warn("cutoff recompute", "project", p.id, "err", err)
			continue
		}

		// Persist to project_window.
		var beyondErrors *int64
		if result.BeyondErrors != nil {
			beyondErrors = result.BeyondErrors
		}
		_ = pool.Queries().UpsertProjectWindow(ctx, sqlc.UpsertProjectWindowParams{
			ProjectID:    p.id,
			CutoffSeq:    result.CutoffSeq,
			RetainSeq:    result.RetainSeq,
			WindowHours:  pgtype.Numeric{Int: new(big.Int).SetUint64(uint64(result.WindowHours * 100)), Exp: -2, Valid: true},
			BeyondErrors: beyondErrors,
		})
	}
}

// hashJobName produces a stable int64 for the advisory lock key.
func hashJobName(name string) int64 {
	var h int64
	for _, c := range name {
		h = h*31 + int64(c)
	}
	return h
}
