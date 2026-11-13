# Slides to code

A slide-by-slide guide to the Phase II deck (`SentinelDB_EL_Phase2.pptx`): for each slide, which files hold the thing it talks about, and what each file does there.

How to read it:
- Each slide gets a one-line summary, then a table of files.
- A name in `backticks` after a file, like `campaign`, is the function to open inside that file.
- Paths are from the project root. Click a path to open the file.

For a map of the whole codebase that isn't tied to the slides, see [code-guide.md](code-guide.md).

---

## Slide 1: Title

No code. Just the project name.

## Slide 2: Presentation outline

No code. The sections are: Recap of Phase I, Project Execution (slides 4 to 9), Tools and Techniques (10 to 14), Partial Results (15 to 20), and Conclusion (21).

---

## Slide 3: Recap of Phase I

**In one line:** all four Phase I promises are built. Each row of the slide's table points to a different part of the code.

| Promise on the slide | Where it lives | What it does |
|---|---|---|
| 5-node Raft-replicated database | [internal/raft/node.go](../internal/raft/node.go) | The Raft protocol: elections, copying the log, committing by majority |
| | [cmd/sentinel-node/main.go](../cmd/sentinel-node/main.go) | The program you run once per node |
| Classifiers that trigger an early handover | [ml/train.py](../ml/train.py) | Trains and compares the 4 models |
| | [internal/predict/](../internal/predict) | Runs the chosen model inside the leader |
| SQL layer on the replicated log | [internal/sql/](../internal/sql), [internal/engine/](../internal/engine), [internal/txn/](../internal/txn) | Reads SQL, runs it, handles transactions |
| Prediction never breaks safety | [internal/modelcheck/](../internal/modelcheck) | The model checker that explores every reachable state |

---

## Slide 4: The cluster as built

**In one line:** five copies of the same program, each with its own ports and its own disk, with no master.

| File | What it does |
|---|---|
| [cmd/sentinel-node/main.go](../cmd/sentinel-node/main.go) | The node program. Reads its settings (its id, its two ports, its data folder, the other nodes' addresses) and starts a server |
| [internal/server/server.go](../internal/server/server.go) | `Start`: opens the node's disk log, recovers its data, and opens the two ports |
| [internal/server/conn.go](../internal/server/conn.go) | `handleClientConn` serves the client port (7101–7105). `handlePeerConn` serves the node-to-node port (7201–7205) |
| [internal/server/conn.go](../internal/server/conn.go) | `relayRequest`: "any node accepts a request and forwards it" to the leader |
| [internal/wal/wal.go](../internal/wal/wal.go) | "Own disk": each node's log and snapshot files |
| [internal/lab/cluster.go](../internal/lab/cluster.go) | What `sentinel-lab` uses to launch, stop and kill the five node processes |
| [cmd/sentinel-lab/main.go](../cmd/sentinel-lab/main.go) | The `sentinel-lab` program itself (`serve` starts a local cluster) |

---

## Slide 5: Inside every node

**In one line:** one loop owns all Raft state; writes become row changes; disk comes before network; the predictor runs in the same loop.

| Point on the slide | File | What it does |
|---|---|---|
| "One loop … ticks every 10 ms" | [internal/server/server.go](../internal/server/server.go) | `loop` is the only code that touches Raft. `TickInterval = 10ms` is set in `defaults` |
| The consensus logic the loop drives | [internal/raft/node.go](../internal/raft/node.go) | Raft as a pure state machine: `Tick` (time passes), `Step` (a message arrives), `Ready` (what to save and send) |
| "SQL engine turns a write into row changes" | [internal/engine/exec.go](../internal/engine/exec.go) | Works out exactly which rows to insert, change or delete |
| "Tables change only once it commits" | [internal/server/server.go](../internal/server/server.go), [internal/engine/catalog.go](../internal/engine/catalog.go) | `applyEntry` hands a committed entry to `Apply`, which changes the in-memory tables |
| "Disk first" | [internal/raft/node.go](../internal/raft/node.go) | `appendLocal` and `persistHard` save to disk straight away, inside the node, before any message is produced |
| | [internal/wal/wal.go](../internal/wal/wal.go) | `Append` / `writeSync`: writes and syncs (fsyncs) the entries |
| | [internal/server/server.go](../internal/server/server.go) | `process` only sends the messages afterwards, so everything sent is already on disk |
| "Predictor runs inside the same loop" | [internal/server/server.go](../internal/server/server.go) | `evaluateMonitor` runs at most every 100 ms, only while leader |

---

## Slide 6: The path of one write

**In one line:** client → any node → leader → locks → disk → 3 of 5 nodes → commit → OK.

| Step | File | What it does |
|---|---|---|
| Client sends the statement | [internal/client/client.go](../internal/client/client.go) | `Exec` / `do`: connects, sends, retries if a node fails |
| A follower forwards to the leader | [internal/server/conn.go](../internal/server/conn.go) | `relayRequest` |
| SQL text becomes something runnable | [internal/sql/](../internal/sql) | Parser and translator (details on slide 12) |
| Leader locks the rows (2PL) | [internal/txn/locks.go](../internal/txn/locks.go) | `Acquire`: row and table locks |
| Leader works out the row changes | [internal/engine/exec.go](../internal/engine/exec.go) | `execInsert`, `execUpdate`, `execDelete` |
| Proposed as one log entry | [internal/server/server.go](../internal/server/server.go) | `Commit` |
| Appended and synced to disk | [internal/raft/node.go](../internal/raft/node.go), [internal/wal/wal.go](../internal/wal/wal.go) | `Propose` adds it to the log; the WAL syncs it |
| Sent to the followers | [internal/transport/peers.go](../internal/transport/peers.go) | `Send`: delivers Raft messages between nodes |
| "Committed once 3 of 5 have it" | [internal/raft/node.go](../internal/raft/node.go) | `maybeCommit` counts copies; `quorum()` is "more than half" |
| Every node applies it | [internal/engine/catalog.go](../internal/engine/catalog.go) | `Apply` |
| "Request IDs make a retry safe" | [internal/client/client.go](../internal/client/client.go) | `newRequestID`: one ID per write, reused on every retry |
| | [internal/engine/catalog.go](../internal/engine/catalog.go) | `seen`: if that ID was already applied, return the old result instead of applying again |

---

## Slide 7: Leader election, and the early handover

**In one line:** a normal election when the leader dies, and a planned handover when the predictor fires. Both use the same voting rules.

| Point on the slide | File | What it does |
|---|---|---|
| Each node's own timer | [internal/raft/node.go](../internal/raft/node.go) | `Tick` counts time; `resetElectionTimer` picks a random timeout |
| "Asks for votes in term 5" | [internal/raft/node.go](../internal/raft/node.go) | `campaign`: become candidate, add 1 to the term, ask everyone for votes |
| Others decide whether to vote | [internal/raft/node.go](../internal/raft/node.go) | `handleVote`: one vote per term, only for a candidate whose log is up to date |
| Counting votes, becoming leader | [internal/raft/node.go](../internal/raft/node.go) | `handleVoteResp`, `countVotes`, `becomeLeader` |
| Role state machine (follower / candidate / leader) | [internal/raft/node.go](../internal/raft/node.go) | `becomeFollower`, `campaign`, `becomeLeader` |
| Handover step 1: "Predictor fires" | [internal/predict/monitor.go](../internal/predict/monitor.go) | `Evaluate`: ≥ 0.75 for 10 readings in a row |
| Handover step 2: "Pauses writes, catches the target up" | [internal/raft/node.go](../internal/raft/node.go) | `TransferLeadership` |
| Handover step 3: "Sends TimeoutNow" | [internal/raft/node.go](../internal/raft/node.go) | `maybeSendTimeoutNow` |
| Handover step 4: "Target wins a normal vote" | [internal/raft/node.go](../internal/raft/node.go) | `campaign(transfer=true)`: the same election as usual, just started immediately |
| What connects predictor to Raft | [internal/server/server.go](../internal/server/server.go) | `evaluateMonitor` asks Raft to transfer to the follower the predictor picked |

---

## Slide 8: Experiments on real processes

**In one line:** the lab harness starts real clusters, injects faults on the network, and records data for training and benchmarks.

| Point on the slide | File | What it does |
|---|---|---|
| "5 separate node processes" | [internal/lab/cluster.go](../internal/lab/cluster.go) | `StartCluster`, `StartNode`, `Kill` |
| "12 fault scenarios" | [internal/lab/scenario.go](../internal/lab/scenario.go) | Each scenario by name: `degrade-delay`, `degrade-jitter`, `degrade-loss`, `degrade-mixed`, `hard-kill`, `healthy`, `blip`, `steady-slow`, `steady-slow-high`, `one-bad-link`, `follower-kill`, `two-follower-kill` (plus three unseen ones used only in testing) |
| "Injected on the network" | [internal/transport/peers.go](../internal/transport/peers.go) | `SetChaos`: adds delay, jitter or packet loss to a node's messages |
| | [internal/lab/cluster.go](../internal/lab/cluster.go) | `Chaos`: tells a running node to start that fault |
| "144 training runs, 13,789 readings" | [internal/lab/runner.go](../internal/lab/runner.go) | `Collect`: runs a scenario and saves the leader's readings |
| | [cmd/sentinel-lab/main.go](../cmd/sentinel-lab/main.go) | `collect`: the command that runs all of them |
| | [internal/predict/monitor.go](../internal/predict/monitor.go) | `writeTelemetry`: the leader writes one row per reading |
| "288 benchmark trials, paired" | [internal/lab/runner.go](../internal/lab/runner.go) | `Bench`: runs one trial in one mode and measures the outage (`analyze`) |
| | [cmd/sentinel-lab/main.go](../cmd/sentinel-lab/main.go) | `bench`: runs each scenario in plain and prediction mode |
| The collected data | `ml/data/` | `runs.csv` (one line per run) and `telemetry.csv` (one line per reading) |

---

## Slide 9: Where the build changed from the plan

**In one line:** five design changes. Each row points to the code that implements the new choice.

| Change (what we built) | File | What it does |
|---|---|---|
| Own message protocol over TCP + TLS 1.3, standard library only | [internal/protocol/protocol.go](../internal/protocol/protocol.go) | The client message format (JSON, one per line) |
| | [internal/transport/peers.go](../internal/transport/peers.go) | Node-to-node messages over TLS |
| | [go.mod](../go.mod) | Lists no third-party modules |
| Train on gradual degradation; sudden kills labelled healthy | [ml/train.py](../ml/train.py) | `label`: decides which readings count as "about to fail" |
| | [internal/lab/scenario.go](../internal/lab/scenario.go) | The `degrade-*` ramps, and `hard-kill` |
| At most one leader per term, checked exhaustively | [internal/modelcheck/check.go](../internal/modelcheck/check.go) | `checkElectionSafety` |
| Reads confirm leadership first (ReadIndex) | [internal/raft/node.go](../internal/raft/node.go) | `ReadIndex` |
| | [internal/server/server.go](../internal/server/server.go) | `ReadBarrier`: a SELECT waits for this before answering |
| Pick the model per run, not by F1 | [ml/train.py](../ml/train.py) | `event_metrics`, `rule_score`, `tune_rule` |

---

## Slide 10: Software and tools used

**In one line:** the eight tools, and where each one shows up.

| # | Tool | Where | What it does |
|---|---|---|---|
| 1 | Go 1.23 | everything under [cmd/](../cmd) and [internal/](../internal) | All five programs |
| 2 | Python + scikit-learn | [ml/train.py](../ml/train.py) | Trains and compares the four classifiers |
| | pandas + matplotlib | [scripts/evaluate.py](../scripts/evaluate.py) | Turns benchmark results into the report and charts |
| 3 | TLS 1.3 + tokens | [internal/transport/tls.go](../internal/transport/tls.go) | TLS settings, certificates, and checking tokens (`Tokens.Check`) |
| | | [cmd/sentinel-certs/main.go](../cmd/sentinel-certs/main.go) | Creates the certificates and the read / write / admin tokens |
| | | [internal/server/conn.go](../internal/server/conn.go) | `requiredRole`: which token a statement needs |
| 4 | Write-ahead log | [internal/wal/wal.go](../internal/wal/wal.go) | Checksummed (CRC32C) log on disk, synced on every write; snapshots |
| 5 | Own model checker | [internal/modelcheck/](../internal/modelcheck), [cmd/sentinel-check/main.go](../cmd/sentinel-check/main.go) | The exhaustive safety check, and the command that runs it |
| 6 | Lab harness | [internal/lab/](../internal/lab), [cmd/sentinel-lab/main.go](../cmd/sentinel-lab/main.go) | Runs real clusters, injects faults, kills nodes |
| 7 | Web dashboard | [cmd/sentinel-lab/dashboard.html](../cmd/sentinel-lab/dashboard.html), [cmd/sentinel-lab/demo.go](../cmd/sentinel-lab/demo.go) | The live demo page, and the server behind it |
| 8 | Docker | no file in the repo | Only used to run Go's race detector. The command is in [docs/verification.md](verification.md) |

---

## Slide 11: Consensus techniques (Raft)

**In one line:** seven Raft techniques. Almost all are in one file, `internal/raft/node.go`.

| Technique on the slide | File | Function |
|---|---|---|
| Randomised election timeouts (500–1000 ms) | [internal/raft/node.go](../internal/raft/node.go) | `resetElectionTimer`: base timeout plus a random extra of up to the same again |
| | [cmd/sentinel-node/main.go](../cmd/sentinel-node/main.go) | `-election-ticks 50` × 10 ms ticks = 500 ms base, so 500–1000 ms |
| Majority commit (3 of 5) | [internal/raft/node.go](../internal/raft/node.go) | `quorum`, `maybeCommit` |
| Leader stickiness + check-quorum | [internal/raft/node.go](../internal/raft/node.go) | `handleVote` refuses to vote while the node still knows a current leader; `checkQuorum` makes a cut-off leader step down |
| Leadership transfer (TimeoutNow) | [internal/raft/node.go](../internal/raft/node.go) | `TransferLeadership`, `maybeSendTimeoutNow` |
| ReadIndex | [internal/raft/node.go](../internal/raft/node.go) | `ReadIndex`, `releaseReads` |
| | [internal/server/server.go](../internal/server/server.go) | `ReadBarrier`, `handleReads` |
| Snapshots every 10,000 entries | [internal/server/server.go](../internal/server/server.go) | `SnapshotEvery = 10000`, `takeSnapshot` |
| | [internal/raft/node.go](../internal/raft/node.go) | `Compact` (drop old log), `handleSnapshot` (a lagging follower receives one) |
| | [internal/engine/catalog.go](../internal/engine/catalog.go) | `Snapshot` / `Restore`: save and reload the tables |
| Group commit (256 writes, one sync) | [internal/server/server.go](../internal/server/server.go) | `handleProposals` gathers up to 256 writes |
| | [internal/raft/node.go](../internal/raft/node.go) | `ProposeBatch` |
| "Any two majorities share a node" (the picture) | [internal/raft/node.go](../internal/raft/node.go) | The reason `quorum()` is `n/2 + 1` |

---

## Slide 12: Database techniques

**In one line:** grammar → parse tree → relational algebra → executor, with locking, request IDs and EXPLAIN.

| Point on the slide | File | What it does |
|---|---|---|
| Context-free grammar | [docs/grammar.md](grammar.md) | The grammar rules, written out |
| Recursive descent, one method per rule | [internal/sql/lexer.go](../internal/sql/lexer.go) | Splits text into tokens (keywords, names, numbers) |
| | [internal/sql/parser.go](../internal/sql/parser.go) | One function per grammar rule; builds the parse tree |
| Relational algebra (σ, π, ⋈) | [internal/sql/translate.go](../internal/sql/translate.go) | Turns the parse tree into algebra |
| | [internal/sql/algebra.go](../internal/sql/algebra.go) | The algebra operators and expression evaluation |
| | [internal/sql/value.go](../internal/sql/value.go) | Values, comparisons, arithmetic |
| Executor over in-memory tables | [internal/engine/exec.go](../internal/engine/exec.go) | Runs the algebra: scans, filters, joins, sorting |
| | [internal/engine/catalog.go](../internal/engine/catalog.go) | The tables themselves |
| Primary and foreign keys | [internal/engine/exec.go](../internal/engine/exec.go) | `execInsert` (duplicate key check), `checkForeignKeys`, `ensureNoChildren` |
| Strict two-phase locking | [internal/txn/locks.go](../internal/txn/locks.go) | `Acquire`, `ReleaseAll`; wait-die prevents deadlocks |
| Serializability via precedence graph | [internal/txn/serial.go](../internal/txn/serial.go) | `CheckConflictSerializable`, `findCycle` |
| Request IDs (exactly once) | [internal/client/client.go](../internal/client/client.go), [internal/engine/catalog.go](../internal/engine/catalog.go) | `newRequestID`, `seen` |
| EXPLAIN (the screenshot) | [internal/engine/exec.go](../internal/engine/exec.go) | `explain`: prints the parse tree and the algebra |
| The sample tables used in examples | [docs/schema.sql](schema.sql), [docs/schema.md](schema.md) | customers, accounts, products, orders, order_items |

---

## Slide 13: Verification techniques

**In one line:** model checking (the design), mutation testing (do the checks work?), and randomised simulation (the real code).

| Box on the slide | File | What it does |
|---|---|---|
| 1. Exhaustive model checking | [internal/modelcheck/model.go](../internal/modelcheck/model.go) | The model: each node's state, the events, the network |
| | [internal/modelcheck/check.go](../internal/modelcheck/check.go) | `Check`: visits every reachable state; `Invariants`: the four safety rules |
| | [cmd/sentinel-check/main.go](../cmd/sentinel-check/main.go) | The command (`sentinel-check -suite`) |
| 2. Mutation testing, 8 model bugs | [internal/modelcheck/model.go](../internal/modelcheck/model.go) | The `Bug` switches (`ignore-vote`, `commit-old-term`, …) |
| | [internal/modelcheck/check_test.go](../internal/modelcheck/check_test.go) | `TestCheckerFindsEveryInjectedBug` |
| 2. Mutation testing, 14 code bugs | [scripts/mutation_check.py](../scripts/mutation_check.py) | Puts each bug into a copy of the Raft code and runs the tests |
| 3. Randomised simulation | [internal/raft/sim_test.go](../internal/raft/sim_test.go) | `TestSimulation`: 8 scenarios × 40 seeds |
| Scripted Raft tests (incl. Figure 8) | [internal/raft/node_test.go](../internal/raft/node_test.go) | Specific cases the random runs can't reliably hit |
| "Predictor fires at arbitrary moments" | [internal/modelcheck/model.go](../internal/modelcheck/model.go) | The `timeout-now` event: any leader may hand off to anyone, any time |

The full explanation is in [verification-explained.md](verification-explained.md).

---

## Slide 14: Machine learning: the failure predictor

**In one line:** heartbeats → 6 numbers → random forest → "sure enough?" → hand over. Trained in Python, run in Go.

| Box on the slide | File | What it does |
|---|---|---|
| "Heartbeats … each reply is timed" | [internal/server/server.go](../internal/server/server.go) | `process` and `step` tell the monitor when a heartbeat was sent and answered |
| | [internal/predict/monitor.go](../internal/predict/monitor.go) | `HeartbeatSent`, `HeartbeatAcked` |
| "Last 2 seconds" | [internal/predict/features.go](../internal/predict/features.go) | `peerWindow`: keeps each follower's recent heartbeats |
| "6 numbers" (the feature table) | [internal/predict/features.go](../internal/predict/features.go) | `peerFeatures`: computes the 6 numbers; `quorumFeatures`: reads them at the follower the majority depends on |
| "Random forest → chance of failing" | [internal/predict/model.go](../internal/predict/model.go) | `LoadModel` reads the JSON; `Probability` walks the trees and averages them |
| | [ml/models/deployed.json](../ml/models/deployed.json) | The trained random forest, plus its threshold (0.75) and count (10) |
| "Sure enough? ≥ 0.75 for 10 checks" | [internal/predict/monitor.go](../internal/predict/monitor.go) | `Evaluate` |
| "Hand over to the follower that is caught up and fastest" | [internal/predict/monitor.go](../internal/predict/monitor.go) | `chooseTarget` |
| "Not in the first 2.5 s, not within 5 s of the last handover" | [internal/predict/monitor.go](../internal/predict/monitor.go) | `warmup` and `cooldown` |
| "4 models" | [ml/train.py](../ml/train.py) | `candidates` |
| "Split by whole run" | [ml/train.py](../ml/train.py) | `split_runs` |
| "Chosen by failures caught and false alarms per run" | [ml/train.py](../ml/train.py) | `event_metrics`, `rule_score`, `tune_rule` |
| "Exported to JSON … matches scikit-learn" | [ml/train.py](../ml/train.py) | `export_model` (also saves scikit-learn's own answers) |
| | [internal/predict/predict_test.go](../internal/predict/predict_test.go) | `TestExportedModelsMatchScikitLearn` |
| How a node is told to use the model | [cmd/sentinel-node/main.go](../cmd/sentinel-node/main.go), [cmd/sentinel-node/monitor.go](../cmd/sentinel-node/monitor.go) | The `-predict` flag loads a model file. Without it, the node is plain Raft |

---

## Slide 15: Outage when the leader fails

**In one line:** the benchmark results: 910 → 196 ms, 40/40 handed over early, 0/70 false handovers.

| File | What it does |
|---|---|
| [internal/lab/runner.go](../internal/lab/runner.go) | `Bench` runs one trial; `StartProber` writes continuously; `analyze` measures the longest gap with no completed write |
| [internal/lab/scenario.go](../internal/lab/scenario.go) | The faults used (growing delay, jitter, loss, mixed, sudden kill, and the healthy ones) |
| [cmd/sentinel-lab/main.go](../cmd/sentinel-lab/main.go) | `bench`: runs plain and prediction mode on the same faults |
| [scripts/evaluate.py](../scripts/evaluate.py) | Medians, confidence intervals, sign tests, the bar chart (`fig_outage`) |
| [results/evaluation.md](../results/evaluation.md) | The written report with every number on this slide |
| [results/outage-by-scenario.png](../results/outage-by-scenario.png) | The chart |

---

## Slide 16: Live demo screenshots

**In one line:** the browser dashboard, showing the same fault in both modes.

| File | What it does |
|---|---|
| [cmd/sentinel-lab/dashboard.html](../cmd/sentinel-lab/dashboard.html) | The web page: latency chart, outage bands, the predictor's probability, buttons |
| [cmd/sentinel-lab/demo.go](../cmd/sentinel-lab/demo.go) | The server behind the page: `start` (a cluster in either mode), `action` (button presses), `degradeThenKill`, `state` (the data the page shows) |
| [Sentinel DB/9 - How to Give the Demo.md](<../Sentinel DB/9 - How to Give the Demo.md>) | Step-by-step script for giving the demo |

Run it with `bin\sentinel-lab demo`, then open http://127.0.0.1:8088.

---

## Slide 17: Write latency during one recorded trial

**In one line:** one trial drawn over time (gaps of 872 ms vs 207 ms), plus the slowest-1% table.

| File | What it does |
|---|---|
| [cmd/sentinel-lab/main.go](../cmd/sentinel-lab/main.go) | `writeTimeline`: saves every write's latency during a trial |
| `results/timelines/` | Those saved timelines |
| [scripts/evaluate.py](../scripts/evaluate.py) | `fig_walkthrough` draws the chart; the p99 ("slowest 1%") table is in `report` |
| [results/walkthrough.png](../results/walkthrough.png) | The chart on the slide |
| [results/evaluation.md](../results/evaluation.md) | The tail-latency table |

---

## Slide 18: Safety verification results

**In one line:** the state counts, 8/8 model bugs, 14/14 code bugs, and Figure 8.

| Number on the slide | File | What it does |
|---|---|---|
| The table (states, transitions, 0 violations) | [cmd/sentinel-check/main.go](../cmd/sentinel-check/main.go) | `runSuite` (`sentinel-check -suite`): runs all the bounded models and prints the report |
| | [results/verification.md](../results/verification.md) | The report the table is copied from |
| 8 / 8 model bugs | [internal/modelcheck/check_test.go](../internal/modelcheck/check_test.go) | `TestCheckerFindsEveryInjectedBug` |
| 14 / 14 code bugs | [scripts/mutation_check.py](../scripts/mutation_check.py) | The mutation script |
| 18 steps (Figure 8) | [internal/modelcheck/model.go](../internal/modelcheck/model.go) | The `commit-old-term` bug |
| | [internal/raft/node.go](../internal/raft/node.go) | `maybeCommit`: the real rule that prevents it (only count copies of current-term entries) |
| | [internal/raft/node_test.go](../internal/raft/node_test.go) | `TestFigure8`: the same scenario against the real code |

---

## Slide 19: Choosing the model

**In one line:** the four models compared per reading and per run; the random forest was chosen.

| File | What it does |
|---|---|
| [ml/train.py](../ml/train.py) | Everything on the slide: `label`, `split_runs` (86 / 29 / 29), `candidates`, `event_metrics` (per-run columns), the per-reading scores, choosing the winner |
| [results/ml-comparison.md](../results/ml-comparison.md) | The report the table is copied from (also the decision tree as readable rules, and feature importances) |
| `ml/data/` | The 144 runs and 13,789 readings |
| [ml/models/](../ml/models) | All four trained models as JSON; `deployed.json` is the random forest |

---

## Slide 20: The SQL layer on the live cluster

**In one line:** a real `sentinel-cli` session: a join, a transaction, and the serializability check.

| Point on the slide | File | What it does |
|---|---|---|
| The shell in the screenshot | [cmd/sentinel-cli/main.go](../cmd/sentinel-cli/main.go) | The SQL shell: reads a line, sends it, prints a table |
| Joins through any node | [internal/engine/exec.go](../internal/engine/exec.go) | Join execution |
| | [internal/server/server.go](../internal/server/server.go) | `ReadBarrier`: the answer is the leader's committed data |
| Moving 25.00 as one transaction | [internal/client/client.go](../internal/client/client.go) | `Begin`, `Tx.Exec`, `Commit` |
| | [internal/txn/locks.go](../internal/txn/locks.go) | Locks held until commit |
| `\schedule` command | [cmd/sentinel-cli/schedule.go](../cmd/sentinel-cli/schedule.go) | Prints the result of the serializability test |
| | [internal/txn/serial.go](../internal/txn/serial.go) | `ParseSchedule`, `CheckConflictSerializable` |
| Failover tests (no lost or doubled writes) | [internal/server/cluster_test.go](../internal/server/cluster_test.go) | Real multi-node clusters: kills the leader repeatedly and checks every acknowledged write |
| The tables used | [docs/schema.sql](schema.sql) | customers, accounts, products, orders, order_items |

---

## Slide 21: Conclusion and next steps

No new code. Where the claims come from:

| Claim | Source |
|---|---|
| 910 → 196 ms, 0 false handovers in 70 | [results/evaluation.md](../results/evaluation.md) |
| Unseen slow ramp: 4 of 8 handed over | [results/evaluation.md](../results/evaluation.md) ("Outside the training data"); scenarios `slow-ramp`, `fast-ramp`, `ramp-recover` in [internal/lab/scenario.go](../internal/lab/scenario.go) |
| Limits of each verification claim | [docs/verification.md](verification.md) |

## Slide 22: References

No code. Background on the papers is in [Sentinel DB/2 - Why We Built This (Inspiration and Related Papers).md](<../Sentinel DB/2 - Why We Built This (Inspiration and Related Papers).md>).

## Slide 23: Closing slide

No code.

---

## Quick reference: one line per folder

| Folder | In plain words |
|---|---|
| `cmd/sentinel-node` | The database node program |
| `cmd/sentinel-cli` | The SQL shell |
| `cmd/sentinel-lab` | Runs clusters, experiments and the web demo |
| `cmd/sentinel-check` | Runs the model checker |
| `cmd/sentinel-certs` | Makes certificates and tokens |
| `internal/raft` | Raft: elections, log copying, committing |
| `internal/wal` | Saving the log to disk safely |
| `internal/sql` | Understanding SQL text |
| `internal/engine` | Tables, and running queries on them |
| `internal/txn` | Locks and the serializability check |
| `internal/server` | Joins everything into one running node |
| `internal/transport` | Network and TLS between nodes |
| `internal/protocol` | The message format between client and node |
| `internal/client` | How programs talk to the database |
| `internal/predict` | The failure predictor, at runtime |
| `internal/modelcheck` | The exhaustive safety check |
| `internal/lab` | Fault scenarios and experiment runs |
| `ml/` | Training the models (Python), the data, the trained models |
| `scripts/` | The report generator and the mutation check |
| `results/` | Measured results and charts |
| `docs/` | Explanations, grammar, schema, and this file |
