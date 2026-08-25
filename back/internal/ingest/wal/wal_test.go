package wal

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func newWAL(t *testing.T) (*WAL, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "ingest.wal")
	w, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = w.Close() })
	return w, path
}

// TestRoundTrip: records appended and fsync'd are all recoverable.
func TestRoundTrip(t *testing.T) {
	w, _ := newWAL(t)
	for _, payload := range []string{"alpha", "beta", "gamma"} {
		if _, _, err := w.Append([]byte(payload)); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
	if _, err := w.Sync(); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	var got []string
	validTo, err := w.Replay(0, func(_ int64, data []byte) error {
		got = append(got, string(data))
		return nil
	})
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if len(got) != 3 || got[0] != "alpha" || got[2] != "gamma" {
		t.Errorf("recovered %v, want [alpha beta gamma]", got)
	}
	if validTo <= 0 {
		t.Errorf("validTo = %d, want > 0", validTo)
	}
}

// TestCheckpointSkipsConsumed: after a checkpoint, Replay starts past it.
func TestCheckpointSkipsConsumed(t *testing.T) {
	w, _ := newWAL(t)
	off1, _, _ := w.Append([]byte("first"))
	w.Sync()
	w.Append([]byte("second"))
	cp, _ := w.Sync()
	if err := w.Checkpoint(cp); err != nil {
		t.Fatal(err)
	}
	_ = off1
	// The handle must be retained, or each call leaks one: Close only ever
	// closes w.cp; on Windows the leak surfaces as TempDir cleanup failing.
	if w.cp == nil {
		t.Error("Checkpoint left w.cp nil — the .cp handle is leaked, one per call")
	}
	var got []string
	w.Replay(w.CheckpointOffset(), func(_ int64, data []byte) error {
		got = append(got, string(data))
		return nil
	})
	// Checkpoint was at the end of "second", so nothing should replay.
	if len(got) != 0 {
		t.Errorf("replayed %v past checkpoint, want none", got)
	}
}

// A confirmed batch survives a crash that tears the in-flight record's tail:
// three fsync'd records recover, the bad-CRC fourth is truncated away.
func TestTornTailRecoveredAndTruncated(t *testing.T) {
	w, path := newWAL(t)
	for _, p := range []string{"r1", "r2", "r3"} {
		w.Append([]byte(p))
	}
	endConfirmed, _ := w.Sync()
	// Simulate a torn tail: plausible header + data but a wrong CRC, written
	// directly to the file WITHOUT fsync, as if the process died mid-write.
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	// A "record" with a header claiming 5 data bytes, the bytes, and a bogus CRC.
	garbage := []byte{0, 0, 0, 5, 'x', 'x', 'x', 'x', 'x', 0xDE, 0xAD, 0xBE, 0xEF}
	if _, err := f.Write(garbage); err != nil {
		t.Fatal(err)
	}
	f.Close() // no fsync — the torn tail is on disk only by chance; CRC still fails

	w2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer w2.Close()
	var got []string
	validTo, err := w2.Replay(0, func(_ int64, data []byte) error {
		got = append(got, string(data))
		return nil
	})
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if len(got) != 3 || got[0] != "r1" || got[2] != "r3" {
		t.Fatalf("recovered %v — confirmed records were lost (§3.1 gate)", got)
	}
	if validTo != endConfirmed {
		t.Errorf("validTo = %d, want end of confirmed %d (torn tail not bounded)", validTo, endConfirmed)
	}
	// Truncate the torn tail; a second replay is clean and still has the three.
	if err := w2.Truncate(validTo); err != nil {
		t.Fatal(err)
	}
	got = nil
	w2.Replay(0, func(_ int64, data []byte) error { got = append(got, string(data)); return nil })
	if len(got) != 3 {
		t.Errorf("post-truncate replay got %v, want 3", got)
	}
}

// TestCRCMismatchStopsReplay: a record whose CRC does not match its data is the
// boundary of valid data.
func TestCRCMismatchStopsReplay(t *testing.T) {
	w, path := newWAL(t)
	w.Append([]byte("good"))
	w.Sync()
	w.Close()
	// Flip a byte in the data region of the good record.
	f, _ := os.OpenFile(path, os.O_RDWR, 0o644)
	buf := make([]byte, 32)
	n, _ := f.Read(buf)
	// Flip the last data byte (index 4+len-1; here data is "good" so byte 7).
	if n >= 8 {
		buf[7] ^= 0xFF
	}
	f.Seek(0, 0)
	f.Write(buf[:n])
	f.Close()

	w2, _ := Open(path)
	defer w2.Close()
	var got []string
	validTo, _ := w2.Replay(0, func(_ int64, data []byte) error {
		got = append(got, string(data))
		return nil
	})
	// The corrupted record must not be emitted; validTo is at its start.
	if len(got) != 0 {
		t.Errorf("emitted corrupted record %v, want none", got)
	}
	if validTo != 0 {
		t.Errorf("validTo = %d, want 0 (nothing valid before corruption)", validTo)
	}
}

// TestEarlyStop: returning an error from the callback halts replay.
func TestEarlyStop(t *testing.T) {
	w, _ := newWAL(t)
	w.Append([]byte("a"))
	w.Append([]byte("b"))
	w.Sync()
	stop := errors.New("stop")
	var n int
	w.Replay(0, func(_ int64, data []byte) error {
		n++
		if bytes.Equal(data, []byte("a")) {
			return stop
		}
		return nil
	})
	if n != 1 {
		t.Errorf("visited %d records, want 1 (early stop)", n)
	}
}
