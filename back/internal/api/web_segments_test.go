package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"go.upcontrol.io/back/internal/ingest"
)

// fakeSegmentStore holds one project's names; replaced records each PUT.
type fakeSegmentStore struct {
	names    []string
	replaced [][]string
}

func (f *fakeSegmentStore) Segments(context.Context, int64) ([]string, error) {
	return append([]string{}, f.names...), nil
}

func (f *fakeSegmentStore) ReplaceSegments(_ context.Context, _ int64, names []string) error {
	f.replaced = append(f.replaced, names)
	f.names = names
	return nil
}

func putSegments(t *testing.T, store segmentStore, keys keyResolver, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPut, "/v1/web/segments", strings.NewReader(body))
	req.Header.Set("X-Upcontrol-Key", "uc_live_test")
	rr := httptest.NewRecorder()
	segmentsByKey(rr, req, keys, store)
	return rr
}

func secretKeys() *webStubKeys {
	return &webStubKeys{tenant: ingest.Tenant{TenantID: 7, ProjectID: 9, Kind: "secret"}}
}

func TestDeclaredSegments(t *testing.T) {
	for _, tc := range []struct {
		in   []string
		want []string
		ok   bool
	}{
		{[]string{}, []string{}, true},
		{[]string{"member", "anon"}, []string{"member", "anon"}, true},
		{[]string{"a", "a", "b", "c", "d", "b"}, []string{"a", "b", "c", "d"}, true}, // duplicates fold before the cap
		{[]string{"a", "b", "c", "d", "e"}, nil, false},
		{[]string{"member", "Member"}, nil, false},
		{[]string{"user_12345"}, nil, false},
		{[]string{""}, nil, false},
	} {
		got, ok := declaredSegments(tc.in)
		if ok != tc.ok || (ok && !slices.Equal(got, tc.want)) {
			t.Errorf("declaredSegments(%q) = %q, %v; want %q, %v", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

// A secret key replaces the set and the door's cached names for the project
// go with it; the answer is the stored list.
func TestPutSegmentsBySecretKey(t *testing.T) {
	store := &fakeSegmentStore{names: []string{"old"}}
	webSegments.put(9, []string{"old"}, time.Now())
	rr := putSegments(t, store, secretKeys(), `{"names":["member","member","anon"]}`)
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `{"names":["member","anon"]}`) {
		t.Fatalf("%d %s, want 200 and the deduped list", rr.Code, rr.Body.String())
	}
	if len(store.replaced) != 1 || !slices.Equal(store.replaced[0], []string{"member", "anon"}) {
		t.Errorf("replaced with %q", store.replaced)
	}
	if _, hit := webSegments.get(9, time.Now()); hit {
		t.Error("the door's cache still holds the project's old names")
	}
}

func TestPutSegmentsRefusals(t *testing.T) {
	for _, tc := range []struct {
		name string
		keys *webStubKeys
		body string
		want int
	}{
		{"a public key", &webStubKeys{tenant: publicWebTenant()}, `{"names":["member"]}`, http.StatusUnauthorized},
		{"no key of ours", &webStubKeys{err: context.Canceled}, `{"names":["member"]}`, http.StatusUnauthorized},
		{"five names", secretKeys(), `{"names":["a","b","c","d","e"]}`, http.StatusBadRequest},
		{"a bad shape", secretKeys(), `{"names":["Member"]}`, http.StatusBadRequest},
		{"no names", secretKeys(), `{}`, http.StatusBadRequest},
		{"not json", secretKeys(), `names`, http.StatusBadRequest},
	} {
		store := &fakeSegmentStore{}
		if rr := putSegments(t, store, tc.keys, tc.body); rr.Code != tc.want || len(store.replaced) != 0 {
			t.Errorf("%s: %d %s, %d writes; want %d and none", tc.name, rr.Code, rr.Body.String(), len(store.replaced), tc.want)
		}
	}
}

// A session reaching no project reads an empty list and writes nothing.
func TestSegmentsWithoutAProject(t *testing.T) {
	store := &fakeSegmentStore{names: []string{"member"}}
	rr := httptest.NewRecorder()
	serveSegments(rr, httptest.NewRequest(http.MethodGet, "/v1/web/segments", nil), store, 0)
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"names":[]`) {
		t.Errorf("GET: %d %s, want an empty list", rr.Code, rr.Body.String())
	}
	rr = httptest.NewRecorder()
	serveSegments(rr, httptest.NewRequest(http.MethodPut, "/v1/web/segments", strings.NewReader(`{"names":["a"]}`)), store, 0)
	if rr.Code != http.StatusNotFound || len(store.replaced) != 0 {
		t.Errorf("PUT: %d, %d writes; want 404 and none", rr.Code, len(store.replaced))
	}
}
