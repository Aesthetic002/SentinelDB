# Check 1, live: the judge's data survives a leader kill

The same check as [check-1-judge-data.md](check-1-judge-data.md), done the way you'd use a real database: a cluster in one window, a SQL shell in another, and Windows' own tools in a third to kill and revive a node. Nothing is deleted first. The cluster keeps the data it already has, as a database should.

**What it shows:** a judge picks a name. It's stored on all five nodes. The leader's process is killed from outside our software. The name is still there, the database keeps accepting writes, and when the dead node comes back it catches up on what it missed.

Run everything from the project folder in PowerShell. The examples use the name `Hemanth`, the leader `n4`, and IDs `99` and `100`. Change them to match your run.

---

Every step below was dry-run on 2026-11-13 against a throwaway copy of the cluster. The outputs quoted are what the programs actually print.

## The four windows

| Window | What runs there | Stays open? |
|---|---|---|
| **Terminal 1** | The cluster (5 nodes). Its log shows who is leader | Yes, the whole time |
| **Terminal 2** | The SQL shell, `sentinel>`. All the database work happens here | Yes, the whole time |
| **Terminal 3** | Windows commands: look at the processes, kill one, check the disks | Used now and then |
| **Terminal 4** | Brings the dead node back | Opened near the end |

---

## Before the judges arrive (once)

**1. Build**, only if the code changed since the last build or `bin\` is missing:

```powershell
go build -o bin/ ./cmd/...
```

**2. Make sure the sample tables exist.** Start the cluster (Terminal 1, step 1 below), then in another window:

```powershell
bin\sentinel-cli -e "SELECT * FROM customers"
```

- If it prints a table, you're ready. Leave it as it is.
- If it says `table customers does not exist`, load the tables once:

```powershell
Get-Content docs\schema.sql | bin\sentinel-cli
```

You don't need to do this again. The tables and rows are kept on disk between runs.

---

## 1. Start the cluster (Terminal 1)

```powershell
bin\sentinel-lab serve
```

Leave it running.

**Look for:** a line like `n4: Leader in term 3`. The latest such line names the current leader. The term number will be higher if this cluster has been used before. That's normal.

---

## 2. Open the database (Terminal 2)

```powershell
bin\sentinel-cli
```

You get a `sentinel>` prompt. From here, type SQL and end each statement with `;`. Lines starting with `\` are shell commands and don't need `;`.

**Show the cluster:**

```
\status
```

**Look for:** `"leader": "n4"` (who leads right now), `"term"`, the five `"members"`, and `"tables"`, which lists the tables in the database. The `"id"` and `"role"` at the top are the node the shell happens to be connected to, often a follower. That's fine: any node can take your requests.

**Show what's already stored:**

```sql
SELECT * FROM customers ORDER BY id;
```

**Say:** "This data is from earlier sessions. Nothing was reset. The database keeps it on disk."

**Pick a free ID** for the judge's row: any number not in that list. The examples use `99`.

---

## 3. Store the judge's name (Terminal 2)

Ask the judge for a name, then:

```sql
INSERT INTO customers VALUES (99, 'Hemanth', 'Bengaluru');
```

**Look for:** `1 row affected`.

```sql
SELECT * FROM customers WHERE id = 99;
```

**Look for:** the judge's row.

If you get `duplicate primary key`, that ID was already used in an earlier session. Pick another number. That error is also worth pointing out: the database refuses to store two rows with the same key.

---

## 4. Show it's on all five disks (Terminal 3)

This uses Windows' own search, not our software:

```powershell
Select-String -Path data\n*\raft.wal -Pattern 'Hemanth' -SimpleMatch -List | Select-Object Path
```

**Look for:** five paths, `data\n1\raft.wal` to `data\n5\raft.wal`. Each node wrote the name to its own file on disk.

---

## 5. Kill the leader (Terminal 3)

**First, list the five node processes as Windows sees them:**

```powershell
Get-CimInstance Win32_Process -Filter "Name='sentinel-node.exe'" | ForEach-Object { "{0,-8} {1}" -f $_.ProcessId, ($_.CommandLine -replace '.*-id (\S+).*', '$1') }
```

**Look for:** five lines, each a process ID and the node it runs, for example:

```
14212    n1
9876     n2
15340    n3
11024    n4
8812     n5
```

**Then kill the leader by its process ID.** Use the leader from step 2 and its number from the list:

```powershell
Stop-Process -Id 11024 -Force
```

This prints nothing if it works. `-Force` kills the process at once, with no chance to shut down cleanly, the same as a crash.

**Run the list again:**

```powershell
Get-CimInstance Win32_Process -Filter "Name='sentinel-node.exe'" | ForEach-Object { "{0,-8} {1}" -f $_.ProcessId, ($_.CommandLine -replace '.*-id (\S+).*', '$1') }
```

**Look for:** four lines. The leader is gone, according to Windows.

**Meanwhile in Terminal 1:** a few `peer stream ended … forcibly closed by the remote host` lines appear. Those are the other nodes noticing their connection to the dead node broke, which is expected. Within about a second, a new line like `n2: Leader in term 4` follows. A different node leads, and the term has gone up by one.

---

## 6. Read the name back (Terminal 2)

The same shell, still open:

```sql
SELECT * FROM customers WHERE id = 99;
```

**Look for:** the judge's name. The node that was leading when it was written is dead, and the data is still there.

The first statement after the kill can take a moment while the shell finds the new leader. That's the failover happening.

```
\status
```

**Look for:** a different `"leader"` and a higher `"term"`.

---

## 7. Write while the node is down (Terminal 2)

```sql
INSERT INTO customers VALUES (100, 'Written while n4 was dead', NULL);
```

**Look for:** `1 row affected`. Four of five nodes are a majority, so the database keeps working.

---

## 8. Bring the node back (Terminal 4)

Set the number of the node you killed, then start it with the same settings the cluster gave it:

```powershell
$n = 4
```

```powershell
bin\sentinel-node -id "n$n" -cluster "n1=127.0.0.1:7201,n2=127.0.0.1:7202,n3=127.0.0.1:7203,n4=127.0.0.1:7204,n5=127.0.0.1:7205" -client-addr "127.0.0.1:710$n" -data "data\n$n" -certs certs -election-ticks 50 -enable-chaos
```

Leave it running. It reads its own disk, rejoins the cluster, and catches up on what it missed.

**Look for:** a line like `n4: serving clients on 127.0.0.1:7104, peers on 127.0.0.1:7204`. It comes back as a follower, because the cluster already has a leader, so no "Leader" line appears for it.

---

## 9. Show it caught up (Terminal 3)

Ask only the revived node, from its own copy (`-stale` means "answer from this node's data, don't go to the leader"):

```powershell
bin\sentinel-cli -addrs 127.0.0.1:7104 -stale -e "SELECT * FROM customers WHERE id = 100"
```

Use port `710` followed by the node number: 7101 for n1, 7104 for n4.

**Look for:** `Written while n4 was dead`, and `(1 row, stale read from n4)` underneath. That node was dead when the row was written, so it can only have it by catching up from the others.

**Then list the processes once more:**

```powershell
Get-CimInstance Win32_Process -Filter "Name='sentinel-node.exe'" | ForEach-Object { "{0,-8} {1}" -f $_.ProcessId, ($_.CommandLine -replace '.*-id (\S+).*', '$1') }
```

**Look for:** five lines again, with a new process ID for the revived node.

---

## 10. Finish

- **Terminal 2:** type `\q` to leave the shell.
- **Terminal 4:** press **Ctrl-C**. The revived node runs on its own, so the cluster can't stop it for you.
- **Terminal 1:** press **Ctrl-C**.

The data stays on disk for next time, including the judge's name.

If anything is still running afterwards:

```powershell
Get-Process sentinel-node,sentinel-lab -ErrorAction SilentlyContinue | Stop-Process -Force
```

---

## If something goes wrong

| What you see | What to do |
|---|---|
| `table customers does not exist` | The tables were never loaded, or the `data` folder was removed. Do "Before the judges arrive", step 2 |
| `duplicate primary key` | That ID was used in an earlier session. Pick another number, and use it in the `SELECT` too |
| The shell shows a connection error | The cluster isn't up yet. Wait for a `Leader in term …` line in Terminal 1 |
| The first statement after the kill takes a few seconds or reports an error | Run it again. The shell is reconnecting to the new leader |
| Step 8 says the address is already in use | That node is still running. Check the process list from step 5 |
| `Stop-Process` says it can't find the process | The ID was mistyped, or that node already died. List the processes again |
| You want a completely fresh database | Stop everything, then use step 1 of [check-1-judge-data.md](check-1-judge-data.md), which deletes the `data` folder. Only do this when you mean to throw everything away |

---

## What to say

> "This is a database that already had data in it. Nothing was reset for the demo. You chose that name, so it can't be pre-recorded. Windows' own search shows it's on all five disks. I killed the leader's process by its ID, from outside our software, and you can see Windows now lists only four. The name is still there, and the database kept accepting writes. When the node came back, it caught up on the row it missed while it was dead."
