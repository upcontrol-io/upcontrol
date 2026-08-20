package discover

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"sync"
)

// PagesWanted is how many pages the shortlist carries. Five is what the strip
// can show without becoming a list, and it is already more than the free plan's
// three checks — the picker, not this package, is where the plan limit bites.
const PagesWanted = 5

// Page is one discovered page and how it answered. Status 0 means it was found
// but never probed (the budget ran out), which the caller reports as unknown
// rather than as down.
type Page struct {
	URL     string
	Path    string
	Source  string // "in sitemap.xml" | "linked from the homepage" | "found in DNS"
	Status  uint16
	TotalMs uint32
	OK      bool
	Slowest bool
	// Error is the executor's class when the probe ran and failed. It is what
	// separates "we never asked" from "we asked and nothing answered" — both
	// leave Status at 0, and for a monitoring product the second is a finding,
	// not a gap.
	Error string
}

// findPages builds the shortlist and probes it.
//
// Order of discovery is chosen by cost, not by preference: robots.txt is one
// small request and we need it anyway to behave, the sitemap is one or two more
// and is the site's own statement of what its pages are, and homepage links cost
// nothing at all because the body is already in hand — so they are the fallback
// for the many sites that have no sitemap, not a second-class source.
func findPages(ctx context.Context, p Prober, dns Resolver, base string, homepage []byte) (pages, hosts []Page) {
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

// probePages runs the shortlist concurrently. Bounded by its own length, which
// rank capped at PagesWanted — so this cannot fan out however long the sitemap
// happened to be.
func probePages(ctx context.Context, p Prober, cands []candidate) []Page {
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
