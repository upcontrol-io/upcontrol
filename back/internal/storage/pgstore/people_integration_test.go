//go:build integration

// Integration test for the people reads against a real Postgres with every
// migration applied: the four answers the SDK's feeds computed on the client,
// now GROUP BYs over events with a person behind them. The law is under test
// as much as the arithmetic: an event with no actor is a real event — count(*)
// sees it — and never a person.
// Run: go test -tags=integration ./internal/storage/pgstore/...
package pgstore

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// A tenant and project of their own, in the spirit of series_integration_test:
// no seed from another file can bend a count.
const (
	peopleTenant  = 95
	peopleProject = 96
)

// seedEvent writes one event the way the ingest will once it carries actors:
// straight SQL, because InsertEvents has no actor field yet.
func seedEvent(t *testing.T, pool *pgxpool.Pool, ts time.Time, name, actor string, labels map[string]string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO events (tenant_id, project_id, ts, name, labels, actor)
		 VALUES ($1, $2, $3, $4, $5, $6)`,
		peopleTenant, peopleProject, ts, name, jsonb(labels), actor); err != nil {
		t.Fatalf("seed %s/%s: %v", name, actor, err)
	}
}

func TestPeopleFunnelAndBreakdown(t *testing.T) {
	s, pool := openStore(t)
	ctx := context.Background()
	at := time.Now().UTC().Add(-5 * time.Minute)
	from, to := at.Add(-time.Minute), at.Add(10*time.Minute)

	// One person, three visits: three events, one person. A second person
	// visits once, and one visit has nobody behind it at all.
	seedEvent(t, pool, at, "visit", "ada", nil)
	seedEvent(t, pool, at.Add(time.Second), "visit", "ada", nil)
	seedEvent(t, pool, at.Add(2*time.Second), "visit", "ada", nil)
	seedEvent(t, pool, at.Add(3*time.Second), "visit", "grace", nil)
	seedEvent(t, pool, at.Add(4*time.Second), "visit", "", nil)
	seedEvent(t, pool, at.Add(5*time.Second), "signup", "ada", nil)

	steps, err := s.FunnelSteps(ctx, peopleTenant, peopleProject, []string{"visit", "signup", "paid"}, from, to)
	if err != nil {
		t.Fatalf("funnel steps: %v", err)
	}
	// In the order given, zeros included: a funnel whose later steps answer
	// nothing is itself the reading.
	if len(steps) != 3 ||
		steps[0].Labels["step"] != "visit" || steps[0].Sum != 2 ||
		steps[1].Labels["step"] != "signup" || steps[1].Sum != 1 ||
		steps[2].Labels["step"] != "paid" || steps[2].Sum != 0 {
		t.Fatalf("visit=2 people, signup=1, paid=0, in order; got %+v", steps)
	}

	// The actorless visit IS an event: count(*) counts it, the people count
	// did not. Both readings are right and they must never agree by accident.
	var events int64
	for _, b := range mustEventBuckets(t, s, "visit", from, to) {
		events += b.Count
	}
	if events != 5 {
		t.Fatalf("count(*) sees all five visits, actorless one included; got %d", events)
	}

	// The breakdown folds the same law by a label: two people on pro, one on
	// free, most people first; the unlabelled row and the actorless one answer
	// no value.
	seedEvent(t, pool, at, "purchase", "ada", map[string]string{"plan": "pro"})
	seedEvent(t, pool, at.Add(time.Second), "purchase", "grace", map[string]string{"plan": "pro"})
	seedEvent(t, pool, at.Add(2*time.Second), "purchase", "linus", map[string]string{"plan": "free"})
	seedEvent(t, pool, at.Add(3*time.Second), "purchase", "", map[string]string{"plan": "pro"})
	seedEvent(t, pool, at.Add(4*time.Second), "purchase", "ada", nil)
	values, err := s.BreakdownValues(ctx, peopleTenant, peopleProject, "purchase", "plan", false, from, to)
	if err != nil {
		t.Fatalf("breakdown values: %v", err)
	}
	if len(values) != 2 ||
		values[0].Labels["plan"] != "pro" || values[0].Sum != 2 ||
		values[1].Labels["plan"] != "free" || values[1].Sum != 1 {
		t.Fatalf("pro=2 people then free=1; got %+v", values)
	}

	// The same fold counting rows. The pro row nobody is behind is the whole
	// point: it is invisible to the people count above and must be counted
	// here, or a dimension with no actors at all — a delivery outcome, an HTTP
	// status — reads 0 and says nothing about why.
	rowsPerPlan, err := s.BreakdownValues(ctx, peopleTenant, peopleProject, "purchase", "plan", true, from, to)
	if err != nil {
		t.Fatalf("breakdown events: %v", err)
	}
	if len(rowsPerPlan) != 2 ||
		rowsPerPlan[0].Labels["plan"] != "pro" || rowsPerPlan[0].Sum != 3 ||
		rowsPerPlan[1].Labels["plan"] != "free" || rowsPerPlan[1].Sum != 1 {
		t.Fatalf("pro=3 events (the actorless one counts) then free=1; got %+v", rowsPerPlan)
	}
}

func mustEventBuckets(t *testing.T, s *Store, name string, from, to time.Time) []Bucket {
	t.Helper()
	rows, err := s.EventBuckets(context.Background(), peopleTenant, peopleProject, name, from, to, 600)
	if err != nil {
		t.Fatalf("event buckets: %v", err)
	}
	return rows
}

// mondayUTC is the ISO Monday of ts's week at 00:00 UTC, the cohort grid's
// own unit.
func mondayUTC(ts time.Time) time.Time {
	wd := (int(ts.Weekday()) + 6) % 7 // Monday = 0
	return ts.AddDate(0, 0, -wd).Truncate(24 * time.Hour)
}

func TestPeopleRetentionAndExperiment(t *testing.T) {
	s, pool := openStore(t)
	ctx := context.Background()
	// A window of four whole weeks, Monday-aligned, so the grid's edges are
	// the grid's own unit.
	to := mondayUTC(time.Now().UTC())
	from := to.Add(-28 * 24 * time.Hour)

	// ada is first seen inside the window's first week and comes back a week
	// later: her cohort is that first Monday and her return lands at offset 1,
	// not in a fresh cohort of its own.
	seedEvent(t, pool, from.Add(24*time.Hour), "visit", "ada", nil)
	seedEvent(t, pool, from.Add(8*24*time.Hour), "visit", "ada", nil)
	// grace is first seen BEFORE the window and active inside it: her cohort's
	// Monday is before `from`, so she answers nothing — a cohort that began
	// earlier would come back as a fraction of itself.
	seedEvent(t, pool, from.Add(-48*time.Hour), "visit", "grace", nil)
	seedEvent(t, pool, from.Add(2*24*time.Hour), "visit", "grace", nil)
	// An event with nobody behind it, inside the window: an event, never a person.
	seedEvent(t, pool, from.Add(3*24*time.Hour), "visit", "", nil)

	cohorts, err := s.RetentionCohorts(ctx, peopleTenant, peopleProject, from, to)
	if err != nil {
		t.Fatalf("retention cohorts: %v", err)
	}
	first := from.Format("2006-01-02")
	if len(cohorts) != 2 {
		t.Fatalf("ada alone makes two cells, at offsets 0 and 1; got %+v", cohorts)
	}
	if cohorts[0].Labels["cohort"] != first || cohorts[0].Labels["week"] != "0" || cohorts[0].Sum != 1 {
		t.Fatalf("week 0 of the first Monday's cohort holds ada alone (grace's cohort predates the window, the actorless event nobody); got %+v", cohorts[0])
	}
	if cohorts[1].Labels["cohort"] != first || cohorts[1].Labels["week"] != "1" || cohorts[1].Sum != 1 {
		t.Fatalf("ada's return is offset 1 of her first week, not a cohort of its own; got %+v", cohorts[1])
	}

	// One A/B test is ONE event name whose rows carry the arm and the stat in
	// the wire's own two label keys. Two arms exposed — ada twice, which is
	// two events and one person — one arm converted, and a row missing either
	// key answers nothing. Seeded after the retention assertions on purpose:
	// every event is also an actor's possible first event, so these would move
	// the grid above.
	seedEvent(t, pool, from.Add(24*time.Hour), "checkout_test", "ada", map[string]string{"uc.variant": "A", "uc.stat": "exposed"})
	seedEvent(t, pool, from.Add(25*time.Hour), "checkout_test", "ada", map[string]string{"uc.variant": "A", "uc.stat": "exposed"})
	seedEvent(t, pool, from.Add(26*time.Hour), "checkout_test", "grace", map[string]string{"uc.variant": "B", "uc.stat": "exposed"})
	seedEvent(t, pool, from.Add(27*time.Hour), "checkout_test", "ada", map[string]string{"uc.variant": "A", "uc.stat": "converted"})
	// One row missing its arm and one missing its stat: they answer neither.
	seedEvent(t, pool, from.Add(28*time.Hour), "checkout_test", "linus", map[string]string{"uc.stat": "exposed"})
	seedEvent(t, pool, from.Add(29*time.Hour), "checkout_test", "linus", map[string]string{"uc.variant": "B"})

	arms, err := s.ExperimentArms(ctx, peopleTenant, peopleProject, "checkout_test", "uc.variant", "uc.stat", from, to)
	if err != nil {
		t.Fatalf("experiment arms: %v", err)
	}
	people := map[string]float64{}
	for _, row := range arms {
		people[row.Labels["uc.variant"]+"/"+row.Labels["uc.stat"]] = row.Sum
	}
	if len(people) != 3 ||
		people["A/exposed"] != 1 || people["B/exposed"] != 1 || people["A/converted"] != 1 {
		t.Fatalf("A exposed 1 (ada twice is one person), B exposed 1, A converted 1, the keyless rows nothing; got %v", people)
	}
}
