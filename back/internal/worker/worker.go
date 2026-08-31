// Package worker holds the background jobs: the delivery queue and the
// periodic maintenance and detection ticks. Every job takes a Postgres
// advisory lock and skips when another instance holds it, so it is safe to run
// from N processes -- which is what lets ucapi run them in-process on a
// single-container install.
package worker

import (
	"context"
	"math/big"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	sqlc "go.upcontrol.io/back/gen/pg"
	"go.upcontrol.io/back/internal/deliver"
	"go.upcontrol.io/back/internal/detect"
	"go.upcontrol.io/back/internal/detect/errorlog"
	"go.upcontrol.io/back/internal/heartbeat"
	"go.upcontrol.io/back/internal/incident"
	"go.upcontrol.io/back/internal/notify/mailer"
	"go.upcontrol.io/back/internal/platform/app"
	"go.upcontrol.io/back/internal/platform/config"
	"go.upcontrol.io/back/internal/ring/cutoff"
	"go.upcontrol.io/back/internal/storage/pg"
	"go.upcontrol.io/back/internal/storage/pgstore"
)

// Start wires and launches every background job against an already-open pool.
// It does not block: each job runs on its own goroutine until ctx is done.
func Start(ctx context.Context, d app.Deps, pool *pg.Pool) error {
	// Delivery worker (queue → channels): the advisory lock in each Tick's
	// lease query prevents double delivery.
	var openFn func([]byte) ([]byte, error)

	if d.Config.SecretKeyHex != "" {
		if k, kerr := config.SecretKeyFromHex(d.Config.SecretKeyHex); kerr == nil {
			openFn = k.Open
		}
	}
	tgToken := func(c context.Context) string {
		return pool.InstanceValue(c, openFn, "telegram_bot_token", d.Config.TelegramBotToken)
	}
	appURL := strings.TrimRight(d.Config.PublicOrigin, "/") + "/app"
	dw := deliver.NewWorker(pool, d.Logger, "ucworker")
	dw.RegisterChannel(&deliver.TelegramChannel{Token: tgToken, AppURL: appURL})
	dw.RegisterChannel(&deliver.DiscordChannel{})
	dw.RegisterChannel(&deliver.SlackChannel{})
	// Alert email goes through the email agent only when UC_EMAIL_URL is set;
	// without it a configured SMTP relay carries the alerts (self-host path).
	if d.Config.EmailURL != "" {
		dw.RegisterChannel(&deliver.EmailChannel{
			APIURL: d.Config.EmailURL,
			APIKey: d.Config.EmailAPIKey,
			// Where the mail's button sends the reader. The agent has no idea
			// what origin this deployment answers on, so it travels per send.
			AppURL: appURL,
		})
	} else {
		// SMTP resolves per send (a relay saved in Settings wins over UC_SMTP_*):
		// UC_SMTP_HOST with no From is still a boot config error.
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
	go dw.Run(ctx)

	// Cutoff recompute: every 1 minute per project.
	go runWithLock(ctx, pool, d, "cutoff", time.Minute, func(ctx context.Context) {
		recomputeCutoff(ctx, pool, d)
	})

	// Purge expired ingest batches: every 5 minutes.
	go runWithLock(ctx, pool, d, "purge-batches", 5*time.Minute, func(ctx context.Context) {
		_ = pool.Queries().PurgeExpiredBatches(ctx)
	})

	// Day partitions of logs: every hour. 001 made two and left the rolling to
	// a job that never existed, so once the calendar passed them every flush
	// failed and the lines were lost. Migration 003 seeds four days, which is
	// why an hourly tick is soon enough.
	go runWithLock(ctx, pool, d, "log-partitions", time.Hour, func(ctx context.Context) {
		rollLogPartitions(ctx, pool, d)
	})

	// Unclaimed anonymous tenants: monitors pause after 24 h, the tenant dies
	// after 7 days (Decision 10, docs/plans/projects-axis.md).
	go runWithLock(ctx, pool, d, "unclaimed-reaper", 10*time.Minute, func(ctx context.Context) {
		if err := reapUnclaimed(ctx, pool); err != nil {
			d.Logger.Warn("unclaimed reaper tick error", "err", err)
		}
	})

	// Incident hard purge: hourly, and only past the WIDEST plan window — below
	// it closed incidents are merely hidden by the read clamp (ListIncidents-
	// ByTenant.since_days), so an upgrade restores them at once. The ceiling
	// comes from plan_entitlement, never a constant: the table decides.
	// Cascades take slices, updates and queue rows with the incident.
	go runWithLock(ctx, pool, d, "incident-purge", time.Hour, func(ctx context.Context) {
		if _, err := pool.Raw().Exec(ctx,
			`DELETE FROM incident
			  WHERE resolved_at IS NOT NULL
			    AND detected_at < now() - make_interval(days => (SELECT max(incident_days)::int FROM plan_entitlement))`); err != nil {
			d.Logger.Warn("incident purge tick error", "err", err)
		}
	})

	// Error-log notification scanner, every 60 seconds; it backs the per-channel
	// "Error logs" / "Repeating error logs" settings.
	jobs := "delivery+cutoff+purge+reaper+incident-purge"
	pgs := pgstore.New(pool.Raw())
	scanner := errorlog.New(pool, pgs, d.Logger)
	go runWithLock(ctx, pool, d, "errorlog-scan", time.Minute, func(ctx context.Context) {
		if err := scanner.Tick(ctx); err != nil {
			d.Logger.Warn("errorlog scan tick error", "err", err)
		}
	})
	jobs += "+errorlog"
	// Heartbeat miss sweep, every minute: a window that closed without
	// a ping is a failed check. Shares the lifecycle with detect below.
	lc := incident.New(pool, pgs)
	hb := heartbeat.New(pool, pgs, lc)
	go runWithLock(ctx, pool, d, "heartbeat", time.Minute, func(ctx context.Context) {
		if err := hb.Tick(ctx); err != nil {
			d.Logger.Warn("heartbeat tick error", "err", err)
		}
	})
	jobs += "+heartbeat"
	// Detection (error-rate incidents), every minute; same advisory-lock
	// pattern as every other job. On by default, UC_DETECT_ENABLED=0 kills.
	if d.Config.DetectEnabled {
		det := detect.New(pool, pgs, lc, d.Logger)
		go runWithLock(ctx, pool, d, "detect", time.Minute, func(ctx context.Context) {
			_ = det.Tick(ctx)
		})
		jobs += "+detect"
	}

	d.Logger.Info("background jobs started", "jobs", jobs)
	return nil
}

// runWithLock acquires a named advisory lock, runs fn, then releases; if the
// lock is held by another instance it skips (non-blocking, single-writer).
func runWithLock(ctx context.Context, pool *pg.Pool, _ app.Deps, jobName string, every time.Duration, fn func(context.Context)) {
	lockKey := hashJobName(jobName)
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			// Advisory locks are session-scoped: the try-lock, fn and unlock must
			// ride ONE pooled connection, or the unlock lands on another session.
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
		_ = pool.Queries().UpsertProjectWindow(ctx, sqlc.UpsertProjectWindowParams{
			ProjectID:   p.id,
			CutoffSeq:   result.CutoffSeq,
			RetainSeq:   result.RetainSeq,
			WindowHours: pgtype.Numeric{Int: new(big.Int).SetUint64(uint64(result.WindowHours * 100)), Exp: -2, Valid: true},
		})
	}
}

// reapUnclaimed retires abandoned anonymous tenants (Decision 10 of
// docs/plans/projects-axis.md): their monitors pause after 24 h and the
// tenant row is deleted after 7 days. The delete's cascade clears the
// tenant's remaining rows, the same cascade claim adoption relies on.
func reapUnclaimed(ctx context.Context, pool *pg.Pool) error {
	if _, err := pool.Raw().Exec(ctx, `UPDATE monitor SET paused = true WHERE NOT paused AND tenant_id IN (SELECT id FROM tenant WHERE claim_token_hash IS NOT NULL AND created_at < now() - interval '24 hours')`); err != nil {
		return err
	}
	// An unclaimed tenant is not always an abandoned demo page: `uc init`
	// without an account mints one through POST /v1/projects/anonymous and
	// hands the developer its API key, which they may be shipping data
	// through for weeks before they ever sign up. Such an install is spared,
	// and it is recognised by BOTH halves of what it is — a key it still
	// holds, and data that has actually flowed through it:
	//
	//   - `project_seq.next > 1` is the ingest marker: every project is born
	//     at 1 and only LeaseSeqBlock (internal/ring/seq) moves it.
	//   - an api_key still on the project. A page RELEASED by a project
	//     deletion has ingested plenty but its key died with the release
	//     (releaseProject), so the seq alone would spare it forever; that is
	//     an ownerless page, and it is exactly what this job is for.
	_, err := pool.Raw().Exec(ctx,
		`DELETE FROM tenant t
		  WHERE t.claim_token_hash IS NOT NULL
		    AND t.created_at < now() - interval '7 days'
		    AND NOT EXISTS (SELECT 1 FROM project p
		                      JOIN project_seq ps ON ps.project_id = p.id
		                      JOIN api_key ak    ON ak.project_id = p.id
		                     WHERE p.tenant_id = t.id AND ps.next > 1)`)
	return err
}

// rollLogPartitions keeps the logs day partitions covering the widest plan
// window plus a day. The window is read from plan_entitlement on every run:
// partitions are global while the window is per plan, so the floor has to sit
// under the widest one or the largest plan silently loses days it paid for.
// A window it cannot read drops nothing: a missing floor is not a floor of
// zero.
func rollLogPartitions(ctx context.Context, pool *pg.Pool, d app.Deps) {
	var hours int
	if err := pool.Raw().QueryRow(ctx, `SELECT max(window_hours) FROM plan_entitlement`).Scan(&hours); err != nil {
		d.Logger.Warn("log-partitions: plan window lookup failed", "err", err)
		return
	}
	keep := time.Duration(hours)*time.Hour + 24*time.Hour
	created, dropped, err := pgstore.New(pool.Raw()).RollLogPartitions(ctx, time.Now(), 3, keep)
	if err != nil {
		d.Logger.Warn("log-partitions: roll failed", "err", err, "created", created, "dropped", dropped)
		return
	}
	if len(created) > 0 || len(dropped) > 0 {
		d.Logger.Info("log-partitions rolled",
			"created", created, "dropped", dropped, "keep_hours", int(keep.Hours()))
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
