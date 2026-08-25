package api

import "testing"

// The vectors that matter are the ones a whitespace/slash-only check waved
// through: every "bad" case below except the last three was accepted as a relay.
func TestValidRelayHost(t *testing.T) {
	good := []string{
		"smtp.eu.mailgun.org",
		"smtp.gmail.com",
		"localhost",
		"mail-relay.internal",
		"192.168.1.25",
		"a.b.c.d.e.f.example.com",
		"xn--80ak6aa92e.com", // punycode is ASCII and legitimate
	}
	for _, h := range good {
		if !validRelayHost(h) {
			t.Errorf("validRelayHost(%q) = false, want true", h)
		}
	}

	bad := []string{
		"bad..host..name",         // empty label
		".leading.dot",            // empty first label
		"trailing.dot.",           // empty last label
		"-leading.hyphen.com",     // label may not start with a hyphen
		"trailing.hyphen-.com",    // nor end with one
		"under_score.example.com", // not a hostname character
		"smtp.example.com:587",    // a port is a separate field
		"smtps://smtp.example",    // a scheme is not a hostname
		"smtp.example.com/path",   // nor a path
		"münchen.example.com",     // reaches the wire as ASCII or not at all
		"smtp .example.com",       // whitespace
		"smtp\texample.com",
		"smtp\r\nexample.com", // header injection shape
		"",
	}
	for _, h := range bad {
		if validRelayHost(h) {
			t.Errorf("validRelayHost(%q) = true, want false", h)
		}
	}

	// 63 is the label ceiling, 255 the whole-name ceiling.
	if !validRelayHost(rep('a', 63) + ".example.com") {
		t.Error("a 63-character label should pass")
	}
	if validRelayHost(rep('a', 64) + ".example.com") {
		t.Error("a 64-character label should fail")
	}
	if validRelayHost(rep('a', 256)) {
		t.Error("a 256-character name should fail")
	}
}

func rep(c byte, n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = c
	}
	return string(b)
}
