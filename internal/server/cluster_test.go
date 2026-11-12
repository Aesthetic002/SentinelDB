package server

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"sentineldb/internal/client"
	"sentineldb/internal/protocol"
	"sentineldb/internal/raft"
	"sentineldb/internal/transport"
)

const (
	adminToken = "admin-token"
	writeToken = "write-token"
	readToken  = "read-token"
)

// testCluster is N real nodes in one process: real TLS over real sockets and real WAL
// files. Only the process boundary is missing, and cmd/sentinel-lab covers that.
type testCluster struct {
	t      *testing.T
	dir    string
	ca     *transport.Material
	tokens transport.Tokens
	addrs  map[raft.NodeID]string // peer addresses
	cfgs   map[raft.NodeID]Config
	nodes  map[raft.NodeID]*Server
	mu     sync.Mutex
}

func pickAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().String()
}

func newCluster(t *testing.T, n int) *testCluster {
	t.Helper()
	dir := t.TempDir()
	caCert, caKey, err := transport.GenerateCA(dir)
	if err != nil {
		t.Fatal(err)
	}
	c := &testCluster{
		t: t, dir: dir, addrs: map[raft.NodeID]string{},
		cfgs: map[raft.NodeID]Config{}, nodes: map[raft.NodeID]*Server{},
		tokens: transport.Tokens{
			transport.HashToken(adminToken): protocol.RoleAdmin,
			transport.HashToken(writeToken): protocol.RoleWrite,
			transport.HashToken(readToken):  protocol.RoleRead,
		},
	}
	c.ca, err = transport.LoadMaterial(filepath.Join(dir, "ca.crt"), "", "")
	if err != nil {
		t.Fatal(err)
	}
	members := raft.Membership{}
	for i := 1; i <= n; i++ {
		id := raft.NodeID(fmt.Sprintf("n%d", i))
		if err := transport.GenerateNodeCert(dir, string(id), caCert, caKey, nil); err != nil {
			t.Fatal(err)
		}
		c.addrs[id] = pickAddr(t)
		members[id] = c.addrs[id]
	}
	for id := range members {
		c.cfgs[id] = c.configFor(id, members)
		c.start(id)
	}
	t.Cleanup(func() {
		for id := range c.nodes {
			c.stop(id)
		}
	})
	return c
}

func (c *testCluster) configFor(id raft.NodeID, members raft.Membership) Config {
	mat, err := transport.LoadMaterial(filepath.Join(c.dir, "ca.crt"),
		filepath.Join(c.dir, string(id)+".crt"), filepath.Join(c.dir, string(id)+".key"))
	if err != nil {
		c.t.Fatal(err)
	}
	return Config{
		ID: id, PeerAddr: c.addrs[id], ClientAddr: "127.0.0.1:0", Members: members,
		DataDir:      filepath.Join(c.dir, "data-"+string(id)),
		TickInterval: 5 * time.Millisecond, ElectionTicks: 30, HeartbeatTicks: 5,
		Material: mat, Tokens: c.tokens, EnableChaos: true, NoSync: true,
		Logger: testLogger(),
	}
}

// testLogger is silent unless SENTINEL_TEST_LOG is set, for investigating a failure.
func testLogger() *log.Logger {
	if os.Getenv("SENTINEL_TEST_LOG") != "" {
		return log.New(os.Stderr, "", log.Lmicroseconds)
	}
	return log.New(io.Discard, "", 0)
}

func (c *testCluster) start(id raft.NodeID) *Server {
	c.t.Helper()
	cfg := c.cfgs[id]
	// The port from a previous run of this node may take a moment to free up.
	var s *Server
	var err error
	for i := 0; i < 50; i++ {
		s, err = Start(cfg)
		if err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		c.t.Fatalf("start %s: %v", id, err)
	}
	c.mu.Lock()
	c.nodes[id] = s
	c.mu.Unlock()
	return s
}

func (c *testCluster) stop(id raft.NodeID) {
	c.mu.Lock()
	s := c.nodes[id]
	delete(c.nodes, id)
	c.mu.Unlock()
	if s != nil {
		s.Close()
	}
}

func (c *testCluster) client(token string) *client.Client {
	var addrs []string
	for _, s := range c.nodes {
		addrs = append(addrs, s.ClientAddr())
	}
	cl := client.New(client.Config{Addrs: addrs, Token: token, TLS: c.ca.ClientConfig(), Timeout: 20 * time.Second})
	c.t.Cleanup(cl.Close)
	return cl
}

func (c *testCluster) clientFor(id raft.NodeID, token string) *client.Client {
	cl := client.New(client.Config{Addrs: []string{c.nodes[id].ClientAddr()}, Token: token, TLS: c.ca.ClientConfig(), Timeout: 20 * time.Second})
	c.t.Cleanup(cl.Close)
	return cl
}

func (c *testCluster) waitLeader() raft.NodeID {
	c.t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		var leader raft.NodeID
		var term uint64
		for id, s := range c.nodes {
			st := s.Status()
			if st.Role == raft.Leader && st.LeaderReady && st.Term > term {
				leader, term = id, st.Term
			}
		}
		if leader != raft.None {
			return leader
		}
		time.Sleep(5 * time.Millisecond)
	}
	c.t.Fatal("no leader elected")
	return raft.None
}

func (c *testCluster) follower(leader raft.NodeID) raft.NodeID {
	for id := range c.nodes {
		if id != leader {
			return id
		}
	}
	c.t.Fatal("no follower")
	return raft.None
}

// converged waits until every live node holds byte-identical state.
func (c *testCluster) converged() []byte {
	c.t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	var diff string
	for time.Now().Before(deadline) {
		var first []byte
		same := true
		var firstID raft.NodeID
		for id, s := range c.nodes {
			b, err := s.DB().Snapshot()
			if err != nil {
				c.t.Fatal(err)
			}
			if first == nil {
				first, firstID = b, id
			} else if !bytes.Equal(first, b) {
				same = false
				diff = fmt.Sprintf("%s and %s differ (%d vs %d bytes)", firstID, id, len(first), len(b))
				break
			}
		}
		if same {
			return first
		}
		time.Sleep(20 * time.Millisecond)
	}
	c.t.Fatalf("replicas never converged: %s", diff)
	return nil
}

func ctx() context.Context { return context.Background() }

func mustExec(t *testing.T, cl *client.Client, q string, params ...any) *client.Result {
	t.Helper()
	r, err := cl.Exec(ctx(), q, params...)
	if err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return r
}

func intCell(t *testing.T, v any) int64 {
	t.Helper()
	n, ok := v.(json.Number)
	if !ok {
		t.Fatalf("not a number: %#v", v)
	}
	i, err := n.Int64()
	if err != nil {
		t.Fatal(err)
	}
	return i
}

func TestWriteReplicatesToEveryNode(t *testing.T) {
	c := newCluster(t, 5)
	c.waitLeader()
	cl := c.client(writeToken)
	mustExec(t, cl, "CREATE TABLE kv (k TEXT PRIMARY KEY, v INT)")
	mustExec(t, cl, "INSERT INTO kv VALUES ('a', 1), ('b', 2)")
	r := mustExec(t, cl, "SELECT v FROM kv WHERE k = ?", "b")
	if intCell(t, r.Rows[0][0]) != 2 {
		t.Fatalf("got %v", r.Rows)
	}
	c.converged()
}

func TestAnyNodeForwardsToLeader(t *testing.T) {
	c := newCluster(t, 3)
	leader := c.waitLeader()
	follower := c.follower(leader)
	cl := c.clientFor(follower, writeToken)
	mustExec(t, cl, "CREATE TABLE t (id INT PRIMARY KEY, n INT)")
	r := mustExec(t, cl, "INSERT INTO t VALUES (1, 10)")
	if r.Affected != 1 {
		t.Fatalf("affected %d", r.Affected)
	}
	if got := intCell(t, mustExec(t, cl, "SELECT n FROM t WHERE id = 1").Rows[0][0]); got != 10 {
		t.Fatalf("forwarded read: %d", got)
	}

	// A whole transaction through a follower must stay on one leader session.
	tx, err := cl.Begin(ctx())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec("UPDATE t SET n = n + 1 WHERE id = 1"); err != nil {
		t.Fatal(err)
	}
	r2, err := tx.Exec("SELECT n FROM t WHERE id = 1")
	if err != nil {
		t.Fatal(err)
	}
	if intCell(t, r2.Rows[0][0]) != 11 {
		t.Fatalf("transaction lost its own write through the relay: %v", r2.Rows)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if got := intCell(t, mustExec(t, cl, "SELECT n FROM t WHERE id = 1").Rows[0][0]); got != 11 {
		t.Fatalf("after commit: %d", got)
	}
}

func TestCommittedWritesSurviveLeaderFailure(t *testing.T) {
	c := newCluster(t, 5)
	first := c.waitLeader()
	cl := c.client(writeToken)
	mustExec(t, cl, "CREATE TABLE t (id INT PRIMARY KEY)")
	for i := 0; i < 20; i++ {
		mustExec(t, cl, "INSERT INTO t VALUES (?)", i)
	}
	c.stop(first)

	second := c.waitLeader()
	if second == first {
		t.Fatal("dead node reported as leader")
	}
	r := mustExec(t, c.client(writeToken), "SELECT id FROM t")
	if len(r.Rows) != 20 {
		t.Fatalf("lost committed rows: have %d of 20", len(r.Rows))
	}
}

// With 5 nodes the system loses any 2 and keeps working, and with 3 down it must refuse
// writes rather than risk anything.
func TestToleratesTwoOfFiveAndRefusesWithoutMajority(t *testing.T) {
	c := newCluster(t, 5)
	c.waitLeader()
	cl := c.client(writeToken)
	mustExec(t, cl, "CREATE TABLE t (id INT PRIMARY KEY)")

	var downed []raft.NodeID
	for len(downed) < 2 {
		id := c.waitLeader()
		c.stop(id)
		downed = append(downed, id)
	}
	cl2 := c.client(writeToken)
	mustExec(t, cl2, "INSERT INTO t VALUES (1)")

	id := c.waitLeader()
	c.stop(id)
	downed = append(downed, id)

	var survivors []string
	for _, s := range c.nodes {
		survivors = append(survivors, s.ClientAddr())
	}
	short := client.New(client.Config{Addrs: survivors, Token: writeToken, TLS: c.ca.ClientConfig(), Timeout: 1500 * time.Millisecond})
	defer short.Close()
	if _, err := short.Exec(ctx(), "INSERT INTO t VALUES (2)"); err == nil {
		t.Fatal("wrote with only 2 of 5 nodes up: that is not a majority")
	}

	// Recovery: with a majority back, service resumes and nothing is lost or invented.
	for _, id := range downed {
		c.start(id)
	}
	c.waitLeader()
	r := mustExec(t, c.client(writeToken), "SELECT id FROM t ORDER BY id")
	if len(r.Rows) != 1 || intCell(t, r.Rows[0][0]) != 1 {
		t.Fatalf("expected exactly row 1 to exist after recovery, got %v", r.Rows)
	}
	c.converged()
}

func TestRestartedNodeCatchesUpFromDisk(t *testing.T) {
	c := newCluster(t, 3)
	leader := c.waitLeader()
	follower := c.follower(leader)
	cl := c.clientFor(leader, writeToken)
	mustExec(t, cl, "CREATE TABLE t (id INT PRIMARY KEY)")
	mustExec(t, cl, "INSERT INTO t VALUES (1)")
	c.stop(follower)
	mustExec(t, cl, "INSERT INTO t VALUES (2)")
	c.start(follower)
	mustExec(t, cl, "INSERT INTO t VALUES (3)")
	c.converged()
}

func TestSnapshotCatchUpOverTheNetwork(t *testing.T) {
	c := newCluster(t, 3)
	// Compact aggressively, so that a stopped follower falls behind the retained log.
	for id, cfg := range c.cfgs {
		cfg.SnapshotEvery = 10
		c.cfgs[id] = cfg
		c.stop(id)
		c.start(id)
	}
	leader := c.waitLeader()
	follower := c.follower(leader)
	cl := c.clientFor(leader, writeToken)
	mustExec(t, cl, "CREATE TABLE t (id INT PRIMARY KEY)")
	c.stop(follower)
	for i := 0; i < 60; i++ {
		mustExec(t, cl, "INSERT INTO t VALUES (?)", i)
	}
	st := c.nodes[leader].Status()
	if st.FirstIndex <= 2 {
		t.Fatalf("leader never compacted: first index %d", st.FirstIndex)
	}
	s := c.start(follower)
	c.converged()
	if got := s.Status().FirstIndex; got <= 2 {
		t.Fatalf("follower should have installed a snapshot, first index %d", got)
	}
	// It must also come back correctly from the snapshot on disk.
	c.stop(follower)
	c.start(follower)
	mustExec(t, cl, "INSERT INTO t VALUES (1000)")
	c.converged()
}

// The motivating example from the design notes. Under real replication, real locks and
// real failover, the invariant must hold: no money created or destroyed, and every
// acknowledged transfer present.
func TestBankInvariantAcrossLeaderChanges(t *testing.T) {
	c := newCluster(t, 3)
	c.waitLeader()
	setup := c.client(writeToken)
	mustExec(t, setup, "CREATE TABLE accounts (id INT PRIMARY KEY, balance INT NOT NULL)")
	const accounts, each = 4, 1000
	for i := 0; i < accounts; i++ {
		mustExec(t, setup, "INSERT INTO accounts VALUES (?, ?)", i, each)
	}

	stop := make(chan struct{})
	var wg sync.WaitGroup
	var committed, aborted atomic.Int64
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			cl := c.client(writeToken)
			for i := 0; ; i++ {
				select {
				case <-stop:
					return
				default:
				}
				from, to := (i+w)%accounts, (i+w+1)%accounts
				tx, err := cl.Begin(ctx())
				if err != nil {
					aborted.Add(1)
					continue
				}
				_, e1 := tx.Exec("UPDATE accounts SET balance = balance - 1 WHERE id = ?", from)
				_, e2 := error(nil), error(nil)
				if e1 == nil {
					_, e2 = tx.Exec("UPDATE accounts SET balance = balance + 1 WHERE id = ?", to)
				}
				if e1 == nil && e2 == nil && tx.Commit() == nil {
					committed.Add(1)
				} else {
					tx.Rollback()
					aborted.Add(1)
				}
			}
		}(w)
	}

	// Force several leadership changes while transfers run.
	for i := 0; i < 4; i++ {
		time.Sleep(300 * time.Millisecond)
		leader := c.waitLeader()
		if i%2 == 0 {
			c.stop(leader)
			time.Sleep(200 * time.Millisecond)
			c.start(leader)
		} else {
			to := c.follower(leader)
			c.clientFor(leader, adminToken).Admin(ctx(), &protocol.AdminRequest{Op: "transfer", Node: string(to)})
		}
	}
	time.Sleep(300 * time.Millisecond)
	close(stop)
	wg.Wait()

	c.waitLeader()
	r := mustExec(t, c.client(readToken), "SELECT balance FROM accounts")
	var sum int64
	for _, row := range r.Rows {
		sum += intCell(t, row[0])
	}
	if sum != accounts*each {
		t.Fatalf("total is %d, want %d: atomicity or replication failed", sum, accounts*each)
	}
	if committed.Load() == 0 {
		t.Fatalf("no transfer ever committed; the test proves nothing")
	}
	t.Logf("%d committed, %d aborted across forced leadership changes", committed.Load(), aborted.Load())
	c.converged()
}

// Every acknowledged write must be there afterwards, and retries must not apply twice.
// Counting increments makes both visible: the final count must lie between the number
// acknowledged and the number attempted.
func TestAcknowledgedWritesNeverLostOrDoubled(t *testing.T) {
	c := newCluster(t, 3)
	c.waitLeader()
	cl := c.client(writeToken)
	mustExec(t, cl, "CREATE TABLE counter (id INT PRIMARY KEY, n INT NOT NULL)")
	mustExec(t, cl, "INSERT INTO counter VALUES (1, 0)")

	stop := make(chan struct{})
	var attempted, acked atomic.Int64
	var wg sync.WaitGroup
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			wcl := c.client(writeToken)
			for {
				select {
				case <-stop:
					return
				default:
				}
				attempted.Add(1)
				if _, err := wcl.Exec(ctx(), "UPDATE counter SET n = n + 1 WHERE id = 1"); err == nil {
					acked.Add(1)
				}
			}
		}()
	}
	for i := 0; i < 3; i++ {
		time.Sleep(300 * time.Millisecond)
		leader := c.waitLeader()
		c.stop(leader)
		time.Sleep(150 * time.Millisecond)
		c.start(leader)
	}
	time.Sleep(200 * time.Millisecond)
	close(stop)
	wg.Wait()

	c.waitLeader()
	r := mustExec(t, c.client(readToken), "SELECT n FROM counter WHERE id = 1")
	n := intCell(t, r.Rows[0][0])
	if n < acked.Load() {
		t.Fatalf("LOST WRITES: %d acknowledged but the counter is %d", acked.Load(), n)
	}
	if n > attempted.Load() {
		t.Fatalf("DOUBLED WRITES: %d attempted but the counter is %d", attempted.Load(), n)
	}
	if acked.Load() < 10 {
		t.Fatalf("only %d writes acknowledged; the test proves little", acked.Load())
	}
	t.Logf("attempted %d, acknowledged %d, counter %d", attempted.Load(), acked.Load(), n)
}

// TestLeadershipTransferEndToEnd checks that a transfer really moves leadership to the node
// that was asked for, over real sockets. A transfer is allowed to fail: a leader abandons
// one that has not finished within an election timeout, and on a badly starved machine that
// happens, as do spurious elections. Both are safe. So this retries, as the predictor does
// after its cooldown, and asks whichever node is leader now. The precise guarantee, that a
// transfer does not wait for a timer, is checked without timing noise in the deterministic
// TestLeadershipTransfer.
func TestLeadershipTransferEndToEnd(t *testing.T) {
	c := newCluster(t, 5)
	c.waitLeader()
	cl := c.client(writeToken)
	mustExec(t, cl, "CREATE TABLE t (id INT PRIMARY KEY)")
	mustExec(t, cl, "INSERT INTO t VALUES (1)")

	var to raft.NodeID
	start := time.Now()
	attempts := 0
	for done := false; !done; {
		if time.Since(start) > 30*time.Second {
			var b strings.Builder
			for id, n := range c.nodes {
				st := n.Status()
				fmt.Fprintf(&b, "\n  %s: %s term %d leader %q last %d commit %d transferee %q",
					id, st.Role, st.Term, st.Leader, st.LastIndex, st.Commit, st.Transferee)
			}
			t.Fatalf("leadership never reached %s in %d attempts:%s", to, attempts, b.String())
		}
		leader := c.waitLeader()
		if to == raft.None || to == leader {
			to = c.follower(leader)
		}
		attempts++
		admin := client.New(client.Config{
			Addrs: []string{c.nodes[leader].ClientAddr()}, Token: adminToken,
			TLS: c.ca.ClientConfig(), Timeout: time.Second,
		})
		_, err := admin.Admin(ctx(), &protocol.AdminRequest{Op: "transfer", Node: string(to)})
		admin.Close()
		if err != nil {
			continue // leadership moved on before we asked; look again
		}
		for wait := time.Now(); time.Since(wait) < time.Second; time.Sleep(time.Millisecond) {
			if c.nodes[to].Status().Role == raft.Leader {
				done = true
				break
			}
		}
	}
	t.Logf("leadership reached %s after %v, on attempt %d", to, time.Since(start), attempts)
	mustExec(t, c.client(writeToken), "INSERT INTO t VALUES (2)")
	c.converged()
}

func TestStaleReadFromFollower(t *testing.T) {
	c := newCluster(t, 3)
	leader := c.waitLeader()
	follower := c.follower(leader)
	cl := c.clientFor(leader, writeToken)
	mustExec(t, cl, "CREATE TABLE t (id INT PRIMARY KEY)")
	mustExec(t, cl, "INSERT INTO t VALUES (1)")
	c.converged()

	fcl := c.clientFor(follower, readToken)
	r, err := fcl.ExecStale(ctx(), "SELECT id FROM t")
	if err != nil {
		t.Fatal(err)
	}
	if !r.Stale || r.Node != string(follower) || len(r.Rows) != 1 {
		t.Fatalf("stale read should be served locally and marked: %+v", r)
	}
	// It still works with the leader gone, where a strong read cannot.
	c.stop(leader)
	if _, err := fcl.ExecStale(ctx(), "SELECT id FROM t"); err != nil {
		t.Fatalf("stale reads should survive losing the leader: %v", err)
	}
	if _, err := fcl.ExecStale(ctx(), "DELETE FROM t"); err == nil {
		t.Fatal("a stale request must not be able to write")
	}
}

func TestMembershipAddAndRemove(t *testing.T) {
	c := newCluster(t, 3)
	leader := c.waitLeader()
	cl := c.clientFor(leader, writeToken)
	mustExec(t, cl, "CREATE TABLE t (id INT PRIMARY KEY)")
	mustExec(t, cl, "INSERT INTO t VALUES (1)")

	// A new node starts empty and learns everything from the leader.
	caCert, caKey := readCAFiles(t, c.dir)
	if err := transport.GenerateNodeCert(c.dir, "n4", caCert, caKey, nil); err != nil {
		t.Fatal(err)
	}
	c.addrs["n4"] = pickAddr(t)
	cfg := c.configFor("n4", nil)
	cfg.Contacts = raft.Membership{"n1": c.addrs["n1"], "n2": c.addrs["n2"], "n3": c.addrs["n3"]}
	c.cfgs["n4"] = cfg
	joiner := c.start("n4")
	if joiner.Status().Role != raft.Follower {
		t.Fatal("a joining node must not campaign")
	}

	admin := c.clientFor(leader, adminToken)
	req := &protocol.AdminRequest{Op: "add_node", Node: "n4", Addr: c.addrs["n4"]}
	if _, err := c.clientFor(leader, writeToken).Admin(ctx(), req); err == nil {
		t.Fatal("a write token must not be able to change membership")
	}
	if _, err := admin.Admin(ctx(), req); err != nil {
		t.Fatal(err)
	}
	c.converged()
	if _, ok := joiner.Status().Members["n4"]; !ok {
		t.Fatalf("joiner does not know it was added: %v", joiner.Status().Members)
	}

	if _, err := admin.Admin(ctx(), &protocol.AdminRequest{Op: "remove_node", Node: string(c.follower(leader))}); err != nil {
		t.Fatal(err)
	}
	mustExec(t, cl, "INSERT INTO t VALUES (2)")
}

// --- security -----------------------------------------------------------------

func TestTokensAndRoles(t *testing.T) {
	c := newCluster(t, 3)
	c.waitLeader()
	w := c.client(writeToken)
	mustExec(t, w, "CREATE TABLE t (id INT PRIMARY KEY)")

	r := c.client(readToken)
	mustExec(t, r, "SELECT * FROM t")
	if _, err := r.Exec(ctx(), "INSERT INTO t VALUES (1)"); !isCode(err, protocol.CodeAuth) {
		t.Fatalf("a read-only token wrote: %v", err)
	}
	// Reads must stay read-only inside a transaction too, including through the relay.
	for id := range c.nodes {
		tx, err := c.clientFor(id, readToken).Begin(ctx())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec("DELETE FROM t"); !isCode(err, protocol.CodeAuth) {
			t.Fatalf("a read-only token wrote inside a transaction on %s: %v", id, err)
		}
		tx.Rollback()
	}
	if _, err := r.Admin(ctx(), &protocol.AdminRequest{Op: "transfer", Node: "n1"}); !isCode(err, protocol.CodeAuth) {
		t.Fatalf("a read token ran an admin action: %v", err)
	}

	bad := client.New(client.Config{Addrs: []string{c.nodes["n1"].ClientAddr()}, Token: "wrong", TLS: c.ca.ClientConfig(), Timeout: time.Second})
	defer bad.Close()
	if _, err := bad.Exec(ctx(), "SELECT * FROM t"); !isCode(err, protocol.CodeAuth) {
		t.Fatalf("a bad token was accepted: %v", err)
	}
}

func isCode(err error, code string) bool {
	var e *client.Error
	return errors.As(err, &e) && e.Code == code
}

// A client cannot claim an elevated role by setting the field nodes use when relaying.
func TestClientCannotForgeForwardedRole(t *testing.T) {
	c := newCluster(t, 3)
	leader := c.waitLeader()
	mustExec(t, c.clientFor(leader, writeToken), "CREATE TABLE t (id INT PRIMARY KEY)")

	conn, err := tls.Dial("tcp", c.nodes[leader].ClientAddr(), c.ca.ClientConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	enc := json.NewEncoder(conn)
	dec := json.NewDecoder(conn)
	var resp protocol.Response
	enc.Encode(protocol.Request{Auth: readToken})
	dec.Decode(&resp)
	enc.Encode(protocol.Request{SQL: "INSERT INTO t VALUES (1)", ForwardRole: protocol.RoleAdmin})
	resp = protocol.Response{}
	dec.Decode(&resp)
	if resp.OK || resp.Code != protocol.CodeAuth {
		t.Fatalf("forged role was honoured: %+v", resp)
	}
}

func TestPeerPortRejectsUnauthorizedConnections(t *testing.T) {
	c := newCluster(t, 3)
	c.waitLeader()
	addr := c.addrs["n1"]

	dial := func(cfg *tls.Config) error {
		conn, err := tls.DialWithDialer(&net.Dialer{Timeout: time.Second}, "tcp", addr, cfg)
		if err != nil {
			return err
		}
		defer conn.Close()
		// TLS 1.3 clients finish their handshake before the server's verdict arrives,
		// so a rejection shows up on the first read.
		conn.SetDeadline(time.Now().Add(time.Second))
		conn.Write([]byte{transport.StreamRaft})
		_, err = conn.Read(make([]byte, 1))
		if err != nil && strings.Contains(err.Error(), "timeout") {
			return nil // the connection was held open: it was accepted
		}
		return err
	}

	// No certificate at all.
	noCert := c.ca.ClientConfig()
	noCert.ServerName = "n1"
	if err := dial(noCert); err == nil {
		t.Fatal("peer port accepted a connection with no certificate")
	}

	// A certificate from the right CA, but a client's rather than a node's.
	caCert, caKey := readCAFiles(t, c.dir)
	if err := transport.GenerateClientCert(c.dir, "mallory", caCert, caKey); err != nil {
		t.Fatal(err)
	}
	m, err := transport.LoadMaterial(filepath.Join(c.dir, "ca.crt"), filepath.Join(c.dir, "mallory.crt"), filepath.Join(c.dir, "mallory.key"))
	if err != nil {
		t.Fatal(err)
	}
	if err := dial(m.PeerClientConfig("n1")); err == nil {
		t.Fatal("peer port accepted a non-node certificate")
	}

	// A real node certificate from a different CA.
	other := t.TempDir()
	oc, ok, err := transport.GenerateCA(other)
	if err != nil {
		t.Fatal(err)
	}
	if err := transport.GenerateNodeCert(other, "n1", oc, ok, nil); err != nil {
		t.Fatal(err)
	}
	om, err := transport.LoadMaterial(filepath.Join(c.dir, "ca.crt"), filepath.Join(other, "n1.crt"), filepath.Join(other, "n1.key"))
	if err != nil {
		t.Fatal(err)
	}
	if err := dial(om.PeerClientConfig("n1")); err == nil {
		t.Fatal("peer port accepted a certificate from another CA")
	}

	// As a control, a genuine node certificate must get through.
	good, err := transport.LoadMaterial(filepath.Join(c.dir, "ca.crt"), filepath.Join(c.dir, "n2.crt"), filepath.Join(c.dir, "n2.key"))
	if err != nil {
		t.Fatal(err)
	}
	if err := dial(good.PeerClientConfig("n1")); err != nil {
		t.Fatalf("a real node was refused, so the checks above prove nothing: %v", err)
	}
}

// A node cannot send messages claiming to come from another node.
func TestPeerCannotImpersonateAnotherNode(t *testing.T) {
	c := newCluster(t, 3)
	c.waitLeader()
	termBefore := c.nodes["n1"].Status().Term

	mat, err := transport.LoadMaterial(filepath.Join(c.dir, "ca.crt"), filepath.Join(c.dir, "n2.crt"), filepath.Join(c.dir, "n2.key"))
	if err != nil {
		t.Fatal(err)
	}
	conn, err := transport.DialPeer(ctx(), mat, "n1", c.addrs["n1"], transport.StreamRaft)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	// n2's certificate, but the message says it is from n3.
	enc := gobEncoder(conn)
	if err := enc.Encode(&raft.Message{Type: raft.MsgVote, From: "n3", To: "n1", Term: termBefore + 100}); err != nil {
		t.Fatal(err)
	}
	conn.SetDeadline(time.Now().Add(2 * time.Second))
	if _, err := conn.Read(make([]byte, 1)); err == nil {
		t.Fatal("connection should be dropped")
	}
	if got := c.nodes["n1"].Status().Term; got >= termBefore+100 {
		t.Fatalf("a forged message moved n1 to term %d", got)
	}
}

func TestClientRejectsServerFromAnotherCluster(t *testing.T) {
	c := newCluster(t, 3)
	other := t.TempDir()
	if _, _, err := transport.GenerateCA(other); err != nil {
		t.Fatal(err)
	}
	wrong, err := transport.LoadMaterial(filepath.Join(other, "ca.crt"), "", "")
	if err != nil {
		t.Fatal(err)
	}
	_, err = tls.Dial("tcp", c.nodes["n1"].ClientAddr(), wrong.ClientConfig())
	if err == nil {
		t.Fatal("client trusted a server it has no reason to trust")
	}
}

func TestRequireClientCertification(t *testing.T) {
	c := newCluster(t, 1)
	cfg := c.cfgs["n1"]
	cfg.RequireClientCert = true
	c.cfgs["n1"] = cfg
	c.stop("n1")
	s := c.start("n1")
	c.waitLeader()

	// Token but no certificate: refused.
	cl := client.New(client.Config{Addrs: []string{s.ClientAddr()}, Token: writeToken, TLS: c.ca.ClientConfig(), Timeout: 700 * time.Millisecond})
	defer cl.Close()
	if _, err := cl.Exec(ctx(), "SELECT 1 FROM t"); err == nil {
		t.Fatal("a client without a certificate got through")
	}

	caCert, caKey := readCAFiles(t, c.dir)
	if err := transport.GenerateClientCert(c.dir, "alice", caCert, caKey); err != nil {
		t.Fatal(err)
	}
	m, err := transport.LoadMaterial(filepath.Join(c.dir, "ca.crt"), filepath.Join(c.dir, "alice.crt"), filepath.Join(c.dir, "alice.key"))
	if err != nil {
		t.Fatal(err)
	}
	ok := client.New(client.Config{Addrs: []string{s.ClientAddr()}, Token: writeToken, TLS: m.ClientConfig(), Timeout: 5 * time.Second})
	defer ok.Close()
	mustExec(t, ok, "CREATE TABLE t (id INT PRIMARY KEY)")
}

func TestChaosRefusedUnlessEnabled(t *testing.T) {
	c := newCluster(t, 1)
	cfg := c.cfgs["n1"]
	cfg.EnableChaos = false
	c.cfgs["n1"] = cfg
	c.stop("n1")
	s := c.start("n1")
	c.waitLeader()
	cl := client.New(client.Config{Addrs: []string{s.ClientAddr()}, Token: adminToken, TLS: c.ca.ClientConfig(), Timeout: time.Second})
	defer cl.Close()
	if _, err := cl.Admin(ctx(), &protocol.AdminRequest{Op: "chaos", Chaos: &protocol.ChaosSpec{DelayMs: 100}}); err == nil {
		t.Fatal("a production node accepted fault injection")
	}
}

// A failed accept must not end the accept loop: the node would go on voting and looking
// healthy while refusing every new connection, until someone restarted it.
func TestNodeSurvivesAcceptErrors(t *testing.T) {
	c := newCluster(t, 1)
	s := c.nodes["n1"]
	c.waitLeader()

	fake := &flakyListener{Listener: s.listeners[1], failures: 5}
	s.connWG.Add(1)
	go s.acceptLoop(fake, s.handleClientConn)
	deadline := time.Now().Add(5 * time.Second)
	for s.acceptErrors.Load() < 5 {
		if time.Now().After(deadline) {
			t.Fatalf("the accept loop stopped after %d errors", s.acceptErrors.Load())
		}
		time.Sleep(time.Millisecond)
	}
	mustExec(t, c.clientFor("n1", writeToken), "CREATE TABLE t (id INT PRIMARY KEY)")
}

// flakyListener fails a set number of accepts before behaving normally.
type flakyListener struct {
	net.Listener
	mu       sync.Mutex
	failures int
}

func (l *flakyListener) Accept() (net.Conn, error) {
	l.mu.Lock()
	fail := l.failures > 0
	if fail {
		l.failures--
	}
	l.mu.Unlock()
	if fail {
		return nil, errors.New("simulated temporary accept failure")
	}
	return l.Listener.Accept()
}

func TestShutdownIsPrompt(t *testing.T) {
	c := newCluster(t, 3)
	leader := c.waitLeader()
	cl := c.clientFor(c.follower(leader), writeToken)
	mustExec(t, cl, "CREATE TABLE t (id INT PRIMARY KEY)")
	// Leave a transaction holding a lock with another waiting on it.
	tx, err := c.clientFor(leader, writeToken).Begin(ctx())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec("INSERT INTO t VALUES (1)"); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	c.stop(leader)
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("shutdown took %v", d)
	}
}
