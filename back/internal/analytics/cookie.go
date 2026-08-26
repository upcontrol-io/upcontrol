package analytics

import (
	cryptorand "crypto/rand"
	"encoding/hex"
	"net/http"
	"time"
)

// uc_vid is the first-party visitor cookie: HttpOnly, SameSite=Lax, one year.
// A random token, not a fingerprint; the server stores only sha256(token).
const (
	cookieName       = "uc_vid"
	visitorCookieTTL = 365 * 24 * time.Hour
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

// VisitorToken returns the raw uc_vid cookie, or ok=false when absent or not
// 32 hex chars: a corrupt cookie counts as no cookie, not garbage lookup.
func VisitorToken(r *http.Request) (string, bool) {
	c, err := r.Cookie(cookieName)
	if err != nil || len(c.Value) != tokenHexLen {
		return "", false
	}
	if _, err := hex.DecodeString(c.Value); err != nil {
		return "", false
	}
	return c.Value, true
}

// SetVisitorCookie writes the uc_vid cookie; the caller decides Secure from
// the request (dev runs plain HTTP, a Secure cookie would be dropped).
func SetVisitorCookie(w http.ResponseWriter, token string, secure bool) {
	http.SetCookie(w, &http.Cookie{
		Name: cookieName, Value: token, Path: "/",
		MaxAge: int(visitorCookieTTL.Seconds()), HttpOnly: true,
		Secure: secure, SameSite: http.SameSiteLaxMode,
	})
}
