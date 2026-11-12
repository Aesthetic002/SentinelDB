package raft

import (
	"errors"
	"math/rand"
	"testing"
)

func calmSim(t *testing.T, n int) *sim {
	t.Helper()
	return newSim(t, 7, simOpts{nodes: n, maxDelay: 1})
}

func (s *sim) waitLeader() *simNode {
	s.t.Helper()
	for i := 0; i < 2000; i++ {
		s.tick()
		if ld := s.leader(); ld != nil && ld.node.leaderReady() {
			return ld
		}
	}
	s.t.Fatal("no leader elected")
	return nil
}

func (s *sim) settle(ticks int) {
	for i := 0; i < ticks; i++ {
		s.tick()
	}
}

func TestElectsExactlyOneLeader(t *testing.T) {
	s := calmSim(t, 5)
	s.waitLeader()
	s.settle(50)
	leaders := 0
	for _, sn := range s.nodes {
		if sn.node.role == Leader {
			leaders++
		}
	}
	if leaders != 1 {
		t.Fatalf("got %d leaders", leaders)
	}
}

func TestCommitRequiresMajority(t *testing.T) {
	s := calmSim(t, 5)
	ld := s.waitLeader()
	s.settle(20)

	// Isolate the leader with one follower: a minority of two.
	var follower *simNode
	for _, sn := range s.nodes {
		if sn != ld {
			follower = sn
			break
		}
	}
	for _, sn := range s.nodes {
		s.group[sn.id] = 1
	}
	s.group[ld.id], s.group[follower.id] = 2, 2

	idx, _, err := ld.node.Propose([]byte("minority write"))
	if err != nil {
		t.Fatal(err)
	}
	s.drain(ld)
	for i := 0; i < 11; i++ { // short of CheckQuorum's step-down
		s.tick()
	}
	if ld.node.commit >= idx {
		t.Fatalf("committed index %d with only a minority of replicas", idx)
	}

	// The majority side elects a new leader; healing must discard the minority write.
	s.settle(200)
	s.group = map[NodeID]int{}
	nl := s.waitLeader()
	idx2, _, err := nl.node.Propose([]byte("majority write"))
	if err != nil {
		t.Fatal(err)
	}
	s.drain(nl)
	s.settle(200)
	got, _ := ld.node.termAt(idx)
	if ld.node.commit < idx2 {
		t.Fatalf("old leader did not catch up: commit %d, want >= %d", ld.node.commit, idx2)
	}
	if got == 0 {
		t.Fatalf("old leader lost its log")
	}
	for _, e := range ld.node.log {
		if string(e.Data) == "minority write" {
			t.Fatalf("uncommitted minority write survived on the old leader")
		}
	}
}

func TestVoteIsDurableAcrossRestart(t *testing.T) {
	st := &memStore{}
	members := Membership{"a": "", "b": "", "c": ""}
	mk := func() *Node {
		return NewNode(Config{
			ID: "a", Members: members, ElectionTicks: 10, HeartbeatTicks: 2,
			Rand: rand.New(rand.NewSource(1)).Intn, Storage: st,
			Initial: InitialState{Hard: st.hard, Snapshot: st.snap, Entries: st.log},
		})
	}
	n := mk()
	n.Step(Message{Type: MsgVote, From: "b", To: "a", Term: 5})
	if r := n.Ready(); len(r.Messages) != 1 || !r.Messages[0].Granted {
		t.Fatalf("expected a granted vote, got %+v", r.Messages)
	}

	n = mk() // crash and restart
	n.Step(Message{Type: MsgVote, From: "c", To: "a", Term: 5})
	if r := n.Ready(); len(r.Messages) != 1 || r.Messages[0].Granted {
		t.Fatalf("node voted twice in term 5 after a restart: %+v", r.Messages)
	}
	if n.term != 5 {
		t.Fatalf("term not recovered: %d", n.term)
	}
}

func TestStaleCandidateCannotWinVote(t *testing.T) {
	st := &memStore{log: []Entry{{Index: 1, Term: 3}}}
	n := NewNode(Config{
		ID: "a", Members: Membership{"a": "", "b": "", "c": ""}, ElectionTicks: 10, HeartbeatTicks: 2,
		Rand: rand.New(rand.NewSource(1)).Intn, Storage: st,
		Initial: InitialState{Hard: HardState{Term: 3}, Entries: st.log},
	})
	// Higher term but an older log: the second voting rule must refuse.
	n.Step(Message{Type: MsgVote, From: "b", To: "a", Term: 9, LastLogIndex: 5, LastLogTerm: 2})
	if r := n.Ready(); len(r.Messages) != 1 || r.Messages[0].Granted {
		t.Fatalf("granted a vote to a candidate with an out-of-date log: %+v", r.Messages)
	}
	if n.term != 9 {
		t.Fatalf("term should still advance to 9, got %d", n.term)
	}
}

func TestLeadershipTransfer(t *testing.T) {
	s := calmSim(t, 5)
	ld := s.waitLeader()
	s.settle(20)

	var target *simNode
	for _, sn := range s.nodes {
		if sn != ld {
			target = sn
			break
		}
	}
	oldTerm := ld.node.term
	if err := ld.node.TransferLeadership(target.id); err != nil {
		t.Fatal(err)
	}
	s.drain(ld)
	if _, _, err := ld.node.Propose([]byte("x")); !errors.Is(err, ErrTransferring) {
		t.Fatalf("proposals must be refused during a transfer, got %v", err)
	}

	// Well under one election timeout: the handover does not wait for a timer.
	for i := 0; i < 8 && target.node.role != Leader; i++ {
		s.tick()
	}
	if target.node.role != Leader {
		t.Fatalf("target did not take over promptly (role %v)", target.node.role)
	}
	if target.node.term != oldTerm+1 {
		t.Fatalf("expected exactly one term bump, %d -> %d", oldTerm, target.node.term)
	}
	s.settle(5)
	if ld.node.role == Leader {
		t.Fatalf("old leader did not step down")
	}
}

func TestTransferToLaggingFollowerWaitsForCatchUp(t *testing.T) {
	s := calmSim(t, 3)
	ld := s.waitLeader()
	s.settle(20)
	var target *simNode
	for _, sn := range s.nodes {
		if sn != ld {
			target = sn
			break
		}
	}
	s.group[target.id] = 9 // target misses these writes
	for i := 0; i < 5; i++ {
		if _, _, err := ld.node.Propose([]byte("w")); err != nil {
			t.Fatal(err)
		}
		s.drain(ld)
	}
	s.settle(5)
	s.group = map[NodeID]int{}
	if err := ld.node.TransferLeadership(target.id); err != nil {
		t.Fatal(err)
	}
	s.drain(ld)
	s.settle(10)
	if target.node.role != Leader {
		t.Fatalf("lagging target should catch up then lead, role %v", target.node.role)
	}
	if target.node.lastIndex() < ld.node.commit {
		t.Fatalf("new leader is missing committed entries")
	}
}

func TestFailedTransferIsAbandoned(t *testing.T) {
	s := calmSim(t, 5)
	ld := s.waitLeader()
	s.settle(20)
	var target *simNode
	for _, sn := range s.nodes {
		if sn != ld {
			target = sn
			break
		}
	}
	target.node = nil // the target is dead
	if err := ld.node.TransferLeadership(target.id); err != nil {
		t.Fatal(err)
	}
	s.drain(ld)
	s.settle(ld.node.electionTicks + 1)
	if ld.node.role != Leader {
		t.Fatalf("leader should survive a failed transfer")
	}
	if _, _, err := ld.node.Propose([]byte("x")); err != nil {
		t.Fatalf("proposals should resume once the transfer is abandoned: %v", err)
	}
}

func TestPartitionedLeaderCannotServeReads(t *testing.T) {
	s := calmSim(t, 5)
	ld := s.waitLeader()
	s.settle(20)
	s.group[ld.id] = 1 // cut the leader off
	s.reads[1] = s.maxCommitted
	if err := ld.node.ReadIndex(1); err != nil {
		t.Fatal(err)
	}
	s.drain(ld)
	s.settle(200)
	if s.stats.readsOK != 0 {
		t.Fatalf("a partitioned leader confirmed a read without a quorum")
	}
	if ld.node.role == Leader {
		t.Fatalf("CheckQuorum should have made the partitioned leader step down")
	}
	if s.stats.readsFailed != 1 {
		t.Fatalf("the abandoned read should be reported as failed (failed=%d)", s.stats.readsFailed)
	}
}

func TestSnapshotCatchUp(t *testing.T) {
	s := calmSim(t, 3)
	ld := s.waitLeader()
	s.settle(20)
	var lag *simNode
	for _, sn := range s.nodes {
		if sn != ld {
			lag = sn
			break
		}
	}
	s.group[lag.id] = 9
	for i := 0; i < 40; i++ {
		_, _, _ = ld.node.Propose([]byte("w"))
		s.drain(ld)
		s.tick()
	}
	s.settle(10)
	if err := ld.node.Compact(ld.machine.index, ld.machine.encode()); err != nil {
		t.Fatal(err)
	}
	want := ld.machine.index
	s.group = map[NodeID]int{}
	s.settle(100)
	if lag.machine.index < want {
		t.Fatalf("lagging follower at %d, want >= %d", lag.machine.index, want)
	}
	if lag.node.snap.Index == 0 {
		t.Fatalf("follower should have been caught up via InstallSnapshot")
	}
}

func TestMembershipChangeRules(t *testing.T) {
	s := calmSim(t, 3)
	ld := s.waitLeader()
	s.settle(20)
	next := ld.node.members.Clone()
	next["x"] = "addr"
	next["y"] = "addr"
	if _, _, err := ld.node.ProposeMembership(next); !errors.Is(err, ErrBadConfig) {
		t.Fatalf("adding two nodes at once must fail: %v", err)
	}
	if _, _, err := ld.node.ProposeMembership(ld.node.members.Clone()); !errors.Is(err, ErrBadConfig) {
		t.Fatalf("a no-op change must fail: %v", err)
	}
}

func TestLeaderStepsDownWhenRemoved(t *testing.T) {
	s := newSim(t, 3, simOpts{nodes: 5, initial: 5, maxDelay: 1})
	ld := s.waitLeader()
	s.settle(20)
	next := ld.node.members.Clone()
	delete(next, ld.id)
	if _, _, err := ld.node.ProposeMembership(next); err != nil {
		t.Fatal(err)
	}
	s.drain(ld)
	s.settle(300)
	if ld.node.role == Leader {
		t.Fatalf("removed leader kept leading")
	}
	nl := s.leader()
	if nl == nil || nl == ld {
		t.Fatalf("remaining members failed to elect a new leader")
	}
	if _, in := nl.node.members[ld.id]; in {
		t.Fatalf("new leader still counts the removed node")
	}
}

// TestFigure8 reproduces Figure 8 of the Raft paper against the real code. An entry
// from an older term reaches a majority, but the leader must not call it committed:
// a node holding a newer-term entry at that index can still win an election and
// overwrite it. Random chaos rarely finds this interleaving, so it is scripted.
func TestFigure8(t *testing.T) {
	members := Membership{"n1": "", "n2": "", "n3": "", "n4": "", "n5": ""}
	logs := map[NodeID][]Entry{
		"n1": {{Index: 1, Term: 1}, {Index: 2, Term: 2}},
		"n2": {{Index: 1, Term: 1}, {Index: 2, Term: 2}},
		"n3": {{Index: 1, Term: 1}},
		"n4": {{Index: 1, Term: 1}},
		"n5": {{Index: 1, Term: 1}, {Index: 2, Term: 3}},
	}
	nodes := map[NodeID]*Node{}
	for id, l := range logs {
		st := &memStore{log: append([]Entry(nil), l...)}
		term := uint64(3)
		if id == "n5" {
			term = 4 // so its next campaign is in term 5, above n1's term 4
		}
		nodes[id] = NewNode(Config{
			ID: id, Members: members, ElectionTicks: 10, HeartbeatTicks: 5,
			Rand: rand.New(rand.NewSource(1)).Intn, Storage: st, MaxBatchEntries: 1,
			Initial: InitialState{Hard: HardState{Term: term}, Entries: l},
		})
	}
	var inflight []Message
	collect := func(id NodeID) { inflight = append(inflight, nodes[id].Ready().Messages...) }
	// deliver routes queued messages until quiet, except those blocked says to drop.
	deliver := func(blocked func(Message) bool) {
		for len(inflight) > 0 {
			m := inflight[0]
			inflight = inflight[1:]
			if blocked(m) {
				continue
			}
			nodes[m.To].Step(m)
			collect(m.To)
		}
	}

	// n1 wins term 4 with votes from n2, n3 and n4. n5 never hears about it.
	nodes["n1"].campaign(false)
	collect("n1")
	toN5 := func(m Message) bool { return m.To == "n5" || m.From == "n5" }
	// Index 2 reaches n3 and n4, so with n1 it is on a majority. Everything that would
	// carry index 3 is held back, as is all append traffic to n2, so nothing from the
	// leader's own term is replicated. Only the current-term rule now prevents a commit.
	deliver(func(m Message) bool {
		if toN5(m) || (m.To == "n2" && m.Type == MsgApp) {
			return true
		}
		return m.Type == MsgApp && len(m.Entries) > 0 && m.Entries[0].Index == 3 && nodes[m.To].lastIndex() >= 2
	})

	n1 := nodes["n1"]
	if n1.role != Leader || n1.term != 4 {
		t.Fatalf("setup: n1 should lead term 4, got %v term %d", n1.role, n1.term)
	}
	if m := n1.prs["n3"].match; m != 2 {
		t.Fatalf("setup: n3 should hold exactly index 2, match=%d", m)
	}
	if m := n1.prs["n4"].match; m != 2 {
		t.Fatalf("setup: n4 should hold exactly index 2, match=%d", m)
	}
	if n1.commit >= 2 {
		t.Fatalf("FIGURE 8: leader of term 4 committed index 2 from term 2 by counting replicas")
	}

	// The reason it matters: n5 (last log term 3) can still win without n1, then
	// overwrite index 2. Had n1 acknowledged index 2, that write would now be lost.
	for _, id := range []NodeID{"n2", "n3", "n4"} {
		nodes[id].leader = None // as if n1 has now been silent for an election timeout
	}
	nodes["n5"].campaign(false)
	collect("n5")
	deliver(func(m Message) bool {
		return m.To == "n1" || m.From == "n1" || (m.Type == MsgApp && m.To != "n3")
	})
	n5 := nodes["n5"]
	if n5.role != Leader {
		t.Fatalf("n5 should be able to win with votes from n2, n3, n4: role %v", n5.role)
	}
	if got, _ := nodes["n3"].termAt(2); got != 3 {
		t.Fatalf("n3 should have had its uncommitted index 2 overwritten with term 3, got term %d", got)
	}
}

func TestSecondMembershipChangeWaitsForFirst(t *testing.T) {
	s := calmSim(t, 5)
	ld := s.waitLeader()
	s.settle(20)

	var victim NodeID
	for id := range ld.node.members {
		if id != ld.id {
			victim = id
			break
		}
	}
	first := ld.node.members.Clone()
	delete(first, victim)
	if _, _, err := ld.node.ProposeMembership(first); err != nil {
		t.Fatal(err)
	}
	s.drain(ld)

	second := first.Clone()
	second["n9"] = "addr"
	if _, _, err := ld.node.ProposeMembership(second); !errors.Is(err, ErrConfigPending) {
		t.Fatalf("overlapping membership changes must be refused: %v", err)
	}
	s.settle(20)
	if _, _, err := ld.node.ProposeMembership(second); err != nil {
		t.Fatalf("should be allowed once the first change committed: %v", err)
	}
	s.drain(ld)
}

func TestMembershipChangeRequiresCurrentTermCommit(t *testing.T) {
	s := calmSim(t, 3)
	ld := s.waitLeader()
	s.settle(20)
	ld.node.commit = 0 // as if this leader had not yet committed anything in its term
	next := ld.node.members.Clone()
	next["n9"] = "addr"
	if _, _, err := ld.node.ProposeMembership(next); !errors.Is(err, ErrLeaderNotInit) {
		t.Fatalf("a leader must commit in its own term before changing membership: %v", err)
	}
}
