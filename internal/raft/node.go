package raft

import (
	"fmt"
	"sort"
)

// progress is the leader's view of one follower.
type progress struct {
	match uint64 // highest index known to be replicated
	next  uint64 // next index to send (optimistically advanced while pipelining)
	// recentActive records contact since the last CheckQuorum sweep.
	recentActive bool
	// ackSeq is the highest heartbeat sequence this follower has acknowledged.
	ackSeq uint64
	// snapWait counts down ticks before re-sending an unacknowledged snapshot.
	snapWait int
}

type pendingRead struct {
	ctx   uint64
	index uint64
	seq   uint64
}

// Node is the Raft state machine. It is not safe for concurrent use: exactly one
// goroutine (the driver) may call its methods.
type Node struct {
	id             NodeID
	electionTicks  int
	heartbeatTicks int
	rnd            func(int) int
	maxBatch       int
	store          Storage

	// Persistent.
	term uint64
	vote NodeID
	// log holds entries after the snapshot; log[i].Index == snap.Index+1+i.
	log  []Entry
	snap Snapshot

	// Volatile.
	role    Role
	leader  NodeID
	commit  uint64
	applied uint64

	members    Membership
	bootConfig Membership

	// Election state.
	votes             map[NodeID]bool
	electionElapsed   int
	randomizedTimeout int

	// Leader state.
	prs              map[NodeID]*progress
	heartbeatElapsed int
	hbSeq            uint64
	transferee       NodeID
	configIndex      uint64 // index of the latest config entry in the log
	leaderStart      uint64
	reads            []pendingRead

	// Outputs drained by Ready.
	msgs        []Message
	readyReads  []ReadState
	failedReads []uint64
	snapToApply *Snapshot
}

// NewNode builds a node from recovered durable state.
func NewNode(cfg Config) *Node {
	if cfg.ElectionTicks <= cfg.HeartbeatTicks || cfg.HeartbeatTicks <= 0 {
		panic("raft: ElectionTicks must exceed HeartbeatTicks > 0")
	}
	if cfg.Rand == nil {
		panic("raft: Config.Rand is required")
	}
	if cfg.MaxBatchEntries == 0 {
		cfg.MaxBatchEntries = 256
	}
	n := &Node{
		id:             cfg.ID,
		electionTicks:  cfg.ElectionTicks,
		heartbeatTicks: cfg.HeartbeatTicks,
		rnd:            cfg.Rand,
		maxBatch:       cfg.MaxBatchEntries,
		store:          cfg.Storage,
		term:           cfg.Initial.Hard.Term,
		vote:           cfg.Initial.Hard.Vote,
		snap:           cfg.Initial.Snapshot,
		log:            append([]Entry(nil), cfg.Initial.Entries...),
		bootConfig:     cfg.Members.Clone(),
		votes:          map[NodeID]bool{},
		prs:            map[NodeID]*progress{},
	}
	n.commit = n.snap.Index
	n.applied = n.snap.Index
	n.refreshMembers()
	n.resetElectionTimer()
	return n
}

// ---------------------------------------------------------------------------
// Log helpers
// ---------------------------------------------------------------------------

func (n *Node) lastIndex() uint64 { return n.snap.Index + uint64(len(n.log)) }

func (n *Node) lastTerm() uint64 {
	if len(n.log) == 0 {
		return n.snap.Term
	}
	return n.log[len(n.log)-1].Term
}

// termAt returns the term of the entry at index. ok is false when the term is
// unknowable: the index is past the end or was compacted away.
func (n *Node) termAt(index uint64) (term uint64, ok bool) {
	if index == n.snap.Index {
		return n.snap.Term, true
	}
	if index < n.snap.Index || index > n.lastIndex() {
		return 0, false
	}
	return n.log[index-n.snap.Index-1].Term, true
}

func (n *Node) entriesFrom(lo uint64, max int) []Entry {
	if lo > n.lastIndex() {
		return nil
	}
	s := n.log[lo-n.snap.Index-1:]
	if len(s) > max {
		s = s[:max]
	}
	return s
}

func must(err error) {
	if err != nil {
		panic(fmt.Sprintf("raft: durable storage failed, halting to preserve safety: %v", err))
	}
}

func (n *Node) persistHard() {
	must(n.store.SaveHardState(HardState{Term: n.term, Vote: n.vote}))
}

func (n *Node) appendLocal(es ...Entry) {
	must(n.store.Append(es))
	n.log = append(n.log, es...)
	for _, e := range es {
		if e.Kind == EntryConfig {
			n.refreshMembers()
			break
		}
	}
}

func (n *Node) truncateLocal(from uint64) {
	if from <= n.commit {
		panic(fmt.Sprintf("raft: %s asked to truncate committed entry %d (commit %d): state machine safety violated",
			n.id, from, n.commit))
	}
	must(n.store.TruncateFrom(from))
	removedConfig := false
	for _, e := range n.log[from-n.snap.Index-1:] {
		removedConfig = removedConfig || e.Kind == EntryConfig
	}
	n.log = n.log[:from-n.snap.Index-1]
	if removedConfig {
		n.refreshMembers()
	}
}

// refreshMembers recomputes the active configuration. Raft applies a config as
// soon as it is in the log (not when committed), so truncating a config entry
// must revert to the previous one; rescanning the log handles both cases.
func (n *Node) refreshMembers() {
	n.configIndex = 0
	n.members = n.membersAt(n.lastIndex())
	for i := len(n.log) - 1; i >= 0; i-- {
		if n.log[i].Kind == EntryConfig {
			n.configIndex = n.log[i].Index
			break
		}
	}
	if n.role == Leader {
		for _, id := range n.members.IDs() {
			if id != n.id && n.prs[id] == nil {
				n.prs[id] = &progress{next: n.lastIndex() + 1, recentActive: true}
			}
		}
		for id := range n.prs {
			if _, ok := n.members[id]; !ok {
				delete(n.prs, id)
			}
		}
	}
}

// membersAt returns the configuration in effect at the given log index.
func (n *Node) membersAt(index uint64) Membership {
	for i := len(n.log) - 1; i >= 0; i-- {
		e := n.log[i]
		if e.Index > index || e.Kind != EntryConfig {
			continue
		}
		m, err := DecodeMembership(e.Data)
		if err != nil {
			panic(fmt.Sprintf("raft: corrupt config entry %d: %v", e.Index, err))
		}
		return m
	}
	if n.snap.Members != nil {
		return n.snap.Members
	}
	return n.bootConfig
}

func (n *Node) quorum() int { return len(n.members)/2 + 1 }

func (n *Node) promotable() bool {
	_, ok := n.members[n.id]
	return ok
}

// ---------------------------------------------------------------------------
// Role transitions
// ---------------------------------------------------------------------------

func (n *Node) resetElectionTimer() {
	n.electionElapsed = 0
	n.randomizedTimeout = n.electionTicks + n.rnd(n.electionTicks)
}

func (n *Node) abortReads() {
	for _, r := range n.reads {
		n.failedReads = append(n.failedReads, r.ctx)
	}
	n.reads = nil
}

func (n *Node) becomeFollower(term uint64, leader NodeID) {
	if term != n.term {
		n.term = term
		n.vote = None
		n.persistHard()
	}
	n.role = Follower
	n.leader = leader
	n.transferee = None
	n.abortReads()
	n.resetElectionTimer()
}

func (n *Node) campaign(transfer bool) {
	if !n.promotable() {
		return
	}
	n.role = Candidate
	n.term++
	n.vote = n.id
	n.leader = None
	n.persistHard()
	n.votes = map[NodeID]bool{n.id: true}
	n.abortReads()
	n.resetElectionTimer()
	if n.countVotes() >= n.quorum() {
		n.becomeLeader()
		return
	}
	for _, id := range n.members.IDs() {
		if id == n.id {
			continue
		}
		n.send(Message{
			Type: MsgVote, To: id,
			LastLogIndex: n.lastIndex(), LastLogTerm: n.lastTerm(),
			Transfer: transfer,
		})
	}
}

func (n *Node) countVotes() int {
	c := 0
	for id, granted := range n.votes {
		if _, member := n.members[id]; member && granted {
			c++
		}
	}
	return c
}

func (n *Node) becomeLeader() {
	n.role = Leader
	n.leader = n.id
	n.transferee = None
	n.heartbeatElapsed = 0
	n.electionElapsed = 0
	n.prs = map[NodeID]*progress{}
	for _, id := range n.members.IDs() {
		if id != n.id {
			n.prs[id] = &progress{next: n.lastIndex() + 1, recentActive: true}
		}
	}
	n.leaderStart = n.lastIndex() + 1
	n.appendLocal(Entry{Index: n.leaderStart, Term: n.term, Kind: EntryNoop})
	n.maybeCommit()
	n.broadcastAppend(0)
}

// ---------------------------------------------------------------------------
// Driver API
// ---------------------------------------------------------------------------

// Tick advances logical time by one tick.
func (n *Node) Tick() {
	n.electionElapsed++
	if n.role != Leader {
		if n.electionElapsed >= n.randomizedTimeout {
			n.campaign(false)
			n.resetElectionTimer()
		}
		return
	}

	for _, pr := range n.prs {
		if pr.snapWait > 0 {
			pr.snapWait--
		}
	}
	n.heartbeatElapsed++
	if n.heartbeatElapsed >= n.heartbeatTicks {
		n.heartbeatElapsed = 0
		n.hbSeq++
		n.broadcastAppend(n.hbSeq)
		// TimeoutNow is one-shot and may be lost, so repeat it while transferring.
		if n.transferee != None {
			n.maybeSendTimeoutNow()
		}
	}
	if n.electionElapsed >= n.electionTicks {
		n.electionElapsed = 0
		n.transferee = None // a transfer that has not finished by now is abandoned
		n.checkQuorum()
	}
}

// checkQuorum makes a leader that cannot reach a majority step down instead of
// lingering. This shortens the window in which a partitioned leader looks healthy.
func (n *Node) checkQuorum() {
	active := 0
	if n.promotable() {
		active = 1
	}
	for id, pr := range n.prs {
		if _, ok := n.members[id]; ok && pr.recentActive {
			active++
		}
		pr.recentActive = false
	}
	if active < n.quorum() {
		n.becomeFollower(n.term, None)
	}
}

// Propose appends a command to the log. It returns the index and term the entry
// was assigned. Acceptance is no promise of commitment; the caller must watch for
// that index being committed with that term.
func (n *Node) Propose(data []byte) (index, term uint64, err error) {
	if n.role != Leader {
		return 0, 0, ErrNotLeader
	}
	if n.transferee != None {
		return 0, 0, ErrTransferring
	}
	e := Entry{Index: n.lastIndex() + 1, Term: n.term, Kind: EntryNormal, Data: data}
	n.appendLocal(e)
	n.maybeCommit()
	n.broadcastAppend(0)
	return e.Index, e.Term, nil
}

// ProposeBatch appends several commands with a single durable write.
func (n *Node) ProposeBatch(data [][]byte) (firstIndex, term uint64, err error) {
	if n.role != Leader {
		return 0, 0, ErrNotLeader
	}
	if n.transferee != None {
		return 0, 0, ErrTransferring
	}
	if len(data) == 0 {
		return 0, 0, nil
	}
	first := n.lastIndex() + 1
	es := make([]Entry, len(data))
	for i, d := range data {
		es[i] = Entry{Index: first + uint64(i), Term: n.term, Kind: EntryNormal, Data: d}
	}
	n.appendLocal(es...)
	n.maybeCommit()
	n.broadcastAppend(0)
	return first, n.term, nil
}

// ProposeMembership replicates a new configuration differing from the current one
// by exactly one node. Single-server changes keep old and new majorities
// overlapping. The current-term-commit requirement is the correction Ongaro
// published to the original algorithm, without which safety can be violated.
func (n *Node) ProposeMembership(next Membership) (index, term uint64, err error) {
	if n.role != Leader {
		return 0, 0, ErrNotLeader
	}
	if n.transferee != None {
		return 0, 0, ErrTransferring
	}
	if !n.leaderReady() {
		return 0, 0, ErrLeaderNotInit
	}
	if n.configIndex > n.commit {
		return 0, 0, ErrConfigPending
	}
	diff := 0
	for id := range next {
		if _, ok := n.members[id]; !ok {
			diff++
		}
	}
	for id := range n.members {
		if _, ok := next[id]; !ok {
			diff++
		}
	}
	if diff != 1 || len(next) == 0 {
		return 0, 0, ErrBadConfig
	}
	e := Entry{Index: n.lastIndex() + 1, Term: n.term, Kind: EntryConfig, Data: EncodeMembership(next)}
	n.appendLocal(e)
	n.maybeCommit()
	n.broadcastAppend(0)
	return e.Index, e.Term, nil
}

// TransferLeadership begins handing leadership to the given follower (Raft thesis
// §3.10). New proposals are refused until it finishes or times out; once the target
// has the whole log it is told to start an election immediately. The old leader
// steps down when it sees the new, higher term, so safety rests on ordinary voting.
func (n *Node) TransferLeadership(to NodeID) error {
	if n.role != Leader {
		return ErrNotLeader
	}
	if n.transferee != None {
		return ErrTransferring
	}
	if _, ok := n.members[to]; !ok || to == n.id {
		return ErrBadTransfer
	}
	n.transferee = to
	n.electionElapsed = 0
	n.maybeSendTimeoutNow()
	if n.prs[to].match < n.lastIndex() {
		n.sendAppend(to, 0)
	}
	return nil
}

func (n *Node) maybeSendTimeoutNow() {
	pr := n.prs[n.transferee]
	if pr != nil && pr.match == n.lastIndex() {
		n.send(Message{Type: MsgTimeoutNow, To: n.transferee})
	}
}

func (n *Node) leaderReady() bool {
	t, ok := n.termAt(n.commit)
	return n.role == Leader && ok && t == n.term
}

// ReadIndex requests confirmation that this leader is still current, so reads can
// be served without appending to the log. Each ctx is reported in Ready().Reads
// once a quorum has acknowledged a heartbeat sent after this call; if leadership
// is lost first it is reported in FailedReads.
func (n *Node) ReadIndex(ctxs ...uint64) error {
	if n.role != Leader {
		return ErrNotLeader
	}
	if !n.leaderReady() {
		return ErrLeaderNotInit
	}
	if len(ctxs) == 0 {
		return nil
	}
	n.hbSeq++
	for _, c := range ctxs {
		n.reads = append(n.reads, pendingRead{ctx: c, index: n.commit, seq: n.hbSeq})
	}
	n.releaseReads()
	if len(n.reads) > 0 {
		n.broadcastAppend(n.hbSeq)
	}
	return nil
}

func (n *Node) releaseReads() {
	if len(n.reads) == 0 {
		return
	}
	acks := make([]uint64, 0, len(n.members))
	if n.promotable() {
		acks = append(acks, n.hbSeq)
	}
	for id, pr := range n.prs {
		if _, ok := n.members[id]; ok {
			acks = append(acks, pr.ackSeq)
		}
	}
	sort.Slice(acks, func(i, j int) bool { return acks[i] > acks[j] })
	if len(acks) < n.quorum() {
		return
	}
	confirmed := acks[n.quorum()-1]
	i := 0
	for ; i < len(n.reads) && n.reads[i].seq <= confirmed; i++ {
		n.readyReads = append(n.readyReads, ReadState{Ctx: n.reads[i].ctx, Index: n.reads[i].index})
	}
	n.reads = n.reads[i:]
}

// Compact installs a snapshot covering the log through index, which must already
// have been applied, and discards the covered prefix.
func (n *Node) Compact(index uint64, data []byte) error {
	if index <= n.snap.Index || index > n.applied {
		return ErrCompacted
	}
	term, ok := n.termAt(index)
	if !ok {
		return ErrCompacted
	}
	s := Snapshot{Index: index, Term: term, Members: n.membersAt(index).Clone(), Data: data}
	keep := append([]Entry(nil), n.log[index-n.snap.Index:]...)
	must(n.store.SaveSnapshot(s, keep))
	n.log = keep
	n.snap = s
	return nil
}

// Ready drains pending output. Returned entries count as applied.
func (n *Node) Ready() Ready {
	r := Ready{
		Messages:    n.msgs,
		Snapshot:    n.snapToApply,
		Reads:       n.readyReads,
		FailedReads: n.failedReads,
	}
	n.msgs, n.snapToApply, n.readyReads, n.failedReads = nil, nil, nil, nil
	if n.commit > n.applied {
		r.Committed = append(r.Committed, n.log[n.applied-n.snap.Index:n.commit-n.snap.Index]...)
		n.applied = n.commit
	}
	return r
}

// Status returns a summary of the node's state.
func (n *Node) Status() Status {
	s := Status{
		ID: n.id, Role: n.role, Term: n.term, Leader: n.leader,
		Commit: n.commit, Applied: n.applied,
		LastIndex: n.lastIndex(), FirstIndex: n.snap.Index + 1,
		Transferee: n.transferee, Members: n.members.Clone(),
		LeaderReady: n.leaderReady(), LeaderStartIndex: n.leaderStart,
	}
	if n.role == Leader {
		s.Match = map[NodeID]uint64{n.id: n.lastIndex()}
		for id, pr := range n.prs {
			s.Match[id] = pr.match
		}
	}
	return s
}

// ---------------------------------------------------------------------------
// Sending
// ---------------------------------------------------------------------------

func (n *Node) send(m Message) {
	m.From = n.id
	if m.Term == 0 {
		m.Term = n.term
	}
	n.msgs = append(n.msgs, m)
}

// broadcastAppend sends AppendEntries to every peer. A nonzero seq marks the
// round as a heartbeat. Heartbeats carry the normal consistency check, so a
// follower that lost a pipelined message is detected and repaired by the next one.
func (n *Node) broadcastAppend(seq uint64) {
	for _, id := range n.members.IDs() {
		if id != n.id {
			n.sendAppend(id, seq)
		}
	}
}

func (n *Node) sendAppend(to NodeID, seq uint64) {
	pr := n.prs[to]
	if pr == nil {
		return
	}
	prevTerm, ok := n.termAt(pr.next - 1)
	if !ok {
		// The entries this follower needs were compacted away.
		if pr.snapWait > 0 {
			return
		}
		pr.snapWait = n.electionTicks
		s := n.snap
		n.send(Message{Type: MsgSnap, To: to, Snapshot: &s, Seq: seq})
		return
	}
	// Copy: a message may outlive this slice, and a later truncation followed by an
	// append would otherwise overwrite the entries of a message still in flight.
	es := append([]Entry(nil), n.entriesFrom(pr.next, n.maxBatch)...)
	n.send(Message{
		Type: MsgApp, To: to,
		PrevLogIndex: pr.next - 1, PrevLogTerm: prevTerm,
		Entries: es, Commit: n.commit, Seq: seq,
	})
	pr.next += uint64(len(es)) // optimistic; a rejection rewinds it
}

// ---------------------------------------------------------------------------
// Receiving
// ---------------------------------------------------------------------------

// Step processes one incoming message.
func (n *Node) Step(m Message) {
	if m.To != n.id {
		return
	}

	// Leader stickiness (Raft §6): ignore vote requests while we have recently
	// heard from a live leader, so that a removed or flaky server cannot depose a
	// healthy leader. A transfer election is exempt because it was requested.
	if m.Type == MsgVote && !m.Transfer && m.Term > n.term &&
		n.leader != None && n.electionElapsed < n.electionTicks {
		return
	}

	switch {
	case m.Term > n.term:
		var leader NodeID
		if m.Type == MsgApp || m.Type == MsgSnap {
			leader = m.From
		}
		n.becomeFollower(m.Term, leader)
	case m.Term < n.term:
		// A stale sender must learn the new term or it may never step down.
		switch m.Type {
		case MsgApp, MsgSnap:
			n.send(Message{Type: MsgAppResp, To: m.From, Success: false, Hint: m.PrevLogIndex + 1, Seq: 0})
		case MsgVote:
			n.send(Message{Type: MsgVoteResp, To: m.From, Granted: false})
		}
		return
	}

	switch m.Type {
	case MsgVote:
		n.handleVote(m)
	case MsgVoteResp:
		n.handleVoteResp(m)
	case MsgApp:
		n.handleAppend(m)
	case MsgSnap:
		n.handleSnapshot(m)
	case MsgAppResp:
		n.handleAppendResp(m)
	case MsgTimeoutNow:
		if n.role != Leader {
			n.campaign(true)
		}
	}
}

func (n *Node) handleVote(m Message) {
	free := n.vote == m.From || (n.vote == None && n.leader == None)
	upToDate := m.LastLogTerm > n.lastTerm() ||
		(m.LastLogTerm == n.lastTerm() && m.LastLogIndex >= n.lastIndex())
	if !free || !upToDate {
		n.send(Message{Type: MsgVoteResp, To: m.From, Granted: false})
		return
	}
	if n.vote != m.From {
		n.vote = m.From
		n.persistHard() // durable before the reply can leave
	}
	n.resetElectionTimer()
	n.send(Message{Type: MsgVoteResp, To: m.From, Granted: true})
}

func (n *Node) handleVoteResp(m Message) {
	if n.role != Candidate {
		return
	}
	n.votes[m.From] = m.Granted
	if n.countVotes() >= n.quorum() {
		n.becomeLeader()
	}
}

func (n *Node) handleAppend(m Message) {
	if n.role == Leader {
		// Two leaders in one term is the catastrophic state this whole project
		// exists to exclude. Fail loudly rather than corrupt data quietly.
		panic(fmt.Sprintf("raft: election safety violated: %s and %s are both leader in term %d", n.id, m.From, n.term))
	}
	if n.role == Candidate {
		n.becomeFollower(n.term, m.From)
	}
	n.leader = m.From
	n.resetElectionTimer()

	resp := Message{Type: MsgAppResp, To: m.From, Seq: m.Seq, SentAt: m.SentAt}

	prev, prevTerm, entries := m.PrevLogIndex, m.PrevLogTerm, m.Entries
	if prev < n.snap.Index {
		// The prefix is inside our snapshot, so it is committed and therefore
		// matches the leader by Leader Completeness. Skip to where we can check.
		skip := n.snap.Index - prev
		if skip >= uint64(len(entries)) {
			entries = nil
		} else {
			entries = entries[skip:]
		}
		prev, prevTerm = n.snap.Index, n.snap.Term
	}

	if t, ok := n.termAt(prev); !ok || t != prevTerm {
		resp.Success = false
		if !ok {
			resp.Hint = n.lastIndex() + 1
		} else {
			// Skip back over the whole conflicting term in one round trip.
			h := prev
			for h > n.snap.Index+1 {
				if pt, _ := n.termAt(h - 1); pt != t {
					break
				}
				h--
			}
			resp.Hint = h
		}
		n.send(resp)
		return
	}

	for i, e := range entries {
		if e.Index <= n.lastIndex() {
			if t, _ := n.termAt(e.Index); t == e.Term {
				continue
			}
			n.truncateLocal(e.Index)
		}
		n.appendLocal(entries[i:]...)
		break
	}

	match := prev + uint64(len(entries))
	// Only what this message verified may be committed. Our log may hold unverified
	// entries past match, and those may yet be overwritten.
	if c := min(m.Commit, match); c > n.commit {
		n.commit = c
	}
	resp.Success = true
	resp.Match = match
	n.send(resp)
}

func (n *Node) handleSnapshot(m Message) {
	if n.role == Leader {
		panic(fmt.Sprintf("raft: election safety violated: %s and %s are both leader in term %d", n.id, m.From, n.term))
	}
	if n.role == Candidate {
		n.becomeFollower(n.term, m.From)
	}
	n.leader = m.From
	n.resetElectionTimer()

	s := m.Snapshot
	resp := Message{Type: MsgAppResp, To: m.From, Seq: m.Seq, SentAt: m.SentAt, Success: true}
	if s.Index <= n.commit {
		resp.Match = n.commit
		n.send(resp)
		return
	}
	if t, ok := n.termAt(s.Index); ok && t == s.Term {
		// We already hold this prefix, so it is enough to learn that it committed.
		n.commit = s.Index
		resp.Match = s.Index
		n.send(resp)
		return
	}
	must(n.store.SaveSnapshot(*s, nil))
	n.snap = *s
	n.log = nil
	n.commit = s.Index
	n.applied = s.Index
	n.snapToApply = s
	n.refreshMembers()
	resp.Match = s.Index
	n.send(resp)
}

func (n *Node) handleAppendResp(m Message) {
	if n.role != Leader {
		return
	}
	pr := n.prs[m.From]
	if pr == nil {
		return
	}
	pr.recentActive = true
	if m.Seq > pr.ackSeq {
		pr.ackSeq = m.Seq
		n.releaseReads()
	}

	if !m.Success {
		// Never rewind below what is known to match; a stale rejection can arrive
		// after later successes.
		pr.next = max(pr.match+1, min(m.Hint, n.lastIndex()+1))
		pr.snapWait = 0
		n.sendAppend(m.From, 0)
		return
	}

	if m.Match > pr.match {
		pr.match = m.Match
	}
	if pr.next <= pr.match {
		pr.next = pr.match + 1
	}
	if n.maybeCommit() {
		n.broadcastAppend(0)
	} else if pr.next <= n.lastIndex() {
		n.sendAppend(m.From, 0)
	}
	if n.role == Leader && n.transferee == m.From {
		n.maybeSendTimeoutNow()
	}
}

// maybeCommit advances the commit index to the highest entry stored on a majority.
// A leader may only count replicas of entries from its own term: committing an
// older-term entry this way is unsafe (Figure 8 of the Raft paper, reproduced by
// the model checker's BugCommitOldTerm).
func (n *Node) maybeCommit() bool {
	matches := make([]uint64, 0, len(n.members))
	for _, id := range n.members.IDs() {
		if id == n.id {
			matches = append(matches, n.lastIndex())
		} else if pr := n.prs[id]; pr != nil {
			matches = append(matches, pr.match)
		} else {
			matches = append(matches, 0)
		}
	}
	sort.Slice(matches, func(i, j int) bool { return matches[i] > matches[j] })
	N := matches[n.quorum()-1]
	if N <= n.commit {
		return false
	}
	if t, ok := n.termAt(N); !ok || t != n.term {
		return false
	}
	n.commit = N
	// A leader removed by a now-committed config entry steps aside.
	if !n.promotable() && n.configIndex <= n.commit {
		n.becomeFollower(n.term, None)
	}
	return true
}
