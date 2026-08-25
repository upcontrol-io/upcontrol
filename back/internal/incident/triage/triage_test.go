package triage

import (
	"strings"
	"testing"
)

func TestBuild_TitleDerivedFromErrorClass(t *testing.T) {
	for _, tc := range []struct {
		err  string
		code int
		want string
	}{
		{"status", 502, "Checkout returning 502"},
		{"timeout", 0, "Checkout timed out"},
		{"connect", 0, "Checkout connection refused"},
		{"dns", 0, "Checkout DNS error"},
		{"keyword_missing", 0, "Checkout keyword not found"},
		{"", 0, "Checkout is down"}, // unknown error class
	} {
		v := Build("Checkout", tc.err, tc.code, nil)
		if v.Title != tc.want {
			t.Fatalf("error=%q code=%d: title %q, want %q", tc.err, tc.code, v.Title, tc.want)
		}
	}
}

func TestBuild_CheckFailureIsAlwaysAFact(t *testing.T) {
	v := Build("Api", "status", 500, nil)
	if len(v.Facts) != 1 || v.Facts[0].Kind != "check_failure" {
		t.Fatalf("the check result must always be the first fact; got %+v", v.Facts)
	}
	if !strings.Contains(v.Facts[0].Detail, "HTTP 500") {
		t.Fatalf("check_failure detail must carry the label; got %q", v.Facts[0].Detail)
	}
	// No deploy => no hypothesis and no command.
	if v.Hypothesis != "" || v.Command != "" {
		t.Fatalf("without a deploy there must be no hypothesis/command; got %+v", v)
	}
}

func TestBuild_DeployBecomesHypothesisWithRunnableCommand(t *testing.T) {
	v := Build("Api", "status", 502, &DeployContext{Hash: "abc1234", Message: "bump deps", At: "now"})
	// The deploy is a fact (joined once), AND the prime-suspect hypothesis
	// is labelled as a guess with a runnable command, not advice.
	kinds := map[string]bool{}
	for _, f := range v.Facts {
		kinds[f.Kind] = true
	}
	if !kinds["check_failure"] || !kinds["deploy"] {
		t.Fatalf("expected check_failure + deploy facts; got %+v", v.Facts)
	}
	if !strings.Contains(v.Hypothesis, "may have caused") {
		t.Fatalf("hypothesis must be labelled a guess; got %q", v.Hypothesis)
	}
	if !strings.Contains(v.Command, "git revert abc1234") {
		t.Fatalf("command must be runnable (git revert <hash>); got %q", v.Command)
	}
}

func TestTruncate(t *testing.T) {
	if truncate("short", 60) != "short" {
		t.Fatal("short string must pass through unchanged")
	}
	long := strings.Repeat("x", 80)
	got := truncate(long, 60)
	// '…' is one rune (3 bytes), so check the rune count, not byte length.
	if !strings.HasSuffix(got, "…") {
		t.Fatalf("truncated string must end in …; got %q", got)
	}
	runes := []rune(got)
	if len(runes) != 60 {
		t.Fatalf("truncate(80,60) = %d runes, want 60", len(runes))
	}
}
