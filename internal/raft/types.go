// Package raft implements the Raft consensus protocol (Ongaro & Ousterhout,
// USENIX ATC 2014) as a deterministic state machine.
//
// The core does no I/O of its own beyond synchronous calls to Storage: it has no
// clock, no goroutines and no sockets. A driver feeds it Tick() and Step(), then
// drains Ready() and performs the resulting sends. Because every source of
// nondeterminism is injected, a test can run the real protocol code under seeded
// chaos and replay any failure exactly (see sim_test.go).
package raft

import (
	"encoding/json"
	"errors"
	"sort"
)

// NodeID names a cluster member. It is also the CommonName of the node's TLS
// certificate, which lets the transport check that a peer is who it claims to be.
type NodeID string

// None is the zero NodeID: "no vote cast" or "no known leader".
const None NodeID = ""

// Role is one of the three states of the per-node automaton.
type Role uint8

const (
	Follower Role = iota
	Candidate
	Leader
)

func (r Role) String() string {
	switch r {
	case Follower:
		return "Follower"
	case Candidate:
		return "Candidate"
	case Leader:
		return "Leader"
	}
	return "Unknown"
}

// EntryKind distinguishes what a log entry means to the replicated state machine.
type EntryKind uint8

const (
	// EntryNormal carries an opaque command for the state machine.
	EntryNormal EntryKind = iota
	// EntryNoop is appended by every new leader. A leader may only count replicas
	// of entries from its own term (Raft §5.4.2), so this entry is what lets it
	// learn what is committed, and therefore serve reads, without client traffic.
	EntryNoop
	// EntryConfig carries a JSON-encoded Membership.
	EntryConfig
)

// Entry is one slot of the replicated log.
type Entry struct {
	Index uint64
	Term  uint64
	Kind  EntryKind
	Data  []byte
}

// Membership maps each voting member to its peer-network address.
type Membership map[NodeID]string

// IDs returns the members in sorted order so that iteration is deterministic.
func (m Membership) IDs() []NodeID {
	ids := make([]NodeID, 0, len(m))
	for id := range m {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

// Clone returns an independent copy.
func (m Membership) Clone() Membership {
	c := make(Membership, len(m))
	for k, v := range m {
		c[k] = v
	}
	return c
}

// EncodeMembership serializes a membership for an EntryConfig payload.
func EncodeMembership(m Membership) []byte {
	b, err := json.Marshal(m)
	if err != nil {
		panic(err) // a map[string]string cannot fail to marshal
	}
	return b
}

// DecodeMembership parses an EntryConfig payload.
func DecodeMembership(b []byte) (Membership, error) {
	var m Membership
	err := json.Unmarshal(b, &m)
	return m, err
}

// HardState is what must survive a crash before any message is sent. Forgetting
// Vote across a restart would let one node vote twice in a term, which is exactly
// the double-leader bug (the model checker's BugForgetVote shows the trace).
type HardState struct {
	Term uint64
	Vote NodeID
}

// Snapshot captures the state machine up to and including Index.
type Snapshot struct {
	Index   uint64
	Term    uint64
	Members Membership
	Data    []byte
}

// MsgType enumerates the protocol's messages.
type MsgType uint8

const (
	MsgVote MsgType = iota + 1
	MsgVoteResp
	MsgApp
	MsgAppResp
	MsgSnap
	MsgTimeoutNow
)

func (t MsgType) String() string {
	switch t {
	case MsgVote:
		return "RequestVote"
	case MsgVoteResp:
		return "VoteResponse"
	case MsgApp:
		return "AppendEntries"
	case MsgAppResp:
		return "AppendResponse"
	case MsgSnap:
		return "InstallSnapshot"
	case MsgTimeoutNow:
		return "TimeoutNow"
	}
	return "Unknown"
}

// Message is the single wire type for all protocol messages. Only the fields
// relevant to Type are populated.
type Message struct {
	Type MsgType
	From NodeID
	To   NodeID
	Term uint64

	// MsgVote: the candidate's last log position. Transfer marks an election
	// started by a leadership transfer, which voters must not ignore on the
	// grounds that they recently heard from a live leader.
	LastLogIndex uint64
	LastLogTerm  uint64
	Transfer     bool

	// MsgVoteResp.
	Granted bool

	// MsgApp.
	PrevLogIndex uint64
	PrevLogTerm  uint64
	Entries      []Entry
	Commit       uint64

	// MsgAppResp. On success Match is the highest index known to match the
	// leader. On failure Hint is where the leader should retry from.
	Success bool
	Match   uint64
	Hint    uint64

	// Seq is nonzero on heartbeats and echoed in the reply. It proves a quorum
	// still recognized this leader after a given moment (ReadIndex).
	Seq uint64

	// SentAt is stamped by the driver on outgoing heartbeats and echoed verbatim.
	// The core never interprets it; only the sender's clock is ever consulted, so
	// round-trip telemetry needs no clock synchronization.
	SentAt int64

	// MsgSnap.
	Snapshot *Snapshot
}

// Storage is the durable half of the protocol. Each method must be durable on
// return: the core assumes it may send messages promising what was just saved.
// A failed write panics, because a node that cannot persist must not keep voting.
type Storage interface {
	SaveHardState(HardState) error
	// Append durably adds entries whose indices follow the existing log.
	Append([]Entry) error
	// TruncateFrom durably removes all entries with Index >= index.
	TruncateFrom(index uint64) error
	// SaveSnapshot durably stores the snapshot and discards log entries it covers.
	// keep lists the log suffix that remains afterwards.
	SaveSnapshot(s Snapshot, keep []Entry) error
}

// InitialState is what a driver recovered from Storage at boot.
type InitialState struct {
	Hard     HardState
	Snapshot Snapshot
	Entries  []Entry
}

// Config configures a Node.
type Config struct {
	ID NodeID
	// Members is the boot configuration, used only when the recovered state has
	// no snapshot or config entry. Leave it empty for a node joining a live cluster.
	Members Membership
	// ElectionTicks is the base election timeout; each node draws its actual
	// timeout uniformly from [ElectionTicks, 2*ElectionTicks).
	ElectionTicks int
	// HeartbeatTicks is the leader's heartbeat interval.
	HeartbeatTicks int
	// Rand supplies election-timeout jitter in [0, n). Injected for determinism.
	Rand func(n int) int
	// MaxBatchEntries caps the entries in a single AppendEntries (default 256).
	MaxBatchEntries int
	Storage         Storage
	Initial         InitialState
}

// ReadState reports that the read identified by Ctx is safe to serve once the
// state machine has applied through Index.
type ReadState struct {
	Ctx   uint64
	Index uint64
}

// Ready is everything the driver must act on after Tick, Step or a proposal.
type Ready struct {
	Messages []Message
	// Snapshot, if set, must replace the state machine before Committed is applied.
	Snapshot *Snapshot
	// Committed lists newly committed entries, in order.
	Committed []Entry
	// Reads lists confirmed ReadIndex requests.
	Reads []ReadState
	// FailedReads lists ReadIndex requests abandoned because leadership was lost.
	FailedReads []uint64
}

// Status is a read-only summary of a node.
type Status struct {
	ID         NodeID
	Role       Role
	Term       uint64
	Leader     NodeID
	Commit     uint64
	Applied    uint64
	LastIndex  uint64
	FirstIndex uint64
	Transferee NodeID
	Members    Membership
	Match      map[NodeID]uint64
	// LeaderReady is true once this leader has committed an entry from its own
	// term, i.e. it knows the full committed prefix.
	LeaderReady bool
	// LeaderStartIndex is the index of this leader's no-op entry.
	LeaderStartIndex uint64
}

// Errors returned by proposal-style calls.
var (
	ErrNotLeader     = errors.New("raft: not the leader")
	ErrTransferring  = errors.New("raft: leadership transfer in progress")
	ErrLeaderNotInit = errors.New("raft: leader has not yet committed an entry in its term")
	ErrConfigPending = errors.New("raft: a membership change is still in progress")
	ErrBadConfig     = errors.New("raft: membership change must add or remove exactly one node")
	ErrBadTransfer   = errors.New("raft: invalid leadership transfer target")
	ErrCompacted     = errors.New("raft: index is already compacted or not yet applied")
)
