---
tags: [sentinel-db, demo]
aliases: [SentinelDB Demo, SentinelDB Demo Script]
created: 2026-11-04
updated: 2026-11-04
---

# How to Give the Demo

Back to [[1 - Overview|Overview]]. This is the script for showing SentinelDB working live, in about 8 minutes. The commands here were rehearsed as written, and the live numbers come from repeated runs.

## Before you start

Run this once from the project folder, in PowerShell. It needs Go, but no Python.

```powershell
go build -o bin/ ./cmd/...
Remove-Item -Recurse -Force data -ErrorAction SilentlyContinue
```

Clearing `data` matters whenever you rehearse: otherwise the sample tables already exist and loading them again prints errors.

- Open three PowerShell windows in the project folder, with large text.
- **Open the browser before the demo starts, and close anything heavy.** Launching a browser put a visible spike on the latency graph in rehearsal. The nodes all share your laptop, so anything else it's doing shows up on the graph.
- Keep `results/walkthrough.png` open as a fallback.

## Act 1: it's a real database, and it survives failure (3 minutes)

**Window 1**, leave this running:

```powershell
bin\sentinel-lab serve
```

**Window 2:**

```powershell
Get-Content docs\schema.sql | bin\sentinel-cli
bin\sentinel-cli
```

Then, in the shell:

```sql
SELECT o.id, c.name FROM orders o JOIN customers c ON o.customer_id = c.id WHERE c.id = 42;
```

Show that it's a real query engine, not a disguised if-statement. Scroll to the bottom of the output for the relational algebra:

```sql
EXPLAIN SELECT id FROM orders WHERE customer_id = 42;
```

Show the grammar refusing things:

```sql
SELECT * FROM orders WHERE id < 5 < 9;
SELECT * FROM orders; DROP TABLE orders;
```

Show a transaction, then kill the leader. First find out who it is:

```sql
BEGIN;
UPDATE accounts SET balance_cents = balance_cents - 2500 WHERE id = 1;
UPDATE accounts SET balance_cents = balance_cents + 2500 WHERE id = 2;
COMMIT;
\status
```

Read the `"leader"` line. In **Window 3**, kill that node, replacing `n1` with the leader's name. The spaces inside `*-id n1 *` matter.

```powershell
Get-CimInstance Win32_Process -Filter "Name='sentinel-node.exe'" | Where-Object { $_.CommandLine -like '*-id n1 *' } | ForEach-Object { Stop-Process -Id $_.ProcessId -Force }
```

Back in the shell, the data is still there and writes still work:

```sql
SELECT id, balance_cents FROM accounts;
INSERT INTO customers VALUES (7, 'Written after the failure', NULL);
\status
```

**Say:** "That process is really dead. A new leader was elected by itself, and nothing committed was lost."

Then quit with `\q` and press **Ctrl-C** in Window 1. It stops the remaining nodes.

## Act 2: checking it can't break (2 minutes)

**Window 2:**

```powershell
bin\sentinel-check -nodes 3 -terms 3 -log 1
```

This takes about 2 seconds. **Say:** "That's every reachable state of this model, not a sample, with leaders handing over at random moments, however wrongly."

Then show that it can fail, by breaking a rule on purpose. This takes about 5 seconds, and finds the bug from Figure 8 of the Raft paper by itself:

```powershell
bin\sentinel-check -nodes 3 -terms 4 -log 1 -transfers=false -restarts=false -bug commit-old-term
```

**Say:** "Those are the exact 18 steps that lose committed data if you get this one rule wrong." Optionally, show the state a sharp judge will ask about:

```powershell
bin\sentinel-check -nodes 3 -terms 2 -log 0 -two-leaders
```

## Act 3: the pitch, on one graph (3 minutes)

**Window 1:**

```powershell
bin\sentinel-lab demo
```

Open **http://127.0.0.1:8088**. It starts in **Plain Raft** mode. **Wait about 6 seconds after every mode change** before pressing anything, as the predictor needs that long to warm up.

1. Under **Plain Raft**, press **Degrade the leader, then kill it**. Latency climbs for 8 seconds, the node dies, and a red outage band appears. Read out **Longest outage in view** (800 to 1000 ms in rehearsal).
2. Press **With prediction**, wait 6 seconds, and press the same button. Watch the probability line cross the threshold 2 to 3 seconds in. Leadership moves, latency drops back, and the node's death later goes unnoticed. **Longest outage in view** will usually say **none**, because the dashboard only counts a gap of 250 ms or more as an outage, and gaps here measured 210 to 250 ms in rehearsal. If it shows a number, it will be close to 250 ms. The events list shows the order: the new leader appears *before* the old node died.
3. Be honest about the limit. Press **Kill the leader now**. There's a real outage (about 600 to 900 ms), because there was nothing to predict. **Say:** "It only helps when there's warning. For a sudden death, plain Raft is still the safety net."

Switching mode starts a fresh cluster, so the two runs can't share one live graph. Keep the first number in mind, or show `results/walkthrough.png`, which has both on one axis.

Press **Ctrl-C** in Window 1 when you're done.

## Questions to expect

**"Can there ever be two leaders?"** Not in the same term, which is what safety requires. An old leader can briefly still believe it leads, in an older term, but it can't commit anything, because a majority has already moved on. Run `-two-leaders` to show it.

**"Did you prove your code is correct?"** No. We exhaustively checked a bounded model of the design. The real code has 320 randomized simulated runs on top, and 14 bugs injected to check those tests can fail. That's strong evidence, not a proof.

**"Why not just use a shorter timeout?"** On a real network that causes needless elections. The timeout has to allow for normal delay, and the predictor doesn't wait on it.

**"What if the model is wrong?"** The worst case is a needless handover, which costs little: when a node recovered, handing over still left a shorter gap than staying (206 ms against 376 ms). Safety doesn't depend on the model at all, because the safety check lets it fire at any moment.

**"Is this accurate on real failures?"** We don't know. The faults are injected, and real hardware may degrade differently, or not gradually at all. It also missed half of a ramp far slower than its training data. What's shown is that the mechanism works when warning signs exist, not how often they do.

**"Is zero false alarms believable?"** Zero in 70 trials means below about 4% of runs with 95% confidence, not zero. The first version had a great F1 of 0.976 and still raised false alarms on every steady slow network in development, which is why we tuned for per-run outcomes instead.

**"Is it deep learning?"** No, deliberately. The deployed model is a Random Forest, and the Decision Tree that came second can be read out as rules.

**"Does it run on separate machines?"** It's built for that, since nodes only talk over the network, but everything measured here ran on one machine. So compare the two modes, rather than quoting absolute latencies.

## If something goes wrong

**Reset everything:**

```powershell
Get-Process sentinel-node,sentinel-lab -ErrorAction SilentlyContinue | Stop-Process -Force
Remove-Item -Recurse -Force data -ErrorAction SilentlyContinue
```

| What you see | Do this |
|---|---|
| `table customers already exists` when loading the schema | Harmless leftovers from a rehearsal. Reset as above |
| The dashboard says the port is in use | Reset as above, or run `bin\sentinel-lab demo -addr 127.0.0.1:8099` |
| **With prediction** is greyed out | Run from the project folder: it reads `ml\models\deployed.json` from there |
| The prediction run shows a bigger outage than expected | Something else was using the machine, or you didn't wait 6 seconds. Run it again |
| Anything else mid-demo | Show `results/walkthrough.png` and `results/evaluation.md`. Those are measured results, so the point still stands |
