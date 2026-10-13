---
tags: [sentinel-db, ai-ml, machine-learning, failure-prediction]
aliases: [SentinelDB ML Layer, SentinelDB Prediction]
created: 2026-10-13
updated: 2026-10-13
---

# How Failure Prediction Works

Back to [[1 - Overview|Overview]]. This note assumes you've read [[3 - How Leader Election Works|how leader election works]] — the whole point of this layer is to make that process faster and smoother.

## The problem being solved here

Plain Raft only notices a leader is gone once it's gone fully silent for a fixed amount of time (the election timeout). That's safe, but it wastes time — every single failure costs at least that much downtime, even if the leader was visibly struggling for several seconds before it actually died. The idea here is simple: watch for the warning signs, and act before the timeout would have fired anyway.

## What data we're actually watching

Every node continuously exchanges small "I'm alive" messages (heartbeats) with the others. From this stream, a handful of simple, computable signals are extracted for each node over a short recent time window: how long its responses are taking (round-trip time), how irregular that timing is (jitter — a steady slow response is different from one that's wildly inconsistent), and how many of its last few expected heartbeats were missed entirely. None of this requires any special sensor or extra instrumentation — it's exactly the information the consensus protocol is already exchanging anyway, just looked at a bit more closely.

## Getting labelled examples to learn from

A healthy, well-behaved cluster doesn't naturally produce examples of "about to fail" on its own — so to train anything useful, failures have to be deliberately and repeatedly caused on purpose (killing a node's process, artificially slowing its network) while recording what its heartbeat pattern looked like in the seconds leading up to that induced failure. Those windows become the "about to fail" examples; ordinary operation provides the "healthy" examples. This labelled dataset is what any of the models below actually learn from.

## The models we actually use, explained plainly

Nothing here is a neural network or anything exotic — on purpose, both because that's genuinely what the AI/ML coursework this semester covers, and because an interpretable model is a real advantage: it lets us point to exactly *why* a particular prediction was made, instead of shrugging at a black box.

- **Decision Tree** — asks a series of yes/no questions about the data ("is the latest response time over X milliseconds? if yes, has it missed more than Y of the last heartbeats?") and follows a branching path to a final answer. Easy to read out loud as a set of rules, which makes it a good first model to get working and a good one to actually explain live in a demo.
- **Naive Bayes** — looks at each signal (latency, jitter, missed beats) somewhat independently and asks "given everything I've seen about healthy nodes versus failing ones before, which pattern does this one look more like?" using basic probability. Fast, simple, and a reasonable baseline to compare against.
- **Logistic Regression** — draws a boundary line (in a mathematical sense, across all the signals at once) separating "healthy-looking" patterns from "failing-looking" ones, and reports how confident it is a given node is on the failing side of that line.
- **Random Forest** — trains many different Decision Trees on slightly different slices of the data and lets them vote; this usually smooths out the mistakes any single tree would make on its own, at the cost of being a little harder to explain in one sentence than a single tree.

## Choosing which one to actually use

All four are trained on the exact same data and compared fairly: some of the labelled examples are held back (never shown during training) purely to test each model afterward, and each model's performance is measured with three numbers — **precision** (when it says "about to fail," how often is it right), **recall** (of all the times a node genuinely was about to fail, how many did it actually catch), and **F1** (a single combined score balancing the two). Whichever model scores best on this held-out test data is the one that gets deployed, and the comparison itself — not just the winning number — is written up, since showing the reasoning is more convincing than just announcing a result.

## What actually happens when the model is confident

This is the part that makes the whole layer more than a dashboard decoration: when the deployed model predicts, with high confidence, that the current leader is about to fail, the system doesn't just log a warning — it actively triggers Raft's built-in leadership-transfer mechanism (mentioned in [[3 - How Leader Election Works|How Leader Election Works]]), handing control to a healthy follower voluntarily and immediately, before the struggling leader actually goes silent. The client barely notices anything happened, instead of experiencing the multi-second gap a full reactive election would have caused.

## How we check this is actually working, honestly

Two numbers matter, and both need to be reported, not just the flattering one:

- **How much shorter is the "no leader in charge" gap**, measured directly, comparing the reactive-only baseline against the proactive version, across many independently induced failures.
- **How often does the model trigger a handover when the node was actually fine** (a false alarm). This number has to stay low — if it doesn't, the system isn't really predicting anything useful, it's just handing off leadership randomly and getting lucky on the downtime number, which would be a dishonest result if presented without this check alongside it.
