// Telegram Mini App auth: POST /v1/auth/telegram verifies initData's HMAC
// with the bot token and issues the session cookie. No bot token = 501.

package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"go.upcontrol.io/back/internal/account/session"
	"go.upcontrol.io/back/internal/storage/pg"
)

// initDataFreshness bounds replay: Telegram refreshes initData on every WebApp
// open; a day is the forgiving edge.
const initDataFreshness = 24 * time.Hour

// TelegramMiniApp handles POST /v1/auth/telegram.
type TelegramMiniApp struct {
	pool     *pg.Pool
	sess     *session.Manager
	botToken string
	devMode  bool
}

// NewTelegramMiniApp builds the handler. botToken empty means no Telegram bot
// is configured; the handler then answers 501, same as before it existed.
func NewTelegramMiniApp(p *pg.Pool, sm *session.Manager, botToken string, devMode bool) *TelegramMiniApp {
	return &TelegramMiniApp{pool: p, sess: sm, botToken: strings.TrimSpace(botToken), devMode: devMode}
}

func (h *TelegramMiniApp) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if h.botToken == "" {
		NewNotImplemented("Telegram").ServeHTTP(w, r)
		return
	}
	var req struct {
		InitData string `json:"initData"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil || req.InitData == "" {
		writeErr(w, http.StatusBadRequest, "missing_init_data")
		return
	}
	tgUser, ok := VerifyInitData(req.InitData, h.botToken, time.Now())
	if !ok {
		// One refusal for bad signature AND stale auth_date: no oracle.
		writeErr(w, http.StatusUnauthorized, "invalid_init_data")
		return
	}

	ctx := r.Context()
	var personID, tenantID int64
	var name, email string
	if err := h.pool.Raw().QueryRow(ctx,
		`SELECT p.id, tm.tenant_id, coalesce(p.name, ''), coalesce(p.email, '')
		   FROM person p
		   JOIN tenant_member tm ON tm.person_id = p.id
		  WHERE p.telegram_id = $1
		  ORDER BY tm.tenant_id LIMIT 1`, tgUser.ID).Scan(&personID, &tenantID, &name, &email); err != nil {
		// A Telegram account that never redeemed an invite is nobody here.
		writeErr(w, http.StatusUnauthorized, "not_a_member")
		return
	}
	if strings.TrimSpace(name) == "" {
		name = tgUser.FirstName
	}

	sessToken, err := h.sess.Create(ctx, personID, tenantID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}
	session.SetCookie(w, sessToken, session.DefaultTTL, !h.devMode)
	writeJSON(w, http.StatusOK, map[string]any{
		"id":       tgUser.ID,
		"name":     name,
		"email":    email,
		"initials": initials(name, email),
		"plan":     "Free",
		"billing":  "annual",
	})
}

// tgUserLite is the `user` field of initData.
type tgUserLite struct {
	ID        int64  `json:"id"`
	FirstName string `json:"first_name"`
}

// VerifyInitData checks signature and freshness and returns the Telegram user.
// Pure function — unit-tested without a bot or a database.
func VerifyInitData(initData, botToken string, now time.Time) (tgUserLite, bool) {
	vals, err := url.ParseQuery(initData)
	if err != nil {
		return tgUserLite{}, false
	}
	hash := vals.Get("hash")
	if hash == "" {
		return tgUserLite{}, false
	}
	vals.Del("hash")

	// data_check_string: sorted key=value lines joined with \n.
	keys := make([]string, 0, len(vals))
	for k := range vals {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var sb strings.Builder
	for _, k := range keys {
		sb.WriteString(k)
		sb.WriteByte('=')
		sb.WriteString(vals.Get(k))
		sb.WriteByte('\n')
	}
	dataCheckString := strings.TrimSuffix(sb.String(), "\n")

	secret := hmac.New(sha256.New, []byte("WebAppData"))
	secret.Write([]byte(botToken))
	mac := hmac.New(sha256.New, secret.Sum(nil))
	mac.Write([]byte(dataCheckString))
	expected := mac.Sum(nil)
	got, err := hexDecodeHash(hash)
	if err != nil || subtle.ConstantTimeCompare(got, expected) != 1 {
		return tgUserLite{}, false
	}

	// Freshness (replay guard).
	authDate, err := strconv.ParseInt(vals.Get("auth_date"), 10, 64)
	if err != nil || now.Sub(time.Unix(authDate, 0)) > initDataFreshness {
		return tgUserLite{}, false
	}

	var user tgUserLite
	if err := json.Unmarshal([]byte(vals.Get("user")), &user); err != nil || user.ID == 0 {
		return tgUserLite{}, false
	}
	return user, true
}

// hexDecodeHash decodes the 64-char sha256 hex `hash` field.
func hexDecodeHash(s string) ([]byte, error) {
	if len(s) != 64 {
		return nil, errors.New("auth: bad hash length")
	}
	return hex.DecodeString(s)
}
