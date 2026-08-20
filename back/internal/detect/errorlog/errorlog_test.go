package errorlog

import (
	"strings"
	"testing"
	"time"
)

var now = time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC)

func recent() Group {
	return Group{Fingerprint: 42, Count: 1, Service: "api", Message: "boom", LastTS: now.Add(-30 * time.Second)}
}

func TestShouldFireNew_FirstAppearanceFires(t *testing.T) {
	if !ShouldFireNew(recent(), nil, now) {
		t.Fatal("a fingerprint that never alerted must fire")
	}
}

func TestShouldFireNew_CooldownHolds(t *testing.T) {
	last := now.Add(-NewErrorCooldown / 2)
	if ShouldFireNew(recent(), &last, now) {
		t.Fatal("inside the cooldown a persisting error must stay quiet")
	}
	old := now.Add(-NewErrorCooldown)
	if !ShouldFireNew(recent(), &old, now) {
		t.Fatal("after the cooldown the fingerprint may fire again")
	}
}

func TestShouldFireNew_OldNoiseDoesNotFire(t *testing.T) {
	g := recent()
	g.LastTS = now.Add(-Lookback - time.Minute)
	if ShouldFireNew(g, nil, now) {
		t.Fatal("an error last seen before the lookback did not just appear")
	}
}

func TestShouldFireRepeat_ThresholdAndWindow(t *testing.T) {
	window := 5 * time.Minute
	g := recent()
	g.Count = 1
	if ShouldFireRepeat(g, nil, window, now) {
		t.Fatal("a single line is not repeating")
	}
	g.Count = RepeatThreshold
	if !ShouldFireRepeat(g, nil, window, now) {
		t.Fatal("threshold crossed with no prior alert must fire")
	}
	last := now.Add(-window / 2)
	if ShouldFireRepeat(g, &last, window, now) {
		t.Fatal("a steadily-repeating error pages once per window, not per tick")
	}
	old := now.Add(-window)
	if !ShouldFireRepeat(g, &old, window, now) {
		t.Fatal("one full window after the last alert it may fire again")
	}
}

func TestTitles(t *testing.T) {
	g := recent()
	if got := NewErrorTitle(g); got != "Error in api: boom" {
		t.Fatalf("NewErrorTitle = %q", got)
	}
	g.Service = ""
	if got := NewErrorTitle(g); got != "Error: boom" {
		t.Fatalf("NewErrorTitle without service = %q", got)
	}
	g.Count = 7
	if got := RepeatTitle(g, 5); got != "Repeating error (7× in 5 min): boom" {
		t.Fatalf("RepeatTitle = %q", got)
	}
}

func TestTitleTruncatesOnRuneBoundary(t *testing.T) {
	g := recent()
	g.Message = strings.Repeat("é", 200) // 2 bytes per rune: byte-slicing would split one
	title := NewErrorTitle(g)
	if !strings.HasSuffix(title, "…") {
		t.Fatalf("long message must be truncated, got %q", title)
	}
	if strings.ContainsRune(title, '�') {
		t.Fatal("truncation split a rune")
	}
}
