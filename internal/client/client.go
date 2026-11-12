// Package client is the SentinelDB client library. It hides the cluster: it finds a live
// node, follows leadership changes, and retries writes safely using request IDs.
package client

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"sentineldb/internal/protocol"
)

// Config says how to reach a cluster.
type Config struct {
	Addrs []string
	Token string
	TLS   *tls.Config
	// Timeout bounds one Exec including all its retries (default 15s).
	Timeout time.Duration
}

// Error is a failure reported by the database.
type Error struct {
	Code string
	Msg  string
}

func (e *Error) Error() string { return e.Msg }

// Retryable says that rerunning the same request cannot make things worse.
func (e *Error) Retryable() bool {
	switch e.Code {
	case protocol.CodeNotLeader, protocol.CodeNoLeader, protocol.CodeRetry, protocol.CodeUnknownOutcome:
		return true
	}
	return false
}

// Client is a connection to a cluster. Autocommit calls are safe for concurrent use.
type Client struct {
	cfg  Config
	mu   sync.Mutex
	next int
	conn *conn
}

// New creates a client. It connects lazily.
func New(cfg Config) *Client {
	if cfg.Timeout == 0 {
		cfg.Timeout = 15 * time.Second
	}
	return &Client{cfg: cfg}
}

// Close closes the connection.
func (c *Client) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn != nil {
		c.conn.close()
		c.conn = nil
	}
}

type conn struct {
	net net.Conn
	r   *bufio.Reader
	enc *json.Encoder
}

func (c *conn) close() { c.net.Close() }

func (c *conn) roundTrip(req *protocol.Request, deadline time.Time) (*protocol.Response, error) {
	c.net.SetDeadline(deadline)
	if err := c.enc.Encode(req); err != nil {
		return nil, err
	}
	line, err := c.r.ReadBytes('\n')
	if err != nil {
		return nil, err
	}
	var resp protocol.Response
	dec := json.NewDecoder(bytes.NewReader(line))
	dec.UseNumber()
	if err := dec.Decode(&resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// Dial authenticates to one specific address.
func Dial(ctx context.Context, addr, token string, cfg *tls.Config) (*conn, error) {
	d := tls.Dialer{Config: cfg, NetDialer: &net.Dialer{Timeout: 2 * time.Second}}
	dctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	nc, err := d.DialContext(dctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	c := &conn{net: nc, r: bufio.NewReaderSize(nc, 64*1024), enc: json.NewEncoder(nc)}
	resp, err := c.roundTrip(&protocol.Request{Auth: token}, time.Now().Add(5*time.Second))
	if err != nil {
		nc.Close()
		return nil, err
	}
	if !resp.OK {
		nc.Close()
		return nil, &Error{Code: resp.Code, Msg: resp.Error}
	}
	return c, nil
}

func (c *Client) connected(ctx context.Context) (*conn, error) {
	if c.conn != nil {
		return c.conn, nil
	}
	var last error
	for range c.cfg.Addrs {
		addr := c.cfg.Addrs[c.next%len(c.cfg.Addrs)]
		c.next++
		cn, err := Dial(ctx, addr, c.cfg.Token, c.cfg.TLS)
		if err == nil {
			c.conn = cn
			return cn, nil
		}
		last = err
		var de *Error
		if errors.As(err, &de) && de.Code == protocol.CodeAuth {
			return nil, err
		}
	}
	return nil, fmt.Errorf("no node reachable: %w", last)
}

func newRequestID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

// Result is what a statement returned.
type Result = protocol.Response

// Exec runs one autocommit statement, retrying through leadership changes. Every attempt
// carries one request ID, so a write that committed just before the connection died is
// recognized on retry rather than being applied twice.
func (c *Client) Exec(ctx context.Context, query string, params ...any) (*Result, error) {
	return c.do(ctx, &protocol.Request{SQL: query, Params: params, RequestID: newRequestID()})
}

// ExecStale reads this client's connected node's local copy, which may lag the leader.
func (c *Client) ExecStale(ctx context.Context, query string, params ...any) (*Result, error) {
	return c.do(ctx, &protocol.Request{SQL: query, Params: params, Stale: true})
}

// Admin runs an administrative action, trying each node until one accepts it.
func (c *Client) Admin(ctx context.Context, a *protocol.AdminRequest) (*Result, error) {
	return c.do(ctx, &protocol.Request{Admin: a})
}

func (c *Client) do(ctx context.Context, req *protocol.Request) (*Result, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, c.cfg.Timeout)
	defer cancel()
	deadline, _ := ctx.Deadline()

	var last error
	for attempt := 0; ctx.Err() == nil; attempt++ {
		cn, err := c.connected(ctx)
		if err != nil {
			var de *Error
			if errors.As(err, &de) && !de.Retryable() {
				return nil, err
			}
			last = err
			pause(ctx, attempt)
			continue
		}
		resp, err := cn.roundTrip(req, deadline)
		if err != nil {
			cn.close()
			c.conn = nil
			last = err
			pause(ctx, attempt)
			continue
		}
		if resp.OK {
			return resp, nil
		}
		e := &Error{Code: resp.Code, Msg: resp.Error}
		// An unknown outcome is only safe to retry if the request can be recognized.
		// Without an ID, a retry could apply the write twice, so the caller must decide.
		if !e.Retryable() || (e.Code == protocol.CodeUnknownOutcome && req.RequestID == "" && req.Admin == nil) {
			return nil, e
		}
		last = e
		// Try elsewhere: this node knows nothing useful or could not reach the leader.
		cn.close()
		c.conn = nil
		pause(ctx, attempt)
	}
	if last == nil {
		last = ctx.Err()
	}
	return nil, fmt.Errorf("gave up after retries: %w", last)
}

func pause(ctx context.Context, attempt int) {
	d := 5 * time.Millisecond << min(attempt, 5)
	select {
	case <-time.After(d):
	case <-ctx.Done():
	}
}

// Tx is an explicit transaction, bound to one connection and so to one leader session.
type Tx struct {
	conn     *conn
	deadline time.Time
	done     bool
}

// Begin opens a transaction on a dedicated connection. If leadership changes, its
// statements fail with txn_aborted and the caller reruns the transaction from the start.
func (c *Client) Begin(ctx context.Context) (*Tx, error) {
	var last error
	deadline := time.Now().Add(c.cfg.Timeout)
	for attempt := 0; time.Now().Before(deadline); attempt++ {
		c.mu.Lock()
		addr := c.cfg.Addrs[c.next%len(c.cfg.Addrs)]
		c.next++
		c.mu.Unlock()
		cn, err := Dial(ctx, addr, c.cfg.Token, c.cfg.TLS)
		if err != nil {
			last = err
			pause(ctx, attempt)
			continue
		}
		resp, err := cn.roundTrip(&protocol.Request{SQL: "BEGIN"}, deadline)
		if err == nil && resp.OK {
			return &Tx{conn: cn, deadline: deadline}, nil
		}
		cn.close()
		if err != nil {
			last = err
		} else {
			e := &Error{Code: resp.Code, Msg: resp.Error}
			if !e.Retryable() {
				return nil, e
			}
			last = e
		}
		pause(ctx, attempt)
	}
	return nil, fmt.Errorf("could not begin a transaction: %w", last)
}

// Exec runs a statement inside the transaction.
func (t *Tx) Exec(query string, params ...any) (*Result, error) {
	return t.round(&protocol.Request{SQL: query, Params: params})
}

func (t *Tx) round(req *protocol.Request) (*Result, error) {
	if t.done {
		return nil, errors.New("transaction already finished")
	}
	resp, err := t.conn.roundTrip(req, t.deadline)
	if err != nil {
		t.finish()
		if errors.Is(err, io.EOF) {
			err = io.ErrUnexpectedEOF
		}
		return nil, &Error{Code: protocol.CodeUnknownOutcome, Msg: "connection lost during transaction: " + err.Error()}
	}
	if !resp.OK {
		e := &Error{Code: resp.Code, Msg: resp.Error}
		if !resp.InTxn {
			t.finish()
		}
		return nil, e
	}
	return resp, nil
}

func (t *Tx) finish() {
	t.done = true
	t.conn.close()
}

// Commit commits the transaction.
func (t *Tx) Commit() error {
	_, err := t.round(&protocol.Request{SQL: "COMMIT", RequestID: newRequestID()})
	if !t.done {
		t.finish()
	}
	return err
}

// Rollback abandons the transaction. Calling it on a finished transaction does nothing,
// so it is safe to defer.
func (t *Tx) Rollback() {
	if t.done {
		return
	}
	t.round(&protocol.Request{SQL: "ROLLBACK"})
	if !t.done {
		t.finish()
	}
}
