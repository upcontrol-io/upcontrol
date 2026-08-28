package ingest

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.upcontrol.io/back/internal/ingest/cardinality"
)

type fakeKeys struct{ bad bool }

func (f *fakeKeys) Resolve(_ context.Context, key string) (Tenant, error) {
	if f.bad || key == "bad" {
		return Tenant{}, ErrBadKey
	}
	return Tenant{TenantID: 7, ProjectID: 9}, nil
}

type fakeSeq struct{ n int64 }

func (f *fakeSeq) Next(_ context.Context, _ int64) (int64, error) { f.n++; return f.n, nil }

type fakeSink struct {
	added int
	rows  [][]byte
}

func (f *fakeSink) Add(_ context.Context, _ string, row []byte) error {
	f.added++
	f.rows = append(f.rows, row)
	return nil
}

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

func TestKeyInHeader(t *testing.T) {
	h, sink, _ := newIngester(0, nil)
	rr := post(t, h, `{"msg":"hi"}`, map[string]string{"X-Upcontrol-Key": "uc_live_x"})
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
	rr := post(t, h, `{"msg":"hi"}`, map[string]string{"X-Upcontrol-Key": "bad"})
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("code %d, want 401", rr.Code)
	}
}

func TestNDJSONAcceptedCount(t *testing.T) {
	h, sink, _ := newIngester(0, nil)
	body := `{"msg":"a"}` + "\n" + `{"msg":"b"}` + "\n" + `{"msg":"c"}` + "\n"
	rr := post(t, h, body, map[string]string{"X-Upcontrol-Key": "k"})
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
	rr := post(t, h, body, map[string]string{"X-Upcontrol-Key": "k"})
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

func TestRowCarriesFingerprintAndLevelRaw(t *testing.T) {
	h, sink, _ := newIngester(0, nil)
	body := `{"level":"ERROR","message":"user 42 not found"}` + "\n" +
		`{"level":"ERROR","message":"user 7 not found"}` + "\n"
	rr := post(t, h, body, map[string]string{"X-Upcontrol-Key": "k"})
	if rr.Code != http.StatusOK {
		t.Fatalf("code %d body %s", rr.Code, rr.Body.String())
	}
	if len(sink.rows) != 2 {
		t.Fatalf("sink rows %d, want 2", len(sink.rows))
	}
	var fps [2]uint64
	for i, raw := range sink.rows {
		var env RowEnvelope
		if err := json.Unmarshal(raw, &env); err != nil {
			t.Fatalf("row %d: %v", i, err)
		}
		if env.Fingerprint == 0 {
			t.Errorf("row %d fingerprint is 0", i)
		}
		if env.Level != "error" || env.LevelRaw != "ERROR" {
			t.Errorf("row %d level=%q level_raw=%q", i, env.Level, env.LevelRaw)
		}
		fps[i] = env.Fingerprint
	}
	if fps[0] != fps[1] {
		t.Errorf("same-shape messages fingerprinted apart: %d != %d", fps[0], fps[1])
	}
}

func TestAttrValuesAreScrubbed(t *testing.T) {
	h, sink, _ := newIngester(0, nil)
	// The Bearer vector from scrub_test.go, riding an attr instead of the message.
	body := `{"msg":"auth ok","auth":"Bearer supersecrettoken1234567890"}`
	rr := post(t, h, body, map[string]string{"X-Upcontrol-Key": "k"})
	if rr.Code != http.StatusOK {
		t.Fatalf("code %d body %s", rr.Code, rr.Body.String())
	}
	if !contains(rr.Body.String(), `"code":"scrubbed"`) {
		t.Errorf("missing scrubbed warning: %s", rr.Body.String())
	}
	if contains(rr.Body.String(), "supersecrettoken1234567890") {
		t.Errorf("secret leaked into receipt: %s", rr.Body.String())
	}
	var env RowEnvelope
	if err := json.Unmarshal(sink.rows[0], &env); err != nil {
		t.Fatalf("row: %v", err)
	}
	if contains(env.Attrs["auth"], "supersecrettoken1234567890") {
		t.Errorf("secret survived in attr value: %q", env.Attrs["auth"])
	}
}

func TestAttrsCappedAtSixtyFourKeys(t *testing.T) {
	h, sink, _ := newIngester(0, nil)
	var b strings.Builder
	b.WriteString(`{"msg":"cap"`)
	for i := 0; i < 100; i++ {
		fmt.Fprintf(&b, `,"k%02d":"v%02d"`, i, i)
	}
	b.WriteString("}")
	rr := post(t, h, b.String(), map[string]string{"X-Upcontrol-Key": "k"})
	if rr.Code != http.StatusOK {
		t.Fatalf("code %d body %s", rr.Code, rr.Body.String())
	}
	if !contains(rr.Body.String(), `"code":"attr_key_capped"`) {
		t.Errorf("missing attr_key_capped warning: %s", rr.Body.String())
	}
	var env RowEnvelope
	if err := json.Unmarshal(sink.rows[0], &env); err != nil {
		t.Fatalf("row: %v", err)
	}
	if len(env.Attrs) != 64 {
		t.Fatalf("stored %d attrs, want 64", len(env.Attrs))
	}
	for i := 0; i < 100; i++ {
		_, ok := env.Attrs[fmt.Sprintf("k%02d", i)]
		if ok != (i < 64) {
			t.Errorf("k%02d present=%v, want %v (sorted-order keep)", i, ok, i < 64)
		}
	}
}

func TestAttrCapIsDeterministic(t *testing.T) {
	attrs := make(map[string]string, 100)
	for i := 0; i < 100; i++ {
		attrs[fmt.Sprintf("k%02d", i)] = "v"
	}
	kept := func() map[string]bool {
		out, _, _ := capAttrs(attrs)
		set := make(map[string]bool, len(out))
		for k := range out {
			set[k] = true
		}
		return set
	}
	a, b := kept(), kept()
	if len(a) != 64 {
		t.Fatalf("kept %d keys, want 64", len(a))
	}
	for k := range a {
		if !b[k] {
			t.Fatalf("two caps disagree on key %q", k)
		}
	}
}

func TestLongAttrValueTruncated(t *testing.T) {
	h, sink, _ := newIngester(0, nil)
	body := `{"msg":"cap","big":"` + strings.Repeat("a", 20000) + `"}`
	rr := post(t, h, body, map[string]string{"X-Upcontrol-Key": "k"})
	if rr.Code != http.StatusOK {
		t.Fatalf("code %d body %s", rr.Code, rr.Body.String())
	}
	if !contains(rr.Body.String(), `"code":"field_cap_exceeded"`) {
		t.Errorf("missing field_cap_exceeded warning: %s", rr.Body.String())
	}
	var env RowEnvelope
	if err := json.Unmarshal(sink.rows[0], &env); err != nil {
		t.Fatalf("row: %v", err)
	}
	if v, ok := env.Attrs["big"]; !ok || len(v) != MaxAttrValBytes {
		t.Errorf("attr big present=%v len=%d, want len %d", ok, len(v), MaxAttrValBytes)
	}
}

func TestLongAttrKeyTruncated(t *testing.T) {
	h, sink, _ := newIngester(0, nil)
	longKey := strings.Repeat("k", 300)
	body := `{"msg":"cap","` + longKey + `":"v"}`
	rr := post(t, h, body, map[string]string{"X-Upcontrol-Key": "k"})
	if rr.Code != http.StatusOK {
		t.Fatalf("code %d body %s", rr.Code, rr.Body.String())
	}
	if !contains(rr.Body.String(), `"code":"attr_key_capped"`) {
		t.Errorf("missing attr_key_capped warning: %s", rr.Body.String())
	}
	var env RowEnvelope
	if err := json.Unmarshal(sink.rows[0], &env); err != nil {
		t.Fatalf("row: %v", err)
	}
	if len(env.Attrs) != 1 {
		t.Fatalf("stored %d attrs, want 1", len(env.Attrs))
	}
	for k := range env.Attrs {
		if k != longKey[:MaxAttrKeyBytes] {
			t.Errorf("key len %d, want the %d-byte prefix", len(k), MaxAttrKeyBytes)
		}
	}
}

func TestNormalAttrsProduceNoCapWarning(t *testing.T) {
	h, sink, _ := newIngester(0, nil)
	rr := post(t, h, `{"msg":"ok","a":"1","b":"2","c":"3"}`, map[string]string{"X-Upcontrol-Key": "k"})
	if rr.Code != http.StatusOK {
		t.Fatalf("code %d body %s", rr.Code, rr.Body.String())
	}
	if contains(rr.Body.String(), `"code":"attr_key_capped"`) || contains(rr.Body.String(), `"code":"field_cap_exceeded"`) {
		t.Errorf("cap fired on normal attrs: %s", rr.Body.String())
	}
	var env RowEnvelope
	if err := json.Unmarshal(sink.rows[0], &env); err != nil {
		t.Fatalf("row: %v", err)
	}
	for k, want := range map[string]string{"a": "1", "b": "2", "c": "3"} {
		if env.Attrs[k] != want {
			t.Errorf("attr %q = %q, want %q", k, env.Attrs[k], want)
		}
	}
}

func TestIdempotencyReplayNoDoubleWrite(t *testing.T) {
	h, sink, idem := newIngester(0, nil)
	body := `{"msg":"once"}`
	hdr := map[string]string{"X-Upcontrol-Key": "k"}
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
	rr := post(t, h, body, map[string]string{"X-Upcontrol-Key": "k"})
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
	rr := post(t, h, body, map[string]string{"X-Upcontrol-Key": "k"})
	if rr.Code != http.StatusOK {
		t.Fatalf("code %d", rr.Code)
	}
	if sink.added != 1 {
		t.Errorf("sink added %d, want 1 (info shed)", sink.added)
	}
}

func TestOverload100Rejects503(t *testing.T) {
	h, sink, _ := newIngester(100, nil)
	rr := post(t, h, `{"msg":"x"}`, map[string]string{"X-Upcontrol-Key": "k"})
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
	rr := post(t, h, body, map[string]string{"X-Upcontrol-Key": "k"})
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
