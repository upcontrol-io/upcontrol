package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A public key authenticates only from an origin stored byte for byte as a
// browser sends it, so anything else must be refused at mint: minted, it would
// authenticate nowhere and nothing would say why.
func TestValidOrigin(t *testing.T) {
	ok := []string{"https://a.com", "https://www.a.com", "http://localhost:5173", "https://a.com:8443", "http://127.0.0.1:3000", "http://[::1]:3000", "https://xn--exmple-cua.com"}
	bad := []string{
		"", "*", "null", "a.com", "ftp://a.com", "https://",
		"https://a.com/", "https://a.com/path", "https://a.com?", "https://a.com?x=1", "https://a.com#", "https://a.com#f",
		"https://a.com:", "https://user@a.com", "https://user:pw@a.com",
		"HTTPS://a.com", "https://A.com", "https://*.a.com/",
		// no trailing slash: the star itself has to be what refuses it
		"https://*.a.com", "https://*", "https://a.com,b.com", "https://a.com;x", "https://a.com=", "https://exämple.com",
		// a browser omits the scheme's default port, so these would match nothing
		"http://a.com:80", "https://a.com:443",
	}
	for _, o := range ok {
		if !validOrigin(o) {
			t.Errorf("validOrigin(%q) = false, want true", o)
		}
	}
	for _, o := range bad {
		if validOrigin(o) {
			t.Errorf("validOrigin(%q) = true, want false", o)
		}
	}
}

// callIssue drives issue directly. The publicOnly refusals below fire before
// any read, so a handler with no pool answers them — the same direct-call
// harness this file already uses.
func callIssue(t *testing.T, body string, publicOnly bool) (int, string) {
	t.Helper()
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/v1/keys", strings.NewReader(body))
	(&keys{}).issue(w, r, 0, 0, publicOnly)
	return w.Code, strings.TrimSpace(w.Body.String())
}

// A presented key mints a public key only: anything that resolves to a secret
// kind — asked for outright, or the bare POST's default — is a 403 in words,
// never a silent secret mint.
func TestPublicOnlyIssueRefusesASecretKind(t *testing.T) {
	for _, body := range []string{"", `{"name":"staging"}`, `{"kind":"secret"}`} {
		code, out := callIssue(t, body, true)
		if code != http.StatusForbidden || !strings.Contains(out, "key_mints_secret") {
			t.Fatalf("publicOnly issue of %q = %d %s, want 403 key_mints_secret", body, code, out)
		}
	}
}
