// Command ucapi is the public-facing service: the OpenAPI HTTP API (/v1/*), the
// public surface (/public/*), the schemaless ingest (POST /i) and ProbeService.
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
	// Teardown stops the recorder BEFORE closing pools: Shutdown runs tasks
	// concurrently, and the final flush would race closed pools otherwise.
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

	ingester, batch, err := api.WireIngest(d.Config.SpoolDir, pgPool, chConn)
	if err != nil {
		_ = chConn.Close()
		pgPool.Close()
		return err
	}
	mux.Handle("POST /i", http.HandlerFunc(ingester.Handle))
	// The endpoint the docs promise: the SAME pipeline under the name agents
	// read in /docs/api. An alias, not a second ingest.
	mux.Handle("POST /v1/event", http.HandlerFunc(ingester.Handle))
	go driveBatcher(ctx, batch, 50*time.Millisecond, d.Logger)
	d.Shutdown.Register(st("batcher", func(c context.Context) error { return batch.Close(c) }))

	// Product analytics recorder: async, buffered, never on the response
	// path. Its drain is sequenced into the pg/ch teardown tasks above.
	recorder = analytics.NewRecorder(analytics.PoolStore{Pool: pgPool}, chConn, d.Logger)
	recorder.Start()

	// Instance-settable secrets: values are sealed under UC_SECRET_KEY_HEX
	// before landing in Postgres; a UI-set value wins over the env one.
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

	sm := session.New(pgPool, 0, d.Logger)
	if d.Config.UCAuth == "none" {
		// Single-user mode: provision the owner at boot and pin every request
		// to that identity; signing in changes nothing.
		personID, tenantID, err := auth.Provision(ctx, pgPool, d.Config.OwnerEmail, "", recorder, d.Config.SelfHosted)
		if err != nil {
			d.Logger.Error("single-user mode: owner provisioning failed", "err", err)
			os.Exit(1)
		}
		sm.WithFixedIdentity(personID, tenantID)
		d.Logger.Warn("auth disabled (UC_AUTH=none): every request acts as the owner", "email", d.Config.OwnerEmail)
	}
	devMode := !d.Config.IsProd()
	// Without a mailer the code is stored and never sent: outside dev mode
	// nobody can sign in. Configured = required, and it fails at boot.
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
		// SMTP resolves per message (a Settings relay wins over UC_SMTP_*).
		// UC_SMTP_HOST with no From is still a boot-time config error.
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
	// Google sign-in, mounted whether or not configured: unconfigured it
	// answers 503, which the sign-in page reads to decide the button.
	googleAuth := auth.NewGoogle(pgPool, sm, d.Config.GoogleClientID, d.Config.GoogleClientSecret,
		d.Config.GoogleRedirectURIs, devMode, recorder, d.Logger).WithSelfHosted(d.Config.SelfHosted)
	mux.Handle("POST /v1/auth/google", googleAuth)
	if googleAuth.Configured() {
		d.Logger.Info("auth: google sign-in enabled", "redirect_uris", d.Config.GoogleRedirectURIs)
	}
	// Telegram Mini App sign-in: the bot token IS the HMAC key; without a bot
	// the endpoint stays a 501. A Settings token arrives on the next restart.
	mux.Handle("POST /v1/auth/telegram", auth.NewTelegramMiniApp(pgPool, sm, tgToken(ctx), devMode))

	mon := api.NewMonitors(pgPool, sm)
	mux.Handle("GET /v1/monitors", mon)
	mux.Handle("POST /v1/monitors", mon)
	mux.Handle("PATCH /v1/monitors/{id}", mon)
	// The docs' word for a monitor is "check", so that path answers too.
	// Same handler: the mux binds {id}; ServeHTTP ignores the prefix.
	mux.Handle("PATCH /v1/checks/{id}", mon)
	mux.Handle("DELETE /v1/monitors/{id}", mon)

	rd := api.NewReadAPI(pgPool, chConn, sm, tgUsername)
	mux.Handle("GET /v1/plan", rd)
	mux.Handle("GET /v1/sources", rd)
	mux.Handle("GET /v1/channels", rd)
	mux.Handle("GET /v1/recipients", rd)
	mux.Handle("GET /v1/incidents", rd)
	mux.Handle("GET /v1/overview", rd)

	keys := api.NewKeys(pgPool, sm)
	mux.Handle("GET /v1/keys", keys)
	mux.Handle("POST /v1/keys/rotate", keys)

	// Instance settings (self-host only; the hosted cloud answers 404): the
	// Settings fields write here, sealed before storage.
	instSettings := api.NewInstanceSettings(pgPool, sm, d.Config.SelfHosted, sealFn)
	mux.Handle("PUT /v1/instance/ai", instSettings)
	mux.Handle("DELETE /v1/instance/ai", instSettings)
	mux.Handle("PUT /v1/instance/telegram-bot", instSettings)
	mux.Handle("DELETE /v1/instance/telegram-bot", instSettings)
	mux.Handle("PUT /v1/instance/smtp", instSettings)
	mux.Handle("DELETE /v1/instance/smtp", instSettings)

	// Explain has ONE brain, OpenAI-compatible: a Settings-set key wins over
	// UC_AI_API_KEY; no key anywhere means 503, never a canned fallback.
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
	// The gear's notification settings. The mux route is half the wiring:
	// without it the PATCH answered 405 with the handler unreachable.
	mux.Handle("PATCH /v1/channels/{id}", wa)
	mux.Handle("DELETE /v1/channels/{id}", wa)
	mux.Handle("POST /v1/channels/{id}/test", wa)
	// The outcome half of Send test: the queue row's own state, not a promise at
	// enqueue time.
	mux.Handle("GET /v1/deliveries/{id}", wa)
	mux.Handle("POST /v1/recipients", wa)
	mux.Handle("PATCH /v1/recipients/{id}", wa)
	mux.Handle("DELETE /v1/recipients/{id}", wa)
	// Telegram invite links (one-time inv_<token>, plan-gated). Session-authed;
	// notify-role members get 403: inviting people is a settings act.
	tginv := api.NewTelegram(pgPool, sm, tgUsername)
	mux.Handle("POST /v1/telegram/invites", tginv)
	mux.Handle("DELETE /v1/telegram/invites/{id}", tginv)
	mux.Handle("POST /v1/sources/{kind}/connect", wa)
	mux.Handle("DELETE /v1/sources/{id}", wa)
	mux.Handle("PATCH /v1/sources/{id}", wa)
	mux.Handle("GET /v1/status-page", wa)
	mux.Handle("PUT /v1/status-page", wa)
	mux.Handle("GET /v1/logs", wa)
	mux.Handle("POST /v1/logs/explain", wa)
	// The preview composes the same prompt without the model call: dev
	// observability for the prompt-editing loop.
	mux.Handle("POST /v1/logs/explain/preview", wa)
	mux.Handle("GET /v1/incidents/{id}", wa)
	// The incident's own explain: the request is just the id. One more
	// segment and a different method than the GET above: no mux overlap.
	mux.Handle("POST /v1/incidents/{id}/explain", wa)
	mux.Handle("GET /v1/export", wa)
	mux.Handle("DELETE /v1/project", wa)
	// The projects axis: list/create behind the session door, and the
	// switcher's write. Same handler, mounted next to its sibling route.
	mux.Handle("GET /v1/projects", wa)
	mux.Handle("POST /v1/projects", wa)
	mux.Handle("POST /v1/project/switch", wa)

	// Installer endpoints: anonymous mint is public (throttled); claim needs
	// a session; status and spec upload authenticate by the project API key.
	inst := api.NewInstall(pgPool, chConn, sm, d.Config.PublicOrigin, d.Config.SelfHosted)
	mux.Handle("POST /v1/projects/anonymous", inst)
	mux.Handle("POST /v1/claim", inst)
	mux.Handle("GET /v1/install/status", inst)
	mux.Handle("POST /v1/install/token", inst)
	mux.Handle("POST /v1/install/redeem", inst)
	mux.Handle("PUT /v1/project/meta", inst)

	mux.Handle("POST /public/check", wa)
	mux.Handle("POST /public/watch", wa)
	mux.Handle("POST /public/track", wa)
	mux.Handle("GET /public/status/{slug}", wa)

	lc := incident.New(pgPool, chConn)
	probeSvc := rpc.NewProbeService(pgPool, chConn, lc, d.Config.NodeToken)
	probePath, probeHandler := probev1connect.NewProbeServiceHandler(probeSvc)
	mux.Handle(probePath, probeHandler)

	whSecrets := map[string][]byte{
		"stripe": []byte(os.Getenv("UC_STRIPE_WEBHOOK_SECRET")),
		"github": []byte(os.Getenv("UC_GITHUB_WEBHOOK_SECRET")),
		"vercel": []byte(os.Getenv("UC_VERCEL_WEBHOOK_SECRET")),
	}
	whHandler := webhook.New(pgPool, chConn, whSecrets)
	mux.Handle("POST /hooks/", whHandler)
	mux.Handle("POST /hooks/{provider}", whHandler)

	// Telegram bot, long-poll under advisory lock. UC_TELEGRAM_BOT_TOKEN is
	// not UC_NODE_TOKEN: different secrets for fleet and Bot API.
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

	// Delivery worker lives in ucworker, NOT ucapi: two ucapi replicas with
	// an inline worker would duplicate deliveries. ucapi only enqueues.

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
