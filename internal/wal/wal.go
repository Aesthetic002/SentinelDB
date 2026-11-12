// Package wal is the durable storage behind Raft: an append-only, checksummed log
// plus a checksummed snapshot file.
//
// Record format (little endian):
//
//	length      uint32  bytes in payload
//	headerCRC   uint32  CRC-32C of the 4 length bytes
//	payloadCRC  uint32  CRC-32C of the payload
//	payload     [length]byte, whose first byte is the record type
//
// Length has its own checksum because a flipped bit there would otherwise look like
// a record running past end of file, indistinguishable from a torn write.
//
// A crash can only damage the end of the file, so damage followed by no further valid
// record is a torn write: it is cut off and the node boots (the lost suffix was never
// acknowledged, since Append fsyncs before returning). Damage with a valid record
// after it cannot be explained by a crash. That is corruption, and the node refuses
// to start, because silently losing an acknowledged entry could lose a committed write.
package wal

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"

	"sentineldb/internal/raft"
)

// ErrCorrupt reports stored data that failed verification and cannot be explained by
// a crash.
var ErrCorrupt = errors.New("wal: stored data is corrupt")

const (
	headerSize   = 12
	logFile      = "raft.wal"
	snapFile     = "raft.snap"
	maxRecordLen = 1 << 30

	recHardState byte = 1
	recEntry     byte = 2
	recTruncate  byte = 3
)

var castagnoli = crc32.MakeTable(crc32.Castagnoli)

// Options tunes a WAL.
type Options struct {
	// NoSync skips fsync. Only tests and throwaway benchmarks may set it: without
	// fsync an acknowledged write can be lost in a power failure, which breaks Raft.
	NoSync bool
}

// WAL implements raft.Storage on the local filesystem. It is not safe for concurrent
// use; the Raft driver goroutine is its only caller.
type WAL struct {
	dir  string
	opts Options
	f    *os.File
	hard raft.HardState
}

var _ raft.Storage = (*WAL)(nil)

// Open recovers state from dir, creating it if necessary. It repairs a torn tail and
// returns ErrCorrupt (wrapped) if verification fails.
func Open(dir string, opts Options) (*WAL, raft.InitialState, error) {
	var init raft.InitialState
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, init, err
	}
	w := &WAL{dir: dir, opts: opts}

	snap, err := loadSnapshot(filepath.Join(dir, snapFile))
	if err != nil {
		return nil, init, err
	}
	init.Snapshot = snap

	path := filepath.Join(dir, logFile)
	hard, entries, goodLen, err := replay(path)
	if err != nil {
		return nil, init, err
	}
	w.hard = hard
	init.Hard = hard

	// A crash between writing a snapshot and rewriting the log leaves entries the
	// snapshot supersedes. Raft §7 says what to do: if the log holds the snapshot's
	// last entry, keep what follows it; otherwise the whole log is discarded.
	stale := false
	if snap.Index > 0 {
		kept := entries[:0:0]
		found := false
		for _, e := range entries {
			if e.Index == snap.Index && e.Term == snap.Term {
				found = true
			}
			if e.Index > snap.Index {
				kept = append(kept, e)
			}
		}
		if len(entries) > 0 && entries[0].Index <= snap.Index {
			stale = true
			if !found {
				kept = nil
			}
		} else if len(entries) > 0 && entries[0].Index != snap.Index+1 {
			return nil, init, fmt.Errorf("%w: log starts at %d but snapshot ends at %d",
				ErrCorrupt, entries[0].Index, snap.Index)
		}
		entries = kept
	}
	init.Entries = entries

	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, init, err
	}
	if err := f.Truncate(goodLen); err != nil {
		f.Close()
		return nil, init, err
	}
	if _, err := f.Seek(goodLen, io.SeekStart); err != nil {
		f.Close()
		return nil, init, err
	}
	w.f = f
	if stale {
		if err := w.rewrite(entries); err != nil {
			f.Close()
			return nil, init, err
		}
	}
	return w, init, nil
}

// Close releases the log file.
func (w *WAL) Close() error { return w.f.Close() }

func frame(buf []byte, payload []byte) []byte {
	var h [headerSize]byte
	binary.LittleEndian.PutUint32(h[0:], uint32(len(payload)))
	binary.LittleEndian.PutUint32(h[4:], crc32.Checksum(h[0:4], castagnoli))
	binary.LittleEndian.PutUint32(h[8:], crc32.Checksum(payload, castagnoli))
	buf = append(buf, h[:]...)
	return append(buf, payload...)
}

func encodeHard(h raft.HardState) []byte {
	p := make([]byte, 0, 16+len(h.Vote))
	p = append(p, recHardState)
	p = binary.LittleEndian.AppendUint64(p, h.Term)
	return append(p, h.Vote...)
}

func encodeEntry(e raft.Entry) []byte {
	p := make([]byte, 0, 18+len(e.Data))
	p = append(p, recEntry)
	p = binary.LittleEndian.AppendUint64(p, e.Index)
	p = binary.LittleEndian.AppendUint64(p, e.Term)
	p = append(p, byte(e.Kind))
	return append(p, e.Data...)
}

func (w *WAL) writeSync(b []byte) error {
	if _, err := w.f.Write(b); err != nil {
		return err
	}
	if w.opts.NoSync {
		return nil
	}
	return w.f.Sync()
}

// SaveHardState implements raft.Storage.
func (w *WAL) SaveHardState(h raft.HardState) error {
	w.hard = h
	return w.writeSync(frame(nil, encodeHard(h)))
}

// Append implements raft.Storage. All entries share one write and one fsync.
func (w *WAL) Append(es []raft.Entry) error {
	if len(es) == 0 {
		return nil
	}
	var buf []byte
	for _, e := range es {
		buf = frame(buf, encodeEntry(e))
	}
	return w.writeSync(buf)
}

// TruncateFrom implements raft.Storage by recording the truncation.
func (w *WAL) TruncateFrom(index uint64) error {
	p := make([]byte, 0, 9)
	p = append(p, recTruncate)
	p = binary.LittleEndian.AppendUint64(p, index)
	return w.writeSync(frame(nil, p))
}

// SaveSnapshot implements raft.Storage. The snapshot becomes durable first, so a crash
// at any point leaves a state Open can recover; the log is then rewritten without the
// covered prefix, which is what bounds its size.
func (w *WAL) SaveSnapshot(s raft.Snapshot, keep []raft.Entry) error {
	if err := writeSnapshot(filepath.Join(w.dir, snapFile), s, w.opts.NoSync); err != nil {
		return err
	}
	return w.rewrite(keep)
}

// rewrite atomically replaces the log with the current hard state and the given entries.
func (w *WAL) rewrite(keep []raft.Entry) error {
	path := filepath.Join(w.dir, logFile)
	buf := frame(nil, encodeHard(w.hard))
	for _, e := range keep {
		buf = frame(buf, encodeEntry(e))
	}
	// Windows refuses to replace an open file, so the handle must close first. If the
	// write then fails, the caller (Raft) halts the node rather than carrying on.
	if err := w.f.Close(); err != nil {
		return err
	}
	if err := atomicWrite(path, buf, w.opts.NoSync); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Seek(0, io.SeekEnd); err != nil {
		f.Close()
		return err
	}
	w.f = f
	return nil
}

func replay(path string) (raft.HardState, []raft.Entry, int64, error) {
	var hard raft.HardState
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return hard, nil, 0, nil
	}
	if err != nil {
		return hard, nil, 0, err
	}

	var entries []raft.Entry
	off := 0
	for off < len(data) {
		payload, next, ok := readRecord(data, off)
		if !ok {
			if resyncable(data, off+1) {
				return hard, nil, 0, fmt.Errorf("%w: bad record at byte %d of %s with valid records after it",
					ErrCorrupt, off, path)
			}
			break // torn tail: nothing valid follows, so a crash explains the damage
		}
		if len(payload) == 0 {
			return hard, nil, 0, fmt.Errorf("%w: empty record at byte %d of %s", ErrCorrupt, off, path)
		}
		body := payload[1:]
		switch payload[0] {
		case recHardState:
			if len(body) < 8 {
				return hard, nil, 0, fmt.Errorf("%w: short hard state at byte %d", ErrCorrupt, off)
			}
			hard.Term = binary.LittleEndian.Uint64(body)
			hard.Vote = raft.NodeID(body[8:])
		case recEntry:
			if len(body) < 17 {
				return hard, nil, 0, fmt.Errorf("%w: short entry at byte %d", ErrCorrupt, off)
			}
			e := raft.Entry{
				Index: binary.LittleEndian.Uint64(body),
				Term:  binary.LittleEndian.Uint64(body[8:]),
				Kind:  raft.EntryKind(body[16]),
				Data:  append([]byte(nil), body[17:]...),
			}
			if n := len(entries); n > 0 && e.Index != entries[n-1].Index+1 {
				return hard, nil, 0, fmt.Errorf("%w: entry %d follows entry %d", ErrCorrupt, e.Index, entries[n-1].Index)
			}
			entries = append(entries, e)
		case recTruncate:
			if len(body) != 8 {
				return hard, nil, 0, fmt.Errorf("%w: short truncation at byte %d", ErrCorrupt, off)
			}
			from := binary.LittleEndian.Uint64(body)
			for len(entries) > 0 && entries[len(entries)-1].Index >= from {
				entries = entries[:len(entries)-1]
			}
		default:
			return hard, nil, 0, fmt.Errorf("%w: unknown record type %d at byte %d", ErrCorrupt, payload[0], off)
		}
		off = next
	}
	return hard, entries, int64(off), nil
}

// readRecord validates the record at off.
func readRecord(data []byte, off int) (payload []byte, next int, ok bool) {
	if len(data)-off < headerSize {
		return nil, 0, false
	}
	h := data[off : off+headerSize]
	if crc32.Checksum(h[0:4], castagnoli) != binary.LittleEndian.Uint32(h[4:]) {
		return nil, 0, false
	}
	n := binary.LittleEndian.Uint32(h)
	if n > maxRecordLen || len(data)-off-headerSize < int(n) {
		return nil, 0, false
	}
	p := data[off+headerSize : off+headerSize+int(n)]
	if crc32.Checksum(p, castagnoli) != binary.LittleEndian.Uint32(h[8:]) {
		return nil, 0, false
	}
	return p, off + headerSize + int(n), true
}

// resyncable reports whether any valid record begins at or after off.
func resyncable(data []byte, off int) bool {
	for ; off+headerSize <= len(data); off++ {
		if _, _, ok := readRecord(data, off); ok {
			return true
		}
	}
	return false
}

// --- snapshot file ---------------------------------------------------------

type snapHeader struct {
	Index   uint64
	Term    uint64
	Members raft.Membership
}

func writeSnapshot(path string, s raft.Snapshot, noSync bool) error {
	meta, err := json.Marshal(snapHeader{s.Index, s.Term, s.Members})
	if err != nil {
		return err
	}
	var payload []byte
	payload = binary.LittleEndian.AppendUint32(payload, uint32(len(meta)))
	payload = append(payload, meta...)
	payload = append(payload, s.Data...)
	return atomicWrite(path, frame(nil, payload), noSync)
}

func loadSnapshot(path string) (raft.Snapshot, error) {
	var s raft.Snapshot
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return s, err
	}
	// Snapshots replace their file atomically, so unlike the log they cannot be torn:
	// any damage at all is corruption.
	payload, next, ok := readRecord(data, 0)
	if !ok || next != len(data) || len(payload) < 4 {
		return s, fmt.Errorf("%w: snapshot %s failed its checksum", ErrCorrupt, path)
	}
	metaLen := binary.LittleEndian.Uint32(payload)
	if int(metaLen) > len(payload)-4 {
		return s, fmt.Errorf("%w: snapshot %s has a bad header", ErrCorrupt, path)
	}
	var h snapHeader
	if err := json.Unmarshal(payload[4:4+metaLen], &h); err != nil {
		return s, fmt.Errorf("%w: snapshot %s: %v", ErrCorrupt, path, err)
	}
	return raft.Snapshot{
		Index: h.Index, Term: h.Term, Members: h.Members,
		Data: append([]byte(nil), payload[4+metaLen:]...),
	}, nil
}

// atomicWrite replaces path so that a crash leaves either the old or the new content.
func atomicWrite(path string, b []byte, noSync bool) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if !noSync {
		if err := f.Sync(); err != nil {
			f.Close()
			return err
		}
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	if !noSync {
		syncDir(filepath.Dir(path))
	}
	return nil
}

// syncDir makes a rename durable. Windows cannot fsync directories, and NTFS journals
// metadata itself, so a failure here is deliberately ignored.
func syncDir(dir string) {
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		d.Close()
	}
}
