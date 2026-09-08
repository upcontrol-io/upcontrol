package api

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// Every path the contract promises has to be REGISTERED, and nothing else in
// this suite can tell. The handler tests drive ServeHTTP directly, which skips
// the mux entirely, so a path present in openapi.yaml and answered by a handler
// still 404s in production if nobody wired it up. That is not hypothetical: it
// shipped twice. `POST /v1/recipients/{id}/resend` was dead the whole time —
// http.ServeMux patterns are exact per segment, so "POST /v1/recipients/{id}"
// never matched it — and the board's three new doors were about to go the same
// way (both found 2026-09-08, by curling a running server rather than by a
// test).
//
// The check is textual on purpose: building the real mux needs a pool, a
// session manager, a mailer and a config, and a test that heavy would be
// skipped in exactly the environment that needs it.
func TestEveryContractPathIsRegisteredInTheMux(t *testing.T) {
	spec, err := os.ReadFile("../../api/openapi.yaml")
	if err != nil {
		t.Fatalf("read the contract: %v", err)
	}
	wiring, err := os.ReadFile("../../cmd/ucapi/main.go")
	if err != nil {
		t.Fatalf("read the wiring: %v", err)
	}

	// A path is a top-level key under `paths:`, which is the only place a line
	// starts with exactly two spaces, a slash and ends in a colon.
	pathLine := regexp.MustCompile(`(?m)^ {2}(/[A-Za-z0-9/{}._-]+):`)
	found := pathLine.FindAllStringSubmatch(string(spec), -1)
	if len(found) < 40 {
		t.Fatalf("only %d paths matched — the contract's shape changed and this test stopped reading it", len(found))
	}

	seen := map[string]bool{}
	for _, match := range found {
		path := match[1]
		if seen[path] {
			continue
		}
		seen[path] = true
		// The mux registers `mux.Handle("<METHOD> <path>", h)`, so the path is
		// preceded by a space and followed by the closing quote.
		if !strings.Contains(string(wiring), " "+path+`"`) {
			t.Errorf("%s is in the contract and not in cmd/ucapi/main.go: it will 404", path)
		}
	}
}
