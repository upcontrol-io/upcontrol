package api

import (
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
)

// The API is the gate, not the form: POST /v1/monitors accepted an empty
// target and answered 201, creating a row that consumed one of the plan's
// HTTP-check slots and handed the probe fleet an empty string to schedule.
func TestValidateMonitorCreate(t *testing.T) {
	cases := []struct {
		name, kind, target, want string
	}{
		{"empty target on a website", "website", "", "missing_target"},
		{"blank target on a website", "website", "   ", "missing_target"},
		{"target that is not a URL", "website", "not a url", "bad_target"},
		{"target with no host", "website", "https://", "bad_target"},
		{"a good https target", "website", "https://example.com/checkout", ""},
		{"a good http target", "website", "http://example.com", ""},
		// A heartbeat is pinged by the customer's job; we generate its URL, so it
		// carries no target of its own and must not be judged against one.
		{"heartbeat needs no target", "heartbeat", "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := validateMonitorCreate(c.kind, c.target); got != c.want {
				t.Fatalf("validateMonitorCreate(%q, %q) = %q, want %q", c.kind, c.target, got, c.want)
			}
		})
	}
}

// A PATCH answers with the row as it now is. It used to answer "nodata" from a
// literal, so a rename greyed a healthy check out until something re-read it.
// This pins the helper the fixed caller passes real facts through.
func TestMonitorPatchKeepsStatus(t *testing.T) {
	got := monitorRowToAPI("website", "Renamed", "https://example.com", "", 300,
		"ok", pgtype.Timestamptz{}, pgtype.Timestamptz{}, pgtype.UUID{Valid: true})
	if got["status"] != "ok" {
		t.Fatalf("status = %v, want ok", got["status"])
	}
}
