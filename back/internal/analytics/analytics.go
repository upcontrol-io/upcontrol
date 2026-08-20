// Package analytics is the first-party product-analytics core (plan:
// product-analytics): event validation, the async recorder that fans visitor
// events into ClickHouse and visitor state into Postgres, the hand-written UA
// parser, the embedded GeoIP resolver and the uc_vid visitor cookie.
//
// Privacy contract (§Decision 11): no third parties, no fingerprints, and a
// full client IP is never stored — the raw IP is used once at enqueue time for
// a country lookup and a truncated sha256, then discarded.
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

// Request-level caps (§Decision 10). The per-event caps live in validate.
const (
	MaxEventsPerRequest = 20
	MaxBodyBytes        = 64 << 10
)

// Per-event caps (§Decision 10): path ≤256, title ≤128, referrer ≤512,
// utm fields ≤128, props ≤16 keys with key ≤64 and value ≤200.
const (
	maxPathLen     = 256
	maxTitleLen    = 128
	maxReferrerLen = 512
	maxUTMLen      = 128
	maxProps       = 16
	maxPropKeyLen  = 64
	maxPropValLen  = 200
)

// eventNameRe is the closed name grammar (§Decision 10): lowercase letters,
// digits, underscore and dot, 1..64 chars. Anything else is dropped, so a
// hostile client cannot smuggle arbitrary strings into the name column.
var eventNameRe = regexp.MustCompile(`^[a-z0-9_.]{1,64}$`)

// Event is one analytics event, client- or server-originated. All string
// fields arrive untrusted: sanitize strips control characters, validate
// enforces the caps, and the endpoint drops invalid events individually.
type Event struct {
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
// event carries. Control bytes in analytics strings are never meaningful and
// make logs and downstream consumers unhappy; the values are also capped in
// validate, so a hostile blob cannot ride through either field.
func stripControl(s string) string {
	return strings.Map(func(r rune) rune {
		if r >= 0x20 && r != 0x7f {
			return r
		}
		return -1
	}, s)
}

// sanitize mutates e in place: strips control chars everywhere.
func (e *Event) sanitize() {
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

// validate reports whether the (already sanitized) event satisfies every cap.
// One bad field drops the whole event — the event is the unit of trust, and a
// truncated string is a lie about what the visitor did.
func validate(e Event) bool {
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

// Payload is the POST /public/track body shape.
type Payload struct {
	Events []Event `json:"events"`
}

// ParseBody decodes and validates a /public/track body. It never fails: a
// malformed or oversized body yields zero events (the endpoint still 204s —
// a collector that answers errors teaches clients to retry), invalid events
// are dropped individually and counted so the loss is visible in logs, and
// only the first MaxEventsPerRequest events survive.
func ParseBody(rd io.Reader) (kept []Event, dropped int) {
	var p Payload
	if err := json.NewDecoder(io.LimitReader(rd, MaxBodyBytes+1)).Decode(&p); err != nil {
		return nil, 0
	}
	if len(p.Events) > MaxEventsPerRequest {
		dropped += len(p.Events) - MaxEventsPerRequest
		p.Events = p.Events[:MaxEventsPerRequest]
	}
	kept = make([]Event, 0, len(p.Events))
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

// Scope is the request-scoped analytics context the public doors attach to
// their request context before any recorder call. Token is the raw uc_vid
// cookie ("" = anonymous); IP and UA are used exactly once, at enqueue time
// (country lookup + parse + truncated hash), and never stored raw.
type Scope struct {
	Token string
	IP    string
	UA    string
}

type scopeKey struct{}

// WithScope attaches s to ctx so downstream code (auth redeem, account
// provisioning) can fire server events with the same visitor identity the
// originating request carried.
func WithScope(ctx context.Context, s *Scope) context.Context {
	return context.WithValue(ctx, scopeKey{}, s)
}

// ScopeFrom returns the scope attached to ctx, or nil.
func ScopeFrom(ctx context.Context) *Scope {
	s, _ := ctx.Value(scopeKey{}).(*Scope)
	return s
}

// ScopeFromRequest builds a Scope from the request: the uc_vid cookie, the
// client IP (X-Forwarded-For first — Caddy sets it; same rule as
// api.clientIPFrom, duplicated here because api imports analytics, not the
// other way round), and the User-Agent.
func ScopeFromRequest(r *http.Request) *Scope {
	tok, _ := VisitorToken(r)
	return &Scope{Token: tok, IP: ClientIP(r), UA: r.UserAgent()}
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
