package api

import (
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
)

// The API is the gate, not the form: an empty target must not create a row
// that consumes an HTTP-check slot and schedules an empty string.
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

// The four cadences the contract carries, and nothing else: a value outside
// them must be refused, never rounded into a 5m check nobody asked for.
func TestParseInterval(t *testing.T) {
	cases := []struct {
		in   string
		sec  int32
		want bool
	}{
		{"1m", 60, true},
		{"5m", 300, true},
		{"30m", 1800, true},
		{"1h", 3600, true},
		{"6h", 21600, true},
		{"1d", 86400, true},
		{"", 0, false},
		{"2m", 0, false},
	}
	for _, c := range cases {
		sec, ok := parseInterval(c.in)
		if sec != c.sec || ok != c.want {
			t.Errorf("parseInterval(%q) = (%d, %v), want (%d, %v)", c.in, sec, ok, c.sec, c.want)
		}
		// Every cadence the API accepts prints back as itself: a row that read "5m"
		// for a daily heartbeat was the old default arm.
		if ok && intervalLabel(sec) != c.in {
			t.Errorf("intervalLabel(%d) = %q, want %q", sec, intervalLabel(sec), c.in)
		}
	}
}

func TestHeartbeatCadence(t *testing.T) {
	// A website may not be asked to run less than hourly; a heartbeat may.
	if _, ok := parseCadence("website", "1h"); !ok {
		t.Fatal("a website may run hourly")
	}
	if _, ok := parseCadence("website", "1d"); ok {
		t.Fatal("a website may not run daily")
	}
	if v, ok := parseCadence("heartbeat", "1d"); !ok || v != 86400 {
		t.Fatal("a heartbeat may run daily")
	}
	// Grace is the interval up to an hour: a daily job's window closes an hour
	// late, not a whole day late.
	for in, want := range map[int32]int32{300: 300, 3600: 3600, 21600: 3600, 86400: 3600} {
		if got := heartbeatGrace(in); got != want {
			t.Errorf("heartbeatGrace(%d) = %d, want %d", in, got, want)
		}
	}
}

// A PATCH answers with the row as it now is, never a hardcoded "nodata":
// this pins the helper the caller passes real facts through.
func TestMonitorPatchKeepsStatus(t *testing.T) {
	got := monitorRowToAPI("website", "Renamed", "https://example.com", "", 300,
		"ok", pgtype.Timestamptz{}, pgtype.Timestamptz{}, pgtype.UUID{Valid: true}, "", false, nil)
	if got["status"] != "ok" {
		t.Fatalf("status = %v, want ok", got["status"])
	}
}
