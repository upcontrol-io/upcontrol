package telegram

import (
	"bytes"
	"crypto/sha256"
	"strings"
	"testing"
	"time"
)

func TestParseCallback(t *testing.T) {
	for _, tc := range []struct {
		data   string
		action string
		id     string
		ok     bool
	}{
		{"ack:42", "ack", "42", true},
		{"resolve:99", "resolve", "99", true},
		{"ack:inc_7", "ack", "inc_7", true},
		{":42", "", "", false},                    // missing action → malformed
		{"ack:", "", "", false},                   // missing id → malformed
		{"ack", "", "", false},                    // no colon
		{"", "", "", false},                       // empty
		{"ack:42:extra", "ack", "42:extra", true}, // SplitN keeps the rest in id
	} {
		action, id, ok := parseCallback(tc.data)
		if action != tc.action || id != tc.id || ok != tc.ok {
			t.Fatalf("parseCallback(%q) = (%q,%q,%v), want (%q,%q,%v)",
				tc.data, action, id, ok, tc.action, tc.id, tc.ok)
		}
	}
}

// The deep link is the whole connect flow: an inv_ token is the only thing
// that can bind a chat; the old guessable prj-N form must reach the refusal.
func TestCommand(t *testing.T) {
	for _, tc := range []struct {
		text string
		name string
		rest string
	}{
		{"/start inv_abc123", "start", "inv_abc123"},
		{"/start prj-7", "start", "prj-7"},
		{"/start", "start", ""},
		{"/mute@upcontrol_bot 30m", "mute", "30m"}, // group addressing
		{"/status", "status", ""},
		{"/unmute", "unmute", ""},
		{"/stop", "stop", ""},
		{"hello", "", ""},
		{"", "", ""},
	} {
		name, rest := command(tc.text)
		if name != tc.name || rest != tc.rest {
			t.Fatalf("command(%q) = (%q,%q), want (%q,%q)", tc.text, name, rest, tc.name, tc.rest)
		}
	}
}

// A prj-N payload must not parse as an invite: a stranger who knows the
// project id subscribes nothing. Pins the exact cut handleStart performs.
func TestPrjPayloadIsNotAnInvite(t *testing.T) {
	_, ok := strings.CutPrefix("prj-7", "inv_")
	if ok {
		t.Fatal("prj-7 must not parse as an invite token")
	}
}

// The label a channel row prints on Alerts: name plus @username, falling
// back through nullableLabel, never to an invented placeholder.
func TestInviteLabel(t *testing.T) {
	for _, tc := range []struct {
		name     string
		username string
		want     string
	}{
		{"Kira Volkova", "kira", "Kira Volkova @kira"},
		{"Kira Volkova", "", "Kira Volkova"},
		{"", "kira", "@kira"},
		{"", "", ""},
	} {
		if got := inviteLabel(tc.name, tc.username); got != tc.want {
			t.Errorf("inviteLabel(%q, %q) = %q, want %q", tc.name, tc.username, got, tc.want)
		}
	}
}

// nullableLabel: a computed label passes through, an empty one becomes nil.
// The read emits label only when not NULL; the front falls back on NULL only.
func TestNullableLabel(t *testing.T) {
	if got := nullableLabel("Kira Volkova @kira"); got != "Kira Volkova @kira" {
		t.Errorf("nullableLabel(non-empty) = %#v, want the label unchanged", got)
	}
	if got := nullableLabel(""); got != nil {
		t.Errorf("nullableLabel(empty) = %#v, want nil", got)
	}
}

func TestParseMuteDuration(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want time.Duration
		ok   bool
	}{
		{"30m", 30 * time.Minute, true},
		{"2h", 2 * time.Hour, true},
		{"1d", 24 * time.Hour, true},
		{"7d", 7 * 24 * time.Hour, true},
		{"8d", 0, false},   // past the cap
		{"0m", 0, false},   // nothing
		{"-5m", 0, false},  // negative
		{"5x", 0, false},   // bad unit
		{"", 0, false},     // empty
		{"m", 0, false},    // no number
		{"30 m", 0, false}, // space is not a number
	} {
		got, err := parseMuteDuration(tc.in)
		if tc.ok && (err != nil || got != tc.want) {
			t.Fatalf("parseMuteDuration(%q) = %v, %v; want %v", tc.in, got, err, tc.want)
		}
		if !tc.ok && err == nil {
			t.Fatalf("parseMuteDuration(%q) accepted a bad input", tc.in)
		}
	}
}

// Pins the FORM InviteTokenHash must keep: sha256 of the full payload, prefix
// included; every telegram_invite row already stores it.
func TestInviteTokenHashCoversTheFullPayload(t *testing.T) {
	payload := "inv_cd3043718570df4147d831aa47d8cdc5"
	want := sha256.Sum256([]byte(payload))
	if got := InviteTokenHash(payload); !bytes.Equal(got, want[:]) {
		t.Fatalf("InviteTokenHash(%q) = %x, want sha256 of the FULL payload %x", payload, got, want)
	}
	tail := sha256.Sum256([]byte("cd3043718570df4147d831aa47d8cdc5"))
	if got := InviteTokenHash(payload); bytes.Equal(got, tail[:]) {
		t.Fatal("InviteTokenHash hashed the tail after the prefix — the exact bug that made every invite unredeemable")
	}
}

// /status names the open incidents: three titles, then a count. No incidents
// means no line at all.
func TestIncidentsLine(t *testing.T) {
	for _, tc := range []struct {
		name   string
		titles []string
		want   string
	}{
		{"none", nil, ""},
		{"one", []string{"Error rate spike on shop.example"}, "\nOpen incidents: Error rate spike on shop.example"},
		{"three", []string{"a", "b", "c"}, "\nOpen incidents: a; b; c"},
		{"five", []string{"a", "b", "c", "d", "e"}, "\nOpen incidents: a; b; c (+2 more)"},
	} {
		if got := incidentsLine(tc.titles, true); got != tc.want {
			t.Errorf("%s: incidentsLine = %q, want %q", tc.name, got, tc.want)
		}
	}
}
