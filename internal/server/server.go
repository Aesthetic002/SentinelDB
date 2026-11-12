// Package server assembles a SentinelDB node: it drives the Raft state machine, applies
// committed entries to the database, and serves clients and peers.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math/rand"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"sentineldb/internal/engine"
	"sentineldb/internal/protocol"
	"sentineldb/internal/raft"
	"sentineldb/internal/transport"
	"sentineldb/internal/txn"
	"sentineldb/internal/wal"
)

// Config configures a node.
type Config struct {
	ID         raft.NodeID
	PeerAddr   string // listen address for other nodes
	ClientAddr string // listen address for clients
	// Members is the boot configuration. Leave empty when joining a running cluster.
	Members raft.Membership
	// Contacts lists existing nodes a joining node can talk to before it has learned
	// the membership. It only gives replies somewhere to go: it grants no votes and
	// counts toward no quorum, because that is decided purely by the replicated config.
	Contacts raft.Membership
	DataDir  string
	// TickInterval, ElectionTicks and HeartbeatTicks set the protocol's timing.
	TickInterval   time.Duration
	ElectionTicks  int
	HeartbeatTicks int
	// SnapshotEvery compacts the log after this many applied entries.
	SnapshotEvery uint64

	Material          *transport.Material
	Tokens            transport.Tokens
	RequireClientCert bool
	EnableChaos       bool
	// NoSync disables fsync. Only for tests; it voids the durability guarantee.
	NoSync bool
	// Monitor, when set, watches heartbeats and may trigger leadership transfers.
	Monitor Monitor
	Logger  *log.Logger
}

// Monitor is the prediction layer's view of the consensus layer. It runs on the raft
// loop goroutine, so implementations must return quickly.
type Monitor interface {
	// HeartbeatSent notes a heartbeat to a peer.
	HeartbeatSent(peer raft.NodeID, now time.Time)
	// HeartbeatAcked notes a round trip completed.
	HeartbeatAcked(peer raft.NodeID, sent, now time.Time)
	// Evaluate is called periodically while this node is leader. It returns a follower
	// to hand leadership to, or None to carry on as leader.
	Evaluate(st raft.Status, now time.Time) raft.NodeID
	// LeadershipChanged tells the monitor this node gained or lost leadership.
	LeadershipChanged(isLeader bool, now time.Time)
	// Status summarizes the monitor for operators.
	Status() *protocol.MonitorStatus
}

func (c *Config) defaults() {
	if c.TickInterval == 0 {
		c.TickInterval = 10 * time.Millisecond
	}
	if c.ElectionTicks == 0 {
		c.ElectionTicks = 50
	}
	if c.HeartbeatTicks == 0 {
		c.HeartbeatTicks = 5
	}
	if c.SnapshotEvery == 0 {
		c.SnapshotEvery = 10000
	}
	if c.Logger == nil {
		c.Logger = log.Default()
	}
}

// Server is a running node.
type Server struct {
	cfg  Config
	node *raft.Node
	wal  *wal.WAL
	db   *engine.DB
	exec *engine.Executor

	locks *txn.Manager
	peers *transport.Peers

	view atomic.Pointer[view]

	recvc  chan raft.Message
	propc  chan *proposal
	readc  chan *readReq
	callc  chan func()
	stopc  chan struct{}
	loopWG sync.WaitGroup
	connWG sync.WaitGroup

	// Loop-owned state.
	applied     uint64
	snapIndex   uint64
	waiters     map[uint64]*proposal
	reads       map[uint64]*readReq
	readySoon   []*readReq
	nextRead    uint64
	lastMembers raft.Membership
	wasLeader   bool
	lastEval    time.Time

	listeners []net.Listener
	connMu    sync.Mutex
	conns     map[net.Conn]struct{}
	closed    atomic.Bool
	// acceptErrors counts failed accepts, which the loop survives.
	acceptErrors atomic.Int64
}

// view is an immutable summary that other goroutines read without touching the loop.
type view struct {
	st      raft.Status
	applied uint64
}

type proposal struct {
	epoch uint64
	data  []byte
	term  uint64
	done  chan proposalResult
}

type proposalResult struct {
	res engine.ApplyResult
	err error
}

type readReq struct {
	epoch uint64
	index uint64
	done  chan error
}

// NotLeaderError reports that a request needs the leader, and who that is if known.
type NotLeaderError struct{ Leader raft.NodeID }

func (e *NotLeaderError) Error() string {
	if e.Leader == raft.None {
		return "this node is not the leader and no leader is currently known"
	}
	return fmt.Sprintf("this node is not the leader; the leader is %s", e.Leader)
}

var (
	// ErrUnknownOutcome means a write was proposed but leadership was lost before its
	// fate was known. It may yet commit. Only a retry with the same request ID is safe.
	ErrUnknownOutcome = errors.New("leadership changed while the write was in flight; its outcome is unknown")
	errStopped        = errors.New("server is shutting down")
)

// Start recovers state from disk and begins serving.
func Start(cfg Config) (*Server, error) {
	cfg.defaults()
	w, init, err := wal.Open(cfg.DataDir, wal.Options{NoSync: cfg.NoSync})
	if err != nil {
		return nil, err
	}
	s := &Server{
		cfg:     cfg,
		wal:     w,
		db:      engine.NewDB(),
		locks:   txn.NewManager(),
		recvc:   make(chan raft.Message, 8192),
		propc:   make(chan *proposal, 4096),
		readc:   make(chan *readReq, 4096),
		callc:   make(chan func(), 64),
		stopc:   make(chan struct{}),
		waiters: map[uint64]*proposal{},
		reads:   map[uint64]*readReq{},
		conns:   map[net.Conn]struct{}{},
	}
	if init.Snapshot.Index > 0 {
		if err := s.db.Restore(init.Snapshot.Data); err != nil {
			w.Close()
			return nil, err
		}
	}
	s.applied = init.Snapshot.Index
	s.snapIndex = init.Snapshot.Index
	seed := time.Now().UnixNano() ^ int64(fnv(string(cfg.ID)))
	rng := rand.New(rand.NewSource(seed))
	s.node = raft.NewNode(raft.Config{
		ID: cfg.ID, Members: cfg.Members,
		ElectionTicks: cfg.ElectionTicks, HeartbeatTicks: cfg.HeartbeatTicks,
		Rand: rng.Intn, Storage: w, Initial: init,
	})
	s.exec = &engine.Executor{DB: s.db, Locks: s.locks, Backend: s}
	s.peers = transport.NewPeers(cfg.ID, cfg.Material)
	s.lastMembers = s.node.Status().Members
	s.peers.SetMembers(s.reachable(s.lastMembers))
	s.publish()

	peerL, err := tlsListen(cfg.PeerAddr, cfg.Material.PeerServerConfig())
	if err != nil {
		s.shutdownStorage()
		return nil, fmt.Errorf("peer listener: %w", err)
	}
	clientL, err := tlsListen(cfg.ClientAddr, cfg.Material.ClientServerConfig(cfg.RequireClientCert))
	if err != nil {
		peerL.Close()
		s.shutdownStorage()
		return nil, fmt.Errorf("client listener: %w", err)
	}
	s.listeners = []net.Listener{peerL, clientL}

	s.loopWG.Add(1)
	go s.loop()
	s.connWG.Add(2)
	go s.acceptLoop(peerL, s.handlePeerConn)
	go s.acceptLoop(clientL, s.handleClientConn)
	return s, nil
}

func fnv(s string) uint64 {
	h := uint64(14695981039346656037)
	for i := 0; i < len(s); i++ {
		h ^= uint64(s[i])
		h *= 1099511628211
	}
	return h
}

// PeerAddr returns the actual peer listener address.
func (s *Server) PeerAddr() string { return s.listeners[0].Addr().String() }

// ClientAddr returns the actual client listener address.
func (s *Server) ClientAddr() string { return s.listeners[1].Addr().String() }

// DB exposes the state machine, for tests that compare replicas.
func (s *Server) DB() *engine.DB { return s.db }

// Executor exposes the SQL executor.
func (s *Server) Executor() *engine.Executor { return s.exec }

func (s *Server) shutdownStorage() {
	s.peers.Close()
	s.wal.Close()
}

// Close stops the node, simulating a clean process exit. In-flight requests fail, and
// nothing that was acknowledged is lost, because it is already on disk.
func (s *Server) Close() {
	if !s.closed.CompareAndSwap(false, true) {
		return
	}
	close(s.stopc)
	for _, l := range s.listeners {
		l.Close()
	}
	s.connMu.Lock()
	for c := range s.conns {
		c.Close()
	}
	s.connMu.Unlock()
	s.loopWG.Wait()
	// Wake anything still waiting for a lock: nothing will release it now.
	s.locks.SetEpoch(^uint64(0))
	s.connWG.Wait()
	s.peers.Close()
	s.wal.Close()
}

// ---------------------------------------------------------------------------
// The consensus loop: the only goroutine that touches the Raft node
// ---------------------------------------------------------------------------

func (s *Server) loop() {
	defer s.loopWG.Done()
	ticker := time.NewTicker(s.cfg.TickInterval)
	defer ticker.Stop()
	for {
		select {
		case <-s.stopc:
			s.failAll(errStopped)
			return
		case <-ticker.C:
			s.node.Tick()
			s.evaluateMonitor()
		case m := <-s.recvc:
			s.step(m)
			// Handle everything that has arrived before doing I/O for any of it.
			for drained := false; !drained; {
				select {
				case m = <-s.recvc:
					s.step(m)
				default:
					drained = true
				}
			}
		case p := <-s.propc:
			s.handleProposals(p)
		case r := <-s.readc:
			s.handleReads(r)
		case f := <-s.callc:
			f()
		}
		s.process()
	}
}

func (s *Server) step(m raft.Message) {
	if s.cfg.Monitor != nil && m.Type == raft.MsgAppResp && m.SentAt != 0 {
		s.cfg.Monitor.HeartbeatAcked(m.From, time.Unix(0, m.SentAt), time.Now())
	}
	s.node.Step(m)
}

// handleProposals appends all waiting proposals with one durable write. This group
// commit is what keeps throughput reasonable when each fsync is expensive.
func (s *Server) handleProposals(first *proposal) {
	batch := []*proposal{first}
	for len(batch) < 256 {
		select {
		case p := <-s.propc:
			batch = append(batch, p)
			continue
		default:
		}
		break
	}
	st := s.node.Status()
	var accepted []*proposal
	var data [][]byte
	for _, p := range batch {
		switch {
		case st.Role != raft.Leader:
			p.done <- proposalResult{err: &NotLeaderError{Leader: st.Leader}}
		case p.epoch != st.Term:
			// This transaction's locks belonged to an earlier term, so committing its
			// writes now could violate two-phase locking.
			p.done <- proposalResult{err: txn.ErrStale}
		default:
			accepted = append(accepted, p)
			data = append(data, p.data)
		}
	}
	if len(accepted) == 0 {
		return
	}
	idx, term, err := s.node.ProposeBatch(data)
	if err != nil {
		for _, p := range accepted {
			p.done <- proposalResult{err: err}
		}
		return
	}
	for i, p := range accepted {
		p.term = term
		s.waiters[idx+uint64(i)] = p
	}
}

func (s *Server) handleReads(first *readReq) {
	batch := []*readReq{first}
	for len(batch) < 256 {
		select {
		case r := <-s.readc:
			batch = append(batch, r)
			continue
		default:
		}
		break
	}
	st := s.node.Status()
	ctxs := make([]uint64, 0, len(batch))
	for _, r := range batch {
		switch {
		case st.Role != raft.Leader:
			r.done <- &NotLeaderError{Leader: st.Leader}
		case r.epoch != st.Term:
			r.done <- txn.ErrStale
		default:
			s.nextRead++
			s.reads[s.nextRead] = r
			ctxs = append(ctxs, s.nextRead)
		}
	}
	if err := s.node.ReadIndex(ctxs...); err != nil {
		for _, c := range ctxs {
			s.reads[c].done <- err
			delete(s.reads, c)
		}
	}
}

// process acts on what the Raft node produced. Storage writes already happened
// synchronously inside the node, so everything here follows durable state.
func (s *Server) process() {
	rd := s.node.Ready()

	if rd.Snapshot != nil {
		if err := s.db.Restore(rd.Snapshot.Data); err != nil {
			panic(fmt.Sprintf("restoring a snapshot that passed its checksum failed: %v", err))
		}
		s.applied = rd.Snapshot.Index
		s.snapIndex = rd.Snapshot.Index
	}
	for i := range rd.Committed {
		s.applyEntry(&rd.Committed[i])
	}
	now := time.Now()
	for i := range rd.Messages {
		m := &rd.Messages[i]
		if m.Type == raft.MsgApp && m.Seq != 0 {
			m.SentAt = now.UnixNano()
			if s.cfg.Monitor != nil {
				s.cfg.Monitor.HeartbeatSent(m.To, now)
			}
		}
		s.peers.Send(*m)
	}
	for _, r := range rd.Reads {
		if req := s.reads[r.Ctx]; req != nil {
			delete(s.reads, r.Ctx)
			req.index = r.Index
			s.readySoon = append(s.readySoon, req)
		}
	}
	for _, c := range rd.FailedReads {
		if req := s.reads[c]; req != nil {
			delete(s.reads, c)
			req.done <- &NotLeaderError{Leader: s.node.Status().Leader}
		}
	}
	kept := s.readySoon[:0]
	for _, r := range s.readySoon {
		if r.index <= s.applied {
			r.done <- nil
		} else {
			kept = append(kept, r)
		}
	}
	s.readySoon = kept

	st := s.node.Status()
	isLeader := st.Role == raft.Leader
	if isLeader != s.wasLeader {
		if isLeader {
			s.locks.SetEpoch(st.Term)
		} else {
			s.locks.SetEpoch(0)
			s.failAll(ErrUnknownOutcome)
		}
		s.wasLeader = isLeader
		if s.cfg.Monitor != nil {
			s.cfg.Monitor.LeadershipChanged(isLeader, now)
		}
		s.cfg.Logger.Printf("%s: %s in term %d", s.cfg.ID, st.Role, st.Term)
	}
	if !sameMembers(st.Members, s.lastMembers) {
		s.peers.SetMembers(s.reachable(st.Members))
		s.lastMembers = st.Members
		s.cfg.Logger.Printf("%s: membership is now %v", s.cfg.ID, st.Members.IDs())
	}
	if s.applied >= s.snapIndex+s.cfg.SnapshotEvery {
		s.takeSnapshot()
	}
	s.publish()
}

// reachable is who this node needs connections to. A node not yet in the configuration
// must still be able to answer the leader that is bringing it up to date.
func (s *Server) reachable(members raft.Membership) raft.Membership {
	if _, in := members[s.cfg.ID]; in {
		return members
	}
	out := members.Clone()
	for id, addr := range s.cfg.Contacts {
		if _, known := out[id]; !known {
			out[id] = addr
		}
	}
	return out
}

func (s *Server) publish() {
	s.view.Store(&view{st: s.node.Status(), applied: s.applied})
}

func sameMembers(a, b raft.Membership) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if w, ok := b[k]; !ok || w != v {
			return false
		}
	}
	return true
}

func (s *Server) applyEntry(e *raft.Entry) {
	var res engine.ApplyResult
	if e.Kind == raft.EntryNormal {
		var c engine.Command
		if err := json.Unmarshal(e.Data, &c); err != nil {
			panic(fmt.Sprintf("corrupt command at index %d: %v", e.Index, err))
		}
		res = s.db.Apply(&c)
	}
	s.applied = e.Index
	if p, ok := s.waiters[e.Index]; ok {
		delete(s.waiters, e.Index)
		if p.term == e.Term {
			p.done <- proposalResult{res: res}
		} else {
			// A different leader's entry landed in this slot, so ours was overwritten
			// and definitely did not commit.
			p.done <- proposalResult{err: txn.ErrStale}
		}
	}
}

func (s *Server) takeSnapshot() {
	data, err := s.db.Snapshot()
	if err != nil {
		s.cfg.Logger.Printf("%s: snapshot failed: %v", s.cfg.ID, err)
		return
	}
	if err := s.node.Compact(s.applied, data); err != nil {
		s.cfg.Logger.Printf("%s: compaction failed: %v", s.cfg.ID, err)
		return
	}
	s.snapIndex = s.applied
}

func (s *Server) failAll(err error) {
	for idx, p := range s.waiters {
		delete(s.waiters, idx)
		p.done <- proposalResult{err: err}
	}
	for c, r := range s.reads {
		delete(s.reads, c)
		r.done <- err
	}
	for _, r := range s.readySoon {
		r.done <- err
	}
	s.readySoon = nil
}

// evaluateMonitor gives the prediction layer a chance to act, about every 100ms. A
// transfer it asks for goes through the same protocol path as any other: it cannot
// bypass voting, so a wrong prediction costs a handover but can never affect safety.
func (s *Server) evaluateMonitor() {
	if s.cfg.Monitor == nil {
		return
	}
	st := s.node.Status()
	if st.Role != raft.Leader {
		return
	}
	now := time.Now()
	if now.Sub(s.lastEval) < 100*time.Millisecond {
		return
	}
	s.lastEval = now
	if to := s.cfg.Monitor.Evaluate(st, now); to != raft.None {
		if err := s.node.TransferLeadership(to); err != nil {
			s.cfg.Logger.Printf("%s: proactive transfer to %s failed: %v", s.cfg.ID, to, err)
		} else {
			s.cfg.Logger.Printf("%s: proactive handover to %s", s.cfg.ID, to)
		}
	}
}

// call runs f on the loop goroutine and waits for it.
func (s *Server) call(ctx context.Context, f func()) error {
	done := make(chan struct{})
	wrapped := func() { f(); close(done) }
	select {
	case s.callc <- wrapped:
	case <-ctx.Done():
		return ctx.Err()
	case <-s.stopc:
		return errStopped
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-s.stopc:
		return errStopped
	}
}

// ---------------------------------------------------------------------------
// engine.Backend: the contract between the database and consensus layers
// ---------------------------------------------------------------------------

// LeaderEpoch implements engine.Backend. A new leader may still be applying entries from
// earlier terms. Serving before they are applied could read old data or lose an update,
// so this waits for the leader's no-op entry to be applied.
func (s *Server) LeaderEpoch(ctx context.Context) (uint64, error) {
	for {
		v := s.view.Load()
		if v.st.Role != raft.Leader {
			return 0, &NotLeaderError{Leader: v.st.Leader}
		}
		if v.st.LeaderReady && v.applied >= v.st.LeaderStartIndex {
			return v.st.Term, nil
		}
		select {
		case <-time.After(time.Millisecond):
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-s.stopc:
			return 0, errStopped
		}
	}
}

// Commit implements engine.Backend.
func (s *Server) Commit(ctx context.Context, epoch uint64, c *engine.Command) (engine.ApplyResult, error) {
	data, err := json.Marshal(c)
	if err != nil {
		return engine.ApplyResult{}, err
	}
	p := &proposal{epoch: epoch, data: data, done: make(chan proposalResult, 1)}
	select {
	case s.propc <- p:
	case <-ctx.Done():
		return engine.ApplyResult{}, ctx.Err()
	case <-s.stopc:
		return engine.ApplyResult{}, errStopped
	}
	select {
	case r := <-p.done:
		return r.res, r.err
	case <-ctx.Done():
		return engine.ApplyResult{}, ErrUnknownOutcome
	case <-s.stopc:
		return engine.ApplyResult{}, ErrUnknownOutcome
	}
}

// ReadBarrier implements engine.Backend using Raft's ReadIndex.
func (s *Server) ReadBarrier(ctx context.Context, epoch uint64) error {
	r := &readReq{epoch: epoch, done: make(chan error, 1)}
	select {
	case s.readc <- r:
	case <-ctx.Done():
		return ctx.Err()
	case <-s.stopc:
		return errStopped
	}
	select {
	case err := <-r.done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	case <-s.stopc:
		return errStopped
	}
}

// Status returns a summary of the node.
func (s *Server) Status() raft.Status { return s.view.Load().st }

// Applied returns the last applied log index.
func (s *Server) Applied() uint64 { return s.view.Load().applied }
