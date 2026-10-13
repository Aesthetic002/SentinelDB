---
tags: [sentinel-db, distributed-systems, motivation, papers]
aliases: [SentinelDB Inspiration, SentinelDB Related Work]
created: 2026-10-13
updated: 2026-10-13
---

# Why We Built This — Inspiration and Related Papers

Back to [[1 - Overview|Overview]].

This note answers two questions honestly: what real problem is this solving, and where did every major idea in SentinelDB actually come from. Nothing here was invented in a vacuum — each piece is a small, teaching-scale version of something real systems and real researchers already do, and this note says exactly which ones, with working links.

## The real problem

Picture any system that genuinely cannot afford to lose data or go offline when one machine dies — your bank's account balances, a messaging app's message history, the control plane that keeps a company's cloud infrastructure running. All of these keep several copies of the truth on different machines, and all of them need a way to keep those copies agreeing with each other, including while individual machines crash, restart, or temporarily lose network connectivity. That agreement problem is called **distributed consensus**, and it's one of the most studied, most practically important problems in computer science. SentinelDB is a small, honest attempt at solving it end to end — not a toy simulation, an actual working system you can kill a node on and watch recover.

## The real systems that inspired the shape of this project

- **etcd** — the key-value store that sits underneath almost every Kubernetes cluster in the world, keeping track of cluster state. It uses Raft internally. The Raft reference material and visualizations most people learn from (including us) live at [raft.github.io](https://raft.github.io), maintained by people connected to etcd's development.
- **CockroachDB** — a real production SQL database that shards and replicates data across machines using a Raft-based protocol under the hood, and still lets you run normal SQL queries against it. This is almost exactly SentinelDB's shape (SQL on top, Raft-replicated shards underneath), just at a scale we're not attempting. Their own engineering team published a real paper on it: [CockroachDB: The Resilient Geo-Distributed SQL Database (SIGMOD 2020)](https://rcs.uwaterloo.ca/~ali/cs854-f23/papers/cockroachdb.pdf), also summarized on their [engineering blog](https://www.cockroachlabs.com/blog/cockroachdb-sigmod-2020/).
- **TiKV** — another real, production distributed key-value store built the same way (Raft + sharding), used as the storage layer under TiDB. Worth a glance if you want to see how a serious engineering team structures the same idea we're building.

The honest takeaway: we are not inventing a new kind of system. We're building a small, explainable version of a pattern that real infrastructure companies run in production every day, which is exactly why it's a credible academic project rather than a made-up toy problem.

## The paper behind the consensus protocol

**Diego Ongaro and John Ousterhout, "In Search of an Understandable Consensus Algorithm" (USENIX ATC 2014)** — the paper that introduced Raft.
Paper: [raft.github.io/raft.pdf](https://raft.github.io/raft.pdf) (extended version) · [conference page](https://www.usenix.org/conference/atc14/technical-sessions/presentation/ongaro)

**Why we built on Raft specifically, not the older alternative (Paxos):** the paper's own title says it — Raft was designed, on purpose, to be *understandable*. The older consensus algorithm, Paxos, is known to be notoriously hard to reason about and even harder to implement correctly; Raft breaks the same problem into clearly separated sub-problems (who's the leader, how the log replicates, how safety is preserved) specifically so that students and engineers can actually hold the whole thing in their head. That's precisely the property we needed for a project two people have to build, understand completely, and defend in front of a panel — not just get to compile.

## The paper (and real research project) behind our safety proof

Most student projects validate their system by testing it and hoping nothing was missed. We wanted to do something stronger for the one failure mode that matters most (two leaders at once), so we looked at how real researchers have handled exactly this problem for Raft itself:

- **James R. Wilcox, Doug Woos, Pavel Panchekha, Zachary Tatlock, Xi Wang, Michael D. Ernst, Thomas Anderson, "Verdi: A Framework for Implementing and Formally Verifying Distributed Systems" (PLDI 2015)** — [paper PDF](https://homes.cs.washington.edu/~ztatlock/pubs/verdi-wilcox-pldi15.pdf)
- **Doug Woos, James R. Wilcox, Steve Anton, Zachary Tatlock, Michael D. Ernst, Thomas Anderson, "Planning for Change in a Formal Verification of the Raft Consensus Protocol" (CPP 2016)** — [paper PDF](https://jamesrwilcox.com/raft-proof.pdf)

These researchers built a machine-checked, mathematically rigorous proof that a real Raft implementation satisfies its safety properties — a vastly more sophisticated undertaking than what we're doing. Our own safety check (see [[5 - How We Prove It Cant Break|How We Prove It Can't Break]]) is a much smaller, teaching-scale cousin of the same idea: instead of a full machine-checked proof over an unbounded system, we build an explicit model of a small, bounded cluster and exhaustively check every state it can reach. It's a legitimate technique with a real name (explicit-state model checking) — we're just applying it at a scale two people can actually finish in a semester, and being upfront about that scope difference rather than overclaiming.

## The papers behind predicting failure before it happens

Most fault-tolerant systems, including a plain implementation of Raft, are *reactive*: a node is only considered dead once it's been silent for a fixed timeout. Several real research groups have asked whether you can do better by watching for the warning signs first:

- **"A Proactive Approach to Fault Tolerance Using Predictive Machine Learning Models in Distributed Systems"** — [paper](https://www.researchgate.net/publication/385502216_A_Proactive_Approach_to_Fault_Tolerance_Using_Predictive_Machine_Learning_Models_in_Distributed_Systems)
- **"Proactive Fault Tolerance using Heartbeat Strategy for Fault Detection"** — [paper](https://www.researchgate.net/publication/354532918_Proactive_Fault_Tolerance_using_Heartbeat_Strategy_for_Fault_Detection)
- **"Time-Series Learning for Proactive Fault Prediction in Distributed Systems with Deep Neural Structures"** (arXiv, 2025) — [paper](https://arxiv.org/abs/2505.20705)

These confirmed the core idea is a real, active research direction, not something we made up to sound clever. Where SentinelDB differs from most of what's published here: a lot of this line of work uses deep learning (neural networks) to make the prediction. We deliberately use classical, interpretable models instead — Decision Trees, Naive Bayes, Logistic Regression, Random Forests (see [[6 - How Failure Prediction Works|How Failure Prediction Works]]) — partly because that's genuinely what our AI/ML coursework covers, and partly because an interpretable model is actually an advantage here: if a judge asks "why did it predict this node was failing," we can point to the exact decision rule, not shrug at a black box.

## Why we made the decisions we made, summarized

| Decision | Why |
|---|---|
| Raft over Paxos | Deliberately designed to be understandable and explainable, which matters when two people have to build and defend the whole thing |
| Exhaustive state-space checking over "just testing" | The one failure mode that matters most (split leadership) deserves a stronger guarantee than "we ran it a lot and it seemed fine" — and real researchers (Verdi/CPP16) have shown this class of technique is worth doing for Raft specifically |
| A real SQL layer over a raw key-value store | Production systems that actually matter (CockroachDB, TiKV) are built exactly this way — SQL on top, replicated shards underneath — so it's not an arbitrary addition, it's the realistic shape of the problem |
| Classical ML over deep learning for failure prediction | Matches what the course actually teaches, and gives us an explainable decision rule instead of a black box, which is a real advantage when defending the result |
