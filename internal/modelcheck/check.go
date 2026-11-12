package modelcheck

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// Violation is a property failing at a reachable state.
type Violation struct {
	Property string
	Detail   string
	// Trace is a shortest path from the initial state to the violation.
	Trace []TraceStep
}

// TraceStep is one step of a counterexample.
type TraceStep struct {
	Action string
	State  string
}

// Result is the outcome of a run.
type Result struct {
	Bounds Bounds
	// States and Transitions count distinct reachable states (up to node renaming) and
	// the transitions explored between them.
	States      int
	Transitions int
	// MaxDepth is the length of the longest shortest path from the start.
	MaxDepth  int
	Violation *Violation
	// Exhaustive is true when the whole reachable space was explored. It is false if the
	// run stopped at a violation or at the state limit, in which case no claim about the
	// whole space may be made.
	Exhaustive bool
	Elapsed    time.Duration
	// RoleEdges lists the role changes observed: the per-node automaton, as derived from
	// the model rather than drawn by hand.
	RoleEdges map[RoleEdge]int
	// Witness, if requested, is a path to a state satisfying the witness predicate.
	Witness []TraceStep
}

// RoleEdge is one transition of the per-node role automaton.
type RoleEdge struct {
	From, To uint8
	Event    EventKind
}

// Options controls a run.
type Options struct {
	// MaxStates aborts the search rather than exhausting memory. An aborted run makes no
	// claim of exhaustiveness.
	MaxStates int
	// Witness, if set, records a shortest path to the first state where it holds.
	Witness func(*State, Bounds) bool
	// NoSymmetry turns off symmetry reduction, as a cross-check that it is not hiding
	// any states.
	NoSymmetry bool
}

// Invariant is a property that must hold in every reachable state.
type Invariant struct {
	Name  string
	Check func(*State, Bounds) string // an empty string means it holds
}

// Invariants are the safety properties of the Raft paper (Figure 3), checked at every state.
var Invariants = []Invariant{
	{"Election Safety", checkElectionSafety},
	{"Log Matching", checkLogMatching},
	{"Leader Completeness", checkLeaderCompleteness},
	{"State Machine Safety", checkStateMachineSafety},
}

// checkElectionSafety: at most one leader per term. The history variable means this also
// covers two leaders of one term that never existed at the same instant.
func checkElectionSafety(s *State, b Bounds) string {
	if s.SplitTerm != 0 {
		return fmt.Sprintf("two nodes have both been leader in term %d", s.SplitTerm)
	}
	for i := 0; i < b.Nodes; i++ {
		for j := i + 1; j < b.Nodes; j++ {
			if s.Nodes[i].Role == Leader && s.Nodes[j].Role == Leader && s.Nodes[i].Term == s.Nodes[j].Term {
				return fmt.Sprintf("n%d and n%d are both leader in term %d", i+1, j+1, s.Nodes[i].Term)
			}
		}
	}
	return ""
}

// checkLogMatching: logs agreeing at an index and term agree on everything before it.
func checkLogMatching(s *State, b Bounds) string {
	if b.Log == 0 {
		return ""
	}
	for i := 0; i < b.Nodes; i++ {
		for j := i + 1; j < b.Nodes; j++ {
			a, c := &s.Nodes[i], &s.Nodes[j]
			hi := min(a.LogLen, c.LogLen)
			agreed := false
			for k := int(hi) - 1; k >= 0; k-- {
				if a.Log[k] == c.Log[k] {
					agreed = true
				} else if agreed {
					return fmt.Sprintf("n%d and n%d agree above index %d but differ at it", i+1, j+1, k+1)
				}
			}
		}
	}
	return ""
}

// checkLeaderCompleteness: a leader's log holds every entry committed in an earlier term.
func checkLeaderCompleteness(s *State, b Bounds) string {
	for i := 0; i < b.Nodes; i++ {
		n := &s.Nodes[i]
		if n.Role != Leader {
			continue
		}
		for k, c := range s.Committed {
			if c.EntryTerm == 0 || n.Term <= c.ByTerm {
				continue
			}
			if int(n.LogLen) <= k || n.Log[k] != c.EntryTerm {
				return fmt.Sprintf("n%d leads term %d but lacks index %d (term %d), committed in term %d",
					i+1, n.Term, k+1, c.EntryTerm, c.ByTerm)
			}
		}
	}
	return ""
}

// checkStateMachineSafety: no two nodes ever commit different entries at one index, so
// replicas apply the same commands in the same order.
func checkStateMachineSafety(s *State, b Bounds) string {
	if s.LostCommit {
		return "a node truncated a committed entry from its log"
	}
	for i := 0; i < b.Nodes; i++ {
		n := &s.Nodes[i]
		if n.Commit > n.LogLen {
			return fmt.Sprintf("n%d's commit index %d is past its log's end at %d", i+1, n.Commit, n.LogLen)
		}
		for k := 0; k < int(n.Commit); k++ {
			if c := s.Committed[k]; c.EntryTerm != n.Log[k] {
				return fmt.Sprintf("n%d committed term %d at index %d, but the committed entry there is term %d",
					i+1, n.Log[k], k+1, c.EntryTerm)
			}
		}
	}
	return ""
}

type visitedState struct {
	st     *State
	parent int32
	act    Action
}

// Check explores every reachable state breadth-first. Breadth-first order makes any
// counterexample a shortest one, which is what makes it readable.
func Check(b Bounds, opts Options) Result {
	if b.Nodes < 1 || b.Nodes > MaxNodes || b.Log > MaxLog || b.Terms >= 16 {
		panic(fmt.Sprintf("bounds outside what the model's arrays hold: %+v", b))
	}
	if opts.MaxStates == 0 {
		opts.MaxStates = 8_000_000 // roughly 2 GB; raise it deliberately, with the memory to back it
	}
	start := time.Now()
	res := Result{Bounds: b, RoleEdges: map[RoleEdge]int{}}

	var perms [][]int8
	if !opts.NoSymmetry {
		perms = permutations(b.Nodes)
	}
	buf := make([]byte, 0, 512)

	init := Initial(b)
	seen := map[string]struct{}{b.key(init, perms, buf): {}}
	all := []visitedState{{st: init, parent: -1}}
	depths := []int32{0}

	finish := func() Result {
		res.States = len(all)
		res.Elapsed = time.Since(start)
		return res
	}

	// Expanded states are dropped to save memory, so a trace is rebuilt by replaying its
	// actions from the start. Replay also confirms each step is a real transition.
	trace := func(i int32) []TraceStep {
		var acts []Action
		for ; all[i].parent >= 0; i = all[i].parent {
			acts = append(acts, all[i].act)
		}
		cur := Initial(b)
		out := []TraceStep{{"initial state", cur.Summary(b)}}
		for k := len(acts) - 1; k >= 0; k-- {
			var next *State
			for _, sc := range b.successors(cur) {
				if sc.act == acts[k] {
					next = sc.st
					break
				}
			}
			if next == nil {
				panic("counterexample does not replay: the checker is inconsistent")
			}
			cur = next
			out = append(out, TraceStep{acts[k].String(), cur.Summary(b)})
		}
		return out
	}

	checkAt := func(i int32) bool {
		for _, inv := range Invariants {
			if d := inv.Check(all[i].st, b); d != "" {
				res.Violation = &Violation{Property: inv.Name, Detail: d, Trace: trace(i)}
				return false
			}
		}
		if opts.Witness != nil && res.Witness == nil && opts.Witness(all[i].st, b) {
			res.Witness = trace(i)
		}
		return true
	}
	if !checkAt(0) {
		return finish()
	}

	for head := int32(0); int(head) < len(all); head++ {
		cur := all[head].st
		if int(depths[head]) > res.MaxDepth {
			res.MaxDepth = int(depths[head])
		}
		for _, sc := range b.successors(cur) {
			res.Transitions++
			if sc.changed {
				ev := sc.act.Event
				if ev == EvVoteGranted && sc.to == Leader {
					ev = EvMajority // the vote that completes a majority
				}
				res.RoleEdges[RoleEdge{sc.from, sc.to, ev}]++
			}
			k := b.key(sc.st, perms, buf)
			if _, dup := seen[k]; dup {
				continue
			}
			seen[k] = struct{}{}
			all = append(all, visitedState{st: sc.st, parent: head, act: sc.act})
			depths = append(depths, depths[head]+1)
			if !checkAt(int32(len(all) - 1)) {
				return finish()
			}
			if len(all) >= opts.MaxStates {
				return finish()
			}
		}
		all[head].st = nil
	}
	res.Exhaustive = true
	return finish()
}

// TwoLeadersAtOnce holds where two nodes consider themselves leader at the same instant,
// necessarily in different terms. Plain Raft allows this. A deposed leader that has not
// heard the news still thinks it leads, but it cannot commit anything.
func TwoLeadersAtOnce(s *State, b Bounds) bool {
	leaders := 0
	for i := 0; i < b.Nodes; i++ {
		if s.Nodes[i].Role == Leader {
			leaders++
		}
	}
	return leaders >= 2
}

// Statement renders the result as the precise claim it supports, in the form the design
// notes call for.
func (r Result) Statement() string {
	if r.Violation != nil {
		return fmt.Sprintf("VIOLATION of %s after %d steps: %s", r.Violation.Property, len(r.Violation.Trace)-1, r.Violation.Detail)
	}
	if !r.Exhaustive {
		return fmt.Sprintf("INCOMPLETE: stopped at the state limit after %d states; no claim can be made about this model", r.States)
	}
	names := make([]string, len(Invariants))
	for i, inv := range Invariants {
		names[i] = inv.Name
	}
	return fmt.Sprintf("Modelling the cluster as the product automaton of %d per-node automata and the network "+
		"(%s), explicit state-space exploration visited %d reachable states via %d transitions, and found zero "+
		"violations of: %s.", r.Bounds.Nodes, r.Bounds, r.States, r.Transitions, strings.Join(names, ", "))
}

// TraceString renders a counterexample or witness.
func TraceString(t []TraceStep) string {
	var b strings.Builder
	for i, s := range t {
		fmt.Fprintf(&b, "  %2d. %s\n      %s\n", i, s.Action, s.State)
	}
	return b.String()
}

// DOT renders the per-node role automaton in Graphviz format, from transitions that were
// actually observed in the exploration.
func (r Result) DOT() string {
	type pair struct{ from, to uint8 }
	labels := map[pair][]string{}
	for e := range r.RoleEdges {
		k := pair{e.From, e.To}
		labels[k] = append(labels[k], EventNames[e.Event])
	}
	keys := make([]pair, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].from != keys[j].from {
			return keys[i].from < keys[j].from
		}
		return keys[i].to < keys[j].to
	})
	var b strings.Builder
	b.WriteString("digraph raft_node {\n  rankdir=LR;\n  node [shape=circle];\n  start [shape=point];\n  start -> Follower;\n")
	for _, k := range keys {
		ls := labels[k]
		sort.Strings(ls)
		fmt.Fprintf(&b, "  %s -> %s [label=%q];\n", RoleName(k.from), RoleName(k.to), strings.Join(ls, "\n"))
	}
	b.WriteString("}\n")
	return b.String()
}

// TransitionTable renders the same automaton as a text table.
func (r Result) TransitionTable() string {
	edges := make([]RoleEdge, 0, len(r.RoleEdges))
	for e := range r.RoleEdges {
		edges = append(edges, e)
	}
	sort.Slice(edges, func(i, j int) bool {
		a, c := edges[i], edges[j]
		if a.From != c.From {
			return a.From < c.From
		}
		if a.To != c.To {
			return a.To < c.To
		}
		return EventNames[a.Event] < EventNames[c.Event]
	})
	var b strings.Builder
	fmt.Fprintf(&b, "  %-10s %-30s %s\n", "from", "event", "to")
	for _, e := range edges {
		fmt.Fprintf(&b, "  %-10s %-30s %s\n", RoleName(e.From), EventNames[e.Event], RoleName(e.To))
	}
	return b.String()
}
