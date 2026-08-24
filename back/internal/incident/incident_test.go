package incident

import (
	"encoding/json"
	"testing"

	sqlc "go.upcontrol.io/back/gen/pg"
	"go.upcontrol.io/back/internal/deliver"
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
	// The slice is evidence, not a precondition: an incident must still open when
	// there is no ClickHouse to read the window from (tests, a degraded node),
	// and it must not error on that path — the alert matters more than the card.
	l := New(nil, nil)
	if err := l.freezeSlice(t.Context(), 1, 2, 3); err != nil {
		t.Fatalf("freezeSlice without ClickHouse must be a silent no-op, got: %v", err)
	}
}

func TestUUIDStr_IsDashlessLowercaseHex(t *testing.T) {
	// The public_id format is shared with every other handler (uuidStr in
	// internal/api renders the same way); a dashed or upper-case rendering here
	// would make the same incident look like two different ids across responses.
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

// The detection alert crosses a queue: this side writes a JSON map, the
// delivery side reads deliver.AlertPayload by struct tag. Nothing fails when a
// key is misspelled — the field just never arrives — so the round trip is
// pinned here rather than discovered in somebody's inbox.
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
