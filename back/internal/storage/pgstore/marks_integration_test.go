//go:build integration

// Integration test for the board's mark reads against a real Postgres with
// every migration applied: which events read as deploys, when a release is
// new, which incidents overlap a window, and which rows the cap keeps.
// Run: go test -tags=integration ./internal/storage/pgstore/...
package pgstore

import (
	"context"
	"strconv"
	"testing"
	"time"
)

// A tenant and projects of their own: the incident rows need real tenant and
// project rows behind them, and no other file's seed may land in a window.
const (
	marksTenant  = 97
	marksProject = 98
	marksSibling = 99
	marksBusy    = 100
)

func TestMarkReads(t *testing.T) {
	s, pool := openStore(t)
	ctx := context.Background()
	to := time.Now().UTC().Truncate(time.Hour)
	from := to.Add(-24 * time.Hour)

	for _, stmt := range []string{
		`INSERT INTO tenant (id, public_id, name) VALUES (97, gen_random_uuid(), 'marks')`,
		`INSERT INTO project (id, public_id, tenant_id, domain) VALUES (98, gen_random_uuid(), 97, 'marks.example')`,
		`INSERT INTO project (id, public_id, tenant_id, domain) VALUES (99, gen_random_uuid(), 97, 'sibling.example')`,
	} {
		if _, err := pool.Exec(ctx, stmt); err != nil {
			t.Fatalf("seed %q: %v", stmt[:40], err)
		}
	}

	event := func(project uint64, at time.Time, name string, labels map[string]string) EventRow {
		return EventRow{TenantID: marksTenant, ProjectID: project, TS: at, Name: name, Labels: labels}
	}
	sha := "0123456789abcdef0123456789abcdef01234567"
	events := []EventRow{
		event(marksProject, from.Add(time.Hour), "deploy", map[string]string{"sha": sha}),
		// An unmarked log line that starts with "deploy" is stored as an event
		// named after its whole message: a sentence, not a deploy.
		event(marksProject, from.Add(2*time.Hour), "deploy failed for api", nil),
		// Whatever splits its words does: a tab or a line break is no deploy
		// either. No space anywhere, so only the whitespace check refuses them.
		event(marksProject, from.Add(130*time.Minute), "deploy\tfailed", nil),
		event(marksProject, from.Add(140*time.Minute), "deploy\nfailed", nil),
		event(marksProject, from.Add(3*time.Hour), "github_deployment_status_created", nil),
		// Before the window, and a sibling project's deploy inside it.
		event(marksProject, from.Add(-time.Hour), "deploy", nil),
		event(marksSibling, from.Add(4*time.Hour), "deploy", nil),

		// 1.0.0 was already running before the window opened: not new inside it.
		event(marksProject, from.Add(-48*time.Hour), "app_started", map[string]string{"version": "1.0.0"}),
		event(marksProject, from.Add(time.Hour), "app_started", map[string]string{"version": "1.0.0"}),
		// 1.1.0 is first seen inside, and its restart is the same release.
		event(marksProject, from.Add(2*time.Hour), "app_started", map[string]string{"version": "1.1.0"}),
		event(marksProject, from.Add(5*time.Hour), "app_started", map[string]string{"version": "1.1.0"}),
		// The SDK's "0" is no version at all.
		event(marksProject, from.Add(3*time.Hour), "app_started", map[string]string{"version": "0"}),
		// release outranks version, and an empty one falls back to it.
		event(marksProject, from.Add(6*time.Hour), "app_started", map[string]string{"release": sha, "version": "1.1.0"}),
		event(marksProject, from.Add(7*time.Hour), "app_started", map[string]string{"release": "", "version": "2.0.0"}),
	}
	// One more deploy than the cap: the read keeps the latest hundred.
	for i := range marksCap + 5 {
		events = append(events, event(marksBusy, from.Add(time.Duration(i)*time.Minute), "deploy_"+strconv.Itoa(i), nil))
	}
	if err := s.InsertEvents(ctx, events); err != nil {
		t.Fatalf("insert events: %v", err)
	}

	incident := func(title string, detected time.Time, resolved *time.Time) {
		t.Helper()
		status := "down"
		if resolved != nil {
			status = "ok"
		}
		if _, err := pool.Exec(ctx,
			`INSERT INTO incident (public_id, tenant_id, project_id, detector, fingerprint, title, status, detected_at, resolved_at)
			 VALUES (gen_random_uuid(), $1, $2, 'availability', 1, $3, $4, $5, $6)`,
			marksTenant, marksProject, title, status, detected, resolved); err != nil {
			t.Fatalf("seed incident %q: %v", title, err)
		}
	}
	healed, gone := from.Add(time.Hour), from.Add(-4*time.Hour)
	incident("Checkout was down", from.Add(-2*time.Hour), &healed) // began before, ended inside
	incident("Long gone", from.Add(-5*time.Hour), &gone)           // over before the window
	incident("API is down", from.Add(10*time.Hour), nil)           // still open

	deploys, err := s.DeployMarks(ctx, marksTenant, marksProject, from, to)
	if err != nil {
		t.Fatalf("deploy marks: %v", err)
	}
	if len(deploys) != 2 ||
		!deploys[0].At.Equal(from.Add(time.Hour)) || deploys[0].Name != "deploy" || deploys[0].SHA != sha ||
		deploys[1].Name != "github_deployment_status_created" || deploys[1].SHA != "" {
		t.Fatalf("the sha'd deploy then the hook's, oldest first; not a sentence however its words are split, the earlier one or the sibling's; got %+v", deploys)
	}

	busy, err := s.DeployMarks(ctx, marksTenant, marksBusy, from, to)
	if err != nil {
		t.Fatalf("busy deploy marks: %v", err)
	}
	if len(busy) != marksCap || busy[0].Name != "deploy_5" || busy[marksCap-1].Name != "deploy_104" {
		t.Fatalf("the cap keeps the latest %d, oldest first; got %d from %+v", marksCap, len(busy), busy[0])
	}

	releases, err := s.ReleaseMarks(ctx, marksTenant, marksProject, from, to)
	if err != nil {
		t.Fatalf("release marks: %v", err)
	}
	if len(releases) != 3 ||
		releases[0].Name != "1.1.0" || !releases[0].At.Equal(from.Add(2*time.Hour)) ||
		releases[1].Name != sha || !releases[1].At.Equal(from.Add(6*time.Hour)) ||
		releases[2].Name != "2.0.0" {
		t.Fatalf("1.1.0 at its first sighting, the release label, then 2.0.0; never 1.0.0 or 0; got %+v", releases)
	}

	incidents, err := s.IncidentMarks(ctx, marksTenant, marksProject, from, to)
	if err != nil {
		t.Fatalf("incident marks: %v", err)
	}
	if len(incidents) != 2 ||
		incidents[0].Name != "Checkout was down" || incidents[0].Until == nil || !incidents[0].Until.Equal(healed) ||
		incidents[1].Name != "API is down" || incidents[1].Until != nil {
		t.Fatalf("the one that ended inside the window, then the open one with no until; got %+v", incidents)
	}
}
