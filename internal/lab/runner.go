package lab

import (
	"bufio"
	"context"
	"encoding/csv"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"sentineldb/internal/client"
)

// WriteSample is one completed client write.
type WriteSample struct {
	StartMs, EndMs int64
	OK             bool
}

// Prober is a client doing steady writes and recording how each one went. It sees exactly
// what an application would, including every retry the client library does on its behalf.
type Prober struct {
	mu      sync.Mutex
	samples []WriteSample
	stop    chan struct{}
	done    chan struct{}
}

// StartProber begins writing. pause sets the rate.
func StartProber(ctx context.Context, cl *client.Client, table string, pause time.Duration) *Prober {
	p := &Prober{stop: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(p.done)
		for i := int64(0); ; i++ {
			select {
			case <-p.stop:
				return
			case <-ctx.Done():
				return
			default:
			}
			start := time.Now()
			_, err := cl.Exec(ctx, "INSERT INTO "+table+" VALUES (?, ?)", i, start.UnixMilli())
			end := time.Now()
			p.mu.Lock()
			p.samples = append(p.samples, WriteSample{start.UnixMilli(), end.UnixMilli(), err == nil})
			p.mu.Unlock()
			if pause > 0 {
				time.Sleep(pause)
			}
		}
	}()
	return p
}

// Stop ends the prober and returns its samples.
func (p *Prober) Stop() []WriteSample {
	close(p.stop)
	<-p.done
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.samples
}

// Snapshot returns the samples so far.
func (p *Prober) Snapshot() []WriteSample {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]WriteSample(nil), p.samples...)
}

// LeaderEvent records when the observed leader changed.
type LeaderEvent struct {
	AtMs   int64
	Leader string
	Term   uint64
}

// watchLeader polls for leadership changes until stopped.
func watchLeader(c *Cluster, stop <-chan struct{}) <-chan []LeaderEvent {
	out := make(chan []LeaderEvent, 1)
	go func() {
		var events []LeaderEvent
		var last string
		var lastTerm uint64
		for {
			select {
			case <-stop:
				out <- events
				return
			case <-time.After(25 * time.Millisecond):
			}
			if id, term := c.Leader(); id != "" && (id != last || term != lastTerm) {
				events = append(events, LeaderEvent{nowMs(), id, term})
				last, lastTerm = id, term
			}
		}
	}()
	return out
}

// Trial is the outcome of one bench run.
type Trial struct {
	Mode, Scenario string
	N              int
	Victim         string
	// HandedOver is whether leadership left the victim before it died, and LeadMs how far
	// ahead of the death that was.
	HandedOver bool
	LeadMs     int64
	// OutageMs is the longest stretch with no write completing, from fault start to the
	// end of the run. This is what a client actually experiences.
	OutageMs int64
	// PostKillGapMs is the same, measured only from the moment of the kill.
	PostKillGapMs int64
	// DegradedP99Ms is the p99 write latency while the fault was building, before the kill.
	DegradedP99Ms int64
	Writes        int
	Failed        int
	LeaderChanges int
	DurationMs    int64
	Timeline      []WriteSample
	Leaders       []LeaderEvent
}

// Bench runs one measured trial.
func Bench(ctx context.Context, opts Options, sc *Scenario, mode string, n int, seed int64) (*Trial, error) {
	c, err := StartCluster(opts)
	if err != nil {
		return nil, err
	}
	defer c.Remove()
	r := rand.New(rand.NewSource(seed))

	bootCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	if _, err := c.WaitLeader(bootCtx); err != nil {
		return nil, err
	}
	setup := c.NewWriter(10 * time.Second)
	defer setup.Close()
	if _, err := setup.Exec(ctx, "CREATE TABLE probes (id INT PRIMARY KEY, at INT)"); err != nil {
		return nil, err
	}
	writer := c.NewWriter(15 * time.Second)
	defer writer.Close()
	prober := StartProber(ctx, writer, "probes", 5*time.Millisecond)

	// Let any predictor finish its warm-up on a healthy cluster before anything happens.
	if err := sleep(ctx, 4*time.Second); err != nil {
		return nil, err
	}
	victim, err := c.WaitLeader(bootCtx)
	if err != nil {
		return nil, err
	}
	stop := make(chan struct{})
	leaders := watchLeader(c, stop)

	ev := &Events{Victim: victim}
	start := nowMs()
	runErr := sc.Run(ctx, c, victim, r, ev)
	end := nowMs()
	close(stop)
	le := <-leaders
	samples := prober.Stop()
	if runErr != nil {
		return nil, runErr
	}

	t := &Trial{Mode: mode, Scenario: sc.Name, N: n, Victim: victim, Timeline: samples, Leaders: le, DurationMs: end - start}
	if len(le) > 0 {
		t.LeaderChanges = len(le) - 1
	}
	analyze(t, samples, ev, start, end)
	return t, nil
}

func analyze(t *Trial, samples []WriteSample, ev *Events, start, end int64) {
	var degraded []int64
	last := start
	lastAfterKill := ev.KillMs
	for _, s := range samples {
		if s.EndMs < start {
			continue
		}
		t.Writes++
		if !s.OK {
			t.Failed++
			continue
		}
		if s.EndMs-last > t.OutageMs {
			t.OutageMs = s.EndMs - last
		}
		last = s.EndMs
		if ev.KillMs > 0 && s.EndMs >= ev.KillMs {
			if s.EndMs-lastAfterKill > t.PostKillGapMs {
				t.PostKillGapMs = s.EndMs - lastAfterKill
			}
			lastAfterKill = s.EndMs
		}
		if ev.KillMs == 0 || s.EndMs < ev.KillMs {
			degraded = append(degraded, s.EndMs-s.StartMs)
		}
	}
	if end-last > t.OutageMs {
		t.OutageMs = end - last
	}
	if ev.KillMs > 0 && end-lastAfterKill > t.PostKillGapMs {
		t.PostKillGapMs = end - lastAfterKill
	}
	sort.Slice(degraded, func(i, j int) bool { return degraded[i] < degraded[j] })
	if n := len(degraded); n > 0 {
		t.DegradedP99Ms = degraded[min(n-1, n*99/100)]
	}
	if ev.KillMs > 0 {
		for _, e := range t.Leaders {
			if e.Leader != ev.Victim && e.AtMs < ev.KillMs {
				t.HandedOver = true
				t.LeadMs = ev.KillMs - e.AtMs
				break
			}
		}
	}
}

// WriteHeader writes the bench CSV header.
func WriteHeader(w *csv.Writer) error {
	return w.Write([]string{"mode", "scenario", "trial", "victim", "handed_over", "lead_ms", "outage_ms",
		"post_kill_gap_ms", "degraded_p99_ms", "writes", "failed", "leader_changes", "duration_ms"})
}

// Row renders a trial for the CSV.
func (t *Trial) Row() []string {
	i := func(v int64) string { return strconv.FormatInt(v, 10) }
	return []string{t.Mode, t.Scenario, strconv.Itoa(t.N), t.Victim, strconv.FormatBool(t.HandedOver), i(t.LeadMs),
		i(t.OutageMs), i(t.PostKillGapMs), i(t.DegradedP99Ms), strconv.Itoa(t.Writes), strconv.Itoa(t.Failed),
		strconv.Itoa(t.LeaderChanges), i(t.DurationMs)}
}

// Collect runs one data-collection trial, appending telemetry and event times to the two
// dataset files. Nothing is labelled here: ml/train.py derives labels from the event times,
// so a change to the labelling rule never requires collecting everything again.
func Collect(ctx context.Context, opts Options, sc *Scenario, runID string, seed int64, runs, telemetry *csv.Writer) error {
	opts.Telemetry = true
	opts.Model = ""
	c, err := StartCluster(opts)
	if err != nil {
		return err
	}
	defer c.Remove()
	r := rand.New(rand.NewSource(seed))

	bootCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	if _, err := c.WaitLeader(bootCtx); err != nil {
		return err
	}
	setup := c.NewWriter(10 * time.Second)
	defer setup.Close()
	if _, err := setup.Exec(ctx, "CREATE TABLE probes (id INT PRIMARY KEY, at INT)"); err != nil {
		return err
	}
	writer := c.NewWriter(10 * time.Second)
	defer writer.Close()
	// A light background workload, so features are not measured on an idle system.
	prober := StartProber(ctx, writer, "probes", 20*time.Millisecond)

	if err := sleep(ctx, 4*time.Second); err != nil {
		return err
	}
	victim, err := c.WaitLeader(bootCtx)
	if err != nil {
		return err
	}
	ev := &Events{Victim: victim}
	if err := sc.Run(ctx, c, victim, r, ev); err != nil {
		return err
	}
	prober.Stop()
	c.Stop()

	if err := runs.Write([]string{runID, sc.Name, strconv.FormatBool(sc.Degrades), strconv.FormatBool(sc.KillsLeader), victim,
		strconv.FormatInt(ev.FaultStartMs, 10), strconv.FormatInt(ev.KillMs, 10)}); err != nil {
		return err
	}
	for _, id := range c.IDs {
		if err := appendTelemetry(c.Nodes[id].Telemetry, runID, telemetry); err != nil {
			return err
		}
	}
	runs.Flush()
	telemetry.Flush()
	if err := runs.Error(); err != nil {
		return err
	}
	return telemetry.Error()
}

func appendTelemetry(path, runID string, w *csv.Writer) error {
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil // this node never led
	}
	if err != nil {
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		fields := strings.Split(line, ",")
		// A killed process can leave a partial last line. Dropping it is correct; the row
		// was never completely written.
		if len(fields) != 9 {
			continue
		}
		if err := w.Write(append([]string{runID}, fields...)); err != nil {
			return err
		}
	}
	return sc.Err()
}

// WorkDir returns a fresh per-run directory under base.
func WorkDir(base, name string) string {
	return filepath.Join(base, fmt.Sprintf("%s-%d", name, time.Now().UnixNano()))
}
