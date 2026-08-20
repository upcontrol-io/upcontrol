package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// decodeStrict is the §14 contract: a management PATCH with a mistyped field
// answers 400 naming the field, never a silent 200 no-op. The typo case is the
// one the docs' example literally promises ("intervl" → error naming it).
func TestDecodeStrictNamesTheUnknownField(t *testing.T) {
	var req struct {
		Interval *string `json:"interval"`
	}
	r := httptest.NewRequest(http.MethodPatch, "/v1/checks/x", strings.NewReader(`{"intervl": 60}`))
	w := httptest.NewRecorder()
	if decodeStrict(w, r, &req) {
		t.Fatal("an unknown field must be refused, not accepted")
	}
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, `"unknown_field"`) || !strings.Contains(body, `"intervl"`) {
		t.Fatalf("body must name the unknown field, got %s", body)
	}
}

// The valid sparse patch passes through untouched — strictness must not
// tighten the contract for callers who spell their fields right.
func TestDecodeStrictAcceptsKnownSparseFields(t *testing.T) {
	var req struct {
		Name     *string `json:"name"`
		Interval *string `json:"interval"`
	}
	r := httptest.NewRequest(http.MethodPatch, "/v1/checks/x", strings.NewReader(`{"name":"Storefront"}`))
	w := httptest.NewRecorder()
	if !decodeStrict(w, r, &req) {
		t.Fatalf("a known sparse patch must decode, got %d %s", w.Code, w.Body.String())
	}
	if req.Name == nil || *req.Name != "Storefront" {
		t.Fatal("the field did not decode")
	}
	if req.Interval != nil {
		t.Fatal("an absent field must stay nil — that is how sparse works")
	}
}

// Garbage is garbage loudly: a broken body is a 400, never a half-decoded
// zero-value struct the handler then "applies".
func TestDecodeStrictRefusesMalformedBody(t *testing.T) {
	var req struct {
		Role string `json:"role"`
	}
	r := httptest.NewRequest(http.MethodPatch, "/v1/recipients/x", strings.NewReader(`{nope`))
	w := httptest.NewRecorder()
	if decodeStrict(w, r, &req) {
		t.Fatal("malformed JSON must be refused")
	}
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}
