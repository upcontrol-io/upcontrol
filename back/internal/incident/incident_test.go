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

func TestFreezeSlice_WithoutClickHouseIsANoOp(t *testing.T) {
	// The slice is evidence, not a precondition: an incident must still open
	// with no ClickHouse (tests, a degraded node) and not error there.
	l := New(nil, nil)
	if err := l.freezeSlice(t.Context(), 1, 2, 3); err != nil {
		t.Fatalf("freezeSlice without ClickHouse must be a silent no-op, got: %v", err)
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

// Pins the timeline wording: the raw reason is storage detail and must not
// leak. UC_TEST_POSTGRES unset = skip.
func TestClose_MonitorDeleteWordsTheTimeline(t *testing.T) {
	dsn := os.Getenv("UC_TEST_POSTGRES")
	if dsn == "" {
		t.Skip("UC_TEST_POSTGRES not set; skipping close-wording test")
	}
	ctx := context.Background()
	if err := migrate.Run(ctx, dsn, "../../../db/postgres"); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}
	pool, err := pg.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	t.Cleanup(pool.Close)

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
	var monitorID int64
	if err := pool.Raw().QueryRow(ctx,
		`INSERT INTO monitor (public_id, tenant_id, project_id, kind, name, target, interval_sec)
		 VALUES (gen_random_uuid(), $1, $2, 'http', 'Checkout', 'https://shop.example.com', 300)
		 RETURNING id`, tenantID, projectID).Scan(&monitorID); err != nil {
		t.Fatalf("monitor: %v", err)
	}

	l := New(pool, nil)
	incidentID, created, err := l.Open(ctx, monitorID, "Checkout is down")
	if err != nil || !created {
		t.Fatalf("open incident: created=%v err=%v", created, err)
	}
	if err := l.Close(ctx, monitorID, ReasonMonitorDelete); err != nil {
		t.Fatalf("close: %v", err)
	}

	var kind, text string
	if err := pool.Raw().QueryRow(ctx,
		`SELECT kind, text FROM incident_update WHERE incident_id = $1 ORDER BY id DESC LIMIT 1`,
		incidentID).Scan(&kind, &text); err != nil {
		t.Fatalf("read newest update: %v", err)
	}
	if kind != "resolved" || text != "Monitor deleted" {
		t.Fatalf("newest update = kind %q text %q, want resolved / \"Monitor deleted\"", kind, text)
	}
}
