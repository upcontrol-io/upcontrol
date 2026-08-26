package discover

import (
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// hrefRe pulls href values out of HTML; a regex, not a parser, because the job
// is "find the obvious internal links", and a parser is a dependency.
var hrefRe = regexp.MustCompile(`(?i)<a\s[^>]*href\s*=\s*["']([^"'#][^"']*)["']`)

// linksFrom extracts same-host page links from a body we already hold. Costs no
// request, which is why it is the fallback rather than a second choice.
func linksFrom(base string, body []byte) []candidate {
	if len(body) == 0 {
		return nil
	}
	root, err := url.Parse(base)
	if err != nil {
		return nil
	}
	seen := map[string]bool{}
	var out []candidate
	for _, m := range hrefRe.FindAllStringSubmatch(string(body), -1) {
		ref, err := url.Parse(strings.TrimSpace(m[1]))
		if err != nil {
			continue
		}
		abs := root.ResolveReference(ref)
		if abs.Scheme != "http" && abs.Scheme != "https" {
			continue // mailto:, tel:, javascript:
		}
		// An outbound link is not this site's page. A subdomain of it is, and
		// this is the cheapest place to find one.
		if !sameSite(root.Host, abs.Host) {
			continue
		}
		clean := abs.String()
		if seen[clean] {
			continue
		}
		seen[clean] = true
		out = append(out, candidate{URL: clean, Source: "linked from the homepage"})
	}
	return out
}

// assetExt are paths that are not pages. Watching a stylesheet tells you the CDN
// is up, which is not what anyone typed their domain in to learn.
var assetExt = []string{
	".png", ".jpg", ".jpeg", ".gif", ".svg", ".webp", ".ico", ".avif",
	".css", ".js", ".mjs", ".map", ".json", ".xml", ".txt",
	".pdf", ".zip", ".gz", ".mp4", ".webm", ".woff", ".woff2", ".ttf", ".rss",
}

// valuableSection is a section whose breaking costs money; matched against
// the FIRST path segment, whole, never as a substring.
var valuableSection = map[string]bool{
	"checkout": true, "payment": true, "payments": true, "billing": true,
	"pricing": true, "plans": true, "login": true, "signin": true, "sign-in": true,
	"signup": true, "sign-up": true, "register": true, "dashboard": true,
	"account": true, "app": true, "api": true, "docs": true, "status": true,
}

// boilerplateSection is a page that exists on every site and nobody is paged
// for at 3am; not dropped, just outranked.
var boilerplateSection = map[string]bool{
	"terms": true, "privacy": true, "legal": true, "cookies": true,
	"imprint": true, "impressum": true, "tos": true,
}

// rank turns raw candidates into the shortlist, capped at pagesWanted. Pure:
// no network, no clock. The host root is dropped (the Live row has it).
func rank(base string, cands []candidate, r robots) []candidate {
	root, err := url.Parse(base)
	if err != nil {
		return nil
	}
	type scored struct {
		c     candidate
		score int
		order int
	}
	seen := map[string]bool{}
	var list []scored

	for i, c := range cands {
		u, err := url.Parse(strings.TrimSpace(c.URL))
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
			continue
		}
		// A sitemap is attacker-controlled input: it can name other domains;
		// not making the request at all is cheaper than the guard refusing it.
		if !sameSite(root.Host, u.Host) {
			continue
		}
		// Another host is a different failure domain; findHosts gives it its
		// own budget, so competing here would cost the site two slots.
		if u.Host != root.Host {
			continue
		}
		u.Fragment = ""
		u.RawQuery = "" // ?utm_source=… is the same page
		path := u.Path
		if path == "" {
			path = "/"
		}
		if strings.HasSuffix(path, "/") && path != "/" {
			path = strings.TrimRight(path, "/")
			u.Path = path
		}
		// The Live probe row covers THIS host's root.
		if path == "/" {
			continue
		}
		if isAsset(path) || !r.allowed(path) {
			continue
		}
		clean := u.String()
		if seen[clean] {
			continue
		}
		seen[clean] = true

		c.URL = clean
		list = append(list, scored{c: c, score: scoreOf(path, c), order: i})
	}

	sort.SliceStable(list, func(a, b int) bool {
		if list[a].score != list[b].score {
			return list[a].score > list[b].score
		}
		return list[a].order < list[b].order // stable: document order breaks ties
	})
	if len(list) > pagesWanted {
		list = list[:pagesWanted]
	}
	out := make([]candidate, 0, len(list))
	for _, s := range list {
		out = append(out, s.c)
	}
	return out
}

// scoreOf ranks by entry points, not articles: a sitemap is mostly articles,
// and without the depth penalty the site's front doors never appear.
func scoreOf(path string, c candidate) int {
	score := 0
	segments := strings.Split(strings.Trim(path, "/"), "/")
	depth := len(segments)
	if path == "" || path == "/" {
		depth = 0
	}
	first := ""
	if depth > 0 {
		first = strings.ToLower(segments[0])
	}

	switch {
	case depth == 1:
		score += 40 // a section: the site's own front doors
	case depth >= 2:
		score -= 20 * (depth - 1) // an article, and deeper is worse
	}

	if valuableSection[first] {
		score += 50
	}
	if boilerplateSection[first] {
		score -= 30
	}

	// The site's own opinion, when it stated one. Absent priority is not zero
	// priority — the sitemap default is 0.5, so an absent value scores as that.
	priority := 0.5
	if c.Priority != "" {
		if p, err := strconv.ParseFloat(strings.TrimSpace(c.Priority), 64); err == nil {
			priority = p
		}
	}
	score += int(priority * 20)
	return score
}

func isAsset(path string) bool {
	lower := strings.ToLower(path)
	for _, ext := range assetExt {
		if strings.HasSuffix(lower, ext) {
			return true
		}
	}
	return false
}

// sameSite reports whether host is the requested host or one of its subdomains;
// suffix-matched against "."+root so it can only widen to a name the domain owns.
func sameSite(root, host string) bool {
	return host == root || strings.HasSuffix(host, "."+root)
}

// sameHost reports whether raw points at base's site. Used before every fetch
// this package makes on a URL it did not construct itself.
func sameHost(base, raw string) bool {
	root, err := url.Parse(base)
	if err != nil {
		return false
	}
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return false
	}
	return sameSite(root.Host, u.Host)
}
