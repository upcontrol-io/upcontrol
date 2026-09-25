// Package worker holds the background jobs: the delivery queue and the
// periodic maintenance and detection ticks. Every job takes a Postgres
// advisory lock and skips when another instance holds it, so it is safe to run
// from N processes -- which is what lets ucapi run them in-process on a
// single-container install.
package worker

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"golang.org/x/net/publicsuffix"

	"go.upcontrol.io/back/internal/deliver"
	"go.upcontrol.io/back/internal/detect"
	"go.upcontrol.io/back/internal/detect/errorlog"
	"go.upcontrol.io/back/internal/dnstokens"
	"go.upcontrol.io/back/internal/heartbeat"
	"go.upcontrol.io/back/internal/incident"
	"go.upcontrol.io/back/internal/notify/mailer"
	"go.upcontrol.io/back/internal/platform/app"
	"go.upcontrol.io/back/internal/platform/config"
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

	// Purge expired ingest batches: every 5 minutes. The install tokens nobody
	// redeemed ride along: a handful of rows with a TTL and no other reaper, and
	// the house style for a TTL purge is a job rather than a DELETE in a handler.
	go runWithLock(ctx, pool, d, "purge-batches", 5*time.Minute, func(ctx context.Context) {
		_ = pool.Queries().PurgeExpiredBatches(ctx)
		_, _ = pool.Raw().Exec(ctx, `DELETE FROM install_token WHERE expires_at < now()`)
	})

	// Day partitions of logs: every hour. 001 made two and left the rolling to
	// a job that never existed, so once the calendar passed them every flush
	// failed and the lines were lost. Migration 003 seeds four days, which is
	// why an hourly tick is soon enough.
	go runWithLock(ctx, pool, d, "log-partitions", time.Hour, func(ctx context.Context) {
		rollLogPartitions(ctx, pool, d)
	})

	// Unclaimed anonymous tenants: monitors pause after 24 h, the tenant dies
	// after 7 days - except a tenant
	// holding a live host page whose root target has answered at least once:
	// that page is forever (plan part 2).
	go runWithLock(ctx, pool, d, "unclaimed-reaper", 10*time.Minute, func(ctx context.Context) {
		if err := reapUnclaimed(ctx, pool); err != nil {
			d.Logger.Warn("unclaimed reaper tick error", "err", err)
		}
	})

	// Day partitions of checks: hourly, like the logs roller, with the checks
	// retention floor instead (max(history_days)+1 day; a NULL history_days
	// anywhere means never drop - the widest plan's days are the floor, and
	// an unlimited plan is unlimited).
	go runWithLock(ctx, pool, d, "checks-partitions", time.Hour, func(ctx context.Context) {
		rollCheckPartitions(ctx, pool, d)
	})

	// DNS TXT tokens: self-serve removal. Every 10 minutes against the real
	// resolver; tests inject lookupTXT.
	go runWithLock(ctx, pool, d, "dns-tokens", 10*time.Minute, func(ctx context.Context) {
		if err := removeByToken(ctx, pool, d.Logger); err != nil {
			d.Logger.Warn("dns-tokens remove tick error", "err", err)
		}
	})

	// Incident hard purge: hourly, and only past the WIDEST plan window — below
	// it closed incidents are merely hidden by the read clamp (ListIncidents-
	// ByTenant.since_days), so an upgrade restores them at once. The ceiling
	// comes from plan_entitlement, never a constant: the table decides.
	// Cascades take slices, updates and queue rows with the incident. A frozen
	// project's incidents up to the freeze are a snapshot and never purge: the
	// freeze promise is "as it stopped", and an upgrade restores exactly that.
	// What it records after the freeze purges like anyone's.
	go runWithLock(ctx, pool, d, "incident-purge", time.Hour, func(ctx context.Context) {
		if _, err := pool.Raw().Exec(ctx,
			`DELETE FROM incident
			  WHERE resolved_at IS NOT NULL
			    AND detected_at < now() - make_interval(days => (SELECT max(incident_days)::int FROM plan_entitlement))
			    AND NOT EXISTS (SELECT 1 FROM project p
			                    WHERE p.id = incident.project_id AND p.frozen_at IS NOT NULL
			                      AND incident.detected_at <= p.frozen_at)`); err != nil {
			d.Logger.Warn("incident purge tick error", "err", err)
		}
	})

	jobs := "delivery+purge+reaper+incident-purge"
	pgs := pgstore.New(pool.Raw())

	// The board's rollup, trimmed to the tenant's plan depth: hourly, because
	// the depth is sold in days and an hour of slack is invisible. NULL trims
	// nothing, which is what makes Self-hosted unlimited. The statement lives
	// in pgstore so the integration test runs the very code this job runs.
	// The web compaction rides along once per UTC day; a restart runs it
	// again, which its idempotent steps allow.
	compacted := ""
	go runWithLock(ctx, pool, d, "history-trim", time.Hour, func(ctx context.Context) {
		if _, err := pgs.TrimHistory(ctx); err != nil {
			d.Logger.Warn("history trim tick error", "err", err)
		}
		now := time.Now().UTC()
		if day := now.Format(time.DateOnly); day != compacted {
			if err := pgs.CompactWeb(ctx, now); err != nil {
				d.Logger.Warn("web compaction tick error", "err", err)
				return
			}
			compacted = day
		}
	})
	jobs += "+history-trim"

	// Plan capacity, once at start and then every minute, so a payment lifts
	// the walls within a minute and a deploy does not push that minute back.
	lc := incident.New(pool, pgs)
	capacity := func(ctx context.Context) { PlanCapacity(ctx, pgs, lc, d.Logger) }
	go func() {
		runLocked(ctx, pool, hashJobName("plan-capacity"), capacity)
		runWithLock(ctx, pool, d, "plan-capacity", time.Minute, capacity)
	}()
	jobs += "+plan-capacity"

	// Error-log notification scanner, every 60 seconds; it backs the per-channel
	// "Error logs" / "Repeating error logs" settings.
	scanner := errorlog.New(pool, pgs, d.Logger)
	go runWithLock(ctx, pool, d, "errorlog-scan", time.Minute, func(ctx context.Context) {
		if err := scanner.Tick(ctx); err != nil {
			d.Logger.Warn("errorlog scan tick error", "err", err)
		}
	})
	jobs += "+errorlog"
	// Heartbeat miss sweep, every minute: a window that closed without
	// a ping is a failed check. Shares the lifecycle with detect below.
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

// PlanCapacity is one run of the plan-capacity job: a plan buys live
// projects, live HTTP checks and a custom domain. In order: the freeze first,
// so a frozen project's checks hold no budget slot when the budget is
// counted; then the budget, and the close of every plan-paused check's open
// incident; then the outages a thawed project recorded while frozen, after
// the budget so a check about to be paused is never paged; then the domain
// grace. Project deletion runs it too, so a snapshot takes the released slot
// at once.
func PlanCapacity(ctx context.Context, pgs *pgstore.Store, lc *incident.Lifecycle, log *slog.Logger) {
	thawed, err := pgs.FreezeSweep(ctx)
	if err != nil {
		log.Warn("project freeze tick error", "err", err)
	}
	if _, err := pgs.MonitorBudgetSweep(ctx); err != nil {
		log.Warn("monitor budget tick error", "err", err)
	}
	if err := lc.ClosePlanPaused(ctx); err != nil {
		log.Warn("plan pause: incident close failed", "err", err)
	}
	for _, id := range thawed {
		if err := lc.NotifyThawed(ctx, id); err != nil {
			log.Warn("thaw: incident delivery failed", "project_id", id, "err", err)
		}
	}
	if err := pgs.DomainGraceSweep(ctx); err != nil {
		log.Warn("domain grace tick error", "err", err)
	}
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
			runLocked(ctx, pool, lockKey, fn)
		}
	}
}

// runLocked is one run of fn under the advisory lock, skipped when another
// instance holds it.
func runLocked(ctx context.Context, pool *pg.Pool, lockKey int64, fn func(context.Context)) {
	// Advisory locks are session-scoped: the try-lock, fn and unlock must
	// ride ONE pooled connection, or the unlock lands on another session.
	conn, err := pool.Raw().Acquire(ctx)
	if err != nil {
		return
	}
	defer conn.Release()
	// Try to acquire the lock (non-blocking — skip if held).
	var got bool
	if err := conn.QueryRow(ctx,
		"SELECT pg_try_advisory_lock($1)", lockKey).Scan(&got); err != nil || !got {
		return // another instance is handling it
	}
	defer func() { _, _ = conn.Exec(ctx, "SELECT pg_advisory_unlock($1)", lockKey) }()
	fn(ctx)
}

// reapUnclaimed retires abandoned anonymous tenants: their monitors pause
// after 24 h and the
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
	//   - data: `project_seq.next > 1` (every project is born at 1 and only
	//     LeaseSeqBlock, internal/ring/seq, moves it), any events row, or a
	//     web_usage row. The web door writes page views straight into events
	//     and leases no seq, so a site-only install never moves the seq, and
	//     history-trim expires its page views past the web depth (a day on
	//     Free); web_usage keeps this month's visits and last month's.
	//   - an api_key still on the project. A page RELEASED by a project
	//     deletion has ingested plenty but its key died with the release
	//     (releaseProject), so the seq alone would spare it forever; that is
	//     an ownerless page, and it is exactly what this job is for.
	_, err := pool.Raw().Exec(ctx,
		`DELETE FROM tenant t
		  WHERE t.claim_token_hash IS NOT NULL
		    AND t.created_at < now() - interval '7 days'
		    AND NOT EXISTS (SELECT 1 FROM project p
		                      LEFT JOIN project_seq ps ON ps.project_id = p.id
		                      JOIN api_key ak    ON ak.project_id = p.id
		                     WHERE p.tenant_id = t.id
		                       AND (ps.next > 1 OR EXISTS (SELECT 1 FROM events e
		                                                    WHERE e.tenant_id = t.id AND e.project_id = p.id)
		                            OR EXISTS (SELECT 1 FROM web_usage u WHERE u.tenant_id = t.id)))
		    AND NOT EXISTS (SELECT 1 FROM status_page sp
		                    JOIN probe_target pt ON pt.id = sp.root_target_id
		                   WHERE sp.tenant_id = t.id AND sp.is_host_page
		                     AND sp.removed_at IS NULL AND sp.root_target_id IS NOT NULL
		                     AND pt.first_ok_at IS NOT NULL)`)
	return err
}

// rollCheckPartitions keeps the checks day partitions covering
// max(history_days)+1 day. A NULL history_days row anywhere means the widest
// plan is unlimited, so this tick never drops anything - it only creates
// ahead (the same "never drop" semantics TrimHistory implements). The
// host-page exemption inside reapUnclaimed is the other half of part 2's
// "the page never dies": an unclaimed tenant is spared the 7-day delete only
// when one of its pages is a live HOST page (is_host_page, removed_at NULL,
// root set) whose root target has answered at least once (first_ok_at).
// Suffixed pages get NO exemption - the old rule collects them.
func rollCheckPartitions(ctx context.Context, pool *pg.Pool, d app.Deps) {
	var maxDays *int
	var anyNull bool
	if err := pool.Raw().QueryRow(ctx,
		`SELECT max(history_days), bool_or(history_days IS NULL) FROM plan_entitlement`).Scan(&maxDays, &anyNull); err != nil || maxDays == nil {
		d.Logger.Warn("checks-partitions: plan window lookup failed or no rows; skipping", "err", err)
		return
	}
	keep := time.Duration(*maxDays+1) * 24 * time.Hour
	if anyNull {
		// Skip this tick entirely (the spec's "never drop"): the roller's
		// create-ahead and drop-behind share one horizon, so an unlimited plan
		// means no rolling at all - the default partition catches inserts and
		// the operator sizes retention.
		return
	}
	created, dropped, err := pgstore.New(pool.Raw()).RollCheckPartitions(ctx, time.Now(), 3, keep)
	if err != nil {
		d.Logger.Warn("checks-partitions: roll failed", "err", err, "created", created, "dropped", dropped)
		return
	}
	if len(created) > 0 || len(dropped) > 0 {
		d.Logger.Info("checks-partitions rolled",
			"created", created, "dropped", dropped, "keep_hours", int(keep.Hours()))
	}
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

// lookupTXT is the removal arm's resolver, a package var so tests can inject
// one (real DNS is untestable). The pattern
// is verifyStatusDomain's: the default resolver, a short-lived context.
var lookupTXT = func(ctx context.Context, name string) ([]string, error) {
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return net.DefaultResolver.LookupTXT(cctx, name)
}

// sweepTokenPages is the DNS-token loop: query the pages with an
// outstanding token, reduce each host to its eTLD+1, look the TXT record up,
// and apply the action on the pages whose record carries the token.
// tokenCol and pendingCol are removal_token/removed_at; record is the full
// record label (dot included, from internal/dnstokens).
func sweepTokenPages(ctx context.Context, pool *pg.Pool, tokenCol, pendingCol, record string,
	apply func(ctx context.Context, page int64, domain string) error) error {
	rows, err := pool.Raw().Query(ctx, fmt.Sprintf(
		`SELECT sp.id, sp.%s, p.domain
		   FROM status_page sp
		   JOIN project p ON p.id = sp.project_id
		  WHERE sp.%s IS NOT NULL AND sp.%s IS NULL`, tokenCol, tokenCol, pendingCol))
	if err != nil {
		return err
	}
	defer rows.Close()
	type pending struct {
		id    int64
		token string
		name  string
	}
	var work []pending
	for rows.Next() {
		var p pending
		if err := rows.Scan(&p.id, &p.token, &p.name); err != nil {
			return err
		}
		work = append(work, p)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, p := range work {
		domain, err := publicsuffix.EffectiveTLDPlusOne(p.name)
		if err != nil {
			continue // an unregibrable host can prove nothing by DNS
		}
		txts, err := lookupTXT(ctx, record+domain)
		if err != nil {
			continue
		}
		matched := false
		for _, txt := range txts {
			if strings.Contains(txt, p.token) {
				matched = true
				break
			}
		}
		if !matched {
			continue
		}
		if err := apply(ctx, p.id, domain); err != nil {
			return err
		}
	}
	return nil
}

// removeByToken is the self-serve removal: _upcontrol-remove.<eTLD+1>
// containing the page's removal token takes the page out, drops its project's
// subscriptions on the root target, and writes the eTLD+1 to blocked_host so
// the host is never minted again. One transaction: a half-removal is the one
// state worse than a live page.
func removeByToken(ctx context.Context, pool *pg.Pool, log *slog.Logger) error {
	return sweepTokenPages(ctx, pool, "removal_token", "removed_at", dnstokens.RemoveRecord,
		func(ctx context.Context, page int64, domain string) error {
			tx, err := pool.Raw().Begin(ctx)
			if err != nil {
				return err
			}
			// The root reference is read BEFORE it is dropped: RETURNING would
			// yield the NEW (NULL) value, and the subscription DELETE below needs
			// the old one.
			var root *int64
			if err := tx.QueryRow(ctx,
				`SELECT root_target_id FROM status_page WHERE id = $1 FOR UPDATE`, page).Scan(&root); err != nil {
				_ = tx.Rollback(ctx)
				return err
			}
			if _, err := tx.Exec(ctx,
				`UPDATE status_page SET removed_at = now(), root_target_id = NULL
			  WHERE id = $1 AND removed_at IS NULL`, page); err != nil {
				_ = tx.Rollback(ctx)
				return err
			}
			if root != nil {
				if _, err := tx.Exec(ctx,
					`DELETE FROM monitor m USING status_page sp
				  WHERE sp.id = $1 AND m.project_id = sp.project_id AND m.target_id = $2`,
					page, *root); err != nil {
					_ = tx.Rollback(ctx)
					return err
				}
			}
			if _, err := tx.Exec(ctx,
				`INSERT INTO blocked_host (domain, reason) VALUES ($1, 'self-serve TXT removal')
			 ON CONFLICT (domain) DO NOTHING`, domain); err != nil {
				_ = tx.Rollback(ctx)
				return err
			}
			if err := tx.Commit(ctx); err != nil {
				return err
			}
			log.Info("page_removed", "page", page, "domain", domain)
			return nil
		})
}

// hashJobName produces a stable int64 for the advisory lock key.
func hashJobName(name string) int64 {
	var h int64
	for _, c := range name {
		h = h*31 + int64(c)
	}
	return h
}
