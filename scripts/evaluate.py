"""Turn the raw bench results into the evaluation report and its two figures.

Both of the numbers the design notes insist on are reported side by side: how much shorter
the outage is, and how often the predictor hands over when nothing was wrong. Neither is
meaningful without the other.

Usage: python scripts/evaluate.py [--bench results/bench.csv] [--timelines results/timelines] [--out results]
"""
import argparse
import pathlib

import matplotlib

matplotlib.use("Agg")
import matplotlib.pyplot as plt
import numpy as np
import pandas as pd

SURFACE, INK, INK2, MUTED, GRID, AXIS = "#fcfcfb", "#0b0b0b", "#52514e", "#898781", "#e1e0d9", "#c3c2b7"
REACTIVE, PROACTIVE = "#eb6834", "#2a78d6"  # color follows the mode in every figure
CRITICAL = "#d03b3b"
FONT = ["Segoe UI", "DejaVu Sans"]

DEGRADE = ["degrade-delay", "degrade-jitter", "degrade-loss", "degrade-mixed"]
NO_FAILURE = ["healthy", "blip", "steady-slow", "steady-slow-high", "one-bad-link", "follower-kill", "two-follower-kill"]
LABELS = {
    "degrade-delay": "Growing delay", "degrade-jitter": "Growing jitter", "degrade-loss": "Growing packet loss",
    "degrade-mixed": "Mixed degradation", "hard-kill": "Sudden kill, no warning",
    "healthy": "Healthy", "blip": "Brief latency blips", "steady-slow": "Steady slow network", "steady-slow-high": "Steady, very slow network",
    "one-bad-link": "One bad follower link", "follower-kill": "One follower dies", "two-follower-kill": "Two followers die",
}


NOTES = """
## Reading these numbers

- **Plain Raft's outage depends on the election timeout.** These clusters use a timeout of 500 to 1000 ms, and the outage is close to that. With a longer timeout, plain Raft's outage grows in proportion, while the proactive path barely changes, as it does not wait for a timer. So the percentage is a statement about this configuration, not a constant. A timeout cannot simply be made tiny to avoid all this, because on a real network that causes needless elections.
- **All nodes share one machine.** Absolute latencies are not those of a real network. Comparisons between the two modes are the meaningful part.
- **The faults are injected.** A ramp of delay, jitter and loss stands in for a failing machine. Real hardware may degrade differently, or not gradually at all, so nothing here is a claim about how often real failures could be predicted.
- **Sign tests on 10 pairs bottom out at p = 0.002**, which is what all-10-pairs-agree gives. The effect is large enough that more trials would not change the conclusion, but they would tighten the intervals.
"""


def style(ax):
    ax.set_facecolor(SURFACE)
    for side in ("top", "right", "left"):
        ax.spines[side].set_visible(False)
    ax.spines["bottom"].set_color(AXIS)
    ax.spines["bottom"].set_linewidth(1)
    ax.tick_params(colors=MUTED, length=0, labelsize=9)


def bootstrap_ci(a, b, n=4000, seed=7):
    """95% interval for the difference in medians, resampling each group."""
    rng = np.random.default_rng(seed)
    a, b = np.asarray(a, float), np.asarray(b, float)
    d = np.median(rng.choice(a, (n, len(a))), axis=1) - np.median(rng.choice(b, (n, len(b))), axis=1)
    return float(np.percentile(d, 2.5)), float(np.percentile(d, 97.5))


def sign_test(pairs):
    """Two-sided exact sign test on paired differences, needing no assumption about shape."""
    from math import comb
    pos = sum(1 for d in pairs if d > 0)
    neg = sum(1 for d in pairs if d < 0)
    n = pos + neg
    if n == 0:
        return 1.0
    k = max(pos, neg)
    p = 2 * sum(comb(n, i) for i in range(k, n + 1)) / 2 ** n
    return min(1.0, p)


def fig_outage(df, out):
    """Horizontal bars of median outage, mode by mode, with the interquartile range."""
    scenarios = DEGRADE + ["hard-kill"]
    fig, ax = plt.subplots(figsize=(9, 4.4), dpi=160)
    fig.patch.set_facecolor(SURFACE)
    style(ax)
    h = 0.34
    rows = np.arange(len(scenarios))[::-1]
    peak = 0
    for offset, mode, color in ((h / 2 + 0.02, "reactive", REACTIVE), (-h / 2 - 0.02, "proactive", PROACTIVE)):
        for y, s in zip(rows, scenarios):
            v = df[(df.scenario == s) & (df["mode"] == mode)].outage_ms
            if v.empty:
                continue
            med, q1, q3 = v.median(), v.quantile(0.25), v.quantile(0.75)
            peak = max(peak, q3)
            ax.barh(y + offset, med, height=h, color=color, zorder=2)
            ax.plot([q1, q3], [y + offset] * 2, color=INK2, linewidth=1, zorder=3)
            ax.text(q3 + 12, y + offset, f"{med:,.0f} ms", va="center", ha="left", fontsize=9, color=INK)
    ax.set_yticks(rows)
    ax.set_yticklabels([LABELS[s] for s in scenarios], color=INK2, fontsize=10)
    ax.set_xlim(0, peak * 1.2)
    ax.xaxis.grid(True, color=GRID, linewidth=1, zorder=0)
    ax.set_axisbelow(True)
    ax.set_xlabel("Longest stretch with no write completing (ms): median, line shows interquartile range", color=INK2, fontsize=9)
    from matplotlib.patches import Patch
    ax.legend(handles=[Patch(color=REACTIVE, label="Plain Raft"), Patch(color=PROACTIVE, label="With prediction")],
              loc="lower right", frameon=False, fontsize=9, labelcolor=INK2)
    ax.set_title("Prediction shortens the outage when a failure has warning signs, and cannot help when it has none",
                 loc="left", fontsize=11, color=INK, fontweight="semibold")
    fig.tight_layout()
    fig.savefig(out / "outage-by-scenario.png", facecolor=SURFACE)
    plt.close(fig)


def load_timeline(path):
    t = pd.read_csv(path)
    t = t[t.ok.astype(str).str.lower() == "true"]
    return t


def fig_walkthrough(timelines: pathlib.Path, out, scenario="degrade-delay"):
    """The same fault, plain Raft above and with prediction below, on one shared axis."""
    fig, axes = plt.subplots(2, 1, figsize=(9, 5.2), dpi=160, sharex=True, sharey=True)
    fig.patch.set_facecolor(SURFACE)
    series = []
    for mode in ("reactive", "proactive"):
        p = timelines / f"{scenario}-{mode}.csv"
        if not p.exists():
            return False
        series.append((mode, load_timeline(p)))
    # Align on the end of the run: every run in a scenario ends 3s after its kill.
    for ax, (mode, t) in zip(axes, series):
        style(ax)
        end = t.end_ms.max()
        x = (t.end_ms - end) / 1000
        lat = t.end_ms - t.start_ms
        color = REACTIVE if mode == "reactive" else PROACTIVE
        ax.plot(x, lat, color=color, linewidth=1.2, solid_joinstyle="round", zorder=2)
        ax.yaxis.grid(True, color=GRID, linewidth=1, zorder=0)
        ax.set_axisbelow(True)
        name = "Plain Raft" if mode == "reactive" else "With prediction"
        ax.text(0.005, 0.93, name, transform=ax.transAxes, fontsize=10, color=INK, fontweight="semibold", va="top")
        gaps = t.end_ms.diff()
        i = gaps.idxmax()
        gap_end = (t.end_ms[i] - end) / 1000
        ax.axvspan(gap_end - gaps[i] / 1000, gap_end, color=CRITICAL, alpha=0.12, linewidth=0, zorder=1)
        ax.annotate(f"longest gap {gaps[i]:,.0f} ms", xy=(gap_end, ax.get_ylim()[1]), fontsize=9, color=INK2,
                    xytext=(6, -14), textcoords="offset points", va="top", annotation_clip=False)
        ax.set_ylabel("Write latency (ms)", color=INK2, fontsize=9)
        ax.set_xlim(-15, 0)
        ax.set_ylim(bottom=0)
    axes[-1].set_xlabel("Seconds before the end of the run. The node dies 3 seconds before the end.", color=INK2, fontsize=9)
    for ax in axes:
        ax.axvline(-3, color=INK2, linewidth=1, zorder=1)
    axes[0].text(-3, axes[0].get_ylim()[1], " node killed", fontsize=9, color=INK2, va="top", ha="left")
    axes[0].set_title("One failing node, two responses", loc="left", fontsize=11, color=INK, fontweight="semibold")
    fig.tight_layout()
    fig.savefig(out / "walkthrough.png", facecolor=SURFACE)
    plt.close(fig)
    return True


def report(df, out, walkthrough):
    L = ["# Evaluation: does predicting failure help?\n"]
    L.append("Real 5-node clusters of separate processes on one machine, with real fsync and mutual TLS. "
             "A client writes continuously throughout, and each trial pairs plain Raft with the predictor on the same fault. "
             "**Outage** means the longest stretch in which no write completed, as seen by the client, retries included.\n")
    trials = df.groupby(["scenario", "mode"]).size()
    L.append(f"{len(df)} trials, {int(trials.min())} to {int(trials.max())} per scenario per mode.\n")

    L.append("## Number 1: how much shorter is the outage?\n")
    if walkthrough:
        L.append("![The same fault under plain Raft and with prediction](walkthrough.png)\n")
    L.append("![Median outage by scenario](outage-by-scenario.png)\n")
    L.append("| Failure | Plain Raft, median | With prediction, median | Change | 95% CI of the difference | Paired sign test p | Handed over before death |")
    L.append("|---|---:|---:|---:|---|---:|---:|")
    for s in DEGRADE + ["hard-kill"]:
        r = df[(df.scenario == s) & (df["mode"] == "reactive")].sort_values("trial")
        p = df[(df.scenario == s) & (df["mode"] == "proactive")].sort_values("trial")
        if r.empty or p.empty:
            continue
        rm, pm = r.outage_ms.median(), p.outage_ms.median()
        lo, hi = bootstrap_ci(p.outage_ms, r.outage_ms)
        merged = r.merge(p, on="trial", suffixes=("_r", "_p"))
        pv = sign_test((merged.outage_ms_p - merged.outage_ms_r).tolist())
        L.append(f"| {LABELS[s]} | {rm:,.0f} ms | {pm:,.0f} ms | {(pm - rm) / rm:+.0%} | {lo:+,.0f} to {hi:+,.0f} ms | {pv:.4f} | "
                 f"{int(p.handed_over.sum())}/{len(p)} |")
    deg = df[df.scenario.isin(DEGRADE)]
    if not deg.empty:
        r = deg[deg["mode"] == "reactive"].outage_ms
        p = deg[deg["mode"] == "proactive"]
        L.append("")
        L.append(f"Across all gradual failures: **{r.median():,.0f} ms** with plain Raft against **{p.outage_ms.median():,.0f} ms** with prediction. "
                 f"The predictor moved leadership before the death in {int(p.handed_over.sum())} of {len(p)} trials"
                 + (f", a median of {p[p.handed_over].lead_ms.median() / 1000:.1f} s ahead of it." if p.handed_over.any() else "."))
        L.append("")
    L.append("### Tail latency while the node was getting worse\n")
    L.append("A failing leader hurts before it dies, so handing over early should help during that period as well.\n")
    L.append("| Failure | Plain Raft, p99 | With prediction, p99 |")
    L.append("|---|---:|---:|")
    for s in DEGRADE:
        r = df[(df.scenario == s) & (df["mode"] == "reactive")].degraded_p99_ms
        p = df[(df.scenario == s) & (df["mode"] == "proactive")].degraded_p99_ms
        if len(r) and len(p):
            L.append(f"| {LABELS[s]} | {r.median():,.0f} ms | {p.median():,.0f} ms |")

    L.append("\n## Number 2: how often does it act when nothing is wrong?\n")
    L.append("A predictor could cut outages just by moving leadership around at random, so this number has to be read together with the first. "
             "In these scenarios the leader never fails, so any leadership change is unnecessary.\n")
    L.append("| Situation (leader stays healthy) | Trials per mode | Leader changes, plain Raft | Leader changes, with prediction | Trials with a false handover |")
    L.append("|---|---:|---:|---:|---:|")
    tot_hours, tot_changes, tot_base = 0.0, 0, 0
    for s in NO_FAILURE:
        r = df[(df.scenario == s) & (df["mode"] == "reactive")]
        p = df[(df.scenario == s) & (df["mode"] == "proactive")]
        if r.empty or p.empty:
            continue
        L.append(f"| {LABELS[s]} | {len(p)} | {int(r.leader_changes.sum())} | {int(p.leader_changes.sum())} | {int((p.leader_changes > 0).sum())}/{len(p)} |")
        tot_hours += p.duration_ms.sum() / 3.6e6
        tot_changes += int(p.leader_changes.sum())
        tot_base += int(r.leader_changes.sum())
    if tot_hours:
        L.append("")
        trials = sum(len(df[(df.scenario == s) & (df["mode"] == "proactive")]) for s in NO_FAILURE)
        L.append(f"Over {tot_hours * 60:.1f} minutes of no-failure operation in {trials} trials, the predictor changed leader {tot_changes} time(s), against {tot_base} for plain Raft.")
        if tot_changes == 0:
            # The "rule of three": 3/n is the 95% upper bound when zero events occur in n trials.
            L.append("")
            L.append(f"Zero observed does not mean a rate of zero. With none in {trials} trials, the 95% upper bound is about **{3 / trials:.0%} of runs** "
                     f"(the rule of three), or about {3 / tot_hours:.0f} per hour of these conditions. The data cannot rule out a rate below that, "
                     "and these are conditions built to look like failure, not a quiet system. A real false-alarm rate would need far longer running than this project has done.")
    L.append("")
    return "\n".join(L)


def unseen_section(path):
    """How the model behaves on patterns kept out of its training data."""
    if not path.exists():
        return ""
    d = pd.read_csv(path)
    L = ["## Outside the training data\n"]
    L.append("The scenarios above are fresh runs, but of the same kinds of fault the model trained on. "
             "These three were never in the training data, and answer the questions a score on familiar data cannot.\n")
    L.append("| Scenario | Trials per mode | Outage, plain Raft | Outage, with prediction | Handed over before death | Leader changes per trial, plain Raft / prediction |")
    L.append("|---|---:|---:|---:|---:|---:|")
    names = {"slow-ramp": "Ramp over 25 s, much slower than training, then death",
             "fast-ramp": "Ramp over 1.5 s, faster than the rule can act, then death",
             "ramp-recover": "Same ramp as a real failure, but the node recovers"}
    for s in ("slow-ramp", "fast-ramp", "ramp-recover"):
        r, p = d[(d.scenario == s) & (d["mode"] == "reactive")], d[(d.scenario == s) & (d["mode"] == "proactive")]
        if r.empty or p.empty:
            continue
        handed = f"{int(p.handed_over.sum())}/{len(p)}" if s != "ramp-recover" else "n/a (no death)"
        L.append(f"| {names[s]} | {len(p)} | {r.outage_ms.median():,.0f} ms | {p.outage_ms.median():,.0f} ms | {handed} | "
                 f"{r.leader_changes.mean():.1f} / {p.leader_changes.mean():.1f} |")
    L.append("")
    return "\n".join(L)


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--bench", default="results/bench.csv", type=pathlib.Path)
    ap.add_argument("--timelines", default="results/timelines", type=pathlib.Path)
    ap.add_argument("--out", default="results", type=pathlib.Path)
    args = ap.parse_args()
    plt.rcParams["font.family"] = FONT
    df = pd.read_csv(args.bench)
    args.out.mkdir(parents=True, exist_ok=True)
    fig_outage(df, args.out)
    walkthrough = fig_walkthrough(args.timelines, args.out)
    body = report(df, args.out, walkthrough) + "\n" + unseen_section(args.out / "bench-unseen.csv")
    body += NOTES
    (args.out / "evaluation.md").write_text(body, encoding="utf-8")
    print("wrote", args.out / "evaluation.md")


if __name__ == "__main__":
    main()
