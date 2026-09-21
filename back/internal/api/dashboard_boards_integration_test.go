//go:build integration

// Several named boards per project, against a real database: the list and the
// alias, the plan's cap and the two walls it words, the freeze a downgrade
// computes on read, isolation between tenants AND between sibling projects of
// one tenant, provenance per board, and the atomic save. The session doors are
// driven through ServeHTTP over the owner's fixed identity, so the dispatch is
// under test too; the key doors ride keyCall, the way a deployed agent calls
// them. Run with -tags=integration and UC_TEST_POSTGRES set.

package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	sqlc "go.upcontrol.io/back/gen/pg"
	"go.upcontrol.io/back/internal/account/session"
	"go.upcontrol.io/back/internal/storage/pg"
)

// sessionCall drives one request through ServeHTTP with no key: the session's
// fixed identity is the workspace's owner, so this is the app's own door,
// routing included — the half a handler test cannot see. An empty key header
// is no key at all (presentedKey trims), so the key door is never taken.
func sessionCall(t *testing.T, h *writeAPI, method, path, body string) (int, string) {
	t.Helper()
	return keyCall(t, h, method, path, "", body)
}

type boardEntry struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Frozen    bool   `json:"frozen"`
	WrittenBy string `json:"writtenBy"`
	Widgets   int    `json:"widgets"`
	Proposed  bool   `json:"proposed"`
}

type boardListing struct {
	Boards []boardEntry `json:"boards"`
	Max    *int         `json:"max"`
}

func listing(t *testing.T, h *writeAPI) boardListing {
	t.Helper()
	code, body := sessionCall(t, h, http.MethodGet, "/v1/dashboards", "")
	if code != http.StatusOK {
		t.Fatalf("GET /v1/dashboards = %d %s", code, body)
	}
	var out boardListing
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("the list must be a DashboardList: %v (%s)", err, body)
	}
	return out
}

// newBoard creates one through the session door and answers its id.
func newBoard(t *testing.T, h *writeAPI, name string) string {
	t.Helper()
	code, body := sessionCall(t, h, http.MethodPost, "/v1/dashboards", `{"name":"`+name+`"}`)
	if code != http.StatusCreated {
		t.Fatalf("creating %q = %d %s", name, code, body)
	}
	var out struct{ ID string }
	if err := json.Unmarshal([]byte(body), &out); err != nil || out.ID == "" {
		t.Fatalf("a create answers its new id: %v (%s)", err, body)
	}
	return out.ID
}

func setPlan(t *testing.T, pool *pg.Pool, tenantID int64, plan string) {
	t.Helper()
	if _, err := pool.Raw().Exec(t.Context(), `UPDATE tenant SET plan = $1 WHERE id = $2`, plan, tenantID); err != nil {
		t.Fatalf("move the tenant to %s: %v", plan, err)
	}
}

// bigBoard is one valid widget padded to roughly `pad` bytes: the title is the
// only unbounded field a layout carries.
func bigBoard(pad int) string {
	return `{"version":2,"widgets":[{"id":"w_1","kind":"stat","title":"` + strings.Repeat("x", pad) +
		`","metrics":[{"source":"logs"}],"x":0,"y":0,"w":4,"h":4}]}`
}

// A project that never saved a board still has one: the synthetic alias. It
// turns into a row on the first write, and the row is the same board.
func TestTheAliasIsABoardBeforeItIsARow(t *testing.T) {
	pool := openProjectsGateDB(t)
	tenantID := seedPlanTenant(t, pool, "Free", 1)
	h := planTenantAPI(t, pool, tenantID)

	list := listing(t, h)
	if len(list.Boards) != 1 || list.Boards[0].ID != mainBoard || list.Boards[0].Name != firstBoardName {
		t.Fatalf("a project with nothing stored lists the synthetic main; got %+v", list.Boards)
	}
	// Nobody has curated it, so the key may write it: that is what the column
	// says about a board with no widgets on it.
	if list.Boards[0].WrittenBy != "key" || list.Boards[0].Widgets != 0 || list.Boards[0].Frozen {
		t.Fatalf("the synthetic board is nobody's, empty and running; got %+v", list.Boards[0])
	}
	if list.Max == nil || *list.Max != 1 {
		t.Fatalf("Free carries one board per project; got %v", list.Max)
	}
	// The alias reads as the empty layout on both spellings, never a 404.
	for _, path := range []string{"/v1/dashboard", "/v1/dashboards/main"} {
		if code, body := sessionCall(t, h, http.MethodGet, path, ""); code != http.StatusOK || body != string(emptyLayout) {
			t.Fatalf("GET %s = %d %s", path, code, body)
		}
	}
	if code, body := sessionCall(t, h, http.MethodPut, "/v1/dashboards/main", oneWidgetBoard); code != http.StatusOK {
		t.Fatalf("the alias's first write = %d %s", code, body)
	}
	list = listing(t, h)
	if len(list.Boards) != 1 || list.Boards[0].ID == mainBoard || list.Boards[0].Name != firstBoardName {
		t.Fatalf("the write materialised the alias as a row; got %+v", list.Boards)
	}
	if list.Boards[0].WrittenBy != "session" || list.Boards[0].Widgets != 1 {
		t.Fatalf("a session save curates the board it writes; got %+v", list.Boards[0])
	}
	// Both spellings still answer the same document.
	for _, path := range []string{"/v1/dashboard", "/v1/dashboards/main", "/v1/dashboards/" + list.Boards[0].ID} {
		code, body := sessionCall(t, h, http.MethodGet, path, "")
		if code != http.StatusOK || !sameBoard(t, body, oneWidgetBoard) {
			t.Fatalf("GET %s = %d %s", path, code, body)
		}
	}
}

// The cap is the plan's, counted per project, and the wall names the cheapest
// plan with room for one MORE board. At the top of the ladder there is no plan
// to name, and the sentence says so instead.
func TestTheCreateWallIsThePlansCountAndNamesTheLiftingPlan(t *testing.T) {
	pool := openProjectsGateDB(t)
	tenantID := seedPlanTenant(t, pool, "Free", 1)
	h := planTenantAPI(t, pool, tenantID)

	code, body := sessionCall(t, h, http.MethodPost, "/v1/dashboards", `{"name":"Sales"}`)
	if code != http.StatusPaymentRequired {
		t.Fatalf("a second board on Free is a 402; got %d %s", code, body)
	}
	if !strings.Contains(body, `"plan":"growth"`) ||
		!strings.Contains(body, "Your plan carries 1 dashboard per project. Growth carries 3.") {
		t.Fatalf("the wall names the cheapest plan with room for one more; got %s", body)
	}

	setPlan(t, pool, tenantID, "Growth")
	first := newBoard(t, h, "Sales")
	// A name the project already holds is a conflict, not a second board and
	// not a write onto the first. Asked while there is still room, or the cap
	// would answer before the name ever did.
	if code, body := sessionCall(t, h, http.MethodPost, "/v1/dashboards", `{"name":"sales"}`); code != http.StatusConflict ||
		!strings.Contains(body, "name_taken") {
		t.Fatalf("a name taken case-insensitively is a 409; got %d %s", code, body)
	}
	newBoard(t, h, "Ops")
	if list := listing(t, h); len(list.Boards) != 3 || *list.Max != 3 {
		t.Fatalf("Growth carries three boards per project; got %+v max %v", list.Boards, list.Max)
	}
	if code, body := sessionCall(t, h, http.MethodPatch, "/v1/dashboards/"+first, `{"name":"ops"}`); code != http.StatusConflict ||
		!strings.Contains(body, "name_taken") {
		t.Fatalf("renaming onto a taken name is a 409; got %d %s", code, body)
	}
	code, body = sessionCall(t, h, http.MethodPost, "/v1/dashboards", `{"name":"Fourth"}`)
	if code != http.StatusPaymentRequired || !strings.Contains(body, `"plan":"agency"`) {
		t.Fatalf("a fourth board on Growth points at Agency; got %d %s", code, body)
	}

	setPlan(t, pool, tenantID, "Agency")
	for _, name := range []string{"Fourth", "Fifth"} {
		newBoard(t, h, name)
	}
	code, body = sessionCall(t, h, http.MethodPost, "/v1/dashboards", `{"name":"Sixth"}`)
	if code != http.StatusPaymentRequired || strings.Contains(body, `"plan"`) {
		t.Fatalf("the top of the ladder has no plan to offer; got %d %s", code, body)
	}
	if !strings.Contains(body, "Agency carries 5 dashboards per project.") {
		t.Fatalf("the top rung's wall is a sentence about the plan held; got %s", body)
	}
}

// A downgrade freezes what the plan no longer carries, in creation order, and
// nothing is deleted or written to say so. The frozen wall names a plan that
// CARRIES every board the project holds, which is a different question from
// the create wall's room for one more.
func TestBoardsFreezePastThePlanAndComeBackWhenOneGoes(t *testing.T) {
	pool := openProjectsGateDB(t)
	tenantID := seedPlanTenant(t, pool, "Growth", 1)
	h := planTenantAPI(t, pool, tenantID)

	if code, body := sessionCall(t, h, http.MethodPut, "/v1/dashboards/main", oneWidgetBoard); code != http.StatusOK {
		t.Fatalf("the first board = %d %s", code, body)
	}
	second := newBoard(t, h, "Sales")
	third := newBoard(t, h, "Ops")

	setPlan(t, pool, tenantID, "Free")
	list := listing(t, h)
	if len(list.Boards) != 3 || list.Boards[0].Frozen || !list.Boards[1].Frozen || !list.Boards[2].Frozen {
		t.Fatalf("Free runs the oldest board and keeps the rest; got %+v", list.Boards)
	}
	// Every read and every write of a frozen board is the wall, on both doors.
	for _, door := range []struct{ method, path, body string }{
		{http.MethodGet, "/v1/dashboards/" + second, ""},
		{http.MethodPut, "/v1/dashboards/" + second, oneWidgetBoard},
		{http.MethodPatch, "/v1/dashboards/" + second, `{"name":"Renamed"}`},
		{http.MethodGet, "/v1/dashboards/" + second + "/proposal", ""},
		{http.MethodPut, "/v1/dashboards", `{"boards":[{"id":"` + second + `","layout":` + oneWidgetBoard + `}]}`},
	} {
		code, body := sessionCall(t, h, door.method, door.path, door.body)
		if code != http.StatusPaymentRequired {
			t.Fatalf("%s %s on a frozen board = %d %s", door.method, door.path, code, body)
		}
		// A plan that CARRIES three boards, not one with room for a fourth.
		if !strings.Contains(body, `"plan":"growth"`) ||
			!strings.Contains(body, "This dashboard is kept, not running. Growth carries 3 per project.") {
			t.Fatalf("%s %s: the frozen wall's plan must carry every board held; got %s", door.method, door.path, body)
		}
	}
	// The same project asking for a FOURTH board asks a different question of
	// the same table, and gets a different answer.
	if _, body := sessionCall(t, h, http.MethodPost, "/v1/dashboards", `{"name":"Fourth"}`); !strings.Contains(body, `"plan":"agency"`) {
		t.Fatalf("room for one more past three is Agency; got %s", body)
	}
	// Deleting the live board thaws the oldest frozen one: the count is what
	// the freeze reads, and nothing was stored about it.
	if code, body := sessionCall(t, h, http.MethodDelete, "/v1/dashboards/main", ""); code != http.StatusNoContent {
		t.Fatalf("deleting a live board = %d %s", code, body)
	}
	list = listing(t, h)
	if len(list.Boards) != 2 || list.Boards[0].ID != second || list.Boards[0].Frozen || !list.Boards[1].Frozen {
		t.Fatalf("the oldest remaining board runs; got %+v", list.Boards)
	}
	// A plan that carries them brings them back, with nothing written.
	setPlan(t, pool, tenantID, "Growth")
	for _, b := range listing(t, h).Boards {
		if b.Frozen {
			t.Fatalf("an upgrade runs every kept board; %s is still frozen", b.Name)
		}
	}
	if code, body := sessionCall(t, h, http.MethodGet, "/v1/dashboards/"+third, ""); code != http.StatusOK {
		t.Fatalf("a thawed board reads again; got %d %s", code, body)
	}
	// And it renames: a PATCH has to REACH renameBoard through the dispatch,
	// where routes_test reads the mux textually and cannot see a shadowed arm.
	// A `not_found` here would be the request falling through to the default.
	if code, body := sessionCall(t, h, http.MethodPatch, "/v1/dashboards/"+third, `{"name":"Renamed"}`); code != http.StatusOK ||
		strings.Contains(body, `"not_found"`) {
		t.Fatalf("PATCH on a thawed board = %d %s", code, body)
	}
	// A project keeps at least one board, whichever one is left.
	if code, body := sessionCall(t, h, http.MethodDelete, "/v1/dashboards/"+third, ""); code != http.StatusNoContent {
		t.Fatalf("deleting the third board = %d %s", code, body)
	}
	if code, body := sessionCall(t, h, http.MethodDelete, "/v1/dashboards/"+second, ""); code != http.StatusConflict ||
		!strings.Contains(body, "last_board") {
		t.Fatalf("the last board stays; got %d %s", code, body)
	}
}

// A board id resolves inside the caller's tenant AND project or it resolves to
// nothing. A sibling project of the same workspace is as foreign as a
// stranger's: the session stands in one project, and that is the only place
// its ids mean anything.
func TestABoardOutsideTheCallersProjectIsUnknownOnEveryVerb(t *testing.T) {
	pool := openProjectsGateDB(t)
	tenantID := seedPlanTenant(t, pool, "Growth", 2)
	other := seedPlanTenant(t, pool, "Growth", 1)
	h := planTenantAPI(t, pool, tenantID)
	hOther := planTenantAPI(t, pool, other)

	// The caller's own project needs two boards, or `last_board` would answer
	// the delete before the lookup ever did.
	if code, body := sessionCall(t, h, http.MethodPut, "/v1/dashboards/main", oneWidgetBoard); code != http.StatusOK {
		t.Fatalf("the caller's own board = %d %s", code, body)
	}
	newBoard(t, h, "Second")

	// A board in the workspace's OTHER project, laid down directly: no door
	// reaches it, which is the point.
	var sibling string
	if err := pool.Raw().QueryRow(t.Context(),
		`INSERT INTO dashboard (tenant_id, project_id, name, layout)
		 SELECT $1, max(id), 'Sibling', $2::jsonb FROM project WHERE tenant_id = $1
		 RETURNING replace(public_id::text, '-', '')`,
		tenantID, curatedBoard).Scan(&sibling); err != nil {
		t.Fatalf("seed the sibling project's board: %v", err)
	}
	// And one in another workspace entirely.
	if code, body := sessionCall(t, hOther, http.MethodPut, "/v1/dashboards/main", curatedBoard); code != http.StatusOK {
		t.Fatalf("the other workspace's board = %d %s", code, body)
	}
	stranger := listing(t, hOther).Boards[0].ID

	for _, id := range []string{sibling, stranger, "deadbeefdeadbeefdeadbeefdeadbeef", "not-an-id"} {
		for _, door := range []struct{ method, path, body string }{
			{http.MethodGet, "/v1/dashboards/" + id, ""},
			{http.MethodPut, "/v1/dashboards/" + id, oneWidgetBoard},
			{http.MethodPatch, "/v1/dashboards/" + id, `{"name":"Taken over"}`},
			{http.MethodDelete, "/v1/dashboards/" + id, ""},
			{http.MethodGet, "/v1/dashboards/" + id + "/proposal", ""},
			{http.MethodDelete, "/v1/dashboards/" + id + "/proposal", ""},
			{http.MethodPut, "/v1/dashboards", `{"boards":[{"id":"` + id + `","layout":` + oneWidgetBoard + `}]}`},
		} {
			code, body := sessionCall(t, h, door.method, door.path, door.body)
			if code != http.StatusNotFound || !strings.Contains(body, "unknown_board") {
				t.Fatalf("%s %s = %d %s, want 404 unknown_board", door.method, door.path, code, body)
			}
		}
	}
	// Nothing was written on the way: a write that matched no row must not
	// have matched a row somewhere else either.
	var layout string
	if err := pool.Raw().QueryRow(t.Context(),
		`SELECT layout::text FROM dashboard WHERE replace(public_id::text, '-', '') = $1`, sibling).Scan(&layout); err != nil {
		t.Fatalf("read the sibling board back: %v", err)
	}
	if !sameBoard(t, layout, curatedBoard) {
		t.Fatalf("the sibling project's board must be untouched; got %s", layout)
	}
}

// Provenance is per board, and an EMPTY board is nobody's: the agent asked to
// fill a board a person just named writes it, rather than parking a proposal
// on a board with nothing on it. A board the session actually saved takes the
// key's replacement as a proposal, per board.
func TestProvenanceIsPerBoardAndAnEmptyBoardIsNobodys(t *testing.T) {
	pool := openProjectsGateDB(t)
	tenantID := seedPlanTenant(t, pool, "Growth", 1)
	h := planTenantAPI(t, pool, tenantID)
	projectID := boardOf(t, pool, tenantID)
	key := seedAgentKey(t, pool, tenantID, projectID)

	// The person names a board and leaves it empty. Creating it materialises
	// the alias beside it, so the project now holds two.
	empty := newBoard(t, h, "For the agent")
	if code, body := keyCall(t, h, http.MethodPut, "/v1/dashboards/"+empty, key, curatedBoard); code != http.StatusOK {
		t.Fatalf("a board nobody curated is the key's to write; got %d %s", code, body)
	}
	// The person saves the board they were given.
	curated := listing(t, h).Boards[0].ID
	if code, body := sessionCall(t, h, http.MethodPut, "/v1/dashboards/"+curated, oneWidgetBoard); code != http.StatusOK {
		t.Fatalf("the session's save = %d %s", code, body)
	}
	code, body := keyCall(t, h, http.MethodPut, "/v1/dashboards/"+curated, key, curatedBoard)
	if code != http.StatusAccepted || !strings.Contains(body, `"status":"proposed"`) {
		t.Fatalf("a curated board takes the key's layout as a proposal; got %d %s", code, body)
	}
	// The 202 names the board, so a caller that addressed it by alias or by
	// name can point a human at the right tab.
	if !strings.Contains(body, `"id":"`+curated+`"`) || !strings.Contains(body, `"name":"`+firstBoardName+`"`) {
		t.Fatalf("the 202 carries the board it waits on; got %s", body)
	}
	if code, got := sessionCall(t, h, http.MethodGet, "/v1/dashboards/"+curated, ""); code != http.StatusOK || !sameBoard(t, got, oneWidgetBoard) {
		t.Fatalf("the proposal must not have touched the board; got %d %s", code, got)
	}
	// The proposal is the board's own: its neighbour holds none.
	if code, body := sessionCall(t, h, http.MethodGet, "/v1/dashboards/"+empty+"/proposal", ""); code != http.StatusNotFound ||
		!strings.Contains(body, "no_proposal") {
		t.Fatalf("a proposal belongs to one board; got %d %s", code, body)
	}
	if code, got := sessionCall(t, h, http.MethodGet, "/v1/dashboards/"+curated+"/proposal", ""); code != http.StatusOK ||
		!sameBoard(t, got, curatedBoard) {
		t.Fatalf("the offered layout reads back; got %d %s", code, got)
	}
	for _, b := range listing(t, h).Boards {
		if (b.ID == curated) != b.Proposed {
			t.Fatalf("only the board with an offer on it says so; got %+v", b)
		}
	}
	if code, body := sessionCall(t, h, http.MethodDelete, "/v1/dashboards/"+curated+"/proposal", ""); code != http.StatusNoContent {
		t.Fatalf("dropping the offer = %d %s", code, body)
	}
	if code, body := sessionCall(t, h, http.MethodGet, "/v1/dashboards/"+curated+"/proposal", ""); code != http.StatusNotFound {
		t.Fatalf("a dropped proposal is gone; got %d %s", code, body)
	}
	// The key creates freely up to the cap, and is held to the same one.
	code, body = keyCall(t, h, http.MethodPost, "/v1/dashboards", key, `{"name":"Agent board"}`)
	if code != http.StatusCreated {
		t.Fatalf("the key creates a board; got %d %s", code, body)
	}
	if code, body := keyCall(t, h, http.MethodPost, "/v1/dashboards", key, `{"name":"One too many"}`); code != http.StatusPaymentRequired {
		t.Fatalf("the key is held to the same cap; got %d %s", code, body)
	}
	// It may write boards; it may not name one, delete one or read what it
	// offered. Those doors are the session's, and a caller standing on a key
	// alone is refused there — this handler has no fixed identity, the way a
	// deployed agent has no cookie.
	keyOnly := &writeAPI{pool: pool, keys: pg.NewKeyResolver(pool, nil), sess: session.New(pool, session.DefaultTTL, nil)}
	for _, door := range []struct{ method, path string }{
		{http.MethodPatch, "/v1/dashboards/" + curated},
		{http.MethodDelete, "/v1/dashboards/" + curated},
		{http.MethodPut, "/v1/dashboards"},
		{http.MethodGet, "/v1/dashboards/" + curated + "/proposal"},
		{http.MethodDelete, "/v1/dashboards/" + curated + "/proposal"},
	} {
		if code, body := keyCall(t, keyOnly, door.method, door.path, key, `{"name":"Agent's"}`); code != http.StatusUnauthorized {
			t.Fatalf("%s %s with a key alone = %d %s", door.method, door.path, code, body)
		}
	}
}

// /v1/dashboard and its two sub-paths keep answering the OLDEST board, whatever
// else the project holds: a CLI already on somebody's machine knows no other
// path, and the board it has been writing must stay the board it writes.
func TestTheLegacyAliasKeepsAnsweringTheOldestBoard(t *testing.T) {
	pool := openProjectsGateDB(t)
	tenantID := seedPlanTenant(t, pool, "Growth", 1)
	h := planTenantAPI(t, pool, tenantID)
	projectID := boardOf(t, pool, tenantID)
	key := seedAgentKey(t, pool, tenantID, projectID)

	// Version 2 on both: the append below is refused on a board still stored in
	// the older row unit, which is a rule of its own and not this test's.
	const salesBoard = `{"version":2,"widgets":[{"id":"s_1","kind":"stat","title":"Sales","metrics":[{"source":"logs"}],"x":0,"y":0,"w":4,"h":4}]}`
	if code, body := sessionCall(t, h, http.MethodPut, "/v1/dashboard", curatedBoard); code != http.StatusOK {
		t.Fatalf("the legacy save = %d %s", code, body)
	}
	second := newBoard(t, h, "Sales")
	if code, body := sessionCall(t, h, http.MethodPut, "/v1/dashboards/"+second, salesBoard); code != http.StatusOK {
		t.Fatalf("the second board's save = %d %s", code, body)
	}
	for _, read := range []func() (int, string){
		func() (int, string) { return getBoard(t, h) },
		func() (int, string) { return keyCall(t, h, http.MethodGet, "/v1/dashboard", key, "") },
	} {
		if code, body := read(); code != http.StatusOK || !sameBoard(t, body, curatedBoard) {
			t.Fatalf("the alias answers the oldest board; got %d %s", code, body)
		}
	}
	// The key's append on the legacy path lands on the alias, not on the
	// newest board.
	if code, body := keyCall(t, h, http.MethodPost, "/v1/dashboard/widgets", key, agentBlock); code != http.StatusOK {
		t.Fatalf("the legacy append = %d %s", code, body)
	}
	if code, body := sessionCall(t, h, http.MethodGet, "/v1/dashboards/"+second, ""); code != http.StatusOK ||
		!sameBoard(t, body, salesBoard) {
		t.Fatalf("the append must not have touched the other board; got %d %s", code, body)
	}
	// A board NAMED main never outranks the alias: the alias is the oldest
	// board, and the CLI's one handle cannot be taken by a rename. Names are
	// unique whatever their case, so the name is free only once the first
	// board has given it up.
	if code, body := sessionCall(t, h, http.MethodPatch, "/v1/dashboards/main", `{"name":"Overview"}`); code != http.StatusOK {
		t.Fatalf("renaming the first board = %d %s", code, body)
	}
	renamed := newBoard(t, h, "main")
	if code, body := sessionCall(t, h, http.MethodPut, "/v1/dashboards/"+renamed, `{"version":2,"widgets":[]}`); code != http.StatusOK {
		t.Fatalf("the board called main saves like any other; got %d %s", code, body)
	}
	if code, body := sessionCall(t, h, http.MethodGet, "/v1/dashboards/main", ""); code != http.StatusOK || sameBoard(t, body, string(emptyLayout)) {
		t.Fatalf("the alias still names the oldest board; got %d %s", code, body)
	}
}

// One Save, one transaction: a widget moved between two boards is two
// documents, and saving one without the other would duplicate it or lose it.
func TestTheAtomicSaveWritesEveryBoardOrNone(t *testing.T) {
	pool := openProjectsGateDB(t)
	tenantID := seedPlanTenant(t, pool, "Growth", 1)
	h := planTenantAPI(t, pool, tenantID)

	if code, body := sessionCall(t, h, http.MethodPut, "/v1/dashboards/main", oneWidgetBoard); code != http.StatusOK {
		t.Fatalf("the first board = %d %s", code, body)
	}
	first := listing(t, h).Boards[0].ID
	second := newBoard(t, h, "Sales")

	// One board past the grid refuses the whole request, and the message names
	// the board it is about.
	bad := `{"version":2,"widgets":[{"id":"w_1","kind":"line","title":"x","metrics":[{"source":"logs"}],"x":7,"y":0,"w":6,"h":4}]}`
	code, body := sessionCall(t, h, http.MethodPut, "/v1/dashboards",
		`{"boards":[{"id":"`+first+`","layout":`+curatedBoard+`},{"id":"`+second+`","layout":`+bad+`}]}`)
	if code != http.StatusBadRequest || !strings.Contains(body, "bad_layout") || !strings.Contains(body, second) {
		t.Fatalf("a refused board names itself; got %d %s", code, body)
	}
	if code, got := sessionCall(t, h, http.MethodGet, "/v1/dashboards/"+first, ""); code != http.StatusOK || !sameBoard(t, got, oneWidgetBoard) {
		t.Fatalf("the board saved before the refusal must roll back; got %d %s", code, got)
	}

	code, body = sessionCall(t, h, http.MethodPut, "/v1/dashboards",
		`{"boards":[{"id":"`+first+`","layout":`+curatedBoard+`},{"id":"`+second+`","layout":`+oneWidgetBoard+`}]}`)
	if code != http.StatusOK {
		t.Fatalf("both boards save in one request; got %d %s", code, body)
	}
	if code, got := sessionCall(t, h, http.MethodGet, "/v1/dashboards/"+first, ""); code != http.StatusOK || !sameBoard(t, got, curatedBoard) {
		t.Fatalf("the first board took the save; got %d %s", code, got)
	}
	if code, got := sessionCall(t, h, http.MethodGet, "/v1/dashboards/"+second, ""); code != http.StatusOK || !sameBoard(t, got, oneWidgetBoard) {
		t.Fatalf("the second board took the save; got %d %s", code, got)
	}

	// Each board is held to the same 64 KB a single save is, and two of them
	// near that ceiling still ride one request.
	code, body = sessionCall(t, h, http.MethodPut, "/v1/dashboards",
		`{"boards":[{"id":"`+first+`","layout":`+bigBoard(60000)+`},{"id":"`+second+`","layout":`+bigBoard(60000)+`}]}`)
	if code != http.StatusOK {
		t.Fatalf("two boards near the per-board cap save together; got %d %s", code, body)
	}
	if code, body := sessionCall(t, h, http.MethodPut, "/v1/dashboards",
		`{"boards":[{"id":"`+first+`","layout":`+bigBoard(dashboardMaxBody)+`}]}`); code != http.StatusBadRequest ||
		!strings.Contains(body, "bad_layout") {
		t.Fatalf("a board past the cap is refused by name; got %d %s", code, body)
	}

	// Seventeen boards is not a save, whatever they hold.
	many := make([]string, 0, maxSaveBoards+1)
	for i := 0; i <= maxSaveBoards; i++ {
		many = append(many, `{"id":"`+first+`","layout":`+oneWidgetBoard+`}`)
	}
	if code, body := sessionCall(t, h, http.MethodPut, "/v1/dashboards",
		`{"boards":[`+strings.Join(many, ",")+`]}`); code != http.StatusBadRequest {
		t.Fatalf("%d boards in one save is a 400; got %d %s", len(many), code, body)
	}
}

// Creating a second board first lays the alias down, in the CURRENT unit: a
// Main the server itself made must be one the agent can append to.
func TestCreatingASecondBoardMaterialisesAnAppendableMain(t *testing.T) {
	pool := openProjectsGateDB(t)
	tenantID := seedPlanTenant(t, pool, "Growth", 1)
	h := planTenantAPI(t, pool, tenantID)
	projectID := boardOf(t, pool, tenantID)
	key := seedAgentKey(t, pool, tenantID, projectID)

	newBoard(t, h, "Sales")
	list := listing(t, h)
	if len(list.Boards) != 2 || list.Boards[0].Name != firstBoardName {
		t.Fatalf("the alias is laid down first, so it stays the oldest; got %+v", list.Boards)
	}
	if code, body := keyCall(t, h, http.MethodPost, "/v1/dashboard/widgets", key, agentBlock); code != http.StatusOK {
		t.Fatalf("the materialised Main takes an append; got %d %s", code, body)
	}
	if code, body := keyCall(t, h, http.MethodPost, "/v1/dashboards/"+list.Boards[1].ID+"/widgets", key, agentBlock); code != http.StatusOK {
		t.Fatalf("so does the board beside it; got %d %s", code, body)
	}
}

// Every door decides to lay the alias down from a read that found the project
// empty, and a person's first Save may land in between. That Main is kept whole
// and handed back; only a row an EARLIER tenant left behind is taken over
// (TestDashboardFollowsTheProject).
func TestLayingTheAliasKeepsAMainThisWorkspaceAlreadySaved(t *testing.T) {
	pool := openProjectsGateDB(t)
	tenantID := seedPlanTenant(t, pool, "Growth", 1)
	h := planTenantAPI(t, pool, tenantID)

	if code, body := sessionCall(t, h, http.MethodPut, "/v1/dashboards",
		`{"boards":[{"id":"main","layout":`+curatedBoard+`}]}`); code != http.StatusOK {
		t.Fatalf("the person's first Save = %d %s", code, body)
	}
	// What createBoard, a key PUT and an append do after their stale read.
	d := boardDoor{tenantID: tenantID, projectID: boardOf(t, pool, tenantID)}
	b, laid, err := layAlias(t.Context(), pool.Queries(), d, emptyLayout, "key")
	if err != nil || laid {
		t.Fatalf("a Main this workspace holds is not laid again; laid %v, err %v", laid, err)
	}
	if b.writtenBy != "session" || !sameBoard(t, string(b.layout), curatedBoard) {
		t.Fatalf("the stored Main is handed back; got %s written by %s", b.layout, b.writtenBy)
	}
	if code, got := getBoard(t, h); code != http.StatusOK || !sameBoard(t, got, curatedBoard) {
		t.Fatalf("the saved Main must be untouched; got %d %s", code, got)
	}
}

// A rename submitted twice on the alias of a project with nothing stored lands
// once, and both answers say so: a 500 beside a tab already carrying the name
// would report a failure that did not happen.
func TestARenameSubmittedTwiceLandsOnce(t *testing.T) {
	pool := openProjectsGateDB(t)
	for i := range 10 {
		tenantID := seedPlanTenant(t, pool, "Growth", 1)
		h := planTenantAPI(t, pool, tenantID)
		var codes [2]int
		var bodies [2]string
		var wg sync.WaitGroup
		for j := range 2 {
			wg.Go(func() {
				codes[j], bodies[j] = sessionCall(t, h, http.MethodPatch, "/v1/dashboards/main", `{"name":"Ops"}`)
			})
		}
		wg.Wait()
		if codes != [2]int{http.StatusOK, http.StatusOK} {
			t.Fatalf("round %d: both renames answer 200; got %v %v", i, codes, bodies)
		}
		if list := listing(t, h); len(list.Boards) != 1 || list.Boards[0].Name != "Ops" {
			t.Fatalf("round %d: one board, named once; got %+v", i, list.Boards)
		}
	}
}

// A board door stands in the project of the session the role gate read, never
// in one a second read finds: a project switch landing between the two would
// put a write where the caller only reads.
func TestTheBoardDoorStandsWhereTheGatesSessionDoes(t *testing.T) {
	pool := openProjectsGateDB(t)
	tenantID := seedPlanTenant(t, pool, "Growth", 2)
	// The handler's own read of the session resolves the FIRST project...
	h := planTenantAPI(t, pool, tenantID)
	var owner, second int64
	if err := pool.Raw().QueryRow(t.Context(),
		`SELECT t.owner_person_id, max(p.id) FROM tenant t JOIN project p ON p.tenant_id = t.id
		  WHERE t.id = $1 GROUP BY t.owner_person_id`, tenantID).Scan(&owner, &second); err != nil {
		t.Fatalf("read the owner and the second project: %v", err)
	}
	// ...and the gate's copy stands in the second.
	s := sqlc.Session{PersonID: owner, TenantID: tenantID, ProjectID: &second}
	d := h.sessionBoards(httptest.NewRequest(http.MethodGet, "/v1/dashboards", nil), s)
	if d.tenantID != tenantID || d.projectID != second || !d.session {
		t.Fatalf("the door must stand where the gate's session does; got %+v, want project %d", d, second)
	}
}

// A reader who reaches no project still reads a board — the front draws an
// empty one rather than an error — but there is nowhere for a write to land.
func TestBoardsWithoutAProject(t *testing.T) {
	pool := openProjectsGateDB(t)
	tenantID := seedPlanTenant(t, pool, "Free", 0)
	h := planTenantAPI(t, pool, tenantID)

	if list := listing(t, h); len(list.Boards) != 1 || list.Boards[0].ID != mainBoard {
		t.Fatalf("the synthetic board is what a project-less reader lists; got %+v", list.Boards)
	}
	if code, body := sessionCall(t, h, http.MethodGet, "/v1/dashboards/main", ""); code != http.StatusOK || body != string(emptyLayout) {
		t.Fatalf("the alias still reads empty; got %d %s", code, body)
	}
	for _, door := range []struct{ method, path, body string }{
		{http.MethodPost, "/v1/dashboards", `{"name":"Sales"}`},
		{http.MethodPut, "/v1/dashboards", `{"boards":[{"id":"main","layout":` + oneWidgetBoard + `}]}`},
		{http.MethodPut, "/v1/dashboards/main", oneWidgetBoard},
		{http.MethodPut, "/v1/dashboard", oneWidgetBoard},
	} {
		if code, body := sessionCall(t, h, door.method, door.path, door.body); code != http.StatusNotFound {
			t.Fatalf("%s %s with no project = %d %s", door.method, door.path, code, body)
		}
	}
}

// The cap is a paid gate that counts rows, so it locks the row it counts
// against: creates arriving together on a project one board short of the
// plan's count land exactly one board, and every other one is the wall.
func TestCreatesArrivingTogetherLandOneBoardPastTheLast(t *testing.T) {
	pool := openProjectsGateDB(t)
	tenantID := seedPlanTenant(t, pool, "Growth", 1)
	h := planTenantAPI(t, pool, tenantID)
	newBoard(t, h, "Second") // Main is laid down under it: two of Growth's three.

	const racers = 8
	codes := make([]int, racers)
	var wg sync.WaitGroup
	for i := range racers {
		wg.Go(func() {
			codes[i], _ = sessionCall(t, h, http.MethodPost, "/v1/dashboards", `{"name":"Racer `+string(rune('A'+i))+`"}`)
		})
	}
	wg.Wait()
	created := 0
	for _, code := range codes {
		switch code {
		case http.StatusCreated:
			created++
		case http.StatusPaymentRequired:
		default:
			t.Fatalf("a racing create answers 201 or the wall; got %v", codes)
		}
	}
	if created != 1 {
		t.Fatalf("exactly one create may take the last board; got %d of %v", created, codes)
	}
	if got := len(listing(t, h).Boards); got != 3 {
		t.Fatalf("the project holds the plan's three boards, no more; got %d", got)
	}
}
