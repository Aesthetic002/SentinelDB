// Package modelcheck is an explicit-state model checker for the Raft protocol as used
// by SentinelDB.
//
// Each node is a finite automaton: a fixed set of local states and a transition function
// over a fixed alphabet of events (see step). The network is one more automaton, whose
// state is the set of messages that have been sent and could still have an effect. The
// cluster is their product, whose states are tuples of those local states. Check explores
// every product state reachable from the initial one and tests the safety properties at
// each. Because the model is finite, this search terminates, so "can it reach a bad
// state?" is decidable here in a way it is not for arbitrary programs.
//
// A tuple of bare roles would not be enough to prove anything. Whether a second leader can
// arise depends on who voted for whom, in which term, and whose log is newer, so all of
// that is part of each node's automaton state.
//
// Wherever the model departs from the implementation it does so on the side of allowing
// MORE behaviour, so that a safety result covers the implementation's real behaviour:
//   - The network may lose, delay, duplicate and reorder any message: a message stays
//     deliverable from the set any number of times, or is simply never delivered.
//   - A crash at any moment is a restart keeping only persistent state. A long-dead node
//     needs no extra state, since it behaves like one that is never scheduled.
//   - The failure predictor is not modelled as accurate, or as anything at all: any leader
//     may hand off to any node at any time. So the safety result holds regardless of how
//     good or bad the classifier is.
//   - Leaders may step down at any time, and the no-op a real leader appends at the start
//     of its term is merely one possible client request.
//   - A node may learn any term already in use at any time, covering every stale reply.
//
// AppendEntries and TimeoutNow are not stored as individual messages, because every subset
// of messages in flight would be a separate state, for no gain. Within a term the leader's
// log only grows, so every AppendEntries it could ever have sent is fully determined by
// its log as of now: one starting at any position, with any commit value it has reached.
// The model records that per term (TermLog, TermCommit) and treats all of them as always
// deliverable. This loses nothing and adds nothing: any such message could really have been
// sent an instant before the leader stepped down, and then delayed arbitrarily.
package modelcheck

import (
	"fmt"
	"sort"
	"strings"
)

const (
	// MaxNodes and MaxLog size the fixed arrays. They are array bounds, not what a run
	// explores; Bounds selects that.
	MaxNodes = 5
	MaxLog   = 4
)

// Roles.
const (
	Follower uint8 = iota
	Candidate
	Leader
)

// RoleName names a role.
func RoleName(r uint8) string { return [...]string{"Follower", "Candidate", "Leader"}[r] }

const noNode = int8(-1)

// Bug injects a known protocol mistake. The checker finding each of these is the evidence
// that a clean result on the correct protocol means something.
type Bug int

const (
	NoBug Bug = iota
	// BugIgnoreVote lets a node vote for several candidates in one term.
	BugIgnoreVote
	// BugForgetVote loses the vote across a restart, as if it had not been persisted.
	BugForgetVote
	// BugSmallQuorum wins elections and commits with half the nodes instead of a majority.
	BugSmallQuorum
	// BugIgnoreLogFreshness grants votes to candidates with out-of-date logs.
	BugIgnoreLogFreshness
	// BugCommitOldTerm counts replicas of older-term entries (Figure 8 of the Raft paper).
	BugCommitOldTerm
	// BugTransferNoTermBump has a transfer target take over without a new term.
	BugTransferNoTermBump
	// BugStaleAppend accepts AppendEntries from an older term.
	BugStaleAppend
	// BugCommitUnverified has a follower trust the leader's commit index past what the
	// message actually verified.
	BugCommitUnverified
)

// Bugs lists every injectable bug with its name.
var Bugs = map[string]Bug{
	"ignore-vote":          BugIgnoreVote,
	"forget-vote":          BugForgetVote,
	"small-quorum":         BugSmallQuorum,
	"ignore-log-freshness": BugIgnoreLogFreshness,
	"commit-old-term":      BugCommitOldTerm,
	"transfer-no-bump":     BugTransferNoTermBump,
	"stale-append":         BugStaleAppend,
	"commit-unverified":    BugCommitUnverified,
}

// Bounds fix the finite model.
type Bounds struct {
	Nodes int
	Terms int // highest term reachable
	Log   int // highest log length; 0 models elections only
	// Transfers enables leadership transfer.
	Transfers bool
	// Restarts enables crash-restart.
	Restarts bool
	// MaxMessages caps messages in flight, or 0 for no cap. A cap makes the result weaker,
	// since behaviours needing more in flight are not explored, and is reported as such.
	MaxMessages int
	Bug         Bug
	// allPerms tries every renaming, for testing the faster path against it.
	allPerms bool
}

func (b Bounds) String() string {
	s := fmt.Sprintf("%d nodes, terms<=%d", b.Nodes, b.Terms)
	if b.Log > 0 {
		s += fmt.Sprintf(", log<=%d", b.Log)
	} else {
		s += ", elections only"
	}
	if b.Transfers {
		s += ", transfers"
	}
	if b.Restarts {
		s += ", restarts"
	}
	if b.MaxMessages > 0 {
		s += fmt.Sprintf(", <=%d messages in flight", b.MaxMessages)
	}
	return s
}

func (b Bounds) quorum() int {
	if b.Bug == BugSmallQuorum {
		return b.Nodes / 2
	}
	return b.Nodes/2 + 1
}

// NodeState is one node's automaton state.
type NodeState struct {
	Role   uint8
	Term   uint8
	Vote   int8
	Votes  uint8 // candidate: bitmask of voters
	LogLen uint8
	Log    [MaxLog]uint8 // the term of each entry
	Commit uint8
	Match  [MaxNodes]uint8 // leader: what each follower is known to hold
}

// Message kinds. These are the messages kept as explicit network state; see the package
// comment for why AppendEntries and TimeoutNow do not need to be.
const (
	MsgVoteReq uint8 = iota + 1
	MsgVoteResp
	MsgAppendResp
)

// Msg is a message in the network. Vote requests have To == broadcast: delivering one to
// any subset of nodes is then a matter of which deliveries happen.
type Msg struct {
	Kind, Term uint8
	From, To   int8
	// VoteReq: Prev and PrevTerm are the candidate's last log index and term.
	// AppendResp: Prev is the match index.
	Prev, PrevTerm uint8
}

// Delivery describes a symbolic AppendEntries or TimeoutNow being delivered, for traces.
type Delivery struct {
	Term, Prev, PrevTerm, Entry, Commit uint8
	From, To                            int8
}

const broadcast = int8(-2)

// committedEntry records an entry's commitment, for the safety checks.
type committedEntry struct {
	EntryTerm uint8 // 0 means not committed
	ByTerm    uint8 // the term of the leader that committed it
}

// State is one state of the product automaton, plus history variables that exist only so
// that properties about the past can be checked at a single state.
type State struct {
	Nodes [MaxNodes]NodeState
	Msgs  []Msg
	// LeaderOf[t] is who became leader of term t, or noNode.
	LeaderOf [16]int8
	// TermLen and TermLog hold the log of the leader of each term, and TermCommit the
	// highest commit index it reached. See the package comment.
	TermLen    [16]uint8
	TermLog    [16][MaxLog]uint8
	TermCommit [16]uint8
	// Committed[i] records what is committed at index i+1.
	Committed [MaxLog]committedEntry
	// SplitTerm is set if two nodes have ever led one term.
	SplitTerm uint8
	// LostCommit is set if a node truncated a committed entry from its log.
	LostCommit bool
}

// Initial is the natural starting state: every node a follower, before any election.
func Initial(b Bounds) *State {
	s := &State{}
	for i := 0; i < b.Nodes; i++ {
		s.Nodes[i].Vote = noNode
	}
	for i := range s.LeaderOf {
		s.LeaderOf[i] = noNode
	}
	return s
}

func (s *State) clone() *State {
	c := *s
	c.Msgs = append([]Msg(nil), s.Msgs...)
	return &c
}

func (s *State) addMsg(m Msg) {
	for _, x := range s.Msgs {
		if x == m {
			return
		}
	}
	s.Msgs = append(s.Msgs, m)
}

func (s *State) removeMsg(m Msg) {
	for i, x := range s.Msgs {
		if x == m {
			s.Msgs = append(s.Msgs[:i], s.Msgs[i+1:]...)
			return
		}
	}
}

func (n *NodeState) lastTerm() uint8 {
	if n.LogLen == 0 {
		return 0
	}
	return n.Log[n.LogLen-1]
}

// Action is one labelled transition. Event is the input symbol from the per-node alphabet,
// used to produce the DFA diagram and readable counterexamples.
type Action struct {
	Event EventKind
	Node  int8
	Msg   Msg
	Del   Delivery
	Arg   uint8
}

// EventKind is the alphabet of the per-node automaton.
type EventKind uint8

const (
	EvTimeout EventKind = iota
	EvVoteRequest
	EvVoteGranted
	EvMajority
	EvHigherTerm
	EvAppendEntries
	EvAppendResponse
	EvClientRequest
	EvRestart
	EvStepDown
	EvTimeoutNow
)

// EventNames labels the alphabet.
var EventNames = map[EventKind]string{
	EvTimeout:        "timeout",
	EvVoteRequest:    "vote-request",
	EvVoteGranted:    "vote-granted",
	EvMajority:       "majority-achieved",
	EvHigherTerm:     "saw-higher-term",
	EvAppendEntries:  "received-append-entries",
	EvAppendResponse: "append-response",
	EvClientRequest:  "client-request",
	EvRestart:        "restart",
	EvStepDown:       "lost-quorum",
	EvTimeoutNow:     "timeout-now",
}

func (a Action) String() string {
	n := fmt.Sprintf("n%d", a.Node+1)
	switch a.Event {
	case EvTimeout:
		return n + " times out and starts an election"
	case EvVoteRequest:
		return fmt.Sprintf("%s receives RequestVote from n%d (term %d, log %d@%d)", n, a.Msg.From+1, a.Msg.Term, a.Msg.Prev, a.Msg.PrevTerm)
	case EvVoteGranted:
		return fmt.Sprintf("%s receives a vote from n%d (term %d)", n, a.Msg.From+1, a.Msg.Term)
	case EvHigherTerm:
		return fmt.Sprintf("%s learns of term %d", n, a.Arg)
	case EvAppendEntries:
		entry := "heartbeat"
		if a.Del.Entry != 0 {
			entry = fmt.Sprintf("entry[%d]=term %d", a.Del.Prev+1, a.Del.Entry)
		}
		return fmt.Sprintf("%s receives AppendEntries from n%d (term %d, prev %d@%d, %s, commit %d)",
			n, a.Del.From+1, a.Del.Term, a.Del.Prev, a.Del.PrevTerm, entry, a.Del.Commit)
	case EvAppendResponse:
		return fmt.Sprintf("%s learns n%d matches through index %d", n, a.Msg.From+1, a.Msg.Prev)
	case EvClientRequest:
		return n + " appends a client request"
	case EvRestart:
		return n + " crashes and restarts"
	case EvStepDown:
		return n + " steps down as leader"
	case EvTimeoutNow:
		return fmt.Sprintf("%s receives TimeoutNow from n%d (the predictor fired) and takes over", n, a.Del.From+1)
	}
	return "?"
}

type successor struct {
	st  *State
	act Action
	// roleEdge records a change of role, for the automaton diagram.
	from, to uint8
	changed  bool
}

// campaign is the Candidate transition.
func (b *Bounds) campaign(s *State, i int) {
	n := &s.Nodes[i]
	n.Term++
	n.Role = Candidate
	n.Vote = int8(i)
	n.Votes = 1 << i
	n.Match = [MaxNodes]uint8{}
	if b.quorum() <= 1 {
		b.becomeLeader(s, i)
		return
	}
	s.addMsg(Msg{Kind: MsgVoteReq, Term: n.Term, From: int8(i), To: broadcast, Prev: n.LogLen, PrevTerm: n.lastTerm()})
}

func (b *Bounds) becomeLeader(s *State, i int) {
	n := &s.Nodes[i]
	n.Role = Leader
	n.Votes = 0
	if who := s.LeaderOf[n.Term]; who != noNode && who != int8(i) {
		s.SplitTerm = n.Term
	}
	s.LeaderOf[n.Term] = int8(i)
	s.TermLen[n.Term] = n.LogLen
	s.TermLog[n.Term] = n.Log
}

func (n *NodeState) adoptTerm(t uint8) {
	n.Term = t
	n.Role = Follower
	n.Vote = noNode
	n.Votes = 0
}

// successors computes every transition enabled in s. The per-node step logic for each
// event is the DFA's transition function.
func (b *Bounds) successors(s *State) []successor {
	var out []successor
	add := func(ns *State, act Action) {
		b.gc(ns)
		if b.MaxMessages > 0 && len(ns.Msgs) > b.MaxMessages {
			return
		}
		from, to := s.Nodes[act.Node].Role, ns.Nodes[act.Node].Role
		out = append(out, successor{st: ns, act: act, from: from, to: to, changed: from != to})
	}

	maxTerm := uint8(0)
	for i := 0; i < b.Nodes; i++ {
		if t := s.Nodes[i].Term; t > maxTerm {
			maxTerm = t
		}
	}

	for i := 0; i < b.Nodes; i++ {
		n := &s.Nodes[i]
		id := int8(i)

		// timeout: a non-leader starts an election.
		if n.Role != Leader && int(n.Term) < b.Terms {
			ns := s.clone()
			b.campaign(ns, i)
			add(ns, Action{Event: EvTimeout, Node: id})
		}

		// saw-higher-term: any term in use may reach any node at any time.
		for t := n.Term + 1; t <= maxTerm; t++ {
			ns := s.clone()
			ns.Nodes[i].adoptTerm(t)
			add(ns, Action{Event: EvHigherTerm, Node: id, Arg: t})
		}

		if b.Restarts {
			// A restart with nothing volatile to lose is a no-op, so skip it.
			if n.Role != Follower || n.Commit != 0 || n.Votes != 0 || (b.Bug == BugForgetVote && n.Vote != noNode) {
				ns := s.clone()
				m := &ns.Nodes[i]
				m.Role, m.Votes, m.Commit = Follower, 0, 0
				m.Match = [MaxNodes]uint8{}
				if b.Bug == BugForgetVote {
					m.Vote = noNode
				}
				add(ns, Action{Event: EvRestart, Node: id})
			}
		}

		if n.Role == Leader {
			// lost-quorum: CheckQuorum, or any other reason to step down, at any time.
			ns := s.clone()
			ns.Nodes[i].Role = Follower
			add(ns, Action{Event: EvStepDown, Node: id})

			// client-request.
			if int(n.LogLen) < b.Log {
				ns := s.clone()
				m := &ns.Nodes[i]
				m.Log[m.LogLen] = m.Term
				m.LogLen++
				ns.TermLen[m.Term] = m.LogLen
				ns.TermLog[m.Term] = m.Log
				add(ns, Action{Event: EvClientRequest, Node: id})
			}
		}
	}

	// Every AppendEntries and TimeoutNow any leader of any term could have sent.
	for t := uint8(1); int(t) <= b.Terms; t++ {
		leader := s.LeaderOf[t]
		if leader == noNode {
			continue
		}
		for j := 0; j < b.Nodes; j++ {
			if int8(j) == leader {
				continue
			}
			// predictor-requests-transfer: however wrong, at any moment, to anyone.
			if b.Transfers {
				if ns := b.deliverTimeoutNow(s, t, leader, j); ns != nil {
					add(ns, Action{Event: EvTimeoutNow, Node: int8(j), Del: Delivery{Term: t, From: leader, To: int8(j)}})
				}
			}
			for prev := uint8(0); prev <= s.TermLen[t]; prev++ {
				d := Delivery{Term: t, From: leader, To: int8(j), Prev: prev}
				if prev > 0 {
					d.PrevTerm = s.TermLog[t][prev-1]
				}
				if prev < s.TermLen[t] {
					d.Entry = s.TermLog[t][prev]
				}
				// Each commit value the leader reached is a distinct possible message.
				for c := uint8(0); c <= s.TermCommit[t]; c++ {
					d.Commit = c
					if ns := b.deliverAppend(s, j, d); ns != nil {
						add(ns, Action{Event: EvAppendEntries, Node: int8(j), Del: d})
					}
				}
			}
		}
	}

	for _, m := range s.Msgs {
		switch m.Kind {
		case MsgVoteReq:
			for v := 0; v < b.Nodes; v++ {
				if int8(v) == m.From {
					continue
				}
				if ns := b.deliverVoteReq(s, v, m); ns != nil {
					add(ns, Action{Event: EvVoteRequest, Node: int8(v), Msg: m})
				}
			}
		case MsgVoteResp:
			if ns := b.deliverVoteResp(s, m); ns != nil {
				add(ns, Action{Event: EvVoteGranted, Node: m.To, Msg: m})
			}
		case MsgAppendResp:
			if ns := b.deliverAppendResp(s, m); ns != nil {
				add(ns, Action{Event: EvAppendResponse, Node: m.To, Msg: m})
			}
		}
	}
	return out
}

func (b *Bounds) deliverVoteReq(s *State, v int, m Msg) *State {
	n := &s.Nodes[v]
	if m.Term < n.Term {
		return nil
	}
	ns := s.clone()
	nv := &ns.Nodes[v]
	if m.Term > nv.Term {
		nv.adoptTerm(m.Term)
	}
	free := nv.Vote == noNode || nv.Vote == m.From
	if b.Bug == BugIgnoreVote {
		free = true
	}
	fresh := m.PrevTerm > nv.lastTerm() || (m.PrevTerm == nv.lastTerm() && m.Prev >= nv.LogLen)
	if b.Bug == BugIgnoreLogFreshness {
		fresh = true
	}
	if free && fresh && nv.Role != Leader {
		nv.Vote = m.From
		ns.addMsg(Msg{Kind: MsgVoteResp, Term: m.Term, From: int8(v), To: m.From})
	}
	if statesEqual(s, ns) {
		return nil
	}
	return ns
}

func (b *Bounds) deliverVoteResp(s *State, m Msg) *State {
	c := &s.Nodes[m.To]
	if c.Role != Candidate || c.Term != m.Term {
		return nil
	}
	ns := s.clone()
	ns.removeMsg(m)
	nc := &ns.Nodes[m.To]
	nc.Votes |= 1 << m.From
	if popcount(nc.Votes) >= b.quorum() {
		b.becomeLeader(ns, int(m.To))
	}
	return ns
}

func popcount(x uint8) int {
	c := 0
	for ; x != 0; x &= x - 1 {
		c++
	}
	return c
}

func (b *Bounds) deliverAppend(s *State, j int, d Delivery) *State {
	n := &s.Nodes[j]
	if d.Term < n.Term && b.Bug != BugStaleAppend {
		return nil
	}
	ns := s.clone()
	nv := &ns.Nodes[j]
	if d.Term > nv.Term {
		nv.adoptTerm(d.Term)
	} else if nv.Role == Candidate && d.Term == nv.Term {
		nv.Role = Follower
		nv.Votes = 0
	}
	if nv.Role == Leader && d.Term == nv.Term {
		// Two leaders in one term. SplitTerm has already flagged this.
		return nil
	}
	if b.Log == 0 {
		if statesEqual(s, ns) {
			return nil
		}
		return ns
	}
	if d.Prev > nv.LogLen || (d.Prev > 0 && nv.Log[d.Prev-1] != d.PrevTerm) {
		// Rejection. Its only effect on a real leader is to change where it sends from
		// next, and every starting position here is already deliverable.
		if statesEqual(s, ns) {
			return nil
		}
		return ns
	}
	match := d.Prev
	if d.Entry != 0 {
		match = d.Prev + 1
		if nv.LogLen >= match {
			if nv.Log[match-1] != d.Entry {
				if nv.Commit >= match {
					ns.LostCommit = true
				}
				nv.Log[match-1] = d.Entry
				for k := match; k < nv.LogLen; k++ {
					nv.Log[k] = 0
				}
				nv.LogLen = match
			}
		} else {
			nv.Log[match-1] = d.Entry
			nv.LogLen = match
		}
	}
	verified := match
	if b.Bug == BugCommitUnverified {
		verified = nv.LogLen
	}
	if c := min(d.Commit, verified); c > nv.Commit {
		nv.Commit = c
	}
	if match > 0 {
		ns.addMsg(Msg{Kind: MsgAppendResp, Term: d.Term, From: int8(j), To: d.From, Prev: match})
	}
	b.gc(ns)
	if statesEqual(s, ns) {
		return nil
	}
	return ns
}

func (b *Bounds) deliverAppendResp(s *State, m Msg) *State {
	n := &s.Nodes[m.To]
	if n.Role != Leader || n.Term != m.Term || n.Match[m.From] >= m.Prev {
		return nil
	}
	ns := s.clone()
	ns.removeMsg(m)
	nl := &ns.Nodes[m.To]
	nl.Match[m.From] = m.Prev
	b.advanceCommit(ns, int(m.To))
	return ns
}

// advanceCommit applies the commit rule: a majority holds the entry, and it is from this
// leader's own term.
func (b *Bounds) advanceCommit(s *State, i int) {
	n := &s.Nodes[i]
	for idx := n.LogLen; idx > n.Commit; idx-- {
		count := 1 // the leader itself
		for j := 0; j < b.Nodes; j++ {
			if j != i && n.Match[j] >= idx {
				count++
			}
		}
		if count < b.quorum() {
			continue
		}
		if n.Log[idx-1] != n.Term && b.Bug != BugCommitOldTerm {
			continue
		}
		for k := n.Commit + 1; k <= idx; k++ {
			if s.Committed[k-1].EntryTerm == 0 {
				s.Committed[k-1] = committedEntry{EntryTerm: n.Log[k-1], ByTerm: n.Term}
			}
		}
		n.Commit = idx
		s.TermCommit[n.Term] = idx
		return
	}
}

func (b *Bounds) deliverTimeoutNow(s *State, t uint8, from int8, j int) *State {
	n := &s.Nodes[j]
	if t < n.Term || n.Role == Leader {
		return nil
	}
	ns := s.clone()
	nv := &ns.Nodes[j]
	if t > nv.Term {
		nv.adoptTerm(t)
	}
	if b.Bug == BugTransferNoTermBump {
		// Takes over at once in the sender's term, skipping the election altogether.
		nv.Vote = int8(j)
		b.becomeLeader(ns, j)
		return ns
	}
	if int(nv.Term) >= b.Terms {
		return nil
	}
	b.campaign(ns, j)
	return ns
}

// gc removes messages that can no longer affect anything, keeping the search finite in
// practice without losing behaviours. A node's term never decreases and it ignores lower
// terms, so such a message is inert forever. Anything a real stale reply could teach its
// sender is already covered by the saw-higher-term event.
func (b *Bounds) gc(s *State) {
	kept := s.Msgs[:0]
	for _, m := range s.Msgs {
		if b.live(s, m) {
			kept = append(kept, m)
		}
	}
	s.Msgs = kept
}

func (b *Bounds) live(s *State, m Msg) bool {
	switch m.Kind {
	case MsgVoteReq:
		for v := 0; v < b.Nodes; v++ {
			if int8(v) != m.From && s.Nodes[v].Term <= m.Term {
				return true
			}
		}
		return false
	case MsgVoteResp:
		c := &s.Nodes[m.To]
		return c.Role == Candidate && c.Term == m.Term && c.Votes&(1<<m.From) == 0
	case MsgAppendResp:
		n := &s.Nodes[m.To]
		return n.Role == Leader && n.Term == m.Term && n.Match[m.From] < m.Prev
	}
	return false
}

func statesEqual(a, b *State) bool {
	if a.Nodes != b.Nodes || a.LeaderOf != b.LeaderOf || a.Committed != b.Committed ||
		a.TermLen != b.TermLen || a.TermLog != b.TermLog || a.TermCommit != b.TermCommit ||
		a.SplitTerm != b.SplitTerm || a.LostCommit != b.LostCommit || len(a.Msgs) != len(b.Msgs) {
		return false
	}
	for i := range a.Msgs {
		if a.Msgs[i] != b.Msgs[i] {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------------------
// Canonical form: symmetry reduction
// ---------------------------------------------------------------------------

// Nodes are interchangeable, so states differing only by a renaming of nodes behave
// identically, and exploring one of them is enough. key encodes a state under its
// smallest renaming, so that all such states share one key.
func (b *Bounds) key(s *State, perms [][]int8, buf []byte) string {
	if len(perms) == 0 {
		return string(b.encode(s, identity(b.Nodes), buf))
	}
	sig := make([]uint64, b.Nodes)
	if !b.allPerms {
		for i := range sig {
			sig[i] = s.Nodes[i].signature()
		}
	}
	var best []byte
	for _, p := range perms {
		if !b.allPerms && !sortsBySignature(p, sig) {
			continue
		}
		e := b.encode(s, p, buf)
		if best == nil || string(e) < string(best) {
			best = append(best[:0], e...)
		}
	}
	return string(best)
}

// signature summarizes the part of a node's state that does not mention other nodes, so
// renaming nodes cannot change it. Only renamings that put nodes in signature order are
// tried in key. That is still canonical, because whether a renaming qualifies depends only
// on the state's equivalence class, so equivalent states try equivalent renamings. And it
// leaves only a few per state to try, out of the 120 that exist for 5 nodes.
func (n *NodeState) signature() uint64 {
	v := uint64(n.Role)<<40 | uint64(n.Term)<<32 | uint64(n.LogLen)<<28 | uint64(n.Commit)<<24
	v |= uint64(popcount(n.Votes)) << 20
	if n.Vote != noNode {
		v |= 1 << 16
	}
	for i, t := range n.Log {
		v |= uint64(t&0xf) << (4 * i)
	}
	return v
}

func sortsBySignature(p []int8, sig []uint64) bool {
	for a := range sig {
		for c := range sig {
			if sig[a] < sig[c] && p[a] > p[c] {
				return false
			}
		}
	}
	return true
}

func identity(n int) []int8 {
	p := make([]int8, n)
	for i := range p {
		p[i] = int8(i)
	}
	return p
}

func permutations(n int) [][]int8 {
	var out [][]int8
	p := identity(n)
	var rec func(int)
	rec = func(k int) {
		if k == n {
			out = append(out, append([]int8(nil), p...))
			return
		}
		for i := k; i < n; i++ {
			p[k], p[i] = p[i], p[k]
			rec(k + 1)
			p[k], p[i] = p[i], p[k]
		}
	}
	rec(0)
	return out
}

// encode serializes s with nodes renamed by p (p[old] = new).
func (b *Bounds) encode(s *State, p []int8, buf []byte) []byte {
	n := b.Nodes
	buf = buf[:0]
	inv := make([]int, n)
	for old, nw := range p {
		inv[nw] = old
	}
	mapNode := func(x int8) uint8 {
		if x < 0 {
			return 255 + uint8(x+1) // noNode -> 255, broadcast -> 254
		}
		return uint8(p[x])
	}
	for nw := 0; nw < n; nw++ {
		nd := &s.Nodes[inv[nw]]
		var votes uint8
		for v := 0; v < n; v++ {
			if nd.Votes&(1<<v) != 0 {
				votes |= 1 << p[v]
			}
		}
		buf = append(buf, nd.Role, nd.Term, mapNode(nd.Vote), votes, nd.LogLen)
		buf = append(buf, nd.Log[:b.Log]...)
		buf = append(buf, nd.Commit)
		for k := 0; k < n; k++ {
			buf = append(buf, nd.Match[inv[k]])
		}
	}
	for t := 0; t <= b.Terms; t++ {
		buf = append(buf, mapNode(s.LeaderOf[t]), s.TermLen[t], s.TermCommit[t])
		buf = append(buf, s.TermLog[t][:b.Log]...)
	}
	for _, c := range s.Committed[:b.Log] {
		buf = append(buf, c.EntryTerm, c.ByTerm)
	}
	buf = append(buf, s.SplitTerm)
	if s.LostCommit {
		buf = append(buf, 1)
	} else {
		buf = append(buf, 0)
	}
	// Messages are a set, so sort them to make equal sets encode equally.
	start := len(buf)
	for _, m := range s.Msgs {
		buf = append(buf, m.Kind, m.Term, mapNode(m.From), mapNode(m.To), m.Prev, m.PrevTerm)
	}
	sortChunks(buf[start:], 6)
	return buf
}

func sortChunks(b []byte, size int) {
	n := len(b) / size
	if n < 2 {
		return
	}
	sort.Sort(&chunks{b, size, make([]byte, size)})
}

type chunks struct {
	b    []byte
	size int
	tmp  []byte
}

func (c *chunks) Len() int { return len(c.b) / c.size }
func (c *chunks) Less(i, j int) bool {
	return string(c.b[i*c.size:(i+1)*c.size]) < string(c.b[j*c.size:(j+1)*c.size])
}
func (c *chunks) Swap(i, j int) {
	a, d := c.b[i*c.size:(i+1)*c.size], c.b[j*c.size:(j+1)*c.size]
	copy(c.tmp, a)
	copy(a, d)
	copy(d, c.tmp)
}

// ---------------------------------------------------------------------------
// Display
// ---------------------------------------------------------------------------

// Summary renders a state on one line, for counterexample traces.
func (s *State) Summary(b Bounds) string {
	parts := make([]string, b.Nodes)
	for i := 0; i < b.Nodes; i++ {
		n := &s.Nodes[i]
		r := RoleName(n.Role)[:1]
		vote := "-"
		if n.Vote >= 0 {
			vote = fmt.Sprintf("n%d", n.Vote+1)
		}
		parts[i] = fmt.Sprintf("n%d:%s t%d v=%s", i+1, r, n.Term, vote)
		if b.Log > 0 {
			log := make([]string, n.LogLen)
			for k := uint8(0); k < n.LogLen; k++ {
				log[k] = fmt.Sprint(n.Log[k])
			}
			parts[i] += fmt.Sprintf(" log=[%s] c=%d", strings.Join(log, ","), n.Commit)
		}
	}
	return strings.Join(parts, " | ")
}
