package discover

import (
	"context"
	"net/http"
	"strings"
)

// robots is what /robots.txt told us. A host without one, or one we could not
// read, yields the zero value — which allows everything, because a missing
// robots.txt means "no restrictions stated", not "stay out".
type robots struct {
	sitemaps []string
	disallow []string
}

// Groups are matched for "*" and for our own token. A record naming us
// specifically wins over the wildcard, which is what the convention says and
// also the only reading under which an operator can single us out.
const robotsUAToken = "upcontrol"

const (
	robotsMaxBytes    = 64 << 10
	robotsMaxSitemaps = 3
)

// fetchRobots reads /robots.txt. It is the first thing this package asks for,
// because from the moment we request site pages we are a crawler, and a crawler
// that has not read robots.txt is just a scraper.
func fetchRobots(ctx context.Context, p Prober, base string) robots {
	res := p.Execute(ctx, CheckSpec{
		URL: base + "/robots.txt", Method: http.MethodGet,
		TimeoutMs:    uint32(perRequestTimeout.Milliseconds()),
		MaxRedirects: 2,
		MaxBodyBytes: robotsMaxBytes,
		CollectBody:  true,
	})
	if res.StatusCode < 200 || res.StatusCode >= 300 || len(res.Body) == 0 {
		return robots{}
	}
	return parseRobots(string(res.Body))
}

// parseRobots is deliberately a small subset: Sitemap directives (which are
// global, not per-group) and Disallow paths for the groups that apply to us.
// Allow, Crawl-delay and wildcard patterns are not honoured because this package
// fetches at most five pages once per host per cache window — the cases those
// rules exist for cannot arise here.
func parseRobots(body string) robots {
	var r robots
	// applies tracks whether the group being read is one of ours. Until the
	// first User-agent line there is no group, so Disallow lines are ignored.
	applies := false
	specific := false // a group naming us explicitly was seen
	for _, raw := range strings.Split(body, "\n") {
		line := strings.TrimSpace(raw)
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = strings.TrimSpace(line[:i])
		}
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		key = strings.ToLower(strings.TrimSpace(key))
		value = strings.TrimSpace(value)
		switch key {
		case "sitemap":
			// Global directive: valid anywhere in the file, regardless of group.
			if value != "" && len(r.sitemaps) < robotsMaxSitemaps {
				r.sitemaps = append(r.sitemaps, value)
			}
		case "user-agent":
			agent := strings.ToLower(value)
			if strings.Contains(agent, robotsUAToken) {
				if !specific {
					// A record naming us replaces whatever the wildcard said.
					r.disallow = nil
					specific = true
				}
				applies = true
			} else {
				applies = agent == "*" && !specific
			}
		case "disallow":
			// "Disallow:" with an empty value means "nothing is disallowed" and
			// must not be stored as a prefix, or it would match every path.
			if applies && value != "" {
				r.disallow = append(r.disallow, value)
			}
		}
	}
	return r
}

// allowed reports whether path may be requested. Prefix matching, which is what
// the un-wildcarded form of the directive means.
func (r robots) allowed(path string) bool {
	if path == "" {
		path = "/"
	}
	for _, prefix := range r.disallow {
		if strings.HasPrefix(path, prefix) {
			return false
		}
	}
	return true
}
