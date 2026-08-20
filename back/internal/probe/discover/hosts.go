package discover

import (
	"context"
	"net/url"
	"sort"
	"strings"
	"sync"
)

// Resolver is the DNS lookup this package needs, so tests can answer without a
// network and without depending on what some real domain happens to publish.
type Resolver interface {
	LookupHost(ctx context.Context, host string) ([]string, error)
}

// conventionalHosts are the names a product puts its moving parts on. They are
// asked for by DNS, which costs about a millisecond and sends NOTHING to the
// customer's servers — only names that answer get an HTTP probe.
//
// This is why the list may exist at all. Enumerating subdomains from
// certificate-transparency logs was rejected (an external dependency, and
// thousands of guesses); six conventional names resolved locally is a different
// mechanism with a different cost. And it is the only one that works: harpa.ai's
// homepage does not mention api.harpa.ai anywhere, its certificate is a wildcard
// so the SANs list nothing, and the call that reveals it happens in JavaScript
// we do not execute. DNS finds it in one lookup.
//
// `admin` is deliberately absent: suggesting that someone's admin panel be
// watched reads as reconnaissance, whatever our intent.
var conventionalHosts = []string{"api", "app", "docs", "status", "dashboard", "auth"}

// hostsWanted caps how many hosts reach the shortlist, and therefore how many
// HTTP probes this costs on top of the DNS lookups.
const hostsWanted = 3

// findHosts discovers the site's other hosts: the conventional names that
// resolve, plus any subdomain the sitemap or the homepage already pointed at.
//
// An api. or app. host is its own failure domain — separate DNS, separate
// certificate, usually a separate deploy — so it can be down while the marketing
// site is perfectly healthy. That is exactly the outage a status page built from
// one URL misses, which is why this is worth a lookup.
func findHosts(ctx context.Context, p Prober, dns Resolver, base string, seen []candidate) []Page {
	root, err := url.Parse(base)
	if err != nil || dns == nil {
		return nil
	}

	// Names already evidenced by the site itself outrank names we guessed: the
	// site linked them, so they exist and matter.
	found := map[string]string{} // host → source
	var order []string
	add := func(host, source string) {
		if host == root.Host || !sameSite(root.Host, host) {
			return
		}
		if _, dup := found[host]; dup {
			return
		}
		found[host] = source
		order = append(order, host)
	}
	for _, c := range seen {
		if u, err := url.Parse(c.URL); err == nil {
			add(u.Host, "linked from the site")
		}
	}

	// Then the conventional names, resolved concurrently: six lookups against a
	// resolver, none of them a request to the customer.
	type hit struct {
		host string
		ok   bool
	}
	hits := make([]hit, len(conventionalHosts))
	var wg sync.WaitGroup
	for i, label := range conventionalHosts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			host := label + "." + root.Host
			addrs, err := dns.LookupHost(ctx, host)
			hits[i] = hit{host: host, ok: err == nil && len(addrs) > 0}
		}()
	}
	wg.Wait()
	for _, h := range hits {
		if h.ok {
			add(h.host, "found in DNS")
		}
	}
	if len(order) == 0 {
		return nil
	}

	// Rank by what the name says it does, so api. and app. outrank docs.
	sort.SliceStable(order, func(a, b int) bool {
		return hostScore(order[a], root.Host) > hostScore(order[b], root.Host)
	})
	if len(order) > hostsWanted {
		order = order[:hostsWanted]
	}

	cands := make([]candidate, 0, len(order))
	for _, host := range order {
		cands = append(cands, candidate{URL: root.Scheme + "://" + host, Source: found[host]})
	}
	return probePages(ctx, p, cands)
}

// hostTier ranks a host by what its name says it carries. Deliberately NOT the
// path vocabulary (valuableSection): /docs is a section worth watching on a
// site, but docs.example.com going down costs nobody money, while api. and auth.
// take the whole product with them.
var hostTier = map[string]int{
	"api": 60, "app": 60, "auth": 60, "dashboard": 60, "checkout": 60, "pay": 60,
	"docs": 30, "status": 30, "cdn": 30, "static": 30, "assets": 30,
}

func hostScore(host, root string) int {
	label := strings.TrimSuffix(host, "."+root)
	if i := strings.IndexByte(label, '.'); i >= 0 {
		label = label[:i] // deepest label is the one that names the service
	}
	return hostTier[strings.ToLower(label)]
}
