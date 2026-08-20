package discover

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
)

// fakeDNS answers from a set of names that exist. It also records every lookup,
// because the cost argument for this feature — DNS is cheap and goes to a
// resolver, not to the customer — only holds if the lookups stay bounded.
type fakeDNS struct {
	mu      sync.Mutex
	exists  map[string]bool
	lookups []string
}

func (f *fakeDNS) LookupHost(_ context.Context, host string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lookups = append(f.lookups, host)
	if f.exists[host] {
		return []string{"203.0.113.10"}, nil
	}
	return nil, errors.New("no such host")
}

func hostNames(pages []Page) []string {
	out := make([]string, 0, len(pages))
	for _, p := range pages {
		out = append(out, p.Path)
	}
	return out
}

func TestHostsFoundInDNSWhenTheHTMLNeverMentionsThem(t *testing.T) {
	// The case that motivated this: harpa.ai's homepage does not contain the
	// string "api.harpa.ai" anywhere, its certificate is a wildcard so the SANs
	// list nothing, and the call that reveals it happens in JavaScript we do not
	// execute. One DNS lookup finds it.
	dns := &fakeDNS{exists: map[string]bool{"api.example.com": true}}
	p := &bodyProber{body: map[string]string{"https://api.example.com": "ok"}}

	hosts := findHosts(context.Background(), p, dns, base, nil)
	if len(hosts) != 1 || hosts[0].Path != "api.example.com" {
		t.Fatalf("hosts = %v, want api.example.com", hostNames(hosts))
	}
	if hosts[0].Source != "found in DNS" {
		t.Errorf("source = %q", hosts[0].Source)
	}
}

func TestHostsAreNotProbedUntilDNSSaysTheyExist(t *testing.T) {
	// The whole cost argument: names that do not resolve cost a lookup and
	// nothing else. Probing all six blindly would be six requests to a stranger.
	dns := &fakeDNS{exists: map[string]bool{}}
	p := &bodyProber{body: map[string]string{}}

	if hosts := findHosts(context.Background(), p, dns, base, nil); len(hosts) != 0 {
		t.Fatalf("hosts = %v, want none", hostNames(hosts))
	}
	if len(p.asked) != 0 {
		t.Errorf("sent %v, want no HTTP request for names that do not resolve", p.asked)
	}
	if len(dns.lookups) != len(conventionalHosts) {
		t.Errorf("looked up %d names, want %d", len(dns.lookups), len(conventionalHosts))
	}
}

func TestHostsPreferWhatTheNameSaysItDoes(t *testing.T) {
	dns := &fakeDNS{exists: map[string]bool{
		"docs.example.com": true, "api.example.com": true, "app.example.com": true,
		"auth.example.com": true,
	}}
	p := &bodyProber{body: map[string]string{}}

	hosts := findHosts(context.Background(), p, dns, base, nil)
	if len(hosts) != hostsWanted {
		t.Fatalf("hosts = %v, want %d", hostNames(hosts), hostsWanted)
	}
	// api/app/auth carry a customer; docs is the one to drop at the cap.
	for _, h := range hosts {
		if h.Path == "docs.example.com" {
			t.Errorf("docs kept over a service host: %v", hostNames(hosts))
		}
	}
}

func TestHostsLinkedBySiteOutrankGuesses(t *testing.T) {
	// A name the site itself links is evidence; a conventional name is a guess.
	dns := &fakeDNS{exists: map[string]bool{"docs.example.com": true}}
	p := &bodyProber{body: map[string]string{}}
	seen := []candidate{{URL: "https://get.example.com/download"}}

	hosts := findHosts(context.Background(), p, dns, base, seen)
	names := hostNames(hosts)
	found := false
	for _, n := range names {
		if n == "get.example.com" {
			found = true
		}
	}
	if !found {
		t.Errorf("hosts = %v, want the linked get.example.com", names)
	}
	for _, h := range hosts {
		if h.Source == "linked from the site" && h.Path != "get.example.com" {
			t.Errorf("wrong source on %v", h)
		}
	}
}

func TestHostsNeverLeaveTheRequestedDomain(t *testing.T) {
	// A link to another company's site must not turn into a host we offer to
	// watch — nor into a DNS lookup that pretends it belongs to this domain.
	dns := &fakeDNS{exists: map[string]bool{}}
	p := &bodyProber{body: map[string]string{}}
	seen := []candidate{
		{URL: "https://victim.example.net/"},
		{URL: "https://example.com.evil.test/"}, // ends in the letters, not the dot-suffix
	}

	hosts := findHosts(context.Background(), p, dns, base, seen)
	for _, h := range hosts {
		if !strings.HasSuffix(h.Path, ".example.com") {
			t.Errorf("offered a host outside the domain: %q", h.Path)
		}
	}
	for _, l := range dns.lookups {
		if !strings.HasSuffix(l, ".example.com") {
			t.Errorf("looked up a name outside the domain: %q", l)
		}
	}
}

func TestHostsSkipTheRequestedHostItself(t *testing.T) {
	// The Live probe row already covers it; offering it twice would spend one of
	// three free checks on a duplicate.
	dns := &fakeDNS{exists: map[string]bool{}}
	p := &bodyProber{body: map[string]string{}}
	seen := []candidate{{URL: base + "/pricing"}}

	for _, h := range findHosts(context.Background(), p, dns, base, seen) {
		if h.Path == "example.com" {
			t.Error("offered the requested host as one of its own subdomains")
		}
	}
}

func TestHostDiscoveryOffWithoutAResolver(t *testing.T) {
	// nil resolver disables the feature rather than panicking: every other fact
	// still stands.
	p := &bodyProber{body: map[string]string{}}
	if hosts := findHosts(context.Background(), p, nil, base, nil); hosts != nil {
		t.Errorf("hosts = %v, want nil without a resolver", hostNames(hosts))
	}
}
