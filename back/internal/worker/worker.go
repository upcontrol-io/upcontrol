// Package worker holds the background jobs: the delivery queue and the
// periodic maintenance and detection ticks. Every job takes a Postgres
// advisory lock and skips when another instance holds it, so it is safe to run
// from N processes -- which is what lets ucapi run them in-process on a
// single-container install.
package worker

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
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
	// after 7 days (Decision 10, docs/plans/projects-axis.md) - except a tenant
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

	// The index gate and its ramp (plan part 4): qualify, stamp at most
	// cfg.IndexRampPerDay a day, and leave with hysteresis. Every 10 minutes;
	// the DNS probes are cached per host per run.
	gate := newIndexGate(d.Config.StatusPageKnobs)
	go runWithLock(ctx, pool, d, "index-gate", 10*time.Minute, func(ctx context.Context) {
		gate.tick(ctx, pool, d.Logger)
	})

	// DNS TXT tokens: host verification (claimed pages proving control) and
	// self-serve removal. Every 10 minutes against the real resolver; tests
	// inject lookupTXT.
	go runWithLock(ctx, pool, d, "dns-tokens", 10*time.Minute, func(ctx context.Context) {
		if err := verifyHostTokens(ctx, pool, d.Logger); err != nil {
			d.Logger.Warn("dns-tokens verify tick error", "err", err)
		}
		if err := removeByToken(ctx, pool, d.Logger); err != nil {
			d.Logger.Warn("dns-tokens remove tick error", "err", err)
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

	jobs := "delivery+purge+reaper+incident-purge"
	pgs := pgstore.New(pool.Raw())

	// The board's rollup, trimmed to the tenant's plan depth: hourly, because
	// the depth is sold in days and an hour of slack is invisible. NULL trims
	// nothing, which is what makes Self-hosted unlimited. The statement lives
	// in pgstore so the integration test runs the very code this job runs.
	go runWithLock(ctx, pool, d, "history-trim", time.Hour, func(ctx context.Context) {
		if _, err := pgs.TrimHistory(ctx); err != nil {
			d.Logger.Warn("history trim tick error", "err", err)
		}
	})
	jobs += "+history-trim"

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
		                     WHERE p.tenant_id = t.id AND ps.next > 1)
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

// lookupHost is the gate's DNS probe, a package var so tests can inject one
// (real DNS is untestable).
var lookupHost = net.LookupHost

// gatePage is one candidate row of the index gate.
type gatePage struct {
	ID           int64
	ProjectID    int64
	Domain       string
	RootTargetID int64
	MintedSource *string
	LastSeenAt   *time.Time
	IndexedAt    *time.Time
	ReindexHold  bool
	Claimed      bool
}

// indexGate is the part-4 job: qualification, ramp and hysteresis. The knob
// set is read once per process (a knob change is a restart); the per-run DNS
// cache lives in tick.
type indexGate struct {
	cfg config.StatusPageKnobs
	// lastLogDay tracks the daily log line: once per calendar day, the run
	// that first crosses midnight reports.
	lastLogDay string
}

func newIndexGate(cfg config.StatusPageKnobs) *indexGate { return &indexGate{cfg: cfg} }

// indexCandidates is the one candidate set: pages with removed_at NULL.
// An UNCLAIMED host page qualifies on continuity alone (nobody exists to
// ask for proof); a CLAIMED page - ANY page, host page included - only when
// opted in AND DNS-verified (review decision 16). A root target is required
// - continuity is measured on it.
func indexCandidates(ctx context.Context, pool *pg.Pool) ([]gatePage, error) {
	// ORDER BY created_at: the oldest page first for the ramp (the mint's
	// own timestamp, backfilled from the tenant for pre-009 pages).
	rows, err := pool.Raw().Query(ctx,
		`SELECT sp.id, sp.project_id, p.domain, sp.root_target_id, sp.minted_source,
		       sp.last_seen_at, sp.indexed_at, sp.reindex_hold,
		       (t.claim_token_hash IS NULL) AS claimed
		  FROM status_page sp
		  JOIN project p ON p.id = sp.project_id
		  JOIN tenant t ON t.id = sp.tenant_id
		 WHERE sp.removed_at IS NULL AND sp.root_target_id IS NOT NULL
		   AND ((sp.is_host_page AND t.claim_token_hash IS NOT NULL)
		        OR (t.claim_token_hash IS NULL AND sp.index_opt_in AND sp.host_verified_at IS NOT NULL))
		 ORDER BY sp.created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []gatePage
	for rows.Next() {
		var g gatePage
		if err := rows.Scan(&g.ID, &g.ProjectID, &g.Domain, &g.RootTargetID, &g.MintedSource,
			&g.LastSeenAt, &g.IndexedAt, &g.ReindexHold, &g.Claimed); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// continuity is the measured half of qualification over one window.
type continuity struct {
	measured, ok, total uint64
	expected            float64
	lastOK              *time.Time
	firstTS             *time.Time
	cadence             int32
}

// continuityOver reads the target's measurements in the window and computes
// the ratio inputs: measured (not unmeasured) readings, ok among them, the
// expected count from the cadence of the newest row, and the newest ok's
// age. expected counts only the time the target has actually existed for
// (first reading to now), so a young target is judged on its own window.
func continuityOver(ctx context.Context, pool *pg.Pool, targetID int64, window time.Duration) (continuity, error) {
	var c continuity
	var lastOK, firstTS *time.Time
	err := pool.Raw().QueryRow(ctx, fmt.Sprintf(`
		SELECT count(*),
		       count(*) FILTER (WHERE %s),
		       count(*) FILTER (WHERE ok),
		       max(ts) FILTER (WHERE ok),
		       min(ts),
		       (SELECT interval_sec FROM checks WHERE target_id = $1 ORDER BY ts DESC LIMIT 1)
		  FROM checks
		 WHERE target_id = $1 AND ts >= now() - make_interval(secs => $2::double precision)`,
		pgstore.MeasurableSQL), targetID, window.Seconds()).Scan(
		&c.total, &c.measured, &c.ok, &lastOK, &firstTS, &c.cadence)
	c.lastOK, c.firstTS = lastOK, firstTS
	if err != nil {
		return c, err
	}
	if c.cadence <= 0 {
		return c, nil
	}
	elapsed := window
	if firstTS != nil {
		if age := time.Since(*firstTS); age < elapsed {
			elapsed = age
		}
	}
	c.expected = elapsed.Seconds() / float64(c.cadence)
	return c, nil
}

// qualifies is the continuity bar over 72 h: at least 90% of the expected
// checks measured, at least 95% of those ok, and the newest ok under an hour
// old.
func (c continuity) qualifies(now time.Time) bool {
	if c.expected <= 0 || c.measured == 0 {
		return false
	}
	if float64(c.measured)/c.expected < 0.9 {
		return false
	}
	if float64(c.ok)/float64(c.measured) < 0.95 {
		return false
	}
	if c.lastOK == nil || now.Sub(*c.lastOK) > time.Hour {
		return false
	}
	return true
}

// tick is the gate's run: qualify the candidates, stamp within the day's
// ramp and the instance ceiling, apply hysteresis to indexed pages, and log
// one line a day.
func (g *indexGate) tick(ctx context.Context, pool *pg.Pool, log *slog.Logger) {
	if g.cfg.IndexDisabled {
		return
	}
	now := time.Now().UTC()
	candidates, err := indexCandidates(ctx, pool)
	if err != nil {
		log.Warn("index-gate: candidate read failed", "err", err)
		return
	}
	// Wildcard-DNS cache, per host per run: one lookup per host, not per page.
	wildcard := map[string]bool{}
	wildcardHit := func(host string) bool {
		if v, ok := wildcard[host]; ok {
			return v
		}
		b := make([]byte, 4)
		_, _ = rand.Read(b)
		_, err := lookupHost(hex.EncodeToString(b) + "." + host)
		w := err == nil
		wildcard[host] = w
		return w
	}
	refusals := map[string]int{}
	refuse := func(reason string) { refusals[reason]++ }
	qualified := 0
	var qualifiedUnindexed []gatePage
	for _, p := range candidates {
		// Hysteresis first: an indexed page whose continuity has been broken
		// for 7 consecutive days, or whose host went NXDOMAIN, leaves the
		// index with a hold only the operator clears.
		if p.IndexedAt != nil {
			g.hysteresis(ctx, pool, log, p, wildcardHit)
		}
		if p.IndexedAt != nil || p.ReindexHold {
			continue
		}
		c, cerr := continuityOver(ctx, pool, p.RootTargetID, 72*time.Hour)
		if cerr != nil || !c.qualifies(now) {
			refuse("continuity")
			continue
		}
		if p.Domain != "" && wildcardHit(p.Domain) {
			// A wildcard host answers anything: the page would rank for typos
			// and parked domains.
			refuse("wildcard_dns")
			continue
		}
		// Origin (review decision 17): a page a human asked for on the landing
		// qualifies on continuity alone; a seeded page only once a human has
		// interacted with it or a state change was recorded on its host.
		if src := ptrString(p.MintedSource); src == "seed" {
			if !seedInteraction(ctx, pool, p) {
				refuse("seed_no_interaction")
				continue
			}
		}
		qualified++
		qualifiedUnindexed = append(qualifiedUnindexed, p)
	}

	// The ramp: oldest qualified first, at most cfg.IndexRampPerDay stamps
	// today, never more than cfg.IndexMaxPages indexed at once.
	stampedToday := countQuery(ctx, pool,
		`SELECT count(*) FROM status_page WHERE indexed_at >= date_trunc('day', now() AT TIME ZONE 'utc')`)
	totalIndexed := countQuery(ctx, pool,
		`SELECT count(*) FROM status_page WHERE indexed_at IS NOT NULL`)
	for _, p := range qualifiedUnindexed {
		if stampedToday >= g.rampPerDay() || totalIndexed >= g.indexMax() {
			refuse("ramp_full")
			break
		}
		tag, err := pool.Raw().Exec(ctx,
			`UPDATE status_page SET indexed_at = now() WHERE id = $1 AND indexed_at IS NULL`, p.ID)
		if err != nil {
			continue
		}
		if tag.RowsAffected() > 0 {
			stampedToday++
			totalIndexed++
		}
	}

	// One line a day: qualified, stamped today, total indexed, and the most
	// common refusal reason (the operator's window into the gate).
	if day := now.Format("2006-01-02"); day != g.lastLogDay {
		g.lastLogDay = day
		top, topN := "", 0
		for reason, n := range refusals {
			if n > topN {
				top, topN = reason, n
			}
		}
		log.Info("index gate",
			"qualified", qualified,
			"stamped_today", stampedToday,
			"total_indexed", totalIndexed,
			"top_refusal", top)
	}
}

// rampPerDay and indexMax guard zero-valued knobs (a Config built by hand in
// a test): the defaults are the config package's.
func (g *indexGate) rampPerDay() int {
	return g.cfg.WithDefaults().IndexRampPerDay
}

func (g *indexGate) indexMax() int {
	return g.cfg.WithDefaults().IndexMaxPages
}

// hysteresis takes an indexed page OUT of the index: continuity broken for
// 7 consecutive days (no day in the last 7 met the bar), or NXDOMAIN. Both
// set reindex_hold - re-entry needs the operator to clear it, and then the
// ramp again, so the robots meta cannot flap.
func (g *indexGate) hysteresis(ctx context.Context, pool *pg.Pool, log *slog.Logger, p gatePage, wildcardHit func(string) bool) {
	if p.Domain == "" {
		return
	}
	gone := false
	if _, err := lookupHost(p.Domain); err != nil {
		var dnsErr *net.DNSError
		if errors.As(err, &dnsErr) && dnsErr.IsNotFound {
			gone = true // NXDOMAIN: the host itself is gone
		}
	}
	if !gone && !anyDayMetBar(ctx, pool, p.RootTargetID, 7) {
		gone = true
	}
	if !gone {
		return
	}
	if _, err := pool.Raw().Exec(ctx,
		`UPDATE status_page SET indexed_at = NULL, reindex_hold = true WHERE id = $1`, p.ID); err == nil {
		log.Info("index gate: page left the index", "page", p.ID, "domain", p.Domain)
	}
}

// anyDayMetBar answers whether any of the last `days` UTC days met the
// continuity bar on the target (the same ratios as qualification, evaluated
// per day; a day with no measurements at all is below the bar). A page
// leaves only when every one of them was below - seven consecutive broken
// days, not one bad Wednesday.
func anyDayMetBar(ctx context.Context, pool *pg.Pool, targetID int64, days int) bool {
	var cadence int32
	if err := pool.Raw().QueryRow(ctx,
		`SELECT interval_sec FROM checks WHERE target_id = $1 ORDER BY ts DESC LIMIT 1`, targetID).Scan(&cadence); err != nil || cadence <= 0 {
		return false // nothing measured at all: below the bar
	}
	rows, err := pool.Raw().Query(ctx, fmt.Sprintf(`
		SELECT date_trunc('day', ts AT TIME ZONE 'utc') AS day,
		       count(*) FILTER (WHERE %s) AS measured,
		       count(*) FILTER (WHERE ok) AS ok
		  FROM checks
		 WHERE target_id = $1 AND ts >= date_trunc('day', now() AT TIME ZONE 'utc') - make_interval(days => $2::int)
		 GROUP BY day ORDER BY day`, pgstore.MeasurableSQL), targetID, days)
	if err != nil {
		return false
	}
	defer rows.Close()
	now := time.Now().UTC()
	for rows.Next() {
		var day time.Time
		var measured, ok uint64
		if rows.Scan(&day, &measured, &ok) != nil {
			continue
		}
		// How much of this day has elapsed: a partial day is judged on the
		// time it has had, capped at the full 24 h so yesterday counts whole.
		end := day.Add(24 * time.Hour)
		elapsed := 24 * time.Hour
		if end.After(now) {
			elapsed = now.Sub(day)
		}
		if elapsed <= 0 {
			continue
		}
		expected := elapsed.Seconds() / float64(cadence)
		if expected <= 0 || measured == 0 {
			continue
		}
		if float64(measured)/expected >= 0.9 && float64(ok)/float64(measured) >= 0.95 {
			return true
		}
	}
	return false
}

// seedInteraction answers whether a seeded page has been touched by a human
// or has recorded a state change: claimed, or a monitor on the same root
// target from ANOTHER project, or a visit (last_seen_at), or an incident in
// the page's project.
func seedInteraction(ctx context.Context, pool *pg.Pool, p gatePage) bool {
	if p.Claimed || p.LastSeenAt != nil {
		return true
	}
	var ok bool
	_ = pool.Raw().QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM monitor m
		                WHERE m.target_id = $1 AND m.project_id <> $2)
		    OR EXISTS (SELECT 1 FROM incident i WHERE i.project_id = $2)`,
		p.RootTargetID, p.ProjectID).Scan(&ok)
	return ok
}

func countQuery(ctx context.Context, pool *pg.Pool, q string) int {
	var n int
	_ = pool.Raw().QueryRow(ctx, q).Scan(&n)
	return n
}

func ptrString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// lookupTXT is the resolver the verification and removal arms share, a
// package var so tests can inject one (real DNS is untestable). The pattern
// is verifyStatusDomain's: the default resolver, a short-lived context.
var lookupTXT = func(ctx context.Context, name string) ([]string, error) {
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return net.DefaultResolver.LookupTXT(cctx, name)
}

// sweepTokenPages is the loop both DNS-token arms run: query the pages
// with an outstanding token, reduce each host to its eTLD+1, look the arm's
// TXT record up, and apply the arm's action on the pages whose record
// carries the token. tokenCol and pendingCol are the two columns that tell
// the arms apart (verification_token/host_verified_at vs
// removal_token/removed_at); record is the full record label (dot included,
// from internal/dnstokens - the one home of both names).
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

// verifyHostTokens stamps host_verified_at on claimed pages whose DNS TXT
// proof has landed: _upcontrol-verify.<eTLD+1> containing the issued token.
// The token is cleared on success, so the door stops offering one.
func verifyHostTokens(ctx context.Context, pool *pg.Pool, log *slog.Logger) error {
	return sweepTokenPages(ctx, pool, "verification_token", "host_verified_at", dnstokens.VerifyRecord,
		func(ctx context.Context, page int64, domain string) error {
			if _, err := pool.Raw().Exec(ctx,
				`UPDATE status_page SET host_verified_at = now(), verification_token = NULL
			  WHERE id = $1 AND host_verified_at IS NULL`, page); err != nil {
				return err
			}
			// The worker has no analytics recorder: the event is logged here, and
			// the page_verified server event lands when the wiring does (noted in
			// the Group 2 receipt).
			log.Info("page_verified", "page", page, "domain", domain)
			return nil
		})
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
