package txn

import (
	"context"
	"errors"
	"math/rand"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestCompatibilityMatrix(t *testing.T) {
	want := map[[2]Mode]bool{
		{IS, IS}: true, {IS, IX}: true, {IS, S}: true, {IS, X}: false,
		{IX, IS}: true, {IX, IX}: true, {IX, S}: false, {IX, X}: false,
		{S, IS}: true, {S, IX}: false, {S, S}: true, {S, X}: false,
		{X, IS}: false, {X, IX}: false, {X, S}: false, {X, X}: false,
	}
	for k, v := range want {
		if compatible(k[0], k[1]) != v {
			t.Errorf("compatible(%v,%v) != %v", k[0], k[1], v)
		}
		if compatible(k[0], k[1]) != compatible(k[1], k[0]) {
			t.Errorf("matrix not symmetric at %v,%v", k[0], k[1])
		}
	}
}

func TestSharedLocksCoexistExclusiveDoesNot(t *testing.T) {
	m := NewManager()
	ctx := context.Background()
	if err := m.Acquire(ctx, 0, 1, "x", S); err != nil {
		t.Fatal(err)
	}
	if err := m.Acquire(ctx, 0, 2, "x", S); err != nil {
		t.Fatalf("two readers should coexist: %v", err)
	}
	// Younger writer conflicts with an older reader: wait-die says it dies.
	if err := m.Acquire(ctx, 0, 3, "x", X); !errors.Is(err, ErrDie) {
		t.Fatalf("younger writer should die, got %v", err)
	}
}

func TestOlderWaitsForYounger(t *testing.T) {
	m := NewManager()
	ctx := context.Background()
	if err := m.Acquire(ctx, 0, 5, "x", X); err != nil {
		t.Fatal(err)
	}
	got := make(chan error, 1)
	go func() { got <- m.Acquire(ctx, 0, 1, "x", S) }()
	select {
	case err := <-got:
		t.Fatalf("older transaction should wait, returned %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	m.ReleaseAll(5)
	if err := <-got; err != nil {
		t.Fatalf("waiter should be granted after release: %v", err)
	}
}

func TestUpgrade(t *testing.T) {
	m := NewManager()
	ctx := context.Background()
	if err := m.Acquire(ctx, 0, 1, "x", S); err != nil {
		t.Fatal(err)
	}
	if err := m.Acquire(ctx, 0, 1, "x", X); err != nil {
		t.Fatalf("sole holder should upgrade: %v", err)
	}
	if err := m.Acquire(ctx, 0, 1, "x", S); err != nil {
		t.Fatalf("X covers S: %v", err)
	}
}

func TestWriterIsNotStarvedByReaders(t *testing.T) {
	m := NewManager()
	ctx := context.Background()
	if err := m.Acquire(ctx, 0, 5, "x", S); err != nil {
		t.Fatal(err)
	}
	writer := make(chan error, 1)
	go func() { writer <- m.Acquire(ctx, 0, 1, "x", X) }()
	deadline := time.After(time.Second)
	for {
		m.mu.Lock()
		queued := m.locks["x"] != nil && len(m.locks["x"].queue) == 1
		m.mu.Unlock()
		if queued {
			break
		}
		select {
		case <-deadline:
			t.Fatal("writer never queued")
		default:
		}
	}
	// A newer reader must not overtake the queued older writer.
	if err := m.Acquire(ctx, 0, 9, "x", S); !errors.Is(err, ErrDie) {
		t.Fatalf("younger reader should yield to the queued writer, got %v", err)
	}
	m.ReleaseAll(5)
	if err := <-writer; err != nil {
		t.Fatal(err)
	}
}

func TestContextCancelStopsWaiting(t *testing.T) {
	m := NewManager()
	if err := m.Acquire(context.Background(), 0, 5, "x", X); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := m.Acquire(ctx, 0, 1, "x", X); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %v", err)
	}
	m.ReleaseAll(5)
	if m.Size() != 0 {
		t.Fatalf("lock table leaked %d entries", m.Size())
	}
}

func TestEpochChangeInvalidatesEverything(t *testing.T) {
	m := NewManager()
	ctx := context.Background()
	if err := m.Acquire(ctx, 0, 5, "x", X); err != nil {
		t.Fatal(err)
	}
	waiting := make(chan error, 1)
	go func() { waiting <- m.Acquire(ctx, 0, 1, "x", X) }()
	time.Sleep(20 * time.Millisecond)
	m.SetEpoch(7)
	if err := <-waiting; !errors.Is(err, ErrStale) {
		t.Fatalf("a waiter should fail when leadership changes: %v", err)
	}
	// A transaction from the old term must not get new locks in the new term.
	if err := m.Acquire(ctx, 0, 5, "y", S); !errors.Is(err, ErrStale) {
		t.Fatalf("stale transaction acquired a lock: %v", err)
	}
	if err := m.Acquire(ctx, 7, 9, "x", X); err != nil {
		t.Fatalf("current-epoch transaction should proceed: %v", err)
	}
}

// Under heavy contention with lock orders chosen to deadlock, wait-die must always
// terminate, and a retried transaction keeps its ID so it cannot starve.
func TestNoDeadlockUnderContention(t *testing.T) {
	m := NewManager()
	var finished atomic.Int64
	var wg sync.WaitGroup
	const workers, rounds = 16, 150
	resources := []string{"a", "b", "c", "d"}
	done := make(chan struct{})
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(seed int64) {
			defer wg.Done()
			r := rand.New(rand.NewSource(seed))
			for i := 0; i < rounds; i++ {
				tx := m.NewTxID()
				order := r.Perm(len(resources))
			retry:
				for {
					for _, idx := range order {
						mode := S
						if r.Intn(2) == 0 {
							mode = X
						}
						if err := m.Acquire(context.Background(), 0, tx, resources[idx], mode); err != nil {
							m.ReleaseAll(tx)
							continue retry
						}
					}
					break
				}
				m.ReleaseAll(tx)
				finished.Add(1)
			}
		}(int64(w))
	}
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(60 * time.Second):
		t.Fatalf("stuck: only %d transactions finished, a deadlock is likely", finished.Load())
	}
	if m.Size() != 0 {
		t.Fatalf("lock table leaked %d entries", m.Size())
	}
}

func TestPrecedenceGraphOnTextbookSchedules(t *testing.T) {
	cases := []struct {
		name, schedule string
		serializable   bool
	}{
		{"serial", "r1(x) w1(x) c1 r2(x) w2(x) c2", true},
		{"no conflicts", "r1(x) r2(x) w1(y) w2(z)", true},
		{"reads never conflict", "r1(x) r2(x) r1(y) r2(y)", true},
		{"interleaved but equivalent to T1,T2", "r1(a) w1(a) r2(a) w2(a) r1(b) w1(b) r2(b) w2(b)", true},
		{"lost update", "r1(x) r2(x) w1(x) w2(x)", false},
		{"unrepeatable read", "r1(x) w2(x) c2 r1(x)", false},
		{"transfer against a total", "w1(a) r2(a) r2(b) w1(b)", false},
		{"three-way cycle", "r1(x) w2(x) r2(y) w3(y) r3(z) w1(z)", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ops, err := ParseSchedule(c.schedule)
			if err != nil {
				t.Fatal(err)
			}
			res := CheckConflictSerializable(ops)
			if res.Serializable != c.serializable {
				t.Fatalf("got serializable=%v, edges %v", res.Serializable, res.Edges)
			}
			if c.serializable {
				verifySerialOrder(t, ops, res.Order)
			} else if len(res.Cycle) < 2 {
				t.Fatalf("expected a cycle witness, got %v", res.Cycle)
			}
		})
	}
}

// verifySerialOrder checks the answer independently of the algorithm: running the
// transactions in the reported order must preserve every conflict's direction.
func verifySerialOrder(t *testing.T, ops []Op, order []uint64) {
	t.Helper()
	pos := map[uint64]int{}
	for i, tx := range order {
		pos[tx] = i
	}
	for i, a := range ops {
		for _, b := range ops[i+1:] {
			if a.Tx != b.Tx && a.Item == b.Item && (a.Write || b.Write) && pos[a.Tx] > pos[b.Tx] {
				t.Fatalf("order %v reverses the conflict %v then %v", order, a, b)
			}
		}
	}
}

func TestParseScheduleRejectsNonsense(t *testing.T) {
	for _, s := range []string{"x1(a)", "r(a)", "r1()", "r0(a)", "hello"} {
		if _, err := ParseSchedule(s); err == nil {
			t.Errorf("%q should be rejected", s)
		}
	}
}
