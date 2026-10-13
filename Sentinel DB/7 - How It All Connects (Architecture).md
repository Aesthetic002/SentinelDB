---
tags: [sentinel-db, architecture, system-design]
aliases: [SentinelDB Architecture, SentinelDB End to End]
created: 2026-10-13
updated: 2026-10-13
---

# How It All Connects (Architecture)

Back to [[1 - Overview|Overview]]. This note assumes you've read the four component notes — [[3 - How Leader Election Works|leader election]], [[4 - How the Database Works|the database layer]], [[5 - How We Prove It Cant Break|the safety proof]], and [[6 - How Failure Prediction Works|failure prediction]] — and ties them together into one picture.

## The four layers, as a stack

```
            Client sends a SQL query
                      |
                      v
   ┌───────────────────────────────────┐
   │   Query layer (DBMS)               │   parses SQL → parse tree → relational algebra
   │   schema, SQL subset, transactions │
   └───────────────────────────────────┘
                      |
                      v
   ┌───────────────────────────────────┐
   │   Consensus layer (Distributed     │   leader election, log replication,
   │   Systems — Raft)                  │   majority-commit rule
   └───────────────────────────────────┘
                      ^
                      |  watches heartbeats, triggers early handover
   ┌───────────────────────────────────┐
   │   Prediction layer (AI/ML)         │
   │   classifier on heartbeat signals  │
   └───────────────────────────────────┘

   ┌───────────────────────────────────┐
   │   Verification layer (TOC)         │   runs separately, offline —
   │   automaton model + CFG grammar    │   checks the design, not live traffic
   └───────────────────────────────────┘
```

The query and consensus layers are in the live request path — every single query passes through both. The prediction layer runs continuously in the background, watching the consensus layer's heartbeats and occasionally reaching in to trigger a leadership transfer. The verification layer is different in kind from the other three: it doesn't run on live traffic at all — it's a static check, run ahead of time (or alongside testing), against a model of the protocol's design.

## Walkthrough 1: a normal query, nothing going wrong

1. A client sends a SQL statement (say, `SELECT * FROM orders WHERE customer_id = 42`) to any node.
2. If that node isn't the current leader, it forwards the request on.
3. The leader parses the query against the grammar described in [[5 - How We Prove It Cant Break|the safety-proof note]], producing a parse tree, then translates it into relational-algebra operations.
4. For this read-only query, the leader can answer directly from its own current data (or, if configured for speed over strict freshness, a follower could answer from its own copy).
5. The result goes back to the client. For a write instead of a read, step 4 would instead mean: propose the write as a new log entry, replicate it to the other nodes, wait for a majority to confirm, then respond — as described in [[3 - How Leader Election Works|How Leader Election Works]] and [[4 - How the Database Works|How the Database Works]].

## Walkthrough 2: a node fails, the reactive way

1. The current leader's process is killed (or it genuinely crashes).
2. The other nodes simply stop receiving heartbeats from it.
3. Each follower's randomized election timer is running independently; the first one to expire becomes a candidate and requests votes.
4. Once a majority vote for it, it becomes the new leader and the cluster resumes accepting writes.
5. The gap between step 1 and step 4 is real, measurable downtime — however long the timeout was set to, plus the time for the election itself.

## Walkthrough 3: a node is about to fail, the SentinelDB way

1. The current leader starts showing early warning signs — its response times to heartbeats start growing or becoming irregular.
2. The prediction layer, watching this telemetry continuously, feeds the recent window of signals into the deployed classifier (from [[6 - How Failure Prediction Works|How Failure Prediction Works]]).
3. The classifier returns a high-confidence "about to fail" prediction for this node.
4. Instead of waiting for the node to actually go silent, the system immediately triggers Raft's leadership-transfer mechanism, handing control to a healthy follower voluntarily.
5. The new leader takes over almost immediately — there's no multi-second gap, because the handover happened *before* the old leader was actually unreachable, not after.
6. Separately and offline, the fact that this whole handover process — including the proactive trigger — still can never result in two simultaneous leaders is exactly what the automaton-based check in [[5 - How We Prove It Cant Break|How We Prove It Can't Break]] was built to verify in the first place.

## Why it's worth presenting the system this way

Walkthroughs 2 and 3, told back to back with the same latency graph, are the single clearest way to make the project's value obvious to someone seeing it for the first time — the difference between them is the entire pitch in visual form, no explanation needed before they can see it happening.
