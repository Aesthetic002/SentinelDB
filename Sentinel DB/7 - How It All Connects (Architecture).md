---
tags: [sentinel-db, architecture, system-design]
aliases: [SentinelDB Architecture, SentinelDB End to End]
created: 2026-10-13
updated: 2026-11-04
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

The query and consensus layers are in the live request path — every single query passes through both. The prediction layer runs continuously in the background on the current leader, watching the consensus layer's heartbeats and occasionally reaching in to trigger a leadership transfer. The verification layer is different in kind from the other three: it doesn't run on live traffic at all — it's a static check, run ahead of time (or alongside testing), against a model of the protocol's design.

## Walkthrough 1: a normal query, nothing going wrong

1. A client sends a SQL statement (say, `SELECT * FROM orders WHERE customer_id = 42`) to any node, over TLS, having presented a token.
2. If that node isn't the current leader, it forwards the request on.
3. The leader parses the query against the grammar described in [[5 - How We Prove It Cant Break|the safety-proof note]], producing a parse tree, then translates it into relational-algebra operations.
4. For this read-only query, the leader takes the locks it needs, reads its data, and confirms with a majority that it's still the leader before answering. (Or, if the client explicitly asks for speed over strict freshness, any node can answer from its own copy, and the result is marked as possibly stale.)
5. The result goes back to the client. For a write instead of a read, step 4 would instead mean: propose the write as a new log entry, replicate it to the other nodes, wait for a majority to confirm, then respond — as described in [[3 - How Leader Election Works|How Leader Election Works]] and [[4 - How the Database Works|How the Database Works]].

## Walkthrough 2: a node fails, the reactive way

1. The current leader's process is killed (or it genuinely crashes).
2. The other nodes simply stop receiving heartbeats from it.
3. Each follower's randomized election timer is running independently; the first one to expire becomes a candidate and requests votes.
4. Once a majority vote for it, it becomes the new leader and the cluster resumes accepting writes.
5. The gap between step 1 and step 4 is real, measurable downtime — however long the timeout was set to, plus the time for the election itself. Measured: a median of about 0.6 seconds after a sudden kill, and about 0.9 seconds if the node had been struggling first, with the timeout set to between 500 and 1000 ms.

## Walkthrough 3: a node is about to fail, the SentinelDB way

1. The current leader starts showing early warning signs — its heartbeat round trips start growing, becoming irregular, or going unanswered.
2. The prediction layer on the leader, reading this telemetry ten times a second, feeds the recent window of signals into the deployed classifier (from [[6 - How Failure Prediction Works|How Failure Prediction Works]]).
3. The classifier returns a high-confidence "about to fail" prediction for this node, and keeps doing so for a full second.
4. Instead of waiting for the node to actually go silent, the system immediately triggers Raft's leadership-transfer mechanism, handing control to the healthiest up-to-date follower voluntarily. New writes pause for the moment this takes, and clients retry automatically.
5. The new leader takes over almost immediately — the handover happened *before* the old leader was actually unreachable, not after. Measured: leadership moved about 4.6 seconds before the death in 40 of 40 trials, the longest gap in writes was a median of about 200 ms, and when the old leader finally died, clients didn't notice.
6. Separately and offline, the [[5 - How We Prove It Cant Break|automaton-based check]] verifies that this whole handover process, including the proactive trigger firing at any moment, can never produce two leaders in the same term. The old and new leaders can overlap for an instant in *different* terms, which is harmless: the old one can no longer reach a majority.

## Why it's worth presenting the system this way

Walkthroughs 2 and 3, told back to back with the same latency graph, are the single clearest way to make the project's value obvious to someone seeing it for the first time — the difference between them is the entire pitch in visual form, no explanation needed before they can see it happening. `results/walkthrough.png` is that graph, and [[9 - How to Give the Demo|How to Give the Demo]] shows it happening live.

## Where each piece lives in the code

| Layer | Code |
|---|---|
| Query layer | `internal/sql`, `internal/engine`, `internal/txn` |
| Consensus layer | `internal/raft`, `internal/wal`, `internal/server` |
| Prediction layer | `internal/predict`, trained by `ml/train.py` |
| Verification layer | `internal/modelcheck`, run with `sentinel-check` |
| Network and security | `internal/transport` |
