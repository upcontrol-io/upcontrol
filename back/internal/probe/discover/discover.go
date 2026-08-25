// Package discover establishes facts one request cannot answer: error pages,
// health endpoints, headers, pages, hosts. All requests ride the executor.
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

// Paths tried, in order, for a health endpoint; stops at the first 2xx. Four
// sequential requests to a stranger is the outer edge of polite.
var healthPaths = []string{"/health", "/healthz", "/status", "/api/health"}

// missingPath is requested to see how the host reports a missing page; fixed
// so the behaviour is reproducible.
const missingPath = "/uc-probe-missing-page-check"

const (
	perRequestTimeout = 2500 * time.Millisecond
	// Total budget for everything in this package; the landing waits on the
	// response. Unestablished by the deadline is reported unmeasured.
	Budget = 6 * time.Second
	// MaxRequests is the ceiling on outbound HTTP requests per Run; every cap
	// in this package keeps this promise true. DNS lookups are not counted.
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
	// Correct reports whether the status is an error: a site answering 200
	// for a missing page reads "fine" straight through an outage.
	Correct bool
}

// Health is the health endpoint. A zero Path is a measured "there is none",
// not an absence.
type Health struct {
	Path string
}

// Facts carries only what was established; a nil field means unmeasured,
// which renders differently from "measured, and the answer is no".
type Facts struct {
	Headers   *Headers
	ErrorPage *ErrorPage
	Health    *Health
	// Pages is the shortlist of the site's own pages; empty when the host
	// stated none and linked to none; an empty group is not rendered.
	Pages []Page
	// API is the app's own API entry point. Nil means unmeasured; a zero Path
	// means we looked and the site exposes none.
	API *API
	// Hosts is the site's other hosts. Kept apart from Pages: a separate host
	// is a separate failure domain, and one shortlist cannot serve both.
	Hosts []Page
}

// Run establishes what it can about baseURL within Budget. Empty Facts when
// the caller's probe got no response; dns nil turns host discovery off.
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
		// Do not follow redirects: a redirect to the homepage has answered
		// the question; following would report the homepage's 200.
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
