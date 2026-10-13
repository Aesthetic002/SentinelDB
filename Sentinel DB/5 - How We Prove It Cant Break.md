---
tags: [sentinel-db, theory-of-computation, formal-verification, automata]
aliases: [SentinelDB Formal Proof, SentinelDB Safety Proof]
created: 2026-10-13
updated: 2026-10-13
---

# How We Prove It Can't Break

Back to [[1 - Overview|Overview]]. This builds on [[3 - How Leader Election Works|how leader election works]] — read that first if you haven't.

## The one bug that would be genuinely catastrophic

Of everything that could go wrong in this system, one failure is far worse than the rest: **two nodes both believing they're the leader at the same time**. If that happened, both could accept writes independently, the two copies of the data would silently diverge, and there would be no clean way to tell which version is correct — the database would effectively be corrupted without any obvious error message telling you so. Everything else (a node crashing, a slow network, a lost message) Raft is specifically designed to survive gracefully. This one specific scenario is the thing that must never happen.

## Why "we tested it a lot" isn't good enough for this one

Most student projects validate a system by running it, trying some scenarios, and checking nothing obviously broke. That's reasonable for most bugs, but it only tells you "it survived the specific situations we happened to try" — it says nothing about the situations nobody thought to test. For the one failure mode that would silently corrupt the whole database, we wanted something stronger: not "we didn't see it happen," but "we checked every situation it could possibly be in, and it never happens in any of them."

## Modelling a node as a finite automaton

Here's the part that connects directly to Theory of Computation coursework, not as a forced connection but as a genuinely natural fit: a Raft node's behaviour — Follower, Candidate, Leader, moving between these based on events like a timeout or winning a vote — is *literally* a finite state machine. Formally, it's a **Deterministic Finite Automaton (DFA)**: a fixed set of states (Follower / Candidate / Leader, each tagged with the current term number), a fixed input alphabet of events (timeout, vote-granted, majority-achieved, saw-a-higher-term, received-append-entries), and a transition rule saying exactly which state each event leads to from each starting state. This isn't a loose metaphor borrowed from the course — it's an actual DFA that can be drawn out, with circles for states and labelled arrows for transitions, exactly like any other automaton diagram.

## Combining all the nodes into one big automaton

A single node's automaton only describes one machine. What we actually care about is the behaviour of the *whole cluster* at once — because "two leaders at the same time" is a statement about all five nodes together, not about any one of them individually. The standard way to combine several automata into one that describes all of them together is called the **product construction**: you take the per-node automata and build one new automaton whose states are *tuples* — one state from each node at once. With 5 nodes each having 3 possible roles, a global state looks like (Follower, Leader, Follower, Candidate, Follower), and so on for every combination. This product automaton is a completely standard, well-established technique (it's the same construction used, for instance, to check whether two regular languages share any string in common) — we're just applying it to our own protocol instead of a textbook example.

## The actual check: trying every reachable combination

Once the product automaton exists, a small program explores it starting from the natural starting point (every node begins as a Follower, before any election has happened) and follows every possible sequence of valid events outward, visiting every global state that can actually be reached by some legal sequence of events. At every single state it visits, it checks one rule: *is more than one node currently a Leader in the same term?* If the program finishes exploring the entire reachable space and that rule was never broken, we can state the result precisely:

> "Modelling the cluster as the product automaton of N per-node DFAs, bounded to a small number of nodes and a bounded range of terms, explicit state-space exploration visited X reachable states via Y transitions and found zero states where two nodes were Leader in the same term."

That sentence is a genuinely different, stronger kind of claim than "we tested it and it seemed fine" — it's a concrete, falsifiable fact about every state the system can reach, not an impression from a sample of runs.

## Why the model is deliberately kept small — and why that's not a trick

The number of possible global states grows very fast as you add more nodes or allow more terms — roughly (states per node × number of terms) raised to the power of the number of nodes. That's why the check is run on a small, bounded version (a handful of nodes, a limited range of terms) rather than the full, unbounded real system. This isn't a shortcut being quietly hidden — it's a standard, well-understood trade-off in a real technique called model checking (the same basic idea underlies serious industrial tools like TLA+, which is what Amazon uses internally to check distributed systems designs). The honest, correct thing to say if asked about it directly is exactly this: the check is exhaustive and genuinely proves the property *for the bounded model*, and the model is kept small enough that the check actually finishes in reasonable time.

## A neat side note: why this is even possible at all

It's worth knowing, and worth saying out loud if it comes up, that this kind of exhaustive check is only possible *because* the model is a finite automaton with a bounded number of states. Checking whether an arbitrary, unrestricted computer program behaves correctly is, in general, mathematically impossible to do for every possible program — this is the famous Halting Problem, and it's a core result covered in Theory of Computation. Our safety check sidesteps that entirely, not by being clever, but because we deliberately chose to model the system as something simpler and more restricted than a general program — a finite automaton — for which checking "can it ever reach a bad state" is a completely solvable, decidable question. That contrast (decidable for our bounded model vs. undecidable for arbitrary programs) is a good, honest thing to mention if a judge wants to probe how deep the understanding goes.

## The other Theory of Computation piece: the SQL language's grammar

Separately from the safety proof, the small SQL language described in [[4 - How the Database Works|How the Database Works]] is defined formally as a **Context-Free Grammar (CFG)** — a precise set of rules saying exactly what counts as a valid query, written the same way a programming language's syntax is defined. A small parser (built by hand, following the standard "recursive-descent" method) reads an incoming query and builds an explicit **parse tree** from it before anything is executed. Where the most natural version of the grammar would have been ambiguous (more than one valid way to parse the same query — a classic problem with things like chained comparisons), the grammar was deliberately restructured to remove that ambiguity, which is a genuine, demonstrable example of a core Theory of Computation skill (grammar simplification), not just a necessary implementation detail glossed over. As a useful side effect, parsing every query through a proper grammar before running it also closes off classic SQL-injection attacks — see [[8 - Security - What's Protected and What Isn't|Security]] for that angle.
