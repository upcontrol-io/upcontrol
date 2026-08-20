package analytics

import (
	cryptorand "crypto/rand"
	"encoding/hex"
	"net/http"
	"time"
)

// uc_vid is the first-party visitor cookie (§Decision 2): HttpOnly,
// SameSite=Lax, Path=/, one year. It is a random token, not a fingerprint —
// the server stores only sha256(token) and the ClickHouse rows carry the
// resulting visitor id, so the raw value lives in the browser alone.
const (
	CookieName       = "uc_vid"
	VisitorCookieTTL = 365 * 24 * time.Hour
	tokenHexLen      = 32 // 16 random bytes, hex-encoded
)

// MintVisitorToken returns a fresh 32-hex-char visitor token.
func MintVisitorToken() string {
	var b [tokenHexLen / 2]byte
	if _, err := cryptorand.Read(b[:]); err != nil {
		panic("analytics: crypto/rand: " + err.Error())
	}
	return hex.EncodeToString(b[:])
}

// VisitorToken returns the raw uc_vid cookie, or ok=false when absent,
// malformed, or not exactly 32 hex chars — a corrupt cookie is treated as no
// cookie (a new one is minted) rather than a lookup on garbage.
func VisitorToken(r *http.Request) (string, bool) {
	c, err := r.Cookie(CookieName)
	if err != nil || len(c.Value) != tokenHexLen {
		return "", false
	}
	if _, err := hex.DecodeString(c.Value); err != nil {
		return "", false
	}
	return c.Value, true
}

// SetVisitorCookie writes the uc_vid cookie. Secure is decided by the caller
// from the request itself (r.TLS != nil): dev runs plain HTTP through Caddy
// on :80, and a Secure cookie there would be silently dropped.
func SetVisitorCookie(w http.ResponseWriter, token string, secure bool) {
	http.SetCookie(w, &http.Cookie{
		Name: CookieName, Value: token, Path: "/",
		MaxAge: int(VisitorCookieTTL.Seconds()), HttpOnly: true,
		Secure: secure, SameSite: http.SameSiteLaxMode,
	})
}
