package api

import "testing"

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
