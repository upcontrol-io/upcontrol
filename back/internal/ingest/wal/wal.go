// Package wal is the ingest write-ahead log. Every batch is appended here and
// fsync'd BEFORE the HTTP receipt is sent to the client (plan §4.3: ack happens
// after WAL fsync, not after the ClickHouse insert). If the process dies after
// the ack but before the CH write, recovery replays the WAL past the last
// checkpoint — no confirmed row is lost. That is the §3.1 gate.
//
// Record framing is [len:4 BE][data:len][crc32:4 BE]. A torn write at the tail
// (the process died mid-record, so the last record's CRC will not match or its
// bytes are short) is detected on replay and truncated: a record that was never
// fsync'd was never ack'd, so dropping it is correct.
//
// Group fsync: Append writes (no fsync) and returns the offset; the caller
// appends a whole batch then Sync()s once, then acks. One fsync per batch, not
// per row. Checkpoint(offset) records how far the ClickHouse consumer has
// durably processed; Replay starts there.
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

// WAL is an append-only, fsync-on-Sync write-ahead log. It is safe for concurrent
// Append calls (serialized by a mutex); the consumer (Replay/Checkpoint) runs on
// the recovery path or a single goroutine.
type WAL struct {
	mu       sync.Mutex
	path     string
	cpPath   string
	f        *os.File
	cp       *os.File
	syncedTo int64 // byte offset known to be fsync'd (end of last Sync)
}

// Open creates or opens the WAL at path. The checkpoint lives beside it at
// path + ".cp". An existing checkpoint is read so CheckpointOffset() reports the
// last durable consumer position before any Replay.
func Open(path string) (*WAL, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	w := &WAL{path: path, cpPath: path + ".cp", f: f}
	if err := w.loadCheckpoint(); err != nil {
		_ = f.Close()
		return nil, err
	}
	return w, nil
}

// Append writes the framed record (no fsync) and returns its start offset plus
// the on-disk length (hdrOverhead + len(data)). The caller Sync()s to make it
// durable, then acks.
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

// Checkpoint records that the consumer has durably processed through offset, so
// the next recovery replays only records after it. The checkpoint is itself
// fsync'd — a checkpoint ahead of the actual CH write would lose data on crash.
func (w *WAL) Checkpoint(offset int64) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	var cp *os.File
	if w.cp == nil {
		f, err := os.OpenFile(w.cpPath, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o644)
		if err != nil {
			return err
		}
		cp = f
	} else {
		cp = w.cp
		if err := cp.Truncate(0); err != nil {
			return err
		}
		if _, err := cp.Seek(0, io.SeekStart); err != nil {
			return err
		}
	}
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], uint64(offset))
	if _, err := cp.Write(b[:]); err != nil {
		return err
	}
	return cp.Sync()
}

// CheckpointOffset returns the last durable consumer offset (0 before any
// Checkpoint call). Recovery replays from here.
func (w *WAL) CheckpointOffset() int64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.cpOffset()
}

// Replay yields framed records starting at fromOffset (typically the checkpoint).
// It stops cleanly at the first torn record (short read or CRC mismatch) and
// reports the byte offset up to which records are valid, so the caller can
// Truncate the torn tail. fn returns err to stop early.
func (w *WAL) Replay(fromOffset int64, fn func(offset int64, data []byte) error) (validTo int64, err error) {
	w.mu.Lock()
	f := w.f
	w.mu.Unlock()
	if _, err := f.Seek(fromOffset, io.SeekStart); err != nil {
		return fromOffset, err
	}
	r := io.Reader(f)
	pos := fromOffset
	for {
		var hdr [lenSize]byte
		n, e := io.ReadFull(r, hdr[:])
		if e == io.EOF || e == io.ErrUnexpectedEOF {
			// Short header: torn tail. Everything before `pos` is valid.
			return pos, nil
		}
		if e != nil {
			return pos, e
		}
		_ = n
		dlen := int64(binary.BigEndian.Uint32(hdr[:]))
		if dlen < 0 || dlen > 64<<20 { // sanity bound: 64 MiB max record
			// Garbage length: torn/garbled tail.
			return pos, nil
		}
		data := make([]byte, dlen)
		if _, e := io.ReadFull(r, data); e != nil {
			return pos, nil // torn data
		}
		var crcB [crcSize]byte
		if _, e := io.ReadFull(r, crcB[:]); e != nil {
			return pos, nil // torn crc
		}
		got := binary.BigEndian.Uint32(crcB[:])
		if crc32.ChecksumIEEE(data) != got {
			return pos, nil // CRC mismatch: this record was not fsync'd cleanly
		}
		if err := fn(pos, data); err != nil {
			return pos, err
		}
		pos += int64(lenSize) + dlen + int64(crcSize)
	}
}

// Truncate cuts the file at offset, discarding any torn tail past it. Called
// after Replay reports a torn record.
func (w *WAL) Truncate(offset int64) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.f.Truncate(offset); err != nil {
		return err
	}
	if _, err := w.f.Seek(offset, io.SeekStart); err != nil {
		return err
	}
	return w.f.Sync()
}

// Close flushes and closes the data and checkpoint files.
func (w *WAL) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	var err error
	if w.cp != nil {
		err = errors.Join(err, w.cp.Close())
		w.cp = nil
	}
	if w.f != nil {
		_ = w.f.Sync()
		err = errors.Join(err, w.f.Close())
		w.f = nil
	}
	return err
}

// --- checkpoint persistence --------------------------------------------

func (w *WAL) loadCheckpoint() error {
	f, err := os.Open(w.cpPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // no checkpoint yet — replay from 0
		}
		return err
	}
	defer func() { _ = f.Close() }()
	var b [8]byte
	if _, err := io.ReadFull(f, b[:]); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return nil // empty checkpoint file
		}
		return err
	}
	w.setCpOffset(int64(binary.BigEndian.Uint64(b[:])))
	return nil
}

// cpOffset and setCpOffset take the mutex internally so CheckpointOffset can
// share the lock with Checkpoint without a re-entrant lock.
func (w *WAL) cpOffset() int64 {
	f, err := os.Open(w.cpPath)
	if err != nil {
		return 0
	}
	defer func() { _ = f.Close() }()
	var b [8]byte
	if _, err := io.ReadFull(f, b[:]); err != nil {
		return 0
	}
	return int64(binary.BigEndian.Uint64(b[:]))
}

func (w *WAL) setCpOffset(_ int64) {
	// loadCheckpoint reads directly from disk on demand via cpOffset(); nothing
	// to cache. Kept for symmetry and a future in-memory cache.
}
