package api

import (
	"strings"
	"testing"
)

// The no-key 503 differs by deployment: a self-host operator can open
// Settings; a hosted tenant gets 404 there, so naming it helps nobody.
func TestAINotConfiguredMessage(t *testing.T) {
	selfHost := (&WriteAPI{selfHosted: true}).aiNotConfiguredMsg()
	if !strings.Contains(selfHost, "Settings") {
		t.Errorf("self-host message must name the door that takes a key, got %q", selfHost)
	}

	hosted := (&WriteAPI{selfHosted: false}).aiNotConfiguredMsg()
	if strings.Contains(hosted, "Settings") {
		t.Errorf("hosted message sends the tenant to a door that answers 404 to them, got %q", hosted)
	}
	if hosted == "" {
		t.Error("hosted message is empty — the client would render a blank failure")
	}
}
