# Code Guide: What Handles What

This is a map of the repo: what each part does, and where to look when you need to find or explain something. The ideas behind each part are explained in the notes in `Sentinel DB/`.

## The big picture

The system has four layers, plus the programs that run them:

| Layer | What it does | Code |
|---|---|---|
| **Query** | Understands SQL, stores tables, runs transactions | `internal/sql`, `internal/engine`, `internal/txn` |
| **Consensus** | Keeps all copies of the data in agreement and safe on disk | `internal/raft`, `internal/wal` |
| **Prediction** | Spots a struggling leader and asks it to hand over | `internal/predict`, `ml/train.py` |
| **Verification** | Checks every reachable state of a model of the protocol, offline | `internal/modelcheck` |

`internal/server` joins the query, consensus and prediction layers into one running node. `internal/transport` carries traffic between nodes, and `internal/client` is how programs talk to the database.

## What happens to a write

Following one `INSERT` through the code is the quickest way to see how the parts fit together:

| Step | What happens | Where |
|---|---|---|
| 1 | You type a statement in the shell | `cmd/sentinel-cli/main.go` |
| 2 | The client library sends it to a node over TLS, retrying if needed | `internal/client/client.go` |
| 3 | The node checks your token and reads the request | `internal/server/conn.go` |
| 4 | If this node isn't the leader, it forwards the request to the one that is | `relayRequest` in `conn.go` |
| 5 | The SQL text becomes a parse tree, then a statement or relational algebra | `internal/sql` |
| 6 | The executor takes locks, checks constraints and works out the row changes | `internal/engine/exec.go`, `internal/txn/locks.go` |
| 7 | The changes are proposed as one entry in the Raft log | `Commit` in `internal/server/server.go` |
| 8 | Raft saves the entry to disk and sends it to the followers | `internal/raft/node.go`, `internal/wal/wal.go`, `internal/transport/peers.go` |
| 9 | Once a majority has it, the entry counts as committed | `maybeCommit` in `node.go` |
| 10 | Every node applies the change to its own tables | `applyEntry` in `server.go`, `Apply` in `internal/engine/catalog.go` |
| 11 | The leader releases the locks and answers the client | `exec.go`, `conn.go` |

A `SELECT` follows steps 1 to 6 and then skips the log. The leader instead confirms with a majority that it's still the leader (`ReadBarrier` in `server.go`) and answers from its own copy.

## What happens during a proactive handover

| Step | What happens | Where |
|---|---|---|
| 1 | The leader times each heartbeat's round trip | `process` and `step` in `server.go` |
| 2 | Those timings become six signals, ten times a second | `internal/predict/features.go` |
| 3 | The trained model turns the signals into a probability | `internal/predict/model.go` |
| 4 | After 10 high readings in a row, the monitor picks a healthy follower | `Evaluate` in `internal/predict/monitor.go` |
| 5 | The server asks Raft to transfer leadership | `evaluateMonitor` in `server.go` |
| 6 | The leader stops taking writes, gets the follower up to date, and tells it to start an election | `TransferLeadership` in `node.go` |
| 7 | The follower wins an ordinary vote and takes over | `campaign` in `node.go` |

## The packages

### `internal/raft`: the consensus protocol

Raft as a pure state machine. It has no clock, threads or network of its own, so tests can run it under simulated chaos and replay any failure exactly.

| File | What it handles |
|---|---|
| `types.go` | The shared types: log entries, messages, roles, membership and the storage interface |
| `node.go` | The protocol itself: elections, voting, replication, committing, snapshots, leadership transfer, membership changes and leadership checks for reads (ReadIndex) |
| `sim_test.go` | Randomized runs with crashes, partitions and message loss, checking Raft's safety rules |
| `node_test.go` | Tests of specific scenarios, including Figure 8 from the Raft paper |

### `internal/wal`: storage on disk

| File | What it handles |
|---|---|
| `wal.go` | The checksummed log and snapshot files, crash recovery, and telling a crash apart from corruption |
| `wal_test.go` | Cuts the log at every byte and flips bits to check recovery and corruption detection |

### `internal/sql`: the language

This package only understands queries. It holds no data and takes no locks.

| File | What it handles |
|---|---|
| `lexer.go` | Splits text into tokens: keywords, names, numbers, strings |
| `parser.go` | Checks tokens against the grammar and builds the parse tree, with one function per grammar rule |
| `translate.go` | Turns the parse tree into a statement |
| `algebra.go` | Expressions, their evaluation (including NULL logic), and the relational algebra operators |
| `value.go` | Data values, comparisons, overflow-safe arithmetic and primary-key encoding |

The grammar itself is written out in `docs/grammar.md`.

### `internal/engine`: tables and execution

| File | What it handles |
|---|---|
| `catalog.go` | The tables in memory, applying committed changes, snapshots, and spotting retried requests so they don't apply twice |
| `exec.go` | Running statements: scans, filters, joins and sorting, inserts, updates and deletes, constraints, transactions, `EXPLAIN` and stale reads |

### `internal/txn`: transactions

| File | What it handles |
|---|---|
| `locks.go` | Two-phase locking at row and table level, and preventing deadlocks with wait-die |
| `serial.go` | The conflict-serializability test: records a schedule, builds the precedence graph and looks for cycles |

### `internal/server`: a running node

This is where everything joins up.

| File | What it handles |
|---|---|
| `server.go` | Startup and recovery, and the main loop, the only code that touches Raft: timers, messages, proposals, applying committed entries, and triggering the predictor |
| `conn.go` | Accepting connections, checking tokens, client sessions, forwarding to the leader, turning errors into codes clients can act on, and admin commands |
| `cluster_test.go` | Real multi-node clusters in one process: failover, data survival, forwarding, membership and attempted security attacks |

### `internal/transport`: network and security

| File | What it handles |
|---|---|
| `tls.go` | TLS settings for each kind of connection, generating certificates, and checking tokens |
| `peers.go` | Sending messages between nodes, reconnecting, verifying who sent a message, and injecting faults for experiments |

### `internal/protocol`: the client message format

`protocol.go` defines requests, responses, error codes, roles and node status. Clients and servers both use it. Messages are JSON, one per line.

### `internal/client`: the client library

`client.go` finds a working node, follows leadership changes, and retries writes safely using request IDs. It also handles explicit transactions.

### `internal/predict`: failure prediction at runtime

| File | What it handles |
|---|---|
| `features.go` | Turns heartbeat timings into the six signals, read at the position the leader's majority depends on |
| `model.go` | Loads a trained model from JSON, and calculates its probability for all four model types |
| `monitor.go` | The decision: warm-up period, readings in a row, cooldown, choosing a target, and writing telemetry |

### `internal/modelcheck`: the safety check

| File | What it handles |
|---|---|
| `model.go` | The per-node automaton, the network, and the bugs that can be injected on purpose |
| `check.go` | The exhaustive search, the four safety rules, counterexample traces and the generated automaton diagram |

### `internal/lab`: experiments

| File | What it handles |
|---|---|
| `cluster.go` | Starting, stopping and killing real node processes |
| `scenario.go` | The fault scenarios, such as growing delay, sudden kill and steady slow network |
| `runner.go` | Running a trial, measuring write outages and recording training data |

## The programs

These are built into `bin\` by `go build -o bin/ ./cmd/...`.

| Program | What it does | Code |
|---|---|---|
| `sentinel-node` | Runs one database node | `cmd/sentinel-node` |
| `sentinel-cli` | The SQL shell | `cmd/sentinel-cli` |
| `sentinel-lab` | Runs a local cluster (`serve`), the website (`demo`), data collection (`collect`) and experiments (`bench`) | `cmd/sentinel-lab` |
| `sentinel-check` | Runs the model checker and the schedule test | `cmd/sentinel-check` |
| `sentinel-certs` | Creates certificates and tokens | `cmd/sentinel-certs` |

The website is `cmd/sentinel-lab/dashboard.html`, served by `cmd/sentinel-lab/demo.go`. Everything on the page comes from `/api/state`, which reads from the running nodes.

## Running the website demo

Run these in PowerShell from the project folder.

**Build the programs.** Do this once, and again after any code change:

```powershell
go build -o bin/ ./cmd/...
```

**Start the demo.** Leave it running, then open http://127.0.0.1:8088. Each start creates a fresh cluster with its own data, so there's nothing to clean up between runs. (This is separate from `bin\sentinel-lab serve`, which keeps its data in the `data` folder until you delete it.)

```powershell
bin\sentinel-lab demo
```

**Then use the buttons on the page.** Wait about 6 seconds after pressing a mode button, so the predictor can warm up:

| Press | What to look for |
|---|---|
| **Plain Raft**, then **Degrade the leader, then kill it** | Latency climbs, the node dies, and a red outage band appears |
| **With prediction**, then **Degrade the leader, then kill it** | Leadership moves before the node dies, usually with no red band |
| **Kill the leader now** | A real outage, because there was no warning to act on |
| **Restart dead nodes** | The dead node comes back |

**Other useful commands:**

```powershell
# Start directly in prediction mode
bin\sentinel-lab demo -mode proactive

# Use a different port if 8088 is taken, then open that address instead
bin\sentinel-lab demo -addr 127.0.0.1:8099

# Count the nodes running, as reported by Windows
@(Get-Process sentinel-node).Count

# Count the nodes running with the prediction model: 0 in Plain Raft, 5 in With prediction
@(Get-CimInstance Win32_Process -Filter "Name='sentinel-node.exe'" | Where-Object { $_.CommandLine -like '*-predict *' }).Count

# Kill a node from outside the website. Replace n1 with the leader shown on the page
Get-CimInstance Win32_Process -Filter "Name='sentinel-node.exe'" | Where-Object { $_.CommandLine -like '*-id n1 *' } | ForEach-Object { Stop-Process -Id $_.ProcessId -Force }
```

**Stop the demo** by pressing Ctrl-C in its terminal. If anything is left running or stuck:

```powershell
Get-Process sentinel-node,sentinel-lab -ErrorAction SilentlyContinue | Stop-Process -Force
```

## Everything that isn't Go

| Path | What it is |
|---|---|
| `ml/train.py` | Labels the data, trains and compares the four models, and exports them for Go |
| `ml/data/` | The collected training data |
| `ml/models/` | The trained models. `deployed.json` is the one the system uses |
| `scripts/evaluate.py` | Turns benchmark results into the report and figures |
| `scripts/mutation_check.py` | Injects bugs into the Raft code to confirm the tests catch them |
| `results/` | The measured results and reports |
| `docs/` | The grammar, schema design, verification limits, Check 1 steps and this guide |
| `Sentinel DB/` | The project notes |

## Where do I look if...

| You want to... | Look at |
|---|---|
| Understand or change how elections work | `campaign` and `handleVote` in `internal/raft/node.go` |
| Change election or heartbeat timing | Flags in `cmd/sentinel-node/main.go` |
| Add SQL syntax | `lexer.go`, `parser.go` and `translate.go` in `internal/sql`, then `exec.go`, and update `docs/grammar.md` |
| Change how a query runs | `internal/engine/exec.go` |
| Change what the predictor measures | `internal/predict/features.go`, then collect new data and retrain |
| Change when a handover fires | `threshold` and `consecutive` in `ml/models/deployed.json`, set by `ml/train.py` |
| Add a fault scenario | `internal/lab/scenario.go` |
| Change the website | `cmd/sentinel-lab/dashboard.html`, then rebuild, because it's built into the program |
| Add a token or permission rule | `requiredRole` and `handleAdmin` in `internal/server/conn.go` |
| Check a safety claim | `Invariants` in `internal/modelcheck/check.go` |

## Tests

Most packages have `_test.go` files next to the code. Run them all with:

```powershell
go test ./...
```

This takes about 3 minutes. `docs/verification.md` explains what the tests do and don't prove.
