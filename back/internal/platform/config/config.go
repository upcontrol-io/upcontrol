// Package config loads process configuration from the environment. It validates
// at startup and fails loudly when a required value is missing or malformed — a
// process that boots with half its config is worse than one that does not boot.
//
// There is no viper, no file parsing, no defaults hidden in code: the
// environment is the single source. Booleans are true/false (or 1/0); durations
// use time.ParseDuration syntax (e.g. "20s", "5m").
package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config is the merged configuration for a process. Not every field is used by
// every binary; the per-binary Load* functions read the subset they need.
type Config struct {
	HTTPAddr    string
	RPCAddr     string // connect-go mounts on the same HTTP server in production; kept separate for tests
	Environment string // dev|staging|prod

	PostgresURL    string
	ClickHouseAddr string // host:port
	ClickHouseDB   string
	ClickHouseUser string
	ClickHousePass string

	MigrationsPostgresDir   string // path to db/postgres, for `ucapi migrate`
	MigrationsClickHouseDir string

	// Telegram. The token authenticates the Bot API (ucapi long-polls it,
	// ucworker delivers through it); the username is what the deep link points
	// at. Both empty = no bot: the long-poll never starts and the app stops
	// offering Telegram as a destination, rather than offering a dead one.
	TelegramBotToken    string
	TelegramBotUsername string

	// Mailer. EmailURL set = an external email agent sends the mail;
	// empty falls back to the SMTP fields below, and all-empty
	// = no mailer: dev mode still returns the code in the response, prod logs
	// a boot warning. A half-filled config is refused at mailer.NewSMTP — a
	// mailer that accepts mail and drops it is worse than none.
	SMTPHost     string
	SMTPPort     int
	SMTPUsername string
	SMTPPassword string
	SMTPFrom     string
	SMTPFromName string

	// AI provider (OpenAI-format chat completions) behind Explain. With no
	// AIAPIKey anywhere (here or the Settings-set instance key) Explain is
	// off — endpoints answer 503 ai_not_configured — so nothing here is
	// required in prod. The prices are USD per 1M tokens and feed only the
	// per-call cost log line; 0 = unknown.
	AIBaseURL          string
	AIAPIKey           string
	AIModel            string
	AITimeout          time.Duration
	AIInputPricePer1M  float64
	AIOutputPricePer1M float64
	// AILogPrompt echoes the full prompt (system + user) to the log on every
	// call. A prompt-editing loop wants to SEE the exact bytes the model saw;
	// prod leaves it off because logs are forever.
	AILogPrompt bool

	// DetectEnabled gates the ucworker detection job (error-rate incidents).
	// On by default wherever ClickHouse is configured (owner decision,
	// 2026-08-18, reversing D8's ship-dark default); UC_DETECT_ENABLED=0 is
	// the kill switch.
	DetectEnabled bool

	// SelfHosted marks a self-hosted install (UC_SELF_HOSTED=1): tenants
	// created by ANY door land on the 'Self-hosted' plan instead of 'Free'
	// (Decision 7).
	SelfHosted bool

	// UCAuth selects the sign-in door: "magic-link" (default) or "none" —
	// single-user mode where every request acts as the boot-provisioned
	// OwnerEmail account (Decision 16).
	UCAuth     string
	OwnerEmail string

	// Email agent: its base URL and the bearer key its
	// /send demands. URL empty = agent unused, SMTP above.
	EmailURL    string
	EmailAPIKey string

	LogLevel  string
	LogFormat string // json (prod) | text (dev)

	// Warnings are non-fatal misconfigurations (e.g. a *_FILE secret path
	// that cannot be read) collected during Load, before the process logger
	// exists; app.Run replays them through the real logger right after
	// building it. Load returning nil error does not mean this is empty.
	Warnings []string

	ShutdownTimeout time.Duration

	// Ingest
	SpoolDir       string
	SpoolMaxBytes  int64
	WALFsyncEvery  int // rows per group fsync
	BatchBytes     int
	BatchAge       time.Duration
	MinFlushPerSec int // max 1 flush / sec per (table × bucket); 0 disables

	// Probe / fleet
	NodeToken string // shared secret a probe presents to Lease/SubmitResults

	// Encryption key for secret_enc / token_enc columns (32 raw bytes, hex).
	SecretKeyHex string

	// Public site
	PublicOrigin string // https://upcontrol.io

	// Google sign-in. Empty client id or secret leaves the door answering 503
	// and saying so; the magic link is unaffected either way.
	GoogleClientID     string
	GoogleClientSecret string
	// The exact redirect_uri values a code may be exchanged for. More than one
	// because the app is served from more than one origin (the container, the
	// dev server), and Google matches the value character for character.
	// Defaults to PublicOrigin + /sign-in.
	GoogleRedirectURIs []string
}

// Load reads the environment into Config. Required must all be present; missing
// or invalid ones are aggregated into one error so the operator sees the whole
// list, not one at a time. service is the binary name: in prod the database
// and secret-key requirements do not apply to ucprobe, which by invariant 1
// holds no database credentials.
func Load(service string) (Config, error) {
	var c Config
	var errs []string

	c.Environment = getenv("UC_ENVIRONMENT", "dev")
	c.HTTPAddr = getenv("UC_HTTP_ADDR", ":8080")
	c.RPCAddr = getenv("UC_RPC_ADDR", c.HTTPAddr)

	c.PostgresURL = os.Getenv("UC_POSTGRES_URL")
	// If the URL has no password but UC_POSTGRES_PASSWORD(_FILE) is set, inject
	// it into the URL. This is how compose passes the secret without leaking it
	// in docker inspect.
	if pgPass := getenvOrFile("UC_POSTGRES_PASSWORD", &c.Warnings); pgPass != "" && c.PostgresURL != "" {
		c.PostgresURL = injectPgPassword(c.PostgresURL, pgPass)
	}
	c.ClickHouseAddr = getenv("UC_CLICKHOUSE_ADDR", "localhost:9000")
	c.ClickHouseDB = getenv("UC_CLICKHOUSE_DB", "upcontrol")
	c.ClickHouseUser = getenv("UC_CLICKHOUSE_USER", "default")
	c.ClickHousePass = getenvOrFile("UC_CLICKHOUSE_PASSWORD", &c.Warnings)

	c.TelegramBotToken = getenvOrFile("UC_TELEGRAM_BOT_TOKEN", &c.Warnings)
	c.TelegramBotUsername = strings.TrimPrefix(getenv("UC_TELEGRAM_BOT_USERNAME", ""), "@")

	c.SMTPHost = getenv("UC_SMTP_HOST", "")
	c.SMTPPort = getenvInt("UC_SMTP_PORT", 587, &errs)
	c.SMTPUsername = getenv("UC_SMTP_USERNAME", "")
	c.SMTPPassword = getenvOrFile("UC_SMTP_PASSWORD", &c.Warnings)
	c.SMTPFrom = getenv("UC_SMTP_FROM", "")
	c.SMTPFromName = getenv("UC_SMTP_FROM_NAME", "UpControl")

	c.AIBaseURL = getenv("UC_AI_BASE_URL", "https://api.openai.com/v1")
	c.AIAPIKey = getenvOrFile("UC_AI_API_KEY", &c.Warnings)
	c.AIModel = getenv("UC_AI_MODEL", "gpt-5-nano-2025-08-07")
	c.AITimeout = getenvDuration("UC_AI_TIMEOUT", 60*time.Second, &errs)
	c.AILogPrompt = os.Getenv("UC_AI_LOG_PROMPT") == "1"
	c.DetectEnabled = os.Getenv("UC_DETECT_ENABLED") != "0"
	c.SelfHosted = os.Getenv("UC_SELF_HOSTED") == "1"
	c.UCAuth = getenv("UC_AUTH", "magic-link")
	if c.UCAuth != "magic-link" && c.UCAuth != "none" {
		// A typo here must not boot: "non"/"disabled" silently leaving the
		// magic-link door open is the wrong kind of surprise for an operator
		// who believes auth is off.
		errs = append(errs, fmt.Sprintf("UC_AUTH: must be magic-link or none (%q)", c.UCAuth))
	}
	c.OwnerEmail = getenv("UC_OWNER_EMAIL", "owner@localhost")
	c.AIInputPricePer1M = getenvFloat("UC_AI_INPUT_PRICE_PER_1M", 0, &errs)
	c.AIOutputPricePer1M = getenvFloat("UC_AI_OUTPUT_PRICE_PER_1M", 0, &errs)

	c.EmailURL = getenv("UC_EMAIL_URL", "")
	c.EmailAPIKey = getenvOrFile("UC_EMAIL_API_KEY", &c.Warnings)

	c.MigrationsPostgresDir = getenv("UC_PG_MIGRATIONS_DIR", "../../db/postgres")
	c.MigrationsClickHouseDir = getenv("UC_CH_MIGRATIONS_DIR", "../../db/clickhouse")

	c.LogLevel = getenv("UC_LOG_LEVEL", "info")
	c.LogFormat = getenv("UC_LOG_FORMAT", c.logFormatDefault())
	c.ShutdownTimeout = getenvDuration("UC_SHUTDOWN_TIMEOUT", 20*time.Second, &errs)

	c.SpoolDir = getenv("UC_SPOOL_DIR", "./spool")
	c.SpoolMaxBytes = getenvInt64("UC_SPOOL_MAX_BYTES", 1<<30, &errs) // 1 GiB
	c.WALFsyncEvery = getenvInt("UC_WAL_FSYNC_EVERY", 256, &errs)
	c.BatchBytes = getenvInt("UC_BATCH_BYTES", 8<<20, &errs) // 8 MiB
	c.BatchAge = getenvDuration("UC_BATCH_AGE", 200*time.Millisecond, &errs)
	c.MinFlushPerSec = getenvInt("UC_MIN_FLUSH_PER_SEC", 1, &errs)

	c.NodeToken = getenvOrFile("UC_NODE_TOKEN", &c.Warnings)
	c.SecretKeyHex = getenvOrFile("UC_SECRET_KEY_HEX", &c.Warnings)
	c.PublicOrigin = getenv("UC_PUBLIC_ORIGIN", "http://localhost:5173")

	c.GoogleClientID = getenv("UC_GOOGLE_CLIENT_ID", "")
	c.GoogleClientSecret = getenvOrFile("UC_GOOGLE_CLIENT_SECRET", &c.Warnings)
	// The default is the one origin we always know. A deployment served from a
	// second one (the dev server on :5173 beside the container on :80) lists
	// both, comma-separated, because Google compares the value verbatim.
	c.GoogleRedirectURIs = splitList(getenv("UC_GOOGLE_REDIRECT_URIS",
		strings.TrimRight(c.PublicOrigin, "/")+"/sign-in"))

	// Validate required production values. In dev we allow missing DB URLs so the
	// skeleton can boot without infrastructure (Phase 1).
	if c.Environment == "prod" {
		if service != "ucprobe" {
			require(&errs, c.PostgresURL != "", "UC_POSTGRES_URL")
			require(&errs, c.ClickHouseAddr != "", "UC_CLICKHOUSE_ADDR")
		}
		require(&errs, c.NodeToken != "", "UC_NODE_TOKEN")
	}
	if c.SecretKeyHex != "" {
		if _, err := SecretKeyFromHex(c.SecretKeyHex); err != nil {
			errs = append(errs, fmt.Sprintf("UC_SECRET_KEY_HEX: %v", err))
		}
	}

	if len(errs) > 0 {
		return c, errors.New("config: invalid environment:\n  - " + strings.Join(errs, "\n  - "))
	}
	return c, nil
}

func (c Config) logFormatDefault() string {
	if c.Environment == "dev" {
		return "text"
	}
	return "json"
}

// IsProd reports whether the process runs in production mode.
func (c Config) IsProd() bool { return c.Environment == "prod" }

// helpers ------------------------------------------------------------------

func getenv(k, def string) string {
	if v, ok := os.LookupEnv(k); ok {
		return v
	}
	return def
}

// getenvOrFile resolves a secret value from the direct env var or, failing
// that, its *_FILE companion (a Docker-secrets-style path whose contents are
// the value). The file path is trimmed of surrounding whitespace; the value is
// returned verbatim otherwise. This is how compose mounts UC_*_FILE secrets.
// splitList reads a comma-separated setting. Empty entries are dropped rather
// than kept as "", which would otherwise become an allowlist entry that matches
// a caller who sent no value at all.
func splitList(v string) []string {
	out := []string{}
	for _, part := range strings.Split(v, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

func getenvOrFile(key string, warns *[]string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	if path := os.Getenv(key + "_FILE"); path != "" {
		b, err := os.ReadFile(path)
		if err != nil {
			// Setting <KEY>_FILE is the operator declaring the secret should
			// be mounted. Compose does NOT fail a stack whose secret file is
			// missing — it warns on the host and mounts an empty directory —
			// so every read failure here (missing, unreadable, wrong shape)
			// is collected as a warning instead of silently becoming
			// "unset". Silence belongs to the var not being set at all.
			*warns = append(*warns, fmt.Sprintf("%s_FILE: cannot read %q: %v", key, path, err))
			return ""
		}
		return strings.TrimSpace(string(b))
	}
	return ""
}

func getenvInt(k string, def int, errs *[]string) int {
	v := os.Getenv(k)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		*errs = append(*errs, fmt.Sprintf("%s: not an int (%q)", k, v))
		return def
	}
	return n
}

func getenvInt64(k string, def int64, errs *[]string) int64 {
	v := os.Getenv(k)
	if v == "" {
		return def
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		*errs = append(*errs, fmt.Sprintf("%s: not an int (%q)", k, v))
		return def
	}
	return n
}

func getenvFloat(k string, def float64, errs *[]string) float64 {
	v := os.Getenv(k)
	if v == "" {
		return def
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		*errs = append(*errs, fmt.Sprintf("%s: not a float (%q)", k, v))
		return def
	}
	return f
}

func getenvDuration(k string, def time.Duration, errs *[]string) time.Duration {
	v := os.Getenv(k)
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		*errs = append(*errs, fmt.Sprintf("%s: not a duration (%q)", k, v))
		return def
	}
	return d
}

func require(errs *[]string, ok bool, name string) {
	if !ok {
		*errs = append(*errs, "missing required "+name)
	}
}

// injectPgPassword inserts a password into a postgres:// URL that has none.
// "postgres://user@host/db" → "postgres://user:pass@host/db".
func injectPgPassword(url, password string) string {
	// If the URL already has a password (contains ":" before "@"), leave it.
	atIdx := strings.Index(url, "@")
	if atIdx < 0 {
		return url
	}
	schemeEnd := strings.Index(url, "://")
	if schemeEnd < 0 {
		return url
	}
	userPart := url[schemeEnd+3 : atIdx]
	if strings.Contains(userPart, ":") {
		return url // already has a password
	}
	return url[:schemeEnd+3] + userPart + ":" + password + url[atIdx:]
}
