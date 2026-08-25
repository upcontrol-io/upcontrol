package discover

import (
	"context"
	"strings"
	"sync"
	"testing"
)

// bodyProber answers with a body per URL and records every URL asked for,
// so a test can assert on refusals as well as on returns.
type bodyProber struct {
	// probePages runs the shortlist concurrently, so the recorder needs a lock:
	// a lost append would quietly weaken the request-ceiling guarantee.
	mu     sync.Mutex
	body   map[string]string
	status map[string]uint16
	ms     map[string]uint32
	asked  []string
}

func (b *bodyProber) Execute(_ context.Context, spec CheckSpec) Result {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.asked = append(b.asked, spec.URL)
	code, ok := b.status[spec.URL]
	if !ok {
		if _, hasBody := b.body[spec.URL]; hasBody {
			code = 200
		} else {
			code = 404
		}
	}
	res := Result{StatusCode: code, OK: code >= 200 && code < 400, TotalMs: b.ms[spec.URL]}
	if spec.CollectBody {
		res.Body = []byte(b.body[spec.URL])
	}
	return res
}

func (b *bodyProber) askedFor(substr string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, u := range b.asked {
		if strings.Contains(u, substr) {
			return true
		}
	}
	return false
}

func paths(pages []Page) []string {
	out := make([]string, 0, len(pages))
	for _, p := range pages {
		out = append(out, p.Path)
	}
	return out
}

const base = "https://example.com"

func sitemapXML(locs ...string) string {
	var b strings.Builder
	b.WriteString(`<urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">`)
	for _, l := range locs {
		b.WriteString("<url><loc>" + l + "</loc></url>")
	}
	b.WriteString(`</urlset>`)
	return b.String()
}

func TestPagesFromSitemap(t *testing.T) {
	p := &bodyProber{
		body: map[string]string{
			base + "/robots.txt":  "User-agent: *\nSitemap: " + base + "/sitemap.xml\n",
			base + "/sitemap.xml": sitemapXML(base+"/", base+"/pricing", base+"/blog/2019/07/a-post"),
		},
		ms: map[string]uint32{base + "/pricing": 480},
	}
	pages := findPagesOnly(p, nil)
	got := paths(pages)
	if len(got) != 2 {
		t.Fatalf("paths = %v, want /pricing and the blog post", got)
	}
	// A named, shallow page outranks a deep dated entry.
	if got[0] != "/pricing" {
		t.Errorf("first = %q, want /pricing", got[0])
	}
	// The root is the Live probe row's job; offering it here would spend one of
	// three free checks on a duplicate.
	for _, path := range got {
		if path == "/" {
			t.Error("root offered as a page")
		}
	}
	if pages[0].Source != "in sitemap.xml" {
		t.Errorf("source = %q", pages[0].Source)
	}
	if !pages[0].Slowest {
		t.Error("the slowest page is not marked, which is the point of probing")
	}
}

func TestPagesFallBackToHomepageLinksWithoutASitemap(t *testing.T) {
	// The common case for a small site: no sitemap at all. The homepage body is
	// already in hand, so this path costs no extra request.
	p := &bodyProber{body: map[string]string{}}
	homepage := []byte(`<a href="/pricing">P</a><a href="/login">L</a>
	  <a href="https://twitter.com/x">out</a><a href="mailto:a@b.c">mail</a>`)
	pages := findPagesOnly(p, homepage)
	got := paths(pages)
	if len(got) != 2 {
		t.Fatalf("paths = %v, want the two internal links", got)
	}
	if pages[0].Source != "linked from the homepage" {
		t.Errorf("source = %q", pages[0].Source)
	}
	if p.askedFor("twitter.com") {
		t.Error("followed an outbound link")
	}
}

func TestSitemapCannotSendUsToAnotherHost(t *testing.T) {
	// A sitemap is somebody else's file. Naming a foreign domain — or an
	// internal address behind one — must not turn into a request.
	p := &bodyProber{
		body: map[string]string{
			base + "/robots.txt": "Sitemap: " + base + "/sitemap.xml\n",
			base + "/sitemap.xml": sitemapXML(
				"http://169.254.169.254/latest/meta-data/",
				"https://victim.example.net/expensive",
				base+"/pricing",
			),
		},
	}
	pages := findPagesOnly(p, nil)
	if p.askedFor("169.254.169.254") {
		t.Error("requested a metadata address named by the sitemap")
	}
	if p.askedFor("victim.example.net") {
		t.Error("requested a foreign host named by the sitemap")
	}
	if got := paths(pages); len(got) != 1 || got[0] != "/pricing" {
		t.Errorf("paths = %v, want only /pricing", got)
	}
}

func TestSitemapIndexDescendsExactlyOneLevel(t *testing.T) {
	index := `<sitemapindex xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">` +
		`<sitemap><loc>` + base + `/sitemap-1.xml</loc></sitemap>` +
		`<sitemap><loc>` + base + `/sitemap-2.xml</loc></sitemap>` +
		`<sitemap><loc>` + base + `/sitemap-3.xml</loc></sitemap>` +
		`</sitemapindex>`
	p := &bodyProber{
		body: map[string]string{
			base + "/sitemap.xml":   index,
			base + "/sitemap-1.xml": sitemapXML(base + "/pricing"),
			base + "/sitemap-2.xml": sitemapXML(base + "/login"),
			base + "/sitemap-3.xml": sitemapXML(base + "/docs"),
		},
	}
	findPagesOnly(p, nil)
	// Budget is index + sitemapMaxChildren, so the third child is never fetched:
	// each level multiplies requests against a host that never asked us here.
	if p.askedFor("sitemap-3.xml") {
		t.Errorf("descended past the child budget: %v", p.asked)
	}
}

func TestRobotsDisallowIsHonoured(t *testing.T) {
	p := &bodyProber{
		body: map[string]string{
			base + "/robots.txt":  "User-agent: *\nDisallow: /admin\nSitemap: " + base + "/sitemap.xml\n",
			base + "/sitemap.xml": sitemapXML(base+"/admin/secret", base+"/pricing"),
		},
	}
	pages := findPagesOnly(p, nil)
	if p.askedFor("/admin") {
		t.Error("requested a path robots.txt disallowed")
	}
	if got := paths(pages); len(got) != 1 || got[0] != "/pricing" {
		t.Errorf("paths = %v, want only /pricing", got)
	}
}

func TestRobotsGroupForUsWinsOverTheWildcard(t *testing.T) {
	r := parseRobots("User-agent: *\nDisallow: /\n\nUser-agent: upcontrol\nDisallow: /admin\n")
	if !r.allowed("/pricing") {
		t.Error("/pricing blocked by the wildcard group, but a group names us")
	}
	if r.allowed("/admin/x") {
		t.Error("/admin allowed, but our own group disallows it")
	}
}

func TestRobotsEmptyDisallowMeansEverythingAllowed(t *testing.T) {
	// "Disallow:" with no value is the documented way to allow everything.
	// Stored as a prefix it would match every path and mute the whole feature.
	r := parseRobots("User-agent: *\nDisallow:\n")
	if !r.allowed("/anything") {
		t.Error("an empty Disallow blocked everything")
	}
}

func TestRankDropsAssetsAndDeduplicates(t *testing.T) {
	cands := []candidate{
		{URL: base + "/style.css"}, {URL: base + "/logo.png"}, {URL: base + "/feed.xml"},
		{URL: base + "/pricing?utm_source=x"}, {URL: base + "/pricing"},
		{URL: base + "/pricing#top"}, {URL: base + "/docs/"},
	}
	got := rank(base, cands, robots{})
	if len(got) != 2 {
		t.Fatalf("rank = %v, want /pricing and /docs once each", got)
	}
	for _, c := range got {
		if strings.Contains(c.URL, ".css") || strings.Contains(c.URL, ".png") || strings.Contains(c.URL, ".xml") {
			t.Errorf("asset kept: %s", c.URL)
		}
		if strings.Contains(c.URL, "?") || strings.Contains(c.URL, "#") {
			t.Errorf("query or fragment kept: %s", c.URL)
		}
	}
}

func TestRankCapsTheShortlistHoweverLongTheSitemap(t *testing.T) {
	// The ceiling that makes MaxRequests a promise rather than a hope.
	var cands []candidate
	for i := range 5000 {
		cands = append(cands, candidate{URL: base + "/page-" + string(rune('a'+i%26)) + string(rune('a'+i/26%26))})
	}
	if got := rank(base, cands, robots{}); len(got) > PagesWanted {
		t.Errorf("shortlist = %d, want at most %d", len(got), PagesWanted)
	}
}

func TestSitemapPriorityBreaksTiesTheSiteOwnersWay(t *testing.T) {
	cands := []candidate{
		{URL: base + "/alpha", Priority: "0.1"},
		{URL: base + "/beta", Priority: "1.0"},
	}
	got := rank(base, cands, robots{})
	if len(got) != 2 || !strings.HasSuffix(got[0].URL, "/beta") {
		t.Errorf("rank = %v, want /beta first on its stated priority", got)
	}
}

func TestNoPagesMeansNoGroupNotAnEmptyOne(t *testing.T) {
	p := &bodyProber{body: map[string]string{}}
	if pages := findPagesOnly(p, nil); pages != nil {
		t.Errorf("pages = %v, want nil so the caller renders no group at all", pages)
	}
}

func TestDiscoveryStaysUnderTheRequestCeiling(t *testing.T) {
	// The promise MaxRequests makes, tested against the worst input we accept:
	// robots naming three sitemaps, an index under each, and a long URL list.
	var locs []string
	for i := range 400 {
		locs = append(locs, base+"/p"+string(rune('a'+i%26))+string(rune('a'+i/26%26)))
	}
	p := &bodyProber{body: map[string]string{
		base + "/robots.txt": "Sitemap: " + base + "/a.xml\nSitemap: " + base + "/b.xml\nSitemap: " + base + "/c.xml\n",
		base + "/a.xml":      sitemapXML(locs...),
		base + "/b.xml":      sitemapXML(locs...),
		base + "/c.xml":      sitemapXML(locs...),
	}}
	facts := Run(context.Background(), p, nil, base, Result{StatusCode: 200, OK: true})
	if len(facts.Pages) > PagesWanted {
		t.Errorf("pages = %d, want at most %d", len(facts.Pages), PagesWanted)
	}
	if len(p.asked) > MaxRequests {
		t.Errorf("made %d requests, want at most %d: %v", len(p.asked), MaxRequests, p.asked)
	}
}

// findPagesOnly keeps the page tests reading about pages: host discovery has its
// own budget and its own file.
func findPagesOnly(p Prober, homepage []byte) []Page {
	pages, _ := findPages(context.Background(), p, nil, base, homepage)
	return pages
}
