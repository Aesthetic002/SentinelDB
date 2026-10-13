---
tags: [sentinel-db, dbms, database, sql]
aliases: [SentinelDB Database Layer, SentinelDB SQL]
created: 2026-10-13
updated: 2026-10-13
---

# How the Database Works

Back to [[1 - Overview|Overview]]. This note assumes you already know roughly how [[3 - How Leader Election Works|leader election and replication]] work — the database layer sits directly on top of that.

## Why this isn't "just a key-value store"

It would be easier to build a system that only stores raw key-value pairs (like "user:42" → some blob of data) and call it a day. We deliberately built something closer to a real database instead — proper tables, a schema, a small SQL language — because that's the realistic shape of systems that actually matter in production (see [[2 - Why We Built This (Inspiration and Related Papers)|CockroachDB and TiKV]] in the inspiration note), and because it lets this one project genuinely demonstrate real database-design thinking, not just a glorified dictionary.

## Designing the schema properly

Before writing any storage code, the data model is designed the way a real database would be: starting from an **Entity-Relationship diagram** for the chosen sample domain (for example, a small library system, or an order-management system), identifying the entities, their attributes, and the relationships between them, and only then turning that diagram into actual tables.

A key discipline applied here is **normalization**. In plain terms: make sure every fact is stored in exactly one place, so it's physically impossible for two copies of the same fact to quietly drift apart and disagree. For example, if a customer's address were stored separately in every order row instead of once in a customers table, updating their address would mean finding and fixing every single order — and missing one would leave the database lying to itself. The schema is normalized to at least Third Normal Form (ideally BCNF) with the reasoning behind each decision written down, so it can be defended, not just claimed.

## The query language

SentinelDB supports a deliberately small, real subset of SQL:

- `CREATE TABLE` — define a new table and its columns
- `INSERT` — add a row
- `SELECT ... WHERE` — read rows matching a condition
- a basic `JOIN` — combine rows from two related tables
- `UPDATE` / `DELETE` as natural extensions of the above

Every query is checked against a formally written grammar before it's run (explained properly in [[5 - How We Prove It Cant Break|How We Prove It Can't Break]]), and internally converted into **relational algebra** operations — the mathematical building blocks of database queries, like "select the rows matching this condition" or "join these two tables on this column" — before it's actually executed. This translation step (SQL text → parse tree → relational algebra → execution) is worth showing explicitly in a demo, because it's the concrete, visible proof that this is a real query engine and not a disguised if-statement.

## What happens to a query, step by step

1. A client sends a SQL statement to any node in the cluster.
2. If that node isn't the current leader, it forwards the request to whichever node is.
3. The leader parses the statement and translates it into the underlying read/write operations.
4. For a write: the operation is proposed as a new entry in the replicated log (see [[3 - How Leader Election Works|leader election]]) and only takes effect once a majority of nodes have durably stored it — this is what makes a successful write survive a crash immediately afterward.
5. For a read: the leader can answer directly from its own up-to-date copy (this is the strong, "always correct" option), or a follower can answer from its own copy for speed at the cost of possibly being a few milliseconds out of date — this is a genuine, explainable trade-off worth describing explicitly if asked ("would you rather be always-correct-but-slightly-slower, or fast-but-occasionally-stale") rather than something to gloss over.

## Keeping multiple changes honest: transactions

Some operations need to change more than one thing at once and have it be all-or-nothing — the textbook example is moving money from one account to another: subtract from account A, add to account B, and never let the system stop halfway with the money vanished from A but not yet arrived in B. SentinelDB wraps such operations in explicit `BEGIN`/`COMMIT` transaction boundaries, and uses **Two-Phase Locking (2PL)** to keep concurrent transactions from interfering with each other: a transaction acquires every lock it will need *before* it releases any of them, which is what guarantees that even when many transactions run at the same time, the end result is equivalent to running them one after another in some valid order (this property has a name — conflict-serializability — and it can actually be tested on a concrete example schedule, which is a good thing to demonstrate directly rather than just assert).

## Splitting the data across more than one group (sharding)

If the full data set or write volume outgrows what one 5-node voting group can comfortably handle, the data can be split into multiple independent **shards** — for example, customers A–M in one shard, N–Z in another — where each shard is its own separate Raft group with its own leader and its own copies. Most operations only need agreement within their own shard, which is faster than requiring the whole cluster to agree on everything. The one tricky case is an operation that needs to touch two shards atomically (a transfer between a customer in shard 1 and one in shard 2); that's handled by coordinating the Two-Phase Locking protocol across both shards, so the operation either commits on both or on neither — never half-done.
