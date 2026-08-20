package executor

import (
	"context"
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestExecuteOK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("hello world"))
	}))
	defer srv.Close()

	e := NewWithoutGuard()
	r := e.Execute(context.Background(), CheckSpec{URL: srv.URL, TimeoutMs: 5000})
	if !r.OK {
		t.Errorf("OK = false, want true (err=%s %s)", r.ErrorClass, r.ErrorDetail)
	}
	if r.StatusCode != 200 {
		t.Errorf("StatusCode = %d, want 200", r.StatusCode)
	}
	if r.ErrorClass != "none" {
		t.Errorf("ErrorClass = %q, want none", r.ErrorClass)
	}
	// TotalMs may be 0 for sub-ms local requests; just verify the request worked.
}

func TestExecute500(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	e := NewWithoutGuard()
	r := e.Execute(context.Background(), CheckSpec{URL: srv.URL, TimeoutMs: 5000})
	if r.OK {
		t.Error("OK = true, want false for 500")
	}
	if r.ErrorClass != "status" {
		t.Errorf("ErrorClass = %q, want status", r.ErrorClass)
	}
	if r.StatusCode != 500 {
		t.Errorf("StatusCode = %d, want 500", r.StatusCode)
	}
}

func TestExecuteKeywordMatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("<html>Add to cart</html>"))
	}))
	defer srv.Close()

	e := NewWithoutGuard()
	r := e.Execute(context.Background(), CheckSpec{
		URL: srv.URL, TimeoutMs: 5000, Keyword: "Add to cart",
	})
	if !r.OK {
		t.Errorf("keyword match: OK = false, want true (err=%s)", r.ErrorClass)
	}
}

func TestExecuteKeywordMissing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("<html>Out of stock</html>"))
	}))
	defer srv.Close()

	e := NewWithoutGuard()
	r := e.Execute(context.Background(), CheckSpec{
		URL: srv.URL, TimeoutMs: 5000, Keyword: "Add to cart",
	})
	if r.OK {
		t.Error("keyword missing: OK = true, want false")
	}
	if r.ErrorClass != "keyword_missing" {
		t.Errorf("ErrorClass = %q, want keyword_missing", r.ErrorClass)
	}
}

func TestExecuteBlockedMetadataIP(t *testing.T) {
	// The pre-flight CheckURL catches raw IP URLs in blocked ranges even
	// without the dialer guard (which is disabled in NewWithoutGuard).
	e := New()
	r := e.Execute(context.Background(), CheckSpec{URL: "http://169.254.169.254/"})
	if r.OK {
		t.Error("metadata IP should be blocked")
	}
	if r.ErrorClass != "blocked_target" {
		t.Errorf("ErrorClass = %q, want blocked_target", r.ErrorClass)
	}
}

func TestExecuteBadScheme(t *testing.T) {
	e := New()
	r := e.Execute(context.Background(), CheckSpec{URL: "ftp://example.com/"})
	if r.OK {
		t.Error("ftp scheme should be blocked")
	}
	if r.ErrorClass != "blocked_target" {
		t.Errorf("ErrorClass = %q, want blocked_target", r.ErrorClass)
	}
}

func TestExecuteBodyHash(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("consistent body"))
	}))
	defer srv.Close()

	e := NewWithoutGuard()
	r1 := e.Execute(context.Background(), CheckSpec{URL: srv.URL})
	r2 := e.Execute(context.Background(), CheckSpec{URL: srv.URL})
	if r1.BodyHash == 0 {
		t.Error("BodyHash = 0, want non-zero")
	}
	if r1.BodyHash != r2.BodyHash {
		t.Error("same body should produce same hash")
	}
}

func TestExecuteCountsRedirects(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/a", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/b", http.StatusMovedPermanently)
	})
	mux.HandleFunc("/b", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/c", http.StatusMovedPermanently)
	})
	mux.HandleFunc("/c", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	e := NewWithoutGuard()
	r := e.Execute(context.Background(), CheckSpec{URL: srv.URL + "/a", TimeoutMs: 5000})
	if !r.OK {
		t.Fatalf("OK = false (err=%s %s)", r.ErrorClass, r.ErrorDetail)
	}
	// Two hops: /a → /b → /c. The field used to be declared and never assigned,
	// so a landing row reading it always said "0 hops".
	if r.RedirectCount != 2 {
		t.Errorf("RedirectCount = %d, want 2", r.RedirectCount)
	}
}

func TestExecuteNoRedirectsCountsZero(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	e := NewWithoutGuard()
	r := e.Execute(context.Background(), CheckSpec{URL: srv.URL, TimeoutMs: 5000})
	if r.RedirectCount != 0 {
		t.Errorf("RedirectCount = %d, want 0", r.RedirectCount)
	}
	// An IP URL never resolves a name, so the count stays 0 — that is the
	// "absent, not measured" signal the API turns into a missing field.
	if r.DNSAddrs != 0 {
		t.Errorf("DNSAddrs = %d, want 0 for an IP host", r.DNSAddrs)
	}
	// Plaintext: no handshake, so no version to report.
	if r.TLSVersion != "" {
		t.Errorf("TLSVersion = %q, want empty over plain HTTP", r.TLSVersion)
	}
}

func TestTLSVersionName(t *testing.T) {
	cases := []struct {
		in   uint16
		want string
	}{
		{tls.VersionTLS13, "TLS 1.3"},
		{tls.VersionTLS12, "TLS 1.2"},
		{tls.VersionTLS11, "TLS 1.1"},
		{tls.VersionTLS10, "TLS 1.0"},
		{0x0399, ""},
	}
	for _, c := range cases {
		if got := tlsVersionName(c.in); got != c.want {
			t.Errorf("tlsVersionName(%#04x) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestExecuteSendsAUserAgent(t *testing.T) {
	// Go's default is "Go-http-client/2.0", which WAFs block and which tells an
	// operator nothing about who is asking.
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("User-Agent")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	NewWithoutGuard().Execute(context.Background(), CheckSpec{URL: srv.URL, TimeoutMs: 5000})
	if got != UserAgent {
		t.Errorf("User-Agent = %q, want %q", got, UserAgent)
	}
	if !strings.Contains(got, "http") {
		t.Errorf("User-Agent %q carries no URL to identify us by", got)
	}
}

func TestCollectBodyIsOptIn(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("<html><a href=\"/pricing\">p</a></html>"))
	}))
	defer srv.Close()
	e := NewWithoutGuard()

	// The fleet stores check rows and has no use for 64 KB of HTML per probe.
	if r := e.Execute(context.Background(), CheckSpec{URL: srv.URL}); r.Body != nil {
		t.Errorf("Body = %q without CollectBody, want nil", r.Body)
	}
	r := e.Execute(context.Background(), CheckSpec{URL: srv.URL, CollectBody: true})
	if !strings.Contains(string(r.Body), "/pricing") {
		t.Errorf("Body = %q, want the response body", r.Body)
	}
}
