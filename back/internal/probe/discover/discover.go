// Package discover establishes facts about a host that one request cannot
// answer: whether a missing page is reported as missing, whether the app exposes
// a health endpoint, and what the response headers say about security and
// caching.
//
// It is deliberately NOT a crawler. It reads nothing off robots.txt and follows
// no sitemap; every request it makes is to a path this package names itself, on
// the host the caller already probed. That bound is the point: /public/check is
// anonymous, so one request to us becomes N requests to somebody else's server,
// and a package that fans out over attacker-supplied URLs is an amplifier. When
// sitemap discovery lands it belongs behind a per-domain cache and robots
// etiquette, which none of this needs.
//
// Every request goes through the same executor as the fleet, so the SSRF guard
// applies here too.
package discover

import (
	"context"
	"net/http"
	"strings"
	"time"

	"go.upcontrol.io/back/internal/probe/executor"
)

// Prober is the subset of the executor this package needs, so the tests can
// drive it without a network.
type Prober interface {
	Execute(ctx context.Context, spec CheckSpec) Result
}

// Aliases keep the interface above readable and let callers pass *executor.Executor.
type (
	CheckSpec = executor.CheckSpec
	Result    = executor.Result
)

// Paths tried, in order, looking for a health endpoint. Stops at the first one
// that answers 2xx: four sequential requests to a stranger's server is already
// the outer edge of polite, and these four cover the overwhelming majority.
var healthPaths = []string{"/health", "/healthz", "/status", "/api/health"}

// missingPath is requested to see how the host reports a page that is not there.
// Fixed rather than random so the behaviour is reproducible; the odds a site
// serves this path are nil, and if it did, the answer would be a true 200.
const missingPath = "/uc-probe-missing-page-check"

const (
	perRequestTimeout = 2500 * time.Millisecond
	// Total budget for everything in this package. The landing waits on the
	// response, and its own stepping animation runs about 2.4s, so a few seconds
	// is within the wait the reader already has. Whatever is not established by
	// the deadline is reported as unmeasured rather than guessed.
	//
	// Raised from 4s when page discovery landed: robots and the sitemap are
	// sequential before the shortlist can be probed at all.
	Budget = 6 * time.Second
	// MaxRequests is the ceiling on outbound HTTP requests per Run, and it is a
	// promise rather than an observation: robots (1) + sitemaps (3) + pages (5)
	// + hosts (3) + error page (1) + health (4) + api (bundle 1 + confirm 1, or
	// 3 conventional paths). Every cap in this package exists
	// to keep this number true no matter what the far side's sitemap says,
	// because /public/check is anonymous — one request to us must never become an
	// unbounded number to somebody else.
	//
	// DNS lookups are not counted here and are not bounded by it: they go to a
	// resolver, not to the customer, which is the whole reason host discovery is
	// affordable at all.
	MaxRequests = 20
)

// Headers is what the response headers say. Absent (nil Facts field) means no
// response was read; every field here is a fact about a response we hold.
type Headers struct {
	HSTS         bool
	CacheControl bool
	Compression  string // "gzip", "br", …; empty means the body came uncompressed
}

// ErrorPage is how the host answers a path that does not exist.
type ErrorPage struct {
	Status uint16
	// Correct reports whether the status is actually an error. A site that
	// answers 200 for a missing page is the case worth naming: an uptime checker
	// pointed at any URL sees "fine" straight through an outage. Single-page
	// apps do this by default.
	Correct bool
}

// Health is the health endpoint, if the host has one. A zero Path is a measured
// answer ("we looked, there is none"), not an absence — the caller renders it as
// "none", never as the unmeasured marker.
type Health struct {
	Path string
}

// Facts carries only what was established. A nil field means the fact was not
// measured — the API omits its row and the landing shows its no-data marker,
// which is a different statement from "measured, and the answer is no".
type Facts struct {
	Headers   *Headers
	ErrorPage *ErrorPage
	Health    *Health
	// Pages is the shortlist of the site's own pages worth watching. Empty when
	// the host stated none and linked to none — an empty group is not rendered,
	// which is a different thing from a group of unknowns.
	Pages []Page
	// API is the app's own API entry point. Nil means unmeasured; a zero Path
	// means we looked and the site exposes none.
	API *API
	// Hosts is the site's other hosts — api., app. and the like. Kept apart from
	// Pages because a separate host is a separate failure domain, and because
	// letting the two compete for one shortlist would cost a site with an API
	// either its API or its pages.
	Hosts []Page
}

// Run establishes what it can about baseURL within Budget. res is the result of
// the caller's own probe of the host, reused for the headers.
//
// It returns empty Facts when that probe got no response: a host that just
// refused us, timed out, or was blocked by the SSRF guard must not then be sent
// five more requests.
// dns may be nil, which turns host discovery off; every other fact still stands.
func Run(ctx context.Context, p Prober, dns Resolver, baseURL string, res Result) Facts {
	if res.StatusCode == 0 || res.ErrorClass == "blocked_target" {
		return Facts{}
	}

	var facts Facts
	facts.Headers = headersFrom(res.Header)

	ctx, cancel := context.WithTimeout(ctx, Budget)
	defer cancel()

	// The three lines of enquiry are independent, so they share the budget
	// rather than spending it in series.
	base := strings.TrimRight(baseURL, "/")
	errCh := make(chan *ErrorPage, 1)
	healthCh := make(chan *Health, 1)
	type found struct{ pages, hosts []Page }
	pagesCh := make(chan found, 1)
	apiCh := make(chan *API, 1)
	go func() { errCh <- probeErrorPage(ctx, p, base) }()
	go func() { healthCh <- probeHealth(ctx, p, base) }()
	go func() { apiCh <- findAPI(ctx, p, base, res.Body) }()
	go func() {
		pages, hosts := findPages(ctx, p, dns, base, res.Body)
		pagesCh <- found{pages, hosts}
	}()
	facts.ErrorPage = <-errCh
	facts.Health = <-healthCh
	facts.API = <-apiCh
	f := <-pagesCh
	facts.Pages, facts.Hosts = f.pages, f.hosts
	return facts
}

func headersFrom(h http.Header) *Headers {
	if h == nil {
		return nil
	}
	return &Headers{
		HSTS:         h.Get("Strict-Transport-Security") != "",
		CacheControl: h.Get("Cache-Control") != "",
		Compression:  h.Get("Content-Encoding"),
	}
}

func probeErrorPage(ctx context.Context, p Prober, base string) *ErrorPage {
	res := p.Execute(ctx, CheckSpec{
		URL: base + missingPath, Method: http.MethodGet,
		TimeoutMs: uint32(perRequestTimeout.Milliseconds()),
		// Do not follow redirects: a site that redirects a missing page to its
		// homepage has answered the question already, and following would report
		// the homepage's 200 as the missing page's status.
		MaxRedirects: 1,
	})
	if res.StatusCode == 0 {
		return nil
	}
	return &ErrorPage{Status: res.StatusCode, Correct: res.StatusCode >= 400}
}

func probeHealth(ctx context.Context, p Prober, base string) *Health {
	for _, path := range healthPaths {
		if ctx.Err() != nil {
			// Out of budget with candidates left: we cannot say there is none.
			return nil
		}
		res := p.Execute(ctx, CheckSpec{
			URL: base + path, Method: http.MethodGet,
			TimeoutMs:    uint32(perRequestTimeout.Milliseconds()),
			MaxRedirects: 1,
		})
		if res.StatusCode >= 200 && res.StatusCode < 300 {
			return &Health{Path: path}
		}
	}
	return &Health{} // looked everywhere on the list, found none
}
