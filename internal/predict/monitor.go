package predict

import (
	"bufio"
	"fmt"
	"os"
	"sync"
	"time"

	"sentineldb/internal/protocol"
	"sentineldb/internal/raft"
)

// warmup is how long a new leader collects data before any prediction counts.
const warmup = Window + 500*time.Millisecond

// cooldown is the minimum time between handover requests, so that a wrong call cannot
// turn into leadership bouncing around the cluster.
const cooldown = 5 * time.Second

// Monitor watches this node's heartbeats while it is leader. It may run with no model, in
// which case it only records telemetry for building the training set.
type Monitor struct {
	id    raft.NodeID
	model *Model
	log   *bufio.Writer
	file  *os.File

	mu          sync.Mutex
	peers       map[raft.NodeID]*peerWindow
	leaderSince time.Time
	isLeader    bool
	over        int
	lastAct     time.Time
	handovers   int
	lastProb    float64
	lastFeat    []float64
}

// MonitorConfig configures a Monitor.
type MonitorConfig struct {
	ID    raft.NodeID
	Model *Model
	// TelemetryPath, if set, appends one row per reading while leader.
	TelemetryPath string
}

// NewMonitor creates a monitor.
func NewMonitor(cfg MonitorConfig) (*Monitor, error) {
	m := &Monitor{id: cfg.ID, model: cfg.Model, peers: map[raft.NodeID]*peerWindow{}}
	if cfg.TelemetryPath != "" {
		f, err := os.OpenFile(cfg.TelemetryPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			return nil, err
		}
		m.file = f
		m.log = bufio.NewWriter(f)
	}
	return m, nil
}

// Close flushes telemetry.
func (m *Monitor) Close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.log != nil {
		m.log.Flush()
		m.file.Close()
		m.log = nil
	}
}

// HeartbeatSent implements server.Monitor.
func (m *Monitor) HeartbeatSent(peer raft.NodeID, now time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	w := m.peers[peer]
	if w == nil {
		w = &peerWindow{}
		m.peers[peer] = w
	}
	w.sent(now)
}

// HeartbeatAcked implements server.Monitor.
func (m *Monitor) HeartbeatAcked(peer raft.NodeID, sent, now time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if w := m.peers[peer]; w != nil {
		w.acked(sent, now)
	}
}

// LeadershipChanged implements server.Monitor. Heartbeat history is dropped both ways: it
// describes a period of leadership that has ended, and must not leak into the next.
func (m *Monitor) LeadershipChanged(isLeader bool, now time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.isLeader = isLeader
	m.leaderSince = now
	m.peers = map[raft.NodeID]*peerWindow{}
	m.over = 0
	m.lastProb = 0
	m.lastFeat = nil
}

// Evaluate implements server.Monitor.
func (m *Monitor) Evaluate(st raft.Status, now time.Time) raft.NodeID {
	m.mu.Lock()
	defer m.mu.Unlock()
	if now.Sub(m.leaderSince) < warmup {
		return raft.None
	}
	x, ok := quorumFeatures(m.peers, st.Members, m.id, now)
	if !ok {
		return raft.None
	}
	m.lastFeat = x
	m.writeTelemetry(st, x, now)
	if m.model == nil {
		return raft.None
	}
	m.lastProb = m.model.Probability(x)
	if m.lastProb < m.model.Threshold {
		m.over = 0
		return raft.None
	}
	m.over++
	if m.over < m.model.Consecutive || now.Sub(m.lastAct) < cooldown {
		return raft.None
	}
	target := m.chooseTarget(st, now)
	if target != raft.None {
		m.lastAct = now
		m.over = 0
		m.handovers++
	}
	return target
}

// chooseTarget picks the healthiest follower that is keeping up. A follower that is
// behind can still be chosen, but only as a last resort: the transfer would then have to
// wait for it to catch up, through what is by now a struggling leader.
func (m *Monitor) chooseTarget(st raft.Status, now time.Time) raft.NodeID {
	var best raft.NodeID
	var bestRTT float64
	var bestMatch uint64
	for _, id := range st.Members.IDs() {
		if id == m.id {
			continue
		}
		w := m.peers[id]
		if w == nil {
			continue
		}
		f, ok := w.peerFeatures(now)
		if !ok {
			continue
		}
		match := st.Match[id]
		// Treat a follower within a small margin of the leader as up to date, since a live
		// follower is always slightly behind while writes are flowing.
		if st.LastIndex > match && st.LastIndex-match > 16 {
			match = match / 2
		} else {
			match = st.LastIndex
		}
		if best == raft.None || match > bestMatch || (match == bestMatch && f[0] < bestRTT) {
			best, bestRTT, bestMatch = id, f[0], match
		}
	}
	return best
}

func (m *Monitor) writeTelemetry(st raft.Status, x []float64, now time.Time) {
	if m.log == nil {
		return
	}
	fmt.Fprintf(m.log, "%d,%s,%d", now.UnixMilli(), m.id, st.Term)
	for _, v := range x {
		fmt.Fprintf(m.log, ",%.3f", v)
	}
	m.log.WriteByte('\n')
	m.log.Flush() // a killed process must not lose what led up to its failure
}

// Status implements server.Monitor.
func (m *Monitor) Status() *protocol.MonitorStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := &protocol.MonitorStatus{Enabled: m.model != nil, Probability: m.lastProb, Handovers: m.handovers}
	if m.model != nil {
		s.Model = m.model.Kind
	}
	if m.lastFeat != nil {
		s.Features = map[string]float64{}
		for i, n := range FeatureNames {
			s.Features[n] = m.lastFeat[i]
		}
	}
	return s
}
