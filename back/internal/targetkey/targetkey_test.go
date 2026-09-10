package targetkey

import (
	"strings"
	"testing"
)

func TestNormalizeURL(t *testing.T) {
	cases := []struct{ raw, want string }{
		{"datrade.io", "https://datrade.io"},
		{"https://datrade.io", "https://datrade.io"},
		{"https://datrade.io/", "https://datrade.io"},
		{"http://datrade.io", "http://datrade.io"},
		{"HTTPS://datrade.io", "https://datrade.io"},
		{"https://WWW.Datrade.io", "https://datrade.io"},
		{"https://www2.datrade.io", "https://www2.datrade.io"}, // only a bare www. goes
		{"https://datrade.io:443", "https://datrade.io"},
		{"http://datrade.io:80/path", "http://datrade.io/path"},
		{"https://datrade.io:8443/x", "https://datrade.io:8443/x"},
		{"https://datrade.io/path/", "https://datrade.io/path/"},       // non-empty path untouched
		{"https://datrade.io/#section", "https://datrade.io"},          // fragment dropped
		{"https://datrade.io/a#f?not-a-query", "https://datrade.io/a"}, // fragment beats query parsing
		{"https://datrade.io/?a=1", "https://datrade.io/?a=1"},         // query keeps the slash: path not otherwise empty
		{"https://datrade.io/a/b?c=d", "https://datrade.io/a/b?c=d"},
		{"münchen.de", "https://xn--mnchen-3ya.de"}, // idna punycode
		{"https://www.münchen.de/x", "https://xn--mnchen-3ya.de/x"},
		{"  https://datrade.io  ", "https://datrade.io"},
	}
	for _, c := range cases {
		got, err := NormalizeURL(c.raw)
		if err != nil {
			t.Errorf("NormalizeURL(%q) error: %v", c.raw, err)
			continue
		}
		if got != c.want {
			t.Errorf("NormalizeURL(%q) = %q, want %q", c.raw, got, c.want)
		}
	}
}

func TestNormalizeURLErrors(t *testing.T) {
	for _, raw := range []string{"", "   ", "https://"} {
		if _, err := NormalizeURL(raw); err == nil {
			t.Errorf("NormalizeURL(%q) should error", raw)
		}
	}
}

// The keyword fold: unset and empty keywords are one key, the trailing-slash
// spelling folds with the bare host, and the keyword itself is part of the key.
func TestWebsiteKeywordFold(t *testing.T) {
	a := Website("https://datrade.io", "")
	b := Website("https://datrade.io/", "")
	if a != b {
		t.Errorf("trailing slash must fold: %q vs %q", a, b)
	}
	if Website("https://datrade.io", "ok") == a {
		t.Error("keyword must be part of the key")
	}
}

// An idna-hostile host never fails the door: it falls back to the lowercased
// spelling, so a fresh target is minted instead of a rejected subscribe.
func TestNormalizeURLWeirdHostFallsBack(t *testing.T) {
	got, err := NormalizeURL("https://x_x.example.com")
	if err != nil {
		t.Fatalf("weird host should not fail: %v", err)
	}
	if got != "https://x_x.example.com" {
		t.Errorf("fallback = %q, want the lowercased host", got)
	}
}

func TestHeartbeat(t *testing.T) {
	if Heartbeat("018F44C8-0000-7000-8000-000000000001") !=
		Heartbeat("018f44c8-0000-7000-8000-000000000001") {
		t.Error("case must fold: public_id::text is canonical lowercase")
	}
	if Heartbeat("018f44c8-0000-7000-8000-000000000001") != "heartbeat\x1f018f44c8-0000-7000-8000-000000000001" {
		t.Errorf("Heartbeat shape changed: %q", Heartbeat("018f44c8-0000-7000-8000-000000000001"))
	}
}

// The key's separator must never appear inside a part: any \x1f or NUL in
// a URL or keyword is stripped BEFORE the join, so no (url, keyword) pair
// can ever shift content across a part boundary — the join stays exactly
// kind<sep>url<sep>keyword. Distinct spellings that differ only by smuggled
// separator bytes fold onto one key (like trailing-slash spellings do); they
// are unfetchable control characters, one target for them is correct.
func TestKeySeparatorCannotBeSmuggled(t *testing.T) {
	if Website("https://a.io", "k\x1f2") == Website("https://a.io\x1fk", "2") {
		t.Error("separator smuggling must not move a boundary between url and keyword")
	}
	if Website("https://a.io", "k\x002") == Website("https://a.io\x00k", "2") {
		t.Error("NUL smuggling must not move a boundary either")
	}
	// Every key is exactly kind<sep>url<sep>keyword: two separators, none
	// inside a part, whatever the caller passed.
	for _, tc := range [][2]string{
		{"https://a.io/x\x1fy", ""},
		{"https://a.io", "k\x1f2"},
		{"https://a.io/\x00", "\x1f"},
	} {
		k := Website(tc[0], tc[1])
		if n := strings.Count(k, sep); n != 2 {
			t.Errorf("key %q has %d separators, want exactly the 2 structural ones", k, n)
		}
		if strings.Contains(k, "\x00") {
			t.Errorf("key %q leaks a NUL", k)
		}
	}
}

// NormalizeURL refuses control characters the way net/url does (they error,
// not leak): the leak-proofing that matters lives in the join, above.
func TestNormalizeURLRefusesControlChars(t *testing.T) {
	for _, raw := range []string{
		"https://a.io/\x1f", "https://a.io/?q=\x1f", "https://a\x1f.io",
		"https://a.io/\x00",
	} {
		if _, err := NormalizeURL(raw); err == nil {
			t.Errorf("NormalizeURL(%q) should refuse a control character", raw)
		}
	}
}
