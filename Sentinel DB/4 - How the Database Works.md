---
tags: [sentinel-db, dbms, database, sql]
aliases: [SentinelDB Database Layer, SentinelDB SQL]
created: 2026-10-13
updated: 2026-11-04
---

# How the Database Works

Back to [[1 - Overview|Overview]]. This note assumes you already know roughly how [[3 - How Leader Election Works|leader election and replication]] work — the database layer sits directly on top of that.

## Why this isn't "just a key-value store"

It would be easier to build a system that only stores raw key-value pairs (like "user:42" → some blob of data) and call it a day. We deliberately built something closer to a real database instead — proper tables, a schema, a small SQL language — because that's the realistic shape of systems that actually matter in production (see [[2 - Why We Built This (Inspiration and Related Papers)|CockroachDB and TiKV]] in the inspiration note), and because it lets this one project genuinely demonstrate real database-design thinking, not just a glorified dictionary.

## Designing the schema properly

Before writing any storage code, the data model is designed the way a real database would be: starting from an **Entity-Relationship diagram** for the chosen sample domain (an order-management system: customers, accounts, products, orders and order lines), identifying the entities, their attributes, and the relationships between them, and only then turning that diagram into actual tables. The worked design is in the repo at `docs/schema.md`, and `docs/schema.sql` creates it.

A key discipline applied here is **normalization**. In plain terms: make sure every fact is stored in exactly one place, so it's physically impossible for two copies of the same fact to quietly drift apart and disagree. For example, if a customer's address were stored separately in every order row instead of once in a customers table, updating their address would mean finding and fixing every single order — and missing one would leave the database lying to itself. All five tables are in BCNF, with the reasoning written down, including the decisions that look like violations but aren't: `unit_price_cents` on an order line isn't a duplicate of the product's price, because what something cost on that order and what it costs today are different facts.

## The query language

SentinelDB supports a deliberately small, real subset of SQL:

- `CREATE TABLE` — define a new table and its columns, with `PRIMARY KEY`, `NOT NULL` and `FOREIGN KEY` constraints that are actually enforced
- `INSERT` — add a row
- `SELECT ... WHERE` — read rows matching a condition, with `ORDER BY` and `LIMIT`
- `JOIN` — combine rows from related tables
- `UPDATE` / `DELETE` as natural extensions of the above
- `BEGIN` / `COMMIT` / `ROLLBACK` for transactions
- `EXPLAIN` — show a statement's parse tree and relational algebra without running it

It deliberately leaves out aggregates, subqueries, outer joins, `UNIQUE`, `DROP` and `ALTER`.

Every query is checked against a formally written grammar before it's run (explained properly in [[5 - How We Prove It Cant Break|How We Prove It Can't Break]], with the full grammar in `docs/grammar.md`), and internally converted into **relational algebra** operations — the mathematical building blocks of database queries, like "select the rows matching this condition" or "join these two tables on this column" — before it's actually executed. This translation step (SQL text → parse tree → relational algebra → execution) is worth showing explicitly in a demo, because it's the concrete, visible proof that this is a real query engine and not a disguised if-statement. `EXPLAIN` exists to do exactly that.

## What happens to a query, step by step

1. A client sends a SQL statement to any node in the cluster.
2. That node checks it against the grammar, and that the client's token permits it. If the node isn't the current leader, it forwards the request to whichever node is, keeping one connection so that an open transaction stays with the leader.
3. The leader parses the statement, takes the locks it needs, and works out the exact row changes.
4. For a write: those row changes (not the SQL text) are proposed as a new entry in the replicated log (see [[3 - How Leader Election Works|leader election]]) and only take effect once a majority of nodes have durably stored it — this is what makes a successful write survive a crash immediately afterward. Each request also carries an ID, so if a client retries after losing its connection, a write that had already succeeded isn't applied twice.
5. For a read: the leader first confirms with a majority that it's still the leader (Raft's ReadIndex), then answers from its own copy. That's the strong, "always correct" option. The confirmation matters: a leader that has been replaced but hasn't heard yet would otherwise happily return old data. Alternatively, a client can explicitly ask any node for a stale read from its own copy, for speed and to keep reading when there's no leader at all, at the cost of possibly being out of date. This is a genuine, explainable trade-off worth describing explicitly if asked ("would you rather be always-correct-but-slightly-slower, or fast-but-occasionally-stale"), and stale results are marked as stale.

Table data lives in memory, with durability coming from the replicated log and periodic snapshots on disk. So the data has to fit in RAM, which is a deliberate simplification for a project this size.

## Keeping multiple changes honest: transactions

Some operations need to change more than one thing at once and have it be all-or-nothing — the textbook example is moving money from one account to another: subtract from account A, add to account B, and never let the system stop halfway with the money vanished from A but not yet arrived in B. SentinelDB wraps such operations in explicit `BEGIN`/`COMMIT` transaction boundaries. Two different mechanisms then do two different jobs:

- **All-or-nothing comes from the log.** A transaction's changes are held privately until `COMMIT`, then replicated as a single log entry. One entry either commits or doesn't, so there's no halfway state to crash into.
- **Keeping concurrent transactions apart comes from Two-Phase Locking (2PL).** A transaction acquires every lock it will need *before* it releases any of them, which is what guarantees that even when many transactions run at the same time, the end result is equivalent to running them one after another in some valid order (this property has a name — conflict-serializability). Locks are only released after commit, and they work at two levels: a lookup by primary key locks one row, while a scan locks the whole table, which also stops a new row appearing mid-scan.

Deadlocks can't happen, by a rule called wait-die: an older transaction may wait for a younger one, but a younger one that conflicts with an older one aborts and retries instead, so a cycle of waiting can never form. Locks live only on the leader, so when leadership changes, open transactions abort rather than carrying on without the locks they depended on.

The conflict-serializability claim is tested directly rather than asserted: a real schedule of 721 concurrent transactions has an acyclic precedence graph and no money appears or vanishes, while with locks turned off, the same check immediately finds a cycle. You can also type a schedule into the shell (`\schedule r1(x) w2(x) w1(x)`) and see the graph.

## Splitting the data across more than one group (sharding)

**This part is not built.** It only applies if the data outgrows one group, which hasn't happened in this project, but it's worth knowing how it would work.

If the full data set or write volume outgrows what one 5-node voting group can comfortably handle, the data can be split into multiple independent **shards** — for example, customers A–M in one shard, N–Z in another — where each shard is its own separate Raft group with its own leader and its own copies. Most operations only need agreement within their own shard, which is faster than requiring the whole cluster to agree on everything.

The one tricky case is an operation that needs to touch two shards atomically (a transfer between a customer in shard 1 and one in shard 2). This takes two separate mechanisms, which are easy to mix up because of their similar names: Two-Phase *Locking* keeps concurrent transactions apart, but it can't make two separate groups agree to commit. That needs Two-Phase *Commit*, where a coordinator asks both shards to prepare and only then tells them to commit, so the operation either commits on both or on neither — never half-done.
