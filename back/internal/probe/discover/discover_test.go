package discover

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"
)

// fakeProber answers from a path→status table and records what was asked, so a
// test can assert on the requests this package makes as well as its answers.
type fakeProber struct {
	mu     sync.Mutex
	status map[string]uint16
	asked  []string
	block  chan struct{} // when non-nil, every Execute waits on it
}

func (f *fakeProber) Execute(ctx context.Context, spec CheckSpec) Result {
	if f.block != nil {
		select {
		case <-f.block:
		case <-ctx.Done():
			return Result{ErrorClass: "timeout"}
		}
	}
	path := spec.URL[strings.LastIndex(spec.URL, "/"):]
	f.mu.Lock()
	f.asked = append(f.asked, path)
	code, ok := f.status[path]
	f.mu.Unlock()
	if !ok {
		code = 404
	}
	return Result{StatusCode: code, OK: code < 400}
}

func (f *fakeProber) askedPaths() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.asked...)
}

func okRes(h http.Header) Result { return Result{StatusCode: 200, OK: true, Header: h} }

func TestRunSkipsAHostThatNeverAnswered(t *testing.T) {
	// A host that refused, timed out or was blocked must not then be sent five
	// more requests; for blocked_target it is the SSRF guard honoured.
	for _, res := range []Result{
		{StatusCode: 0, ErrorClass: "timeout"},
		{StatusCode: 0, ErrorClass: "dns"},
		{ErrorClass: "blocked_target"},
	} {
		p := &fakeProber{status: map[string]uint16{}}
		facts := Run(context.Background(), p, nil, "https://example.com", res)
		if got := p.askedPaths(); len(got) != 0 {
			t.Errorf("err=%s: made %v, want no requests", res.ErrorClass, got)
		}
		if facts.Headers != nil || facts.ErrorPage != nil || facts.Health != nil {
			t.Errorf("err=%s: reported facts it never measured: %+v", res.ErrorClass, facts)
		}
	}
}

func TestHeadersComeFromTheProbeWeAlreadyMade(t *testing.T) {
	h := http.Header{}
	h.Set("Strict-Transport-Security", "max-age=63072000")
	h.Set("Content-Encoding", "br")
	p := &fakeProber{status: map[string]uint16{"/health": 200}}

	facts := Run(context.Background(), p, nil, "https://example.com", okRes(h))
	if facts.Headers == nil {
		t.Fatal("headers not reported")
	}
	if !facts.Headers.HSTS {
		t.Error("HSTS = false, want true")
	}
	if facts.Headers.Compression != "br" {
		t.Errorf("Compression = %q, want br", facts.Headers.Compression)
	}
	if facts.Headers.CacheControl {
		t.Error("CacheControl = true, but the response carried none")
	}
	// No extra request may be spent on headers — the response was already held.
	for _, path := range p.askedPaths() {
		if path == "/" {
			t.Error("re-requested the homepage to read its headers")
		}
	}
}

func TestErrorPageCatchesTheSPAThatAnswers200(t *testing.T) {
	// The case the row exists for: a single-page app serves 200 + HTML for every
	// path, so an uptime checker sees "fine" straight through an outage.
	p := &fakeProber{status: map[string]uint16{missingPath: 200}}
	facts := Run(context.Background(), p, nil, "https://example.com", okRes(http.Header{}))
	if facts.ErrorPage == nil {
		t.Fatal("error page not reported")
	}
	if facts.ErrorPage.Status != 200 {
		t.Errorf("Status = %d, want 200", facts.ErrorPage.Status)
	}
	if facts.ErrorPage.Correct {
		t.Error("Correct = true for a 200 on a missing page")
	}
}

func TestErrorPageAcceptsAProper404(t *testing.T) {
	p := &fakeProber{status: map[string]uint16{}} // unknown paths answer 404
	facts := Run(context.Background(), p, nil, "https://example.com", okRes(http.Header{}))
	if facts.ErrorPage == nil || !facts.ErrorPage.Correct {
		t.Fatalf("ErrorPage = %+v, want a correct 404", facts.ErrorPage)
	}
}

func TestHealthStopsAtTheFirstHit(t *testing.T) {
	p := &fakeProber{status: map[string]uint16{"/healthz": 200}}
	facts := Run(context.Background(), p, nil, "https://example.com", okRes(http.Header{}))
	if facts.Health == nil || facts.Health.Path != "/healthz" {
		t.Fatalf("Health = %+v, want /healthz", facts.Health)
	}
	// /status and /api/health must not be asked once /healthz answered.
	for _, path := range p.askedPaths() {
		if path == "/status" || path == "/api/health" {
			t.Errorf("kept probing after a hit: %v", p.askedPaths())
		}
	}
}

func TestHealthNoneIsMeasuredNotAbsent(t *testing.T) {
	// "We looked and there is none" is a different statement from "we did not
	// look", and only the second may render as the no-data marker.
	p := &fakeProber{status: map[string]uint16{}}
	facts := Run(context.Background(), p, nil, "https://example.com", okRes(http.Header{}))
	if facts.Health == nil {
		t.Fatal("Health = nil, want a measured empty path")
	}
	if facts.Health.Path != "" {
		t.Errorf("Path = %q, want empty", facts.Health.Path)
	}
	if len(p.askedPaths()) < len(healthPaths) {
		t.Errorf("gave up early: asked %v", p.askedPaths())
	}
}

func TestBudgetExhaustionReportsUnmeasured(t *testing.T) {
	// Out of time with candidates left, the answer is "unknown" — reporting
	// "none" would claim a search that never finished.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	p := &fakeProber{status: map[string]uint16{}, block: make(chan struct{})}
	facts := Run(ctx, p, nil, "https://example.com", okRes(http.Header{}))
	if facts.Health != nil {
		t.Errorf("Health = %+v, want nil when the budget ran out", facts.Health)
	}
	if facts.ErrorPage != nil {
		t.Errorf("ErrorPage = %+v, want nil when the budget ran out", facts.ErrorPage)
	}
	// Headers still stand: they came off a response we already had.
	if facts.Headers == nil {
		t.Error("Headers = nil, but they needed no request")
	}
}
