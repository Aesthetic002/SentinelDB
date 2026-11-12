// sentinel-check runs the offline verification layer: exhaustive exploration of the
// protocol's product automaton, and the conflict-serializability test on schedules.
package main

import (
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	mc "sentineldb/internal/modelcheck"
	"sentineldb/internal/txn"
)

func main() {
	var (
		suite     = flag.Bool("suite", false, "run the standard verification suite and print a report")
		nodes     = flag.Int("nodes", 3, "nodes in the model")
		terms     = flag.Int("terms", 3, "highest term")
		logLen    = flag.Int("log", 1, "highest log length (0 models elections only)")
		transfers = flag.Bool("transfers", true, "include leadership transfer, fired arbitrarily")
		restarts  = flag.Bool("restarts", true, "include crash-restart")
		bugName   = flag.String("bug", "", "inject a known bug: "+strings.Join(bugNames(), ", "))
		maxStates = flag.Int("max-states", 8_000_000, "stop rather than exhaust memory (about 250 bytes per state)")
		dot       = flag.Bool("dot", false, "print the per-node automaton in Graphviz format")
		table     = flag.Bool("table", false, "print the per-node automaton as a transition table")
		witness   = flag.Bool("two-leaders", false, "show how two nodes can both believe they lead (in different terms)")
		schedule  = flag.String("schedule", "", `test a schedule for conflict-serializability, e.g. "r1(x) w2(x) w1(x)"`)
	)
	flag.Parse()

	switch {
	case *schedule != "":
		os.Exit(runSchedule(*schedule))
	case *suite:
		os.Exit(runSuite(*maxStates))
	}

	b := mc.Bounds{Nodes: *nodes, Terms: *terms, Log: *logLen, Transfers: *transfers, Restarts: *restarts}
	if *bugName != "" {
		bug, ok := mc.Bugs[*bugName]
		if !ok {
			fmt.Fprintf(os.Stderr, "unknown bug %q; choose from %s\n", *bugName, strings.Join(bugNames(), ", "))
			os.Exit(2)
		}
		b.Bug = bug
	}
	opts := mc.Options{MaxStates: *maxStates}
	if *witness {
		opts.Witness = mc.TwoLeadersAtOnce
	}
	r := mc.Check(b, opts)
	fmt.Println(r.Statement())
	fmt.Printf("(%d states, %d transitions, depth %d, %v)\n", r.States, r.Transitions, r.MaxDepth, r.Elapsed.Round(time.Millisecond))
	if r.Violation != nil {
		fmt.Printf("\nShortest path to the violation:\n%s", mc.TraceString(r.Violation.Trace))
	}
	if r.Witness != nil {
		fmt.Printf("\nTwo nodes both believe they lead, in different terms. Only the higher term can commit:\n%s", mc.TraceString(r.Witness))
	}
	if *table {
		fmt.Printf("\nPer-node automaton, as observed during exploration:\n%s", r.TransitionTable())
	}
	if *dot {
		fmt.Printf("\n%s", r.DOT())
	}
	if r.Violation != nil || !r.Exhaustive {
		os.Exit(1)
	}
}

func bugNames() []string {
	out := make([]string, 0, len(mc.Bugs))
	for k := range mc.Bugs {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// runSuite produces the report behind the project's verification claims.
func runSuite(maxStates int) int {
	clean := []mc.Bounds{
		// Smallest first, so a run that hits its state limit has already reported the rest.
		{Nodes: 3, Terms: 4, Log: 0, Transfers: true, Restarts: true},
		{Nodes: 3, Terms: 3, Log: 1, Transfers: true, Restarts: true},
		{Nodes: 3, Terms: 4, Log: 1, Transfers: false, Restarts: false},
		{Nodes: 3, Terms: 3, Log: 2, Transfers: false, Restarts: false},
		{Nodes: 4, Terms: 4, Log: 0, Transfers: true, Restarts: true},
		{Nodes: 3, Terms: 4, Log: 1, Transfers: true, Restarts: true},
		{Nodes: 5, Terms: 3, Log: 0, Transfers: true, Restarts: true},
		{Nodes: 3, Terms: 3, Log: 2, Transfers: true, Restarts: true},
	}
	bad := 0
	fmt.Println("## The correct protocol")
	fmt.Println()
	fmt.Println("| Model | Reachable states | Transitions | Depth | Time | Result |")
	fmt.Println("|---|---:|---:|---:|---:|---|")
	var statements []string
	for _, b := range clean {
		r := mc.Check(b, mc.Options{MaxStates: maxStates})
		verdict := "no violations (exhaustive)"
		switch {
		case r.Violation != nil:
			verdict = "**VIOLATION: " + r.Violation.Property + "**"
			bad++
		case !r.Exhaustive:
			verdict = "stopped at the state limit: no claim"
		default:
			statements = append(statements, r.Statement())
		}
		fmt.Printf("| %s | %d | %d | %d | %v | %s |\n", b, r.States, r.Transitions, r.MaxDepth, r.Elapsed.Round(10*time.Millisecond), verdict)
	}
	fmt.Println()
	fmt.Println("### Claims these runs support")
	fmt.Println()
	for _, s := range statements {
		fmt.Printf("> %s\n\n", s)
	}

	fmt.Println("## Each known bug, injected on purpose")
	fmt.Println()
	fmt.Println("A clean result only means something if the checker can fail. Each row breaks one rule of the protocol.")
	fmt.Println()
	fmt.Println("| Injected bug | Bounds | Found | Steps | Property violated |")
	fmt.Println("|---|---|---|---:|---|")
	bugs := []struct {
		name string
		b    mc.Bounds
	}{
		{"ignore-vote", mc.Bounds{Nodes: 3, Terms: 2}},
		{"forget-vote", mc.Bounds{Nodes: 3, Terms: 2, Restarts: true}},
		{"small-quorum", mc.Bounds{Nodes: 3, Terms: 2}},
		{"transfer-no-bump", mc.Bounds{Nodes: 3, Terms: 2, Transfers: true}},
		{"ignore-log-freshness", mc.Bounds{Nodes: 3, Terms: 3, Log: 1}},
		{"stale-append", mc.Bounds{Nodes: 3, Terms: 3, Log: 1}},
		{"commit-unverified", mc.Bounds{Nodes: 3, Terms: 3, Log: 2}},
		{"commit-old-term", mc.Bounds{Nodes: 3, Terms: 4, Log: 1}},
	}
	for _, c := range bugs {
		b := c.b
		b.Bug = mc.Bugs[c.name]
		r := mc.Check(b, mc.Options{MaxStates: maxStates})
		if r.Violation == nil {
			fmt.Printf("| %s | %s | **NOT FOUND** | | |\n", c.name, c.b)
			bad++
			continue
		}
		fmt.Printf("| %s | %s | yes | %d | %s |\n", c.name, c.b, len(r.Violation.Trace)-1, r.Violation.Property)
	}

	fmt.Println()
	fmt.Println("## Two leaders at once, in different terms")
	fmt.Println()
	r := mc.Check(mc.Bounds{Nodes: 3, Terms: 2, Transfers: true}, mc.Options{Witness: mc.TwoLeadersAtOnce})
	if r.Witness == nil {
		fmt.Println("Not reached.")
		bad++
	} else {
		fmt.Println("Reachable in correct Raft: a deposed leader that has not yet heard the news still believes it leads. " +
			"It cannot commit anything, because a majority has moved to the higher term. The trace:")
		fmt.Println()
		fmt.Println("```")
		fmt.Print(mc.TraceString(r.Witness))
		fmt.Println("```")
	}

	fmt.Println()
	fmt.Println("## The per-node automaton")
	fmt.Println()
	fmt.Println("Derived from the transitions observed during exploration, not drawn by hand.")
	fmt.Println()
	auto := mc.Check(mc.Bounds{Nodes: 3, Terms: 3, Log: 1, Transfers: true, Restarts: true}, mc.Options{MaxStates: maxStates})
	fmt.Println("```")
	fmt.Print(auto.TransitionTable())
	fmt.Println("```")
	if bad > 0 {
		fmt.Fprintf(os.Stderr, "%d problem(s) in the verification suite\n", bad)
		return 1
	}
	return 0
}

func runSchedule(s string) int {
	ops, err := txn.ParseSchedule(s)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	res := txn.CheckConflictSerializable(ops)
	keys := make([][2]uint64, 0, len(res.Edges))
	for e := range res.Edges {
		keys = append(keys, e)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i][0] != keys[j][0] {
			return keys[i][0] < keys[j][0]
		}
		return keys[i][1] < keys[j][1]
	})
	for _, e := range keys {
		fmt.Printf("T%d -> T%d   because %s\n", e[0], e[1], res.Edges[e])
	}
	if res.Serializable {
		fmt.Printf("conflict-serializable: equivalent to serial order %v\n", res.Order)
		return 0
	}
	fmt.Printf("NOT conflict-serializable: the precedence graph has cycle %v\n", res.Cycle)
	return 1
}
