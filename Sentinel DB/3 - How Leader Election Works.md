---
tags: [sentinel-db, distributed-systems, raft, leader-election]
aliases: [SentinelDB Raft, SentinelDB Leader Election]
created: 2026-10-13
updated: 2026-10-13
---

# How Leader Election Works

Back to [[1 - Overview|Overview]]. See also [[2 - Why We Built This (Inspiration and Related Papers)|the Raft paper this is based on]].

## What a "node" actually is

A node is just one running copy of the SentinelDB server program. It's not necessarily a separate physical machine — in development and for a live demo, it's realistic to run all five nodes as separate processes on one laptop (each on a different network port) or as separate Docker containers. What matters isn't the physical hardware, it's that each node keeps its own independent copy of the data and talks to the others only over the network, exactly as if it were on its own machine.

## The three things a node can be

At any moment, every node is in exactly one of three roles:

- **Follower** — the default, passive state. A follower just listens, stores whatever the leader tells it to, and replies to messages.
- **Candidate** — a temporary state a node enters when it thinks the leader might be gone and it's trying to become the new leader itself.
- **Leader** — the one node currently allowed to accept new writes from clients and decide the order they happen in. There is supposed to be at most one of these at any given time.

## Terms: how the system avoids confusion about "which round" it's in

Time is divided into **terms**, numbered 1, 2, 3, and so on, increasing forever. Each term has at most one leader. Terms are the system's way of answering "is this message old news or current" — if a node ever hears from a term lower than one it's already seen, it knows that message is stale and ignores it. This single mechanism is what lets nodes safely tell the difference between an old leader that's come back online after a network hiccup and the genuinely current leader.

## How an election actually happens

Every follower runs a timer with a *randomized* length (not the same number for every node — this randomization matters, explained below). If a follower's timer runs out without it hearing from a leader, it assumes the leader is gone, becomes a candidate, increases the term number by one, votes for itself, and asks every other node for their vote.

A node will grant its vote to a candidate only if it hasn't already voted for someone else in that term, and the candidate's log is at least as up to date as its own (this second rule is what stops a node that's fallen behind on data from ever becoming leader and overwriting newer data with older data). If a candidate collects votes from a **majority** of all nodes — more than half, so 3 out of 5 for a 5-node cluster — it becomes the leader for that term and immediately starts sending heartbeats to the others to assert its leadership and reset their timers.

**Why the timeout is randomized:** if every follower's timer were exactly the same length, they'd all time out at the same instant, all become candidates simultaneously, split the vote evenly, and the election would fail and have to restart — potentially forever. Randomizing each node's timeout means one node almost always notices first, becomes a candidate, and usually wins the election before anyone else's timer even expires.

## How writes actually get replicated

Once a leader is elected, every write a client sends goes through it. The leader appends the write to its own local log and sends it to every follower in parallel. A write is only considered **committed** — safe, durable, and something the client can be told succeeded — once a majority of nodes (including the leader) have stored it in their log. This majority rule is the entire reason the system can survive failures: as long as a majority of nodes are alive and can talk to each other, the system keeps working correctly, because any new leader elected afterward is guaranteed, by the voting rule above, to already have every committed write in its own log.

With 5 nodes, this means the system can lose any 2 nodes at once and keep working perfectly — a concrete number worth having ready if someone asks "how much failure can this actually tolerate."

## What happens when the leader dies

**The normal (reactive) way:** followers simply stop hearing heartbeats, their timers run out, and a new election happens as described above. The catch is that this takes however long the timeout is set to — during that window, there's no leader, so the system can't accept new writes. That gap is real, user-visible downtime.

**The way SentinelDB improves on this:** instead of only reacting once the leader has gone fully silent, a machine-learning model (covered in [[6 - How Failure Prediction Works|How Failure Prediction Works]]) watches the leader's heartbeat pattern for early signs of trouble — growing response times, irregular gaps — and when it's confident something's wrong, it triggers Raft's built-in **leadership transfer**: the current leader voluntarily hands control to a healthy follower *before* it actually goes silent. This is a graceful, near-instant handover instead of a multi-second gap followed by an election, and it's the single biggest practical difference between SentinelDB and a plain textbook Raft implementation.

## What this protocol does and doesn't protect against

Raft (and therefore SentinelDB) assumes every node that's still running is being honest — it protects against nodes crashing, freezing, or losing network connectivity, but not against a node that's still running and deliberately lying (sending fake votes or corrupted data). That second, harder problem is called Byzantine fault tolerance, and it's a genuinely different and more expensive thing to solve — see [[8 - Security - What's Protected and What Isn't|Security]] for the full discussion of what this does and doesn't defend against.
