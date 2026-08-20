package api

import (
	"strings"
	"testing"
	"time"

	"go.upcontrol.io/back/internal/storage/ch"
)

func TestMergeTimelinePutsADeployBeforeTheOutage(t *testing.T) {
	opened := time.Date(2026, 8, 15, 14, 33, 0, 0, time.UTC)
	lifecycle := []map[string]any{
		{"time": "14:33", "ago": "40 min ago", "kind": "error", "text": "Checkout started failing"},
	}
	events := []ch.EventRow{
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
	lifecycle := []map[string]any{{"time": "14:33", "kind": "error", "text": "started failing"}}
	got := mergeTimeline(lifecycle, nil)
	if len(got) != 1 {
		t.Fatalf("got %d entries, want 1 — an absent feed must add nothing, never a placeholder", len(got))
	}
}

func TestEventKindMapsTheNamesTheFrontKnows(t *testing.T) {
	cases := map[string]string{
		"deploy.succeeded":      "deploy",
		"deployment_created":    "deploy",
		"payment_intent.failed": "payment",
		"charge.refunded":       "payment",
		"invoice.paid":          "payment",
		"external_error":        "error",
		"job_failed":            "error",
		"github_push":           "check",
	}
	for name, want := range cases {
		if got := eventKind(name); got != want {
			t.Fatalf("eventKind(%q) = %q, want %q", name, got, want)
		}
	}
}
