---
tags: [sentinel-db, theory-of-computation, formal-verification, automata]
aliases: [SentinelDB Formal Proof, SentinelDB Safety Proof]
created: 2026-10-13
updated: 2026-11-04
---

# How We Prove It Can't Break

Back to [[1 - Overview|Overview]]. This builds on [[3 - How Leader Election Works|how leader election works]] — read that first if you haven't.

## The one bug that would be genuinely catastrophic

Of everything that could go wrong in this system, one failure is far worse than the rest: **two nodes both being the leader in the same term**. If that happened, both could accept writes independently, the two copies of the data would silently diverge, and there would be no clean way to tell which version is correct — the database would effectively be corrupted without any obvious error message telling you so. Everything else (a node crashing, a slow network, a lost message) Raft is specifically designed to survive gracefully. This one specific scenario is the thing that must never happen.

The wording matters, so be precise about it. Two nodes can both *believe* they're leader at the same instant, in *different* terms: an old leader that hasn't heard the news yet. That really can happen in correct Raft, and it happens during an ordinary handover. It's harmless, because a majority has already moved to the higher term, so the old leader can't commit anything, and reads check with a majority first. The checker shows this state is reachable (`sentinel-check -two-leaders`), which is a good thing to show before a judge asks.

## Why "we tested it a lot" isn't good enough for this one

Most student projects validate a system by running it, trying some scenarios, and checking nothing obviously broke. That's reasonable for most bugs, but it only tells you "it survived the specific situations we happened to try" — it says nothing about the situations nobody thought to test. For the one failure mode that would silently corrupt the whole database, we wanted something stronger: not "we didn't see it happen," but "we checked every situation a bounded model of it could possibly be in, and it never happens in any of them."

## Modelling a node as a finite automaton

Here's the part that connects directly to Theory of Computation coursework, not as a forced connection but as a genuinely natural fit: a Raft node's behaviour — Follower, Candidate, Leader, moving between these based on events like a timeout or winning a vote — is *literally* a finite state machine. Formally, it's a **Deterministic Finite Automaton (DFA)**: a fixed set of states, a fixed input alphabet of events (timeout, vote-request, vote-granted, majority-achieved, saw-higher-term, received-append-entries, append-response, client-request, restart, lost-quorum, timeout-now), and a transition rule saying exactly which state each event leads to from each starting state. This isn't a loose metaphor borrowed from the course — it's an actual automaton, and the transition table in `results/verification.md` is generated from the transitions observed during the search, not drawn by hand.

One thing that's easy to get wrong: the role alone can't be the whole state. Whether a second leader can arise depends on who voted for whom, in which term, and whose log is newer. So each node's state is its role, plus its term, its vote, the votes it has collected, its log and its commit position. All of those are bounded, so the automaton is still finite, but it's much bigger than three circles.

## Combining all the nodes into one big automaton

A single node's automaton only describes one machine. What we actually care about is the behaviour of the *whole cluster* at once — because "two leaders in one term" is a statement about all the nodes together, not about any one of them individually. The standard way to combine several automata into one that describes all of them together is called the **product construction**: you take the per-node automata and build one new automaton whose states are *tuples* — one state from each node at once. This product automaton is a completely standard, well-established technique (it's the same construction used, for instance, to check whether two regular languages share any string in common) — we're just applying it to our own protocol instead of a textbook example. The network is one more component of the tuple, because a message that has been sent but not yet delivered affects what can happen next. Messages can be lost, delayed, duplicated and reordered, simply because any delivery may happen or never happen.

## The actual check: trying every reachable combination

Once the product automaton exists, a small program (`sentinel-check`) explores it starting from the natural starting point (every node begins as a Follower, before any election has happened) and follows every possible sequence of valid events outward, visiting every global state that can actually be reached by some legal sequence of events. At every single state it visits, it checks four rules from the Raft paper:

- **Election Safety:** never more than one leader in a term, including two that don't overlap in time.
- **Log Matching:** two logs that agree at some position agree on everything before it.
- **Leader Completeness:** a leader's log contains everything committed in earlier terms.
- **State Machine Safety:** no two nodes ever commit different entries at the same position.

Crashes and restarts are included, keeping only what a real node saves to disk. And crucially, the failure predictor isn't modelled as accurate, or as anything at all: **any leader can hand over to any node at any moment, however wrong that is.** So the result holds regardless of how good the classifier is.

The results, every one of them exhaustive:

| Model | Reachable states | Transitions |
|---|---:|---:|
| 3 nodes, 4 terms, elections only | 34,014 | 358,974 |
| 3 nodes, 3 terms, log of 1 | 135,657 | 1,205,838 |
| 4 nodes, 4 terms, elections only | 1,082,585 | 16,446,310 |
| 3 nodes, 4 terms, log of 1 | 1,578,021 | 16,179,177 |
| 5 nodes, 3 terms, elections only | 2,123,296 | 35,961,757 |
| 3 nodes, 3 terms, log of 2 | 3,482,466 | 34,755,431 |

(All of these include transfers and restarts. Two more runs without them are in `results/verification.md`.) So we can state the result precisely, here for the largest 5-node model:

> "Modelling the cluster as the product automaton of 5 per-node automata and the network, bounded to 3 terms, explicit state-space exploration visited 2,123,296 reachable states via 35,961,757 transitions and found zero violations of Election Safety, Log Matching, Leader Completeness or State Machine Safety."

That sentence is a genuinely different, stronger kind of claim than "we tested it and it seemed fine" — it's a concrete, falsifiable fact about every state the model can reach, not an impression from a sample of runs.

## How we know the checker isn't just blind

A clean result would mean nothing if the checker couldn't see anything. So we broke the protocol on purpose, in 8 known ways, such as voting twice, forgetting a vote after a restart, or letting a follower ignore how up to date a candidate's log is. The checker found all 8, printing the shortest sequence of events leading to the violation, while the same bounds are clean without the bug. It even rediscovered Figure 8 of the Raft paper on its own, as an 18-step counterexample, without being told what to look for. That's a very satisfying thing to run live.

## Why the model is deliberately kept small — and why that's not a trick

The number of possible global states grows very fast as you add more nodes or allow more terms — roughly (states per node × number of terms) raised to the power of the number of nodes. That's why the check is run on a small, bounded version (a handful of nodes, a limited range of terms) rather than the full, unbounded real system. Two techniques keep it practical without hiding anything: states that differ only by swapping node names count as one state (tests confirm this merges only what it should), and everything a leader could have sent is worked out from its log rather than tracked as individual messages. This isn't a shortcut being quietly hidden — it's a standard, well-understood trade-off in a real technique called model checking (the same basic idea underlies serious industrial tools like TLA+, which is what Amazon uses internally to check distributed systems designs). The honest, correct thing to say if asked about it directly is exactly this: the check is exhaustive and genuinely proves the property *for the bounded model*, and the model is kept small enough that the check actually finishes in reasonable time.

## What this does and doesn't show

- **It checks the design, not the Go code.** The model is a separate program, so a bug in the real code that the model doesn't share wouldn't show up here. To cover that gap, the real Raft code also runs through 320 randomized simulated runs checking the same properties, under message loss, partitions and crashes. That's sampling rather than proof, so to check that those tests can actually fail, we injected 14 bugs into the real code, and all 14 were caught.
- **It's bounded.** The result holds for the sizes in the table and says nothing directly about a bigger cluster.
- **Membership changes aren't in the model.** They're covered only by simulation and specific tests.
- **It assumes crashes, not lies.** See [[8 - Security - What's Protected and What Isn't|Security]].

## A neat side note: why this is even possible at all

It's worth knowing, and worth saying out loud if it comes up, that this kind of exhaustive check is only possible *because* the model is a finite automaton with a bounded number of states. Checking whether an arbitrary, unrestricted computer program behaves correctly is, in general, mathematically impossible to do for every possible program — this is the famous Halting Problem, and it's a core result covered in Theory of Computation. Our safety check sidesteps that entirely, not by being clever, but because we deliberately chose to model the system as something simpler and more restricted than a general program — a finite automaton — for which checking "can it ever reach a bad state" is a completely solvable, decidable question. That contrast (decidable for our bounded model vs. undecidable for arbitrary programs) is a good, honest thing to mention if a judge wants to probe how deep the understanding goes.

## The other Theory of Computation piece: the SQL language's grammar

Separately from the safety proof, the small SQL language described in [[4 - How the Database Works|How the Database Works]] is defined formally as a **Context-Free Grammar (CFG)** — a precise set of rules saying exactly what counts as a valid query, written the same way a programming language's syntax is defined (the whole thing is in `docs/grammar.md`). A small parser (built by hand, following the standard "recursive-descent" method) reads an incoming query and builds an explicit **parse tree** from it before anything is executed. The most natural version of the grammar would have been ambiguous (`a = 1 OR b = 2 AND c = 3` would have two valid parse trees with different meanings), so it was deliberately restructured with one rule per precedence level, which is a genuine, demonstrable example of a core Theory of Computation skill. Left recursion was removed too, because recursive descent can't terminate with it. A chained comparison like `a < b < c` has no valid parse at all, so it's rejected with an error telling the user to write `a < b AND b < c`. As a useful side effect, parsing every query through a proper grammar before running it also closes off classic SQL-injection attacks — see [[8 - Security - What's Protected and What Isn't|Security]] for that angle.
