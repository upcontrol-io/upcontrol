//go:build integration

// The freeze wall: a frozen project is a snapshot. The list still carries the
// row (members must see what they lost access to, not have it vanish), the
// switch is the one door into it and answers 402, a frozen project's monitors
// accept no edits or deletes, and no by-id door reaches into it from the
// live project.
// Run with -tags=integration, UC_TEST_POSTGRES set.

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"go.upcontrol.io/back/internal/account/session"
	"go.upcontrol.io/back/internal/incident"
	"go.upcontrol.io/back/internal/storage/pg"
	"go.upcontrol.io/back/internal/storage/pgstore"
)

// freezeFixture: a Free tenant holding two projects, the second frozen by
// hand the way the sweep would leave it.
func newFreezeFixture(t *testing.T) *freezeF {
	t.Helper()
	ctx := context.Background()
	pool := openProjectsGateDB(t)
	uniq := time.Now().UnixNano()

	var personID int64
	if err := pool.Raw().QueryRow(ctx,
		`INSERT INTO person (public_id, email, name) VALUES (gen_random_uuid(), $1, 'Freeze') RETURNING id`,
		fmt.Sprintf("freeze-%d@example.com", uniq)).Scan(&personID); err != nil {
		t.Fatalf("seed person: %v", err)
	}
	tenantID := seedOwnedTenant(t, pool, personID, fmt.Sprintf("freeze-%d", uniq))
	live := seedProject(t, pool, tenantID, fmt.Sprintf("live-%d.example.com", uniq%100000))
	frozen := seedProject(t, pool, tenantID, fmt.Sprintf("frozen-%d.example.com", uniq%100000))
	if _, err := pool.Raw().Exec(ctx,
		`UPDATE project SET frozen_at = now() WHERE id = $1`, frozen); err != nil {
		t.Fatalf("freeze project: %v", err)
	}

	sm := session.New(pool, session.DefaultTTL, nil)
	token, err := sm.Create(ctx, personID, tenantID, &live)
	if err != nil {
		t.Fatalf("mint session: %v", err)
	}

	wa := NewWriteAPI(pool, nil, sm, false, nil, nil, false, "")
	mux := http.NewServeMux()
	mux.Handle("GET /v1/projects", wa)
	mux.Handle("POST /v1/project/switch", wa)
	mux.Handle("DELETE /v1/project", wa)
	mux.Handle("PATCH /v1/sources/{id}", wa)
	mux.Handle("DELETE /v1/sources/{id}", wa)
	mux.Handle("POST /v1/channels/{id}/test", wa)
	mux.Handle("GET /v1/status-page", wa)
	mux.Handle("GET /internal/domain-allowed", wa)
	rd := NewReadAPI(pool, nil, sm, nil)
	mux.Handle("GET /v1/channels", rd)
	mux.Handle("GET /v1/plan", rd)
	mon := NewMonitors(pool, pgstore.New(pool.Raw()), sm, "http://test")
	mux.Handle("POST /v1/monitors", mon)
	mux.Handle("PATCH /v1/monitors/{id}", mon)
	mux.Handle("DELETE /v1/monitors/{id}", mon)
	mux.Handle("POST /v1/projects/{id}/keys/revoke", NewKeys(pool, sm))

	f := &freezeF{t: t, pool: pool, tenantID: tenantID, live: live, frozen: frozen,
		cookie: &http.Cookie{Name: session.CookieName, Value: token}, route: mux, wa: wa}
	return f
}

type freezeF struct {
	t        *testing.T
	pool     *pg.Pool
	tenantID int64
	live     int64
	frozen   int64
	cookie   *http.Cookie
	route    http.Handler
	wa       *writeAPI
}

func (f *freezeF) do(method, path, body string) *httptest.ResponseRecorder {
	f.t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
	}
	r.AddCookie(f.cookie)
	w := httptest.NewRecorder()
	f.route.ServeHTTP(w, r)
	return w
}

// seedMonitor builds the subscription pair the create path would build in
// one project and answers the monitor's wire id and row id. The key carries
// a nanosecond uniq: probe_target.key is globally unique and the shared test
// database survives runs.
func (f *freezeF) seedMonitor(projectID int64) (string, int64) {
	f.t.Helper()
	ctx := context.Background()
	var monID int64
	var pubID pgtype.UUID
	if err := f.pool.Raw().QueryRow(ctx,
		`WITH tgt AS (
		   INSERT INTO probe_target (key, kind, url) VALUES ($3, 'website', $3) RETURNING id)
		 INSERT INTO monitor (public_id, tenant_id, project_id, target_id, kind, name, target, interval_sec)
		 SELECT gen_random_uuid(), $1, $2, tgt.id, 'http', 'Seeded check', $3, 60
		   FROM tgt RETURNING id, public_id`,
		f.tenantID, projectID, fmt.Sprintf("https://seed-%d.example.com", time.Now().UnixNano())).Scan(&monID, &pubID); err != nil {
		f.t.Fatalf("seed monitor: %v", err)
	}
	return uuidStr(pubID), monID
}

// projectWireID is a project's id as GET /v1/projects lists it.
func (f *freezeF) projectWireID(projectID int64) string {
	f.t.Helper()
	var pubID pgtype.UUID
	if err := f.pool.Raw().QueryRow(context.Background(),
		`SELECT public_id FROM project WHERE id = $1`, projectID).Scan(&pubID); err != nil {
		f.t.Fatalf("read project: %v", err)
	}
	return uuidStr(pubID)
}

// The list carries the frozen row with frozen=true: a member's project must
// not vanish from their list — that reads as deleted data.
func TestFrozenProjectListedWithFlag(t *testing.T) {
	f := newFreezeFixture(t)
	w := f.do(http.MethodGet, "/v1/projects", "")
	if w.Code != http.StatusOK {
		t.Fatalf("list = %d (%s)", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "\"frozen\":true") {
		t.Fatalf("list body carries no frozen:true row: %s", w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "\"frozen\":false") {
		t.Fatalf("list body carries no live row: %s", w.Body.String())
	}
}

// The switch into a frozen project is the wall: 402 with the reason and the
// plan that reactivates it, and the session keeps its live pick. Two projects
// on Free: Indie carries both, so the hint is Indie, never Growth's room for
// a third.
func TestSwitchIntoFrozenProjectAnswersTheWall(t *testing.T) {
	f := newFreezeFixture(t)
	w := f.do(http.MethodPost, "/v1/project/switch", `{"id":"`+f.projectWireID(f.frozen)+`"}`)
	if w.Code != http.StatusPaymentRequired {
		t.Fatalf("switch into frozen = %d (%s), want 402", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "frozen") || !strings.Contains(w.Body.String(), `"plan":"indie"`) {
		t.Fatalf("402 body must name the freeze and Indie as the lift: %s", w.Body.String())
	}
	var pick int64
	if err := f.pool.Raw().QueryRow(context.Background(),
		`SELECT project_id FROM session WHERE tenant_id = $1`, f.tenantID).Scan(&pick); err != nil || pick != f.live {
		t.Fatalf("session pick = %d (err %v), want the live project %d", pick, err, f.live)
	}
}

// A guest switching into the owner's frozen project gets the reason alone: a
// plan they bought would be their own workspace's and thaw nothing here.
func TestGuestSwitchIntoFrozenProjectSellsNothing(t *testing.T) {
	f := newFreezeFixture(t)
	ctx := context.Background()
	guest := seedPerson(t, f.pool, fmt.Sprintf("freeze-guest-%d@example.com", time.Now().UnixNano()))
	seedProjectMember(t, f.pool, f.live, guest, f.tenantID, "login")
	seedProjectMember(t, f.pool, f.frozen, guest, f.tenantID, "login")
	token, err := session.New(f.pool, session.DefaultTTL, nil).Create(ctx, guest, f.tenantID, &f.live)
	if err != nil {
		t.Fatalf("mint guest session: %v", err)
	}
	f.cookie = &http.Cookie{Name: session.CookieName, Value: token}
	w := f.do(http.MethodPost, "/v1/project/switch", `{"id":"`+f.projectWireID(f.frozen)+`"}`)
	if w.Code != http.StatusPaymentRequired || strings.Contains(w.Body.String(), `"plan"`) {
		t.Fatalf("guest switch into frozen = %d (%s), want 402 with no plan", w.Code, w.Body.String())
	}
}

// A frozen project's monitor is a snapshot: PATCH and DELETE both answer the
// same wall instead of changing what an upgrade restores.
func TestFrozenProjectMonitorIsReadOnly(t *testing.T) {
	f := newFreezeFixture(t)
	ctx := context.Background()
	id, monID := f.seedMonitor(f.frozen)

	w := f.do(http.MethodPatch, "/v1/monitors/"+id, `{"name":"renamed"}`)
	if w.Code != http.StatusPaymentRequired || !strings.Contains(w.Body.String(), `"plan":"indie"`) {
		t.Fatalf("patch frozen monitor = %d (%s), want 402 naming Indie", w.Code, w.Body.String())
	}
	w = f.do(http.MethodDelete, "/v1/monitors/"+id, "")
	if w.Code != http.StatusPaymentRequired {
		t.Fatalf("delete frozen monitor = %d (%s), want 402", w.Code, w.Body.String())
	}
	var n int
	if err := f.pool.Raw().QueryRow(ctx,
		`SELECT count(*) FROM monitor WHERE id = $1`, monID).Scan(&n); err != nil || n != 1 {
		t.Fatalf("frozen monitor must survive the refused delete: count=%d err=%v", n, err)
	}
}

// Every project frozen resolves the owner to project 0. DELETE /v1/project
// there has nothing current to delete, and closing the account from 0 would
// cascade every snapshot: 409, and nothing changes.
func TestDeleteProjectAtZeroRefuses(t *testing.T) {
	f := newFreezeFixture(t)
	ctx := context.Background()
	if _, err := f.pool.Raw().Exec(ctx, `UPDATE project SET frozen_at = now() WHERE id = $1`, f.live); err != nil {
		t.Fatalf("freeze the live project: %v", err)
	}
	w := f.do(http.MethodDelete, "/v1/project", "")
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "no_current_project") {
		t.Fatalf("delete at zero = %d (%s), want 409 no_current_project", w.Code, w.Body.String())
	}
	var n int
	if err := f.pool.Raw().QueryRow(ctx,
		`SELECT count(*) FROM project WHERE tenant_id = $1`, f.tenantID).Scan(&n); err != nil || n != 2 {
		t.Fatalf("projects after the refused delete = %d (err %v), want 2", n, err)
	}
}

// Deleting the live project hands its slot to the snapshot in the same
// request: the owner never lands on zero live projects.
func TestDeleteLiveProjectPromotesTheSnapshot(t *testing.T) {
	f := newFreezeFixture(t)
	w := f.do(http.MethodDelete, "/v1/project", "")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"accountDeleted":false`) {
		t.Fatalf("delete live = %d (%s), want 200 accountDeleted:false", w.Code, w.Body.String())
	}
	var frozen bool
	if err := f.pool.Raw().QueryRow(context.Background(),
		`SELECT frozen_at IS NOT NULL FROM project WHERE id = $1`, f.frozen).Scan(&frozen); err != nil || frozen {
		t.Fatalf("snapshot still frozen = %v (err %v) after the live project left", frozen, err)
	}
}

// A source is addressed by a sequential id: a sibling project's hook, here a
// frozen one's, answers 404 to both writes and stays as it was.
func TestSourcesByIDStayInTheCurrentProject(t *testing.T) {
	f := newFreezeFixture(t)
	ctx := context.Background()
	var id int64
	if err := f.pool.Raw().QueryRow(ctx,
		`INSERT INTO source_connection (tenant_id, project_id, kind, status, hook_token)
		 VALUES ($1, $2, 'stripe', 'waiting', $3) RETURNING id`,
		f.tenantID, f.frozen, fmt.Sprintf("hook-%d", time.Now().UnixNano())).Scan(&id); err != nil {
		t.Fatalf("seed source: %v", err)
	}
	path := fmt.Sprintf("/v1/sources/src_%d", id)
	if w := f.do(http.MethodPatch, path, `{"paused":true}`); w.Code != http.StatusNotFound {
		t.Fatalf("patch a sibling's source = %d (%s), want 404", w.Code, w.Body.String())
	}
	if w := f.do(http.MethodDelete, path, ""); w.Code != http.StatusNotFound {
		t.Fatalf("delete a sibling's source = %d (%s), want 404", w.Code, w.Body.String())
	}
	var paused bool
	if err := f.pool.Raw().QueryRow(ctx,
		`SELECT paused FROM source_connection WHERE id = $1`, id).Scan(&paused); err != nil || paused {
		t.Fatalf("sibling's source paused=%v err=%v, want untouched", paused, err)
	}
}

// A check the plan paused resumes with the plan, never with a PATCH: 402 and
// the row keeps the plan's marker. paused:true on it still goes through and
// becomes the owner's own pause.
func TestPlanPausedMonitorRefusesTheUnpause(t *testing.T) {
	f := newFreezeFixture(t)
	ctx := context.Background()
	id, monID := f.seedMonitor(f.live)
	if _, err := f.pool.Raw().Exec(ctx,
		`UPDATE monitor SET paused = true, paused_by = 'plan' WHERE id = $1`, monID); err != nil {
		t.Fatalf("plan-pause the monitor: %v", err)
	}
	if w := f.do(http.MethodPatch, "/v1/monitors/"+id, `{"paused":false}`); w.Code != http.StatusPaymentRequired {
		t.Fatalf("unpause a plan-paused check = %d (%s), want 402", w.Code, w.Body.String())
	}
	var paused bool
	var by *string
	if err := f.pool.Raw().QueryRow(ctx,
		`SELECT paused, paused_by FROM monitor WHERE id = $1`, monID).Scan(&paused, &by); err != nil ||
		!paused || by == nil || *by != "plan" {
		t.Fatalf("after the refused unpause paused=%v by=%v err=%v, want paused by plan", paused, by, err)
	}
	w := f.do(http.MethodPatch, "/v1/monitors/"+id, `{"paused":true}`)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"paused":true`) ||
		strings.Contains(w.Body.String(), "pausedBy") {
		t.Fatalf("owner pause of a plan-paused check = %d (%s), want 200 paused with no pausedBy", w.Code, w.Body.String())
	}
}

// Re-adding a URL answers the existing subscription as it stands: an
// owner-paused check comes back paused, not as a fresh running one.
func TestRecreatingAPausedCheckAnswersItPaused(t *testing.T) {
	f := newFreezeFixture(t)
	body := fmt.Sprintf(`{"type":"Website","target":"https://recreate-%d.example.com"}`, time.Now().UnixNano())
	if w := f.do(http.MethodPost, "/v1/monitors", body); w.Code != http.StatusCreated ||
		!strings.Contains(w.Body.String(), `"paused":false`) {
		t.Fatalf("create = %d (%s), want 201 paused:false", w.Code, w.Body.String())
	}
	if _, err := f.pool.Raw().Exec(context.Background(),
		`UPDATE monitor SET paused = true WHERE project_id = $1`, f.live); err != nil {
		t.Fatalf("owner-pause: %v", err)
	}
	if w := f.do(http.MethodPost, "/v1/monitors", body); w.Code != http.StatusCreated ||
		!strings.Contains(w.Body.String(), `"paused":true`) {
		t.Fatalf("re-create = %d (%s), want 201 paused:true", w.Code, w.Body.String())
	}
}

// With every project frozen the owner reaches none: a new check has nowhere
// to land, and project_id 0 would only fail the foreign key as a 500.
func TestCreateCheckAtZeroRefuses(t *testing.T) {
	f := newFreezeFixture(t)
	if _, err := f.pool.Raw().Exec(context.Background(),
		`UPDATE project SET frozen_at = now() WHERE id = $1`, f.live); err != nil {
		t.Fatalf("freeze the live project: %v", err)
	}
	w := f.do(http.MethodPost, "/v1/monitors", `{"type":"Website","target":"https://zero.example.com"}`)
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "no_current_project") {
		t.Fatalf("create at zero = %d (%s), want 409 no_current_project", w.Code, w.Body.String())
	}
}

// Free carries three Telegram seats: the fourth destination is listed with
// mutedBy "plan", and its test send answers the wall instead of passing while
// every real alert skips it. The oldest still tests.
func TestPlanMutedChannelIsMarkedAndRefusesTheTest(t *testing.T) {
	f := newFreezeFixture(t)
	ctx := context.Background()
	uniq := time.Now().UnixNano()
	ids := make([]string, 0, 4)
	for i := range 4 {
		person := seedPerson(t, f.pool, fmt.Sprintf("tg-%d-%d@example.com", uniq, i))
		var pubID pgtype.UUID
		if err := f.pool.Raw().QueryRow(ctx,
			`INSERT INTO alert_channel (public_id, tenant_id, project_id, kind, target, recipient_person_id, created_at)
			 VALUES (gen_random_uuid(), $1, $2, 'telegram', $3, $4, now() + $5::int * interval '1 second')
			 RETURNING public_id`,
			f.tenantID, f.live, fmt.Sprintf("%d", uniq+int64(i)), person, i).Scan(&pubID); err != nil {
			t.Fatalf("seed telegram channel %d: %v", i, err)
		}
		ids = append(ids, uuidStr(pubID))
	}
	w := f.do(http.MethodGet, "/v1/channels", "")
	var list struct {
		Channels []struct {
			ID      string `json:"id"`
			MutedBy string `json:"mutedBy"`
		} `json:"channels"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil || w.Code != http.StatusOK {
		t.Fatalf("channels = %d (%s) err=%v", w.Code, w.Body.String(), err)
	}
	for _, ch := range list.Channels {
		if want := map[bool]string{true: "plan", false: ""}[ch.ID == ids[3]]; ch.MutedBy != want {
			t.Fatalf("channel %s mutedBy=%q, want %q (fourth muted only): %s", ch.ID, ch.MutedBy, want, w.Body.String())
		}
	}
	if w := f.do(http.MethodPost, "/v1/channels/"+ids[3]+"/test", ""); w.Code != http.StatusPaymentRequired {
		t.Fatalf("test a muted destination = %d (%s), want 402", w.Code, w.Body.String())
	}
	if w := f.do(http.MethodPost, "/v1/channels/"+ids[0]+"/test", ""); w.Code != http.StatusAccepted {
		t.Fatalf("test an audible destination = %d (%s), want 202", w.Code, w.Body.String())
	}
}

// While the plan no longer carries a custom domain, the settings read names
// the moment the domain is unbound: the grace stamp plus the grace days.
func TestStatusPageReadNamesTheDomainLapse(t *testing.T) {
	f := newFreezeFixture(t)
	lapsed := time.Now().UTC().Truncate(time.Second)
	if _, err := f.pool.Raw().Exec(context.Background(),
		`INSERT INTO status_page (tenant_id, project_id, slug, domain, domain_lapsed_at)
		 VALUES ($1, $2, $3, $4, $5)`,
		f.tenantID, f.live, fmt.Sprintf("lapse-%d", time.Now().UnixNano()),
		fmt.Sprintf("status.lapse-%d.example.com", time.Now().UnixNano()), lapsed); err != nil {
		t.Fatalf("seed page: %v", err)
	}
	w := f.do(http.MethodGet, "/v1/status-page", "")
	want := lapsed.Add(pgstore.DomainGraceDays * 24 * time.Hour).Format(time.RFC3339)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"domainLapsesAt":"`+want+`"`) {
		t.Fatalf("status page read = %d (%s), want domainLapsesAt %s", w.Code, w.Body.String(), want)
	}
}

// Caddy's certificate ask follows the domain grace: a plan without custom
// domains keeps answering 200 inside the window and 404 past it, and a plan
// that carries them needs no stamp.
func TestDomainAllowedHonoursTheGrace(t *testing.T) {
	f := newFreezeFixture(t)
	ctx := context.Background()
	domain := fmt.Sprintf("status.allowed-%d.example.com", time.Now().UnixNano())
	if _, err := f.pool.Raw().Exec(ctx,
		`INSERT INTO status_page (tenant_id, project_id, slug, domain, domain_verified_at, domain_lapsed_at)
		 VALUES ($1, $2, $3, $4, now(), now())`,
		f.tenantID, f.live, fmt.Sprintf("allowed-%d", time.Now().UnixNano()), domain); err != nil {
		t.Fatalf("seed page: %v", err)
	}
	ask := func(want int, why string) {
		t.Helper()
		if w := f.do(http.MethodGet, "/internal/domain-allowed?domain="+domain, ""); w.Code != want {
			t.Fatalf("%s: ask = %d, want %d", why, w.Code, want)
		}
	}
	ask(http.StatusOK, "Free inside the grace")
	if _, err := f.pool.Raw().Exec(ctx,
		`UPDATE status_page SET domain_lapsed_at = now() - make_interval(days => $2) - interval '1 minute'
		  WHERE project_id = $1`, f.live, pgstore.DomainGraceDays); err != nil {
		t.Fatalf("age the stamp: %v", err)
	}
	ask(http.StatusNotFound, "Free past the grace")
	if _, err := f.pool.Raw().Exec(ctx,
		`UPDATE status_page SET domain_lapsed_at = NULL WHERE project_id = $1`, f.live); err != nil {
		t.Fatalf("clear the stamp: %v", err)
	}
	ask(http.StatusNotFound, "Free with no stamp")
	if _, err := f.pool.Raw().Exec(ctx, `UPDATE tenant SET plan = 'Indie' WHERE id = $1`, f.tenantID); err != nil {
		t.Fatalf("upgrade: %v", err)
	}
	ask(http.StatusOK, "Indie with no stamp")
}

// The public page publishes what was measured. An outage the plan paused out
// of measurement is left out rather than shown resolved, and an open one
// stays listed while its target is probed at the slower cadence it has now:
// a frozen subscription sets no pace, so a probe five minutes old is inside
// three of today's intervals though outside three of the one stamped at open.
func TestPublicStatusIncidentsAreMeasured(t *testing.T) {
	f := newFreezeFixture(t)
	ctx := context.Background()
	lc := incident.New(f.pool, nil)
	open := func(projectID int64) int64 {
		t.Helper()
		_, monID := f.seedMonitor(projectID)
		if _, created, err := lc.Open(ctx, monID, "Seeded check is down", 60); err != nil || !created {
			t.Fatalf("open: created=%v err=%v", created, err)
		}
		return monID
	}
	paused := open(f.live)
	if _, err := f.pool.Raw().Exec(ctx,
		`UPDATE monitor SET paused = true, paused_by = 'plan' WHERE id = $1`, paused); err != nil {
		t.Fatalf("plan-pause: %v", err)
	}
	if err := lc.ClosePlanPaused(ctx); err != nil {
		t.Fatalf("close plan-paused: %v", err)
	}
	frozen := open(f.frozen)
	if _, err := f.pool.Raw().Exec(ctx,
		`INSERT INTO target_facts (target_id, status, last_check_at)
		 SELECT target_id, 'down', now() - interval '5 minutes' FROM monitor WHERE id = $1`, frozen); err != nil {
		t.Fatalf("seed target facts: %v", err)
	}
	incidents := func(projectID int64) []map[string]any {
		data, _ := f.wa.publicStatusData(ctx, projectID, true)
		return data["incidents"].([]map[string]any)
	}
	if got := incidents(f.live); len(got) != 0 {
		t.Fatalf("live page lists %v, want nothing: a plan_paused close measured no recovery", got)
	}
	if got := incidents(f.frozen); len(got) != 1 || got[0]["ongoing"] != true {
		t.Fatalf("frozen page lists %v, want the one ongoing, still measured outage", got)
	}
}

// A leaked key in a frozen project is withdrawn without buying a plan: the
// owner revokes the project's whole key set by project id, the rows stay as
// the record, and the live project's key keeps working. A guest who reaches
// the snapshot may not, and an id the caller does not reach is a 404.
func TestFrozenProjectKeysStayRevocable(t *testing.T) {
	f := newFreezeFixture(t)
	ctx := context.Background()
	for _, name := range []string{"prod", "staging"} {
		if _, _, err := issueNamedKey(ctx, f.pool, f.tenantID, f.frozen, name); err != nil {
			t.Fatalf("issue %s: %v", name, err)
		}
	}
	if _, _, err := issueNamedKey(ctx, f.pool, f.tenantID, f.live, "live"); err != nil {
		t.Fatalf("issue live: %v", err)
	}
	path := "/v1/projects/" + f.projectWireID(f.frozen) + "/keys/revoke"

	owner := f.cookie
	guest := seedPerson(t, f.pool, fmt.Sprintf("keys-guest-%d@example.com", time.Now().UnixNano()))
	seedProjectMember(t, f.pool, f.frozen, guest, f.tenantID, "login")
	token, err := session.New(f.pool, session.DefaultTTL, nil).Create(ctx, guest, f.tenantID, &f.live)
	if err != nil {
		t.Fatalf("mint guest session: %v", err)
	}
	f.cookie = &http.Cookie{Name: session.CookieName, Value: token}
	if w := f.do(http.MethodPost, path, ""); w.Code != http.StatusForbidden {
		t.Fatalf("guest revoke = %d (%s), want 403 owner_only", w.Code, w.Body.String())
	}
	f.cookie = owner

	if w := f.do(http.MethodPost, "/v1/projects/deadbeef/keys/revoke", ""); w.Code != http.StatusNotFound {
		t.Fatalf("unknown project revoke = %d (%s), want 404", w.Code, w.Body.String())
	}
	if w := f.do(http.MethodPost, path, ""); w.Code != http.StatusNoContent {
		t.Fatalf("owner revoke = %d (%s), want 204", w.Code, w.Body.String())
	}
	var revoked, live int
	if err := f.pool.Raw().QueryRow(ctx,
		`SELECT count(*) FILTER (WHERE project_id = $1 AND state = 'revoked' AND revoked_at IS NOT NULL),
		        count(*) FILTER (WHERE project_id = $2 AND state = 'active')
		   FROM api_key WHERE project_id IN ($1, $2)`, f.frozen, f.live).Scan(&revoked, &live); err != nil {
		t.Fatalf("read keys: %v", err)
	}
	if revoked != 2 || live != 1 {
		t.Fatalf("after revoke: frozen revoked rows = %d (want 2, kept), live active = %d (want 1)", revoked, live)
	}
}

// The switch is not a write: a Member whose every project in this workspace
// is frozen resolves to no project, and still switches back to their own.
func TestGuestAtZeroSwitchesBackToTheirOwnProject(t *testing.T) {
	f := newFreezeFixture(t)
	ctx := context.Background()
	uniq := time.Now().UnixNano()
	guest := seedPerson(t, f.pool, fmt.Sprintf("zero-guest-%d@example.com", uniq))
	seedProjectMember(t, f.pool, f.frozen, guest, f.tenantID, "notify")
	ownTenant := seedOwnedTenant(t, f.pool, guest, fmt.Sprintf("zero-guest-ws-%d", uniq))
	own := seedProject(t, f.pool, ownTenant, fmt.Sprintf("own-%d.example.com", uniq%100000))
	token, err := session.New(f.pool, session.DefaultTTL, nil).Create(ctx, guest, f.tenantID, &f.frozen)
	if err != nil {
		t.Fatalf("mint guest session: %v", err)
	}
	f.cookie = &http.Cookie{Name: session.CookieName, Value: token}
	if w := f.do(http.MethodPost, "/v1/project/switch", `{"id":"`+f.projectWireID(own)+`"}`); w.Code != http.StatusNoContent {
		t.Fatalf("switch from zero = %d (%s), want 204", w.Code, w.Body.String())
	}
	var tenant, pick int64
	if err := f.pool.Raw().QueryRow(ctx,
		`SELECT tenant_id, project_id FROM session WHERE person_id = $1`, guest).Scan(&tenant, &pick); err != nil ||
		tenant != ownTenant || pick != own {
		t.Fatalf("session = (%d, %d) err %v, want their own (%d, %d)", tenant, pick, err, ownTenant, own)
	}
}

// The Plan page's day-8 facts: projects.used counts live projects, frozen
// ones ride as projects.frozen, and httpChecks.pausedByPlan counts the
// budget's pauses in live projects only. Both are absent at zero.
func TestPlanReadNamesWhatThePlanHoldsBack(t *testing.T) {
	f := newFreezeFixture(t)
	ctx := context.Background()
	type axis struct {
		Used         int  `json:"used"`
		Max          int  `json:"max"`
		Frozen       *int `json:"frozen"`
		PausedByPlan *int `json:"pausedByPlan"`
	}
	read := func() (checks, projects axis) {
		t.Helper()
		w := f.do(http.MethodGet, "/v1/plan", "")
		var body struct {
			HTTPChecks axis `json:"httpChecks"`
			Projects   axis `json:"projects"`
		}
		if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &body) != nil {
			t.Fatalf("plan = %d (%s)", w.Code, w.Body.String())
		}
		return body.HTTPChecks, body.Projects
	}
	checks, projects := read()
	if projects.Used != 1 || projects.Frozen == nil || *projects.Frozen != 1 || checks.PausedByPlan != nil {
		t.Fatalf("plan read = checks %+v projects %+v, want projects used 1 frozen 1 and no pausedByPlan", checks, projects)
	}
	_, live := f.seedMonitor(f.live)
	_, frozen := f.seedMonitor(f.frozen)
	if _, err := f.pool.Raw().Exec(ctx,
		`UPDATE monitor SET paused = true, paused_by = 'plan' WHERE id IN ($1, $2)`, live, frozen); err != nil {
		t.Fatalf("plan-pause: %v", err)
	}
	if checks, _ = read(); checks.PausedByPlan == nil || *checks.PausedByPlan != 1 {
		t.Fatalf("pausedByPlan = %v, want 1 (the frozen project's pause is not the budget's)", checks.PausedByPlan)
	}
	if _, err := f.pool.Raw().Exec(ctx, `UPDATE project SET frozen_at = NULL WHERE id = $1`, f.frozen); err != nil {
		t.Fatalf("thaw: %v", err)
	}
	if _, projects = read(); projects.Used != 2 || projects.Frozen != nil {
		t.Fatalf("projects after thaw = %+v, want used 2 and no frozen", projects)
	}
}
