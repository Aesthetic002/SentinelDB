package wal

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"sentineldb/internal/raft"
)

func entry(i, t uint64) raft.Entry {
	return raft.Entry{Index: i, Term: t, Kind: raft.EntryNormal, Data: []byte(fmt.Sprintf("cmd-%d", i))}
}

func mustOpen(t *testing.T, dir string) (*WAL, raft.InitialState) {
	t.Helper()
	w, init, err := Open(dir, Options{NoSync: true})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	return w, init
}

func writeSample(t *testing.T, dir string) {
	t.Helper()
	w, _ := mustOpen(t, dir)
	defer w.Close()
	if err := w.SaveHardState(raft.HardState{Term: 2, Vote: "n1"}); err != nil {
		t.Fatal(err)
	}
	for i := uint64(1); i <= 5; i++ {
		if err := w.Append([]raft.Entry{entry(i, 2)}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestRoundTrip(t *testing.T) {
	dir := t.TempDir()
	writeSample(t, dir)
	w, init := mustOpen(t, dir)
	defer w.Close()
	if init.Hard != (raft.HardState{Term: 2, Vote: "n1"}) {
		t.Fatalf("hard state: %+v", init.Hard)
	}
	if len(init.Entries) != 5 || !reflect.DeepEqual(init.Entries[4], entry(5, 2)) {
		t.Fatalf("entries: %+v", init.Entries)
	}
}

func TestTruncateIsReplayed(t *testing.T) {
	dir := t.TempDir()
	w, _ := mustOpen(t, dir)
	w.Append([]raft.Entry{entry(1, 1), entry(2, 1), entry(3, 1)})
	w.TruncateFrom(2)
	w.Append([]raft.Entry{entry(2, 7)})
	w.Close()
	w2, init := mustOpen(t, dir)
	defer w2.Close()
	if len(init.Entries) != 2 || init.Entries[1].Term != 7 {
		t.Fatalf("truncation lost on replay: %+v", init.Entries)
	}
}

// A crash can stop a write at any byte. Every possible cut must recover, keep only
// whole records, and let the log keep working afterwards.
func TestTornWriteAtEveryOffset(t *testing.T) {
	src := t.TempDir()
	writeSample(t, src)
	data, err := os.ReadFile(filepath.Join(src, logFile))
	if err != nil {
		t.Fatal(err)
	}
	prev := -1
	for cut := 0; cut <= len(data); cut++ {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, logFile), data[:cut], 0o600); err != nil {
			t.Fatal(err)
		}
		w, init, err := Open(dir, Options{NoSync: true})
		if err != nil {
			t.Fatalf("cut at %d of %d should recover: %v", cut, len(data), err)
		}
		n := len(init.Entries)
		if n < prev {
			t.Fatalf("cut at %d recovered fewer entries (%d) than a shorter cut (%d)", cut, n, prev)
		}
		prev = n
		for i, e := range init.Entries {
			if !reflect.DeepEqual(e, entry(uint64(i+1), 2)) {
				t.Fatalf("cut at %d: wrong entry %+v", cut, e)
			}
		}
		// The repaired log must be writable and survive another restart.
		next := entry(uint64(n+1), 2)
		if err := w.Append([]raft.Entry{next}); err != nil {
			t.Fatal(err)
		}
		w.Close()
		w2, init2, err := Open(dir, Options{NoSync: true})
		if err != nil {
			t.Fatalf("reopen after cut at %d: %v", cut, err)
		}
		if len(init2.Entries) != n+1 {
			t.Fatalf("cut at %d: entry appended after repair was lost", cut)
		}
		w2.Close()
	}
	if prev != 5 {
		t.Fatalf("a full file should recover all 5 entries, got %d", prev)
	}
}

// Flipping any bit of any record except the final one cannot come from a crash, and
// must never be mistaken for a torn tail.
func TestBitFlipIsDetectedEverywhere(t *testing.T) {
	src := t.TempDir()
	writeSample(t, src)
	path := filepath.Join(src, logFile)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// Find where the final record starts.
	off, last := 0, 0
	for off < len(data) {
		_, next, ok := readRecord(data, off)
		if !ok {
			t.Fatalf("sample log invalid at %d", off)
		}
		last, off = off, next
	}
	for i := 0; i < len(data); i++ {
		for _, bit := range []byte{0x01, 0x80} {
			dir := t.TempDir()
			mut := append([]byte(nil), data...)
			mut[i] ^= bit
			if err := os.WriteFile(filepath.Join(dir, logFile), mut, 0o600); err != nil {
				t.Fatal(err)
			}
			w, init, err := Open(dir, Options{NoSync: true})
			if i < last {
				if !errors.Is(err, ErrCorrupt) {
					t.Fatalf("flip at byte %d (bit %x) not reported as corruption: err=%v", i, bit, err)
				}
				continue
			}
			// Damage to the final record is indistinguishable from a torn write. It
			// must still never yield a modified entry.
			if err != nil {
				t.Fatalf("flip in final record at %d: %v", i, err)
			}
			w.Close()
			if len(init.Entries) != 4 {
				t.Fatalf("flip at %d: final record should have been dropped, got %d entries", i, len(init.Entries))
			}
		}
	}
}

func TestSnapshotCompactsAndRecovers(t *testing.T) {
	dir := t.TempDir()
	w, _ := mustOpen(t, dir)
	w.SaveHardState(raft.HardState{Term: 3, Vote: "n2"})
	w.Append([]raft.Entry{entry(1, 1), entry(2, 1), entry(3, 3), entry(4, 3)})
	s := raft.Snapshot{Index: 2, Term: 1, Members: raft.Membership{"n1": "a"}, Data: []byte("state")}
	if err := w.SaveSnapshot(s, []raft.Entry{entry(3, 3), entry(4, 3)}); err != nil {
		t.Fatal(err)
	}
	if err := w.Append([]raft.Entry{entry(5, 3)}); err != nil {
		t.Fatal(err)
	}
	w.Close()
	w2, init := mustOpen(t, dir)
	defer w2.Close()
	if !reflect.DeepEqual(init.Snapshot, s) {
		t.Fatalf("snapshot: %+v", init.Snapshot)
	}
	if len(init.Entries) != 3 || init.Entries[0].Index != 3 || init.Entries[2].Index != 5 {
		t.Fatalf("entries after compaction: %+v", init.Entries)
	}
	if init.Hard.Vote != "n2" {
		t.Fatalf("compaction lost the vote: %+v", init.Hard)
	}
}

func TestSnapshotCorruptionRefusesToBoot(t *testing.T) {
	dir := t.TempDir()
	w, _ := mustOpen(t, dir)
	s := raft.Snapshot{Index: 2, Term: 1, Members: raft.Membership{"n1": "a"}, Data: []byte("state")}
	if err := w.SaveSnapshot(s, nil); err != nil {
		t.Fatal(err)
	}
	w.Close()
	path := filepath.Join(dir, snapFile)
	data, _ := os.ReadFile(path)
	for i := range data {
		mut := append([]byte(nil), data...)
		mut[i] ^= 0x40
		os.WriteFile(path, mut, 0o600)
		if _, _, err := Open(dir, Options{NoSync: true}); !errors.Is(err, ErrCorrupt) {
			t.Fatalf("flip at snapshot byte %d not detected: %v", i, err)
		}
	}
}

// A crash between the snapshot rename and the log rewrite leaves both files. Raft §7
// decides what survives: keep the suffix only if the log holds the snapshot's last entry.
func TestCrashBetweenSnapshotAndLogRewrite(t *testing.T) {
	cases := []struct {
		name string
		snap raft.Snapshot
		want []uint64
	}{
		{"local compaction keeps suffix", raft.Snapshot{Index: 2, Term: 1}, []uint64{3, 4}},
		{"installed snapshot with conflicting log discards it", raft.Snapshot{Index: 3, Term: 9}, nil},
		{"installed snapshot ahead of log discards it", raft.Snapshot{Index: 9, Term: 9}, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			w, _ := mustOpen(t, dir)
			w.Append([]raft.Entry{entry(1, 1), entry(2, 1), entry(3, 1), entry(4, 1)})
			w.Close()
			c.snap.Members = raft.Membership{"n1": "a"}
			c.snap.Data = []byte("s")
			if err := writeSnapshot(filepath.Join(dir, snapFile), c.snap, true); err != nil {
				t.Fatal(err)
			}
			w2, init := mustOpen(t, dir)
			var got []uint64
			for _, e := range init.Entries {
				got = append(got, e.Index)
			}
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("got %v, want %v", got, c.want)
			}
			// The repair must be durable: new entries must extend from the snapshot.
			if err := w2.Append([]raft.Entry{entry(c.snap.Index+uint64(len(got))+1, 9)}); err != nil {
				t.Fatal(err)
			}
			w2.Close()
			w3, init3, err := Open(dir, Options{NoSync: true})
			if err != nil {
				t.Fatalf("reopen after repair: %v", err)
			}
			defer w3.Close()
			if len(init3.Entries) != len(got)+1 {
				t.Fatalf("after repair: %+v", init3.Entries)
			}
		})
	}
}

// The WAL is a raft.Storage; run a real node against it through a restart.
func TestDrivesRaftThroughRestart(t *testing.T) {
	dir := t.TempDir()
	boot := func() (*raft.Node, *WAL) {
		w, init, err := Open(dir, Options{})
		if err != nil {
			t.Fatal(err)
		}
		return raft.NewNode(raft.Config{
			ID: "solo", Members: raft.Membership{"solo": "x"},
			ElectionTicks: 10, HeartbeatTicks: 2,
			Rand: func(int) int { return 0 }, Storage: w, Initial: init,
		}), w
	}
	n, w := boot()
	for n.Status().Role != raft.Leader {
		n.Tick()
	}
	idx, _, err := n.Propose([]byte("hello"))
	if err != nil {
		t.Fatal(err)
	}
	termBefore := n.Status().Term
	n.Ready()
	w.Close()

	n, w = boot()
	defer w.Close()
	if n.Status().Term != termBefore {
		t.Fatalf("term not recovered")
	}
	for n.Status().Role != raft.Leader {
		n.Tick()
	}
	var found bool
	for _, e := range n.Ready().Committed {
		if e.Index == idx && string(e.Data) == "hello" {
			found = true
		}
	}
	if !found {
		t.Fatalf("committed command not re-applied after restart")
	}
}
