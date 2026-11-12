"""Mutation check for the Raft core.

Passing tests only mean something if those tests can fail. This script injects known
dangerous bugs into a scratch copy of internal/raft, one at a time, and confirms the
test suite catches each. Every entry is a bug that the Raft paper warns about or that
real implementations have shipped.

Usage: python scripts/mutation_check.py <repo-root> <scratch-dir>
"""
import pathlib
import shutil
import subprocess
import sys

repo, work = pathlib.Path(sys.argv[1]), pathlib.Path(sys.argv[2])

MUTATIONS = {
    "vote not persisted": (
        "n.vote = m.From\n\t\tn.persistHard() // durable before the reply can leave",
        "n.vote = m.From"),
    "quorum is half, not majority": (
        "func (n *Node) quorum() int { return len(n.members)/2 + 1 }",
        "func (n *Node) quorum() int { return (len(n.members) + 1) / 2 }"),
    "current-term commit not required for config": (
        "if !n.leaderReady() {\n\t\treturn 0, 0, ErrLeaderNotInit\n\t}\n\tif n.configIndex",
        "if n.configIndex"),
    "voter ignores log freshness": (
        "if !free || !upToDate {",
        "_ = upToDate\n\tif !free {"),
    "voter ignores earlier vote": (
        "if !free || !upToDate {",
        "_ = free\n\tif !upToDate {"),
    "commits older-term entries by counting": (
        "if t, ok := n.termAt(N); !ok || t != n.term {\n\t\treturn false\n\t}\n\tn.commit = N",
        "n.commit = N"),
    "follower commits unverified entries": (
        "if c := min(m.Commit, match); c > n.commit {",
        "if c := min(m.Commit, n.lastIndex()); c > n.commit {"),
    "reads skip quorum confirmation": (
        "confirmed := acks[n.quorum()-1]",
        "confirmed := n.hbSeq"),
    "reads count only one ack": (
        "confirmed := acks[n.quorum()-1]",
        "confirmed := acks[0]"),
    "append skips consistency check": (
        "if t, ok := n.termAt(prev); !ok || t != prevTerm {",
        "if t, ok := n.termAt(prev); !ok && t != prevTerm {"),
    "stale terms are accepted": (
        "case m.Term < n.term:\n",
        "case false:\n"),
    "transfer without a term bump": (
        "n.role = Candidate\n\tn.term++\n",
        "n.role = Candidate\n\tif !transfer {\n\t\tn.term++\n\t}\n"),
    "two-node membership jump allowed": (
        "if diff != 1 || len(next) == 0 {",
        "if len(next) == 0 {"),
    "config in-flight check removed": (
        "if n.configIndex > n.commit {\n\t\treturn 0, 0, ErrConfigPending\n\t}\n",
        ""),
}

TEST = ["go", "test", "./raft", "-count=1", "-timeout=240s", "-sim.seeds=40", "-sim.ticks=4000"]
EVIDENCE = ("SAFETY", "safety", "COMPLETENESS", "MATCHING", "FIGURE 8", "stale read",
            "LIVENESS", "panic:", "converged", "--- FAIL: Test")


def fresh_copy():
    d = work / "mut"
    if d.exists():
        shutil.rmtree(d)
    shutil.copytree(repo / "internal" / "raft", d / "raft")
    (d / "go.mod").write_text("module mut\n\ngo 1.23\n")
    return d


# A failing baseline would make every mutant look "caught", so refuse to continue.
d = fresh_copy()
base = subprocess.run(TEST, cwd=d, capture_output=True, text=True)
if base.returncode != 0:
    sys.exit("baseline tests fail, so mutation results would be meaningless:\n" + base.stdout[-2000:])
print("baseline: unmutated code passes\n", flush=True)

results = {}
for name, (old, new) in MUTATIONS.items():
    d = fresh_copy()
    src = (d / "raft" / "node.go").read_text(encoding="utf-8")
    if old not in src:
        results[name] = "PATTERN NOT FOUND"
        print(f"{name:45s} {results[name]}", flush=True)
        continue
    (d / "raft" / "node.go").write_text(src.replace(old, new, 1), encoding="utf-8")
    try:
        p = subprocess.run(TEST, cwd=d, capture_output=True, text=True, timeout=300)
    except subprocess.TimeoutExpired:
        results[name] = "CAUGHT (hang)"
        print(f"{name:45s} {results[name]}", flush=True)
        continue
    out = p.stdout + p.stderr
    if p.returncode == 0:
        results[name] = "SURVIVED  <-- not detected"
    elif "build failed" in out or "[setup failed]" in out:
        results[name] = "BUILD FAILED: " + out[:200]
    else:
        fails = [l.split()[2] for l in out.splitlines() if l.startswith("--- FAIL:")]
        why = next((l.strip() for l in out.splitlines() if any(k in l for k in EVIDENCE)), "?")
        results[name] = f"CAUGHT by {','.join(fails) or 'panic'}: {why[:90]}"
    print(f"{name:45s} {results[name]}", flush=True)

survived = [k for k, v in results.items() if not v.startswith("CAUGHT")]
print(f"\n{len(results) - len(survived)}/{len(results)} caught; not caught: {survived}")
sys.exit(1 if survived else 0)
