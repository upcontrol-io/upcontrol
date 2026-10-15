package auth

import (
	"strings"
	"testing"
)

// A sign-in request names what it was started for, and only the right shapes
// are kept: the slug a page is minted under, the CLI's 32-hex token, an /app
// path. Anything else is dropped, never refused, and never stored.
func TestPendingFromKeepsOnlyTheRightShapes(t *testing.T) {
	token := strings.Repeat("0a", 16)
	got := pendingFrom(magicLinkReq{Claim: "example-com", ClaimToken: token, Next: "/app/alerts"})
	if got != (pendingClaim{Slug: "example-com", Token: token, Next: "/app/alerts"}) {
		t.Fatalf("well-formed fields = %+v, want all three kept", got)
	}

	for _, slug := range []string{"Example-com", "-lead", "a_b", "x&next=//evil", "example.com", strings.Repeat("a", 65), ""} {
		if p := pendingFrom(magicLinkReq{Claim: slug}); p.Slug != "" {
			t.Errorf("claim %q kept as %q, want dropped", slug, p.Slug)
		}
	}
	for _, tok := range []string{strings.ToUpper(token), token[:31], token + "0", "uct_" + token[4:], ""} {
		if p := pendingFrom(magicLinkReq{ClaimToken: tok}); p.Token != "" {
			t.Errorf("claimToken %q kept, want dropped", tok)
		}
	}
	for next, keep := range map[string]bool{
		"/app":                             true,
		"/app/status-page?tab=settings":    true,
		"/app#add":                         true,
		"/apple":                           false,
		"/app//evil.example":               false,
		`/app/\evil.example`:               false,
		"https://evil.example/app":         false,
		"//evil.example/app":               false,
		"/app/ with space":                 false,
		"/app/" + strings.Repeat("a", 260): false,
		"":                                 false,
	} {
		if p := pendingFrom(magicLinkReq{Next: next}); (p.Next != "") != keep {
			t.Errorf("next %q kept=%v, want %v", next, p.Next != "", keep)
		}
	}
}
