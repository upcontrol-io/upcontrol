package discover

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"sync"
)

// pagesWanted is how many pages the shortlist carries: what the strip can show
// without becoming a list; the picker is where the plan limit bites.
const pagesWanted = 5

// Page is one discovered page and how it answered. Status 0 means found but
// never probed, reported as unknown rather than down.
type Page struct {
	URL     string
	Path    string
	Source  string // "in sitemap.xml" | "linked from the homepage" | "found in DNS"
	Status  uint16
	TotalMs uint32
	OK      bool
	Slowest bool
	// Error is the executor's class when the probe ran and failed: "we asked
	// and nothing answered", which for a monitoring product is a finding.
	Error string
}

// findPages builds the shortlist and probes it. Discovery order is by cost:
// robots, then the sitemap, homepage links as the free fallback.
func findPages(ctx context.Context, p prober, dns resolver, base string, homepage []byte) (pages, hosts []Page) {
	r := fetchRobots(ctx, p, base)

	// Homepage links are read either way: even when a sitemap answers, the links
	// are where a subdomain like get.example.com shows up, and they cost nothing.
	links := linksFrom(base, homepage)
	cands := fetchSitemapURLs(ctx, p, base, r)
	if len(cands) == 0 {
		cands = links
	}

	if short := rank(base, cands, r); len(short) > 0 {
		pages = probePages(ctx, p, short)
	}
	hosts = findHosts(ctx, p, dns, base, append(append([]candidate{}, cands...), links...))
	return pages, hosts
}

// probePages runs the shortlist concurrently, bounded by its own length
// (rank capped it at pagesWanted).
func probePages(ctx context.Context, p prober, cands []candidate) []Page {
	pages := make([]Page, len(cands))
	var wg sync.WaitGroup
	for i, c := range cands {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// A subdomain's root has no path, so it is named by its host —
			// "api.harpa.ai" rather than a blank cell.
			path := c.URL
			if u, err := url.Parse(c.URL); err == nil {
				if p := strings.TrimSuffix(u.Path, "/"); p != "" {
					path = p
				} else {
					path = u.Host
				}
			}
			page := Page{URL: c.URL, Path: path, Source: c.Source}
			res := p.Execute(ctx, CheckSpec{
				URL: c.URL, Method: http.MethodGet,
				TimeoutMs:    uint32(perRequestTimeout.Milliseconds()),
				MaxRedirects: 2,
			})
			page.Status, page.TotalMs, page.OK = res.StatusCode, res.TotalMs, res.OK
			if res.StatusCode == 0 {
				page.Error = res.ErrorClass
			}
			pages[i] = page
		}()
	}
	wg.Wait()

	// Naming the slowest is the point of probing at all: it turns a list of
	// pages into a recommendation about which one to watch.
	slowest, found := 0, false
	for i, page := range pages {
		if page.Status != 0 && (!found || page.TotalMs > pages[slowest].TotalMs) {
			slowest, found = i, true
		}
	}
	if found && len(pages) > 1 {
		pages[slowest].Slowest = true
	}
	return pages
}
