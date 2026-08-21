// The self-host UI's instance-settings door: the AI API key and the Telegram
// bot (token + username) can be pasted into Settings instead of secret files
// (owner decision, 2026-08-20 — the same day the heuristic fallback died).
// Values are sealed (AES-256-GCM under UC_SECRET_KEY_HEX) before they land in
// instance_setting, and never travel back out: presence is read through the
// explain preview's `model` field and the channels screen's telegram surface,
// never a GET that could echo a secret.
//
// The door only exists on a self-host: the hosted cloud's keys are secret
// files owned by the operator, and an instance-level knob writable from a
// tenant session on a multi-tenant deployment would let any tenant steer
// everyone's brain — so off a self-host the answer is 404, the same "this
// door does not exist" the anonymous mint gives (Decision 22's shape).

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	sqlc "go.upcontrol.io/back/gen/pg"
	"go.upcontrol.io/back/internal/account/session"
	"go.upcontrol.io/back/internal/storage/pg"
)

// validRelayHost reports whether s is a bare hostname we would be willing to
// hand to a mail dialer: dot-separated labels, each 1-63 characters of letters,
// digits and inner hyphens. No scheme, no port, no path, no empty labels.
//
// It exists because the first version of this check only refused whitespace and
// slashes, so "bad..host..name" was stored as a relay and the UI reported it
// saved — the contract listed invalid_host as a refusal that nothing could
// actually trigger (rehearsal #3, 2026-08-20). Nothing else on the SMTP form
// can catch this: the dialer only finds out at the first send, hours later,
// with the sign-in mail nobody received.
//
// Deliberately permissive in two directions. A bare "localhost" passes: on a
// self-host the relay is often on the same box. An IPv4 literal passes on the
// label rules alone, which is the right answer for a relay on a private
// network. Unicode is rejected — a relay hostname reaches the wire as ASCII,
// and refusing here beats a punycode surprise at send time.
func validRelayHost(s string) bool {
	if s == "" || len(s) > 255 {
		return false
	}
	for _, label := range strings.Split(s, ".") {
		if len(label) == 0 || len(label) > 63 {
			return false
		}
		if label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for i := 0; i < len(label); i++ {
			c := label[i]
			switch {
			case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-':
			default:
				return false
			}
		}
	}
	return true
}

const (
	aiKeySetting      = "ai_api_key"
	aiModelSetting    = "ai_model"
	aiBaseURLSetting  = "ai_base_url"
	tgTokenSetting    = "telegram_bot_token"
	tgUsernameSetting = "telegram_bot_username"
	smtpHostSetting   = "smtp_host"
	smtpPortSetting   = "smtp_port"
	smtpUserSetting   = "smtp_username"
	smtpPassSetting   = "smtp_password"
	smtpFromSetting   = "smtp_from"
)

type InstanceSettings struct {
	pool       *pg.Pool
	sess       *session.Manager
	selfHosted bool
	// seal encrypts a value for instance_setting.value_enc; nil means
	// UC_SECRET_KEY_HEX is absent and the door refuses rather than storing
	// a secret in plaintext.
	seal func([]byte) ([]byte, error)
}

func NewInstanceSettings(pool *pg.Pool, sm *session.Manager, selfHosted bool, seal func([]byte) ([]byte, error)) *InstanceSettings {
	return &InstanceSettings{pool: pool, sess: sm, selfHosted: selfHosted, seal: seal}
}

func (h *InstanceSettings) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !h.selfHosted {
		writeAPIErr(w, http.StatusNotFound, "not_found")
		return
	}
	ctx := r.Context()
	s, err := h.sess.FromRequest(ctx, r)
	if err != nil {
		writeAPIErr(w, http.StatusUnauthorized, "no_session")
		return
	}
	// Changing the instance's brain is a settings act, not a notify-role one.
	if !roleAtLeastLogin(ctx, h.pool, s.PersonID, s.TenantID) {
		writeAPIErr(w, http.StatusForbidden, "notify_role")
		return
	}

	switch {
	case r.URL.Path == "/v1/instance/ai" && r.Method == http.MethodPut:
		// All three knobs of the OpenAI-compatible client, each optional so a
		// model change never demands re-pasting the key. Only what is sent
		// (non-empty) is stored; DELETE resets the lot to env config.
		var req struct {
			Key     string `json:"key"`
			Model   string `json:"model"`
			BaseURL string `json:"baseUrl"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<14)).Decode(&req); err != nil {
			writeAPIErr(w, http.StatusBadRequest, "bad_json")
			return
		}
		values := map[string]string{}
		if key := strings.TrimSpace(req.Key); key != "" {
			if len(key) < 8 || len(key) > 512 || strings.ContainsAny(key, " \t\r\n") {
				writeAPIErrMsg(w, http.StatusBadRequest, "invalid_key",
					"Paste an OpenAI-format API key (like sk-...). Any OpenAI-compatible provider's key works.")
				return
			}
			values[aiKeySetting] = key
		}
		if model := strings.TrimSpace(req.Model); model != "" {
			if len(model) > 128 || strings.ContainsAny(model, " \t\r\n") {
				writeAPIErrMsg(w, http.StatusBadRequest, "invalid_model",
					"The model name as the provider spells it, e.g. gpt-4o-mini.")
				return
			}
			values[aiModelSetting] = model
		}
		if base := strings.TrimSpace(req.BaseURL); base != "" {
			u, uerr := url.Parse(base)
			if uerr != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || len(base) > 512 {
				writeAPIErrMsg(w, http.StatusBadRequest, "invalid_base_url",
					"A full URL like https://api.openai.com/v1 — any OpenAI-compatible proxy or gateway.")
				return
			}
			values[aiBaseURLSetting] = base
		}
		if len(values) == 0 {
			writeAPIErrMsg(w, http.StatusBadRequest, "nothing_to_store",
				"Send at least one of key, model, baseUrl.")
			return
		}
		h.store(w, ctx, values)
	case r.URL.Path == "/v1/instance/ai" && r.Method == http.MethodDelete:
		h.remove(w, ctx, aiKeySetting, aiModelSetting, aiBaseURLSetting)

	case r.URL.Path == "/v1/instance/telegram-bot" && r.Method == http.MethodPut:
		var req struct {
			Token    string `json:"token"`
			Username string `json:"username"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<14)).Decode(&req); err != nil {
			writeAPIErr(w, http.StatusBadRequest, "bad_json")
			return
		}
		token := strings.TrimSpace(req.Token)
		username := strings.TrimPrefix(strings.TrimSpace(req.Username), "@")
		// A BotFather token is "<digits>:<35-char secret>"; the exact tail
		// format is Telegram's to change, so only the shape is checked.
		if token == "" || !strings.Contains(token, ":") || strings.ContainsAny(token, " \t\r\n") {
			writeAPIErrMsg(w, http.StatusBadRequest, "invalid_token",
				"Paste the bot token exactly as @BotFather printed it (digits, a colon, then the secret).")
			return
		}
		if username == "" || strings.ContainsAny(username, " \t\r\n/") {
			writeAPIErrMsg(w, http.StatusBadRequest, "invalid_username",
				"The bot's username (without @) makes the t.me links — it is on the bot's BotFather page.")
			return
		}
		h.store(w, ctx, map[string]string{tgTokenSetting: token, tgUsernameSetting: username})
	case r.URL.Path == "/v1/instance/telegram-bot" && r.Method == http.MethodDelete:
		h.remove(w, ctx, tgTokenSetting, tgUsernameSetting)

	case r.URL.Path == "/v1/instance/smtp" && r.Method == http.MethodPut:
		// The relay for sign-in mail and email alerts. Each field optional (the
		// AI door's shape) so changing the password never demands re-typing the
		// host; DELETE resets the lot to env config (UC_SMTP_*).
		var req struct {
			Host     string `json:"host"`
			Port     string `json:"port"`
			Username string `json:"username"`
			Password string `json:"password"`
			From     string `json:"from"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<14)).Decode(&req); err != nil {
			writeAPIErr(w, http.StatusBadRequest, "bad_json")
			return
		}
		values := map[string]string{}
		if host := strings.TrimSpace(req.Host); host != "" {
			if !validRelayHost(host) {
				writeAPIErrMsg(w, http.StatusBadRequest, "invalid_host",
					"The relay's hostname alone, like smtp.eu.mailgun.org — no scheme, no port.")
				return
			}
			values[smtpHostSetting] = host
		}
		if port := strings.TrimSpace(req.Port); port != "" {
			if n, perr := strconv.Atoi(port); perr != nil || n < 1 || n > 65535 {
				writeAPIErrMsg(w, http.StatusBadRequest, "invalid_port",
					"A port number, usually 587 (STARTTLS) or 465.")
				return
			}
			values[smtpPortSetting] = port
		}
		if user := strings.TrimSpace(req.Username); user != "" {
			if len(user) > 255 || strings.ContainsAny(user, "\r\n") {
				writeAPIErrMsg(w, http.StatusBadRequest, "invalid_username",
					"The relay login, often the sending address itself.")
				return
			}
			values[smtpUserSetting] = user
		}
		if pass := strings.TrimSpace(req.Password); pass != "" {
			if len(pass) > 512 || strings.ContainsAny(pass, "\r\n") {
				writeAPIErrMsg(w, http.StatusBadRequest, "invalid_password",
					"The relay password or app password, as the provider issued it.")
				return
			}
			values[smtpPassSetting] = pass
		}
		if from := strings.TrimSpace(req.From); from != "" {
			if len(from) > 255 || !strings.Contains(from, "@") || strings.ContainsAny(from, " \t\r\n") {
				writeAPIErrMsg(w, http.StatusBadRequest, "invalid_from",
					"The From address, like alerts@example.com — most relays insist it is one they verified.")
				return
			}
			values[smtpFromSetting] = from
		}
		if len(values) == 0 {
			writeAPIErrMsg(w, http.StatusBadRequest, "nothing_to_store",
				"Send at least one of host, port, username, password, from.")
			return
		}
		h.store(w, ctx, values)
	case r.URL.Path == "/v1/instance/smtp" && r.Method == http.MethodDelete:
		h.remove(w, ctx, smtpHostSetting, smtpPortSetting, smtpUserSetting, smtpPassSetting, smtpFromSetting)

	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

// store seals and upserts each setting; one refusal covers the lot because a
// half-written pair (a token without its username) is a broken deep link.
func (h *InstanceSettings) store(w http.ResponseWriter, ctx context.Context, values map[string]string) {
	if h.seal == nil {
		writeAPIErrMsg(w, http.StatusServiceUnavailable, "secret_key_missing",
			"This instance has no UC_SECRET_KEY_HEX, so it cannot store secrets encrypted. Set it (install.sh generates one) and retry.")
		return
	}
	for key, value := range values {
		enc, err := h.seal([]byte(value))
		if err != nil {
			writeAPIErr(w, http.StatusInternalServerError, "internal")
			return
		}
		if err := h.pool.Queries().UpsertInstanceSetting(ctx, sqlc.UpsertInstanceSettingParams{Key: key, ValueEnc: enc}); err != nil {
			writeAPIErr(w, http.StatusInternalServerError, "internal")
			return
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *InstanceSettings) remove(w http.ResponseWriter, ctx context.Context, keys ...string) {
	for _, key := range keys {
		if err := h.pool.Queries().DeleteInstanceSetting(ctx, key); err != nil {
			writeAPIErr(w, http.StatusInternalServerError, "internal")
			return
		}
	}
	w.WriteHeader(http.StatusNoContent)
}
