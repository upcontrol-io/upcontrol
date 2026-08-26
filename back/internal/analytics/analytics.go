// Package analytics: first-party analytics core (validation, recorder, UA
// parser, GeoIP, uc_vid). No third parties; raw IP used once, never stored.
package analytics

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"regexp"
	"strings"
)

// Request-level caps; the per-event caps live in validate.
const (
	maxEventsPerRequest = 20
	maxBodyBytes        = 64 << 10
)

// Per-event caps: path ≤256, title ≤128, referrer ≤512, utm fields ≤128,
// props ≤16 keys with key ≤64 and value ≤200.
const (
	maxPathLen     = 256
	maxTitleLen    = 128
	maxReferrerLen = 512
	maxUTMLen      = 128
	maxProps       = 16
	maxPropKeyLen  = 64
	maxPropValLen  = 200
)

// eventNameRe is the closed name grammar: lowercase, digits, underscore, dot,
// 1..64 chars; anything else drops, so no arbitrary strings reach the column.
var eventNameRe = regexp.MustCompile(`^[a-z0-9_.]{1,64}$`)

// event is one analytics event. All string fields arrive untrusted: sanitize
// strips control characters, validate enforces the caps.
type event struct {
	Name        string            `json:"name"`
	Path        string            `json:"path"`
	Title       string            `json:"title"`
	Referrer    string            `json:"referrer"`
	UTMSource   string            `json:"utm_source"`
	UTMMedium   string            `json:"utm_medium"`
	UTMCampaign string            `json:"utm_campaign"`
	Props       map[string]string `json:"props"`
}

// stripControl removes control characters (and DEL) from every string an
// event carries; values are also capped in validate.
func stripControl(s string) string {
	return strings.Map(func(r rune) rune {
		if r >= 0x20 && r != 0x7f {
			return r
		}
		return -1
	}, s)
}

// sanitize mutates e in place: strips control chars everywhere.
func (e *event) sanitize() {
	e.Name = stripControl(e.Name)
	e.Path = stripControl(e.Path)
	e.Title = stripControl(e.Title)
	e.Referrer = stripControl(e.Referrer)
	e.UTMSource = stripControl(e.UTMSource)
	e.UTMMedium = stripControl(e.UTMMedium)
	e.UTMCampaign = stripControl(e.UTMCampaign)
	for k, v := range e.Props {
		nk, nv := stripControl(k), stripControl(v)
		if nk != k || nv != v {
			delete(e.Props, k)
			e.Props[nk] = nv
		}
	}
}

// validate reports whether the sanitized event satisfies every cap. One bad
// field drops the whole event: a truncated string is a lie.
func validate(e event) bool {
	if !eventNameRe.MatchString(e.Name) {
		return false
	}
	if len(e.Path) > maxPathLen || len(e.Title) > maxTitleLen || len(e.Referrer) > maxReferrerLen {
		return false
	}
	if len(e.UTMSource) > maxUTMLen || len(e.UTMMedium) > maxUTMLen || len(e.UTMCampaign) > maxUTMLen {
		return false
	}
	if len(e.Props) > maxProps {
		return false
	}
	for k, v := range e.Props {
		if len(k) > maxPropKeyLen || len(v) > maxPropValLen {
			return false
		}
	}
	return true
}

// payload is the POST /public/track body shape.
type payload struct {
	Events []event `json:"events"`
}

// ParseBody decodes and validates a /public/track body; it never fails: a
// malformed body yields zero events, invalid ones drop individually.
func ParseBody(rd io.Reader) (kept []event, dropped int) {
	var p payload
	if err := json.NewDecoder(io.LimitReader(rd, maxBodyBytes+1)).Decode(&p); err != nil {
		return nil, 0
	}
	if len(p.Events) > maxEventsPerRequest {
		dropped += len(p.Events) - maxEventsPerRequest
		p.Events = p.Events[:maxEventsPerRequest]
	}
	kept = make([]event, 0, len(p.Events))
	for _, e := range p.Events {
		e.sanitize()
		if !validate(e) {
			dropped++
			continue
		}
		kept = append(kept, e)
	}
	return kept, dropped
}

// scope is the request-scoped analytics context. Token is the raw uc_vid
// (empty = anonymous); IP and UA are used once at enqueue, never stored raw.
type scope struct {
	Token string
	IP    string
	UA    string
}

type scopeKey struct{}

// WithScope attaches s to ctx so downstream code fires server events with the
// originating request's visitor identity.
func WithScope(ctx context.Context, s *scope) context.Context {
	return context.WithValue(ctx, scopeKey{}, s)
}

// scopeFrom returns the scope attached to ctx, or nil.
func scopeFrom(ctx context.Context) *scope {
	s, _ := ctx.Value(scopeKey{}).(*scope)
	return s
}

// ScopeFromRequest builds a scope from the uc_vid cookie, the client IP
// (X-Forwarded-For first) and the User-Agent.
func ScopeFromRequest(r *http.Request) *scope {
	tok, _ := VisitorToken(r)
	return &scope{Token: tok, IP: ClientIP(r), UA: r.UserAgent()}
}

// ClientIP prefers the first X-Forwarded-For entry (set by Caddy) then falls
// back to the peer address. One canonical copy for auth, api and analytics.
func ClientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if i := strings.IndexByte(xff, ','); i >= 0 {
			return strings.TrimSpace(xff[:i])
		}
		return strings.TrimSpace(xff)
	}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}
