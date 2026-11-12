package raft

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"math/rand"
	"sort"
	"testing"
)

var (
	simSeeds = flag.Int("sim.seeds", 40, "number of seeds per randomized simulation scenario")
	simTicks = flag.Int("sim.ticks", 6000, "logical ticks per simulated run")
	simSeed  = flag.Int64("sim.seed", -1, "run a single seed, for replaying a failure")
)

// memStore is a Storage whose contents survive a simulated crash.
type memStore struct {
	hard HardState
	snap Snapshot
	log  []Entry
}

func (s *memStore) SaveHardState(h HardState) error { s.hard = h; return nil }
func (s *memStore) Append(es []Entry) error {
	if len(es) > 0 {
		want := s.snap.Index + uint64(len(s.log)) + 1
		if es[0].Index != want {
			return fmt.Errorf("append at %d, expected %d", es[0].Index, want)
		}
	}
	s.log = append(s.log, es...)
	return nil
}
func (s *memStore) TruncateFrom(i uint64) error {
	s.log = append([]Entry(nil), s.log[:i-s.snap.Index-1]...)
	return nil
}
func (s *memStore) SaveSnapshot(sn Snapshot, keep []Entry) error {
	s.snap = sn
	s.log = append([]Entry(nil), keep...)
	return nil
}

// simMachine is a hash chain over every applied command. Two replicas that applied
// the same commands in the same order have the same chain value at the same index,
// so this checks State Machine Safety across snapshots as well as plain replay.
type simMachine struct {
	index uint64
	chain [32]byte
}

func (m *simMachine) apply(e Entry) {
	h := sha256.New()
	h.Write(m.chain[:])
	var b [17]byte
	binary.BigEndian.PutUint64(b[:], e.Index)
	binary.BigEndian.PutUint64(b[8:], e.Term)
	b[16] = byte(e.Kind)
	h.Write(b[:])
	h.Write(e.Data)
	copy(m.chain[:], h.Sum(nil))
	m.index = e.Index
}

func (m *simMachine) encode() []byte {
	b, _ := json.Marshal(struct {
		I uint64
		C [32]byte
	}{m.index, m.chain})
	return b
}

func (m *simMachine) decode(data []byte) {
	var v struct {
		I uint64
		C [32]byte
	}
	if err := json.Unmarshal(data, &v); err != nil {
		panic(err)
	}
	m.index, m.chain = v.I, v.C
}

type simNode struct {
	id      NodeID
	store   *memStore
	node    *Node // nil while crashed
	machine simMachine
}

type simMsg struct {
	at uint64
	m  Message
}

type simOpts struct {
	nodes       int
	initial     int // nodes in the boot configuration; the rest may join later
	drop        float64
	dup         float64
	maxDelay    int
	crashEvery  int // mean ticks between crashes; 0 disables
	partEvery   int // mean ticks between partition changes; 0 disables
	transfers   bool
	compact     bool
	membership  bool
	confEvery   int // mean ticks between membership changes (default 500)
	reads       bool
	proposeRate float64
}

type sim struct {
	t       *testing.T
	seed    int64
	rng     *rand.Rand
	opts    simOpts
	nodes   []*simNode
	byID    map[NodeID]*simNode
	queue   []simMsg
	now     uint64
	nextCmd uint64
	group   map[NodeID]int // partition group; nodes talk only within a group

	// Global facts accumulated for the invariant checks.
	leaders   map[uint64]NodeID      // term -> the one leader of that term
	chains    map[uint64][32]byte    // index -> state machine chain
	committed map[uint64]committedAt // index -> entry term and observing term
	entryData map[[2]uint64][32]byte // (index, term) -> content hash

	maxCommitted uint64            // highest index any node has seen committed
	reads        map[uint64]uint64 // ctx -> maxCommitted when the read was issued
	nextRead     uint64

	stats simStats
}

type committedAt struct {
	entryTerm uint64
	seenTerm  uint64
}

type simStats struct {
	elections, crashes, partitions, transfers, compactions, confChanges, readsOK, readsFailed int
}

func (s *sim) fatalf(format string, args ...any) {
	s.t.Helper()
	s.t.Fatalf("seed %d tick %d: "+format+"\n(replay with -sim.seed=%d)",
		append(append([]any{s.seed, s.now}, args...), s.seed)...)
}

func newSim(t *testing.T, seed int64, opts simOpts) *sim {
	s := &sim{
		t: t, seed: seed, rng: rand.New(rand.NewSource(seed)), opts: opts,
		byID: map[NodeID]*simNode{}, group: map[NodeID]int{},
		leaders: map[uint64]NodeID{}, chains: map[uint64][32]byte{},
		committed: map[uint64]committedAt{}, entryData: map[[2]uint64][32]byte{},
		reads: map[uint64]uint64{},
	}
	if opts.initial == 0 {
		s.opts.initial = opts.nodes
	}
	members := Membership{}
	for i := 0; i < s.opts.nodes; i++ {
		id := NodeID(fmt.Sprintf("n%d", i+1))
		sn := &simNode{id: id, store: &memStore{}}
		s.nodes = append(s.nodes, sn)
		s.byID[id] = sn
		if i < s.opts.initial {
			members[id] = "addr"
		}
	}
	for i, sn := range s.nodes {
		if i < s.opts.initial {
			s.start(sn, members)
		} else {
			s.start(sn, Membership{})
		}
	}
	return s
}

func (s *sim) start(sn *simNode, boot Membership) {
	st := sn.store
	sn.machine = simMachine{}
	if st.snap.Data != nil {
		sn.machine.decode(st.snap.Data)
	}
	sn.node = NewNode(Config{
		ID: sn.id, Members: boot,
		ElectionTicks: 12, HeartbeatTicks: 3,
		Rand: s.rng.Intn, MaxBatchEntries: 8,
		Storage: st,
		Initial: InitialState{Hard: st.hard, Snapshot: st.snap, Entries: append([]Entry(nil), st.log...)},
	})
}

// drain performs what a driver would do with Ready, checking invariants on the way.
func (s *sim) drain(sn *simNode) {
	r := sn.node.Ready()
	if r.Snapshot != nil {
		sn.machine.decode(r.Snapshot.Data)
		if want, ok := s.chains[r.Snapshot.Index]; ok && want != sn.machine.chain {
			s.fatalf("state machine safety: snapshot at %d differs on %s", r.Snapshot.Index, sn.id)
		}
	}
	for _, e := range r.Committed {
		if e.Index != sn.machine.index+1 {
			s.fatalf("%s applied %d after %d: gap or replay in apply order", sn.id, e.Index, sn.machine.index)
		}
		sn.machine.apply(e)
		if want, ok := s.chains[e.Index]; ok {
			if want != sn.machine.chain {
				s.fatalf("state machine safety: %s diverged at index %d", sn.id, e.Index)
			}
		} else {
			s.chains[e.Index] = sn.machine.chain
		}
		if c, ok := s.committed[e.Index]; !ok {
			s.committed[e.Index] = committedAt{entryTerm: e.Term, seenTerm: sn.node.term}
		} else if c.entryTerm != e.Term {
			s.fatalf("index %d committed with both term %d and term %d", e.Index, c.entryTerm, e.Term)
		}
		if e.Index > s.maxCommitted {
			s.maxCommitted = e.Index
		}
	}
	for _, rs := range r.Reads {
		required, ok := s.reads[rs.Ctx]
		if !ok {
			s.fatalf("read %d answered twice or never issued", rs.Ctx)
		}
		delete(s.reads, rs.Ctx)
		// Linearizability: a read must reflect every write committed before it began.
		if rs.Index < required {
			s.fatalf("stale read on %s: read index %d but %d was committed before the read began",
				sn.id, rs.Index, required)
		}
		s.stats.readsOK++
	}
	for _, c := range r.FailedReads {
		delete(s.reads, c)
		s.stats.readsFailed++
	}
	for _, m := range r.Messages {
		s.enqueue(m)
	}
	s.checkLeader(sn)
}

func (s *sim) checkLeader(sn *simNode) {
	n := sn.node
	if n.role != Leader {
		return
	}
	if who, ok := s.leaders[n.term]; ok {
		if who != sn.id {
			s.fatalf("ELECTION SAFETY: %s and %s both leader in term %d", who, sn.id, n.term)
		}
		return
	}
	s.leaders[n.term] = sn.id
	s.stats.elections++
	s.checkLeaderCompleteness(sn)
}

func (s *sim) checkLeaderCompleteness(sn *simNode) {
	n := sn.node
	for idx, c := range s.committed {
		if n.term <= c.seenTerm {
			continue
		}
		if t, ok := n.termAt(idx); idx > n.snap.Index && (!ok || t != c.entryTerm) {
			s.fatalf("LEADER COMPLETENESS: %s, leader of term %d, lacks entry %d (term %d) committed by term %d",
				sn.id, n.term, idx, c.entryTerm, c.seenTerm)
		}
	}
}

func (s *sim) checkLogMatching() {
	type key struct{ i, t uint64 }
	for _, sn := range s.nodes {
		for _, e := range sn.store.log {
			h := sha256.Sum256(append([]byte{byte(e.Kind)}, e.Data...))
			k := [2]uint64{e.Index, e.Term}
			if old, ok := s.entryData[k]; ok && old != h {
				s.fatalf("LOG MATCHING: two different entries at index %d term %d", e.Index, e.Term)
			}
			s.entryData[k] = h
		}
	}
	// Log Matching's second half: same (index, term) implies identical prefixes.
	for i, a := range s.nodes {
		for _, b := range s.nodes[i+1:] {
			if a.node == nil || b.node == nil {
				continue
			}
			checkPrefix := false
			hi := min(a.node.lastIndex(), b.node.lastIndex())
			lo := max(a.node.snap.Index, b.node.snap.Index) + 1
			for idx := hi; idx >= lo && idx > 0; idx-- {
				ta, _ := a.node.termAt(idx)
				tb, _ := b.node.termAt(idx)
				if ta == tb {
					checkPrefix = true
				} else if checkPrefix {
					s.fatalf("LOG MATCHING: %s and %s agree above index %d yet differ there", a.id, b.id, idx)
				}
			}
		}
	}
}

func (s *sim) checkCommitted() {
	// Committed entries must never vanish from the durable state of nodes that hold them.
	for _, sn := range s.nodes {
		if sn.node == nil {
			continue
		}
		for idx := sn.node.snap.Index + 1; idx <= sn.node.commit; idx++ {
			t, ok := sn.node.termAt(idx)
			c, seen := s.committed[idx]
			if ok && seen && t != c.entryTerm {
				s.fatalf("%s holds term %d at committed index %d (expected term %d)", sn.id, t, idx, c.entryTerm)
			}
		}
	}
}

func (s *sim) enqueue(m Message) {
	if s.rng.Float64() < s.opts.drop {
		return
	}
	delay := 1
	if s.opts.maxDelay > 1 {
		delay += s.rng.Intn(s.opts.maxDelay)
	}
	s.queue = append(s.queue, simMsg{at: s.now + uint64(delay), m: m})
	if s.rng.Float64() < s.opts.dup {
		s.queue = append(s.queue, simMsg{at: s.now + uint64(delay) + uint64(s.rng.Intn(10)), m: m})
	}
}

func (s *sim) deliver() {
	kept := s.queue[:0]
	var due []simMsg
	for _, q := range s.queue {
		if q.at <= s.now {
			due = append(due, q)
		} else {
			kept = append(kept, q)
		}
	}
	s.queue = kept
	for _, q := range due {
		sn := s.byID[q.m.To]
		if sn == nil || sn.node == nil || s.group[q.m.From] != s.group[q.m.To] {
			continue
		}
		sn.node.Step(q.m)
		s.drain(sn)
	}
}

func (s *sim) leader() *simNode {
	var best *simNode
	for _, sn := range s.nodes {
		if sn.node != nil && sn.node.role == Leader && (best == nil || sn.node.term > best.node.term) {
			best = sn
		}
	}
	return best
}

func chance(r *rand.Rand, every int) bool { return every > 0 && r.Intn(every) == 0 }

func (s *sim) chaos() {
	o := s.opts
	alive := 0
	for _, sn := range s.nodes {
		if sn.node != nil {
			alive++
		}
	}
	if chance(s.rng, o.crashEvery) {
		sn := s.nodes[s.rng.Intn(len(s.nodes))]
		if sn.node != nil {
			sn.node = nil
			s.stats.crashes++
		}
	}
	for _, sn := range s.nodes {
		if sn.node == nil && s.rng.Intn(100) == 0 {
			s.start(sn, s.bootFor(sn))
		}
	}
	if chance(s.rng, o.partEvery) {
		s.stats.partitions++
		if s.rng.Intn(3) == 0 {
			s.group = map[NodeID]int{}
		} else {
			for _, sn := range s.nodes {
				s.group[sn.id] = s.rng.Intn(2)
			}
		}
	}

	ld := s.leader()
	if ld == nil {
		return
	}
	if s.rng.Float64() < o.proposeRate {
		s.nextCmd++
		_, _, _ = ld.node.Propose([]byte(fmt.Sprintf("cmd-%d", s.nextCmd)))
		s.drain(ld)
	}
	if o.transfers && chance(s.rng, 300) {
		to := s.nodes[s.rng.Intn(len(s.nodes))].id
		if ld.node.TransferLeadership(to) == nil {
			s.stats.transfers++
		}
		s.drain(ld)
	}
	if o.reads && s.rng.Intn(20) == 0 {
		s.nextRead++
		s.reads[s.nextRead] = s.maxCommitted
		if ld.node.ReadIndex(s.nextRead) != nil {
			delete(s.reads, s.nextRead)
		}
		s.drain(ld)
	}
	confEvery := o.confEvery
	if confEvery == 0 {
		confEvery = 500
	}
	if o.membership && chance(s.rng, confEvery) {
		s.changeMembership(ld)
	}
	if o.compact && chance(s.rng, 200) {
		sn := s.nodes[s.rng.Intn(len(s.nodes))]
		if sn.node != nil && sn.machine.index > sn.node.snap.Index {
			if err := sn.node.Compact(sn.machine.index, sn.machine.encode()); err == nil {
				s.stats.compactions++
			}
		}
	}
}

func (s *sim) bootFor(sn *simNode) Membership {
	// A node's boot config only matters until it has a config in its log or snapshot;
	// the initial members share the same seed configuration, joiners start empty.
	for i, x := range s.nodes {
		if x == sn && i < s.opts.initial {
			m := Membership{}
			for _, y := range s.nodes[:s.opts.initial] {
				m[y.id] = "addr"
			}
			return m
		}
	}
	return Membership{}
}

func (s *sim) changeMembership(ld *simNode) {
	next := ld.node.members.Clone()
	sn := s.nodes[s.rng.Intn(len(s.nodes))]
	if _, in := next[sn.id]; in {
		if len(next) <= 3 { // keep enough voters for the run to stay interesting
			return
		}
		delete(next, sn.id)
	} else {
		next[sn.id] = "addr"
	}
	if _, _, err := ld.node.ProposeMembership(next); err == nil {
		s.stats.confChanges++
	}
	s.drain(ld)
}

func (s *sim) tick() {
	s.now++
	for _, sn := range s.nodes {
		if sn.node != nil {
			sn.node.Tick()
			s.drain(sn)
		}
	}
	s.deliver()
}

// heal removes all faults and requires the cluster to recover and make progress.
func (s *sim) heal() {
	s.opts.drop, s.opts.dup = 0, 0
	s.group = map[NodeID]int{}
	for _, sn := range s.nodes {
		if sn.node == nil {
			s.start(sn, s.bootFor(sn))
		}
	}
	for i := 0; i < 4000; i++ {
		s.tick()
		ld := s.leader()
		if ld == nil || !ld.node.leaderReady() {
			continue
		}
		before := ld.node.commit
		idx, term, err := ld.node.Propose([]byte("liveness-probe"))
		s.drain(ld)
		if err != nil {
			continue
		}
		for j := 0; j < 500 && ld.node != nil && ld.node.commit < idx; j++ {
			s.tick()
		}
		if ld.node.commit >= idx && ld.node.term == term && before < idx {
			// Everyone in the current config must converge on the same state.
			for j := 0; j < 2000; j++ {
				s.tick()
				if s.converged(ld, idx) {
					return
				}
			}
			s.fatalf("followers never converged at index %d", idx)
		}
	}
	s.fatalf("LIVENESS: no progress within 4000 ticks of healing all faults")
}

func (s *sim) converged(ld *simNode, idx uint64) bool {
	for id := range ld.node.members {
		if s.byID[id].machine.index < idx {
			return false
		}
	}
	return true
}

func (s *sim) run(ticks int) {
	for i := 0; i < ticks; i++ {
		s.chaos()
		s.tick()
		if i%100 == 0 {
			s.checkLogMatching()
			s.checkCommitted()
			for _, sn := range s.nodes {
				if sn.node != nil && sn.node.role == Leader {
					s.checkLeaderCompleteness(sn)
				}
			}
		}
	}
	s.heal()
	s.checkLogMatching()
	s.checkCommitted()
}

func seedsFor(n int) []int64 {
	if *simSeed >= 0 {
		return []int64{*simSeed}
	}
	out := make([]int64, n)
	for i := range out {
		out[i] = int64(i + 1)
	}
	return out
}

func TestSimulation(t *testing.T) {
	scenarios := map[string]simOpts{
		"calm-3":     {nodes: 3, maxDelay: 2, proposeRate: 0.3},
		"lossy-5":    {nodes: 5, maxDelay: 8, drop: 0.15, dup: 0.05, proposeRate: 0.3},
		"crashes-5":  {nodes: 5, maxDelay: 4, drop: 0.05, crashEvery: 150, proposeRate: 0.3},
		"partitions": {nodes: 5, maxDelay: 4, drop: 0.05, partEvery: 150, proposeRate: 0.3},
		"everything": {nodes: 5, maxDelay: 6, drop: 0.1, dup: 0.05, crashEvery: 200, partEvery: 200,
			transfers: true, compact: true, reads: true, proposeRate: 0.3},
		"transfers-storm": {nodes: 5, maxDelay: 3, drop: 0.02, transfers: true, reads: true, proposeRate: 0.5},
		"membership-storm": {nodes: 5, initial: 3, maxDelay: 6, drop: 0.1, crashEvery: 150, partEvery: 100,
			membership: true, confEvery: 15, proposeRate: 0.3},
		"membership": {nodes: 5, initial: 3, maxDelay: 4, drop: 0.05, crashEvery: 400, partEvery: 400,
			membership: true, transfers: true, compact: true, reads: true, proposeRate: 0.3},
	}
	names := make([]string, 0, len(scenarios))
	for k := range scenarios {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, name := range names {
		opts := scenarios[name]
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var total simStats
			for _, seed := range seedsFor(*simSeeds) {
				s := newSim(t, seed, opts)
				s.run(*simTicks)
				total.elections += s.stats.elections
				total.crashes += s.stats.crashes
				total.partitions += s.stats.partitions
				total.transfers += s.stats.transfers
				total.compactions += s.stats.compactions
				total.confChanges += s.stats.confChanges
				total.readsOK += s.stats.readsOK
				total.readsFailed += s.stats.readsFailed
			}
			// Guard against a scenario that quietly stopped exercising its fault.
			if opts.crashEvery > 0 && total.crashes == 0 {
				t.Errorf("scenario injected no crashes")
			}
			if opts.transfers && total.transfers == 0 {
				t.Errorf("scenario performed no leadership transfers")
			}
			if opts.membership && total.confChanges == 0 {
				t.Errorf("scenario performed no membership changes")
			}
			if opts.compact && total.compactions == 0 {
				t.Errorf("scenario performed no compactions")
			}
			if opts.reads && total.readsOK == 0 {
				t.Errorf("scenario completed no reads")
			}
			t.Logf("%+v", total)
		})
	}
}
