package ingest

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.upcontrol.io/back/internal/ingest/cardinality"
)

// --- fakes ---

type fakeKeys struct{ bad bool }

func (f *fakeKeys) Resolve(_ context.Context, key string) (Tenant, error) {
	if f.bad || key == "bad" {
		return Tenant{}, ErrBadKey
	}
	return Tenant{TenantID: 7, ProjectID: 9}, nil
}

type fakeSeq struct{ n int64 }

func (f *fakeSeq) Next(_ context.Context, _ int64) (int64, error) { f.n++; return f.n, nil }

type fakeSink struct{ added int }

func (f *fakeSink) Add(_ context.Context, _ string, _ []byte) error { f.added++; return nil }

type fakeIdem struct{ seen map[string]int }

func newFakeIdem() *fakeIdem { return &fakeIdem{seen: map[string]int{}} }

func (f *fakeIdem) Claim(_ context.Context, batchKey string, _ []byte, accepted int) (bool, int, error) {
	if _, ok := f.seen[batchKey]; ok {
		return true, f.seen[batchKey], nil
	}
	f.seen[batchKey] = accepted
	return false, accepted, nil
}

type fakeSpool struct{ pct int }

func (f *fakeSpool) FillPercent(_ context.Context) (int, error) { return f.pct, nil }

func newIngester(spoolPct int, card *cardinality.Limiter) (*Ingester, *fakeSink, *fakeIdem) {
	sink := &fakeSink{}
	idem := newFakeIdem()
	d := Deps{
		Keys:  &fakeKeys{},
		Seq:   &fakeSeq{},
		Sink:  sink,
		Idem:  idem,
		Spool: &fakeSpool{pct: spoolPct},
		Card:  card,
	}
	return New(d), sink, idem
}

func post(t *testing.T, h *Ingester, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/i", io.NopCloser(bytes.NewReader([]byte(body))))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rr := httptest.NewRecorder()
	h.Handle(rr, req)
	return rr
}

// --- tests ---

func TestKeyInHeader(t *testing.T) {
	h, sink, _ := newIngester(0, nil)
	rr := post(t, h, `{"msg":"hi"}`, map[string]string{"X-UpControl-Key": "uc_live_x"})
	if rr.Code != http.StatusOK {
		t.Fatalf("code %d body %s", rr.Code, rr.Body.String())
	}
	if sink.added != 1 {
		t.Errorf("sink added %d, want 1", sink.added)
	}
}

func TestKeyInBearer(t *testing.T) {
	h, _, _ := newIngester(0, nil)
	rr := post(t, h, `{"msg":"hi"}`, map[string]string{"Authorization": "Bearer uc_live_x"})
	if rr.Code != http.StatusOK {
		t.Fatalf("code %d", rr.Code)
	}
}

func TestKeyInQuery(t *testing.T) {
	h, _, _ := newIngester(0, nil)
	req := httptest.NewRequest(http.MethodPost, "/i?key=uc_live_x", bytes.NewReader([]byte(`{"msg":"hi"}`)))
	rr := httptest.NewRecorder()
	h.Handle(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("code %d", rr.Code)
	}
}

func TestKeyInBodyWarns(t *testing.T) {
	h, _, _ := newIngester(0, nil)
	rr := post(t, h, `{"key":"uc_live_x","msg":"hi"}`, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("code %d body %s", rr.Code, rr.Body.String())
	}
	if !contains(rr.Body.String(), `"code":"key_in_body"`) {
		t.Errorf("missing key_in_body warning: %s", rr.Body.String())
	}
}

func TestNoKeyIs401(t *testing.T) {
	h, sink, _ := newIngester(0, nil)
	rr := post(t, h, `{"msg":"hi"}`, nil)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("code %d, want 401", rr.Code)
	}
	if sink.added != 0 {
		t.Errorf("sink added %d on auth failure, want 0", sink.added)
	}
}

func TestBadKeyIs401(t *testing.T) {
	h, _, _ := newIngester(0, nil)
	rr := post(t, h, `{"msg":"hi"}`, map[string]string{"X-UpControl-Key": "bad"})
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("code %d, want 401", rr.Code)
	}
}

func TestNDJSONAcceptedCount(t *testing.T) {
	h, sink, _ := newIngester(0, nil)
	body := `{"msg":"a"}` + "\n" + `{"msg":"b"}` + "\n" + `{"msg":"c"}` + "\n"
	rr := post(t, h, body, map[string]string{"X-UpControl-Key": "k"})
	if rr.Code != http.StatusOK {
		t.Fatalf("code %d", rr.Code)
	}
	if !contains(rr.Body.String(), `"accepted":3`) {
		t.Errorf("body %s", rr.Body.String())
	}
	if sink.added != 3 {
		t.Errorf("sink added %d, want 3", sink.added)
	}
}

func TestScrubWarningInReceipt(t *testing.T) {
	h, _, _ := newIngester(0, nil)
	// A Stripe key in the message must be scrubbed and counted.
	body := `{"msg":"charged via sk_live_abcdefghijklmnopqrstuvwxyz"}`
	rr := post(t, h, body, map[string]string{"X-UpControl-Key": "k"})
	if rr.Code != http.StatusOK {
		t.Fatalf("code %d", rr.Code)
	}
	if !contains(rr.Body.String(), `"code":"scrubbed"`) {
		t.Errorf("missing scrubbed warning: %s", rr.Body.String())
	}
	if contains(rr.Body.String(), "sk_live_abcdefghijklmnopqrstuvwxyz") {
		t.Errorf("secret leaked into receipt: %s", rr.Body.String())
	}
}

func TestIdempotencyReplayNoDoubleWrite(t *testing.T) {
	h, sink, idem := newIngester(0, nil)
	body := `{"msg":"once"}`
	hdr := map[string]string{"X-UpControl-Key": "k"}
	r1 := post(t, h, body, hdr)
	r2 := post(t, h, body, hdr)
	if r1.Code != http.StatusOK || r2.Code != http.StatusOK {
		t.Fatalf("codes %d %d", r1.Code, r2.Code)
	}
	// Second call is a replay: sink must NOT get a second write.
	if sink.added != 1 {
		t.Errorf("sink added %d after replay, want 1 (no double-write)", sink.added)
	}
	// Both receipts report the same accepted count.
	if !contains(r2.Body.String(), `"accepted":1`) {
		t.Errorf("replay body %s", r2.Body.String())
	}
	if len(idem.seen) != 1 {
		t.Errorf("idem saw %d batches, want 1", len(idem.seen))
	}
}

func TestOverload60ShedsDebugSamplingAdvertised(t *testing.T) {
	h, sink, _ := newIngester(60, nil)
	body := `{"msg":"d","level":"debug"}` + "\n" + `{"msg":"e","level":"error"}` + "\n"
	rr := post(t, h, body, map[string]string{"X-UpControl-Key": "k"})
	if rr.Code != http.StatusOK {
		t.Fatalf("code %d", rr.Code)
	}
	// debug row shed, error row kept.
	if sink.added != 1 {
		t.Errorf("sink added %d, want 1 (debug shed)", sink.added)
	}
	if !contains(rr.Body.String(), `"code":"class_shed"`) {
		t.Errorf("missing class_shed warning: %s", rr.Body.String())
	}
	if !contains(rr.Body.String(), `"sampling"`) {
		t.Errorf("missing sampling instruction: %s", rr.Body.String())
	}
}

func TestOverload75ShedsInfo(t *testing.T) {
	h, sink, _ := newIngester(75, nil)
	body := `{"msg":"i","level":"info"}` + "\n" + `{"msg":"e","level":"error"}` + "\n"
	rr := post(t, h, body, map[string]string{"X-UpControl-Key": "k"})
	if rr.Code != http.StatusOK {
		t.Fatalf("code %d", rr.Code)
	}
	if sink.added != 1 {
		t.Errorf("sink added %d, want 1 (info shed)", sink.added)
	}
}

func TestOverload100Rejects503(t *testing.T) {
	h, sink, _ := newIngester(100, nil)
	rr := post(t, h, `{"msg":"x"}`, map[string]string{"X-UpControl-Key": "k"})
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("code %d, want 503", rr.Code)
	}
	if rr.Header().Get("Retry-After") == "" {
		t.Error("missing Retry-After on 503")
	}
	if sink.added != 0 {
		t.Errorf("sink added %d at 100%%, want 0", sink.added)
	}
}

func TestCardinalityCapWarns(t *testing.T) {
	card := cardinality.New(2) // tiny ceiling to force capping fast
	h, _, _ := newIngester(0, card)
	body := `{"msg":"a","host":"h1"}` + "\n" + `{"msg":"b","host":"h2"}` + "\n" +
		`{"msg":"c","host":"h3"}` + "\n" + `{"msg":"d","host":"h4"}` + "\n"
	rr := post(t, h, body, map[string]string{"X-UpControl-Key": "k"})
	if rr.Code != http.StatusOK {
		t.Fatalf("code %d", rr.Code)
	}
	if !contains(rr.Body.String(), `"code":"cardinality_capped"`) {
		t.Errorf("missing cardinality_capped warning: %s", rr.Body.String())
	}
}

func TestComputeOverloadSteps(t *testing.T) {
	cases := []struct {
		fill       int
		reject     bool
		shedLevels []string
	}{
		{0, false, nil},
		{59, false, nil},
		{60, false, []string{"debug"}},
		{74, false, []string{"debug"}},
		{75, false, []string{"debug", "info"}},
		{89, false, []string{"debug", "info"}},
		{90, false, []string{"debug", "info", "warn"}},
		{99, false, []string{"debug", "info", "warn"}},
		{100, true, nil},
	}
	for _, c := range cases {
		d := computeOverload(c.fill)
		if d.rejectAll != c.reject {
			t.Errorf("fill=%d reject=%v want %v", c.fill, d.rejectAll, c.reject)
		}
		for _, lvl := range c.shedLevels {
			if !d.shed[lvl] {
				t.Errorf("fill=%d should shed %s", c.fill, lvl)
			}
		}
		if c.fill < 60 && len(d.shed) != 0 {
			t.Errorf("fill=%d shed %v, want none", c.fill, d.shed)
		}
	}
}

func contains(s, sub string) bool { return bytes.Contains([]byte(s), []byte(sub)) }
