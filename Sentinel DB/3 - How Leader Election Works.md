---
tags: [sentinel-db, distributed-systems, raft, leader-election]
aliases: [SentinelDB Raft, SentinelDB Leader Election]
created: 2026-10-13
updated: 2026-11-04
---

# How Leader Election Works

Back to [[1 - Overview|Overview]]. See also [[2 - Why We Built This (Inspiration and Related Papers)|the Raft paper this is based on]].

## What a "node" actually is

A node is just one running copy of the SentinelDB server program. It's not necessarily a separate physical machine — in development and for a live demo, it's realistic to run all five nodes as separate processes on one laptop (each on a different network port) or as separate Docker containers. What matters isn't the physical hardware, it's that each node keeps its own independent copy of the data and talks to the others only over the network, exactly as if it were on its own machine.

## The three things a node can be

At any moment, every node is in exactly one of three roles:

- **Follower** — the default, passive state. A follower just listens, stores whatever the leader tells it to, and replies to messages.
- **Candidate** — a temporary state a node enters when it thinks the leader might be gone and it's trying to become the new leader itself.
- **Leader** — the one node currently allowed to accept new writes from clients and decide the order they happen in. There is at most one per term. For a brief moment two nodes can both *believe* they lead, in different terms: an old leader that hasn't heard the news yet. That is harmless, because it can no longer reach a majority, and [[5 - How We Prove It Cant Break|the safety check]] covers it explicitly.

## Terms: how the system avoids confusion about "which round" it's in

Time is divided into **terms**, numbered 1, 2, 3, and so on, increasing forever. Each term has at most one leader. Terms are the system's way of answering "is this message old news or current" — if a node ever hears from a term lower than one it's already seen, it knows that message is stale and ignores it. This single mechanism is what lets nodes safely tell the difference between an old leader that's come back online after a network hiccup and the genuinely current leader.

## How an election actually happens

Every follower runs a timer with a *randomized* length (not the same number for every node — this randomization matters, explained below). If a follower's timer runs out without it hearing from a leader, it assumes the leader is gone, becomes a candidate, increases the term number by one, votes for itself, and asks every other node for their vote.

A node will grant its vote to a candidate only if it hasn't already voted for someone else in that term, and the candidate's log is at least as up to date as its own (this second rule is what stops a node that's fallen behind on data from ever becoming leader and overwriting newer data with older data). If a candidate collects votes from a **majority** of all nodes — more than half, so 3 out of 5 for a 5-node cluster — it becomes the leader for that term and immediately starts sending heartbeats to the others to assert its leadership and reset their timers.

**Why the timeout is randomized:** if every follower's timer were exactly the same length, they'd all time out at the same instant, all become candidates simultaneously, split the vote evenly, and the election would fail and have to restart — potentially forever. Randomizing each node's timeout means one node almost always notices first, becomes a candidate, and usually wins the election before anyone else's timer even expires.

## How writes actually get replicated

Once a leader is elected, every write a client sends goes through it. The leader appends the write to its own local log and sends it to every follower in parallel. A write is only considered **committed** — safe, durable, and something the client can be told succeeded — once a majority of nodes (including the leader) have stored it in their log. This majority rule is the entire reason the system can survive failures: as long as a majority of nodes are alive and can talk to each other, the system keeps working correctly, because any new leader elected afterward is guaranteed, by the voting rule above, to already have every committed write in its own log.

With 5 nodes, this means the system can lose any 2 nodes at once and keep working perfectly — a concrete number worth having ready if someone asks "how much failure can this actually tolerate."

A test kills nodes one at a time to confirm this: with 2 of 5 gone it keeps writing, and with 3 gone it refuses writes rather than risk anything, then recovers with nothing lost or invented once a majority returns.

### One rule the short version leaves out

"Committed once a majority have it" has an exception that catches out real implementations. A new leader must not treat an entry from an *older* term as committed just because a majority now hold it, because a node with a newer entry at that position could still win an election and overwrite it (this is Figure 8 of the Raft paper). A leader can only count replicas of entries from its own term, and earlier entries commit along with them. That's why every new leader immediately appends an empty entry of its own: it lets the leader learn what's committed without waiting for a client to write something. Our model checker found this bug by itself when we deliberately removed the rule.

## What happens when the leader dies

**The normal (reactive) way:** followers simply stop hearing heartbeats, their timers run out, and a new election happens as described above. The catch is that this takes however long the timeout is set to — during that window, there's no leader, so the system can't accept new writes. That gap is real, user-visible downtime: in our measurements about 0.9 seconds, with the timeout set to between 500 and 1000 ms. It scales with that setting, which can't simply be made tiny, because on a real network that causes needless elections.

**The way SentinelDB improves on this:** instead of only reacting once the leader has gone fully silent, a machine-learning model (covered in [[6 - How Failure Prediction Works|How Failure Prediction Works]]) running on the leader watches its heartbeat round trips for early signs of trouble — growing response times, irregular gaps, missed replies — and when it's confident something's wrong, it triggers Raft's built-in **leadership transfer**: the current leader voluntarily hands control to a healthy follower *before* it actually goes silent. The leader stops accepting new writes, makes sure the chosen follower has its whole log, and tells it to start an election straight away. Measured on live clusters, the longest gap in writes falls to about 200 ms, and it's the single biggest practical difference between SentinelDB and a plain textbook Raft implementation.

Two limits are worth saying out loud. A leader that dies with no warning signs still costs the normal timeout, because there was nothing to predict. And a transfer can fail: if it hasn't finished within one election timeout, the leader abandons it and carries on, and ordinary elections remain the safety net. Either way, a transfer goes through normal voting, so a wrong prediction can cost an unnecessary handover but can't break anything.

## What this protocol does and doesn't protect against

Raft (and therefore SentinelDB) assumes every node that's still running is being honest — it protects against nodes crashing, freezing, or losing network connectivity, but not against a node that's still running and deliberately lying (sending fake votes or corrupted data). That second, harder problem is called Byzantine fault tolerance, and it's a genuinely different and more expensive thing to solve — see [[8 - Security - What's Protected and What Isn't|Security]] for the full discussion of what this does and doesn't defend against.
