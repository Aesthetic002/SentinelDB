## The correct protocol

| Model | Reachable states | Transitions | Depth | Time | Result |
|---|---:|---:|---:|---:|---|
| 3 nodes, terms<=4, elections only, transfers, restarts | 34014 | 358974 | 18 | 560ms | no violations (exhaustive) |
| 3 nodes, terms<=3, log<=1, transfers, restarts | 135657 | 1205838 | 26 | 1.47s | no violations (exhaustive) |
| 3 nodes, terms<=4, log<=1 | 791292 | 6308289 | 32 | 10.78s | no violations (exhaustive) |
| 3 nodes, terms<=3, log<=2 | 2086662 | 15888107 | 30 | 28.38s | no violations (exhaustive) |
| 4 nodes, terms<=4, elections only, transfers, restarts | 1082585 | 16446310 | 26 | 40.28s | no violations (exhaustive) |
| 3 nodes, terms<=4, log<=1, transfers, restarts | 1578021 | 16179177 | 32 | 25.76s | no violations (exhaustive) |
| 5 nodes, terms<=3, elections only, transfers, restarts | 2123296 | 35961757 | 24 | 3m23.39s | no violations (exhaustive) |
| 3 nodes, terms<=3, log<=2, transfers, restarts | 3482466 | 34755431 | 30 | 1m1.86s | no violations (exhaustive) |

### Claims these runs support

> Modelling the cluster as the product automaton of 3 per-node automata and the network (3 nodes, terms<=4, elections only, transfers, restarts), explicit state-space exploration visited 34014 reachable states via 358974 transitions, and found zero violations of: Election Safety, Log Matching, Leader Completeness, State Machine Safety.

> Modelling the cluster as the product automaton of 3 per-node automata and the network (3 nodes, terms<=3, log<=1, transfers, restarts), explicit state-space exploration visited 135657 reachable states via 1205838 transitions, and found zero violations of: Election Safety, Log Matching, Leader Completeness, State Machine Safety.

> Modelling the cluster as the product automaton of 3 per-node automata and the network (3 nodes, terms<=4, log<=1), explicit state-space exploration visited 791292 reachable states via 6308289 transitions, and found zero violations of: Election Safety, Log Matching, Leader Completeness, State Machine Safety.

> Modelling the cluster as the product automaton of 3 per-node automata and the network (3 nodes, terms<=3, log<=2), explicit state-space exploration visited 2086662 reachable states via 15888107 transitions, and found zero violations of: Election Safety, Log Matching, Leader Completeness, State Machine Safety.

> Modelling the cluster as the product automaton of 4 per-node automata and the network (4 nodes, terms<=4, elections only, transfers, restarts), explicit state-space exploration visited 1082585 reachable states via 16446310 transitions, and found zero violations of: Election Safety, Log Matching, Leader Completeness, State Machine Safety.

> Modelling the cluster as the product automaton of 3 per-node automata and the network (3 nodes, terms<=4, log<=1, transfers, restarts), explicit state-space exploration visited 1578021 reachable states via 16179177 transitions, and found zero violations of: Election Safety, Log Matching, Leader Completeness, State Machine Safety.

> Modelling the cluster as the product automaton of 5 per-node automata and the network (5 nodes, terms<=3, elections only, transfers, restarts), explicit state-space exploration visited 2123296 reachable states via 35961757 transitions, and found zero violations of: Election Safety, Log Matching, Leader Completeness, State Machine Safety.

> Modelling the cluster as the product automaton of 3 per-node automata and the network (3 nodes, terms<=3, log<=2, transfers, restarts), explicit state-space exploration visited 3482466 reachable states via 34755431 transitions, and found zero violations of: Election Safety, Log Matching, Leader Completeness, State Machine Safety.

## Each known bug, injected on purpose

A clean result only means something if the checker can fail. Each row breaks one rule of the protocol.

| Injected bug | Bounds | Found | Steps | Property violated |
|---|---|---|---:|---|
| ignore-vote | 3 nodes, terms<=2, elections only | yes | 6 | Election Safety |
| forget-vote | 3 nodes, terms<=2, elections only, restarts | yes | 7 | Election Safety |
| small-quorum | 3 nodes, terms<=2, elections only | yes | 2 | Election Safety |
| transfer-no-bump | 3 nodes, terms<=2, elections only, transfers | yes | 4 | Election Safety |
| ignore-log-freshness | 3 nodes, terms<=3, log<=1 | yes | 9 | Leader Completeness |
| stale-append | 3 nodes, terms<=3, log<=1 | yes | 9 | Leader Completeness |
| commit-unverified | 3 nodes, terms<=3, log<=2 | yes | 14 | State Machine Safety |
| commit-old-term | 3 nodes, terms<=4, log<=1 | yes | 18 | Leader Completeness |

## Two leaders at once, in different terms

Reachable in correct Raft: a deposed leader that has not yet heard the news still believes it leads. It cannot commit anything, because a majority has moved to the higher term. The trace:

```
   0. initial state
      n1:F t0 v=- | n2:F t0 v=- | n3:F t0 v=-
   1. n1 times out and starts an election
      n1:C t1 v=n1 | n2:F t0 v=- | n3:F t0 v=-
   2. n2 receives RequestVote from n1 (term 1, log 0@0)
      n1:C t1 v=n1 | n2:F t1 v=n1 | n3:F t0 v=-
   3. n2 times out and starts an election
      n1:C t1 v=n1 | n2:C t2 v=n2 | n3:F t0 v=-
   4. n1 receives a vote from n2 (term 1)
      n1:L t1 v=n1 | n2:C t2 v=n2 | n3:F t0 v=-
   5. n3 receives RequestVote from n2 (term 2, log 0@0)
      n1:L t1 v=n1 | n2:C t2 v=n2 | n3:F t2 v=n2
   6. n2 receives a vote from n3 (term 2)
      n1:L t1 v=n1 | n2:L t2 v=n2 | n3:F t2 v=n2
```

## The per-node automaton

Derived from the transitions observed during exploration, not drawn by hand.

```
  from       event                          to
  Follower   timeout                        Candidate
  Follower   timeout-now                    Candidate
  Candidate  received-append-entries        Follower
  Candidate  restart                        Follower
  Candidate  saw-higher-term                Follower
  Candidate  vote-request                   Follower
  Candidate  majority-achieved              Leader
  Leader     lost-quorum                    Follower
  Leader     received-append-entries        Follower
  Leader     restart                        Follower
  Leader     saw-higher-term                Follower
  Leader     vote-request                   Follower
```
