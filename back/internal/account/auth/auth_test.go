package auth

import (
	"crypto/sha256"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	sqlc "go.upcontrol.io/back/gen/pg"

	"go.upcontrol.io/back/internal/analytics"
)

func hashCode(t *testing.T, code string) []byte {
	t.Helper()
	sum := sha256.Sum256([]byte(code))
	return sum[:]
}

func TestValidateCode_HappyPath(t *testing.T) {
	rec := codeRecord{Hash: hashCode(t, "abcd1234"), Expires: future()}
	if !validateCode(rec, "abcd1234", time.Now()) {
		t.Fatal("correct code within TTL with attempts to spare must validate")
	}
}

func TestValidateCode_WrongCode(t *testing.T) {
	rec := codeRecord{Hash: hashCode(t, "abcd1234"), Expires: future()}
	if validateCode(rec, "00000000", time.Now()) {
		t.Fatal("a wrong code must not validate")
	}
	if validateCode(rec, "abcd1235", time.Now()) { // off by one
		t.Fatal("an off-by-one code must not validate")
	}
}

func TestValidateCode_Expired(t *testing.T) {
	rec := codeRecord{Hash: hashCode(t, "abcd1234"), Expires: time.Now().Add(-time.Minute)}
	if validateCode(rec, "abcd1234", time.Now()) {
		t.Fatal("an expired code must not validate")
	}
}

func TestValidateCode_AlreadyRedeemed(t *testing.T) {
	rec := codeRecord{Hash: hashCode(t, "abcd1234"), Expires: future(), Redeemed: true}
	if validateCode(rec, "abcd1234", time.Now()) {
		t.Fatal("a redeemed code (replay) must not validate a second time")
	}
}

func TestValidateCode_AttemptCap(t *testing.T) {
	rec := codeRecord{Hash: hashCode(t, "abcd1234"), Expires: future(), Attempts: 5}
	if validateCode(rec, "abcd1234", time.Now()) {
		t.Fatal("a code at the attempt cap must not validate even if correct")
	}
	// Just below the cap is still fine.
	rec.Attempts = 4
	if !validateCode(rec, "abcd1234", time.Now()) {
		t.Fatal("a correct code one below the cap must validate")
	}
}

func TestValidateCode_Empty(t *testing.T) {
	if validateCode(codeRecord{Hash: hashCode(t, "abcd1234"), Expires: future()}, "", time.Now()) {
		t.Fatal("an empty submission must not validate")
	}
	if validateCode(codeRecord{Expires: future()}, "abcd1234", time.Now()) {
		t.Fatal("a missing stored hash must not validate")
	}
}

func TestValidateCode_NotAnOracle(t *testing.T) {
	// Every failure path must look identical to a caller — there is no separate
	// "expired" vs "wrong" signal to leak which emails exist or which codes were
	// issued. validateCode returns only a bool, so this holds by construction; the
	// test pins it.
	correct := codeRecord{Hash: hashCode(t, "abcd1234"), Expires: future()}
	expired := codeRecord{Hash: hashCode(t, "abcd1234"), Expires: time.Now().Add(-time.Minute)}
	redeemed := codeRecord{Hash: hashCode(t, "abcd1234"), Expires: future(), Redeemed: true}
	for _, tc := range []struct {
		name string
		rec  codeRecord
	}{
		{"wrong", correct},
		{"expired", expired},
		{"redeemed", redeemed},
	} {
		tc.rec.Hash = hashCode(t, "deadbeef") // submitted code never matches
		if validateCode(tc.rec, "abcd1234", time.Now()) {
			t.Fatalf("%s: must reject", tc.name)
		}
	}
}

func TestGenCode(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 1000; i++ {
		c := genCode()
		if len(c) != 8 {
			t.Fatalf("code %q is not 8 chars", c)
		}
		for _, r := range c {
			if !strings.ContainsRune("0123456789abcdef", r) {
				t.Fatalf("code %q has non-hex rune", c)
			}
		}
		if seen[c] {
			t.Fatalf("code %q collided within 1000 draws — entropy too low", c)
		}
		seen[c] = true
	}
}

func TestClientIP(t *testing.T) {
	t.Parallel()
	// X-Forwarded-For (Caddy) wins and the first hop is taken when chained.
	r := httptest.NewRequest(http.MethodPost, "/v1/auth/magic-link", strings.NewReader("{}"))
	r.RemoteAddr = "10.0.0.1:5000"
	r.Header.Set("X-Forwarded-For", "203.0.113.9, 10.0.0.1")
	if got := analytics.ClientIP(r); got != "203.0.113.9" {
		t.Fatalf("clientIP = %q, want 203.0.113.9", got)
	}
	// Fallback to the peer when no proxy header is present.
	r2 := httptest.NewRequest(http.MethodPost, "/v1/auth/magic-link", strings.NewReader("{}"))
	r2.RemoteAddr = "198.51.100.7:443"
	if got := analytics.ClientIP(r2); got != "198.51.100.7" {
		t.Fatalf("clientIP = %q, want 198.51.100.7", got)
	}
}

// The magic-link door has the same exposure the Google one does: it needs no
// cookie from a victim because it installs one. An attacker who mails a code to
// their OWN address can cross-site POST the redeem and leave the reader signed
// into the attacker's tenant. The guard runs before the pool is touched, which
// is why a nil pool is enough to test it.
func TestMagicLinkRefusesACrossSitePost(t *testing.T) {
	t.Parallel()
	h := NewMagicLink(nil, nil, true, nil, nil, slog.New(slog.DiscardHandler))
	body := `{"email":"ada@example.com","token":"1ece76e3"}`

	for _, site := range []string{"cross-site", "same-site", "none"} {
		r := httptest.NewRequest(http.MethodPost, "/v1/auth/magic-link", strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Sec-Fetch-Site", site)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusForbidden {
			t.Errorf("Sec-Fetch-Site %q: code = %d, want 403", site, w.Code)
		}
	}
	// A cross-site form can only send these, and text/plain is the one whose
	// body parses as JSON.
	for _, ct := range []string{"text/plain", "application/x-www-form-urlencoded", ""} {
		r := httptest.NewRequest(http.MethodPost, "/v1/auth/magic-link", strings.NewReader(body))
		if ct != "" {
			r.Header.Set("Content-Type", ct)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusForbidden {
			t.Errorf("Content-Type %q: code = %d, want 403", ct, w.Code)
		}
	}
}

// The other half of the same rule: the person table's email column is UNIQUE
// and byte-exact, so one spelling has to win before anything is stored.
func TestNormalizeEmailFoldsCaseAndSpace(t *testing.T) {
	t.Parallel()
	for _, tc := range [][2]string{
		{"Ada@Example.COM", "ada@example.com"},
		{"  ada@example.com  ", "ada@example.com"},
		{"ADA@EXAMPLE.COM", "ada@example.com"},
		{"ada@example.com", "ada@example.com"},
		{"   ", ""},
	} {
		if got := NormalizeEmail(tc[0]); got != tc[1] {
			t.Errorf("NormalizeEmail(%q) = %q, want %q", tc[0], got, tc[1])
		}
	}
}

func future() time.Time { return time.Now().Add(10 * time.Minute) }

func TestInitials(t *testing.T) {
	// The avatar's two letters come from whatever the account actually has: a
	// full name, then a single word, then the address. "U" is the last resort,
	// never a slice of an empty string (which would panic).
	cases := []struct{ name, email, want string }{
		{"Ada Lovelace", "ada@example.com", "AL"},
		{"ada", "ada@example.com", "AD"},
		{"", "ada@example.com", "AD"},
		{"", "", "U"},
		{"", "a", "U"},
	}
	for _, c := range cases {
		if got := initials(c.name, c.email); got != c.want {
			t.Fatalf("initials(%q, %q) = %q, want %q", c.name, c.email, got, c.want)
		}
	}
}

func TestNameFromEmail(t *testing.T) {
	// A new account is named from the local part; an address with no local part
	// (or no @ at all) must come back unchanged rather than empty, or the person
	// row is created nameless.
	cases := []struct{ in, want string }{
		{"ada@example.com", "ada"},
		{"ada+upcontrol@example.com", "ada+upcontrol"},
		{"noatsign", "noatsign"},
		{"@example.com", "@example.com"},
	}
	for _, c := range cases {
		if got := NameFromEmail(c.in); got != c.want {
			t.Fatalf("NameFromEmail(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestCodeRecordFrom_MapsRedeemedAndExpiry(t *testing.T) {
	// validateCode is only as good as what it is handed: a redeemed code must
	// arrive as Redeemed=true (pgtype validity, not the zero time), or a used
	// magic link would pass verification a second time.
	at := time.Now().Add(-time.Minute)
	rec := codeRecordFrom(sqlc.MagicLinkCode{
		CodeHash:   []byte{1, 2, 3},
		Attempts:   4,
		ExpiresAt:  pgtype.Timestamptz{Time: at, Valid: true},
		RedeemedAt: pgtype.Timestamptz{Time: at, Valid: true},
	})
	if !rec.Redeemed {
		t.Fatal("a code with redeemed_at set must map to Redeemed=true")
	}
	if rec.Attempts != 4 || !rec.Expires.Equal(at) || len(rec.Hash) != 3 {
		t.Fatalf("codeRecordFrom lost a field: %+v", rec)
	}
	open := codeRecordFrom(sqlc.MagicLinkCode{RedeemedAt: pgtype.Timestamptz{}})
	if open.Redeemed {
		t.Fatal("an unredeemed code must map to Redeemed=false")
	}
}
