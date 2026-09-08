//go:build integration

// The agent's three key-authenticated board doors against a real database:
// replace-or-propose, append below the board, and the board read, plus the
// session's proposal doors. The key doors are driven through ServeHTTP with
// the key header, the way a deployed agent calls them; the session doors are
// driven directly over the owner's fixed identity, the way
// dashboard_store_integration_test drives the board. Run with -tags=integration
// and UC_TEST_POSTGRES set.

package api

import (
	"crypto/sha256"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	apigen "go.upcontrol.io/back/gen/api"
	"go.upcontrol.io/back/internal/account/session"
	"go.upcontrol.io/back/internal/storage/pg"
)

// seedAgentKey mints a working key directly in the table, the way issueKey
// mints one: the stored prefix is the secret's first twelve characters and the
// hash is over the whole key including its scheme. Unique per run; the prefix
// column is unique and this database outlives one test.
func seedAgentKey(t *testing.T, pool *pg.Pool, tenantID, projectID int64) string {
	t.Helper()
	secret := randomHex()
	full := pg.KeyScheme + secret
	hash := sha256.Sum256([]byte(full))
	if _, err := pool.Raw().Exec(t.Context(),
		`INSERT INTO api_key (tenant_id, project_id, prefix, secret_hash) VALUES ($1, $2, $3, $4)`,
		tenantID, projectID, secret[:pg.KeyPrefixLen], hash[:]); err != nil {
		t.Fatalf("seed api_key: %v", err)
	}
	return full
}

// keyCall drives one request with the key header, through ServeHTTP: routing
// is part of what these cases pin.
func keyCall(t *testing.T, h *writeAPI, method, path, key, body string) (int, string) {
	t.Helper()
	w := httptest.NewRecorder()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("X-Upcontrol-Key", key)
	h.ServeHTTP(w, r)
	return w.Code, strings.TrimSpace(w.Body.String())
}

// boardProvenance reads the stored written_by: the column IS the rule under test.
func boardProvenance(t *testing.T, pool *pg.Pool, tenantID, projectID int64) string {
	t.Helper()
	var writtenBy string
	if err := pool.Raw().QueryRow(t.Context(),
		`SELECT written_by FROM dashboard WHERE tenant_id = $1 AND project_id = $2`,
		tenantID, projectID).Scan(&writtenBy); err != nil {
		t.Fatalf("read the board's provenance: %v", err)
	}
	return writtenBy
}

func getProposal(t *testing.T, h *writeAPI, tenantID int64) (int, string) {
	t.Helper()
	w := httptest.NewRecorder()
	h.getDashboardProposal(w, httptest.NewRequest(http.MethodGet, "/v1/dashboard/proposal", nil), tenantID)
	return w.Code, strings.TrimSpace(w.Body.String())
}

func dropProposal(t *testing.T, h *writeAPI, tenantID int64) (int, string) {
	t.Helper()
	w := httptest.NewRecorder()
	h.clearDashboardProposal(w, httptest.NewRequest(http.MethodDelete, "/v1/dashboard/proposal", nil), tenantID)
	return w.Code, strings.TrimSpace(w.Body.String())
}

// A board a human saved: two widgets at known coordinates, bottom at y = 4.
const curatedBoard = `{"version":2,"widgets":[` +
	`{"id":"a_1","kind":"stat","title":"Curated stat","metrics":[],"x":0,"y":0,"w":4,"h":4},` +
	`{"id":"a_2","kind":"line","title":"Curated line","metrics":[],"x":4,"y":2,"w":4,"h":2}]}`

// The agent's offered block, deliberately reusing a_1: the collision is part
// of what the append cases pin.
const agentBlock = `{"widgets":[` +
	`{"id":"a_1","kind":"stat","title":"Agent block","metrics":[],"x":2,"y":0,"w":6,"h":2}]}`

// Provenance, not existence: the key writes the first board, replaces its own,
// and may only propose over one a session saved. The proposal is read back and
// dropped through the session's own doors.
func TestTheKeyReplacesItsOwnBoardAndProposesOntoACuratedOne(t *testing.T) {
	pool := openProjectsGateDB(t)
	tenantID := seedPlanTenant(t, pool, "Free", 1)
	h := planTenantAPI(t, pool, tenantID)
	projectID := boardOf(t, pool, tenantID)
	key := seedAgentKey(t, pool, tenantID, projectID)

	// No board yet: the key's write stores it and the row says who wrote it.
	if code, body := keyCall(t, h, http.MethodPut, "/v1/dashboard", key, oneWidgetBoard); code != http.StatusOK || !sameBoard(t, body, oneWidgetBoard) {
		t.Fatalf("the key's first write stores the board; got %d %s", code, body)
	}
	if got := boardProvenance(t, pool, tenantID, projectID); got != "key" {
		t.Fatalf("a board the key laid down is written_by='key'; got %q", got)
	}
	// The key freely replaces the board the key itself wrote.
	if code, body := keyCall(t, h, http.MethodPut, "/v1/dashboard", key, curatedBoard); code != http.StatusOK || !sameBoard(t, body, curatedBoard) {
		t.Fatalf("the key replaces its own board; got %d %s", code, body)
	}
	if got := boardProvenance(t, pool, tenantID, projectID); got != "key" {
		t.Fatalf("a board the key replaced is still the key's; got %q", got)
	}
	// A session save flips the row to 'session': a human curated this board.
	if code, body := putBoard(t, h, tenantID, oneWidgetBoard); code != http.StatusOK {
		t.Fatalf("the session save = %d %s", code, body)
	}
	if got := boardProvenance(t, pool, tenantID, projectID); got != "session" {
		t.Fatalf("a board saved from a browser is written_by='session'; got %q", got)
	}
	// The key may not replace a curated board: 202, board untouched, proposal kept.
	if code, body := keyCall(t, h, http.MethodPut, "/v1/dashboard", key, curatedBoard); code != http.StatusAccepted || !strings.Contains(body, "proposed") {
		t.Fatalf("a key writing over a curated board answers 202 proposed; got %d %s", code, body)
	}
	if code, got := getBoard(t, h, tenantID); code != http.StatusOK || !sameBoard(t, got, oneWidgetBoard) {
		t.Fatalf("the proposal must not have touched the board; got %d %s", code, got)
	}
	if got := boardProvenance(t, pool, tenantID, projectID); got != "session" {
		t.Fatalf("a proposed-to board keeps its provenance; got %q", got)
	}
	// The session reads the proposal, then drops it.
	if code, got := getProposal(t, h, tenantID); code != http.StatusOK || !sameBoard(t, got, curatedBoard) {
		t.Fatalf("GET /v1/dashboard/proposal answers the offered layout; got %d %s", code, got)
	}
	if code, body := dropProposal(t, h, tenantID); code != http.StatusNoContent {
		t.Fatalf("DELETE /v1/dashboard/proposal = %d %s", code, body)
	}
	if code, got := getProposal(t, h, tenantID); code != http.StatusNotFound || !strings.Contains(got, "no_proposal") {
		t.Fatalf("a dropped proposal reads as 404 no_proposal; got %d %s", code, got)
	}
}

// A real write resolves any pending proposal: the proposal was about the board
// that just changed.
func TestASessionSaveClearsAPendingProposal(t *testing.T) {
	pool := openProjectsGateDB(t)
	tenantID := seedPlanTenant(t, pool, "Free", 1)
	h := planTenantAPI(t, pool, tenantID)
	projectID := boardOf(t, pool, tenantID)
	key := seedAgentKey(t, pool, tenantID, projectID)

	if code, body := putBoard(t, h, tenantID, oneWidgetBoard); code != http.StatusOK {
		t.Fatalf("the curated board lands; got %d %s", code, body)
	}
	if code, body := keyCall(t, h, http.MethodPut, "/v1/dashboard", key, curatedBoard); code != http.StatusAccepted {
		t.Fatalf("the key's offer over a curated board = %d %s", code, body)
	}
	if code, body := putBoard(t, h, tenantID, curatedBoard); code != http.StatusOK {
		t.Fatalf("the session's own save = %d %s", code, body)
	}
	if code, got := getProposal(t, h, tenantID); code != http.StatusNotFound {
		t.Fatalf("the save resolved the proposal; got %d %s", code, got)
	}
	if got := boardProvenance(t, pool, tenantID, projectID); got != "session" {
		t.Fatalf("the save keeps the board curated; got %q", got)
	}
}

// Append lands the offered block whole below the board, keeps the curated
// widgets at their coordinates, re-mints a colliding id instead of refusing
// it, and never changes provenance.
func TestAppendLandsBelowTheCuratedBoardAndMintsCollidingIds(t *testing.T) {
	pool := openProjectsGateDB(t)
	tenantID := seedPlanTenant(t, pool, "Free", 1)
	h := planTenantAPI(t, pool, tenantID)
	projectID := boardOf(t, pool, tenantID)
	key := seedAgentKey(t, pool, tenantID, projectID)

	if code, body := putBoard(t, h, tenantID, curatedBoard); code != http.StatusOK {
		t.Fatalf("the curated board lands; got %d %s", code, body)
	}
	code, body := keyCall(t, h, http.MethodPost, "/v1/dashboard/widgets", key, agentBlock)
	if code != http.StatusOK {
		t.Fatalf("append onto a curated board = %d %s", code, body)
	}
	var got apigen.DashboardLayout
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("append answers a layout: %v (%s)", err, body)
	}
	byID := map[string]apigen.DashboardWidget{}
	for _, wd := range got.Widgets {
		byID[wd.Id] = wd
	}
	if len(byID) != 3 {
		t.Fatalf("the board holds the two curated widgets and the agent's, distinct; got %d: %s", len(byID), body)
	}
	if a := byID["a_1"]; a.X != 0 || a.Y != 0 || a.W != 4 || a.H != 4 {
		t.Fatalf("the curated widget a_1 sits untouched at 0,0 4x4; got %+v", a)
	}
	if a := byID["a_2"]; a.X != 4 || a.Y != 2 {
		t.Fatalf("the curated widget a_2 sits untouched at 4,2; got %+v", a)
	}
	var agent *apigen.DashboardWidget
	for i, wd := range got.Widgets {
		if wd.Title == "Agent block" {
			agent = &got.Widgets[i]
		}
	}
	if agent == nil {
		t.Fatalf("the agent's block is on the board; got %s", body)
	}
	if agent.Id == "a_1" {
		t.Fatalf("the colliding id was re-minted, never reused; got %s", body)
	}
	if agent.X != 2 || agent.W != 6 || agent.H != 2 {
		t.Fatalf("the block keeps its x, w and h; got %+v", *agent)
	}
	if agent.Y != 4 {
		t.Fatalf("the block lands at y = the board's bottom (4); got %+v", *agent)
	}
	if code, stored := getBoard(t, h, tenantID); code != http.StatusOK || !sameBoard(t, stored, body) {
		t.Fatalf("what append answered is what got stored; got %d %s", code, stored)
	}
	if wb := boardProvenance(t, pool, tenantID, projectID); wb != "session" {
		t.Fatalf("appending to a curated board keeps it curated; got %q", wb)
	}
}

// The key reads the board and nothing else: the catalog stays session-only,
// so it refuses for a caller standing on a key alone.
func TestTheKeyReadsTheBoardAndNothingElse(t *testing.T) {
	pool := openProjectsGateDB(t)
	tenantID := seedPlanTenant(t, pool, "Free", 1)
	projectID := boardOf(t, pool, tenantID)
	key := seedAgentKey(t, pool, tenantID, projectID)
	// No fixed identity and no cookie to read: every request through this
	// handler stands on its key alone, the way a deployed agent's does.
	h := &writeAPI{pool: pool, keys: pg.NewKeyResolver(pool, nil), sess: session.New(pool, session.DefaultTTL, nil)}

	if code, body := keyCall(t, h, http.MethodPut, "/v1/dashboard", key, oneWidgetBoard); code != http.StatusOK {
		t.Fatalf("the key's board lands; got %d %s", code, body)
	}
	if code, body := keyCall(t, h, http.MethodGet, "/v1/dashboard", key, ""); code != http.StatusOK || !sameBoard(t, body, oneWidgetBoard) {
		t.Fatalf("the key reads the board document back; got %d %s", code, body)
	}
	if code, body := keyCall(t, h, http.MethodGet, "/v1/dashboard/catalog", key, ""); code != http.StatusUnauthorized {
		t.Fatalf("the catalog stays session-only, key or no key; got %d %s", code, body)
	}
}

// A board still stored in the older row unit is not one the agent may append
// to: its `h` counts something else, and mixing the two units silently draws
// the agent's cards at twice the height it asked for.
func TestAppendRefusesABoardInTheOlderUnit(t *testing.T) {
	pool := openProjectsGateDB(t)
	tenantID := seedPlanTenant(t, pool, "Free", 1)
	h := planTenantAPI(t, pool, tenantID)
	projectID := boardOf(t, pool, tenantID)
	key := seedAgentKey(t, pool, tenantID, projectID)

	const oldUnitBoard = `{"version":1,"widgets":[` +
		`{"id":"a_1","kind":"stat","title":"Old unit","metrics":[],"x":0,"y":0,"w":4,"h":2}]}`
	if code, body := putBoard(t, h, tenantID, oldUnitBoard); code != http.StatusOK {
		t.Fatalf("the old-unit board lands; got %d %s", code, body)
	}
	code, body := keyCall(t, h, http.MethodPost, "/v1/dashboard/widgets", key, agentBlock)
	if code != http.StatusBadRequest || !strings.Contains(body, "older row unit") {
		t.Fatalf("the older unit is refused in words, never silently doubled; got %d %s", code, body)
	}
	if code, stored := getBoard(t, h, tenantID); code != http.StatusOK || !sameBoard(t, stored, oldUnitBoard) {
		t.Fatalf("the refused append must not have touched the board; got %d %s", code, stored)
	}
}

// An append is the agent writing, not the reader answering. It must leave a
// pending proposal alone: the agent told the reader to go and press Review, and
// a second run that quietly deleted the offer would take that away unseen.
func TestAppendLeavesAPendingProposalAlone(t *testing.T) {
	pool := openProjectsGateDB(t)
	tenantID := seedPlanTenant(t, pool, "Free", 1)
	h := planTenantAPI(t, pool, tenantID)
	projectID := boardOf(t, pool, tenantID)
	key := seedAgentKey(t, pool, tenantID, projectID)

	if code, body := putBoard(t, h, tenantID, curatedBoard); code != http.StatusOK {
		t.Fatalf("the curated board lands; got %d %s", code, body)
	}
	if code, body := keyCall(t, h, http.MethodPut, "/v1/dashboard", key, oneWidgetBoard); code != http.StatusAccepted {
		t.Fatalf("a replace onto a curated board is proposed; got %d %s", code, body)
	}
	if code, body := keyCall(t, h, http.MethodPost, "/v1/dashboard/widgets", key, agentBlock); code != http.StatusOK {
		t.Fatalf("append onto a curated board = %d %s", code, body)
	}
	if code, body := getProposal(t, h, tenantID); code != http.StatusOK {
		t.Fatalf("the append must not resolve the reader's pending offer; got %d %s", code, body)
	}
	if wb := boardProvenance(t, pool, tenantID, projectID); wb != "session" {
		t.Fatalf("appending keeps the board curated; got %q", wb)
	}
}
