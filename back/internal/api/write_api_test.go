package api

import "testing"

// Decision 9: exactly the three Explain endpoints leave the role gate — they
// are POSTs on the wire, reads in substance. Every other non-GET stays behind
// it, including paths that merely contain "explain" as a segment.
func TestExplainPath(t *testing.T) {
	for _, p := range []string{
		"/v1/incidents/123/explain",
		"/v1/logs/explain",
		"/v1/logs/explain/preview",
	} {
		if !explainPath(p) {
			t.Errorf("explainPath(%q) = false, want true", p)
		}
	}
	for _, p := range []string{
		"/v1/incidents/x",
		"/v1/channels",
	} {
		if explainPath(p) {
			t.Errorf("explainPath(%q) = true, want false", p)
		}
	}
}
