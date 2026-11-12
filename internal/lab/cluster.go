// Package lab runs experiments against real SentinelDB processes: it starts a cluster,
// injects faults, kills nodes and records what happened. Nothing here is simulated beyond
// the injected network faults themselves.
package lab

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"sentineldb/internal/client"
	"sentineldb/internal/protocol"
	"sentineldb/internal/transport"
)

// Options configures a cluster.
type Options struct {
	Bin       string // directory holding sentinel-node
	CertDir   string
	Dir       string // working directory for this cluster's data
	Nodes     int
	Model     string // prediction model to deploy; empty means purely reactive Raft
	Threshold float64
	// Telemetry records heartbeat features for building a training set.
	Telemetry bool
	// ElectionTicks is the base election timeout in 10ms ticks (default 50: 500-1000ms).
	ElectionTicks int
	Log           io.Writer
	// PortBase, if set, uses fixed ports: clients on PortBase+100+i and peers on
	// PortBase+200+i for node i, counting from 1. Otherwise ports are chosen freely.
	PortBase int
}

// Node is one node process.
type Node struct {
	ID         string
	PeerAddr   string
	ClientAddr string
	Telemetry  string
	proc       *exec.Cmd
	mu         sync.Mutex
	exited     chan struct{}
	admin      *client.Client
}

// Cluster is a running set of node processes.
type Cluster struct {
	opts  Options
	Nodes map[string]*Node
	IDs   []string
	admin string
	write string
	mat   *transport.Material
	spec  string
}

// EnsureCerts creates the shared TLS material once, returning its directory.
func EnsureCerts(dir string, nodes int) error {
	if _, err := os.Stat(filepath.Join(dir, "ca.key")); err == nil {
		return nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	ca, key, err := transport.GenerateCA(dir)
	if err != nil {
		return err
	}
	for i := 1; i <= nodes; i++ {
		if err := transport.GenerateNodeCert(dir, fmt.Sprintf("n%d", i), ca, key, nil); err != nil {
			return err
		}
	}
	entries := []transport.TokenEntry{}
	plain := map[string]string{}
	for _, role := range []string{"read", "write", "admin"} {
		tok, err := transport.NewToken()
		if err != nil {
			return err
		}
		plain[role] = tok
		entries = append(entries, transport.TokenEntry{Name: role, Role: role, SHA256: transport.HashToken(tok)})
	}
	b, _ := json.Marshal(entries)
	if err := os.WriteFile(filepath.Join(dir, "tokens.json"), b, 0o600); err != nil {
		return err
	}
	b, _ = json.Marshal(plain)
	return os.WriteFile(filepath.Join(dir, "client-tokens.json"), b, 0o600)
}

func freePort() (string, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	defer l.Close()
	return l.Addr().String(), nil
}

// StartCluster launches every node.
func StartCluster(opts Options) (*Cluster, error) {
	if opts.ElectionTicks == 0 {
		opts.ElectionTicks = 50
	}
	if opts.Log == nil {
		opts.Log = io.Discard
	}
	if err := os.MkdirAll(opts.Dir, 0o700); err != nil {
		return nil, err
	}
	mat, err := transport.LoadMaterial(filepath.Join(opts.CertDir, "ca.crt"), "", "")
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(filepath.Join(opts.CertDir, "client-tokens.json"))
	if err != nil {
		return nil, err
	}
	var toks map[string]string
	if err := json.Unmarshal(b, &toks); err != nil {
		return nil, err
	}
	c := &Cluster{opts: opts, Nodes: map[string]*Node{}, admin: toks["admin"], write: toks["write"], mat: mat}
	var parts []string
	for i := 1; i <= opts.Nodes; i++ {
		id := fmt.Sprintf("n%d", i)
		var peer, cl string
		if opts.PortBase > 0 {
			peer = fmt.Sprintf("127.0.0.1:%d", opts.PortBase+200+i)
			cl = fmt.Sprintf("127.0.0.1:%d", opts.PortBase+100+i)
		} else {
			if peer, err = freePort(); err != nil {
				return nil, err
			}
			if cl, err = freePort(); err != nil {
				return nil, err
			}
		}
		n := &Node{ID: id, PeerAddr: peer, ClientAddr: cl, Telemetry: filepath.Join(opts.Dir, id+".telemetry.csv")}
		c.Nodes[id] = n
		c.IDs = append(c.IDs, id)
		parts = append(parts, id+"="+peer)
	}
	c.spec = strings.Join(parts, ",")
	for _, id := range c.IDs {
		if err := c.StartNode(id); err != nil {
			c.Stop()
			return nil, err
		}
	}
	return c, nil
}

// StartNode launches or relaunches one node.
func (c *Cluster) StartNode(id string) error {
	n := c.Nodes[id]
	args := []string{
		"-id", id, "-cluster", c.spec, "-client-addr", n.ClientAddr,
		"-data", filepath.Join(c.opts.Dir, id), "-certs", c.opts.CertDir,
		"-election-ticks", strconv.Itoa(c.opts.ElectionTicks), "-enable-chaos",
	}
	if c.opts.Model != "" {
		args = append(args, "-predict", c.opts.Model)
	}
	if c.opts.Threshold != 0 {
		args = append(args, "-predict-threshold", strconv.FormatFloat(c.opts.Threshold, 'f', -1, 64))
	}
	if c.opts.Telemetry {
		args = append(args, "-telemetry", n.Telemetry)
	}
	cmd := exec.Command(filepath.Join(c.opts.Bin, "sentinel-node"), args...)
	cmd.Stdout, cmd.Stderr = c.opts.Log, c.opts.Log
	if err := cmd.Start(); err != nil {
		return err
	}
	done := make(chan struct{})
	n.mu.Lock()
	n.proc, n.exited = cmd, done
	n.mu.Unlock()
	go func() {
		cmd.Wait()
		close(done)
	}()
	return nil
}

// Alive reports whether a node's process is running.
func (n *Node) Alive() bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.proc == nil {
		return false
	}
	select {
	case <-n.exited:
		return false
	default:
		return true
	}
}

// Kill terminates a node's process abruptly, with no chance to clean up.
func (c *Cluster) Kill(id string) {
	n := c.Nodes[id]
	n.mu.Lock()
	p, done := n.proc, n.exited
	n.mu.Unlock()
	if p == nil {
		return
	}
	p.Process.Kill()
	<-done
	if n.admin != nil {
		n.admin.Close()
		n.admin = nil
	}
}

// Stop terminates the whole cluster.
func (c *Cluster) Stop() {
	for _, id := range c.IDs {
		c.Kill(id)
	}
}

// Remove stops the cluster and deletes its data, retrying because Windows may briefly
// hold files open after a process exits.
func (c *Cluster) Remove() {
	c.Stop()
	for i := 0; i < 20; i++ {
		if os.RemoveAll(c.opts.Dir) == nil {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// Addrs lists client addresses, in node order.
func (c *Cluster) Addrs() []string {
	out := make([]string, 0, len(c.IDs))
	for _, id := range c.IDs {
		out = append(out, c.Nodes[id].ClientAddr)
	}
	return out
}

// NewWriter returns a client with the write role, able to reach every node.
func (c *Cluster) NewWriter(timeout time.Duration) *client.Client {
	return client.New(client.Config{Addrs: c.Addrs(), Token: c.write, TLS: c.mat.ClientConfig(), Timeout: timeout})
}

func (c *Cluster) adminClient(id string) *client.Client {
	n := c.Nodes[id]
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.admin == nil {
		n.admin = client.New(client.Config{
			Addrs: []string{n.ClientAddr}, Token: c.admin, TLS: c.mat.ClientConfig(), Timeout: 500 * time.Millisecond,
		})
	}
	return n.admin
}

// Status asks one node for its view of the cluster.
func (c *Cluster) Status(id string) (*protocol.NodeStatus, error) {
	r, err := c.adminClient(id).Admin(context.Background(), &protocol.AdminRequest{Op: "status"})
	if err != nil {
		return nil, err
	}
	return r.Status, nil
}

// Leader finds the node with the highest term that says it leads.
func (c *Cluster) Leader() (string, uint64) {
	var best string
	var term uint64
	for _, id := range c.IDs {
		if !c.Nodes[id].Alive() {
			continue
		}
		s, err := c.Status(id)
		if err == nil && s.Role == "Leader" && s.Term >= term {
			best, term = id, s.Term
		}
	}
	return best, term
}

// WaitLeader blocks until there is a leader.
func (c *Cluster) WaitLeader(ctx context.Context) (string, error) {
	for {
		if id, _ := c.Leader(); id != "" {
			return id, nil
		}
		select {
		case <-time.After(20 * time.Millisecond):
		case <-ctx.Done():
			return "", fmt.Errorf("no leader: %w", ctx.Err())
		}
	}
}

// Chaos sets the fault injected on a node's outgoing traffic. Nil clears it.
func (c *Cluster) Chaos(id string, spec *protocol.ChaosSpec) error {
	_, err := c.adminClient(id).Admin(context.Background(), &protocol.AdminRequest{Op: "chaos", Chaos: spec})
	return err
}
