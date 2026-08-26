package api

import (
	"strings"
	"testing"
	"time"
)

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
	// lowercase segment; an IDN like "münchen.example" is no exception.
	for _, in := range []string{"", "...", "-", "—", "münchen.example", "a..b", "-lead-", strings.Repeat("x", 80) + ".com"} {
		got := slugFromHost(in)
		if got == "" {
			continue
		}
		if strings.HasPrefix(got, "-") || strings.HasSuffix(got, "-") || strings.Contains(got, "--") {
			t.Errorf("slugFromHost(%q) = %q: bad dashes", in, got)
		}
		if len(got) > 40 {
			t.Errorf("slugFromHost(%q) = %q: %d chars, want <= 40", in, got, len(got))
		}
		for _, r := range got {
			if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' {
				t.Errorf("slugFromHost(%q) = %q: %q is not URL-safe", in, got, r)
			}
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
