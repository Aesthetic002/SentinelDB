# What is verified, and how far each claim goes

SentinelDB has several layers of evidence. They support different claims, and they are easy to overstate, so this page says what each one does and does not show. To reproduce any of it, see the commands at the end.

## 1. Exhaustive model checking of the protocol design

`internal/modelcheck`, with results in `results/verification.md`.

**The claim:** in a finite model of Raft, *every* reachable state satisfies Election Safety, Log Matching, Leader Completeness and State Machine Safety. This is the check the design notes call for, and it is exhaustive within its bounds rather than a sample.

**Why it is stronger than testing:** a test covers the situations someone thought of. This covers every interleaving of messages, losses, crashes and leadership transfers, up to the bounds.

**Why the search can finish:** the model is a finite automaton, so "can it reach a bad state?" is decidable. For arbitrary programs the same question is undecidable (it would solve the Halting Problem). The project sidesteps this by checking a deliberately restricted model, not by being clever.

**How a clean result is known to mean something:** eight known bugs are injected one at a time, and the checker must find each. It finds them all, and rediscovers Figure 8 of the Raft paper on its own. The same bounds are clean without the bug, so the bug is what caused each violation.

### What it does not show

- **It checks the design, not the Go code.** The model is a separate program. A bug in `internal/raft` that the model does not share would not be caught here. Section 2 addresses that gap, with weaker evidence.
- **It is bounded.** Results hold for the node, term and log sizes in the report. There is no proof for unbounded systems. Verdi, in the design notes, is what that takes.
- **It does not cover membership changes.** Membership is fixed in the model. Those changes are covered only by randomized simulation and the specific tests of section 2.
- **It assumes crash faults only.** Nodes that lie are out of scope, as in the design notes.

### "Never two leaders" needs careful wording

The precise, proven statement is: **at most one leader per term**. Two nodes can both *believe* they lead at the same instant, in different terms: a deposed leader that has not yet heard the news. The checker shows this is reachable (`sentinel-check -two-leaders`) and it happens during an ordinary handover. It is harmless, because the old leader cannot reach a majority to commit anything: a majority has already moved to the higher term. Reads are protected as well, because a leader confirms it is still current with a quorum before answering.

If a panel asks whether two leaders can ever exist at once, the honest answer is: "Not in the same term, which is what safety requires. A stale one can exist for a moment in an older term, but it cannot do any damage, and the checker shows both."

### The predictor cannot affect safety

In the model, any leader can hand off to any node at any moment, however wrong that is. So the safety result holds regardless of how good the classifier is. The worst a bad prediction can do is cost a needless handover. It cannot cause split leadership or data loss.

### Does the state count mean the model is too small?

The model represents AppendEntries symbolically and treats renamings of nodes as one state. Neither hides behaviour (`TestSymmetryReductionPreservesResults` cross-checks the second), but they do mean the count is not comparable with other tools' counts for "Raft".

## 2. Randomized simulation of the real code

`internal/raft/sim_test.go`.

**The claim:** the actual implementation kept the same safety properties across 320 seeded runs (8 scenarios of 40 seeds each, 6,000 ticks per run) with message loss, reordering, duplication, delay, partitions, crashes, compaction, transfers and membership changes. Any failure prints a seed that replays it exactly.

**What it adds:** it runs the code that ships, including everything the model leaves out: snapshots, batching, membership.

**What it does not show:** that no bug exists. It is sampling, and it can miss rare interleavings. For example, it never found Figure 8 by chance, so that is a scripted test (`TestFigure8`).

**How the tests are known to work:** `scripts/mutation_check.py` injects 14 protocol bugs into the real code and requires the suite to catch each, after first checking that unmodified code passes.

## 3. Isolation: conflict-serializability of real schedules

`internal/txn`, `internal/engine/engine_test.go`.

**The claim:** the schedule actually produced by concurrent transfers under 2PL has an acyclic precedence graph, hence is equivalent to some serial order, and no money appears or vanishes.

**The control:** with locks disabled, the same check reports a cycle (`TestWithoutLocksTheScheduleIsNotSerializable`). Without that, the positive result would prove nothing.

**Limits:** it is a test on one workload. 2PL provides the guarantee by design, and this checks the implementation of it on real runs.

## 4. Durability and corruption

`internal/wal/wal_test.go`.

**The claim:** a crash at any byte of a write recovers to a valid prefix, and a flipped bit anywhere except the final record is reported as corruption instead of being replayed. These two tests are exhaustive over one small sample log.

**Limits:** a flip inside the *final* record cannot be told apart from a torn write, so that record is dropped. That is safe, as a record not fully on disk was never acknowledged. And the test can't simulate a disk that lies about fsync.

## 5. End to end

`internal/server/cluster_test.go`, with real TLS, sockets and files, and `sentinel-lab`, with separate processes and real kills.

**The claim:** acknowledged writes survive leader failure, retried writes are not applied twice, a bank-transfer invariant holds across forced leadership changes, and replicas end up byte-identical.

**Under extreme CPU starvation** (about 100 busy threads on 12 cores, far past normal use), elections become spurious and a leadership transfer can be abandoned. Both are safe outcomes, and a stress run of the transfer test saw only those two, in 2 of 100 runs. One earlier run under the same load had a client unable to connect for 20 seconds. That did not recur, and its cause was not established. Clusters on a machine not under that load passed every run.

**Also checked:** the security properties in the design notes, each against an actual attempt to break it: no certificate, a client certificate on the peer port, a certificate from another CA, a node impersonating another node, forged roles, and insufficient tokens.

The Go race detector is clean on the concurrent packages. It needs a C toolchain, so it runs in Docker:

```
docker run --rm -v "$PWD:/src" -w /src golang:1.24 go test -race ./internal/txn ./internal/engine ./internal/server
```

## 6. The predictor

`results/ml-comparison.md`, `results/evaluation.md`.

**The claim:** on gradually degrading failures, measured with the deployed model on live clusters, handing over early shortens the outage, without many false handovers under conditions designed to mimic failure.

### What it does not show

- **The faults are injected.** Delay, jitter and loss ramps stand in for a failing machine. Real hardware may degrade differently, or not gradually at all. No claim is made about real-world failure prediction rates, which would need production failure data this project does not have.
- **It cannot predict sudden failures.** A process killed with no warning looks perfectly healthy until it is gone. That is why those runs are labelled healthy, and reported separately, and plain Raft's election remains the backstop.
- **It generalizes only partly.** On a ramp much slower than any in training (25 s against 6 to 10 s) it handed over in only 4 of 8 trials. Each miss fell back to plain Raft's outage, so nothing was lost beyond the benefit, but the catch rates on familiar faults should not be read as catch rates in general. Training on a wider range of ramp speeds is the obvious next step.
- **Zero false alarms is a bound, not a rate.** None in 70 trials means a rate below about 4% of runs, with 95% confidence. It does not mean never.
- **All nodes share one machine.** Absolute latencies are not those of a real network. The comparison between modes is the meaningful part.
- **Adversarial heartbeats are not defended against**, as the design notes' threat model says. A misbehaving node can look sick on purpose and cause needless handovers. It cannot cause a safety violation.

## Reproducing all of it

```
go test ./...                                     # everything except the long runs
python scripts/mutation_check.py . /tmp/mutants   # do the tests catch injected bugs?
sentinel-check -suite                             # exhaustive model checking, with report
sentinel-lab collect                              # build the training set (about 35 minutes)
python ml/train.py                                # train, compare, export
sentinel-lab bench                                # live comparison
python scripts/evaluate.py                        # report and figures
```
