package ingest

import "testing"

func TestFingerprintGroupsShapeNotText(t *testing.T) {
	if Fingerprint("user 42 not found") != Fingerprint("user 7 not found") {
		t.Error("integers must not split the group")
	}
	if Fingerprint("req 550e8400-e29b-41d4-a716-446655440000 failed") !=
		Fingerprint("req 123e4567-e89b-12d3-a456-426614174000 failed") {
		t.Error("two UUIDs must land in one group")
	}
	if Fingerprint(`file "a.txt" missing`) != Fingerprint(`file "b.txt" missing`) {
		t.Error("quoted strings must not split the group")
	}
	if Fingerprint("payment failed") == Fingerprint("payment succeeded") {
		t.Error("different texts must fingerprint differently")
	}
	if Fingerprint("") != 0 {
		t.Error("an empty message fingerprints to 0 (zero is silence)")
	}
}

func TestMaskLeavesDictionaryWords(t *testing.T) {
	if got := mask("added 3 items to cart"); got != "added # items to cart" {
		t.Errorf("mask = %q, want %q", got, "added # items to cart")
	}
}
