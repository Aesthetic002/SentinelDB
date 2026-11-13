# Verification, explained

How SentinelDB shows its replication can't silently corrupt data, in plain words. This page goes through the three techniques on the "Verification techniques" slide: what each one does, how it works inside, what it found and where the code lives.

For the short, claim-by-claim version (what each technique does and does not prove), see [verification.md](verification.md). For the raw model-checker report, see [results/verification.md](../results/verification.md).

---

## The big picture

There are three techniques, and each covers a gap the others leave:

```
              ┌────────────────────────────────────────────────┐
              │  ② Mutation testing: "do the alarms work?"     │
              └───────────┬───────────────────────┬────────────┘
                          │ 8 planted bugs        │ 14 planted bugs
                          ▼                       ▼
  ① Exhaustive model checking          ③ Randomized simulation
     checks the DESIGN                     checks the REAL GO CODE
     every reachable state                 320 random fault-filled runs
     (on a small model)                    (realistic, but samples)
```

Strictly, the 14 code bugs test the whole Raft test suite: the scripted tests in `node_test.go` plus the simulation. The mutation results below show which test caught each bug.

| | ① Model checking | ③ Randomized simulation | ② Mutation testing |
|---|---|---|---|
| **What it runs** | a simplified model of Raft | **the real Raft code that ships** | buggy copies of both |
| **Coverage** | every reachable state | random samples | 22 known bugs |
| **Strength** | finds rare bugs that need many exact steps (Figure 8) | full feature set, realistic sizes, recovery | proves ① and ③ can fail |
| **Weakness** | not the real code, small sizes | can miss rare event orders | only tests the bugs you thought of |

**Analogy: building a car.**
- ① is computer analysis of the **design**: every crash angle, mathematically, but it analyses the design, not the car.
- ③ is crash-testing **actual cars off the production line**, many times at random angles. It tests the real thing, but you can't try every angle.
- ② is deliberately damaging a car to confirm the **crash sensors** notice. It doesn't test the car, it tests that the measurements can be trusted.

### What is checked: the four Raft safety properties

All three techniques check the same four rules from the Raft paper:

| Property | In plain words |
|---|---|
| **Election Safety** | at most **one leader per term** |
| **Log Matching** | if two logs agree at some position, they agree on everything before it |
| **Leader Completeness** | a new leader already has every entry committed in earlier terms |
| **State Machine Safety** | no two nodes ever apply **different** entries at the same position |

The last one is what really matters: every replica ends up with the same data. The first three are what guarantee it.

---

## ① Exhaustive model checking

**Code:** [internal/modelcheck/model.go](../internal/modelcheck/model.go) (the model), [internal/modelcheck/check.go](../internal/modelcheck/check.go) (the search and the four checks), [cmd/sentinel-check](../cmd/sentinel-check) (the command-line tool).

### The idea in one sentence

Instead of trying some scenarios, map **every state the cluster could possibly reach** and check each one for danger.

**Layman picture:** think of the cluster as a huge building. Each **room** is one possible situation of the whole cluster, and each **door** is one event (a timeout, a vote, a crash…). Testing walks a few routes through the building. Model checking **maps every reachable room**, opens every door in every room, and checks each room. Once every room has been checked, every possible route is covered.

### Where are the alphabet and the strings?

In a textbook DFA the alphabet is characters and a string is a sequence of characters. Here:

- **The alphabet is events** that can happen to a node ([model.go:263](../internal/modelcheck/model.go)):

| Event | Meaning |
|---|---|
| `timeout` | "I haven't heard from a leader, I'll start an election" |
| `vote-request` | "someone asked me for my vote" |
| `vote-granted` | "someone voted for me" |
| `majority-achieved` | "that vote gave me a majority" |
| `received-append-entries` | "the leader sent me log entries or a heartbeat" |
| `append-response` | "a follower confirmed it has my entries" |
| `client-request` | "a user sent a write" |
| `saw-higher-term` | "I found out a newer term exists" |
| `restart` | "I crashed and came back" |
| `lost-quorum` | "I'm stepping down as leader" |
| `timeout-now` | "the predictor told me to take over" |

- **A string is a sequence of events, which is one possible history** of the cluster:
  ```
  timeout(n1) · vote-request(n2←n1) · vote-granted(n1←n2) · client-request(n1) · restart(n3) …
  ```

**The key difference from a normal DFA:** nobody feeds strings in. A normal DFA answers "given this string, where does it end up?" The checker answers the opposite: **"is there any string that leads to a bad state?"** In ToC terms, it checks whether the language of histories that reach a bad state is **empty**. That is a **reachability** question, answered by a graph search, not by running strings one at a time.

A precise note: each **node** is deterministic, since a given state plus a given event always leads to the same next state. The **cluster** is not deterministic about *which* event happens next (any node could time out, any message could arrive). That freedom is exactly what the checker explores.

### What one node's state holds

From [model.go:144](../internal/modelcheck/model.go):

```go
type NodeState struct {
    Role   // Follower / Candidate / Leader
    Term   // which election round I'm in
    Vote   // who I voted for in this term (or nobody)
    Votes  // if Candidate: who has voted for me so far
    LogLen // how many entries are in my log
    Log    // the term of each entry, e.g. [1, 1, 2]
    Commit // how many entries are safely committed
    Match  // if Leader: how far each follower has copied my log
}
```

Example of one node:
```
n1:  Role=Candidate  Term=1  Vote=n1  Votes={n1}  Log=[]  Commit=0
```
"n1 is running for election in term 1, voted for itself, has one vote so far, and its log is empty."

**Why not just the role?** If the state were only `(Leader, Follower, Follower)`, you couldn't tell whether a node had **already voted** this term, and that is exactly what stops a second leader from appearing. So the vote, the term and the log are all part of the state. It is much bigger than three circles, but still **finite**, because everything is bounded.

### What one global state holds (the product construction)

A global state is the **tuple of every node's state plus the network**, the same product construction used to intersect two DFAs:

```
n1: Leader    t1  voted=n1  log=[1]  commit=0
n2: Follower  t1  voted=n1  log=[]   commit=0
n3: Follower  t0  voted=-   log=[]   commit=0
network: { VoteRequest(term 1, from n1) still undelivered to n3 }
```
"n1 won term 1 with n2's vote and accepted a client write. n2 hasn't received that write yet. n3 is still in term 0 and hasn't heard of the election. n1's vote request is still in the air."

The network is part of the state because a message that has been sent but not yet delivered changes what can happen next.

The global state also keeps a few **history** fields ([model.go:189](../internal/modelcheck/model.go)): `LeaderOf[term]` (who has ever led each term) and `Committed` (what was committed at each position). They let a single state answer questions about the past, such as "were there ever two leaders of term 1, even at different times?"

### How the search works

Breadth-first search, from [check.go:165](../internal/modelcheck/check.go), simplified:

```
queue = [initial state]          // every node a Follower, term 0
seen  = {initial state}

while queue not empty:
    state = queue.pop()
    for each event possible in this state:        // every door
        next = apply(event, state)               // the transition function
        if next in seen: skip                    // already been in this room
        seen.add(next)
        if next breaks any of the four rules:
            print the path from start to next    // the counterexample "string"
            stop
        queue.push(next)

"every reachable state is safe"
```

**The first few steps, with 3 nodes:**

1. Start: `n1:F t0 | n2:F t0 | n3:F t0`, network empty. Possible events: `timeout(n1)`, `timeout(n2)`, `timeout(n3)`.
2. Follow `timeout(n1)`: `n1:C t1 votes={n1} | n2:F t0 | n3:F t0`, network `{VoteReq t1 from n1}`. Now many events are possible: n2 or n3 receives the request, n2 or n3 times out too (two candidates), n1 times out again (term 2), n1 crashes… The checker tries **every one**.
3. Follow `vote-request(n2←n1)` then `vote-granted(n1←n2)`: n1 has 2 of 3 votes, a majority, so it becomes **Leader**.

**Why the search ends: states merge.** Different orders of events often reach the **same** state:
```
timeout(n1) → n2 votes → n3 votes      ┐
timeout(n1) → n3 votes → n2 votes      ┘→ same global state
```
The `seen` set means each state is explored once. There are infinitely many *histories*, but only finitely many *states*, which is how a couple of million states cover every possible history within the bounds.

### How faults become "just more events"

No one writes a "partition scenario" or a "crash scenario." Every fault is an event, or the absence of one:

| Real-world fault | How the model represents it |
|---|---|
| Message lost | the "deliver" event is simply never chosen on that path |
| Message delayed | other events are chosen first |
| Messages reordered | every delivery order is tried |
| Message duplicated | a message can stay deliverable and arrive more than once |
| Network partition | nodes on one side never receive the other side's messages (a combination of losses) |
| Crash | the `restart` event: role, votes and commit are wiped, while term, vote and log survive because they're on disk ([model.go:404](../internal/modelcheck/model.go)) |
| Wrong prediction | the `timeout-now` event: **any leader may hand off to any node at any moment** ([model.go:447](../internal/modelcheck/model.go)) |

Because every event is tried in every state, every combination of these faults at every timing is explored automatically.

**The predictor cannot break safety.** The model does not assume the failure predictor is accurate. It lets any leader hand off to anyone at any time, which is the worst a predictor could do. All four rules still hold. So the worst a bad prediction can cause is an unnecessary handover, never two leaders or lost data.

### Bounds, and the tricks that keep the search small

Without limits, terms could grow forever, giving infinitely many states and a search that never ends. So each run is bounded (for example, 5 nodes and terms ≤ 3). The result is a proof **for that bounded model**.

Three tricks shrink the search without hiding any behaviour:
- **Symmetry reduction** ([model.go:729](../internal/modelcheck/model.go)): "n1 leads, n2 and n3 follow" behaves the same as "n2 leads, n1 and n3 follow" with names swapped, so only one is explored. `TestSymmetryReductionPreservesResults` checks this hides nothing.
- **Symbolic AppendEntries** ([model.go:29](../internal/modelcheck/model.go)): instead of tracking each log-copy message, everything a leader *could* have sent is worked out from its log and treated as deliverable at any time.
- **Removing dead messages** ([model.go:678](../internal/modelcheck/model.go)): a message from an old term that every node would ignore is dropped, so it can't create pointless duplicate states.

### Why this is even possible

"Can this program ever reach a bad state?" is **undecidable** for arbitrary programs (the Halting Problem). It becomes decidable here because the model is a **finite** automaton: just search all of its states. The project sidesteps undecidability by checking a deliberately restricted model.

### Results

From [results/verification.md](../results/verification.md), generated by `sentinel-check -suite`. Every run is exhaustive with zero violations of all four properties:

| Model | Reachable states | Transitions | Time |
|---|---:|---:|---:|
| 3 nodes, terms ≤ 4, elections only, transfers, restarts | 34,014 | 358,974 | 0.6 s |
| 3 nodes, terms ≤ 3, log ≤ 1, transfers, restarts | 135,657 | 1,205,838 | 1.5 s |
| 3 nodes, terms ≤ 4, log ≤ 1 | 791,292 | 6,308,289 | 11 s |
| 3 nodes, terms ≤ 3, log ≤ 2 | 2,086,662 | 15,888,107 | 28 s |
| 4 nodes, terms ≤ 4, elections only, transfers, restarts | 1,082,585 | 16,446,310 | 40 s |
| 3 nodes, terms ≤ 4, log ≤ 1, transfers, restarts | 1,578,021 | 16,179,177 | 26 s |
| **5 nodes, terms ≤ 3, elections only, transfers, restarts** | **2,123,296** | **35,961,757** | 3 min 23 s |
| 3 nodes, terms ≤ 3, log ≤ 2, transfers, restarts | 3,482,466 | 34,755,431 | 1 min 2 s |

### "Can there ever be two leaders?"

The precise answer is **not in the same term**. A stale leader from an older term can exist for a moment during a handover. That's normal in correct Raft and harmless, because a majority has already moved to the higher term, so the old leader can't commit anything, and reads confirm leadership with a majority first. `sentinel-check -two-leaders` prints a 6-step path to this state.

### Limits

- It checks the **design**, not the Go code. The model is a separate program. That gap is what ③ covers.
- It is **bounded**: the results hold for the sizes in the table.
- **Membership changes, snapshots and batching are not in the model.**
- It checks **safety only** (nothing bad happens), not **liveness** (the cluster eventually works again).
- It assumes crash faults, not nodes that lie.

---

## ② Mutation testing

**Code:** the 8 model bugs are switches in [internal/modelcheck/model.go:63](../internal/modelcheck/model.go), tested by `TestCheckerFindsEveryInjectedBug` in [internal/modelcheck/check_test.go](../internal/modelcheck/check_test.go). The 14 code bugs are in [scripts/mutation_check.py](../scripts/mutation_check.py).

### Why it exists

Suppose the model checker says **"0 violations."** That could mean the protocol is safe, or that the checker is broken and can't see anything. The output is the same either way. **A test that can never fail proves nothing.**

**Layman picture: testing a smoke detector.** A detector that has been silent for a year either means no fire or a dead battery. You find out by **holding a lit match under it**. If it beeps, you can trust its silence afterwards.

Mutation testing holds a match under the checks: plant a bug you know is dangerous and confirm the check raises the alarm.

### Faults vs bugs: this does not mean SentinelDB fails

| | Fault | Bug (mutation) |
|---|---|---|
| What it is | something going wrong in the **environment** | a mistake in the **code** |
| Examples | crash, lost message, network split | "vote twice," "use half instead of a majority" |
| Who causes it | the real world | us, on purpose, in a **throwaway copy** |
| Should the system survive it? | yes, that is Raft's job | no, it's a broken program |

The same tests, with the same faults, run on both versions:

| Version | Result | Meaning |
|---|---|---|
| Real SentinelDB code | ✅ tests pass | the real system survives the faults |
| Copy with a planted bug | ❌ tests fail | the tests can spot a broken Raft |

**Both results are what we want.** The failure on the mutant is the goal: it shows the alarm works.

### Vocabulary

| Term | Meaning |
|---|---|
| Mutation | a small deliberate change that introduces a known bug |
| Mutant | the program with that one change applied |
| Caught / killed | the checks **failed** on the mutant, so the alarm works ✅ |
| Survived | the checks **passed** on the mutant, so there's a blind spot ❌ |

### Rules that make the result trustworthy

1. **Clean code must pass first.** Otherwise every mutant would look "caught." The script refuses to continue if the baseline fails ([mutation_check.py:76](../scripts/mutation_check.py)).
2. **One bug at a time**, so you know which bug was caught.
3. **A fresh copy for every mutant.** The real code is never modified.
4. **Control:** the same bounds are clean without the bug, so the bug is what caused the failure (`TestBugBoundsAreCleanWithoutTheBug`).

### Why two sets of bugs

There are two different alarms, so each needs its own test:

```
 Model checker                       ──checks──▶  the MODEL of Raft   ← tested with 8 model bugs
 Raft tests (scripted + simulation)  ──check──▶   the REAL Go code    ← tested with 14 code bugs
```

### Part A: 8 bugs in the model

Each bug is a switch in the model. For example, the voting rule ([model.go:506](../internal/modelcheck/model.go)):

```go
free := nv.Vote == noNode || nv.Vote == m.From   // "have I not voted yet, or voted for you?"
if b.Bug == BugIgnoreVote {
    free = true                                   // the bug: "I'm always free to vote"
}
```

Results, from [results/verification.md](../results/verification.md) and re-run with `go test ./internal/modelcheck -run TestCheckerFindsEveryInjectedBug` (all 8 pass):

| Bug | In plain words | Found after | Rule broken |
|---|---|---:|---|
| `small-quorum` | win with **half** the votes, not a majority | 2 steps | Election Safety |
| `transfer-no-bump` | the predictor's handover skips starting a new term | 4 steps | Election Safety |
| `ignore-vote` | vote for two candidates in one term | 6 steps | Election Safety |
| `forget-vote` | forget your vote after a crash | 7 steps | Election Safety |
| `ignore-log-freshness` | vote for a candidate with an out-of-date log | 9 steps | Leader Completeness |
| `stale-append` | accept log entries from an old leader | 9 steps | Leader Completeness |
| `commit-unverified` | a follower trusts the leader's commit number too far | 14 steps | State Machine Safety |
| `commit-old-term` | **Figure 8 of the Raft paper** | 18 steps | Leader Completeness |

"Steps" is the length of the **shortest** event sequence that reaches the violation (BFS guarantees it's the shortest).

**A real counterexample.** This is the actual output of `sentinel-check -nodes 3 -terms 2 -log 0 -transfers=false -restarts=false -bug ignore-vote`:

```
VIOLATION of Election Safety after 6 steps: two nodes have both been leader in term 1

   0. initial state
      n1:F t0 v=- | n2:F t0 v=- | n3:F t0 v=-
   1. n1 times out and starts an election
      n1:C t1 v=n1 | n2:F t0 v=- | n3:F t0 v=-
   2. n2 times out and starts an election
      n1:C t1 v=n1 | n2:C t1 v=n2 | n3:F t0 v=-
   3. n2 receives RequestVote from n1 (term 1, log 0@0)
      n1:C t1 v=n1 | n2:C t1 v=n1 | n3:F t0 v=-
   4. n1 receives RequestVote from n2 (term 1, log 0@0)
      n1:C t1 v=n2 | n2:C t1 v=n1 | n3:F t0 v=-
   5. n1 receives a vote from n2 (term 1)
      n1:L t1 v=n2 | n2:C t1 v=n1 | n3:F t0 v=-
   6. n2 receives a vote from n1 (term 1)
      n1:L t1 v=n2 | n2:L t1 v=n1 | n3:F t0 v=-
```

Two candidates, each of which had already voted for itself, vote for each other as well. Each then has 2 of 3 votes, so both become leader of term 1. In the correct model, steps 3 and 4 are refused because each node has already voted. **That trace is the "string": a word in the language that reaches a bad state.** No one wrote this scenario; the search found it.

**What to notice:** easy bugs break in a few steps, while subtle ones need long, exact sequences. Figure 8 needs **18 precisely ordered events**, which a human tester would almost never write by hand.

### Part B: 14 bugs in the real Go code

How [scripts/mutation_check.py](../scripts/mutation_check.py) works:

```
1. Copy internal/raft into a scratch folder and run its tests: they must PASS (baseline)
2. For each of the 14 mutations:
     a. take a fresh copy of the code
     b. find one exact piece of node.go and replace it with the buggy version
     c. run the Raft tests (unit tests + the randomized simulation)
     d. tests FAIL → CAUGHT ✅
        tests PASS → SURVIVED ❌  (a blind spot: write a better test)
        tests HANG → CAUGHT (the cluster got stuck, which also counts)
3. Print "14/14 caught", or the list of survivors
```

The mutations are often a single small change, because real bugs are too:

**One character, `||` to `&&`** ("append skips consistency check"):
```go
// correct: reject if I don't have the entry OR it's from a different term
if t, ok := n.termAt(prev); !ok || t != prevTerm {
// mutant:  the check almost never triggers, so followers accept entries that don't fit
if t, ok := n.termAt(prev); !ok && t != prevTerm {
```

**The majority formula** ("quorum is half, not majority"):
```
correct:  len/2 + 1     → 3 nodes: 2   4 nodes: 3   5 nodes: 3
mutant:   (len+1)/2     → 3 nodes: 2   4 nodes: 2   5 nodes: 3
```
This one only differs for **even** cluster sizes. With 4 nodes, 2 votes would win, so two leaders could be elected. A test suite that only ever used 3- or 5-node clusters would miss it.

**Deleting the save-to-disk** ("vote not persisted"): the vote is remembered in memory but not saved before replying. The bug is invisible unless a node crashes at the wrong moment, forgets its vote and votes again, so it is only catchable because the tests include crashes.

All 14:

| # | Mutation | What breaks |
|---:|---|---|
| 1 | vote not persisted | after a crash, a node forgets its vote and can vote twice |
| 2 | quorum is half, not majority | with an even number of nodes, two leaders can win |
| 3 | current-term commit not required for config | a new leader changes membership before it has committed anything in its own term |
| 4 | voter ignores log freshness | a node with an out-of-date log can be elected and lose committed data |
| 5 | voter ignores earlier vote | a node votes for two candidates in one term |
| 6 | commits older-term entries by counting | Figure 8: an entry is marked committed and later overwritten |
| 7 | follower commits unverified entries | a follower marks entries committed that it hasn't confirmed match the leader's |
| 8 | reads skip quorum confirmation | a deposed leader can answer reads with stale data |
| 9 | reads count only one ack | same, with only one follower's confirmation instead of a majority |
| 10 | append skips consistency check | followers accept entries that don't fit their log, so logs diverge |
| 11 | stale terms are accepted | messages from old leaders are acted on |
| 12 | transfer without a term bump | a handover creates a second leader in the same term |
| 13 | two-node membership jump allowed | changing two members at once allows two separate majorities |
| 14 | config in-flight check removed | a second membership change starts before the first is committed |

Every entry is a bug the Raft paper warns about or that real implementations have shipped.

### Mutation results

Run with `python scripts/mutation_check.py . <scratch-dir>` on 2026-11-13. The baseline (unmodified code) passed first, then **all 14 mutants were caught**:

| # | Mutation | Caught by |
|---:|---|---|
| 1 | vote not persisted | `TestVoteIsDurableAcrossRestart` |
| 2 | quorum is half, not majority | **`TestSimulation` only** |
| 3 | current-term commit not required for config | `TestMembershipChangeRequiresCurrentTermCommit` |
| 4 | voter ignores log freshness | `TestCommitRequiresMajority`, `TestStaleCandidateCannotWinVote`, `TestSnapshotCatchUp`, `TestSimulation` |
| 5 | voter ignores earlier vote | 9 tests, including `TestElectsExactlyOneLeader` and `TestSimulation` |
| 6 | commits older-term entries by counting | `TestFigure8` |
| 7 | follower commits unverified entries | **`TestSimulation` only** |
| 8 | reads skip quorum confirmation | `TestPartitionedLeaderCannotServeReads` |
| 9 | reads count only one ack | `TestPartitionedLeaderCannotServeReads` |
| 10 | append skips consistency check | `TestCommitRequiresMajority`, `TestFigure8`, `TestSimulation` |
| 11 | stale terms are accepted | `TestLeadershipTransfer`, `TestSimulation` |
| 12 | transfer without a term bump | `TestLeadershipTransfer`, `TestTransferToLaggingFollowerWaitsForCatchUp` |
| 13 | two-node membership jump allowed | `TestMembershipChangeRules` |
| 14 | config in-flight check removed | `TestSecondMembershipChangeWaitsForFirst`, `TestSimulation` |

```
14/14 caught; not caught: []
```

**What the "caught by" column shows:** the 14 code bugs are caught by the whole Raft test suite, not by the simulation alone. The scripted tests in [node_test.go](../internal/raft/node_test.go) catch 12 of them. The randomized simulation catches 7, and for **2 of them it is the only test that notices**: the even-cluster majority bug (#2) and the follower trusting an unverified commit number (#7). Those are bugs no one wrote a specific test for, which is exactly what random fault injection is for. In the other direction, some bugs, such as Figure 8 (#6), need a precise event order that random runs don't hit, so only a scripted test catches them. The two kinds of test cover each other's gaps.

The script runs the simulation with 4,000 ticks per run instead of the usual 6,000, to keep the 15 test runs fast.

### Limits

It proves the checks catch **these** kinds of bugs, so a clean result means something. It doesn't prove no *other* bugs exist.

In industry, mutation tools usually generate thousands of random mutants automatically (flip every `<` to `<=`, every `+` to `-`). These are **hand-picked, targeted** mutants, each based on a known Raft pitfall. There are fewer of them, but each one is a bug that has caused real problems.

---

## ③ Randomized simulation

**Code:** [internal/raft/sim_test.go](../internal/raft/sim_test.go), the test `TestSimulation`. The code under test is the real Raft implementation in [internal/raft/node.go](../internal/raft/node.go).

### Why it is needed when we already have ① and ②

- **① checks a copy of the idea, not the real code.** The model ([model.go](../internal/modelcheck/model.go)) and the real Raft ([node.go](../internal/raft/node.go)) are **two different programs**. A typo in node.go that the model doesn't share leaves the model checker green. In the analogy, the bridge's blueprint is proven sound, but nobody has checked that the builders followed it.
- **② doesn't test the system at all.** It checks the tests. The 14 real-code bugs are caught by the Raft test suite: the scripted tests plus this simulation. The simulation catches 7 of them, and 2 are caught by nothing else (see the mutation results above):
  ```
  Mutation testing ──checks──▶ Raft tests (scripted + simulation) ──check──▶ real Go code
  ```
- **The model leaves out a lot** so that exhaustive search can finish:

| Feature | Model checker | Simulation |
|---|---|---|
| Elections, voting, log copying | ✅ | ✅ |
| Snapshots / log compaction | ❌ | ✅ |
| Adding and removing nodes (membership) | ❌ | ✅ |
| Reads with leader confirmation | ❌ | ✅ |
| Batching entries | ❌ | ✅ |
| Size | ≤ 3 or 4 terms, log ≤ 2 entries | 6,000 ticks, hundreds of entries, many terms |
| Liveness (does it recover after faults?) | ❌ | ✅ |

### How it works: the real code in a fake world

The simulation runs **the exact Raft code that ships**, five copies in one test process, and replaces everything around it with a fake world the test fully controls:

| Real thing | Simulated as |
|---|---|
| Time | a counter: tick 1, tick 2, … An election timeout is 12 ticks, a heartbeat every 3 |
| Network | a queue of messages, each delivered after a random delay ([sim_test.go:334](../internal/raft/sim_test.go)) |
| Message loss | each message has a set chance (e.g. 15%) of being thrown away |
| Duplicates | a set chance (e.g. 5%) that a message is delivered twice |
| Partition | nodes are split into groups, and messages only reach nodes in the same group ([sim_test.go:361](../internal/raft/sim_test.go)) |
| Crash | the node is deleted and only its "disk" survives. It restarts later from that disk |
| Disk | `memStore`, an in-memory store that survives simulated crashes ([sim_test.go:21](../internal/raft/sim_test.go)) |

**Every tick, chaos happens at random** ([sim_test.go:381](../internal/raft/sim_test.go)), depending on what the scenario switches on:
- maybe crash a node; a crashed node restarts after about 100 ticks on average
- maybe split the network into two random groups, or heal it completely
- maybe send a client write to the leader
- maybe hand leadership to a random node, as a wrong predictor would
- maybe add or remove a node
- maybe compact a node's log into a snapshot
- maybe issue a read

**Every 100 ticks, the safety rules are checked** ([sim_test.go:543](../internal/raft/sim_test.go)): one leader per term, Log Matching, Leader Completeness, and committed entries never change. Reads are checked too: a read must never return data older than what was already committed.

**State Machine Safety uses a hash chain** ([sim_test.go:51](../internal/raft/sim_test.go)). Each node hashes every command it applies into a running chain. If two nodes have the same hash at the same position, they applied exactly the same commands in exactly the same order, even across snapshots.

**At the end: heal and check recovery** ([sim_test.go:497](../internal/raft/sim_test.go)). All faults are removed. The cluster must elect a leader, commit a new write within 4,000 ticks, and bring every node to the same state. If it gets stuck, the test fails with `LIVENESS`. The model checker never checks this.

**Seeds make every run replayable.** All randomness comes from one starting number. If a run fails, the error says, for example:
```
seed 217 tick 3481: ... (replay with -sim.seed=217)
```
and rerunning with that seed reproduces exactly the same crashes, drops and timings.

**A guard against a broken setup** ([sim_test.go:609](../internal/raft/sim_test.go)). After the 40 runs of a scenario, the test fails if a scenario meant to crash nodes performed zero crashes, or a membership scenario made zero membership changes. Without this, a broken setup could pass "safely" without injecting any faults.

### The 8 scenarios

Each scenario is a **fault recipe**. Each runs **40 times** (seeds 1 to 40), **6,000 ticks** per run: **8 × 40 = 320 runs**. Settings from [sim_test.go:574](../internal/raft/sim_test.go):

| Scenario | Nodes | What's thrown at it |
|---|---|---|
| `calm-3` | 3 | baseline: short delays (≤ 2 ticks), client writes, no faults |
| `lossy-5` | 5 | **15% of messages dropped**, 5% duplicated, delays up to 8 ticks |
| `crashes-5` | 5 | **random crashes** (about one every 150 ticks) and restarts, 5% loss |
| `partitions` | 5 | **network split** into random groups about every 150 ticks, or healed, 5% loss |
| `transfers-storm` | 5 | **constant leadership handovers** to random nodes (an extreme "predictor gone wrong"), plus reads, heavy write load |
| `membership` | 3 growing to 5 | **nodes joining and leaving**, plus occasional crashes, partitions, transfers, snapshots and reads |
| `membership-storm` | 3 growing to 5 | membership changes **about every 15 ticks**, frequent crashes and partitions, 10% loss |
| `everything` | 5 | **all faults at once**: 10% loss, 5% duplicates, crashes, partitions, transfers, snapshots, reads |

### Results

Run with `go test ./internal/raft -run TestSimulation -v` on 2026-11-13. **All 8 scenarios, all 320 runs, passed** in about 13 seconds: no safety rule broken, no stale read, and the cluster recovered and converged every time.

Totals across the 40 runs of each scenario:

| Scenario | Elections | Crashes | Partition changes | Transfers | Snapshots | Membership changes | Reads served | Reads refused | Result |
|---|---:|---:|---:|---:|---:|---:|---:|---:|---|
| `calm-3` | 40 | 0 | 0 | 0 | 0 | 0 | 0 | 0 | ✅ PASS |
| `lossy-5` | 47 | 0 | 0 | 0 | 0 | 0 | 0 | 0 | ✅ PASS |
| `crashes-5` | 375 | 1,432 | 0 | 0 | 0 | 0 | 0 | 0 | ✅ PASS |
| `partitions` | 780 | 0 | 1,590 | 0 | 0 | 0 | 0 | 0 | ✅ PASS |
| `transfers-storm` | 638 | 0 | 0 | 598 | 0 | 0 | 11,698 | 91 | ✅ PASS |
| `membership` | 745 | 586 | 661 | 311 | 617 | 236 | 8,733 | 300 | ✅ PASS |
| `membership-storm` | 1,136 | 1,476 | 2,311 | 0 | 0 | 1,664 | 0 | 0 | ✅ PASS |
| `everything` | 959 | 1,110 | 1,227 | 384 | 588 | 0 | 6,360 | 404 | ✅ PASS |

`calm-3` has about one election per run, which is what a fault-free cluster should do: elect a leader once and keep it.

**"Reads refused" are not failures.** They are reads the leader declined because it couldn't confirm it was still the leader, for example during a partition. Refusing is the correct, safe behaviour. A failure would be answering with stale data, which never happened.

### Limits

It is **sampling, not proof**, so it can miss rare event orders. For example, random runs never hit Figure 8 by chance, so that case has a scripted test, `TestFigure8` in [internal/raft/node_test.go](../internal/raft/node_test.go). The model checker found Figure 8 by itself; the simulation covers snapshots and membership, which the model can't. Each covers the other's gap.

---

## Where everything is

| What | File |
|---|---|
| The model of Raft (states, events, transitions, the 8 bug switches) | [internal/modelcheck/model.go](../internal/modelcheck/model.go) |
| The search (BFS) and the four property checks | [internal/modelcheck/check.go](../internal/modelcheck/check.go) |
| Tests: every model bug is found, clean bounds stay clean, symmetry hides nothing | [internal/modelcheck/check_test.go](../internal/modelcheck/check_test.go) |
| Command-line model checker | [cmd/sentinel-check](../cmd/sentinel-check) |
| Model-checker report (state counts, bug table, automaton table) | [results/verification.md](../results/verification.md) |
| The real Raft implementation | [internal/raft/node.go](../internal/raft/node.go) |
| The randomized simulation (fake network, chaos, checks, scenarios) | [internal/raft/sim_test.go](../internal/raft/sim_test.go) |
| Scripted Raft tests, including `TestFigure8` | [internal/raft/node_test.go](../internal/raft/node_test.go) |
| The 14 code mutations and the script that runs them | [scripts/mutation_check.py](../scripts/mutation_check.py) |
| Claim-by-claim summary of all verification | [verification.md](verification.md) |

## Reproducing it

```
sentinel-check -suite                                   # ① full model-checking report (several minutes)
sentinel-check -bug ignore-vote -log 0 -terms 2         # ② one model bug, with its counterexample
go test ./internal/modelcheck -run TestCheckerFindsEveryInjectedBug -v   # ② all 8 model bugs
python scripts/mutation_check.py . <scratch-dir>        # ② all 14 code bugs (several minutes)
go test ./internal/raft -run TestSimulation -v          # ③ the 320 simulation runs
go test ./internal/raft -run TestSimulation -sim.seed=217   # ③ replay one seed exactly
```

## One-liners for a viva

**Model checking:**
> "Each node is a state machine whose inputs are events like timeouts, votes and crashes. The cluster is all the nodes plus the network combined into one product automaton. We don't feed in scenarios. Starting from 'everyone is a follower,' the checker tries every event that could happen next, visits every reachable state once, and checks the four safety rules in each. Faults are just events that may or may not happen, so every combination at every timing is covered. The largest run visited 2.1 million states with zero violations."

**Mutation testing:**
> "A test that never fails proves nothing. So we planted known, realistic Raft bugs one at a time: 8 in the model, all caught, including Figure 8 as an 18-step trace nobody wrote by hand, and 14 in the real Go code, all caught by the Raft test suite, two of them only by the randomized simulation. This doesn't mean SentinelDB fails. The real code passes. It means our checks reliably fail when a bug is present, so their passing actually means something."

**Randomized simulation:**
> "The model checker proves the design, but it's a separate program. The simulation runs the real Raft code in a fake world with 8 fault recipes, from a calm cluster to everything at once, 40 seeds each, 320 runs. It checks the same safety rules, that no read is stale, and that the cluster recovers once faults stop. All 320 pass, and any failure would replay exactly from its seed."
