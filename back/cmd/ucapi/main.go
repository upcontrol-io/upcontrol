// Command ucapi is the public-facing service: the OpenAPI HTTP API (/v1/*), the
// public surface (/public/*) and the intentionally-schemaless ingest endpoint
// (POST /i). It also mounts the connect-go ProbeService on the same port.
//
// This wiring connects the Phase 1–3 ingest pipeline to real Postgres +
// ClickHouse: a POST /i with an API key decodes, scrubs, seq-allocates and
// batches rows into ClickHouse, with a durable WAL fsync before the receipt.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"time"

	"go.upcontrol.io/back/internal/account/auth"
	"go.upcontrol.io/back/internal/account/session"
	"go.upcontrol.io/back/internal/ai"
	"go.upcontrol.io/back/internal/analytics"
	"go.upcontrol.io/back/internal/api"
	"go.upcontrol.io/back/internal/incident"
	"go.upcontrol.io/back/internal/migrate"
	"go.upcontrol.io/back/internal/notify/mailer"
	"go.upcontrol.io/back/internal/platform/app"
	"go.upcontrol.io/back/internal/platform/config"
	"go.upcontrol.io/back/internal/platform/shutdown"
	"go.upcontrol.io/back/internal/rpc"
	"go.upcontrol.io/back/internal/source/webhook"
	"go.upcontrol.io/back/internal/storage/ch"
	"go.upcontrol.io/back/internal/storage/pg"

	probev1connect "go.upcontrol.io/back/gen/rpc/probe/v1/probev1connect"

	"go.upcontrol.io/back/internal/channel/telegram"
)

func main() {
	// Subcommand: `ucapi migrate` applies schema migrations before the app starts.
	if len(os.Args) > 1 && os.Args[1] == "migrate" {
		os.Exit(runMigrate())
	}
	os.Exit(app.Run("ucapi", setup))
}

func runMigrate() int {
	cfg, err := config.Load("ucapi")
	if err != nil {
		fmt.Fprintf(os.Stderr, "migrate: config: %v\n", err)
		return 2
	}
	if err := migrate.Run(context.Background(),
		cfg.PostgresURL,
		cfg.ClickHouseAddr, cfg.ClickHouseDB, cfg.ClickHouseUser, cfg.ClickHousePass,
		cfg.MigrationsPostgresDir, cfg.MigrationsClickHouseDir,
	); err != nil {
		fmt.Fprintf(os.Stderr, "migrate: %v\n", err)
		return 1
	}
	fmt.Println("migrate: OK")
	return 0
}

func setup(ctx context.Context, d app.Deps) (func() error, error) {
	mux := http.NewServeMux()
	mux.Handle("GET /health", d.Health.Handler())

	// Wire the ingest + auth pipeline when databases are configured.
	if d.Config.PostgresURL != "" {
		if err := wireRoutes(ctx, d, mux); err != nil {
			d.Logger.Error("database wiring failed; serving /health only", "err", err)
		}
	}

	return app.ServeHTTP(d.Config.HTTPAddr, mux, d)
}

// wireRoutes opens PG + CH, builds the ingest pipeline and auth handlers, and
// mounts every route on mux.
func wireRoutes(ctx context.Context, d app.Deps, mux *http.ServeMux) error {
	// The analytics recorder is assigned once PG+CH are open. The pool/conn
	// teardown tasks below stop it BEFORE closing: Shutdown runs every task
	// concurrently, so without this the recorder's final flush would race
	// closed pools and lose the tail. Recorder.Stop is idempotent — whichever
	// task runs first drains, the other just waits.
	var recorder *analytics.Recorder

	pgPool, err := pg.Open(ctx, d.Config.PostgresURL)
	if err != nil {
		return err
	}
	d.Health.Register("postgres", pgPool.Ping)
	d.Shutdown.Register(st("pg-pool", func(ctx context.Context) error {
		_ = recorder.Stop(ctx)
		pgPool.Close()
		return nil
	}))

	chConn, err := ch.Open(ctx, ch.Options{
		Addr: []string{d.Config.ClickHouseAddr}, Database: d.Config.ClickHouseDB,
		Username: d.Config.ClickHouseUser, Password: d.Config.ClickHousePass,
	})
	if err != nil {
		pgPool.Close()
		return err
	}
	d.Health.Register("clickhouse", chConn.Ping)
	d.Shutdown.Register(st("ch-conn", func(ctx context.Context) error {
		_ = recorder.Stop(ctx)
		return chConn.Close()
	}))

	_ = os.MkdirAll(d.Config.SpoolDir, 0o755)

	// --- Ingest (POST /i) ---
	ingester, batch, err := api.WireIngest(d.Config.SpoolDir, pgPool, chConn)
	if err != nil {
		_ = chConn.Close()
		pgPool.Close()
		return err
	}
	mux.Handle("POST /i", http.HandlerFunc(ingester.Handle))
	// The endpoint the docs promise (audit §6, design D4): the SAME pipeline,
	// mounted under the name agents read in /docs/api. Key anywhere (header,
	// query, body) and any-shape bodies are already the pipeline's own contract —
	// this is an alias, not a second ingest.
	mux.Handle("POST /v1/event", http.HandlerFunc(ingester.Handle))
	go driveBatcher(ctx, batch, 50*time.Millisecond, d.Logger)
	d.Shutdown.Register(st("batcher", func(c context.Context) error { return batch.Close(c) }))

	// --- Product analytics recorder (plan: product-analytics T1/T2) ---
	// Async, buffered, never on the response path. Created after the ingest
	// wiring succeeded (that error path closes the pools); its drain is
	// sequenced into the pg/ch teardown tasks registered above.
	recorder = analytics.NewRecorder(analytics.PoolStore{Pool: pgPool}, chConn, d.Logger)
	recorder.Start()

	// --- Instance-settable secrets (Settings screen → instance_setting) ---
	// One seal/open pair for everything the UI can store: values are sealed
	// under UC_SECRET_KEY_HEX before they land in Postgres; without that key
	// the UI door refuses and the resolvers fall back to env config. A
	// UI-set value wins over the env one — the last thing the operator did.
	var sealFn, openFn func([]byte) ([]byte, error)
	if d.Config.SecretKeyHex != "" {
		if k, err := config.SecretKeyFromHex(d.Config.SecretKeyHex); err == nil {
			sealFn, openFn = k.Seal, k.Open
		}
	}
	aiSettings := func(c context.Context) ai.OpenAISettings {
		return ai.OpenAISettings{
			BaseURL: pgPool.InstanceValue(c, openFn, "ai_base_url", d.Config.AIBaseURL),
			Model:   pgPool.InstanceValue(c, openFn, "ai_model", d.Config.AIModel),
			Key:     pgPool.InstanceValue(c, openFn, "ai_api_key", d.Config.AIAPIKey),
		}
	}
	tgToken := func(c context.Context) string {
		return pgPool.InstanceValue(c, openFn, "telegram_bot_token", d.Config.TelegramBotToken)
	}
	tgUsername := func(c context.Context) string {
		return pgPool.InstanceValue(c, openFn, "telegram_bot_username", d.Config.TelegramBotUsername)
	}
	smtpCfg := func(c context.Context) mailer.Config {
		port := d.Config.SMTPPort
		if p := pgPool.InstanceValue(c, openFn, "smtp_port", ""); p != "" {
			if n, perr := strconv.Atoi(p); perr == nil {
				port = n
			}
		}
		return mailer.Config{
			Host:     pgPool.InstanceValue(c, openFn, "smtp_host", d.Config.SMTPHost),
			Port:     port,
			Username: pgPool.InstanceValue(c, openFn, "smtp_username", d.Config.SMTPUsername),
			Password: pgPool.InstanceValue(c, openFn, "smtp_password", d.Config.SMTPPassword),
			From:     pgPool.InstanceValue(c, openFn, "smtp_from", d.Config.SMTPFrom),
			FromName: d.Config.SMTPFromName,
		}
	}

	// --- Auth (magic-link + me + logout) ---
	sm := session.New(pgPool, 0, d.Logger)
	if d.Config.UCAuth == "none" {
		// Single-user mode (public-first-split, Decision 16): provision the
		// owner once at boot and pin every request to that identity. The
		// magic-link door stays mounted but FromRequest never reads a cookie,
		// so signing in changes nothing.
		personID, tenantID, err := auth.Provision(ctx, pgPool, d.Config.OwnerEmail, "", recorder, d.Config.SelfHosted)
		if err != nil {
			d.Logger.Error("single-user mode: owner provisioning failed", "err", err)
			os.Exit(1)
		}
		sm.WithFixedIdentity(personID, tenantID)
		d.Logger.Warn("auth disabled (UC_AUTH=none): every request acts as the owner", "email", d.Config.OwnerEmail)
	}
	devMode := !d.Config.IsProd()
	// Without a mailer the code is stored and never sent, which outside dev mode
	// means nobody can sign in at all. Configured = required: a deployment that
	// sets UC_SMTP_HOST and gets it wrong must fail loudly at boot, not silently
	// per message.
	var mail auth.Mailer
	switch {
	case d.Config.EmailURL != "":
		// External email agent: URL set = every mail goes through it over
		// HTTP; SMTP below stays the fallback when it is unset.
		a, err := mailer.NewAgent(d.Config.EmailURL, d.Config.EmailAPIKey, d.Logger)
		if err != nil {
			d.Logger.Error("mailer: refusing to start", "err", err)
			os.Exit(1)
		}
		mail = a.WithSignInBase(d.Config.PublicOrigin)
	default:
		// SMTP resolves per message (a relay saved in Settings wins over
		// UC_SMTP_*), so sign-in mail starts flowing without a restart. The
		// boot-time refusal survives for the env half: UC_SMTP_HOST with no
		// From is still a config error, not a per-message surprise.
		if d.Config.SMTPHost != "" && d.Config.SMTPFrom == "" {
			d.Logger.Error("mailer: refusing to start", "err", "UC_SMTP_HOST set but UC_SMTP_FROM empty")
			os.Exit(1)
		}
		mail = mailer.NewDynamic(smtpCfg, d.Logger).WithSignInBase(d.Config.PublicOrigin)
		if d.Config.SMTPHost == "" && !devMode {
			d.Logger.Warn("mailer: no SMTP at boot; sign-in mail waits for UC_SMTP_HOST or a relay saved in Settings")
		}
	}
	mux.Handle("POST /v1/auth/magic-link", auth.NewMagicLink(pgPool, sm, devMode, mail, recorder, d.Logger).WithSelfHosted(d.Config.SelfHosted))
	mux.Handle("GET /v1/me", auth.NewMe(pgPool, sm))
	mux.Handle("POST /v1/auth/logout", auth.NewLogout(sm))
	// Google sign-in. Mounted whether or not it is configured: unconfigured it
	// answers 503 and names itself, which is a fact the sign-in page can read
	// to decide whether to draw the button at all. A door that is not there is
	// better than one painted on a wall.
	googleAuth := auth.NewGoogle(pgPool, sm, d.Config.GoogleClientID, d.Config.GoogleClientSecret,
		d.Config.GoogleRedirectURIs, devMode, recorder, d.Logger).WithSelfHosted(d.Config.SelfHosted)
	mux.Handle("POST /v1/auth/google", googleAuth)
	if googleAuth.Configured() {
		d.Logger.Info("auth: google sign-in enabled", "redirect_uris", d.Config.GoogleRedirectURIs)
	}
	// Telegram Mini App sign-in: server-verified initData HMAC (design D3).
	// The bot token IS the verification key; without a bot there is nothing to
	// verify against, and the endpoint stays a 501 rather than trusting a
	// client-supplied identity. Resolved at boot: a token pasted into
	// Settings reaches this door on the next restart.
	mux.Handle("POST /v1/auth/telegram", auth.NewTelegramMiniApp(pgPool, sm, tgToken(ctx), devMode))

	// --- Monitors (CRUD + entitlement gate) ---
	mon := api.NewMonitors(pgPool, sm)
	mux.Handle("GET /v1/monitors", mon)
	mux.Handle("POST /v1/monitors", mon)
	mux.Handle("PATCH /v1/monitors/{id}", mon)
	// The docs' word for a monitor is "check" (a component IS a check), so the
	// path they name has to answer (audit §6, D6). Same handler — the mux binds
	// {id} and ServeHTTP never looks at the path prefix.
	mux.Handle("PATCH /v1/checks/{id}", mon)
	mux.Handle("DELETE /v1/monitors/{id}", mon)

	// --- Read-only API (plan, sources, channels, recipients, incidents, overview) ---
	rd := api.NewReadAPI(pgPool, chConn, sm, tgUsername)
	mux.Handle("GET /v1/plan", rd)
	mux.Handle("GET /v1/sources", rd)
	mux.Handle("GET /v1/channels", rd)
	mux.Handle("GET /v1/recipients", rd)
	mux.Handle("GET /v1/incidents", rd)
	mux.Handle("GET /v1/overview", rd)

	// --- Keys (GET /v1/keys + POST /v1/keys/rotate) ---
	keys := api.NewKeys(pgPool, sm)
	mux.Handle("GET /v1/keys", keys)
	mux.Handle("POST /v1/keys/rotate", keys)

	// Instance settings (self-host only; the hosted cloud answers 404): the
	// Settings screen's AI-key and Telegram-bot fields write here, sealed
	// before storage.
	instSettings := api.NewInstanceSettings(pgPool, sm, d.Config.SelfHosted, sealFn)
	mux.Handle("PUT /v1/instance/ai", instSettings)
	mux.Handle("DELETE /v1/instance/ai", instSettings)
	mux.Handle("PUT /v1/instance/telegram-bot", instSettings)
	mux.Handle("DELETE /v1/instance/telegram-bot", instSettings)
	mux.Handle("PUT /v1/instance/smtp", instSettings)
	mux.Handle("DELETE /v1/instance/smtp", instSettings)

	// --- Write API (channels/recipients/sources CRUD, status-page, logs, incidents/{id}) ---
	// Explain has ONE brain: an OpenAI-compatible endpoint. The key resolves
	// per call — a Settings-set key (sealed in instance_setting) wins over
	// UC_AI_API_KEY — and no key anywhere means Explain answers 503
	// ai_not_configured, never a canned fallback (the heuristic was removed;
	// owner decision, 2026-08-20). The key's value is never logged.
	llm := &ai.OpenAIClient{
		Settings:  aiSettings,
		Timeout:   d.Config.AITimeout,
		LogPrompt: d.Config.AILogPrompt,
	}
	if bootAI := aiSettings(ctx); bootAI.Key != "" {
		d.Logger.Info("ai: explain llm active", "llm", "openai-compatible", "model", bootAI.Model, "base_url", bootAI.BaseURL)
	} else {
		d.Logger.Info("ai: explain not configured", "hint",
			"no AI key: paste one in Settings, or provide UC_AI_API_KEY_FILE (secrets/ai_api_key)")
	}
	acct := ai.New(pgPool, llm, ai.Prices{
		InputPer1M:  d.Config.AIInputPricePer1M,
		OutputPer1M: d.Config.AIOutputPricePer1M,
	})
	wa := api.NewWriteAPI(pgPool, chConn, sm, acct, devMode, mail, recorder, d.Config.SelfHosted)
	mux.Handle("POST /v1/channels", wa)
	// The gear's notification settings (docs/plans/channel-notify-settings.md).
	// The mux route is half the wiring: without it the PATCH died here as a bare
	// 405 while the handler sat unreachable — the same hole PATCH /v1/sources
	// documents at the bottom of this list.
	mux.Handle("PATCH /v1/channels/{id}", wa)
	mux.Handle("DELETE /v1/channels/{id}", wa)
	mux.Handle("POST /v1/channels/{id}/test", wa)
	// The outcome half of Send test (audit §3): the queue row's own state, not a
	// promise made at enqueue time.
	mux.Handle("GET /v1/deliveries/{id}", wa)
	mux.Handle("POST /v1/recipients", wa)
	mux.Handle("PATCH /v1/recipients/{id}", wa)
	mux.Handle("DELETE /v1/recipients/{id}", wa)
	// Telegram invite links (one-time inv_<token>, plan-gated). Session-authed;
	// notify-role members get 403 — inviting people is a settings act (§7.4).
	tginv := api.NewTelegram(pgPool, sm, tgUsername)
	mux.Handle("POST /v1/telegram/invites", tginv)
	mux.Handle("PATCH /v1/telegram/invites/{id}", tginv)
	mux.Handle("DELETE /v1/telegram/invites/{id}", tginv)
	mux.Handle("POST /v1/sources/{kind}/connect", wa)
	mux.Handle("DELETE /v1/sources/{id}", wa)
	// PATCH had a handler and a switch arm but no route, so every Pause answered
	// 404 and the screen said "that source is unchanged" — which was true, and
	// unfixable from the UI.
	mux.Handle("PATCH /v1/sources/{id}", wa)
	mux.Handle("GET /v1/status-page", wa)
	mux.Handle("PUT /v1/status-page", wa)
	mux.Handle("GET /v1/logs", wa)
	mux.Handle("POST /v1/logs/explain", wa)
	// The preview composes the same prompt without the model call — dev
	// observability for the prompt-editing loop (the front logs it at
	// dispatch time, see previewExplain).
	mux.Handle("POST /v1/logs/explain/preview", wa)
	mux.Handle("GET /v1/incidents/{id}", wa)
	// The incident's own explain (incident-triage-honesty T5): the server
	// owns the evidence, so the request is just the id. One more segment and
	// a different method than the GET above, so the 1.22 mux never overlaps
	// them; ServeHTTP's dispatch separates them the same way.
	mux.Handle("POST /v1/incidents/{id}/explain", wa)
	// Both had a handler and a dispatch branch and no route, so they answered a
	// bare 404 — the same hole PATCH /v1/sources sat in. The front calls both
	// from /app/projects.
	mux.Handle("GET /v1/export", wa)
	mux.Handle("DELETE /v1/project", wa)

	// --- Installer endpoints (docs/plans/one-command-install.md) ---
	// Anonymous mint is public (throttled); claim needs a session; install
	// status and the project-spec upload authenticate by the project API key,
	// the way POST /i does.
	inst := api.NewInstall(pgPool, chConn, sm, d.Config.PublicOrigin, d.Config.SelfHosted)
	mux.Handle("POST /v1/projects/anonymous", inst)
	mux.Handle("POST /v1/claim", inst)
	mux.Handle("GET /v1/install/status", inst)
	mux.Handle("POST /v1/install/token", inst)
	mux.Handle("POST /v1/install/redeem", inst)
	mux.Handle("PUT /v1/project/meta", inst)

	// --- Public endpoints (no auth) ---
	mux.Handle("POST /public/check", wa)
	mux.Handle("POST /public/watch", wa)
	mux.Handle("POST /public/track", wa)
	mux.Handle("GET /public/status/{slug}", wa)

	// --- ProbeService (connect-go gRPC for the probe fleet) ---
	lc := incident.New(pgPool, chConn)
	probeSvc := rpc.NewProbeService(pgPool, chConn, lc, d.Config.NodeToken)
	probePath, probeHandler := probev1connect.NewProbeServiceHandler(probeSvc)
	mux.Handle(probePath, probeHandler)

	// --- Webhook handlers (Stripe/GitHub/Vercel → events table) ---
	whSecrets := map[string][]byte{
		"stripe": []byte(os.Getenv("UC_STRIPE_WEBHOOK_SECRET")),
		"github": []byte(os.Getenv("UC_GITHUB_WEBHOOK_SECRET")),
		"vercel": []byte(os.Getenv("UC_VERCEL_WEBHOOK_SECRET")),
	}
	whHandler := webhook.New(pgPool, chConn, whSecrets)
	mux.Handle("POST /hooks/", whHandler)
	mux.Handle("POST /hooks/{provider}", whHandler)

	// --- Telegram bot (long-poll under advisory lock) ---
	// Uses UC_TELEGRAM_BOT_TOKEN, NOT UC_NODE_TOKEN — these are different
	// secrets: the node token authenticates the probe fleet, the bot token
	// authenticates the Telegram Bot API. The launcher below polls for a
	// token once a minute, so one pasted into Settings starts the bot with
	// no restart; a CHANGED token still needs one — the loop binds the token
	// it started with.
	go func() {
		for {
			if t := tgToken(ctx); t != "" {
				bot := telegram.NewBot(t, d.Config.PublicOrigin, pgPool, lc, d.Logger)
				if err := bot.Run(ctx); err != nil {
					d.Logger.Warn("telegram bot stopped", "err", err)
				}
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Minute):
			}
		}
	}()

	// NOTE: delivery worker lives in ucworker, NOT ucapi. Two ucapi replicas
	// with an inline worker would duplicate deliveries. The HA test (make test-ha)
	// verifies this. ucapi only enqueues deliveries (via the incident lifecycle);
	// ucworker's delivery worker processes the queue.

	d.Logger.Info("routes mounted",
		"ingest", "POST /i",
		"auth", "magic-link+me+logout",
		"api", "monitors+read+keys",
		"probe", "Lease/Submit/Blind",
		"hooks", "stripe/github/vercel",
		"telegram", "bot")
	return nil
}

// driveBatcher Ticks the batcher on a short cadence so aged batches flush within
// the 200 ms window. Runs until ctx is cancelled (graceful shutdown).
func driveBatcher(ctx context.Context, b *api.Batcher, every time.Duration, log *slog.Logger) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := b.Tick(ctx); err != nil {
				log.Warn("batcher tick error", "err", err)
			}
		}
	}
}

// st is a shorthand for building a shutdown.Task from a name + stop func.
func st(name string, stop func(context.Context) error) shutdown.Task {
	return shutdown.Task{Name: name, Stop: stop}
}
