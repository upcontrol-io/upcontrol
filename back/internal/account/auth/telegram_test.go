package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

// buildInitData constructs a signed initData string the way Telegram does, so
// the tests verify against the real algorithm rather than a copy of our own
// code (a self-referential test would pass even with a wrong key derivation).
func buildInitData(botToken string, tgUserID int64, authAge time.Duration) string {
	user := `{"id":` + strconv.FormatInt(tgUserID, 10) + `,"first_name":"Mira"}`
	vals := url.Values{
		"user":          {user},
		"auth_date":     {strconv.FormatInt(time.Now().Add(-authAge).Unix(), 10)},
		"query_id":      {"AAF1"},
		"chat_instance": {"-123"},
	}
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
	secret := hmac.New(sha256.New, []byte("WebAppData"))
	secret.Write([]byte(botToken))
	mac := hmac.New(sha256.New, secret.Sum(nil))
	mac.Write([]byte(strings.TrimSuffix(sb.String(), "\n")))
	sum := mac.Sum(nil)
	vals.Set("hash", hex.EncodeToString(sum))
	return vals.Encode()
}

// The happy path: correct signature, fresh auth_date, a user — the Mini App
// session door opens.
func TestVerifyInitDataValid(t *testing.T) {
	initData := buildInitData("bot-token-1", 4242, time.Minute)
	user, ok := VerifyInitData(initData, "bot-token-1", time.Now())
	if !ok {
		t.Fatal("valid initData rejected")
	}
	if user.ID != 4242 || user.FirstName != "Mira" {
		t.Fatalf("user = %+v, want id 4242 / Mira", user)
	}
}

// A different bot token cannot sign for us: the secret is derived from OUR
// token, so a signature made with another bot's key must fail.
func TestVerifyInitDataWrongToken(t *testing.T) {
	initData := buildInitData("other-bot-token", 777, time.Minute)
	user, ok := VerifyInitData(initData, "bot-token-1", time.Now())
	if ok {
		t.Fatal("initData signed by another bot accepted")
	}
	_ = user // (kept for symmetry with the valid-path assertion)
}

// A tampered payload (the signature covers the data-check-string, so any field
// edit breaks it) — the forge path a client-attested hash would allow.
func TestVerifyInitDataTampered(t *testing.T) {
	initData := buildInitData("bot-token-1", 4242, time.Minute)
	// Flip the user id inside the signed payload, keep the hash.
	tampered := strings.Replace(initData, `%22id%22%3A4242`, `%22id%22%3A9999`, 1)
	if tampered == initData {
		t.Fatal("test setup: could not tamper the payload")
	}
	if _, ok := VerifyInitData(tampered, "bot-token-1", time.Now()); ok {
		t.Fatal("tampered initData accepted")
	}
}

// Replay: an auth_date older than the freshness window is refused even with a
// perfect signature.
func TestVerifyInitDataStale(t *testing.T) {
	initData := buildInitData("bot-token-1", 4242, 25*time.Hour)
	if _, ok := VerifyInitData(initData, "bot-token-1", time.Now()); ok {
		t.Fatal("stale initData accepted")
	}
}

// Malformed inputs must refuse, never panic: no hash, garbage encoding.
func TestVerifyInitDataMalformed(t *testing.T) {
	for _, bad := range []string{"", "%%%zz", "user=%7B%7D", "hash=deadbeef"} {
		if _, ok := VerifyInitData(bad, "bot-token-1", time.Now()); ok {
			t.Fatalf("malformed initData %q accepted", bad)
		}
	}
}
