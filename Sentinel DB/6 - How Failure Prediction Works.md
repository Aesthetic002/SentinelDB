---
tags: [sentinel-db, ai-ml, machine-learning, failure-prediction]
aliases: [SentinelDB ML Layer, SentinelDB Prediction]
created: 2026-10-13
updated: 2026-11-04
---

# How Failure Prediction Works

Back to [[1 - Overview|Overview]]. This note assumes you've read [[3 - How Leader Election Works|how leader election works]] — the whole point of this layer is to make that process faster and smoother.

## The problem being solved here

Plain Raft only notices a leader is gone once it's gone fully silent for a fixed amount of time (the election timeout). That's safe, but it wastes time — every single failure costs at least that much downtime, even if the leader was visibly struggling for several seconds before it actually died. The idea here is simple: watch for the warning signs, and act before the timeout would have fired anyway.

## What data we're actually watching

The leader continuously exchanges small "I'm alive" messages (heartbeats) with the others, and times the round trip of each one using only its own clock. From this stream, six simple signals are computed ten times a second, over the last two seconds: the average round trip, the average over just the last half second, how irregular the timing is (jitter — a steady slow response is different from one that's wildly inconsistent), the worst round trip, how fast round trips are growing, and what fraction of expected replies never came. None of this requires any special sensor or extra instrumentation — it's exactly the information the consensus protocol is already exchanging anyway, just looked at a bit more closely.

There's one design decision here that does a lot of work. The signals are read at **the position the leader's majority depends on**: a 5-node leader needs replies from 2 of its 4 followers, so it reads the second-best follower. That means a terrible link to one or two followers doesn't change the reading at all, while a problem on the leader's own side affects every follower and shows up immediately. It separates "the leader is sick" from "a follower is sick" without needing to label them differently.

## Getting labelled examples to learn from

A healthy, well-behaved cluster doesn't naturally produce examples of "about to fail" on its own — so to train anything useful, failures have to be deliberately and repeatedly caused on purpose, while recording what the heartbeat pattern looked like in the seconds leading up to that induced failure. The way they're caused matters more than you'd expect.

**You can't learn from a process that's simply killed.** The last second before a sudden kill looks the same as a healthy node, with round trips around 1 ms. There's nothing there to learn, and labelling those moments "about to fail" would only teach the model noise. So failures are induced by making a node degrade gradually over 6 to 10 seconds (growing delay, growing jitter, growing packet loss, or a mix) and then killing it. Sudden kills are still run, but they're labelled healthy and reported separately.

**Hard negatives matter as much as the failures.** Ordinary operation alone is too easy. The "healthy" examples deliberately include things that look alarming but aren't a reason to change leader: brief latency blips, a network that's steadily slow (up to about 300 ms, as bad as a dying node), a terrible link to one follower, and followers dying.

The dataset is **144 runs of real 5-node clusters**, giving 13,789 labelled readings. The first quarter of each ramp is left out, because a fault that has only just begun genuinely can't be told apart from health, and forcing a label on it either way would be a guess.

## The models we actually use, explained plainly

Nothing here is a neural network or anything exotic — on purpose, because that's genuinely what the AI/ML coursework this semester covers. They're also easier to explain, though how easy varies by model, as you'll see below.

- **Decision Tree** — asks a series of yes/no questions about the data ("is the latest response time over X milliseconds? if yes, has it missed more than Y of the last heartbeats?") and follows a branching path to a final answer. Easy to read out loud as a set of rules, which makes it a good first model to get working and a good one to actually explain live in a demo.
- **Naive Bayes** — looks at each signal (latency, jitter, missed beats) somewhat independently and asks "given everything I've seen about healthy nodes versus failing ones before, which pattern does this one look more like?" using basic probability. Fast, simple, and a reasonable baseline to compare against.
- **Logistic Regression** — draws a boundary line (in a mathematical sense, across all the signals at once) separating "healthy-looking" patterns from "failing-looking" ones, and reports how confident it is a given node is on the failing side of that line.
- **Random Forest** — trains many different Decision Trees on slightly different slices of the data and lets them vote; this usually smooths out the mistakes any single tree would make on its own, at the cost of being a little harder to explain in one sentence than a single tree.

## Choosing which one to actually use

All four are trained on the exact same data and compared fairly: some of the labelled examples are held back (never shown during training) purely to test each model afterward. Because neighbouring readings overlap in time and are nearly identical, whole runs are held back rather than individual rows. Splitting rows would put near-duplicates on both sides and report a flattering, meaningless score. Each model's performance is measured with **precision** (when it says "about to fail," how often is it right), **recall** (of all the times a node genuinely was about to fail, how many did it actually catch), and **F1** (a single combined score balancing the two).

**F1 turned out not to be enough, and this is the most interesting result of the whole layer.** The first version picked the model and threshold by F1 and scored 0.976 on held-back runs, which looks excellent. But counting per run instead of per reading, it raised a false alarm on every steady slow network in the development data. A jump from 1 ms to 100 ms looks just like the start of a ramp until enough time has passed to see that it stopped rising, and the model had also learned that "over 100 ms means failure," because almost nothing in its training data got that slow without failing.

So two things changed. The harder `steady-slow-high` scenario was added, so that latency alone can't predict failure. And the threshold and the number of consecutive readings needed before acting are now tuned together by what actually matters per run: fewest false alarms first, then most failures caught, then most warning. The result:

| Model | Precision | Recall | F1 | Failures caught | False-alarm runs | Median warning |
|---|---:|---:|---:|---:|---:|---:|
| **Random Forest (deployed)** | 0.972 | 0.971 | 0.972 | 12/12 | 0/17 | 5.0 s |
| Decision Tree | 0.921 | 0.985 | 0.952 | 12/12 | 0/17 | 2.8 s |
| Logistic Regression | 0.861 | 0.951 | 0.904 | 11/12 | 0/17 | 1.8 s |
| Naive Bayes | 0.990 | 0.580 | 0.731 | 7/12 | 0/17 | 3.2 s |

The Random Forest was chosen using only the training and validation runs. It acts at a probability of 0.75, held for 10 readings in a row (one second). The Decision Tree's exact rules are in `results/ml-comparison.md`, if you want something you can read out loud.

One honest caveat: the held-back runs in this table had already been seen once, when the first version was scored, so they aren't perfectly clean. The diagnosis itself used only development runs, and the live evaluation below, on fresh runs, is the independent check.

## What actually happens when the model is confident

This is the part that makes the whole layer more than a dashboard decoration: when the deployed model predicts, with high confidence and for a sustained second, that the current leader is about to fail, the system doesn't just log a warning — it actively triggers Raft's built-in leadership-transfer mechanism (mentioned in [[3 - How Leader Election Works|How Leader Election Works]]), handing control to the healthiest follower that's caught up, voluntarily and immediately, before the struggling leader actually goes silent. A 5-second cooldown stops a wrong call from turning into leadership bouncing around the cluster. The client barely notices anything happened, instead of experiencing the gap a full reactive election would have caused.

## How we check this is actually working, honestly

Two numbers matter, and both need to be reported, not just the flattering one. These come from live clusters on fresh runs, with a client writing continuously and each trial pairing plain Raft against the predictor on the same fault. The full report is `results/evaluation.md`.

**How much shorter is the gap?** "Outage" here means the longest stretch in which no write completed, as the client saw it.

| Failure | Plain Raft | With prediction | Handed over before death |
|---|---:|---:|---:|
| Growing delay | 890 ms | 202 ms | 10/10 |
| Growing jitter | 1,369 ms | 209 ms | 10/10 |
| Growing packet loss | 724 ms | 62 ms | 10/10 |
| Mixed degradation | 888 ms | 195 ms | 10/10 |
| Sudden kill, no warning | 619 ms | 700 ms | 0/10 |

Across all gradual failures that's a median of **910 ms down to 196 ms**, with leadership moving about 4.6 seconds ahead of the death. Tail latency while the node was getting worse improved as well, because the system stops waiting on a sick leader. The sudden kill shows no significant difference (p = 0.75), exactly as you'd expect: there's nothing to predict.

**How often does it trigger when the node was actually fine?** This number has to stay low — if it doesn't, the system isn't really predicting anything useful, it's just handing off leadership randomly and getting lucky on the downtime number, which would be a dishonest result if presented without this check alongside it. In 70 trials across seven situations where the leader never fails (healthy, blips, steady slow, very slow, one bad link, one or two followers dying), there were **zero false handovers**. Zero observed doesn't mean a rate of zero: with none in 70, the 95% upper bound is about 4% of runs, under conditions built to look like failure.

## What happens outside the training data

Those were fresh runs, but of the same kinds of fault the model trained on. Three scenarios it never saw test what it does beyond that:

| Unseen scenario | Plain Raft | With prediction | What it shows |
|---|---:|---:|---|
| Ramp over 25 s, far slower than training, then death | 828 ms | 660 ms | **Only 4 of 8 handed over.** A real generalization gap, though each miss just fell back to plain Raft |
| Ramp over 1.5 s, faster than the rule can act, then death | 884 ms | 806 ms | 0 of 8 handed over, as expected. It falls back safely |
| Same ramp as a failure, but the node recovers | 376 ms | 206 ms | It hands over every time, but being wrong is cheap: even a needless handover left a shorter gap than staying put |

## What this doesn't show

- **The faults are injected.** Ramps of delay, jitter and loss stand in for a failing machine. Real hardware may degrade differently, or not gradually at all, so none of this says how often real failures could be predicted.
- **All nodes shared one machine.** Comparisons between the two modes are the meaningful part, not absolute latencies.
- **The percentage depends on configuration.** Plain Raft's outage scales with the election timeout (500 to 1000 ms here), while the proactive path barely changes.
- **Safety never depended on this.** A wrong prediction can only cost a needless handover. See [[5 - How We Prove It Cant Break|How We Prove It Can't Break]].
