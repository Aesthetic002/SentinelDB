package txn

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// Op is one action of a schedule.
type Op struct {
	Tx    uint64
	Write bool
	Item  string
}

func (o Op) String() string {
	k := "r"
	if o.Write {
		k = "w"
	}
	return fmt.Sprintf("%s%d(%s)", k, o.Tx, o.Item)
}

// Recorder collects a schedule as it actually happens.
type Recorder struct {
	mu   sync.Mutex
	ops  []Op
	done map[uint64]bool
}

// NewRecorder returns an empty recorder.
func NewRecorder() *Recorder { return &Recorder{done: map[uint64]bool{}} }

// Record appends one action.
func (r *Recorder) Record(tx uint64, write bool, item string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ops = append(r.ops, Op{tx, write, item})
}

// Committed marks a transaction as committed. Aborted transactions' actions never take
// effect, so they are left out of the analysis.
func (r *Recorder) Committed(tx uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.done[tx] = true
}

// Forget erases a failed attempt's actions. A retried transaction keeps its ID, and the
// actions of an attempt that aborted never took effect, so they must not count.
func (r *Recorder) Forget(tx uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	kept := r.ops[:0]
	for _, o := range r.ops {
		if o.Tx != tx {
			kept = append(kept, o)
		}
	}
	r.ops = kept
}

// Schedule returns the recorded actions of committed transactions, in order.
func (r *Recorder) Schedule() []Op {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Op, 0, len(r.ops))
	for _, o := range r.ops {
		if r.done[o.Tx] {
			out = append(out, o)
		}
	}
	return out
}

// PrecedenceResult is the outcome of the conflict-serializability test.
type PrecedenceResult struct {
	Serializable bool
	// Edges are the precedence edges, "t1 -> t2", each with the conflict causing it.
	Edges map[[2]uint64]string
	// Order is a serial order equivalent to the schedule, when one exists.
	Order []uint64
	// Cycle is a witness that no equivalent serial order exists.
	Cycle []uint64
}

// CheckConflictSerializable applies the textbook test. Two actions conflict when they
// belong to different transactions, touch the same item, and at least one writes. Each
// conflict forces its transactions into an order, giving a precedence graph, and the
// schedule is conflict-serializable exactly when that graph has no cycle.
func CheckConflictSerializable(schedule []Op) PrecedenceResult {
	res := PrecedenceResult{Edges: map[[2]uint64]string{}}
	nodes := map[uint64]bool{}

	type itemState struct {
		lastWriter uint64
		hasWriter  bool
		readers    map[uint64]bool
	}
	items := map[string]*itemState{}
	addEdge := func(a, b uint64, why string) {
		if a == b {
			return
		}
		k := [2]uint64{a, b}
		if _, ok := res.Edges[k]; !ok {
			res.Edges[k] = why
		}
	}
	// Linear in the schedule: a read need only be ordered after the latest write, and a
	// write after the latest write and the readers since, because earlier conflicts are
	// already implied transitively through that write.
	for _, o := range schedule {
		nodes[o.Tx] = true
		st := items[o.Item]
		if st == nil {
			st = &itemState{readers: map[uint64]bool{}}
			items[o.Item] = st
		}
		if o.Write {
			if st.hasWriter {
				addEdge(st.lastWriter, o.Tx, fmt.Sprintf("w%d(%s) before w%d(%s)", st.lastWriter, o.Item, o.Tx, o.Item))
			}
			for r := range st.readers {
				addEdge(r, o.Tx, fmt.Sprintf("r%d(%s) before w%d(%s)", r, o.Item, o.Tx, o.Item))
			}
			st.lastWriter, st.hasWriter = o.Tx, true
			st.readers = map[uint64]bool{}
		} else {
			if st.hasWriter {
				addEdge(st.lastWriter, o.Tx, fmt.Sprintf("w%d(%s) before r%d(%s)", st.lastWriter, o.Item, o.Tx, o.Item))
			}
			st.readers[o.Tx] = true
		}
	}

	// Kahn's algorithm: a topological order exists exactly when there is no cycle.
	adj := map[uint64][]uint64{}
	indeg := map[uint64]int{}
	for n := range nodes {
		indeg[n] = 0
	}
	for e := range res.Edges {
		adj[e[0]] = append(adj[e[0]], e[1])
		indeg[e[1]]++
	}
	var ready []uint64
	for n, d := range indeg {
		if d == 0 {
			ready = append(ready, n)
		}
	}
	for len(ready) > 0 {
		sort.Slice(ready, func(i, j int) bool { return ready[i] < ready[j] })
		n := ready[0]
		ready = ready[1:]
		res.Order = append(res.Order, n)
		for _, m := range adj[n] {
			indeg[m]--
			if indeg[m] == 0 {
				ready = append(ready, m)
			}
		}
	}
	res.Serializable = len(res.Order) == len(nodes)
	if !res.Serializable {
		res.Order = nil
		res.Cycle = findCycle(adj, indeg)
	}
	return res
}

func findCycle(adj map[uint64][]uint64, indeg map[uint64]int) []uint64 {
	// Everything left with positive in-degree lies on or after a cycle; walking the
	// remaining edges backwards from any such node must therefore revisit a node.
	inCycle := map[uint64]bool{}
	var start uint64
	for n, d := range indeg {
		if d > 0 {
			inCycle[n] = true
			if start == 0 || n < start {
				start = n
			}
		}
	}
	parents := map[uint64]uint64{}
	for from, tos := range adj {
		if !inCycle[from] {
			continue
		}
		for _, to := range tos {
			if inCycle[to] && (parents[to] == 0 || from < parents[to]) {
				parents[to] = from
			}
		}
	}
	seen := map[uint64]int{}
	var path []uint64
	for n := start; ; n = parents[n] {
		if i, ok := seen[n]; ok {
			cycle := append([]uint64(nil), path[i:]...)
			for l, r := 0, len(cycle)-1; l < r; l, r = l+1, r-1 {
				cycle[l], cycle[r] = cycle[r], cycle[l]
			}
			return cycle
		}
		seen[n] = len(path)
		path = append(path, n)
	}
}

var opPattern = regexp.MustCompile(`^([rw])(\d+)\(([^)]+)\)$`)

// ParseSchedule reads the notation used in textbooks: "r1(x) w2(x) w1(y)". Commit
// actions such as "c1" are accepted and ignored.
func ParseSchedule(s string) ([]Op, error) {
	var ops []Op
	for _, f := range strings.Fields(s) {
		if len(f) > 1 && f[0] == 'c' {
			if _, err := strconv.Atoi(f[1:]); err == nil {
				continue
			}
		}
		m := opPattern.FindStringSubmatch(f)
		if m == nil {
			return nil, fmt.Errorf("cannot read %q: expected the form r1(x) or w2(y)", f)
		}
		tx, _ := strconv.ParseUint(m[2], 10, 64)
		if tx == 0 {
			return nil, fmt.Errorf("transaction numbers start at 1, in %q", f)
		}
		ops = append(ops, Op{Tx: tx, Write: m[1] == "w", Item: m[3]})
	}
	return ops, nil
}
