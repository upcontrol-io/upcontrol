package incident

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	sqlc "go.upcontrol.io/back/gen/pg"
	"go.upcontrol.io/back/internal/deliver"
	"go.upcontrol.io/back/internal/migrate"
	"go.upcontrol.io/back/internal/storage/pg"
	"go.upcontrol.io/back/internal/targetkey"
)

func TestFingerprint_StableAndDistinct(t *testing.T) {
	// fingerprint(monitor, detector) must be stable across calls (so repeated
	// outages of the same monitor group) and distinct per detector.
	a := fingerprint(1, "availability")
	b := fingerprint(1, "availability")
	if a != b {
		t.Fatalf("fingerprint must be stable: %d != %d", a, b)
	}
	c := fingerprint(1, "latency")
	if a == c {
		t.Fatalf("fingerprint must differ by detector for the same monitor")
	}
	d := fingerprint(2, "availability")
	if a == d {
		t.Fatalf("fingerprint must differ by monitor for the same detector")
	}
}

func TestNewUUID_UniqueAndValid(t *testing.T) {
	seen := map[[16]byte]bool{}
	for i := 0; i < 1000; i++ {
		u := newUUID()
		if !u.Valid {
			t.Fatal("newUUID must set Valid=true")
		}
		// RFC 4122 v4: version nibble (byte[6] high) = 0x4, variant (byte[8] high) = 0x8/0x9/a/b.
		if u.Bytes[6]&0xf0 != 0x40 {
			t.Fatalf("uuid %x is not v4 (version nibble)", u.Bytes)
		}
		if u.Bytes[8]&0xc0 != 0x80 {
			t.Fatalf("uuid %x has wrong variant", u.Bytes)
		}
		if seen[u.Bytes] {
			t.Fatalf("uuid collision after %d draws", i)
		}
		seen[u.Bytes] = true
	}
}

func TestFreezeSlice_WithoutStoreIsANoOp(t *testing.T) {
	// The slice is evidence, not a precondition: an incident must still open
	// with no store wired (tests) and not error there.
	l := New(nil, nil)
	if err := l.freezeSlice(t.Context(), 1, 2, 3); err != nil {
		t.Fatalf("freezeSlice without a store must be a silent no-op, got: %v", err)
	}
}

func TestUUIDStr_IsDashlessLowercaseHex(t *testing.T) {
	// The public_id format is shared with every handler; a dashed or
	// upper-case rendering here would split one incident into two ids.
	id := newUUID()
	s := uuidStr(id)
	if len(s) != 32 {
		t.Fatalf("want 32 hex chars, got %d: %q", len(s), s)
	}
	for _, r := range s {
		isDigit := r >= '0' && r <= '9'
		isHexLetter := r >= 'a' && r <= 'f'
		if !isDigit && !isHexLetter {
			t.Fatalf("non lowercase-hex rune %q in %q", r, s)
		}
	}
}

// The detection alert crosses a queue: a misspelled key never fails, the
// field just never arrives, so the round trip is pinned here, not in an inbox.
func TestDetectAlertPayload_SurvivesTheRoundTrip(t *testing.T) {
	p := DetectOpen{
		Detector: "errorrate",
		Title:    "Error rate spike on shop.example",
		Summary:  "Error and fatal lines are 15.0% of the log stream in the last 5 minutes.",
		Fields:   []deliver.Field{{Label: "Error lines", Value: "30 of 200 lines"}},
	}
	slice := []sqlc.ListIncidentSliceRow{
		{Seq: 1, Message: "oldest"},
		{Seq: 2, Message: "newest"},
	}

	var got deliver.AlertPayload
	if err := json.Unmarshal(detectAlertPayload(p, "abc123", slice), &got); err != nil {
		t.Fatal(err)
	}
	if got.Title != p.Title || got.Summary != p.Summary || got.IncidentID != "abc123" {
		t.Errorf("title/summary/id lost in transit: %+v", got)
	}
	// Without this the mail's badge reads "Down" and telegram offers a Resolve
	// button the detector's own incidents refuse.
	if got.Detector != "errorrate" {
		t.Errorf("detector lost in transit: %+v", got)
	}
	// An error-rate spike is degradation, not an outage: the checks passed.
	if got.Status != "check" {
		t.Errorf("status = %q, want check", got.Status)
	}
	if len(got.Fields) != 1 || got.Fields[0].Label != "Error lines" {
		t.Errorf("fields lost in transit: %+v", got.Fields)
	}
	// Oldest first in, newest out: the alert quotes the line closest to the fire.
	if len(got.Lines) != 1 || got.Lines[0] != "newest" {
		t.Errorf("lines = %v, want the newest line only", got.Lines)
	}
	if got.LinesLabel == "" {
		t.Error("a code panel with no label is a block of text nobody can place")
	}

	// No slice, no section: a heading over nothing asserts a read that never
	// happened.
	var bare deliver.AlertPayload
	if err := json.Unmarshal(detectAlertPayload(p, "abc123", nil), &bare); err != nil {
		t.Fatal(err)
	}
	if len(bare.Lines) != 0 || bare.LinesLabel != "" {
		t.Errorf("an empty slice must draw no lines section: %+v", bare)
	}
}

// openTestDB applies the migrations and returns a pool, or skips. The
// migration runs under an advisory lock: `go test ./...` runs packages in
// parallel and two goose runs against one fresh database collide on the
// objects they are both creating.
func openTestDB(t *testing.T, why string) *pg.Pool {
	t.Helper()
	dsn := os.Getenv("UC_TEST_POSTGRES")
	if dsn == "" {
		t.Skip("UC_TEST_POSTGRES not set; skipping " + why)
	}
	ctx := context.Background()
	pool, err := pg.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	t.Cleanup(pool.Close)
	conn, err := pool.Raw().Acquire(ctx)
	if err != nil {
		t.Fatalf("lock connection: %v", err)
	}
	defer conn.Release()
	const migrateLock = 0x75636d67 // "ucmg"
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock($1)", migrateLock); err != nil {
		t.Fatalf("migration lock: %v", err)
	}
	defer func() { _, _ = conn.Exec(ctx, "SELECT pg_advisory_unlock($1)", migrateLock) }()
	if err := migrate.Run(ctx, dsn, "../../../db/postgres"); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}
	return pool
}

// Pins the timeline wording: the raw reason is storage detail and must not
// leak, and a close nobody measured never reads as a recovery.
// UC_TEST_POSTGRES unset = skip.
func TestClose_WordsTheTimeline(t *testing.T) {
	ctx := context.Background()
	pool := openTestDB(t, "close-wording test")

	var tenantID, projectID int64
	if err := pool.Raw().QueryRow(ctx,
		`INSERT INTO tenant (public_id, name) VALUES (gen_random_uuid(), $1) RETURNING id`,
		fmt.Sprintf("closetext-%d", time.Now().UnixNano())).Scan(&tenantID); err != nil {
		t.Fatalf("tenant: %v", err)
	}
	if err := pool.Raw().QueryRow(ctx,
		`INSERT INTO project (public_id, tenant_id, domain) VALUES (gen_random_uuid(), $1, '') RETURNING id`,
		tenantID).Scan(&projectID); err != nil {
		t.Fatalf("project: %v", err)
	}
	for reason, want := range map[string]string{
		ReasonMonitorDelete: "Monitor deleted",
		ReasonPlanPaused:    "Paused by the plan's check limit",
	} {
		// One target per reason: a project subscribes to a target once.
		url := "https://" + reason + ".shop.example.com"
		var monitorID int64
		if err := pool.Raw().QueryRow(ctx,
			`WITH pt AS (
			   INSERT INTO probe_target (key, kind, url) VALUES ($3, 'website', $4) ON CONFLICT (key) DO UPDATE SET url = EXCLUDED.url RETURNING id)
			 INSERT INTO monitor (public_id, tenant_id, project_id, kind, name, target, interval_sec, target_id)
			 SELECT gen_random_uuid(), $1, $2, 'http', 'Checkout', $4, 300, pt.id FROM pt
			 RETURNING id`,
			tenantID, projectID, targetkey.Website(url, ""), url).Scan(&monitorID); err != nil {
			t.Fatalf("monitor: %v", err)
		}

		l := New(pool, nil)
		incidentID, created, err := l.Open(ctx, monitorID, "Checkout is down", 0)
		if err != nil || !created {
			t.Fatalf("open incident: created=%v err=%v", created, err)
		}
		if err := l.Close(ctx, monitorID, reason); err != nil {
			t.Fatalf("close: %v", err)
		}

		var kind, text string
		if err := pool.Raw().QueryRow(ctx,
			`SELECT kind, text FROM incident_update WHERE incident_id = $1 ORDER BY id DESC LIMIT 1`,
			incidentID).Scan(&kind, &text); err != nil {
			t.Fatalf("read newest update: %v", err)
		}
		if kind != "resolved" || text != want {
			t.Fatalf("%s: newest update = kind %q text %q, want resolved / %q", reason, kind, text, want)
		}
	}
}

// A project's incident reaches that project's destinations and nobody else's:
// a sibling project of the same workspace shares a tenant and shares nothing
// else. UC_TEST_POSTGRES unset = skip.
func TestOpen_NotifiesOnlyTheIncidentsProject(t *testing.T) {
	ctx := context.Background()
	pool := openTestDB(t, "fan-out scope test")

	var tenantID int64
	if err := pool.Raw().QueryRow(ctx,
		`INSERT INTO tenant (public_id, name) VALUES (gen_random_uuid(), $1) RETURNING id`,
		fmt.Sprintf("fanout-%d", time.Now().UnixNano())).Scan(&tenantID); err != nil {
		t.Fatalf("tenant: %v", err)
	}
	project := func(domain string) int64 {
		var id int64
		if err := pool.Raw().QueryRow(ctx,
			`INSERT INTO project (public_id, tenant_id, domain) VALUES (gen_random_uuid(), $1, $2) RETURNING id`,
			tenantID, domain).Scan(&id); err != nil {
			t.Fatalf("project %s: %v", domain, err)
		}
		return id
	}
	channel := func(projectID int64, target string) int64 {
		var id int64
		if err := pool.Raw().QueryRow(ctx,
			`INSERT INTO alert_channel (public_id, tenant_id, project_id, kind, target)
			 VALUES (gen_random_uuid(), $1, $2, 'email', $3) RETURNING id`,
			tenantID, projectID, target).Scan(&id); err != nil {
			t.Fatalf("channel %s: %v", target, err)
		}
		return id
	}
	mineProject, theirsProject := project("mine.example"), project("theirs.example")
	mineChannel, theirsChannel := channel(mineProject, "mine@example.com"), channel(theirsProject, "theirs@example.com")

	var monitorID int64
	if err := pool.Raw().QueryRow(ctx,
		`WITH pt AS (
		   INSERT INTO probe_target (key, kind, url) VALUES ($3, 'website', 'https://mine.example') ON CONFLICT (key) DO UPDATE SET url = EXCLUDED.url RETURNING id)
		 INSERT INTO monitor (public_id, tenant_id, project_id, kind, name, target, interval_sec, target_id)
		 SELECT gen_random_uuid(), $1, $2, 'http', 'Checkout', 'https://mine.example', 300, pt.id FROM pt
		 RETURNING id`,
		tenantID, mineProject, targetkey.Website("https://mine.example", "")).Scan(&monitorID); err != nil {
		t.Fatalf("monitor: %v", err)
	}

	incidentID, created, err := New(pool, nil).Open(ctx, monitorID, "mine.example is down", 0)
	if err != nil || !created {
		t.Fatalf("open incident: created=%v err=%v", created, err)
	}

	var channels []int64
	rows, err := pool.Raw().Query(ctx,
		`SELECT channel_id FROM delivery_queue WHERE incident_id = $1 ORDER BY channel_id`, incidentID)
	if err != nil {
		t.Fatalf("read the queue: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan queue row: %v", err)
		}
		channels = append(channels, id)
	}
	if len(channels) != 1 || channels[0] != mineChannel {
		t.Fatalf("queued for channels %v, want exactly the incident's project channel %d (the sibling's is %d)",
			channels, mineChannel, theirsChannel)
	}
}
