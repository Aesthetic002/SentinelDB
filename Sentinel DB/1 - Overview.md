---
tags: [sentinel-db, distributed-systems, overview]
aliases: [SentinelDB, Sentinel DB Overview]
created: 2026-10-13
updated: 2026-10-13
---

# SentinelDB — Overview

This is the starting note. It tells you what SentinelDB actually is, why it exists, and points you to a dedicated note for each part of how it works. Every note in this set is written in plain language on purpose — the goal is that you (or a teammate, or a mentor skimming at 11pm before a review) can open any single note and actually understand that piece without needing a lecture first.

## What SentinelDB actually is

SentinelDB is a small database that keeps its data copied across several machines instead of one, so that if one machine crashes, nothing is lost and almost nothing is noticed. Underneath, it's built from four ideas stacked on top of each other:

1. A **consensus protocol** (based on Raft) that keeps the copies in agreement, even while machines crash and restart.
2. A **real relational database** on top of that — tables, a small SQL language, proper schema design — not just a bucket of key-value pairs.
3. A **formal safety check**, where instead of just testing the system and hoping, we modelled it mathematically and checked, by brute force, that its single worst possible failure (two machines both thinking they're in charge at once) genuinely cannot happen.
4. A **machine-learning predictor** that watches for early warning signs a machine is about to fail, and switches control away from it *before* it actually dies, instead of waiting for a timeout.

None of these four ideas is new on its own — they're all things real systems and real research already do. What makes this project worth doing is combining them into one working system, small enough for two people to build and explain, but honest and rigorous enough to defend under questioning.

## Why this exists (the short version)

Real systems that can't afford downtime or data loss — a bank, a messaging app, the infrastructure behind Kubernetes — all solve the same underlying problem: keep several copies of the truth in sync, and survive individual machines failing. [[2 - Why We Built This (Inspiration and Related Papers)|This note]] goes into the real systems and real papers that inspired this project, with actual links, and explains *why* we made each major design choice the way we did — not just what we built.

## How to read these notes

Read them in this order if you're starting from zero:

1. [[2 - Why We Built This (Inspiration and Related Papers)|Why We Built This]] — the motivation, the real-world systems this is modelled on, and the actual papers behind each piece.
2. [[3 - How Leader Election Works|How Leader Election Works]] — the consensus protocol: how the machines agree on who's in charge, and what happens when that machine dies.
3. [[4 - How the Database Works|How the Database Works]] — the actual database part: tables, the query language, transactions.
4. [[5 - How We Prove It Cant Break|How We Prove It Can't Break]] — the formal safety check, explained without needing a theory-of-computation background to follow it.
5. [[6 - How Failure Prediction Works|How Failure Prediction Works]] — the machine-learning layer that predicts failures early.
6. [[7 - How It All Connects (Architecture)|How It All Connects]] — walks through what actually happens, step by step, for a normal request and for a failure, tying all the pieces together.
7. [[8 - Security - What's Protected and What Isn't|Security]] — what this system actually defends against, and what it honestly doesn't.

## The one-sentence version, if someone stops you in a corridor

"It's a database that copies itself across several machines so it never goes down from one machine failing, it mathematically proves its most dangerous bug can't happen instead of just testing for it, and it predicts a machine dying before it actually does, so the handover is smooth instead of jarring."
