package wal

import (
	"encoding/binary"
	"hash/crc32"
	"os"
	"path/filepath"
	"testing"
)

// Append frames records as len + data + crc and Sync reports the durable end.
func TestAppendFramesRecords(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ingest.wal")
	w, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	var end int64
	for _, p := range []string{"alpha", "beta"} {
		off, n, err := w.Append([]byte(p))
		if err != nil {
			t.Fatalf("Append: %v", err)
		}
		if off != end || n != int64(hdrOverhead+len(p)) {
			t.Fatalf("Append = (%d,%d), want (%d,%d)", off, n, end, hdrOverhead+len(p))
		}
		end += n
	}
	if off, err := w.Sync(); err != nil || off != end {
		t.Fatalf("Sync = (%d,%v), want (%d,nil)", off, err, end)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	for _, p := range []string{"alpha", "beta"} {
		dlen := int(binary.BigEndian.Uint32(raw[:lenSize]))
		if dlen != len(p) || string(raw[lenSize:lenSize+dlen]) != p {
			t.Fatalf("frame data mismatch for %q", p)
		}
		got := binary.BigEndian.Uint32(raw[lenSize+dlen : hdrOverhead+dlen])
		if got != crc32.ChecksumIEEE([]byte(p)) {
			t.Fatalf("frame crc mismatch for %q", p)
		}
		raw = raw[hdrOverhead+dlen:]
	}
}
