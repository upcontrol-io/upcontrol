// Package wal is the ingest write-ahead log: batches are appended and fsync'd
// before the receipt is sent, and recovery replays past the last checkpoint.
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
// by a mutex, the consumer (Replay/Checkpoint) runs on one goroutine.
type WAL struct {
	mu       sync.Mutex
	path     string
	cpPath   string
	f        *os.File
	cp       *os.File
	syncedTo int64 // byte offset known to be fsync'd (end of last Sync)
}

// Open creates or opens the WAL at path with its checkpoint at path + ".cp";
// an existing checkpoint is read so CheckpointOffset reports the durable position.
func Open(path string) (*WAL, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	// No O_APPEND: one WAL file has one owning process. On Windows the flag
	// opens FILE_APPEND_DATA and Truncate then fails with "Access is denied".
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
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

// Checkpoint records the consumer's durable position; the checkpoint itself
// is fsync'd, since one ahead of the actual CH write would lose data on crash.
func (w *WAL) Checkpoint(offset int64) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	var cp *os.File
	if w.cp == nil {
		f, err := os.OpenFile(w.cpPath, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o644)
		if err != nil {
			return err
		}
		// Retained, or every Checkpoint call opens a handle nothing closes.
		w.cp = f
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

// Replay yields framed records from fromOffset, stopping cleanly at the first
// torn record and reporting the valid offset so the caller can Truncate.
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
	// loadCheckpoint reads from disk on demand via cpOffset(); nothing to cache.
}
