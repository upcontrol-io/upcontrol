//go:build integration

// The install token door: who may mint one, how long it lives, and what
// deletes an unredeemed one. Driven through the real handlers over the fixed
// identity the other integration tests use (projects_gate_test.go's helpers),
// against a real database. Run with -tags=integration, UC_TEST_POSTGRES set.

package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.upcontrol.io/back/internal/account/session"
	"go.upcontrol.io/back/internal/storage/pg"
)

// installDoorFor is the install handler behind one person's identity: the
// workspace owner mints, a notify member is refused.
func installDoorFor(t *testing.T, pool *pg.Pool, personID, tenantID int64) *install {
	t.Helper()
	return NewInstall(pool, nil, session.New(pool, session.DefaultTTL, nil).
		WithFixedIdentity(personID, tenantID), "", false)
}

// callInstall drives one install request. Every call names its own client
// address: the redeem door throttles per client, so two calls from one address
// inside the cooldown would answer 429 about nothing under test here.
func callInstall(t *testing.T, h *install, path, body, client string) (int, string) {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	r.Header.Set("X-Forwarded-For", client)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w.Code, strings.TrimSpace(w.Body.String())
}

// installFixture: a Free workspace holding one project, plus its owner's door.
type installFixture struct {
	pool      *pg.Pool
	tenantID  int64
	projectID int64
	door      *install
}

func newInstallFixture(t *testing.T) *installFixture {
	t.Helper()
	pool := openProjectsGateDB(t)
	tenantID := seedPlanTenant(t, pool, "Free", 1)
	var ownerID int64
	if err := pool.Raw().QueryRow(t.Context(),
		`SELECT owner_person_id FROM tenant WHERE id = $1`, tenantID).Scan(&ownerID); err != nil {
		t.Fatalf("read the workspace owner: %v", err)
	}
	return &installFixture{
		pool:      pool,
		tenantID:  tenantID,
		projectID: boardOf(t, pool, tenantID),
		door:      installDoorFor(t, pool, ownerID, tenantID),
	}
}

// tokens counts this project's install_token rows, spent or not.
func (f *installFixture) tokens(t *testing.T) int64 {
	t.Helper()
	var n int64
	if err := f.pool.Raw().QueryRow(t.Context(),
		`SELECT count(*) FROM install_token WHERE project_id = $1`, f.projectID).Scan(&n); err != nil {
		t.Fatalf("count install_token: %v", err)
	}
	return n
}

// mintToken mints one through the owner's door and answers the raw token.
func (f *installFixture) mintToken(t *testing.T, client string) string {
	t.Helper()
	code, body := callInstall(t, f.door, "/v1/install/token", "", client)
	if code != http.StatusOK {
		t.Fatalf("mint an install token = %d %s, want 200", code, body)
	}
	var out struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil || out.Token == "" {
		t.Fatalf("the mint must answer a token: %v (%s)", err, body)
	}
	return out.Token
}

// A Member may not mint one: the token redeems into a secret key, and that is
// exactly what POST /v1/keys refuses them. Without the same gate the install
// card is the way around it, and a refused mint leaves no row behind either.
func TestInstallTokenRefusesANotifyMember(t *testing.T) {
	f := newInstallFixture(t)
	var memberID int64
	if err := f.pool.Raw().QueryRow(t.Context(),
		`INSERT INTO person (public_id, email, name) VALUES (gen_random_uuid(), $1, 'Member') RETURNING id`,
		fmt.Sprintf("member-%d@example.com", time.Now().UnixNano())).Scan(&memberID); err != nil {
		t.Fatalf("seed the member: %v", err)
	}
	if _, err := f.pool.Raw().Exec(t.Context(),
		`INSERT INTO project_member (project_id, person_id, tenant_id, role, status)
		 VALUES ($1, $2, $3, 'notify', 'active')`, f.projectID, memberID, f.tenantID); err != nil {
		t.Fatalf("seed the membership: %v", err)
	}

	code, body := callInstall(t, installDoorFor(t, f.pool, memberID, f.tenantID),
		"/v1/install/token", "", "203.0.113.20")
	if code != http.StatusForbidden || !strings.Contains(body, "notify_role") {
		t.Fatalf("a notify member minting an install token = %d %s, want 403 notify_role", code, body)
	}
	if n := f.tokens(t); n != 0 {
		t.Fatalf("install_token rows = %d, want 0: a refused mint writes nothing", n)
	}
}

// The manager's token, and the day it lives: the card promises 24 hours, and a
// page may not promise what the backend does not do.
func TestInstallTokenForAManagerLastsADay(t *testing.T) {
	f := newInstallFixture(t)
	code, body := callInstall(t, f.door, "/v1/install/token", "", "203.0.113.21")
	if code != http.StatusOK {
		t.Fatalf("a manager minting an install token = %d %s, want 200", code, body)
	}
	var out struct {
		Token     string    `json:"token"`
		Command   string    `json:"command"`
		ExpiresAt time.Time `json:"expiresAt"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("the mint answers the token, the command and the expiry: %v (%s)", err, body)
	}
	if !strings.HasPrefix(out.Token, "uct_") || !strings.Contains(out.Command, out.Token) {
		t.Fatalf("the command must carry the token it was minted with; got %s", body)
	}
	if left := time.Until(out.ExpiresAt); left < 23*time.Hour || left > 24*time.Hour+time.Minute {
		t.Fatalf("a token minted now expires in %s, want about 24 hours", left)
	}
	if n := f.tokens(t); n != 1 {
		t.Fatalf("install_token rows = %d, want 1", n)
	}
}

// Rotation is the everything-now lever, so it takes the unredeemed tokens with
// it: each one is a secret key waiting to be minted, and a rotation that left
// them standing would contain nothing. The token then answers the 404 a spent
// one does.
func TestInstallTokenDiesWithARotation(t *testing.T) {
	f := newInstallFixture(t)
	mintCreationKey(t, f.pool, f.tenantID, f.projectID) // rotation needs an active secret key
	token := f.mintToken(t, "203.0.113.22")

	code, body := callKeys(t, keysAPI(t, f.pool, f.tenantID), http.MethodPost, "/v1/keys/rotate", "")
	if code != http.StatusOK {
		t.Fatalf("rotate = %d %s", code, body)
	}
	if n := f.tokens(t); n != 0 {
		t.Fatalf("%d unredeemed install tokens survived the rotation", n)
	}
	code, body = callInstall(t, f.door, "/v1/install/redeem", `{"token":"`+token+`"}`, "203.0.113.23")
	if code != http.StatusNotFound || !strings.Contains(body, "invalid_token") {
		t.Fatalf("redeeming a rotated-away token = %d %s, want 404 invalid_token", code, body)
	}
}

// An expired token is the same 404 as a spent or an unknown one: no oracle.
func TestInstallTokenPastItsExpiryIs404(t *testing.T) {
	f := newInstallFixture(t)
	token := f.mintToken(t, "203.0.113.24")
	// Age it rather than wait a day for it.
	if _, err := f.pool.Raw().Exec(t.Context(),
		`UPDATE install_token SET expires_at = now() - interval '1 minute' WHERE project_id = $1`,
		f.projectID); err != nil {
		t.Fatalf("age the token: %v", err)
	}

	code, body := callInstall(t, f.door, "/v1/install/redeem", `{"token":"`+token+`"}`, "203.0.113.25")
	if code != http.StatusNotFound || !strings.Contains(body, "invalid_token") {
		t.Fatalf("redeeming an expired token = %d %s, want 404 invalid_token", code, body)
	}
	if n := f.tokens(t); n != 1 {
		t.Fatalf("install_token rows = %d: a refused redeem deletes nothing", n)
	}
}

// The origin list is the whole scope of a public key, so the check belongs at
// the mint and not on the card that collects it: * and null are valid from
// every page on the web, and an origin carrying a path matches no Origin
// header at all, so the key would authenticate nowhere.
func TestPublicKeyOriginsAreCheckedAtMint(t *testing.T) {
	pool := openProjectsGateDB(t)
	tenantID := seedPlanTenant(t, pool, "Free", 1)
	h := keysAPI(t, pool, tenantID)

	for _, bad := range []string{"null", "*", "example.com", "https://a.com/path"} {
		code, body := callKeys(t, h, http.MethodPost, "/v1/keys",
			`{"kind":"public","origins":["`+bad+`"]}`)
		if code != http.StatusBadRequest || !strings.Contains(body, "bad_origin") {
			t.Fatalf("origin %q = %d %s, want 400 bad_origin", bad, code, body)
		}
	}
	for _, good := range []string{"https://a.com", "http://localhost:5173"} {
		code, body := callKeys(t, h, http.MethodPost, "/v1/keys",
			`{"kind":"public","origins":["`+good+`"]}`)
		if code != http.StatusCreated {
			t.Fatalf("origin %q = %d %s, want 201", good, code, body)
		}
	}
}
