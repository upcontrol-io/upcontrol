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
		v := Build("Checkout", tc.err, tc.code)
		if v.Title != tc.want {
			t.Fatalf("error=%q code=%d: title %q, want %q", tc.err, tc.code, v.Title, tc.want)
		}
	}
}

func TestBuild_CheckFailureIsAlwaysAFact(t *testing.T) {
	v := Build("Api", "status", 500)
	if len(v.Facts) != 1 || v.Facts[0].Kind != "check_failure" {
		t.Fatalf("the check result must always be the first fact; got %+v", v.Facts)
	}
	if !strings.Contains(v.Facts[0].Detail, "HTTP 500") {
		t.Fatalf("check_failure detail must carry the label; got %q", v.Facts[0].Detail)
	}
}
