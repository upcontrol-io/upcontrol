// Package cardinality caps the number of distinct values a tenant can put in a
// low-cardinality field (host, service, …). ClickHouse stores these as
// LowCardinality(String): a runaway field (request-ids dumped into `host`) would
// blow up the dictionary and starve the column's compression. Once a field
// exceeds its ceiling, NEW unseen values are replaced with the sentinel
// "__over__" and a cardinality_capped warning rides the receipt (plan §3.5).
//
// A bounded exact set per field holds up to `ceiling` distinct values and is the
// source of truth for the cap DECISION (is this value one we've seen?).
//
// The guarantee the §3.1...§3.5 gate tests: with ceiling C, the stored field
// never carries more than C+1 distinct values (the C real ones plus __over__).
package cardinality

import "sync"

// Sentinel is the replacement value for over-cap distinct inputs.
const Sentinel = "__over__"

// DefaultCeiling is applied when a Limiter is built with New and no override.
const DefaultCeiling = 1000

// Limiter tracks per-field cardinality for one tenant. The zero value is not
// usable; build one with New.
type Limiter struct {
	ceiling int

	mu     sync.Mutex
	fields map[string]*fieldTracker
}

type fieldTracker struct {
	seen map[string]struct{} // exact, up to ceiling
	full bool                // set once seen reached ceiling
}

// New builds a Limiter with the given ceiling (distinct values allowed per field
// before capping).
func New(ceiling int) *Limiter {
	if ceiling <= 0 {
		ceiling = DefaultCeiling
	}
	return &Limiter{ceiling: ceiling, fields: map[string]*fieldTracker{}}
}

// Add records value under field and returns the string to actually store: the
// value itself if it is known or the field is under ceiling, or Sentinel if the
// field is over ceiling and the value is new. The warned flag is true the first
// time a field tips over (the caller raises cardinality_capped once per field).
func (l *Limiter) Add(field, value string) (store string, warned bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	ft := l.fields[field]
	if ft == nil {
		ft = &fieldTracker{seen: map[string]struct{}{}}
		l.fields[field] = ft
	}
	if _, ok := ft.seen[value]; ok {
		return value, false // known value: pass through, no warning
	}
	if !ft.full {
		if len(ft.seen) >= l.ceiling {
			// This new value is the one that would cross the line: it does NOT earn
			// a slot — it becomes the sentinel, and the field tips over. The stored
			// dictionary is then exactly ceiling real values + __over__.
			ft.full = true
			return Sentinel, true
		}
		ft.seen[value] = struct{}{}
		return value, false
	}
	// Over ceiling and this value is new: collapse to the sentinel. The stored
	// dictionary gains nothing.
	return Sentinel, false
}

// Distinct returns the exact count of currently-tracked values for a field
// (bounded by ceiling). For tests/observability.
func (l *Limiter) Distinct(field string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	ft := l.fields[field]
	if ft == nil {
		return 0
	}
	return len(ft.seen)
}
