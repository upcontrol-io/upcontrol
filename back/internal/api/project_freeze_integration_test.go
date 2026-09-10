//go:build integration

// The freeze wall (docs/plans/trial-and-freeze.md): a frozen project is a
// snapshot. The list still carries the row (members must see what they lost
// access to, not have it vanish), the switch is the one door into it and
// answers 402, and a frozen project's monitors accept no edits or deletes.
// Run with -tags=integration, UC_TEST_POSTGRES set.

package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"go.upcontrol.io/back/internal/account/session"
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
	mon := NewMonitors(pool, pgstore.New(pool.Raw()), sm, "http://test")
	mux.Handle("PATCH /v1/monitors/{id}", mon)
	mux.Handle("DELETE /v1/monitors/{id}", mon)

	f := &freezeF{t: t, pool: pool, tenantID: tenantID, live: live, frozen: frozen,
		cookie: &http.Cookie{Name: session.CookieName, Value: token}, route: mux}
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
// cheapest lifting plan, and the session keeps its live pick.
func TestSwitchIntoFrozenProjectAnswersTheWall(t *testing.T) {
	f := newFreezeFixture(t)
	var pubID pgtype.UUID
	if err := f.pool.Raw().QueryRow(context.Background(),
		`SELECT public_id FROM project WHERE id = $1`, f.frozen).Scan(&pubID); err != nil {
		t.Fatalf("read frozen project: %v", err)
	}
	w := f.do(http.MethodPost, "/v1/project/switch", `{"id":"`+uuidStr(pubID)+`"}`)
	if w.Code != http.StatusPaymentRequired {
		t.Fatalf("switch into frozen = %d (%s), want 402", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "frozen") || !strings.Contains(w.Body.String(), "upgrade") {
		t.Fatalf("402 body must name the freeze and the lift: %s", w.Body.String())
	}
	var pick int64
	if err := f.pool.Raw().QueryRow(context.Background(),
		`SELECT project_id FROM session WHERE tenant_id = $1`, f.tenantID).Scan(&pick); err != nil || pick != f.live {
		t.Fatalf("session pick = %d (err %v), want the live project %d", pick, err, f.live)
	}
}

// A frozen project's monitor is a snapshot: PATCH and DELETE both answer the
// same wall instead of changing what an upgrade restores.
func TestFrozenProjectMonitorIsReadOnly(t *testing.T) {
	f := newFreezeFixture(t)
	ctx := context.Background()
	// 0.28.0: a monitor is a subscription to a shared probe_target, so the
	// seed builds the pair the create path would have built. The key carries
	// the run's uniq: probe_target.key is globally unique and the shared test
	// database survives runs.
	var monID int64
	if err := f.pool.Raw().QueryRow(ctx,
		`WITH tgt AS (
		   INSERT INTO probe_target (key, kind, url) VALUES ($3, 'website', $3) RETURNING id)
		 INSERT INTO monitor (public_id, tenant_id, project_id, target_id, kind, name, target, interval_sec)
		 SELECT gen_random_uuid(), $1, $2, tgt.id, 'http', 'Frozen check', $3, 60
		   FROM tgt RETURNING id`,
		f.tenantID, f.frozen, fmt.Sprintf("https://frozen-%d.example.com", time.Now().UnixNano())).Scan(&monID); err != nil {
		t.Fatalf("seed frozen monitor: %v", err)
	}
	var pubID pgtype.UUID
	if err := f.pool.Raw().QueryRow(ctx,
		`SELECT public_id FROM monitor WHERE id = $1`, monID).Scan(&pubID); err != nil {
		t.Fatalf("read monitor: %v", err)
	}
	id := uuidStr(pubID)

	w := f.do(http.MethodPatch, "/v1/monitors/"+id, `{"name":"renamed"}`)
	if w.Code != http.StatusPaymentRequired {
		t.Fatalf("patch frozen monitor = %d (%s), want 402", w.Code, w.Body.String())
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
