//go:build integration

// The board's series reads against a real database, through ServeHTTP over
// the owner's fixed identity: a label filter on an event read and on its
// previous window, the distinct people behind a bare event name, a retention
// grid of one event, a check's percentiles, a sibling project's check reading
// as no check at all, and the read queue. Run with -tags=integration and
// UC_TEST_POSTGRES set.

package api

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.upcontrol.io/back/internal/storage/pg"
	"go.upcontrol.io/back/internal/storage/pgstore"
)

type seriesAnswer struct {
	ID       string     `json:"id"`
	From     string     `json:"from"`
	Step     int        `json:"step"`
	Points   []*float64 `json:"points"`
	Total    *float64   `json:"total"`
	Previous *float64   `json:"previous"`
	Rows     []struct {
		Labels map[string]string `json:"labels"`
		Value  float64           `json:"value"`
	} `json:"rows"`
}

// bucketOf is the index ts lands in on the answer's own clock, so a case never
// depends on where the hour or the half-hour fell while it ran.
func (s seriesAnswer) bucketOf(t *testing.T, ts time.Time) int {
	t.Helper()
	from, err := time.Parse(time.RFC3339, s.From)
	if err != nil {
		t.Fatalf("the answer's from %q: %v", s.From, err)
	}
	return int(ts.Sub(from) / (time.Duration(s.Step) * time.Second))
}

// num reads a nullable number as one comparable value: nil stays nil.
func num(p *float64) any {
	if p == nil {
		return nil
	}
	return *p
}

// seriesWorld is a tenant on Growth (a month of history) whose owner reads the
// board, with the store the series reads go through wired in.
func seriesWorld(t *testing.T, projects int) (*pg.Pool, *writeAPI, int64) {
	t.Helper()
	pool := openProjectsGateDB(t)
	tenantID := seedPlanTenant(t, pool, "Growth", projects)
	h := planTenantAPI(t, pool, tenantID)
	h.pgs = pgstore.New(pool.Raw())
	return pool, h, tenantID
}

// readSeries posts one batch and answers its series by id.
func readSeries(t *testing.T, h *writeAPI, queries string) map[string]seriesAnswer {
	t.Helper()
	code, body := sessionCall(t, h, http.MethodPost, "/v1/series", `{"queries":`+queries+`}`)
	if code != http.StatusOK {
		t.Fatalf("POST /v1/series = %d %s", code, body)
	}
	var out struct {
		Series []seriesAnswer `json:"series"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("decode the series: %v (%s)", err, body)
	}
	byID := map[string]seriesAnswer{}
	for _, s := range out.Series {
		byID[s.ID] = s
	}
	return byID
}

func seedEventRow(t *testing.T, pool *pg.Pool, tenantID, projectID int64, ts time.Time, name, actor, labels string) {
	t.Helper()
	if _, err := pool.Raw().Exec(t.Context(),
		`INSERT INTO events (tenant_id, project_id, ts, name, labels, actor) VALUES ($1, $2, $3, $4, $5::jsonb, $6)`,
		tenantID, projectID, ts, name, labels, actor); err != nil {
		t.Fatalf("seed %s by %q: %v", name, actor, err)
	}
}

func TestEventLabelsNarrowTheWindowAndThePreviousOne(t *testing.T) {
	pool, h, tenantID := seriesWorld(t, 1)
	project := boardOf(t, pool, tenantID)
	now := time.Now().UTC()
	inWindow := []time.Time{now.Add(-10 * time.Minute), now.Add(-12 * time.Minute), now.Add(-14 * time.Minute)}
	for i, labels := range []string{`{"plan":"pro"}`, `{"plan":"pro","region":"eu"}`, `{"plan":"free"}`} {
		seedEventRow(t, pool, tenantID, project, inWindow[i], "purchase", "", labels)
	}
	// The day before: the previous window of a 24h read.
	seedEventRow(t, pool, tenantID, project, now.Add(-30*time.Hour), "purchase", "", `{"plan":"pro"}`)
	seedEventRow(t, pool, tenantID, project, now.Add(-31*time.Hour), "purchase", "", `{"plan":"free"}`)

	got := readSeries(t, h, `[
		{"id":"all","source":"event","range":"24h","name":"purchase"},
		{"id":"pro","source":"event","range":"24h","name":"purchase","labels":{"plan":"pro"}},
		{"id":"pro-eu","source":"event","range":"24h","name":"purchase","labels":{"plan":"pro","region":"eu"}},
		{"id":"hourly","source":"event","range":"24h","name":"purchase","bucket":"hour"},
		{"id":"daily","source":"event","range":"7d","name":"purchase","bucket":"day"}]`)
	for id, want := range map[string][2]any{
		"all": {3.0, 2.0}, "pro": {2.0, 1.0}, "pro-eu": {1.0, 0.0},
		// A week read in days holds all five, and the week before it nothing.
		"daily": {5.0, 0.0},
	} {
		if s := got[id]; num(s.Total) != want[0] || num(s.Previous) != want[1] {
			t.Fatalf("%s: total %v previous %v, want %v", id, num(s.Total), num(s.Previous), want)
		}
	}

	// The buckets are UTC hours and UTC days, and the span is the range's.
	for id, c := range map[string]struct{ step, n int }{"hourly": {3600, 24}, "daily": {86400, 7}} {
		s := got[id]
		from, err := time.Parse(time.RFC3339, s.From)
		if err != nil || s.Step != c.step || len(s.Points) != c.n || from.Unix()%int64(c.step) != 0 {
			t.Fatalf("%s: %d points of %ds from %s, want %d of %ds on a boundary", id, len(s.Points), s.Step, s.From, c.n, c.step)
		}
	}
	hourly := got["hourly"]
	want := make([]float64, len(hourly.Points))
	for _, ts := range inWindow {
		want[hourly.bucketOf(t, ts)]++
	}
	for i, p := range hourly.Points {
		if p == nil || *p != want[i] {
			t.Fatalf("hour %d = %v, want %v (an hour with nothing in it is a measured 0)", i, num(p), want[i])
		}
	}
}

// A bare event name on the people source counts who did it, per bucket, and
// its totals are distinct counts of their own windows: one person in two
// buckets is one person in the window, never two.
func TestABarePeopleNameCountsPeopleNotEvents(t *testing.T) {
	pool, h, tenantID := seriesWorld(t, 1)
	project := boardOf(t, pool, tenantID)
	now := time.Now().UTC()
	// Eighty minutes apart, so always two different hours.
	early, late := now.Add(-90*time.Minute), now.Add(-10*time.Minute)
	seedEventRow(t, pool, tenantID, project, early, "login", "ada", `{"method":"google"}`)
	seedEventRow(t, pool, tenantID, project, late, "login", "ada", `{"method":"google"}`)
	seedEventRow(t, pool, tenantID, project, late, "login", "grace", `{"method":"email"}`)
	seedEventRow(t, pool, tenantID, project, late, "login", "", `{"method":"google"}`)
	seedEventRow(t, pool, tenantID, project, now.Add(-30*time.Hour), "login", "linus", `{}`)

	got := readSeries(t, h, `[
		{"id":"people","source":"people","range":"24h","name":"login","bucket":"hour"},
		{"id":"google","source":"people","range":"24h","name":"login","bucket":"hour","labels":{"method":"google"}},
		{"id":"events","source":"event","range":"24h","name":"login","bucket":"hour"}]`)
	for _, c := range []struct {
		id                         string
		early, late, total, before any
	}{
		// ada early, ada and grace late: 1 and 2, and the window's two people,
		// not the buckets' three. The actorless login is nobody; linus logged
		// in the day before.
		{"people", 1.0, 2.0, 2.0, 1.0},
		{"google", 1.0, 1.0, 1.0, 0.0},
		// The same rows counted as events: every row, the actorless one too.
		{"events", 1.0, 3.0, 4.0, 1.0},
	} {
		s := got[c.id]
		if s.Step != 3600 || len(s.Points) != 24 {
			t.Fatalf("%s: %d points of %ds, want 24 hours", c.id, len(s.Points), s.Step)
		}
		e, l := s.bucketOf(t, early), s.bucketOf(t, late)
		if num(s.Points[e]) != c.early || num(s.Points[l]) != c.late || num(s.Total) != c.total || num(s.Previous) != c.before {
			t.Fatalf("%s: early %v late %v total %v previous %v, want %v %v %v %v", c.id,
				num(s.Points[e]), num(s.Points[l]), num(s.Total), num(s.Previous), c.early, c.late, c.total, c.before)
		}
	}
}

// A retention grid that names an event reads that event's people alone, and the
// name reaches the read through the exact query the board sends: a workspace's
// actor that never sent `visitors` joins the project's grid and not that one.
func TestARetentionGridReadsOneEventsPeople(t *testing.T) {
	pool, h, tenantID := seriesWorld(t, 1)
	project := boardOf(t, pool, tenantID)
	at := time.Now().UTC().Add(-time.Hour)
	seedEventRow(t, pool, tenantID, project, at, "visitors", "ada", `{}`)
	seedEventRow(t, pool, tenantID, project, at, "trial_started", "tenant:7", `{}`)

	got := readSeries(t, h, `[
		{"id":"all","source":"people","range":"31d","cohort":"week"},
		{"id":"visitors","source":"people","range":"31d","cohort":"week","name":"visitors"}]`)
	for id, want := range map[string]float64{"all": 2, "visitors": 1} {
		rows := got[id].Rows
		if len(rows) != 1 || rows[0].Labels["week"] != "0" || rows[0].Value != want {
			t.Fatalf("%s: want one week-0 cell of %v people; got %+v", id, want, rows)
		}
	}
}

func seedProbe(t *testing.T, pool *pg.Pool, target int64, ts time.Time, ok bool, ms int) {
	t.Helper()
	status, class := 200, ""
	if !ok {
		status, class = 500, "status"
	}
	if _, err := pool.Raw().Exec(t.Context(),
		`INSERT INTO checks (target_id, interval_sec, ts, region, ok, status_code, error_class, total_ms)
		 VALUES ($1, 300, $2, 'test', $3, $4, $5, $6)`, target, ts, ok, status, class, ms); err != nil {
		t.Fatalf("seed a probe: %v", err)
	}
}

// p50 and p95 read the ok probes only, a bucket with none of them is null and
// never 0 ms, and the window's and the previous span's are percentiles of
// their own. A check of the workspace's other project reads exactly like a
// check that is gone, though it has probes of its own.
func TestCheckPercentilesAndASiblingProjectsCheck(t *testing.T) {
	pool, h, tenantID := seriesWorld(t, 2)
	var own, sibling int64
	if err := pool.Raw().QueryRow(t.Context(),
		`SELECT min(id), max(id) FROM project WHERE tenant_id = $1`, tenantID).Scan(&own, &sibling); err != nil {
		t.Fatalf("read the two projects: %v", err)
	}
	ownCheck, _, ownTarget := seedScopedMonitor(t, pool, tenantID, own, "Own")
	siblingCheck, _, siblingTarget := seedScopedMonitor(t, pool, tenantID, sibling, "Sibling")
	now := time.Now().UTC()
	// Thirty minutes apart, so always two different half-hour buckets.
	okAt, failedAt := now.Add(-10*time.Minute), now.Add(-40*time.Minute)
	seedProbe(t, pool, ownTarget, okAt, true, 100)
	seedProbe(t, pool, ownTarget, okAt, true, 300)
	seedProbe(t, pool, ownTarget, failedAt, false, 900)
	seedProbe(t, pool, ownTarget, now.Add(-30*time.Hour), true, 1000)
	seedProbe(t, pool, siblingTarget, okAt, true, 500)

	ref := func(id, name, check string) string {
		return `{"id":"` + id + `","source":"check","range":"24h","name":"` + name + `","where":{"check":"` + check + `"}}`
	}
	got := readSeries(t, h, "["+strings.Join([]string{
		ref("p50", "p50", ownCheck), ref("p95", "p95", ownCheck), ref("response", "response", ownCheck),
		ref("sibling", "response", siblingCheck), ref("sibling-p95", "p95", siblingCheck),
	}, ",")+"]")

	for _, c := range []struct {
		id                      string
		ok, total, previous, by float64
	}{
		// 100 and 300: the median 200, p95 interpolated to 290; the day before
		// held one probe of 1000.
		{"p50", 200, 200, 1000, 0},
		{"p95", 290, 290, 1000, 1e-9},
	} {
		s := got[c.id]
		o, f := s.bucketOf(t, okAt), s.bucketOf(t, failedAt)
		near := func(p *float64, want float64) bool { return p != nil && math.Abs(*p-want) <= c.by }
		if !near(s.Points[o], c.ok) || s.Points[f] != nil || !near(s.Total, c.total) || !near(s.Previous, c.previous) {
			t.Fatalf("%s: ok bucket %v, failed bucket %v, total %v, previous %v", c.id,
				num(s.Points[o]), num(s.Points[f]), num(s.Total), num(s.Previous))
		}
	}
	// The project's own check reads, so the sibling's silence below is the
	// project scope and not an empty table.
	if num(got["response"].Total) != 200.0 {
		t.Fatalf("the own check's response total = %v, want 200", num(got["response"].Total))
	}
	for _, id := range []string{"sibling", "sibling-p95"} {
		s := got[id]
		for i, p := range s.Points {
			if p != nil {
				t.Fatalf("%s: bucket %d = %v, want the null series of a check that is gone", id, i, *p)
			}
		}
		if s.Total != nil || s.Previous != nil {
			t.Fatalf("%s: total %v previous %v, want nulls", id, num(s.Total), num(s.Previous))
		}
	}
}

// slotsAtAnswer is a recorder that notes how many read slots were taken when
// the answer began; -1 until one does.
type slotsAtAnswer struct {
	*httptest.ResponseRecorder
	held int
}

func (w *slotsAtAnswer) WriteHeader(code int) {
	w.held = len(readSlots)
	w.ResponseRecorder.WriteHeader(code)
}

// The reads queue rather than fail. With every slot held, a series request
// waits out its own deadline and answers nothing; with the slots free the same
// request answers, and gives its slot back before the answer is written, so a
// client slow to read the body holds none.
func TestSeriesWaitsForAReadSlotAndFreesItBeforeAnswering(t *testing.T) {
	_, h, _ := seriesWorld(t, 1)
	call := func(ctx context.Context) *slotsAtAnswer {
		w := &slotsAtAnswer{ResponseRecorder: httptest.NewRecorder(), held: -1}
		body := strings.NewReader(`{"queries":[{"id":"e","source":"event","range":"24h","name":"signup"}]}`)
		h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/series", body).WithContext(ctx))
		return w
	}

	var held []func()
	free := func() {
		for _, release := range held {
			release()
		}
		held = nil
	}
	t.Cleanup(free) // a slot left taken would stall every later read in this process
	for range cap(readSlots) {
		release, err := takeReadSlot(t.Context())
		if err != nil {
			t.Fatalf("hold a slot: %v", err)
		}
		held = append(held, release)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 500*time.Millisecond)
	defer cancel()
	if w := call(ctx); ctx.Err() == nil || w.held != -1 || w.Body.Len() > 0 {
		t.Fatalf("with every slot held the read waits out its deadline and answers nothing; ended early %v, answered %q",
			ctx.Err() == nil, w.Body.String())
	}

	free()
	if w := call(t.Context()); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"id":"e"`) || w.held != 0 {
		t.Fatalf("with the slots free the read answers, its slot already back; got %d with %d held: %s",
			w.Code, w.held, w.Body.String())
	}
}
