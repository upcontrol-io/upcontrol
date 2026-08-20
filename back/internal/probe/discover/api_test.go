package discover

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

func TestAPIBaseIsTheCommonPrefixNotTheShortestLiteral(t *testing.T) {
	// datrade.io serves its API from /api/api/v1 — a doubled segment no list of
	// conventional names would guess. Reporting the bare "/api" would name a
	// path that may not answer at all.
	bundle := `fetch("/api/api/v1/users/me");const o="/api/api/v1/orders";x("/api/api/v1/plans")`
	if got := apiBaseIn(bundle); got != "/api/api/v1" {
		t.Errorf("apiBaseIn = %q, want /api/api/v1", got)
	}
}

func TestAPIBaseCutsBackToAPathBoundary(t *testing.T) {
	// A shared prefix can end mid-segment: /api/users and /api/usage share
	// "/api/us", which is not a path anyone can request.
	if got := apiBaseIn(`a("/api/users");b("/api/usage")`); got != "/api" {
		t.Errorf("apiBaseIn = %q, want /api", got)
	}
}

func TestAPIBaseIgnoresAssetsAndFindsNothingInAPlainPage(t *testing.T) {
	if got := apiBaseIn(`<img src="/api-logo.png">`); got != "" {
		t.Errorf("apiBaseIn = %q, want empty", got)
	}
	if got := apiBaseIn(`link("/api/docs/logo.svg")`); got != "" {
		t.Errorf("apiBaseIn = %q, want empty for an asset", got)
	}
	if got := apiBaseIn(`the word api appears in this sentence`); got != "" {
		t.Errorf("apiBaseIn = %q, want empty", got)
	}
}

func TestMainBundleSkipsThirdPartyScripts(t *testing.T) {
	// Analytics and chat widgets live on other hosts. Fetching one would be a
	// request to a stranger, and their bundle is not this app's.
	html := []byte(`
		<script async src="https://www.googletagmanager.com/gtag/js?id=G-X"></script>
		<script type="module" crossorigin src="/assets/index-BL-SXdMb.js"></script>`)
	got := mainBundle(base, html)
	if got != base+"/assets/index-BL-SXdMb.js" {
		t.Errorf("mainBundle = %q", got)
	}
}

func TestAPIFromBundleConfirmsBeforeOffering(t *testing.T) {
	// A literal in a bundle is a claim; every other row on the screen is a
	// measurement, so the path is probed before it is reported.
	p := &bodyProber{
		body: map[string]string{
			base + "/app.js": `fetch("/api/api/v1/users/me");x("/api/api/v1/orders")`,
		},
		status: map[string]uint16{base + "/api/api/v1": 401},
	}
	html := []byte(`<script src="/app.js"></script>`)

	api := findAPI(context.Background(), p, base, html)
	if api == nil || api.Path != "/api/api/v1" {
		t.Fatalf("api = %+v, want /api/api/v1", api)
	}
	if api.Source != "in the app bundle" {
		t.Errorf("source = %q", api.Source)
	}
	if !p.askedFor("/api/api/v1") {
		t.Error("reported a path it never probed")
	}
}

func TestAPIFallsBackToConventionalPaths(t *testing.T) {
	// No bundle, or a bundle that says nothing: try the short conventional list.
	p := &bodyProber{
		body:   map[string]string{},
		status: map[string]uint16{base + "/api": 401},
	}
	api := findAPI(context.Background(), p, base, nil)
	if api == nil || api.Path != "/api" {
		t.Fatalf("api = %+v, want /api", api)
	}
	if api.Source != "answers directly" {
		t.Errorf("source = %q", api.Source)
	}
}

func TestAnSPA200OfItsOwnHTMLIsNotAnAPI(t *testing.T) {
	// The trap: a single-page app answers 200 with its shell for every path, so
	// a 200 alone proves nothing. JSON, or an auth refusal, is what identifies
	// an API — the refusal being an API saying "I am here, but not to you".
	html := http.Header{}
	html.Set("Content-Type", "text/html; charset=utf-8")
	if isAPIAnswer(Result{StatusCode: 200, Header: html}) {
		t.Error("an HTML 200 counted as an API")
	}

	json := http.Header{}
	json.Set("Content-Type", "application/json")
	if !isAPIAnswer(Result{StatusCode: 200, Header: json}) {
		t.Error("a JSON 200 did not count as an API")
	}
	for _, code := range []uint16{401, 403} {
		if !isAPIAnswer(Result{StatusCode: code, Header: html}) {
			t.Errorf("HTTP %d did not count as an API", code)
		}
	}
	if isAPIAnswer(Result{StatusCode: 404, Header: json}) {
		t.Error("a 404 counted as an API")
	}
}

func TestAPINoneIsMeasuredNotAbsent(t *testing.T) {
	p := &bodyProber{body: map[string]string{}}
	api := findAPI(context.Background(), p, base, nil)
	if api == nil {
		t.Fatal("api = nil, want a measured empty path")
	}
	if api.Path != "" {
		t.Errorf("Path = %q, want empty", api.Path)
	}
}

func TestAPIBudgetExhaustionReportsUnmeasured(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	p := &bodyProber{body: map[string]string{}}
	if api := findAPI(ctx, p, base, nil); api != nil {
		t.Errorf("api = %+v, want nil when the budget ran out", api)
	}
}

func TestAPIPathProbingStaysBounded(t *testing.T) {
	// Each conventional path is a request to somebody else's server.
	p := &bodyProber{body: map[string]string{}}
	findAPI(context.Background(), p, base, nil)
	n := 0
	for _, u := range p.asked {
		if strings.Contains(u, "/api") || strings.Contains(u, "/graphql") ||
			strings.Contains(u, "openapi") || strings.Contains(u, "swagger") {
			n++
		}
	}
	if n > apiMaxPaths {
		t.Errorf("made %d api probes, want at most %d: %v", n, apiMaxPaths, p.asked)
	}
}
