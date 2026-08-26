// Package cardinality caps the distinct values per low-cardinality field
// (host, service, …); over the ceiling, unseen values become the sentinel.
package cardinality

import "sync"

// Sentinel is the replacement value for over-cap distinct inputs.
const Sentinel = "__over__"

// DefaultCeiling is applied when a Limiter is built with New and no override.
const DefaultCeiling = 1000

// Limiter tracks per-field cardinality for one tenant; build one with New.
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

// Add records value under field and returns the string to store, or Sentinel
// for a new value on a field already over ceiling; warned fires once per field.
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
			// This new value does not earn a slot: it becomes the sentinel and the
			// field tips over, leaving exactly ceiling real values + __over__.
			ft.full = true
			return Sentinel, true
		}
		ft.seen[value] = struct{}{}
		return value, false
	}
	// Over ceiling and new: collapse to the sentinel; the stored dictionary
	// gains nothing.
	return Sentinel, false
}
