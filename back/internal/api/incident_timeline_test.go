package api

import (
	"strings"
	"testing"
	"time"

	"go.upcontrol.io/back/internal/storage/pgstore"
)

func TestMergeTimelinePutsADeployBeforeTheOutage(t *testing.T) {
	opened := time.Date(2026, 8, 15, 14, 33, 0, 0, time.UTC)
	lifecycle := []timelineMark{
		{At: opened, Kind: "error", Text: "Checkout started failing"},
	}
	events := []pgstore.EventRow{
		{TS: opened.Add(-2 * time.Minute), Name: "deploy.succeeded",
			Labels: map[string]string{"sha": "a1b2c3d", "service": "api"}},
	}

	got := mergeTimeline(lifecycle, events)

	if len(got) != 2 {
		t.Fatalf("got %d entries, want 2", len(got))
	}
	// Oldest first: the deploy landed before the outage and must read that way,
	// because the whole point of the card is "what happened just before".
	if got[0]["kind"] != "deploy" {
		t.Fatalf("first entry kind = %v, want deploy", got[0]["kind"])
	}
	if !strings.Contains(got[0]["text"].(string), "a1b2c3d") {
		t.Fatalf("deploy entry does not name the sha: %v", got[0]["text"])
	}
	if got[1]["kind"] != "error" {
		t.Fatalf("second entry kind = %v, want error", got[1]["kind"])
	}
}

func TestMergeTimelineWithNoEventsIsTheLifecycleAlone(t *testing.T) {
	opened := time.Date(2026, 8, 15, 14, 33, 0, 0, time.UTC)
	lifecycle := []timelineMark{{At: opened, Kind: "error", Text: "started failing"}}
	got := mergeTimeline(lifecycle, nil)
	if len(got) != 1 {
		t.Fatalf("got %d entries, want 1 — an absent feed must add nothing, never a placeholder", len(got))
	}
}

func TestMergeTimelineSortsAcrossMidnight(t *testing.T) {
	opened := time.Date(2026, 8, 27, 23, 58, 0, 0, time.UTC)
	eventAt := time.Date(2026, 8, 28, 0, 3, 0, 0, time.UTC)
	lifecycle := []timelineMark{
		{At: opened, Kind: "error", Text: "Checkout started failing"},
	}
	events := []pgstore.EventRow{
		{TS: eventAt, Name: "deploy.succeeded",
			Labels: map[string]string{"sha": "deadbee", "service": "api"}},
	}

	got := mergeTimeline(lifecycle, events)

	if len(got) != 2 {
		t.Fatalf("got %d entries, want 2", len(got))
	}
	// The event at 00:03 is AFTER the 23:58 mark, so it must render last.
	// The old string sort would have reversed them (00:03 < 23:58).
	if got[0]["kind"] != "error" {
		t.Fatalf("first entry kind = %v, want error", got[0]["kind"])
	}
	if got[1]["kind"] != "deploy" {
		t.Fatalf("second entry kind = %v, want deploy", got[1]["kind"])
	}
}

func TestMergeTimelineZeroAtComesFirst(t *testing.T) {
	opened := time.Date(2026, 8, 27, 23, 58, 0, 0, time.UTC)
	lifecycle := []timelineMark{
		{At: time.Time{}, Kind: "error", Text: "Opened at unknown time"},
		{At: opened, Kind: "check", Text: "Resolved"},
	}
	events := []pgstore.EventRow{}

	got := mergeTimeline(lifecycle, events)

	if len(got) != 2 {
		t.Fatalf("got %d entries, want 2", len(got))
	}
	// A zero At renders as empty strings and sorts first.
	if got[0]["time"] != "" {
		t.Fatalf("first entry time = %v, want empty string", got[0]["time"])
	}
	if got[0]["ago"] != "" {
		t.Fatalf("first entry ago = %v, want empty string", got[0]["ago"])
	}
	if got[1]["time"] != "23:58" {
		t.Fatalf("second entry time = %v, want 23:58", got[1]["time"])
	}
}

func TestEventKindMapsTheNamesTheFrontKnows(t *testing.T) {
	cases := map[string]string{
		"deploy.succeeded":   "deploy",
		"deployment_created": "deploy",
		// A failure outranks its subject: this one read as ordinary payment
		// traffic until the error arm moved above the payment arm.
		"payment_intent.failed": "error",
		"charge.refunded":       "payment",
		"invoice.paid":          "payment",
		"external_error":        "error",
		"job_failed":            "error",
		"github_push":           "check",
		// A failed deploy stays a deploy: the error arm sits below it, because
		// the suspect an incident wants attached is the deploy, not the word.
		"deploy_failed": "deploy",
	}
	for name, want := range cases {
		if got := eventKind(name); got != want {
			t.Fatalf("eventKind(%q) = %q, want %q", name, got, want)
		}
	}
}
