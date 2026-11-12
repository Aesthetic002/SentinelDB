package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"sync"
	"time"

	"sentineldb/internal/client"
	"sentineldb/internal/lab"
	"sentineldb/internal/predict"
	"sentineldb/internal/protocol"
)

//go:embed dashboard.html
var dashboardHTML []byte

const demoHistory = 45 * time.Second

// demoState owns the running cluster for the dashboard.
type demoState struct {
	mu          sync.Mutex
	ctx         context.Context
	common      common
	model       string
	mode        string
	cluster     *lab.Cluster
	prober      *lab.Prober
	writer      *client.Client
	events      []demoEvent
	probs       []probSample
	degrading   bool
	lastLeader  string
	lastTerm    uint64
	gen         int
	threshold   float64
	consecutive int
}

type demoEvent struct {
	AtMs int64  `json:"t"`
	Msg  string `json:"msg"`
	Kind string `json:"kind"` // fault, leader, info
}

type probSample struct {
	AtMs int64   `json:"t"`
	P    float64 `json:"p"`
}

type nodeView struct {
	ID        string  `json:"id"`
	Alive     bool    `json:"alive"`
	Reachable bool    `json:"reachable"`
	Role      string  `json:"role"`
	Term      uint64  `json:"term"`
	Applied   uint64  `json:"applied"`
	Prob      float64 `json:"prob"`
	RTT       float64 `json:"rtt_ms"`
}

type writeView struct {
	End int64 `json:"t"`
	Lat int64 `json:"ms"`
	OK  bool  `json:"ok"`
}

func (d *demoState) event(kind, format string, args ...any) {
	d.events = append(d.events, demoEvent{time.Now().UnixMilli(), fmt.Sprintf(format, args...), kind})
	if len(d.events) > 200 {
		d.events = d.events[len(d.events)-200:]
	}
}

// start brings up a fresh cluster in the given mode. The caller holds d.mu.
func (d *demoState) start(mode string) error {
	d.stopLocked()
	opts := d.common.options("demo")
	if mode == "proactive" {
		opts.Model = d.model
	}
	c, err := lab.StartCluster(opts)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(d.ctx, 20*time.Second)
	defer cancel()
	if _, err := c.WaitLeader(ctx); err != nil {
		c.Remove()
		return err
	}
	setup := c.NewWriter(10 * time.Second)
	defer setup.Close()
	if _, err := setup.Exec(ctx, "CREATE TABLE probes (id INT PRIMARY KEY, at INT)"); err != nil {
		c.Remove()
		return err
	}
	d.cluster, d.mode = c, mode
	d.writer = c.NewWriter(15 * time.Second)
	d.prober = lab.StartProber(d.ctx, d.writer, "probes", 5*time.Millisecond)
	d.probs = nil
	d.lastLeader, d.lastTerm = "", 0
	d.degrading = false
	d.gen++
	d.event("info", "Started a 5-node cluster in %s mode", mode)
	return nil
}

func (d *demoState) stopLocked() {
	if d.cluster == nil {
		return
	}
	d.cluster.Remove()
	d.prober.Stop()
	d.writer.Close()
	d.cluster = nil
}

// watch records leadership changes and the predictor's reading.
func (d *demoState) watch() {
	for {
		select {
		case <-d.ctx.Done():
			return
		case <-time.After(100 * time.Millisecond):
		}
		d.mu.Lock()
		c := d.cluster
		d.mu.Unlock()
		if c == nil {
			continue
		}
		id, term := c.Leader()
		var p float64
		if id != "" {
			if s, err := c.Status(id); err == nil && s.Monitor != nil {
				p = s.Monitor.Probability
			}
		}
		d.mu.Lock()
		if d.cluster == c {
			if id != "" && (id != d.lastLeader || term != d.lastTerm) {
				if d.lastLeader != "" {
					d.event("leader", "%s is now leader (term %d)", id, term)
				} else {
					d.event("leader", "%s elected leader (term %d)", id, term)
				}
				d.lastLeader, d.lastTerm = id, term
			}
			d.probs = append(d.probs, probSample{time.Now().UnixMilli(), p})
			if len(d.probs) > 600 {
				d.probs = d.probs[len(d.probs)-600:]
			}
		}
		d.mu.Unlock()
	}
}

func (d *demoState) state() map[string]any {
	d.mu.Lock()
	c, mode, prober := d.cluster, d.mode, d.prober
	events := append([]demoEvent(nil), d.events...)
	probs := append([]probSample(nil), d.probs...)
	degrading := d.degrading
	d.mu.Unlock()

	now := time.Now().UnixMilli()
	out := map[string]any{"now": now, "mode": mode, "events": events, "probs": probs, "degrading": degrading,
		"has_model": d.model != "", "threshold": d.threshold, "consecutive": d.consecutive}
	if c == nil {
		return out
	}
	nodes := make([]nodeView, 0, len(c.IDs))
	for _, id := range c.IDs {
		v := nodeView{ID: id, Alive: c.Nodes[id].Alive()}
		if v.Alive {
			if s, err := c.Status(id); err == nil {
				v.Reachable, v.Role, v.Term, v.Applied = true, s.Role, s.Term, s.Applied
				if s.Monitor != nil {
					v.Prob = s.Monitor.Probability
					v.RTT = s.Monitor.Features["rtt_mean_ms"]
				}
			}
		}
		nodes = append(nodes, v)
	}
	out["nodes"] = nodes
	cut := now - demoHistory.Milliseconds()
	samples := prober.Snapshot()
	writes := make([]writeView, 0, 512)
	ok := 0
	for _, s := range samples {
		if s.OK {
			ok++
		}
		if s.EndMs >= cut {
			writes = append(writes, writeView{s.EndMs, s.EndMs - s.StartMs, s.OK})
		}
	}
	out["writes"] = writes
	out["total_ok"] = ok
	return out
}

func (d *demoState) action(op string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	switch op {
	case "reactive", "proactive":
		if op == "proactive" && d.model == "" {
			return fmt.Errorf("no prediction model is loaded")
		}
		return d.start(op)
	}
	c := d.cluster
	if c == nil {
		return fmt.Errorf("no cluster is running")
	}
	leader := func() (string, error) {
		id, _ := c.Leader()
		if id == "" {
			return "", fmt.Errorf("there is no leader right now")
		}
		return id, nil
	}
	switch op {
	case "kill":
		id, err := leader()
		if err != nil {
			return err
		}
		c.Kill(id)
		d.event("fault", "Killed the leader, %s, with no warning", id)
	case "degrade":
		if d.degrading {
			return fmt.Errorf("a fault is already building")
		}
		id, err := leader()
		if err != nil {
			return err
		}
		d.degrading = true
		gen := d.gen
		d.event("fault", "%s started degrading: it will die in 8s", id)
		go d.degradeThenKill(gen, c, id)
	case "restart":
		n := 0
		for _, id := range c.IDs {
			if !c.Nodes[id].Alive() {
				if err := c.StartNode(id); err != nil {
					return err
				}
				n++
			}
		}
		d.event("info", "Restarted %d node(s)", n)
	default:
		return fmt.Errorf("unknown action %q", op)
	}
	return nil
}

func (d *demoState) degradeThenKill(gen int, c *lab.Cluster, id string) {
	const steps = 40
	final := protocol.ChaosSpec{DelayMs: 400, JitterMs: 80}
	for i := 1; i <= steps; i++ {
		if d.ctx.Err() != nil {
			return
		}
		f := float64(i) / steps
		spec := protocol.ChaosSpec{DelayMs: int(f * float64(final.DelayMs)), JitterMs: int(f * float64(final.JitterMs))}
		_ = c.Chaos(id, &spec)
		time.Sleep(200 * time.Millisecond)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.gen != gen || d.cluster != c {
		return
	}
	c.Kill(id)
	d.degrading = false
	d.event("fault", "%s died", id)
}

// demo runs the live dashboard. It listens on loopback only and checks the Origin of
// actions, because anyone who can reach those endpoints can kill nodes.
func demo(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("demo", flag.ExitOnError)
	var c common
	c.add(fs)
	model := fs.String("model", "ml/models/deployed.json", "model for the proactive mode")
	addr := fs.String("addr", "127.0.0.1:8088", "dashboard address")
	mode := fs.String("mode", "reactive", "starting mode: reactive or proactive")
	fs.Parse(args)
	if err := c.prepare(); err != nil {
		return err
	}
	host, _, err := net.SplitHostPort(*addr)
	if err != nil {
		return err
	}
	if ip := net.ParseIP(host); host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return fmt.Errorf("the dashboard can kill nodes, so it only listens on loopback, not %q", host)
	}
	m := *model
	if _, err := os.Stat(m); err != nil {
		log.Printf("no model at %s: proactive mode is unavailable", m)
		m = ""
	}
	d := &demoState{ctx: ctx, common: c, model: m}
	if m != "" {
		pm, err := predict.LoadModel(m)
		if err != nil {
			return err
		}
		d.threshold, d.consecutive = pm.Threshold, pm.Consecutive
	}
	d.mu.Lock()
	err = d.start(*mode)
	d.mu.Unlock()
	if err != nil {
		return err
	}
	defer func() {
		d.mu.Lock()
		defer d.mu.Unlock()
		d.stopLocked()
	}()
	go d.watch()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; script-src 'unsafe-inline'; connect-src 'self'")
		w.Write(dashboardHTML)
	})
	mux.HandleFunc("GET /api/state", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(d.state())
	})
	mux.HandleFunc("POST /api/action/{op}", func(w http.ResponseWriter, r *http.Request) {
		if !sameOrigin(r) {
			http.Error(w, "cross-origin request refused", http.StatusForbidden)
			return
		}
		if err := d.action(r.PathValue("op")); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	srv := &http.Server{Addr: *addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		srv.Close()
	}()
	log.Printf("dashboard at http://%s (Ctrl-C to stop)", *addr)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

// sameOrigin refuses requests from other sites. Without it, any web page open in the same
// browser could POST to this address and kill nodes.
func sameOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true // not a browser's cross-site request
	}
	u, err := url.Parse(origin)
	return err == nil && u.Host == r.Host
}
