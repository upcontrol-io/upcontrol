// Package executor is the probe's HTTP checker: resolve DNS, apply the SSRF
// guard, run the request; the guard is the last line before a customer URL.
package executor

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"strings"
	"time"

	"go.upcontrol.io/back/internal/probe/guard"
)

// CheckSpec is the input: what to check and how.
type CheckSpec struct {
	URL           string
	Method        string // "GET" if empty
	Keyword       string // empty = no assertion
	TimeoutMs     uint32
	MaxRedirects  uint32
	MaxBodyBytes  uint32 // 0 = 65536 default
	CollectExpiry bool
	// CollectBody keeps the response body on the Result instead of discarding it
	// after hashing; opt-in because the fleet has no use for 64 KB of HTML.
	CollectBody bool
}

// UserAgent identifies us on every request; Go's default "Go-http-client/2.0"
// is blocked by WAFs and tells an operator nothing about who is asking.
const UserAgent = "upcontrol/1.0 (+https://upcontrol.io/bot)"

// Result is the output: the check outcome and its timings.
type Result struct {
	OK            bool
	StatusCode    uint16
	ErrorClass    string // none|dns|connect|tls|timeout|status|keyword_missing|blocked_target
	ErrorDetail   string
	DNSMs         uint32
	ConnectMs     uint32
	TLSMs         uint32
	TTFBMs        uint32
	TotalMs       uint32
	BodyHash      uint64
	RedirectCount uint32
	SSLExpiresAt  time.Time // zero = not collected / no cert
	// DNSAddrs is how many addresses the name resolved to; 0 means the lookup
	// never ran (an IP URL, or a connection that failed before DNS).
	DNSAddrs uint32
	// TLSVersion is "TLS 1.2"/"TLS 1.3"; empty for plaintext or a failed
	// handshake (read straight off the response, no extra cost).
	TLSVersion string
	// Header is the response's headers, nil when no response arrived; carried so
	// HSTS/Cache-Control need no second request for a page we already have.
	Header http.Header
	// Body is the response body, present only when CheckSpec.CollectBody asked
	// for it (link discovery reads the homepage we already fetched).
	Body []byte
}

// Executor runs HTTP checks. It is safe for concurrent use.
type Executor struct {
	guardCheck func([]net.IP) error // nil = skip SSRF (tests only)
}

// New builds a production executor with the SSRF guard active.
func New() *Executor { return &Executor{guardCheck: guard.CheckResolvedIPs} }

// Execute runs a single check and returns the result. It never panics: every
// error becomes a Result with an ErrorClass.
func (e *Executor) Execute(ctx context.Context, spec CheckSpec) Result {
	start := time.Now()
	timeout := time.Duration(spec.TimeoutMs) * time.Millisecond
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	maxBody := int64(spec.MaxBodyBytes)
	if maxBody <= 0 {
		maxBody = 65536
	}
	method := spec.Method
	if method == "" {
		method = http.MethodGet
	}
	maxRedirects := int(spec.MaxRedirects)
	if maxRedirects <= 0 {
		maxRedirects = 5
	}

	// Pre-flight URL validation: the full CheckURL (scheme + port + IP) runs
	// only when guardCheck is set; the zero Executor skips every guard check.
	if e.guardCheck != nil {
		if err := guard.CheckURL(spec.URL); err != nil {
			return blockedResult(err)
		}
	}

	// Build the transport with a guarded dialer.
	transport := e.buildTransport(timeout)

	// Set up the timing recorder.
	tr := newTimingRecorder(start)
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, method, spec.URL, nil)
	if err != nil {
		return Result{ErrorClass: "connect", ErrorDetail: err.Error()}
	}
	req.Header.Set("User-Agent", UserAgent)
	req = req.WithContext(httptraceCtx(ctx, tr))

	// Counted here because CheckRedirect is the only place that sees the chain,
	// and this closure belongs to a single Execute call — no sharing, no lock.
	var redirects uint32
	client := &http.Client{
		Transport: transport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= maxRedirects {
				return fmt.Errorf("too_many_redirects")
			}
			// Re-check the redirect target's URL (scheme/port), gated on the
			// same flag as the pre-flight check (nil guardCheck skips this too).
			if e.guardCheck != nil {
				if err := guard.CheckURL(req.URL.String()); err != nil {
					return err
				}
			}
			redirects = uint32(len(via))
			return nil
		},
	}

	resp, err := client.Do(req)
	if err != nil {
		return errorResult(err, start)
	}
	defer func() { _ = resp.Body.Close() }()

	// Read up to maxBody bytes.
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if int64(len(body)) > maxBody {
		body = body[:maxBody]
	}

	result := Result{
		OK:            resp.StatusCode >= 200 && resp.StatusCode < 400,
		StatusCode:    uint16(resp.StatusCode),
		ErrorClass:    "none",
		BodyHash:      hashBody(body),
		DNSMs:         tr.dnsMs(),
		ConnectMs:     tr.connectMs(),
		TLSMs:         tr.tlsMs(),
		TTFBMs:        tr.ttfbMs(),
		TotalMs:       uint32(time.Since(start).Microseconds() / 1000),
		DNSAddrs:      tr.dnsAddrs,
		RedirectCount: redirects,
		Header:        resp.Header,
	}
	if spec.CollectBody {
		result.Body = body
	}
	if resp.TLS != nil {
		result.TLSVersion = tlsVersionName(resp.TLS.Version)
	}

	// Status assertion: 2xx/3xx is OK; anything else is a status error.
	if !result.OK {
		result.ErrorClass = "status"
		result.ErrorDetail = fmt.Sprintf("HTTP %d", resp.StatusCode)
	}

	// Keyword assertion: the body must contain the keyword.
	if spec.Keyword != "" && result.OK {
		if !bytes.Contains(body, []byte(spec.Keyword)) {
			result.OK = false
			result.ErrorClass = "keyword_missing"
		}
	}

	// SSL expiry: from the TLS connection state.
	if spec.CollectExpiry && resp.TLS != nil && len(resp.TLS.PeerCertificates) > 0 {
		result.SSLExpiresAt = resp.TLS.PeerCertificates[0].NotAfter
	}

	return result
}

// buildTransport creates an http.Transport whose DialContext resolves DNS and
// applies the SSRF guard before connecting.
func (e *Executor) buildTransport(timeout time.Duration) *http.Transport {
	dialer := &net.Dialer{Timeout: timeout, KeepAlive: 30 * time.Second}
	return &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, err
			}
			// If the host is already an IP (e.g. from a redirect to an IP URL),
			// check it directly.
			if ip := net.ParseIP(host); ip != nil {
				if e.guardCheck != nil {
					if err := e.guardCheck([]net.IP{ip}); err != nil {
						return nil, err
					}
				}
				return dialer.DialContext(ctx, network, addr)
			}
			// Resolve and check all IPs.
			ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
			if err != nil {
				return nil, fmt.Errorf("dns: %w", err)
			}
			ipList := make([]net.IP, len(ips))
			for i, ip := range ips {
				ipList[i] = ip.IP
			}
			if e.guardCheck != nil {
				if err := e.guardCheck(ipList); err != nil {
					return nil, err
				}
			}
			return dialer.DialContext(ctx, network, net.JoinHostPort(ipList[0].String(), port))
		},
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          10,
		IdleConnTimeout:       30 * time.Second,
		TLSHandshakeTimeout:   timeout,
		ExpectContinueTimeout: 1 * time.Second,
	}
}

type timingRecorder struct {
	start time.Time
	// dnsAddrs rides along with the timings: DNSDone carries the resolved
	// addresses, so counting them here costs no second lookup.
	dnsAddrs          uint32
	dnsStart          time.Time
	dnsDone           time.Time
	connectStart      time.Time
	connectDone       time.Time
	tlsHandshakeStart time.Time
	tlsHandshakeDone  time.Time
	gotFirstByte      time.Time
}

func newTimingRecorder(start time.Time) *timingRecorder {
	return &timingRecorder{start: start}
}

func (t *timingRecorder) dnsMs() uint32 {
	if t.dnsDone.IsZero() || t.dnsStart.IsZero() {
		return 0
	}
	return uint32(t.dnsDone.Sub(t.dnsStart).Microseconds() / 1000)
}

func (t *timingRecorder) connectMs() uint32 {
	if t.connectDone.IsZero() || t.connectStart.IsZero() {
		return 0
	}
	return uint32(t.connectDone.Sub(t.connectStart).Microseconds() / 1000)
}

func (t *timingRecorder) tlsMs() uint32 {
	if t.tlsHandshakeDone.IsZero() || t.tlsHandshakeStart.IsZero() {
		return 0
	}
	return uint32(t.tlsHandshakeDone.Sub(t.tlsHandshakeStart).Microseconds() / 1000)
}

func (t *timingRecorder) ttfbMs() uint32 {
	if t.gotFirstByte.IsZero() {
		return 0
	}
	return uint32(t.gotFirstByte.Sub(t.start).Microseconds() / 1000)
}

// httptraceCtx attaches the timing hooks to a context.
func httptraceCtx(ctx context.Context, t *timingRecorder) context.Context {
	return httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
		DNSStart: func(httptrace.DNSStartInfo) { t.dnsStart = time.Now() },
		DNSDone: func(info httptrace.DNSDoneInfo) {
			t.dnsDone = time.Now()
			t.dnsAddrs = uint32(len(info.Addrs))
		},
		ConnectStart:         func(_, _ string) { t.connectStart = time.Now() },
		ConnectDone:          func(_, _ string, _ error) { t.connectDone = time.Now() },
		TLSHandshakeStart:    func() { t.tlsHandshakeStart = time.Now() },
		TLSHandshakeDone:     func(_ tls.ConnectionState, _ error) { t.tlsHandshakeDone = time.Now() },
		GotFirstResponseByte: func() { t.gotFirstByte = time.Now() },
	})
}

// tlsVersionName renders the handshake version the way a person reads it; an
// unknown constant returns "" (zero is silence).
func tlsVersionName(v uint16) string {
	switch v {
	case tls.VersionTLS13:
		return "TLS 1.3"
	case tls.VersionTLS12:
		return "TLS 1.2"
	case tls.VersionTLS11:
		return "TLS 1.1"
	case tls.VersionTLS10:
		return "TLS 1.0"
	default:
		return ""
	}
}

func hashBody(body []byte) uint64 {
	h := sha256.Sum256(body)
	return binary.BigEndian.Uint64(h[:8])
}

func blockedResult(err error) Result {
	return Result{
		ErrorClass:  "blocked_target",
		ErrorDetail: err.Error(),
	}
}

func errorResult(err error, start time.Time) Result {
	// Classify the error.
	ec := "connect"
	detail := err.Error()
	switch {
	case isTimeout(err):
		ec = "timeout"
	case isBlocked(err):
		ec = "blocked_target"
	case isDNS(err):
		ec = "dns"
	case isTLS(err):
		ec = "tls"
	}
	return Result{
		ErrorClass:  ec,
		ErrorDetail: detail,
		TotalMs:     uint32(time.Since(start).Microseconds() / 1000),
	}
}

func isTimeout(err error) bool { return err != nil && strings.Contains(err.Error(), "timeout") }
func isBlocked(err error) bool {
	return errors.Is(err, guard.ErrBlockedTarget)
}
func isDNS(err error) bool { return err != nil && strings.Contains(err.Error(), "dns:") }
func isTLS(err error) bool { return err != nil && strings.Contains(err.Error(), "tls:") }
