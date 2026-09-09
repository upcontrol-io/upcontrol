//go:build integration

// The key set: GET/POST /v1/keys and DELETE /v1/keys/{id} against a real
// database, driven through ServeHTTP over the workspace owner's fixed
// identity, the way dashboard_store_integration_test drives the board.
// Run with -tags=integration and UC_TEST_POSTGRES set.

package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.upcontrol.io/back/internal/storage/pg"
)

// keysAPI is the keys handler behind the owner's fixed identity: ServeHTTP
// resolves the current project off it, as a cookie session would.
func keysAPI(t *testing.T, pool *pg.Pool, tenantID int64) *keys {
	t.Helper()
	w := planTenantAPI(t, pool, tenantID)
	return NewKeys(w.pool, w.sess)
}

// callKeys drives one request; an empty body is no body at all, which is the
// optional-body arm of POST /v1/keys.
func callKeys(t *testing.T, h *keys, method, path, body string) (int, string) {
	t.Helper()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(method, path, strings.NewReader(body)))
	return w.Code, strings.TrimSpace(w.Body.String())
}

type keyCard struct {
	Kind      string   `json:"kind"`
	Origins   []string `json:"origins"`
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	State     string   `json:"state"`
	RevokedAt *string  `json:"revokedAt"`
}

type keysAnswer struct {
	Key  keyCard   `json:"key"`
	Keys []keyCard `json:"keys"`
}

func readKeys(t *testing.T, h *keys) (keysAnswer, string) {
	t.Helper()
	code, body := callKeys(t, h, http.MethodGet, "/v1/keys", "")
	if code != http.StatusOK {
		t.Fatalf("GET /v1/keys = %d %s", code, body)
	}
	var a keysAnswer
	if err := json.Unmarshal([]byte(body), &a); err != nil {
		t.Fatalf("GET /v1/keys must answer KeysResponse: %v (%s)", err, body)
	}
	return a, body
}

func findKey(t *testing.T, a keysAnswer, id string) keyCard {
	t.Helper()
	for _, k := range a.Keys {
		if k.ID == id {
			return k
		}
	}
	t.Fatalf("key %s must be listed; got %v", id, a.Keys)
	return keyCard{}
}

// mintCreationKey does what project creation does on the write path: one
// unnamed key through the one mint.
func mintCreationKey(t *testing.T, pool *pg.Pool, tenantID, projectID int64) {
	t.Helper()
	if _, err := issueKey(t.Context(), pool, tenantID, projectID); err != nil {
		t.Fatalf("mint the creation key: %v", err)
	}
}

func TestAFreshProjectListsExactlyItsCreationKey(t *testing.T) {
	pool := openProjectsGateDB(t)
	tenantID := seedPlanTenant(t, pool, "Free", 1)
	mintCreationKey(t, pool, tenantID, boardOf(t, pool, tenantID))
	h := keysAPI(t, pool, tenantID)

	a, _ := readKeys(t, h)
	if len(a.Keys) != 1 || a.Keys[0].State != "active" {
		t.Fatalf("a fresh project lists exactly its creation key; got %v", a.Keys)
	}
	if a.Key.ID != a.Keys[0].ID {
		t.Fatalf("key still names the newest live one; key=%v keys[0]=%v", a.Key, a.Keys[0])
	}
}

func TestIssuedKeyCarriesItsNameAndTheValueShowsOnce(t *testing.T) {
	pool := openProjectsGateDB(t)
	tenantID := seedPlanTenant(t, pool, "Free", 1)
	mintCreationKey(t, pool, tenantID, boardOf(t, pool, tenantID))
	h := keysAPI(t, pool, tenantID)

	code, body := callKeys(t, h, http.MethodPost, "/v1/keys", `{"name":"staging"}`)
	if code != http.StatusCreated {
		t.Fatalf("issue = %d %s", code, body)
	}
	var issued struct {
		ID    string `json:"id"`
		Name  string `json:"name"`
		Value string `json:"value"`
	}
	if err := json.Unmarshal([]byte(body), &issued); err != nil {
		t.Fatalf("issue must answer IssuedKey: %v (%s)", err, body)
	}
	if issued.Name != "staging" || !strings.HasPrefix(issued.Value, "uc_live_") {
		t.Fatalf("issue answers the name and the one-time value; got %s", body)
	}

	a, listBody := readKeys(t, h)
	if len(a.Keys) != 2 {
		t.Fatalf("issuing grows the set to two; got %d", len(a.Keys))
	}
	if k := findKey(t, a, issued.ID); k.Name != "staging" || k.State != "active" {
		t.Fatalf("the new key carries its name in the list; got %+v", k)
	}
	if strings.Contains(listBody, issued.Value) {
		t.Fatal("the full key appears in a later read; it is shown exactly once")
	}
}

func TestRevokingOneKeyLeavesTheOthersListedAndActive(t *testing.T) {
	pool := openProjectsGateDB(t)
	tenantID := seedPlanTenant(t, pool, "Free", 1)
	mintCreationKey(t, pool, tenantID, boardOf(t, pool, tenantID))
	h := keysAPI(t, pool, tenantID)
	first, _ := readKeys(t, h)
	creation := first.Keys[0].ID

	code, issueBody := callKeys(t, h, http.MethodPost, "/v1/keys", `{"name":"staging"}`)
	if code != http.StatusCreated {
		t.Fatalf("issue = %d %s", code, issueBody)
	}
	var issued struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(issueBody), &issued); err != nil {
		t.Fatalf("issue must answer IssuedKey: %v (%s)", err, issueBody)
	}

	if code, body := callKeys(t, h, http.MethodDelete, "/v1/keys/"+creation, ""); code != http.StatusNoContent {
		t.Fatalf("revoke = %d %s", code, body)
	}
	a, _ := readKeys(t, h)
	if len(a.Keys) != 2 {
		t.Fatalf("a revoked key stays in the list, it is not deleted; got %d", len(a.Keys))
	}
	if k := findKey(t, a, issued.ID); k.State != "active" {
		t.Fatalf("the other key is untouched; got %+v", k)
	}
	withdrawn := findKey(t, a, creation)
	if withdrawn.State != "revoked" || withdrawn.RevokedAt == nil {
		t.Fatalf("the withdrawn one is marked with a revokedAt; got %+v", withdrawn)
	}
}

func TestRevokeRefusesRepeatsGarbageAndOtherProjects(t *testing.T) {
	pool := openProjectsGateDB(t)
	tenantID := seedPlanTenant(t, pool, "Free", 1)
	mintCreationKey(t, pool, tenantID, boardOf(t, pool, tenantID))
	h := keysAPI(t, pool, tenantID)
	only, _ := readKeys(t, h)
	id := only.Keys[0].ID

	if code, body := callKeys(t, h, http.MethodDelete, "/v1/keys/"+id, ""); code != http.StatusNoContent {
		t.Fatalf("first revoke = %d %s", code, body)
	}
	if code, _ := callKeys(t, h, http.MethodDelete, "/v1/keys/"+id, ""); code != http.StatusNotFound {
		t.Fatalf("revoking the same id twice = %d, want 404", code)
	}
	if code, _ := callKeys(t, h, http.MethodDelete, "/v1/keys/garbage", ""); code != http.StatusNotFound {
		t.Fatalf("an unparseable id is a 404, never a 400; got %d", code)
	}

	// Another project's key, driven through THIS project's identity.
	tenantB := seedPlanTenant(t, pool, "Free", 1)
	mintCreationKey(t, pool, tenantB, boardOf(t, pool, tenantB))
	hB := keysAPI(t, pool, tenantB)
	other, _ := readKeys(t, hB)
	foreign := other.Keys[0].ID
	if code, _ := callKeys(t, h, http.MethodDelete, "/v1/keys/"+foreign, ""); code != http.StatusNotFound {
		t.Fatalf("another project's id = %d, want 404", code)
	}
	if b, _ := readKeys(t, hB); len(b.Keys) != 1 || b.Keys[0].State != "active" {
		t.Fatalf("the refused foreign revoke must reach nothing; B reads %v", b.Keys)
	}
}

func TestTheSixthLiveKeyIsA409NeverAWall(t *testing.T) {
	pool := openProjectsGateDB(t)
	tenantID := seedPlanTenant(t, pool, "Free", 1)
	for i := 0; i < maxLiveAPIKeys; i++ { // the ceiling, all of them working
		mintCreationKey(t, pool, tenantID, boardOf(t, pool, tenantID))
	}
	h := keysAPI(t, pool, tenantID)

	code, body := callKeys(t, h, http.MethodPost, "/v1/keys", "")
	if code != http.StatusConflict || !strings.Contains(body, "key_limit") {
		t.Fatalf("the key past the ceiling is a 409 key_limit; got %d %s", code, body)
	}
	if !strings.Contains(body, "revoke one first") {
		t.Fatalf("the refusal names the fix; got %s", body)
	}
	var refusal map[string]any
	if err := json.Unmarshal([]byte(body), &refusal); err != nil {
		t.Fatalf("the refusal must be a document: %v (%s)", err, body)
	}
	if _, has := refusal["upgrade"]; has {
		t.Fatalf("the key cap is a fixed ceiling, never a paid wall; got %s", body)
	}

	// Revoked keys do not count, and the body stays optional: a bare POST
	// replaces the withdrawn one.
	last, _ := readKeys(t, h)
	if code, _ := callKeys(t, h, http.MethodDelete, "/v1/keys/"+last.Keys[0].ID, ""); code != http.StatusNoContent {
		t.Fatalf("withdraw one = %d", code)
	}
	if code, body := callKeys(t, h, http.MethodPost, "/v1/keys", ""); code != http.StatusCreated {
		t.Fatalf("a revoked key frees a slot; got %d %s", code, body)
	}
}

// Rotation is the everything-now lever for SECRET keys. A public key must survive
// it: it lives in a deployed browser bundle, its replacement would be a uc_live_
// key that cannot go there, and the only symptom of getting this wrong is a
// website that goes quiet 24 hours later.
func TestRotationRetiresTheSecretKeysAndSparesThePublicOne(t *testing.T) {
	pool := openProjectsGateDB(t)
	tenantID := seedPlanTenant(t, pool, "Free", 1)
	mintCreationKey(t, pool, tenantID, boardOf(t, pool, tenantID))
	h := keysAPI(t, pool, tenantID)

	code, body := callKeys(t, h, http.MethodPost, "/v1/keys",
		`{"name":"site","kind":"public","origins":["https://example.com"]}`)
	if code != http.StatusCreated {
		t.Fatalf("mint a public key = %d %s", code, body)
	}
	var minted struct {
		ID     string `json:"id"`
		Prefix string `json:"prefix"`
		Kind   string `json:"kind"`
	}
	if err := json.Unmarshal([]byte(body), &minted); err != nil {
		t.Fatalf("issue must answer the key: %v (%s)", err, body)
	}
	if !strings.HasPrefix(minted.Prefix, "uc_pub_") || minted.Kind != "public" {
		t.Fatalf("a public key is listed under its own scheme; got %s / %s", minted.Prefix, minted.Kind)
	}

	if code, body := callKeys(t, h, http.MethodPost, "/v1/keys/rotate", ""); code != http.StatusOK {
		t.Fatalf("rotate = %d %s", code, body)
	}

	a, _ := readKeys(t, h)
	pub := findKey(t, a, minted.ID)
	if pub.State != "active" {
		t.Fatalf("the public key must still be active after a rotation; got %q", pub.State)
	}
	// One new secret key, and exactly one: `old` returns a row per retired key, so
	// an INSERT..SELECT over it would mint one replacement per old key.
	fresh := 0
	for _, k := range a.Keys {
		if k.State == "active" && k.ID != minted.ID {
			fresh++
		}
	}
	if fresh != 1 {
		t.Fatalf("rotation issues exactly one new secret key; got %d active others in %v", fresh, a.Keys)
	}
}

// A project holding only a public key has nothing to rotate, and says so rather
// than answering 500 from an empty INSERT..SELECT.
func TestRotationWithNoSecretKeyIsRefusedInWords(t *testing.T) {
	pool := openProjectsGateDB(t)
	tenantID := seedPlanTenant(t, pool, "Free", 1)
	h := keysAPI(t, pool, tenantID)

	if code, body := callKeys(t, h, http.MethodPost, "/v1/keys",
		`{"kind":"public","origins":["https://example.com"]}`); code != http.StatusCreated {
		t.Fatalf("mint a public key = %d %s", code, body)
	}
	code, body := callKeys(t, h, http.MethodPost, "/v1/keys/rotate", "")
	if code != http.StatusConflict || !strings.Contains(body, "nothing_to_rotate") {
		t.Fatalf("rotating with no secret key = 409 nothing_to_rotate; got %d %s", code, body)
	}
}

// A public key must list the origins it may be sent from: with none it is exactly
// the unscoped key it exists to replace, so it is refused at the door.
func TestAPublicKeyWithoutOriginsIsRefused(t *testing.T) {
	pool := openProjectsGateDB(t)
	tenantID := seedPlanTenant(t, pool, "Free", 1)
	h := keysAPI(t, pool, tenantID)

	code, body := callKeys(t, h, http.MethodPost, "/v1/keys", `{"kind":"public","origins":[]}`)
	if code != http.StatusBadRequest || !strings.Contains(body, "origins_required") {
		t.Fatalf("a public key with no origins = 400 origins_required; got %d %s", code, body)
	}
}
