package discover

import (
	"context"
	"net/http"
	"regexp"
	"sort"
	"strings"
)

// API is the app's own API entry point. An empty Path is a measured answer ("we
// looked and found none"), never an absence — the caller renders that as "none",
// the same way it does for the health URL.
type API struct {
	Path   string
	Source string // "in the app bundle" | "answers directly"
	Status uint16
	// Confirmed reports whether the path itself answered like an API. A base
	// lifted from a bundle often does not: datrade.io's app talks to /api/api/v1,
	// so /api/api is a real prefix that returns 404 on its own. Worth telling the
	// reader where the API lives, but not worth pretending the root is watchable.
	Confirmed bool
}

// apiPaths are the conventional entry points, tried when the bundle says
// nothing. Kept short: each one is a request to somebody else's server.
var apiPaths = []string{"/api", "/api/v1", "/graphql", "/openapi.json", "/swagger.json"}

const (
	// The bundle is read up to this much. An SPA's main chunk is usually a few
	// hundred KB; past a megabyte we are downloading someone's whole app to find
	// one string, which is not a trade this feature is worth.
	bundleMaxBytes = 1 << 20
	apiMaxPaths    = 3
)

var (
	scriptSrcRe = regexp.MustCompile(`(?i)<script[^>]+src\s*=\s*["']([^"']+\.js[^"']*)["']`)
	// Path literals inside the bundle. Quoted, rooted, and long enough to be a
	// route rather than the word "api" in a sentence.
	apiLiteralRe = regexp.MustCompile(`["'` + "`" + `](/(?:api|graphql)[a-zA-Z0-9/_.-]*)["'` + "`" + `]`)
)

// findAPI locates the app's API. The bundle comes first because it is one
// request and gives the real base path — datrade.io serves its API from
// /api/api/v1, which no list of conventional names would have guessed — and the
// conventional paths are the fallback for apps whose bundle says nothing.
//
// This exists because an SPA hides its API from everything else we do: the
// homepage carries no links, the sitemap does not exist, and the call itself
// happens in JavaScript we do not execute. The base URL is nevertheless written
// down, in the bundle the browser downloads anyway.
func findAPI(ctx context.Context, p Prober, base string, homepage []byte) *API {
	if api := apiFromBundle(ctx, p, base, homepage); api != nil {
		return api
	}
	return apiFromPaths(ctx, p, base)
}

func apiFromBundle(ctx context.Context, p Prober, base string, homepage []byte) *API {
	src := mainBundle(base, homepage)
	if src == "" {
		return nil
	}
	res := p.Execute(ctx, CheckSpec{
		URL: src, Method: http.MethodGet,
		TimeoutMs:    uint32(perRequestTimeout.Milliseconds()),
		MaxRedirects: 2,
		MaxBodyBytes: bundleMaxBytes,
		CollectBody:  true,
	})
	if len(res.Body) == 0 {
		return nil
	}
	path := apiBaseIn(string(res.Body))
	if path == "" {
		return nil
	}
	// Confirm it answers before offering it: a literal in a bundle is a claim,
	// and every other row on this screen is a measurement.
	probe := p.Execute(ctx, CheckSpec{
		URL: strings.TrimRight(base, "/") + path, Method: http.MethodGet,
		TimeoutMs: uint32(perRequestTimeout.Milliseconds()), MaxRedirects: 1,
	})
	if probe.StatusCode == 0 {
		return nil
	}
	return &API{
		Path: path, Source: "in the app bundle",
		Status: probe.StatusCode, Confirmed: isAPIAnswer(probe),
	}
}

// mainBundle picks the app's own script out of the page. Third-party tags
// (analytics, chat widgets) are on other hosts and are skipped: their bundles
// are not this app's, and fetching them would be a request to a stranger.
func mainBundle(base string, homepage []byte) string {
	if len(homepage) == 0 {
		return ""
	}
	var same []string
	for _, m := range scriptSrcRe.FindAllStringSubmatch(string(homepage), -1) {
		src := strings.TrimSpace(m[1])
		switch {
		case strings.HasPrefix(src, "//"), strings.HasPrefix(src, "http"):
			if !sameHost(base, absolute(base, src)) {
				continue
			}
		case !strings.HasPrefix(src, "/"):
			continue // a relative path we would have to resolve against the page
		}
		same = append(same, absolute(base, src))
	}
	if len(same) == 0 {
		return ""
	}
	// The entry chunk is normally the last module script on the page.
	return same[len(same)-1]
}

func absolute(base, src string) string {
	switch {
	case strings.HasPrefix(src, "//"):
		return "https:" + src
	case strings.HasPrefix(src, "http"):
		return src
	default:
		return strings.TrimRight(base, "/") + src
	}
}

// apiBaseIn returns the common base of the API routes written in a bundle.
//
// The longest common prefix, not the shortest literal: a bundle mentioning
// /api/api/v1/users/me and /api/api/v1/orders describes an API rooted at
// /api/api/v1, and reporting the bare /api would name a path that may not even
// answer.
func apiBaseIn(bundle string) string {
	seen := map[string]bool{}
	var routes []string
	for _, m := range apiLiteralRe.FindAllStringSubmatch(bundle, -1) {
		route := m[1]
		// Assets under /api… are not routes, and a bare "/api" alone tells us
		// nothing a probe would not.
		if isAsset(route) || seen[route] {
			continue
		}
		seen[route] = true
		routes = append(routes, route)
	}
	if len(routes) == 0 {
		return ""
	}
	sort.Strings(routes)
	prefix := routes[0]
	for _, r := range routes[1:] {
		prefix = commonPrefix(prefix, r)
		if prefix == "" {
			break
		}
	}
	// Cut back to a path boundary: a shared prefix can end mid-segment
	// ("/api/us" from /api/users and /api/usage), which is not a path.
	if i := strings.LastIndexByte(prefix, '/'); i > 0 {
		prefix = prefix[:i]
	}
	if prefix == "" || prefix == "/" {
		return ""
	}
	return prefix
}

func commonPrefix(a, b string) string {
	n := min(len(a), len(b))
	i := 0
	for i < n && a[i] == b[i] {
		i++
	}
	return a[:i]
}

// apiFromPaths tries the conventional entry points. The catch an SPA sets is
// that it answers 200 with its own HTML for every path, so a 200 alone proves
// nothing: what identifies an API is a JSON answer, or an authentication
// refusal, which is itself an API saying "I am here, but not to you".
func apiFromPaths(ctx context.Context, p Prober, base string) *API {
	root := strings.TrimRight(base, "/")
	tried := 0
	for _, path := range apiPaths {
		if ctx.Err() != nil {
			return nil // out of budget with candidates left: not "none"
		}
		if tried >= apiMaxPaths {
			// Our own cap, not a timeout. We searched everything we were willing
			// to ask for, so the honest answer is "none" — nil would claim the
			// search never finished.
			break
		}
		tried++
		res := p.Execute(ctx, CheckSpec{
			URL: root + path, Method: http.MethodGet,
			TimeoutMs: uint32(perRequestTimeout.Milliseconds()), MaxRedirects: 1,
		})
		if isAPIAnswer(res) {
			return &API{Path: path, Source: "answers directly", Status: res.StatusCode, Confirmed: true}
		}
	}
	return &API{} // looked everywhere on the list, found none
}

func isAPIAnswer(res Result) bool {
	if res.StatusCode == http.StatusUnauthorized || res.StatusCode == http.StatusForbidden {
		return true // an API refusing us is still an API
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return false
	}
	ct := ""
	if res.Header != nil {
		ct = strings.ToLower(res.Header.Get("Content-Type"))
	}
	return strings.Contains(ct, "json")
}
