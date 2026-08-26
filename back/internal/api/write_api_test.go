package api

import "testing"

// Exactly the three Explain endpoints leave the role gate: POSTs on the wire,
// reads in substance. Paths that merely contain "explain" stay behind it.
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
