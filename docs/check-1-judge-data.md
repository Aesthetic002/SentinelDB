# Check 1: The Judge's Own Data Survives

This shows the demo is real. A judge picks a name, it gets stored on all five nodes, the leader is killed, and the name is still there. A recording couldn't contain a name someone just made up.

Run everything from the project folder in PowerShell. The examples use the name `Hemanth`, the leader `n4`, and IDs `99` and `100`. Change them to match your run.

## Before you start

Build once, if you haven't already:

```powershell
go build -o bin/ ./cmd/...
```

**The data you enter is permanent.** It's saved in the `data` folder and survives closing the terminal or restarting the PC. Step 1 deletes it, so every run that starts from step 1 is fresh, and you can reuse `99` and `100` each time. If you skip that deletion, the old data is still there: loading the tables again gives `already exists` errors, and reusing an ID gives `duplicate primary key`.

## 1. Start the cluster

**Terminal 1.** Leave this running:

```powershell
Remove-Item -Recurse -Force data -ErrorAction SilentlyContinue
bin\sentinel-lab serve
```

**Look for:** a line like `n4: Leader in term 1`. The latest such line tells you the current leader.

## 2. Load the sample tables

**Terminal 2:**

```powershell
Get-Content docs\schema.sql | bin\sentinel-cli
```

**Look for:** `table customers created`, and no lines starting with `error`.

## 3. Store the judge's name

Keep the single quotes, and replace only the name inside them:

```powershell
bin\sentinel-cli -e "INSERT INTO customers VALUES (99, 'Hemanth', NULL)"
```

**Look for:** `1 row affected`.

## 4. Show it's on every node's disk

```powershell
Select-String -Path data\n*\raft.wal -Pattern 'Hemanth' -SimpleMatch -List | Select-Object Path
```

**Look for:** 5 file paths, from `data\n1` to `data\n5`.

## 5. Kill the leader

Replace `n4` with the current leader from Terminal 1, and keep the spaces inside `*-id n4 *`:

```powershell
Get-CimInstance Win32_Process -Filter "Name='sentinel-node.exe'" | Where-Object { $_.CommandLine -like '*-id n4 *' } | ForEach-Object { Stop-Process -Id $_.ProcessId -Force }
```

This prints nothing if it works.

**Look for:** a new line in Terminal 1, like `n2: Leader in term 2`. A different node leads, and the term number has gone up.

## 6. Confirm a node really died

```powershell
@(Get-Process sentinel-node).Count
```

**Look for:** `4`. This count comes from Windows, not from our software.

## 7. Read the name back

```powershell
bin\sentinel-cli -e "SELECT name FROM customers WHERE id = 99"
```

**Look for:** the judge's name. The node that was leading when it was written is dead, and the data is still there.

## 8. Write while the node is down

```powershell
bin\sentinel-cli -e "INSERT INTO customers VALUES (100, 'After the kill', NULL)"
```

**Look for:** `1 row affected`. The database still accepts writes.

## 9. Bring the node back

**Terminal 3.** Leave this running:

```powershell
bin\sentinel-node -id n4 -cluster "n1=127.0.0.1:7201,n2=127.0.0.1:7202,n3=127.0.0.1:7203,n4=127.0.0.1:7204,n5=127.0.0.1:7205" -client-addr 127.0.0.1:7104 -data data\n4 -certs certs -election-ticks 50 -enable-chaos
```

If a different node died, change these three values:

| Node | `-id` | `-client-addr` | `-data` |
|---|---|---|---|
| n1 | `n1` | `127.0.0.1:7101` | `data\n1` |
| n2 | `n2` | `127.0.0.1:7102` | `data\n2` |
| n3 | `n3` | `127.0.0.1:7103` | `data\n3` |
| n4 | `n4` | `127.0.0.1:7104` | `data\n4` |
| n5 | `n5` | `127.0.0.1:7105` | `data\n5` |

Leave the `-cluster` value as it is.

## 10. Show it caught up

**Terminal 2.** Ask only the restarted node, from its own copy. Use that node's address from the table:

```powershell
bin\sentinel-cli -addrs 127.0.0.1:7104 -stale -e "SELECT id, name FROM customers WHERE id = 100"
```

**Look for:** `After the kill`, with `stale read from n4` underneath. That node was dead when the row was written, so it can only have got it by catching up from the others.

Then confirm all five are back:

```powershell
@(Get-Process sentinel-node).Count
```

**Look for:** `5`.

## 11. Stop

Press **Ctrl-C** in Terminal 1 and Terminal 3. The restarted node runs on its own, so it needs stopping separately.

If anything is left running:

```powershell
Get-Process sentinel-node,sentinel-lab -ErrorAction SilentlyContinue | Stop-Process -Force
```

## If something goes wrong

| What you see | What to do |
|---|---|
| `table customers already exists` | Old data from an earlier run. Stop the cluster and start again from step 1 |
| `duplicate primary key` | That ID is already used. Pick a new number, and use it in the `SELECT` too |
| Step 5 prints nothing, but the count in step 6 is still `5` | Wait a second and count again. If it's still `5`, check the node name you used |
| A syntax error on the name | Use plain letters and spaces, and keep the single quotes |

## What to say

> "You chose that name, so it can't be pre-recorded. It's on all five disks, I killed the leader from outside our software, and it's still there. The node that died has also caught up on what it missed."
