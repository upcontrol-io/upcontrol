// Package normalize decides whether a line's message is a NAMED event -- one
// something queries -- rather than an ordinary log line. A name is declared by
// the sender (the uc.event wire marker) or it does not exist: an unmarked line
// is identified by its fingerprint, and a derived identity is not addressable
// as a name in alert rules.
package normalize

import "strings"

// ReservedPrefix is the namespace upcontrol owns; clients may not send it.
const ReservedPrefix = "uc."

// Event is the classification result. Name is empty for an ordinary log line.
type Event struct {
	Name     string
	Reserved bool // the client used the uc.* prefix
}

// Classify reports whether the message names an event we store as one. A
// marked line declares its own name; an unmarked one falls to the heuristic.
func Classify(name string, named bool) Event {
	n := trimLower(name)
	// Reserved first: a client cannot claim the upcontrol namespace even where
	// the rest of the name would otherwise name a real event (uc.deploy).
	if strings.HasPrefix(n, ReservedPrefix) {
		return Event{Reserved: true}
	}
	if named {
		return Event{Name: n}
	}
	// Textual fallback: every already-published SDK predates the marker, and
	// deploy suppression and ucadmin's install_verified count still read these.
	if strings.HasPrefix(n, "deploy") || strings.Contains(n, "deployment") || n == "install_verified" {
		return Event{Name: n}
	}
	return Event{}
}

func trimLower(s string) string {
	// Manual trim+lower (keeps the hot path alloc-free; strings already serves
	// HasPrefix, but this stays allocation-free).
	start, end := 0, len(s)
	for start < end && (s[start] == ' ' || s[start] == '\t' || s[start] == '\n' || s[start] == '\r') {
		start++
	}
	for end > start && (s[end-1] == ' ' || s[end-1] == '\t' || s[end-1] == '\n' || s[end-1] == '\r') {
		end--
	}
	out := make([]byte, 0, end-start)
	for i := start; i < end; i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		out = append(out, c)
	}
	return string(out)
}
