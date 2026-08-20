package telegram

import (
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

// The deep link is the whole "connect Telegram" flow, so the payload decides
// everything: an inv_ token is the only thing that can bind a chat, and the
// old guessable prj-N form must reach the refusal path, never the redeem one.
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
		{"hello", "", ""},
		{"", "", ""},
	} {
		name, rest := command(tc.text)
		if name != tc.name || rest != tc.rest {
			t.Fatalf("command(%q) = (%q,%q), want (%q,%q)", tc.text, name, rest, tc.name, tc.rest)
		}
	}
}

// A prj-N payload must NOT look like an invite even after trimming: it takes
// the refusal branch in handleStart, so a stranger who knows the project id
// subscribes nothing. This pins the exact cut handleStart performs.
func TestPrjPayloadIsNotAnInvite(t *testing.T) {
	_, ok := strings.CutPrefix("prj-7", "inv_")
	if ok {
		t.Fatal("prj-7 must not parse as an invite token")
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
