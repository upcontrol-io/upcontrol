// Package wal is the ingest write-ahead log: batches are appended and fsync'd
// before the receipt is sent.
package wal

import (
	"encoding/binary"
	"errors"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"sync"
)

// Magic prefixes every framed record so a seek into garbage (or a different
// file) is detected rather than misread.
const (
	magic       uint32 = 0x55434F4E // "UCON"
	lenSize            = 4          // uint32 length, big-endian
	crcSize            = 4          // uint32 crc32, big-endian
	hdrOverhead        = lenSize + crcSize
)

// WAL is an append-only, fsync-on-Sync write-ahead log; Append is serialized
// by a mutex.
type WAL struct {
	mu       sync.Mutex
	path     string
	f        *os.File
	syncedTo int64 // byte offset known to be fsync'd (end of last Sync)
}

// Open creates or opens the WAL at path.
func Open(path string) (*WAL, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	// No O_APPEND: one WAL file has one owning process, and Seek+Write keeps
	// behaviour identical on Windows (O_APPEND maps to FILE_APPEND_DATA).
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, err
	}
	return &WAL{path: path, f: f}, nil
}

// Append writes the framed record (no fsync) and returns its offset and
// on-disk length; the caller Sync()s to make it durable, then acks.
func (w *WAL) Append(data []byte) (offset, length int64, err error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	off, err := w.f.Seek(0, io.SeekEnd)
	if err != nil {
		return 0, 0, err
	}
	buf := make([]byte, hdrOverhead+len(data))
	binary.BigEndian.PutUint32(buf[0:lenSize], uint32(len(data)))
	copy(buf[lenSize:lenSize+len(data)], data)
	binary.BigEndian.PutUint32(buf[lenSize+len(data):], crc32.ChecksumIEEE(data))
	if _, err := w.f.Write(buf); err != nil {
		return 0, 0, err
	}
	return off, int64(len(buf)), nil
}

// Sync fsyncs the data file so every Append so far is durable. Returns the byte
// offset that is now safe to ack up to.
func (w *WAL) Sync() (int64, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.f.Sync(); err != nil {
		return 0, err
	}
	off, err := w.f.Seek(0, io.SeekEnd)
	if err != nil {
		return 0, err
	}
	w.syncedTo = off
	return off, nil
}

// Close flushes and closes the data file.
func (w *WAL) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	var err error
	if w.f != nil {
		_ = w.f.Sync()
		err = errors.Join(err, w.f.Close())
		w.f = nil
	}
	return err
}
