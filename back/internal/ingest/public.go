// The public-key half of POST /i: a key that may live in a browser bundle is
// gated on three axes a secret key never sees — an exact Origin match, a rate
// limit per key+IP, and named events only. The gates themselves live in
// Handle; this file holds the pieces they call.

package ingest

import (
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"go.upcontrol.io/back/internal/ingest/decode"
	"go.upcontrol.io/back/internal/ingest/normalize"
)

// clientIP is the rate limiter's second key, beside the API key. Deliberately a copy of
// analytics.ClientIP rather than an import: internal/analytics reaches storage/pg, which
// reaches this package, so importing it here closes the graph into a cycle. Nine lines are
// cheaper than carving out a seam for one helper.
func clientIP(r *http.Request) string {
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

// Public rate limit: the one credential unattended traffic can drive, so it is
// the only ingest credential limited at all. Fixed window per (key, client IP).
const (
	publicRateWindow = time.Minute
	publicRateMax    = 120  // hits per key, per IP, per window
	publicRetryAfter = "60" // the whole window; it empties within one
	sweepAt          = 10000
)

// preflightMaxAge is how long a browser may cache the OPTIONS /i answer.
const preflightMaxAge = "600"

// matchOrigin returns the key's origin the request presented, or "" when the
// Origin header matches none of them. Exact bytes only — no wildcards, no
// suffixes, no scheme folding — and an absent Origin matches nothing. An empty
// origins list is always a refusal, never "any": a public key with no domain
// is the unscoped key it exists to replace.
func matchOrigin(presented string, origins []string) string {
	if presented == "" {
		return ""
	}
	for _, o := range origins {
		if presented == o {
			return presented
		}
	}
	return ""
}

// onlyEvents keeps the records that name an event — the one shape a public key
// may write. Anything else (a plain log line, a metric reading) is dropped and
// tallied, not refused: a public key that could write arbitrary log lines is a
// spam vector with our storage bill attached.
func onlyEvents(recs []decode.Record, ws *warningAccumulator) []decode.Record {
	out := make([]decode.Record, 0, len(recs))
	for _, rec := range recs {
		if ev := normalize.Classify(rec.Message, rec.Named); ev.Name != "" {
			out = append(out, rec)
		} else {
			ws.add("public_key_logs_refused", 1)
		}
	}
	return out
}

// rateLimiter is the public-key fixed window: one mutex over one map.
//
// ponytail: fixed window (bursts up to 2x at a seam) and a sweep that runs
// only once the map crosses sweepAt — plenty for a browser beacon; per-key
// sharding or a sliding window in Redis when a public key's traffic is real
// enough to notice.
type rateLimiter struct {
	mu   sync.Mutex
	hits map[string]rateWindow
}

type rateWindow struct {
	window time.Time
	count  int
}

func newRateLimiter() *rateLimiter { return &rateLimiter{hits: map[string]rateWindow{}} }

// allow reports whether (key, ip) is inside this window's budget, stamping the
// hit when it is.
func (l *rateLimiter) allow(key, ip string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	k := key + "\x00" + ip
	win := now.Truncate(publicRateWindow)
	if len(l.hits) >= sweepAt {
		// Sweep finished windows so a flood of fresh IPs cannot grow the map
		// without bound; entries in the current window survive.
		for ek, h := range l.hits {
			if !h.window.Equal(win) {
				delete(l.hits, ek)
			}
		}
	}
	h := l.hits[k]
	if !h.window.Equal(win) {
		h = rateWindow{window: win}
	}
	if h.count >= publicRateMax {
		l.hits[k] = h
		return false
	}
	h.count++
	l.hits[k] = h
	return true
}

// HandlePreflight is OPTIONS /i: 204 always, with the CORS headers only when
// the query string carries a public key whose origins include the request's
// Origin. A preflight has no body and browsers send no custom headers on it,
// so the key can only ride the query string; anything less is a bare 204 —
// the browser reads that as "no CORS" and never fires the POST, and nothing
// leaks which half failed.
func (h *Ingester) HandlePreflight(w http.ResponseWriter, r *http.Request) {
	key := r.URL.Query().Get("key")
	if key == "" || h.d.Keys == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	tenant, err := h.d.Keys.Resolve(r.Context(), key)
	if err != nil || tenant.Kind != KeyKindPublic {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	origin := matchOrigin(r.Header.Get("Origin"), tenant.Origins)
	if origin == "" {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	w.Header().Set("Access-Control-Allow-Origin", origin)
	w.Header().Set("Vary", "Origin")
	w.Header().Set("Access-Control-Allow-Methods", "POST")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type, X-Upcontrol-Key, Authorization")
	w.Header().Set("Access-Control-Max-Age", preflightMaxAge)
	w.WriteHeader(http.StatusNoContent)
}
