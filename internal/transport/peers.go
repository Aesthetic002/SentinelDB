package transport

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/gob"
	"fmt"
	"math/rand/v2"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"sentineldb/internal/raft"
)

// Stream types: the first byte after the TLS handshake on the peer port.
const (
	StreamRaft    byte = 'R'
	StreamForward byte = 'F'
)

// Chaos is a fault injected into outgoing traffic, used to reproduce a node degrading
// before it fails. It exists only for experiments, and is off unless the node was
// started with --enable-chaos.
type Chaos struct {
	Delay    time.Duration
	Jitter   time.Duration
	DropRate float64
	// OnlyTo limits the fault to one peer, to model a bad link instead of a sick node.
	OnlyTo raft.NodeID
}

// Peers keeps one outbound connection to each other cluster member.
type Peers struct {
	self     raft.NodeID
	material *Material

	mu      sync.Mutex
	senders map[raft.NodeID]*sender

	chaos atomic.Pointer[Chaos]
	ctx   context.Context
	stop  context.CancelFunc
	wg    sync.WaitGroup
}

// NewPeers creates a transport with no peers yet.
func NewPeers(self raft.NodeID, m *Material) *Peers {
	ctx, stop := context.WithCancel(context.Background())
	return &Peers{self: self, material: m, senders: map[raft.NodeID]*sender{}, ctx: ctx, stop: stop}
}

// SetChaos replaces the injected fault. Nil removes it.
func (p *Peers) SetChaos(c *Chaos) { p.chaos.Store(c) }

// SetMembers reconciles senders with a membership.
func (p *Peers) SetMembers(m raft.Membership) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for id, addr := range m {
		if id == p.self {
			continue
		}
		if s, ok := p.senders[id]; ok && s.addr == addr {
			continue
		} else if ok {
			s.cancel()
		}
		p.senders[id] = p.startSender(id, addr)
	}
	for id, s := range p.senders {
		if _, ok := m[id]; !ok {
			s.cancel()
			delete(p.senders, id)
		}
	}
}

// Send queues a message. Raft tolerates loss, so if a peer's queue is full the message
// is dropped rather than letting one slow peer stall the consensus loop.
func (p *Peers) Send(m raft.Message) {
	p.mu.Lock()
	s := p.senders[m.To]
	p.mu.Unlock()
	if s == nil {
		return
	}
	select {
	case s.queue <- queued{m: m, at: time.Now()}:
	default:
	}
}

// Close stops all senders.
func (p *Peers) Close() {
	p.stop()
	p.wg.Wait()
}

type queued struct {
	m  raft.Message
	at time.Time
}

type sender struct {
	id     raft.NodeID
	addr   string
	queue  chan queued
	cancel context.CancelFunc
}

func (p *Peers) startSender(id raft.NodeID, addr string) *sender {
	ctx, cancel := context.WithCancel(p.ctx)
	s := &sender{id: id, addr: addr, queue: make(chan queued, 4096), cancel: cancel}
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		p.senderLoop(ctx, s)
	}()
	return s
}

// DialPeer opens an authenticated stream of the given type to a peer.
func DialPeer(ctx context.Context, m *Material, id, addr string, stream byte) (*tls.Conn, error) {
	d := tls.Dialer{Config: m.PeerClientConfig(id), NetDialer: &net.Dialer{Timeout: time.Second}}
	dctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	c, err := d.DialContext(dctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	conn := c.(*tls.Conn)
	if _, err := conn.Write([]byte{stream}); err != nil {
		conn.Close()
		return nil, err
	}
	return conn, nil
}

func (p *Peers) senderLoop(ctx context.Context, s *sender) {
	backoff := 20 * time.Millisecond
	for ctx.Err() == nil {
		conn, err := DialPeer(ctx, p.material, string(s.id), s.addr, StreamRaft)
		if err != nil {
			p.drain(s)
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
				return
			}
			backoff = min(backoff*2, 500*time.Millisecond)
			continue
		}
		backoff = 20 * time.Millisecond
		// A half-open connection must not hold up Raft, so writes get a deadline.
		w := bufio.NewWriterSize(conn, 64*1024)
		enc := gob.NewEncoder(w)
		p.pump(ctx, s, conn, w, enc)
		conn.Close()
	}
}

// drain discards queued messages while a peer is unreachable. They would be stale by
// the time it came back, and Raft resends anything still relevant.
func (p *Peers) drain(s *sender) {
	for {
		select {
		case <-s.queue:
		default:
			return
		}
	}
}

func (p *Peers) pump(ctx context.Context, s *sender, conn net.Conn, w *bufio.Writer, enc *gob.Encoder) {
	for {
		var q queued
		select {
		case q = <-s.queue:
		case <-ctx.Done():
			return
		}
		if c := p.chaos.Load(); c != nil && (c.OnlyTo == "" || c.OnlyTo == s.id) {
			if c.DropRate > 0 && rand.Float64() < c.DropRate {
				continue
			}
			d := c.Delay
			if c.Jitter > 0 {
				d += time.Duration(rand.Int64N(int64(c.Jitter)))
			}
			// Delay relative to when the message was sent, preserving order. This models
			// a slow network rather than a queue that grows without limit.
			if wait := time.Until(q.at.Add(d)); wait > 0 {
				select {
				case <-time.After(wait):
				case <-ctx.Done():
					return
				}
			}
		}
		conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
		if err := enc.Encode(&q.m); err != nil {
			return
		}
		// Coalesce only what is already queued, then flush so latency stays low. With a
		// fault active, the next message may sit in its delay, so flush this one first.
		if len(s.queue) == 0 || p.chaos.Load() != nil {
			if err := w.Flush(); err != nil {
				return
			}
		}
	}
}

// ServeRaftStream reads messages from an authenticated peer until the connection ends.
// The sender's identity comes from its certificate, so a node cannot speak for another.
func ServeRaftStream(conn *tls.Conn, r *bufio.Reader, deliver func(raft.Message)) error {
	id, err := PeerIdentity(conn)
	if err != nil {
		return err
	}
	dec := gob.NewDecoder(r)
	for {
		var m raft.Message
		if err := dec.Decode(&m); err != nil {
			return err
		}
		if string(m.From) != id {
			return fmt.Errorf("peer %s sent a message claiming to be from %s", id, m.From)
		}
		deliver(m)
	}
}
