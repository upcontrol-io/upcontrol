//go:build integration

// The claim and the activation events (owner rules, 2026-10-15): a claim
// adopts into the claimer's OWN workspace whatever workspace the session
// stands in, moves the project's data with it and opens the session on it;
// its event carries the visitor who pressed it; the CLI's link keeps the
// token in the fragment; a workspace's first check and a project's first
// install count once. newClaimFixture runs on the shared DSN, newSharedWorld
// on a database of its own.
package api

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.upcontrol.io/back/internal/account/session"
	"go.upcontrol.io/back/internal/analytics"
	"go.upcontrol.io/back/internal/ingest"
	"go.upcontrol.io/back/internal/storage/pg"
	"go.upcontrol.io/back/internal/storage/pgstore"
)

// liveRecorder writes web_events through pool; Stop it before reading them.
func liveRecorder(t *testing.T, pool *pg.Pool) *analytics.Recorder {
	t.Helper()
	rec := analytics.NewRecorder(analytics.PoolStore{Pool: pool}, pgstore.New(pool.Raw()), nil)
	rec.Start()
	t.Cleanup(func() { _ = rec.Stop(context.Background()) })
	return rec
}

func stopRecorder(t *testing.T, rec *analytics.Recorder) {
	t.Helper()
	if err := rec.Stop(context.Background()); err != nil {
		t.Fatalf("stop recorder: %v", err)
	}
}

// postAs sends one request through h with the given cookies.
func postAs(h http.Handler, path, body string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	r.Header.Set("X-Forwarded-For", fmt.Sprintf("192.0.2.%d", time.Now().UnixNano()%254+1))
	for _, c := range cookies {
		r.AddCookie(c)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

// sessionScope reads where the session behind a raw cookie value stands.
func sessionScope(t *testing.T, pool *pg.Pool, raw string) (tenantID int64, projectID *int64) {
	t.Helper()
	hash := sha256.Sum256([]byte(raw))
	if err := pool.Raw().QueryRow(context.Background(),
		`SELECT tenant_id, project_id FROM session WHERE token_hash = $1`, hash[:]).Scan(&tenantID, &projectID); err != nil {
		t.Fatalf("read session: %v", err)
	}
	return tenantID, projectID
}

// guestSession stands the fixture's claimer in a host's workspace, as a login
// member of its one project: the claim route, the guest's cookie, the host's
// workspace.
func guestSession(t *testing.T, f *claimFixture) (http.Handler, *http.Cookie, int64) {
	t.Helper()
	uniq := time.Now().UnixNano()
	hostOwner := seedPerson(t, f.pool, fmt.Sprintf("host-owner-%d@example.com", uniq))
	hostTenant := seedOwnedTenant(t, f.pool, hostOwner, fmt.Sprintf("host-%d", uniq))
	hostProject := seedProject(t, f.pool, hostTenant, fmt.Sprintf("host-%d.example.com", uniq%100000))
	seedProjectMember(t, f.pool, hostProject, f.personID, hostTenant, "login")
	sm := session.New(f.pool, session.DefaultTTL, nil)
	guest, err := sm.Create(context.Background(), f.personID, hostTenant, &hostProject)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.Handle("POST /v1/claim", NewInstall(f.pool, nil, sm, "", false))
	return mux, &http.Cookie{Name: session.CookieName, Value: guest}, hostTenant
}

// C8, T4: a claimer whose session stands in a host's workspace, as a guest,
// adopts the project into their OWN workspace. The host's lone empty project
// survives (the absorb is the claimer's, never the host's), and the session
// opens on the claimed project.
func TestGrowthClaimFromAGuestSessionLandsInTheOwnWorkspace(t *testing.T) {
	f := newClaimFixture(t)
	route, guest, hostTenant := guestSession(t, f)

	w := postAs(route, "/v1/claim", `{"claimToken":"`+f.rawToken+`"}`, guest)
	if w.Code != http.StatusOK {
		t.Fatalf("claim from a guest session = %d (%s), want 200", w.Code, w.Body.String())
	}
	if holder := f.count(t, `SELECT tenant_id FROM project WHERE id = $1`, f.anonProjID); holder != f.claimerID {
		t.Fatalf("the claimed project sits in tenant %d, want the claimer's own %d", holder, f.claimerID)
	}
	if n := f.count(t, `SELECT count(*) FROM project WHERE tenant_id = $1`, hostTenant); n != 1 {
		t.Fatalf("the host's workspace holds %d projects, want its own 1 untouched", n)
	}
	tenant, project := sessionScope(t, f.pool, guest.Value)
	if tenant != f.claimerID || project == nil || *project != f.anonProjID {
		t.Fatalf("session scope = (%d, %v), want the claimed project (%d, %d)", tenant, project, f.claimerID, f.anonProjID)
	}
}

// C5 with C8, T6 with T4: the wall refusing a guest's claim is the claimer's
// OWN workspace's, so the session moves there, to its first project, where the
// upgrade is theirs to buy. Nothing is spent.
func TestGrowthClaimRefusedFromAGuestSessionMovesItHome(t *testing.T) {
	f := newClaimFixture(t)
	// The claimer's Free workspace holds a project in use: the absorb keeps
	// it, so the one-project wall refuses.
	used := seedProject(t, f.pool, f.claimerID, fmt.Sprintf("used-%d.example.com", time.Now().UnixNano()%100000))
	if _, err := f.pool.Raw().Exec(context.Background(),
		`INSERT INTO project_seq (project_id, next) VALUES ($1, 2)`, used); err != nil {
		t.Fatal(err)
	}
	route, guest, _ := guestSession(t, f)

	w := postAs(route, "/v1/claim", `{"claimToken":"`+f.rawToken+`"}`, guest)
	if w.Code != http.StatusPaymentRequired {
		t.Fatalf("claim with no room = %d (%s), want 402", w.Code, w.Body.String())
	}
	tenant, project := sessionScope(t, f.pool, guest.Value)
	if tenant != f.claimerID || project == nil || *project != used {
		t.Fatalf("session scope = (%d, %v), want the own workspace's project (%d, %d)", tenant, project, f.claimerID, used)
	}
	if f.unclaimedCount(t) != 1 || f.count(t, `SELECT tenant_id FROM project WHERE id = $1`, f.anonProjID) != f.anonID {
		t.Fatal("the refused claim spent the token or moved the project")
	}
}

// T7: the project's data moves with it, so a claimed CLI project opens with
// its history and its install still verified, and the session opens on it.
func TestGrowthClaimMovesTheDataAndOpensTheSessionOnIt(t *testing.T) {
	f := newClaimFixture(t)
	ctx := context.Background()
	for _, seed := range []string{
		`INSERT INTO events (tenant_id, project_id, ts, name, labels, actor) VALUES ($1, $2, now(), 'install_verified', '{}', '')`,
		`INSERT INTO series_1h (tenant_id, project_id, hour, service, level, fingerprint, lines)
		 VALUES ($1, $2, date_trunc('hour', now()), 'api', 'info', 1, 3)`,
		`INSERT INTO metrics (tenant_id, project_id, ts, name, labels, value) VALUES ($1, $2, now(), 'queue_depth', '{}', 4)`,
	} {
		if _, err := f.pool.Raw().Exec(ctx, seed, f.anonID, f.anonProjID); err != nil {
			t.Fatalf("seed %q: %v", seed, err)
		}
	}

	w := f.claim(t, `{"claimToken":"`+f.rawToken+`"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("claim = %d (%s), want 200", w.Code, w.Body.String())
	}
	for _, table := range []string{"events", "series_1h", "metrics"} {
		if n := f.count(t, `SELECT count(*) FROM `+table+` WHERE project_id = $1 AND tenant_id = $2`, f.anonProjID, f.claimerID); n != 1 {
			t.Fatalf("%s rows of the project under the claimer = %d, want 1", table, n)
		}
		if n := f.count(t, `SELECT count(*) FROM `+table+` WHERE tenant_id = $1`, f.anonID); n != 0 {
			t.Fatalf("%s rows left under the deleted tenant = %d, want 0", table, n)
		}
	}
	tenant, project := sessionScope(t, f.pool, f.cookie.Value)
	if tenant != f.claimerID || project == nil || *project != f.anonProjID {
		t.Fatalf("session scope = (%d, %v), want the claimed project (%d, %d)", tenant, project, f.claimerID, f.anonProjID)
	}
}

// The claim's event carries the visitor who pressed it: page_claimed names
// the page, claim_token_redeemed the project, and neither carries a token.
func TestGrowthClaimEventsCarryTheVisitor(t *testing.T) {
	for _, door := range []string{"slug", "token"} {
		t.Run(door, func(t *testing.T) {
			f := newClaimFixture(t)
			ctx := context.Background()
			rec := liveRecorder(t, f.pool)
			sm := session.New(f.pool, session.DefaultTTL, nil)
			mux := http.NewServeMux()
			mux.Handle("POST /v1/claim", NewInstall(f.pool, nil, sm, "", false).WithRecorder(rec))
			body, name, prop, value := `{"slug":"`+f.slug+`"}`, "page_claimed", "slug", f.slug
			if door == "token" {
				body, name, prop, value = `{"claimToken":"`+f.rawToken+`"}`, "claim_token_redeemed",
					"project", strconv.FormatInt(f.anonProjID, 10)
			}
			w := postAs(mux, "/v1/claim", body, f.cookie, &http.Cookie{Name: "uc_vid", Value: analytics.MintVisitorToken()})
			if w.Code != http.StatusOK {
				t.Fatalf("claim = %d (%s), want 200", w.Code, w.Body.String())
			}
			stopRecorder(t, rec)

			var visitor, person, tenant int64
			if err := f.pool.Raw().QueryRow(ctx,
				`SELECT visitor_id, person_id, tenant_id FROM web_events WHERE name = $1 AND props->>($2::text) = $3`,
				name, prop, value).Scan(&visitor, &person, &tenant); err != nil {
				t.Fatalf("read %s: %v", name, err)
			}
			if visitor == 0 || person != f.personID || tenant != f.claimerID {
				t.Fatalf("%s = visitor %d, person %d, tenant %d; want a visitor, %d, %d",
					name, visitor, person, tenant, f.personID, f.claimerID)
			}
			if n := f.count(t, `SELECT count(*) FROM web_events WHERE props::text LIKE '%' || $1 || '%'`, f.rawToken); n != 0 {
				t.Fatalf("the claim token reached %d analytics rows, want 0", n)
			}
		})
	}
}

// T5, T7: the anonymous mint's link keeps the token in the fragment, which no
// browser sends, and the mint counts with its project and no token.
func TestGrowthAnonymousMintAnswersTheFragmentLink(t *testing.T) {
	f := newClaimFixture(t)
	ctx := context.Background()
	rec := liveRecorder(t, f.pool)
	h := NewInstall(f.pool, nil, session.New(f.pool, session.DefaultTTL, nil), "https://up.example", false).WithRecorder(rec)

	w := postAs(h, "/v1/projects/anonymous", `{"cli_version":"0.4.0"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("anonymous mint = %d (%s), want 200", w.Code, w.Body.String())
	}
	var resp struct {
		ClaimToken string `json:"claimToken"`
		ClaimURL   string `json:"claimUrl"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil || resp.ClaimToken == "" {
		t.Fatalf("decode mint %s: %v", w.Body.String(), err)
	}
	if resp.ClaimURL != "https://up.example/claim#"+resp.ClaimToken {
		t.Fatalf("claimUrl = %q, want the token in the fragment of /claim", resp.ClaimURL)
	}
	hash := sha256.Sum256([]byte(resp.ClaimToken))
	var projectID int64
	if err := f.pool.Raw().QueryRow(ctx,
		`SELECT p.id FROM project p JOIN tenant t ON t.id = p.tenant_id WHERE t.claim_token_hash = $1`, hash[:]).Scan(&projectID); err != nil {
		t.Fatalf("the minted project: %v", err)
	}
	stopRecorder(t, rec)
	if n := f.count(t,
		`SELECT count(*) FROM web_events WHERE name = 'anonymous_project_minted' AND props->>'project' = $1`,
		strconv.FormatInt(projectID, 10)); n != 1 {
		t.Fatalf("anonymous_project_minted rows = %d, want 1", n)
	}
	if n := f.count(t, `SELECT count(*) FROM web_events WHERE props::text LIKE '%' || $1 || '%'`, resp.ClaimToken); n != 0 {
		t.Fatalf("the claim token reached %d analytics rows, want 0", n)
	}
}

// first_check_created: a workspace somebody owns counts its first check of
// any kind, once, even after deleting every check and adding one again; an
// unclaimed demo tenant never counts.
func TestGrowthFirstCheckCountsOnceAndOnlyForAnOwnedWorkspace(t *testing.T) {
	pool, _, seats := newSharedWorld(t, 2)
	ctx := context.Background()
	rec := liveRecorder(t, pool)
	sm := session.New(pool, session.DefaultTTL, nil)
	mux := http.NewServeMux()
	mux.Handle("POST /v1/monitors", NewMonitors(pool, pgstore.New(pool.Raw()), sm, "").WithRecorder(rec))
	uniq := time.Now().UnixNano() % 100000
	website := func(n int) string {
		return fmt.Sprintf(`{"type":"website","name":"Site","target":"https://first-%d-%d.example.com","interval":"5m"}`, uniq, n)
	}
	const heartbeat = `{"type":"heartbeat","name":"Nightly","interval":"5m"}`

	// The demo tenant: no owner, reached by a login member of its project.
	var demoTenant, demoProject int64
	if err := pool.Raw().QueryRow(ctx,
		`INSERT INTO tenant (public_id, name, claim_token_hash) VALUES (gen_random_uuid(), 'demo', $1) RETURNING id`,
		[]byte(fmt.Sprintf("demo-%d", uniq))).Scan(&demoTenant); err != nil {
		t.Fatal(err)
	}
	demoProject = seedProject(t, pool, demoTenant, fmt.Sprintf("demo-%d.example.com", uniq))
	member := seedPerson(t, pool, fmt.Sprintf("demo-member-%d@example.com", uniq))
	seedProjectMember(t, pool, demoProject, member, demoTenant, "login")
	demo, err := sm.Create(ctx, member, demoTenant, &demoProject)
	if err != nil {
		t.Fatal(err)
	}

	for i, step := range []struct {
		cookie *http.Cookie
		body   string
	}{
		{&seats[0], website(1)}, {&seats[0], website(2)},
		{&seats[1], heartbeat}, {&seats[1], heartbeat},
		{&http.Cookie{Name: session.CookieName, Value: demo}, website(3)},
	} {
		if w := postAs(mux, "/v1/monitors", step.body, step.cookie); w.Code != http.StatusCreated {
			t.Fatalf("create %d = %d (%s), want 201", i, w.Code, w.Body.String())
		}
	}
	// Checks are deleted outright, so an emptied workspace looks fresh; it
	// still had its first.
	websiteSeat, _ := sessionScope(t, pool, seats[0].Value)
	if _, err := pool.Raw().Exec(ctx, `DELETE FROM monitor WHERE tenant_id = $1`, websiteSeat); err != nil {
		t.Fatal(err)
	}
	if w := postAs(mux, "/v1/monitors", website(1), &seats[0]); w.Code != http.StatusCreated {
		t.Fatalf("re-add after deleting every check = %d (%s), want 201", w.Code, w.Body.String())
	}
	stopRecorder(t, rec)

	firsts := map[int64]string{}
	rows, err := pool.Raw().Query(ctx, `SELECT tenant_id, props->>'kind' FROM web_events WHERE name = 'first_check_created'`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var tenant int64
		var kind string
		if err := rows.Scan(&tenant, &kind); err != nil {
			t.Fatal(err)
		}
		if _, twice := firsts[tenant]; twice {
			t.Fatalf("tenant %d counted its first check twice", tenant)
		}
		firsts[tenant] = kind
	}
	rows.Close()
	heartbeatSeat, _ := sessionScope(t, pool, seats[1].Value)
	if len(firsts) != 2 || firsts[websiteSeat] != "website" || firsts[heartbeatSeat] != "heartbeat" {
		t.Fatalf("first_check_created by tenant = %v, want website for %d and heartbeat for %d only (demo %d never)",
			firsts, websiteSeat, heartbeatSeat, demoTenant)
	}
}

// first_install_verified: the SDK sends install_verified on every start, and
// only the project's first one stamps the project and counts.
func TestGrowthFirstInstallVerifiedCountsOnce(t *testing.T) {
	pool, _, _ := newSharedWorld(t, 1)
	ctx := context.Background()
	rec := liveRecorder(t, pool)
	var projectID, tenantID int64
	if err := pool.Raw().QueryRow(ctx, `SELECT id, tenant_id FROM project`).Scan(&projectID, &tenantID); err != nil {
		t.Fatal(err)
	}
	sink := &chLogSink{pgs: pgstore.New(pool.Raw()), pool: pool, rec: rec}
	// Older than the log partitions reach: the event row is written, no log row.
	start := func() []byte {
		return mustMarshalEnvelope(t, &ingest.RowEnvelope{
			TenantID: tenantID, ProjectID: projectID, Level: "info",
			TS:      time.Now().UTC().Add(-40 * 24 * time.Hour).Format(time.RFC3339Nano),
			Message: "install_verified", Event: "install_verified",
		})
	}
	stamp := func() *time.Time {
		t.Helper()
		var at *time.Time
		if err := pool.Raw().QueryRow(ctx, `SELECT install_verified_at FROM project WHERE id = $1`, projectID).Scan(&at); err != nil {
			t.Fatal(err)
		}
		return at
	}
	if at := stamp(); at != nil {
		t.Fatalf("a fresh project carries install_verified_at %v, want none", at)
	}

	// Two starts in one batch, then a restart in the next.
	if err := sink.Flush(ctx, "logs", [][]byte{start(), start()}); err != nil {
		t.Fatalf("first flush: %v", err)
	}
	first := stamp()
	if first == nil {
		t.Fatal("the first install_verified did not stamp the project")
	}
	if err := sink.Flush(ctx, "logs", [][]byte{start()}); err != nil {
		t.Fatalf("second flush: %v", err)
	}
	if again := stamp(); again == nil || !again.Equal(*first) {
		t.Fatalf("a restart moved the stamp %v -> %v, want it kept", first, again)
	}
	stopRecorder(t, rec)
	if n := oneInt(t, pool, `SELECT count(*) FROM events WHERE project_id = $1 AND name = 'install_verified'`, projectID); n != 3 {
		t.Fatalf("install_verified events = %d, want 3: every start is still an event", n)
	}
	if n := oneInt(t, pool,
		`SELECT count(*) FROM web_events WHERE name = 'first_install_verified' AND props->>'project' = $1`,
		strconv.FormatInt(projectID, 10)); n != 1 {
		t.Fatalf("first_install_verified rows = %d, want 1", n)
	}
}
