// sentinel-lab runs the experiments behind the project's results, using real node
// processes: "collect" builds the training set; "bench" compares reactive with proactive.
package main

import (
	"context"
	"encoding/csv"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"sentineldb/internal/lab"
	"sentineldb/internal/predict"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	var err error
	switch os.Args[1] {
	case "collect":
		err = collect(ctx, os.Args[2:])
	case "bench":
		err = bench(ctx, os.Args[2:])
	case "demo":
		err = demo(ctx, os.Args[2:])
	case "serve":
		err = serve(ctx, os.Args[2:])
	default:
		usage()
	}
	if err != nil {
		log.Fatal(err)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: sentinel-lab serve|demo|collect|bench [flags]")
	os.Exit(2)
}

type common struct {
	bin, work, certs string
	nodes            int
	election         int
}

func (c *common) add(fs *flag.FlagSet) {
	fs.StringVar(&c.bin, "bin", "bin", "directory holding sentinel-node")
	fs.StringVar(&c.work, "work", "lab-runs", "working directory")
	fs.IntVar(&c.nodes, "nodes", 5, "cluster size")
	fs.IntVar(&c.election, "election-ticks", 50, "base election timeout in 10ms ticks")
}

func (c *common) prepare() error {
	c.certs = filepath.Join(c.work, "certs")
	return lab.EnsureCerts(c.certs, c.nodes)
}

func (c *common) options(name string) lab.Options {
	return lab.Options{
		Bin: c.bin, CertDir: c.certs, Dir: lab.WorkDir(c.work, name),
		Nodes: c.nodes, ElectionTicks: c.election,
	}
}

func parseCounts(spec string) (map[string]int, error) {
	out := map[string]int{}
	for _, part := range strings.Split(spec, ",") {
		name, cnt, ok := strings.Cut(part, "=")
		if !ok {
			return nil, fmt.Errorf("bad count %q: expected scenario=count", part)
		}
		n, err := strconv.Atoi(cnt)
		if err != nil {
			return nil, err
		}
		if lab.ScenarioByName(name) == nil {
			return nil, fmt.Errorf("unknown scenario %q", name)
		}
		out[name] = n
	}
	return out, nil
}

// serve runs a persistent local cluster on fixed ports for sentinel-cli to talk to.
func serve(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	bin := fs.String("bin", "bin", "directory holding sentinel-node")
	certs := fs.String("certs", "certs", "certificate directory, created if missing")
	data := fs.String("data", "data", "data directory; kept between runs")
	nodes := fs.Int("nodes", 5, "cluster size")
	model := fs.String("model", "", "prediction model; empty runs plain reactive Raft")
	fs.Parse(args)
	if err := lab.EnsureCerts(*certs, *nodes); err != nil {
		return err
	}
	c, err := lab.StartCluster(lab.Options{
		Bin: *bin, CertDir: *certs, Dir: *data, Nodes: *nodes, Model: *model, PortBase: 7000, Log: os.Stderr,
	})
	if err != nil {
		return err
	}
	defer c.Stop()
	log.Printf("cluster up, clients on %s", strings.Join(c.Addrs(), ","))
	log.Printf("connect with: sentinel-cli -certs %s", *certs)
	<-ctx.Done()
	return nil
}

const defaultCollect = "degrade-delay=16,degrade-jitter=16,degrade-loss=16,degrade-mixed=16," +
	"hard-kill=8,healthy=10,blip=10,steady-slow=10,steady-slow-high=16,one-bad-link=10,follower-kill=8,two-follower-kill=8"

// collect builds the labelled dataset. Runs of different scenarios are interleaved so that
// any slow drift in the machine's state cannot line up with one class.
func collect(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("collect", flag.ExitOnError)
	var c common
	c.add(fs)
	counts := fs.String("counts", defaultCollect, "runs per scenario")
	out := fs.String("out", "ml/data", "output directory")
	seed := fs.Int64("seed", 20261104, "base random seed")
	fs.Parse(args)
	if err := c.prepare(); err != nil {
		return err
	}
	plan, err := parseCounts(*counts)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(*out, 0o755); err != nil {
		return err
	}
	runsPath, telPath := filepath.Join(*out, "runs.csv"), filepath.Join(*out, "telemetry.csv")
	runsNew, err := isEmpty(runsPath)
	if err != nil {
		return err
	}
	runsF, err := os.OpenFile(runsPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer runsF.Close()
	telF, err := os.OpenFile(telPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer telF.Close()
	runs, tel := csv.NewWriter(runsF), csv.NewWriter(telF)
	if runsNew {
		runs.Write([]string{"run_id", "scenario", "degrades", "kills_leader", "victim", "fault_start_ms", "kill_ms"})
		tel.Write(append([]string{"run_id", "unix_ms", "node", "term"}, predict.FeatureNames...))
	}

	var order []*lab.Scenario
	for remaining := true; remaining; {
		remaining = false
		for i := range lab.Scenarios {
			sc := &lab.Scenarios[i]
			if plan[sc.Name] > 0 {
				plan[sc.Name]--
				order = append(order, sc)
				remaining = true
			}
		}
	}
	for i, sc := range order {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		runID := fmt.Sprintf("%s-%04d-%d", sc.Name, i, *seed)
		start := time.Now()
		err := lab.Collect(ctx, c.options("collect"), sc, runID, *seed+int64(i), runs, tel)
		status := "ok"
		if err != nil {
			status = "FAILED: " + err.Error()
		}
		log.Printf("[%d/%d] %s %s (%v)", i+1, len(order), runID, status, time.Since(start).Round(time.Second))
	}
	return nil
}

func isEmpty(path string) (bool, error) {
	st, err := os.Stat(path)
	if os.IsNotExist(err) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	return st.Size() == 0, nil
}

const defaultBench = "degrade-delay,degrade-jitter,degrade-loss,degrade-mixed,hard-kill," +
	"healthy,blip,steady-slow,steady-slow-high,one-bad-link,follower-kill,two-follower-kill"

// bench measures reactive and proactive clusters on the same scenarios. Modes alternate
// trial by trial, so neither gets a systematically different machine state.
func bench(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("bench", flag.ExitOnError)
	var c common
	c.add(fs)
	model := fs.String("model", "ml/models/deployed.json", "model for the proactive mode")
	threshold := fs.Float64("threshold", 0, "override the model's threshold")
	trials := fs.Int("trials", 12, "trials per scenario per mode")
	scenarios := fs.String("scenarios", defaultBench, "comma-separated scenarios")
	out := fs.String("out", "results/bench.csv", "output CSV")
	timelines := fs.String("timelines", "results/timelines", "where to write per-trial write timelines")
	seed := fs.Int64("seed", 777, "base random seed")
	fs.Parse(args)
	if err := c.prepare(); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(*out), 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(*timelines, 0o755); err != nil {
		return err
	}
	f, err := os.Create(*out)
	if err != nil {
		return err
	}
	defer f.Close()
	w := csv.NewWriter(f)
	lab.WriteHeader(w)

	s := *seed
	for _, name := range strings.Split(*scenarios, ",") {
		sc := lab.ScenarioByName(name)
		if sc == nil {
			return fmt.Errorf("unknown scenario %q", name)
		}
		for n := 0; n < *trials; n++ {
			s++
			for _, mode := range []string{"reactive", "proactive"} {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				opts := c.options("bench")
				if mode == "proactive" {
					opts.Model = *model
					opts.Threshold = *threshold
				}
				// The same seed for both modes: each pair faces the same fault.
				t, err := lab.Bench(ctx, opts, sc, mode, n, s)
				if err != nil {
					log.Printf("%s/%s #%d FAILED: %v", name, mode, n, err)
					continue
				}
				w.Write(t.Row())
				w.Flush()
				log.Printf("%-18s %-9s #%-2d outage=%5dms post-kill=%5dms handed-over=%-5v lead=%5dms changes=%d",
					name, mode, n, t.OutageMs, t.PostKillGapMs, t.HandedOver, t.LeadMs, t.LeaderChanges)
				if n == 0 {
					if err := writeTimeline(filepath.Join(*timelines, name+"-"+mode+".csv"), t); err != nil {
						return err
					}
				}
			}
		}
	}
	return w.Error()
}

func writeTimeline(path string, t *lab.Trial) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	w := csv.NewWriter(f)
	w.Write([]string{"start_ms", "end_ms", "ok"})
	for _, s := range t.Timeline {
		w.Write([]string{strconv.FormatInt(s.StartMs, 10), strconv.FormatInt(s.EndMs, 10), strconv.FormatBool(s.OK)})
	}
	w.Flush()
	return w.Error()
}
