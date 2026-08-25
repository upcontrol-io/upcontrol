package scrub

import (
	"strings"
	"testing"
)

func TestScrubTable(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		secret  string // must NOT appear in the cleaned output
		kind    string
		minRedc int
	}{
		{"stripe", "checkout using sk_live_8f2ac41d9b0eABCDEFghijklmnop", "sk_live_8f2ac41d9b0eABCDEFghijklmnop", "stripe", 1},
		{"github", "token ghp_abcdefghijklmnopqrstuvwxyz0123 deployed", "ghp_abcdefghijklmnopqrstuvwxyz0123", "github", 1},
		{"slack", "bot xoxb-1234567890123-1234567890123-abcdef pinged", "xoxb-1234567890123-1234567890123-abcdef", "slack", 1},
		{"aws", "creds AKIAIOSFODNN7EXAMPLE12345 leaked", "AKIAIOSFODNN7EXAMPLE12345", "aws", 1},
		{"bearer", "Authorization: Bearer supersecrettoken1234567890", "supersecrettoken1234567890", "bearer", 1},
		{"conn", "postgres://app:hunter2@db.example.com:5432/prod", "hunter2", "conn", 1},
		{"session", "Cookie: session=abcdef0123456789token", "abcdef0123456789token", "session", 1},
		{"cookie", "Set-Cookie: uc_session=abcdef0123456789value; Path=/", "abcdef0123456789value", "cookie", 1},
		{"card", "charged 4242 4242 4242 4242 to the customer", "4242 4242 4242 4242", "card", 1},
		{"email", "notify anna@example.com on failure", "anna@example.com", "email", 1},
		{"pem", "key was\n-----BEGIN RSA PRIVATE KEY-----\nMIIabc...\n-----END RSA PRIVATE KEY-----\nend", "MIIabc...", "pem", 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := Scrub(c.in)
			if strings.Contains(r.Cleaned, c.secret) {
				t.Errorf("secret %q leaked into output: %q", c.secret, r.Cleaned)
			}
			if r.Counts[c.kind] < c.minRedc {
				t.Errorf("kind %q count = %d, want >= %d (counts=%v)", c.kind, r.Counts[c.kind], c.minRedc, r.Counts)
			}
			if !strings.Contains(r.Cleaned, "[redacted:"+c.kind+":") {
				t.Errorf("missing marker for %q in %q", c.kind, r.Cleaned)
			}
		})
	}
}

func TestScrubCompositeLine(t *testing.T) {
	// A single line carrying several secrets: all must go.
	in := "auth Bearer abcdef1234567890token for anna@example.com via sk_live_secretkey1234567890XYZ"
	r := Scrub(in)
	for _, leak := range []string{"abcdef1234567890token", "anna@example.com", "sk_live_secretkey1234567890XYZ"} {
		if strings.Contains(r.Cleaned, leak) {
			t.Errorf("leaked %q: %q", leak, r.Cleaned)
		}
	}
	total := 0
	for _, n := range r.Counts {
		total += n
	}
	if total < 3 {
		t.Errorf("redaction count = %d, want >= 3 (counts=%v)", total, r.Counts)
	}
}

func TestScrubLeavesNormalText(t *testing.T) {
	// Ordinary log lines must pass through untouched.
	in := `14:32:04 POST /checkout 502 upstream_closed`
	r := Scrub(in)
	if r.Cleaned != in {
		t.Errorf("normal line changed: %q", r.Cleaned)
	}
	if len(r.Counts) != 0 {
		t.Errorf("normal line scrubbed: %v", r.Counts)
	}
}

func TestScrubLuhnRejectsShort(t *testing.T) {
	// A run below the 13-digit card minimum must NOT be redacted, even though it
	// is the only thing standing between us and eating order IDs.
	in := "order 1234567890 done" // 10 digits
	r := Scrub(in)
	if r.Counts["card"] != 0 {
		t.Errorf("short digit run was redacted as a card: %v", r.Counts)
	}
}

func TestScrubCardLuhnAcceptsValid(t *testing.T) {
	// 4242 4242 4242 4242 is the canonical Luhn-valid test card.
	r := Scrub("card 4242424242424242 done")
	if r.Counts["card"] != 1 {
		t.Errorf("valid card not redacted: %v", r.Counts)
	}
}

func TestScrubMarkerShape(t *testing.T) {
	// The marker is [redacted:TYPE:LEN]; the LEN is the secret's byte length.
	r := Scrub("Bearer abcdefghijklmnop1234567890")
	// secret is 28 bytes (>=12 min).
	if !strings.HasPrefix(r.Cleaned, "[redacted:bearer:") {
		t.Errorf("bad marker: %q", r.Cleaned)
	}
}

// BenchmarkScrub targets a <=2 us per string budget on a typical line.
// Run with: go test -bench=. -benchmem ./internal/ingest/scrub/
func BenchmarkScrub(b *testing.B) {
	in := `2026-08-12 14:32:04 user anna@example.com charged 4242 4242 4242 4242 ` +
		`auth=Bearer abcdef1234567890token ref sk_live_secretkey1234567890XYZ`
	b.SetBytes(int64(len(in)))
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = Scrub(in)
	}
}
