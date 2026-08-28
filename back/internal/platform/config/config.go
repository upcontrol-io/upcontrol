// Package config loads process configuration from the environment and fails
// loudly at startup: the environment is the single source, no hidden defaults.
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
	Environment string // dev|staging|prod

	PostgresURL string

	MigrationsPostgresDir string // path to db/postgres, for `ucapi migrate`

	// Telegram. Both empty = no bot: the long-poll never starts and the app
	// stops offering Telegram, rather than offering a dead destination.
	TelegramBotToken    string
	TelegramBotUsername string

	// Mailer. EmailURL set = the email agent sends the mail; empty falls back
	// to the SMTP fields; all-empty = no mailer (dev returns the code, prod warns).
	SMTPHost     string
	SMTPPort     int
	SMTPUsername string
	SMTPPassword string
	SMTPFrom     string
	SMTPFromName string

	// DetectEnabled gates the ucworker detection job (error-rate incidents);
	// on by default, UC_DETECT_ENABLED=0 is the kill switch.
	DetectEnabled bool

	// ScrubOff disables server-side secret scrubbing (UC_SCRUB=0), and is
	// honoured ONLY on a self-hosted instance. Negative on purpose: the zero
	// value has to be "scrub", so a caller that forgets the field redacts.
	ScrubOff bool

	// SelfHosted marks a self-hosted install (UC_SELF_HOSTED=1): new tenants
	// land on the 'Self-hosted' plan instead of 'Free'.
	SelfHosted bool

	// WithWorker runs the background jobs inside ucapi (--with-worker, or
	// UC_WITH_WORKER=1). Defaults ON for a self-hosted install, where one
	// container is the point, and OFF otherwise, where ucworker runs separately.
	WithWorker bool

	// UCAuth selects the sign-in door: "magic-link" (default) or "none" —
	// single-user mode acting as the boot-provisioned OwnerEmail account.
	UCAuth     string
	OwnerEmail string

	// Email agent: its base URL and the bearer key its
	// /send demands. URL empty = agent unused, SMTP above.
	EmailURL    string
	EmailAPIKey string

	LogLevel  string
	LogFormat string // json (prod) | text (dev)

	// Warnings are non-fatal misconfigurations collected during Load, before
	// the logger exists; app.Run replays them. A nil error does not mean empty.
	Warnings []string

	ShutdownTimeout time.Duration

	// Ingest
	SpoolDir string

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
	// The exact redirect_uri values a code may be exchanged for; Google
	// matches character for character. Defaults to PublicOrigin + /sign-in.
	GoogleRedirectURIs []string
}

// Load reads the environment into Config, aggregating missing/invalid values
// into one error; ucprobe is exempt from database requirements (no credentials).
func Load(service string) (Config, error) {
	var c Config
	var errs []string

	c.Environment = getenv("UC_ENVIRONMENT", "dev")
	c.HTTPAddr = getenv("UC_HTTP_ADDR", ":8080")

	c.PostgresURL = os.Getenv("UC_POSTGRES_URL")
	// If the URL has no password but UC_POSTGRES_PASSWORD(_FILE) is set,
	// inject it: compose passes the secret without leaking it in inspect.
	if pgPass := getenvOrFile("UC_POSTGRES_PASSWORD", &c.Warnings); pgPass != "" && c.PostgresURL != "" {
		c.PostgresURL = injectPgPassword(c.PostgresURL, pgPass)
	}
	c.TelegramBotToken = getenvOrFile("UC_TELEGRAM_BOT_TOKEN", &c.Warnings)
	c.TelegramBotUsername = strings.TrimPrefix(getenv("UC_TELEGRAM_BOT_USERNAME", ""), "@")

	c.SMTPHost = getenv("UC_SMTP_HOST", "")
	c.SMTPPort = getenvInt("UC_SMTP_PORT", 587, &errs)
	c.SMTPUsername = getenv("UC_SMTP_USERNAME", "")
	c.SMTPPassword = getenvOrFile("UC_SMTP_PASSWORD", &c.Warnings)
	c.SMTPFrom = getenv("UC_SMTP_FROM", "")
	c.SMTPFromName = getenv("UC_SMTP_FROM_NAME", "UpControl")

	c.DetectEnabled = os.Getenv("UC_DETECT_ENABLED") != "0"
	c.SelfHosted = os.Getenv("UC_SELF_HOSTED") == "1"
	// Scrubbing is the default and stays mandatory on the hosted service: what
	// it redacts is other people's tokens, which is not an operator's call to
	// make. On their own box it is, so the switch needs UC_SELF_HOSTED with it.
	if os.Getenv("UC_SCRUB") == "0" {
		if c.SelfHosted {
			c.ScrubOff = true
		} else {
			c.Warnings = append(c.Warnings,
				"UC_SCRUB=0 ignored: secret scrubbing is optional only on a self-hosted instance (UC_SELF_HOSTED=1)")
		}
	}
	// UC_WITH_WORKER forces the choice either way; unset keeps the default.
	c.WithWorker = c.SelfHosted
	if v := os.Getenv("UC_WITH_WORKER"); v == "1" || v == "0" {
		c.WithWorker = v == "1"
	}
	c.UCAuth = getenv("UC_AUTH", "magic-link")
	if c.UCAuth != "magic-link" && c.UCAuth != "none" {
		// A typo here must not boot: silently leaving the magic-link door open
		// is the wrong surprise for an operator who believes auth is off.
		errs = append(errs, fmt.Sprintf("UC_AUTH: must be magic-link or none (%q)", c.UCAuth))
	}
	c.OwnerEmail = getenv("UC_OWNER_EMAIL", "owner@localhost")

	c.EmailURL = getenv("UC_EMAIL_URL", "")
	c.EmailAPIKey = getenvOrFile("UC_EMAIL_API_KEY", &c.Warnings)

	c.MigrationsPostgresDir = getenv("UC_PG_MIGRATIONS_DIR", "../../db/postgres")

	c.LogLevel = getenv("UC_LOG_LEVEL", "info")
	c.LogFormat = getenv("UC_LOG_FORMAT", c.logFormatDefault())
	c.ShutdownTimeout = getenvDuration("UC_SHUTDOWN_TIMEOUT", 20*time.Second, &errs)

	c.SpoolDir = getenv("UC_SPOOL_DIR", "./spool")

	c.NodeToken = getenvOrFile("UC_NODE_TOKEN", &c.Warnings)
	c.SecretKeyHex = getenvOrFile("UC_SECRET_KEY_HEX", &c.Warnings)
	c.PublicOrigin = getenv("UC_PUBLIC_ORIGIN", "http://localhost:5173")

	c.GoogleClientID = getenv("UC_GOOGLE_CLIENT_ID", "")
	c.GoogleClientSecret = getenvOrFile("UC_GOOGLE_CLIENT_SECRET", &c.Warnings)
	// The default is the one origin we always know; a deployment served from
	// a second one lists both, comma-separated (Google compares verbatim).
	c.GoogleRedirectURIs = splitList(getenv("UC_GOOGLE_REDIRECT_URIS",
		strings.TrimRight(c.PublicOrigin, "/")+"/sign-in"))

	// Required production values; dev allows missing DB URLs so a skeleton
	// can boot without infrastructure.
	if c.Environment == "prod" {
		if service != "ucprobe" {
			require(&errs, c.PostgresURL != "", "UC_POSTGRES_URL")
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

func getenv(k, def string) string {
	if v, ok := os.LookupEnv(k); ok {
		return v
	}
	return def
}

// splitList reads a comma-separated setting; empty entries are dropped, or an
// allowlist entry would match a caller who sent no value at all.
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
			// Compose does NOT fail a stack whose secret file is missing (it
			// mounts an empty dir), so read failures become warnings, not "unset".
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
