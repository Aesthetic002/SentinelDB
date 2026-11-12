package server

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"time"

	"sentineldb/internal/engine"
	"sentineldb/internal/protocol"
	"sentineldb/internal/raft"
	"sentineldb/internal/sql"
	"sentineldb/internal/transport"
	"sentineldb/internal/txn"
)

const (
	maxRequestBytes = 4 << 20
	authTimeout     = 5 * time.Second
	// requestTimeout bounds a statement, including time waiting for locks.
	requestTimeout = 15 * time.Second
)

func tlsListen(addr string, cfg *tls.Config) (net.Listener, error) {
	return tls.Listen("tcp", addr, cfg)
}

func (s *Server) acceptLoop(l net.Listener, handle func(net.Conn)) {
	defer s.connWG.Done()
	for {
		c, err := l.Accept()
		if err != nil {
			if s.closed.Load() || errors.Is(err, net.ErrClosed) {
				return
			}
			// Ending the loop here would leave a node that still votes and looks healthy,
			// yet refuses every new connection until it is restarted. An accept error can
			// be temporary, such as running out of file handles under load, so log it and
			// carry on, pausing so that a lasting error cannot spin.
			s.cfg.Logger.Printf("%s: accept failed on %s, will retry: %v", s.cfg.ID, l.Addr(), err)
			s.acceptErrors.Add(1)
			select {
			case <-time.After(50 * time.Millisecond):
			case <-s.stopc:
				return
			}
			continue
		}
		s.connMu.Lock()
		if s.closed.Load() {
			s.connMu.Unlock()
			c.Close()
			return
		}
		s.conns[c] = struct{}{}
		s.connMu.Unlock()
		s.connWG.Add(1)
		go func() {
			defer s.connWG.Done()
			defer func() {
				s.connMu.Lock()
				delete(s.conns, c)
				s.connMu.Unlock()
				c.Close()
			}()
			handle(c)
		}()
	}
}

// handlePeerConn serves the mutually authenticated peer port.
func (s *Server) handlePeerConn(c net.Conn) {
	conn := c.(*tls.Conn)
	conn.SetDeadline(time.Now().Add(authTimeout))
	if err := conn.Handshake(); err != nil {
		return
	}
	r := bufio.NewReaderSize(conn, 64*1024)
	kind, err := r.ReadByte()
	if err != nil {
		return
	}
	conn.SetDeadline(time.Time{})
	switch kind {
	case transport.StreamRaft:
		err := transport.ServeRaftStream(conn, r, func(m raft.Message) {
			if m.To != s.cfg.ID {
				return
			}
			select {
			case s.recvc <- m:
			case <-s.stopc:
			}
		})
		if err != nil && !errors.Is(err, io.EOF) && !s.closed.Load() {
			s.cfg.Logger.Printf("%s: peer stream ended: %v", s.cfg.ID, err)
		}
	case transport.StreamForward:
		s.serveSession(conn, r, protocol.RoleNone, true)
	}
}

// handleClientConn serves the client port. The first line must carry a valid token.
func (s *Server) handleClientConn(c net.Conn) {
	conn := c.(*tls.Conn)
	conn.SetDeadline(time.Now().Add(authTimeout))
	if err := conn.Handshake(); err != nil {
		return
	}
	r := bufio.NewReaderSize(conn, 64*1024)
	line, err := readLine(r)
	if err != nil {
		return
	}
	var req protocol.Request
	role := protocol.RoleNone
	if json.Unmarshal(line, &req) == nil {
		role = s.cfg.Tokens.Check(req.Auth)
	}
	enc := json.NewEncoder(conn)
	if role == protocol.RoleNone {
		enc.Encode(protocol.Response{Code: protocol.CodeAuth, Error: "authentication failed", Node: string(s.cfg.ID)})
		return
	}
	if err := enc.Encode(protocol.Response{OK: true, Node: string(s.cfg.ID)}); err != nil {
		return
	}
	conn.SetDeadline(time.Time{})
	s.serveSession(conn, r, role, false)
}

func readLine(r *bufio.Reader) ([]byte, error) {
	var buf []byte
	for {
		chunk, err := r.ReadSlice('\n')
		buf = append(buf, chunk...)
		if len(buf) > maxRequestBytes {
			return nil, fmt.Errorf("request larger than %d bytes", maxRequestBytes)
		}
		if err == nil {
			return bytes.TrimSpace(buf), nil
		}
		if !errors.Is(err, bufio.ErrBufferFull) {
			return nil, err
		}
	}
}

// session is one client connection. It holds at most one transaction, either local
// (this node is the leader) or on a relay to the leader.
type session struct {
	s         *Server
	role      protocol.Role
	forwarded bool
	local     *engine.Session
	relay     *relayConn
}

func (s *Server) serveSession(conn net.Conn, r *bufio.Reader, role protocol.Role, forwarded bool) {
	sess := &session{s: s, role: role, forwarded: forwarded, local: s.exec.NewSession()}
	defer sess.close()
	enc := json.NewEncoder(conn)
	for {
		line, err := readLine(r)
		if err != nil {
			return
		}
		if len(line) == 0 {
			continue
		}
		var req protocol.Request
		dec := json.NewDecoder(bytes.NewReader(line))
		dec.UseNumber()
		var resp *protocol.Response
		if err := dec.Decode(&req); err != nil {
			resp = errResponse(protocol.CodeError, "malformed request: "+err.Error())
		} else {
			if forwarded {
				sess.role = req.ForwardRole
			}
			resp = sess.handle(&req)
		}
		resp.Node = string(s.cfg.ID)
		if resp.Leader == "" {
			resp.Leader = string(s.view.Load().st.Leader)
		}
		if err := enc.Encode(resp); err != nil {
			return
		}
	}
}

func (ss *session) close() {
	ss.local.Abort()
	if ss.relay != nil {
		ss.relay.close()
	}
}

func errResponse(code, msg string) *protocol.Response {
	return &protocol.Response{Code: code, Error: msg}
}

func (ss *session) handle(req *protocol.Request) *protocol.Response {
	if req.Admin != nil {
		return ss.handleAdmin(req.Admin)
	}
	if ss.relay != nil && ss.relay.inTxn {
		return ss.relayRequest(req)
	}
	if req.SQL == "" {
		return errResponse(protocol.CodeError, "empty request")
	}
	st, _, err := sql.ParseStatement(req.SQL)
	if err != nil {
		return errResponse(protocol.CodeSyntax, err.Error())
	}
	if need := requiredRole(st); ss.role < need {
		return errResponse(protocol.CodeAuth, "this token does not permit that statement")
	}
	params, err := protocol.DecodeParams(req.Params)
	if err != nil {
		return errResponse(protocol.CodeError, err.Error())
	}

	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()
	if req.Stale {
		res, err := ss.s.exec.ExecuteStale(ctx, req.SQL, params)
		if err != nil {
			return errResponse(protocol.CodeError, err.Error())
		}
		return toResponse(res, false)
	}
	wasInTxn := ss.local.InTransaction()
	res, err := ss.local.Execute(ctx, req.SQL, params, req.RequestID)
	if err == nil {
		return toResponse(res, ss.local.InTransaction())
	}
	var nl *NotLeaderError
	if errors.As(err, &nl) && !ss.forwarded && !wasInTxn {
		return ss.relayRequest(req)
	}
	return classify(err, wasInTxn, ss.local.InTransaction())
}

func requiredRole(st sql.Statement) protocol.Role {
	switch st.(type) {
	case sql.Select, sql.Explain, sql.Begin, sql.Commit, sql.Rollback:
		return protocol.RoleRead
	}
	return protocol.RoleWrite
}

func toResponse(r *engine.Result, inTxn bool) *protocol.Response {
	return &protocol.Response{
		OK: true, Columns: r.Columns, Rows: protocol.EncodeRows(r.Rows), Affected: r.Affected,
		Message: r.Message, Stale: r.Stale, Duplicate: r.Duplicate, InTxn: inTxn,
	}
}

// classify maps an error to a code saying whether a client can usefully retry. A
// failure inside an explicit transaction is never simply retryable: the transaction's
// earlier statements are gone too, so the client must rerun all of it. A lone statement
// that failed before being proposed has lost nothing, so it can just run again.
func classify(err error, wasInTxn, inTxn bool) *protocol.Response {
	var nl *NotLeaderError
	var se *sql.SyntaxError
	var code string
	resp := &protocol.Response{Error: err.Error(), InTxn: inTxn}
	switch {
	case errors.As(err, &nl):
		code = protocol.CodeNotLeader
		if nl.Leader == raft.None {
			code = protocol.CodeNoLeader
		}
		resp.Leader = string(nl.Leader)
	case errors.As(err, &se):
		code = protocol.CodeSyntax
	case errors.Is(err, raft.ErrTransferring), errors.Is(err, raft.ErrLeaderNotInit):
		code = protocol.CodeRetry
	case errors.Is(err, txn.ErrDie), errors.Is(err, txn.ErrStale):
		code = protocol.CodeRetry
	case errors.Is(err, ErrUnknownOutcome):
		code = protocol.CodeUnknownOutcome
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, errStopped):
		code = protocol.CodeRetry
	default:
		code = protocol.CodeError
	}
	if wasInTxn && code != protocol.CodeSyntax && code != protocol.CodeUnknownOutcome && code != protocol.CodeError {
		code = protocol.CodeTxnAborted
	}
	resp.Code = code
	return resp
}

// ---------------------------------------------------------------------------
// Forwarding to the leader
// ---------------------------------------------------------------------------

type relayConn struct {
	s      *Server
	leader raft.NodeID
	conn   *tls.Conn
	r      *bufio.Reader
	inTxn  bool
}

func (r *relayConn) close() {
	r.s.connMu.Lock()
	delete(r.s.conns, r.conn)
	r.s.connMu.Unlock()
	r.conn.Close()
}

func (ss *session) dialRelay(leader raft.NodeID) (*relayConn, error) {
	v := ss.s.view.Load()
	addr, ok := v.st.Members[leader]
	if !ok {
		return nil, fmt.Errorf("leader %s has no known address", leader)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	conn, err := transport.DialPeer(ctx, ss.s.cfg.Material, string(leader), addr, transport.StreamForward)
	if err != nil {
		return nil, err
	}
	// Track it, so that shutdown can interrupt a request blocked on the leader.
	ss.s.connMu.Lock()
	ss.s.conns[conn] = struct{}{}
	ss.s.connMu.Unlock()
	return &relayConn{s: ss.s, leader: leader, conn: conn, r: bufio.NewReaderSize(conn, 64*1024)}, nil
}

// relayRequest sends a statement to the leader, keeping one stream so that an open
// transaction stays on the leader's session. Forwarding only happens once: a relayed
// statement that finds the leader moved returns to the client, which cannot loop.
func (ss *session) relayRequest(req *protocol.Request) *protocol.Response {
	v := ss.s.view.Load()
	leader := v.st.Leader
	if ss.relay == nil || (!ss.relay.inTxn && ss.relay.leader != leader) {
		if ss.relay != nil {
			ss.relay.close()
			ss.relay = nil
		}
		if leader == raft.None || leader == ss.s.cfg.ID {
			return &protocol.Response{Code: protocol.CodeNoLeader, Error: "no leader is known yet"}
		}
		r, err := ss.dialRelay(leader)
		if err != nil {
			return &protocol.Response{Code: protocol.CodeRetry, Error: "cannot reach the leader: " + err.Error()}
		}
		ss.relay = r
	}

	fwd := *req
	fwd.Auth = ""
	fwd.ForwardRole = ss.role
	body, err := json.Marshal(&fwd)
	if err != nil {
		return errResponse(protocol.CodeError, err.Error())
	}
	relay := ss.relay
	relay.conn.SetDeadline(time.Now().Add(requestTimeout + 2*time.Second))
	defer relay.conn.SetDeadline(time.Time{})

	wasInTxn := relay.inTxn
	fail := func(err error) *protocol.Response {
		relay.close()
		ss.relay = nil
		if wasInTxn {
			return &protocol.Response{Code: protocol.CodeTxnAborted, Error: "lost contact with the leader: " + err.Error()}
		}
		// The request may have reached the leader before the connection failed.
		return &protocol.Response{Code: protocol.CodeUnknownOutcome, Error: "lost contact with the leader: " + err.Error()}
	}
	if _, err := relay.conn.Write(append(body, '\n')); err != nil {
		return fail(err)
	}
	line, err := relay.r.ReadBytes('\n')
	if err != nil {
		return fail(err)
	}
	var resp protocol.Response
	dec := json.NewDecoder(bytes.NewReader(line))
	dec.UseNumber()
	if err := dec.Decode(&resp); err != nil {
		return fail(err)
	}
	relay.inTxn = resp.InTxn
	return &resp
}

// ---------------------------------------------------------------------------
// Administration
// ---------------------------------------------------------------------------

func (ss *session) handleAdmin(a *protocol.AdminRequest) *protocol.Response {
	// Status is read-only. Everything else requires an admin token: membership changes
	// are never reachable from an ordinary client, which is what stops someone sneaking
	// in a rogue node or removing enough honest ones to break the majority.
	if a.Op == "status" {
		if ss.role < protocol.RoleRead {
			return errResponse(protocol.CodeAuth, "this token does not permit that action")
		}
		return ss.s.statusResponse()
	}
	if ss.role < protocol.RoleAdmin {
		return errResponse(protocol.CodeAuth, "this action requires an admin token")
	}
	s := ss.s
	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()

	switch a.Op {
	case "add_node", "remove_node":
		next := s.view.Load().st.Members.Clone()
		id := raft.NodeID(a.Node)
		if a.Op == "add_node" {
			if a.Node == "" || a.Addr == "" {
				return errResponse(protocol.CodeError, "add_node needs a node ID and a peer address")
			}
			next[id] = a.Addr
		} else {
			delete(next, id)
		}
		// Waiting through the same mechanism as a write means this reports success only
		// if this entry itself commits, not merely if something reached that log slot.
		var err error
		wait := &proposal{done: make(chan proposalResult, 1)}
		if cerr := s.call(ctx, func() {
			var index uint64
			index, wait.term, err = s.node.ProposeMembership(next)
			if err == nil {
				s.waiters[index] = wait
			}
		}); cerr != nil {
			return classifyAdmin(cerr)
		}
		if err != nil {
			return classifyAdmin(err)
		}
		select {
		case r := <-wait.done:
			if r.err != nil {
				return classifyAdmin(r.err)
			}
		case <-ctx.Done():
			return classifyAdmin(ErrUnknownOutcome)
		case <-s.stopc:
			return classifyAdmin(errStopped)
		}
		return &protocol.Response{OK: true, Message: fmt.Sprintf("membership is now %v", next.IDs())}

	case "transfer":
		var err error
		if cerr := s.call(ctx, func() { err = s.node.TransferLeadership(raft.NodeID(a.Node)) }); cerr != nil {
			return classifyAdmin(cerr)
		}
		if err != nil {
			return classifyAdmin(err)
		}
		return &protocol.Response{OK: true, Message: "transfer started"}

	case "chaos":
		if !s.cfg.EnableChaos {
			return errResponse(protocol.CodeAuth, "fault injection is disabled on this node")
		}
		if a.Chaos == nil || (a.Chaos.DelayMs == 0 && a.Chaos.JitterMs == 0 && a.Chaos.DropRate == 0) {
			s.peers.SetChaos(nil)
			return &protocol.Response{OK: true, Message: "fault cleared"}
		}
		s.peers.SetChaos(&transport.Chaos{
			Delay:    time.Duration(a.Chaos.DelayMs) * time.Millisecond,
			Jitter:   time.Duration(a.Chaos.JitterMs) * time.Millisecond,
			DropRate: a.Chaos.DropRate,
			OnlyTo:   raft.NodeID(a.Chaos.OnlyTo),
		})
		return &protocol.Response{OK: true, Message: "fault injected"}
	}
	return errResponse(protocol.CodeError, "unknown admin operation "+a.Op)
}

func classifyAdmin(err error) *protocol.Response {
	if errors.Is(err, raft.ErrNotLeader) {
		return &protocol.Response{Code: protocol.CodeNotLeader, Error: err.Error()}
	}
	if errors.Is(err, raft.ErrTransferring) || errors.Is(err, raft.ErrConfigPending) || errors.Is(err, raft.ErrLeaderNotInit) {
		return &protocol.Response{Code: protocol.CodeRetry, Error: err.Error()}
	}
	return classify(err, false, false)
}

func (s *Server) statusResponse() *protocol.Response {
	v := s.view.Load()
	members := make(map[string]string, len(v.st.Members))
	for id, a := range v.st.Members {
		members[string(id)] = a
	}
	ns := &protocol.NodeStatus{
		ID: string(s.cfg.ID), Role: v.st.Role.String(), Term: v.st.Term, Leader: string(v.st.Leader),
		Commit: v.st.Commit, Applied: v.applied, LastIndex: v.st.LastIndex,
		Members: members, Tables: s.db.TableNames(),
	}
	if s.cfg.Monitor != nil {
		ns.Monitor = s.cfg.Monitor.Status()
	}
	return &protocol.Response{OK: true, Status: ns}
}
