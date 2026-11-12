package modelcheck

import (
	"strings"
	"testing"
)

func small() Bounds {
	return Bounds{Nodes: 3, Terms: 3, Log: 1, Transfers: true, Restarts: true}
}

func TestCorrectProtocolIsClean(t *testing.T) {
	r := Check(small(), Options{})
	if r.Violation != nil {
		t.Fatalf("%s\n%s", r.Statement(), TraceString(r.Violation.Trace))
	}
	if !r.Exhaustive {
		t.Fatalf("search not exhaustive: %s", r.Statement())
	}
	if r.States < 1000 {
		t.Fatalf("only %d states, which is too few to mean much", r.States)
	}
	t.Log(r.Statement())
}

// Every known bug must be found. Without this, a clean result could just mean the checker
// cannot see anything.
func TestCheckerFindsEveryInjectedBug(t *testing.T) {
	cases := []struct {
		name     string
		bounds   Bounds
		property string
	}{
		{"ignore-vote", Bounds{Nodes: 3, Terms: 2, Bug: BugIgnoreVote}, "Election Safety"},
		{"forget-vote", Bounds{Nodes: 3, Terms: 2, Restarts: true, Bug: BugForgetVote}, "Election Safety"},
		{"small-quorum", Bounds{Nodes: 3, Terms: 2, Bug: BugSmallQuorum}, "Election Safety"},
		{"ignore-log-freshness", Bounds{Nodes: 3, Terms: 3, Log: 1, Bug: BugIgnoreLogFreshness}, "Leader Completeness"},
		{"commit-old-term", Bounds{Nodes: 3, Terms: 4, Log: 1, Bug: BugCommitOldTerm}, "Leader Completeness"},
		{"transfer-no-bump", Bounds{Nodes: 3, Terms: 2, Transfers: true, Bug: BugTransferNoTermBump}, "Election Safety"},
		{"stale-append", Bounds{Nodes: 3, Terms: 3, Log: 1, Bug: BugStaleAppend}, ""},
		{"commit-unverified", Bounds{Nodes: 3, Terms: 3, Log: 2, Bug: BugCommitUnverified}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := Check(c.bounds, Options{})
			if r.Violation == nil {
				t.Fatalf("bug not found. %s", r.Statement())
			}
			if c.property != "" && r.Violation.Property != c.property {
				t.Errorf("expected a %s violation, got %s: %s", c.property, r.Violation.Property, r.Violation.Detail)
			}
			if len(r.Violation.Trace) < 2 {
				t.Errorf("trace too short to be real: %v", r.Violation.Trace)
			}
			t.Logf("%s in %d steps: %s", r.Violation.Property, len(r.Violation.Trace)-1, r.Violation.Detail)
		})
	}
}

// Without these same bounds being clean, the bug tests above would show nothing: the
// violations would come from the bounds rather than from the injected bug.
func TestBugBoundsAreCleanWithoutTheBug(t *testing.T) {
	for _, b := range []Bounds{
		{Nodes: 3, Terms: 2},
		{Nodes: 3, Terms: 2, Restarts: true},
		{Nodes: 3, Terms: 2, Transfers: true},
		{Nodes: 3, Terms: 3, Log: 1},
		{Nodes: 3, Terms: 4, Log: 1},
		{Nodes: 3, Terms: 3, Log: 2},
	} {
		if testing.Short() && b.Terms+b.Log > 4 {
			continue // the largest take about a minute; sentinel-check -suite covers them too
		}
		r := Check(b, Options{MaxStates: 3_000_000})
		if r.Violation != nil {
			t.Errorf("%v: %s\n%s", b, r.Statement(), TraceString(r.Violation.Trace))
		} else if !r.Exhaustive {
			t.Logf("%v: stopped at the state limit (%d)", b, r.States)
		} else {
			t.Logf("%v: clean, %d states", b, r.States)
		}
	}
}

// Symmetry reduction must only merge states, never hide a violation, or change what
// bugs are found.
func TestSymmetryReductionPreservesResults(t *testing.T) {
	b := Bounds{Nodes: 3, Terms: 2, Transfers: true, Restarts: true}
	with := Check(b, Options{})
	without := Check(b, Options{NoSymmetry: true})
	if with.Violation != nil || without.Violation != nil {
		t.Fatalf("unexpected violation")
	}
	if with.States >= without.States {
		t.Errorf("reduction saved nothing: %d vs %d", with.States, without.States)
	}
	// Every class has at most N! members, so the full count is bounded by that factor.
	if without.States > with.States*6 {
		t.Errorf("full space %d exceeds 3! times the reduced one (%d): the reduction is merging too much",
			without.States, with.States)
	}
	bug := Bounds{Nodes: 3, Terms: 2, Bug: BugIgnoreVote}
	a, c := Check(bug, Options{}), Check(bug, Options{NoSymmetry: true})
	if a.Violation == nil || c.Violation == nil || len(a.Violation.Trace) != len(c.Violation.Trace) {
		t.Errorf("symmetry changed the shortest counterexample: %v vs %v", a.Violation, c.Violation)
	}
	t.Logf("symmetry: %d states reduced to %d", without.States, with.States)
}

// Only trying renamings in signature order must produce the same partition of states as
// trying all of them, or the speedup would be silently merging or splitting states.
func TestSignatureFilterMatchesAllPermutations(t *testing.T) {
	for _, b := range []Bounds{
		{Nodes: 3, Terms: 3, Log: 1, Transfers: true, Restarts: true},
		{Nodes: 4, Terms: 2, Transfers: true, Restarts: true},
		{Nodes: 5, Terms: 2, Transfers: true},
	} {
		fast := Check(b, Options{})
		full := b
		full.allPerms = true
		slow := Check(full, Options{})
		if fast.States != slow.States || fast.Transitions != slow.Transitions {
			t.Errorf("%v: filtered %d states, %d transitions; all permutations %d states, %d transitions",
				b, fast.States, fast.Transitions, slow.States, slow.Transitions)
		}
		t.Logf("%v: %d states either way", b, fast.States)
	}
	// The count recorded before this optimization existed.
	if r := Check(Bounds{Nodes: 3, Terms: 4, Transfers: true, Restarts: true}, Options{}); r.States != 34014 {
		t.Errorf("3-node elections model has %d states, recorded before the speedup as 34014", r.States)
	}
}

// The design notes' "never two leaders" needs to be precise. Two leaders in the SAME term
// is impossible (Election Safety). Two nodes both BELIEVING they lead, in different terms,
// is reachable in correct Raft, and is exactly what happens during a handover. This test
// keeps that distinction honest.
func TestTwoLeadersInDifferentTermsIsReachableAndHarmless(t *testing.T) {
	r := Check(Bounds{Nodes: 3, Terms: 2, Transfers: true}, Options{Witness: TwoLeadersAtOnce})
	if r.Violation != nil {
		t.Fatal(r.Statement())
	}
	if r.Witness == nil {
		t.Fatal("expected to reach a state where two nodes each believe they lead")
	}
	last := r.Witness[len(r.Witness)-1].State
	if strings.Count(last, ":L") != 2 {
		t.Fatalf("witness does not show two leaders: %s", last)
	}
	t.Logf("witness:\n%s", TraceString(r.Witness))
}

func TestStateLimitIsReportedAsIncomplete(t *testing.T) {
	r := Check(small(), Options{MaxStates: 50})
	if r.Exhaustive {
		t.Fatal("a run that hit its limit must not claim to be exhaustive")
	}
	if !strings.Contains(r.Statement(), "INCOMPLETE") {
		t.Fatalf("statement should say so: %s", r.Statement())
	}
}

func TestCounterexampleIsRealExecution(t *testing.T) {
	r := Check(Bounds{Nodes: 3, Terms: 4, Log: 1, Bug: BugCommitOldTerm}, Options{})
	if r.Violation == nil {
		t.Fatal("no violation")
	}
	if !strings.Contains(r.Violation.Trace[0].Action, "initial") {
		t.Fatalf("trace must start at the initial state: %v", r.Violation.Trace[0])
	}
	t.Logf("Figure 8, found by the checker:\n%s", TraceString(r.Violation.Trace))
}

func TestRoleAutomatonMatchesTheProtocol(t *testing.T) {
	r := Check(Bounds{Nodes: 3, Terms: 3, Log: 1, Transfers: true, Restarts: true}, Options{})
	want := []RoleEdge{
		{Follower, Candidate, EvTimeout},
		{Candidate, Candidate, EvTimeout},
		{Candidate, Leader, EvMajority},
		{Candidate, Follower, EvHigherTerm},
		{Candidate, Follower, EvAppendEntries},
		{Leader, Follower, EvHigherTerm},
		{Leader, Follower, EvStepDown},
		{Leader, Follower, EvRestart},
		{Follower, Candidate, EvTimeoutNow},
	}
	for _, e := range want {
		if e.From == e.To {
			continue // a self-loop is not a change of role
		}
		if r.RoleEdges[e] == 0 {
			t.Errorf("transition never observed: %s --%s--> %s", RoleName(e.From), EventNames[e.Event], RoleName(e.To))
		}
	}
	// A follower cannot become leader without being a candidate first.
	for e := range r.RoleEdges {
		if e.From == Follower && e.To == Leader {
			t.Errorf("follower jumped straight to leader via %s", EventNames[e.Event])
		}
	}
	if !strings.Contains(r.DOT(), "Candidate -> Leader") {
		t.Errorf("diagram missing the election edge:\n%s", r.DOT())
	}
}
