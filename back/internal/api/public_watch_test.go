package api

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestWatchWithoutAnEmailIsRefusedOnASelfHost(t *testing.T) {
	// A self-host has no use-before-signup story, so the e-mail-less watch is
	// refused there. The refusal must land before the pool and the executor
	// are touched: both are nil in this struct, so a check that ran later
	// would panic rather than fail.
	h := &writeAPI{selfHosted: true}
	w := httptest.NewRecorder()
	h.publicWatch(w, httptest.NewRequest("POST", "/public/watch",
		strings.NewReader(`{"host":"example.com"}`)))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
	if !strings.Contains(w.Body.String(), "missing_email") {
		t.Errorf("body = %q, want missing_email", w.Body.String())
	}
}

func TestWatchIsNotThrottledByTheCheckThatPrecededIt(t *testing.T) {
	// The landing flow (check, tick, watch) sits inside the check's cooldown:
	// the two must throttle independently, or the watch dies as "no backend".
	h := &writeAPI{checkSeenAt: map[string]time.Time{}}
	const ip = "203.0.113.9"
	if !h.checkAllow(ip, "example.com") {
		t.Fatal("the first check from an address must be allowed")
	}
	if !h.watchAllow(ip) {
		t.Fatal("a watch immediately after a check must be allowed")
	}
	// Each bucket still holds its own line against a repeat.
	if h.checkAllow(ip, "example.com") {
		t.Error("a second check inside the cooldown must be refused")
	}
	if h.watchAllow(ip) {
		t.Error("a second watch inside the cooldown must be refused")
	}
	// And one address's cooldown is not another's.
	if !h.watchAllow("198.51.100.7") {
		t.Error("a different address must not inherit the cooldown")
	}
}

func TestBareHostStripsEverythingButTheDomain(t *testing.T) {
	// What the visitor typed becomes their project's name and their status
	// page's title, so a paste of a full URL must not end up on those screens.
	cases := []struct{ in, want string }{
		{"example.com", "example.com"},
		{"https://example.com", "example.com"},
		{"http://example.com/", "example.com"},
		{"https://mysite.io/pricing", "mysite.io"},
		{"  example.com  ", "example.com"},
		{"example.com:8443", "example.com"},
		{"example.com/a/b?c=d#e", "example.com"},
		{"api.example.com", "api.example.com"},
	}
	for _, c := range cases {
		if got := bareHost(c.in); got != c.want {
			t.Errorf("bareHost(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestBareHostKeepsAnEmptyInputEmpty(t *testing.T) {
	// Provision falls back to its own placeholder when the domain is empty: a
	// bareHost that invented one would name a project after a scheme fragment.
	for _, in := range []string{"", "   ", "https://", "/pricing"} {
		if got := bareHost(in); got != "" {
			t.Errorf("bareHost(%q) = %q, want empty", in, got)
		}
	}
}

func TestSlugFromHostReadsAsTheSiteName(t *testing.T) {
	// The slug is the one string a customer hands to their own users, so it says
	// whose page it is. "prj-20" says nothing and leaks how many accounts exist.
	cases := []struct{ in, want string }{
		{"harpa.ai", "harpa-ai"},
		{"https://harpa.ai/pricing", "harpa-ai"},
		{"example.com", "example-com"},
		{"my-shop.co.uk", "my-shop-co-uk"},
		{"api.example.com:8443", "api-example-com"},
		{"WWW.Example.COM", "www-example-com"},
		{"192.168.1.1", "192-168-1-1"},
	}
	for _, c := range cases {
		if got := slugFromHost(c.in); got != c.want {
			t.Errorf("slugFromHost(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestSlugFromHostIsAlwaysAUsableURLSegment(t *testing.T) {
	// The output is empty (caller falls back to the project id) or a plain
	// lowercase segment; an IDN like "münchen.example" is no exception. A
	// long host is cut at 40 and salted with 6 hex of its hash (plan part 2):
	// 40 + 1 + 6 = 47 is the ceiling, and the salt makes two long hosts
	// sharing a prefix still yield different slugs.
	for _, in := range []string{"", "...", "-", "—", "münchen.example", "a..b", "-lead-", strings.Repeat("x", 80) + ".com"} {
		got := slugFromHost(in)
		if got == "" {
			continue
		}
		if strings.HasPrefix(got, "-") || strings.HasSuffix(got, "-") || strings.Contains(got, "--") {
			t.Errorf("slugFromHost(%q) = %q: bad dashes", in, got)
		}
		if len(got) > 47 {
			t.Errorf("slugFromHost(%q) = %q: %d chars, want <= 47", in, got, len(got))
		}
		for _, r := range got {
			if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' {
				t.Errorf("slugFromHost(%q) = %q: %q is not URL-safe", in, got, r)
			}
		}
	}
	// The same long host always yields the same slug: the salt is a hash of
	// the host, not a roll of the dice.
	long := strings.Repeat("x", 80) + ".com"
	if slugFromHost(long) != slugFromHost("https://"+long+"/pricing") {
		t.Errorf("the same long host yielded two different slugs")
	}
}

func TestCanonicalHostStripsSchemeWWWAndCase(t *testing.T) {
	// The watch door keys everything on this host: one spelling per site, so
	// www.example.com and example.com are one page, one probe (plan part 2).
	cases := []struct{ in, want string }{
		{"example.com", "example.com"},
		{"WWW.Example.COM", "example.com"},
		{"https://www.example.com", "example.com"},
		{"http://www.example.com/pricing?a=1", "example.com"},
		{"www.www.example.com", "www.example.com"}, // ONE leading www. only
		{"example.com:8443", "example.com"},
		{"  shop.example.co.uk  ", "shop.example.co.uk"},
	}
	for _, c := range cases {
		got, err := canonicalHost(c.in)
		if err != nil || got != c.want {
			t.Errorf("canonicalHost(%q) = %q (err %v), want %q", c.in, got, err, c.want)
		}
	}
	for _, in := range []string{"", "   ", "https://", "/pricing"} {
		if got, err := canonicalHost(in); err == nil || got != "" {
			t.Errorf("canonicalHost(%q) = %q (err %v), want the empty refusal", in, got, err)
		}
	}
}

func TestMonitorNameNamesTheTargetNotTheTypedHost(t *testing.T) {
	// Discovery's api./app. hosts become checks of their own: naming them after
	// the typed host labelled every component "harpa.ai".
	cases := []struct{ host, target, want string }{
		{"harpa.ai", "https://harpa.ai", "harpa.ai"},
		{"harpa.ai", "https://api.harpa.ai", "api.harpa.ai"},
		{"harpa.ai", "https://app.harpa.ai/", "app.harpa.ai"},
		{"harpa.ai", "https://api.harpa.ai/v1", "api.harpa.ai/v1"},
		{"harpa.ai", "auto-generated ping URL", "harpa.ai"}, // unparseable: fall back
	}
	for _, c := range cases {
		if got := monitorName(c.host, c.target); got != c.want {
			t.Errorf("monitorName(%q, %q) = %q, want %q", c.host, c.target, got, c.want)
		}
	}
}

func TestSameHostTargetsAcceptsASubdomain(t *testing.T) {
	// The case the watch adds is the one sameHostTargets must NOT refuse: an
	// api./app. host is pickable on the landing and must become a check.
	got := sameHostTargets("https://mine.com", []string{
		"https://api.mine.com/v1",
		"https://mine.com.evil.test/x", // suffix trick, not a subdomain
	})
	if len(got) != 1 || got[0] != "https://api.mine.com/v1" {
		t.Errorf("targets = %v, want the subdomain and nothing else", got)
	}
}

// The mint audit's IP identity: HMAC-SHA256 under the deployment's secret
// key when one is configured (a bare sha256(IP) is reconstructable from a
// traffic dump by enumerating the address space), plain sha256 without one
// so self-hosts keep the spelling their existing audit rows carry.
func TestMintIPHashIsKeyedWhenASecretIsConfigured(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef") // 32 bytes, AES-256 size
	plain := &writeAPI{}
	keyed := &writeAPI{mintSecret: key}

	h := keyed.mintIPHash("203.0.113.7")
	if h != keyed.mintIPHash("203.0.113.7") {
		t.Fatal("the keyed hash is not deterministic for one IP")
	}
	if h == plain.mintIPHash("203.0.113.7") {
		t.Fatal("the keyed hash equals the unkeyed sha256 spelling")
	}
	if keyed.mintIPHash("203.0.113.8") == h {
		t.Fatal("two addresses collided on one keyed hash")
	}
	m := hmac.New(sha256.New, key)
	m.Write([]byte("203.0.113.7"))
	if h != hex.EncodeToString(m.Sum(nil)) {
		t.Fatal("the keyed hash is not HMAC-SHA256 under the configured key")
	}
	s := sha256.Sum256([]byte("203.0.113.7"))
	if plain.mintIPHash("203.0.113.7") != hex.EncodeToString(s[:]) {
		t.Fatal("without a key the hash must stay the plain sha256")
	}
}
