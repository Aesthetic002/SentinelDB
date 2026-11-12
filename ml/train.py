"""Train and compare the failure predictors, then export them for the Go runtime.

Labelling
    A reading is "about to fail" (1) if it comes from the node that will die, while its
    fault is building, and within HORIZON_S of its death. Readings from every other scenario
    are 0, including the moments before a sudden kill: nothing about those moments looks
    wrong, so calling them "about to fail" would only teach the model noise.

    The first part of each ramp is left out entirely. A fault that has only just begun is
    indistinguishable from health, so forcing a label on it either way would be a guess.

Splitting
    Readings overlap in time, so neighbours are nearly identical. Splitting individual rows
    would put near-duplicates on both sides and report a flattering, meaningless score. So
    whole runs are assigned to train, validation or test, with scenarios stratified.

Selection
    Scoring each reading is the wrong objective. A model with a 0.98 F1 can still hand over
    in most runs of a merely slow network, since a brief step in latency looks just like the
    start of a ramp until enough time has passed to see that it stopped rising. So:
      - Hyperparameters are tuned by cross-validation on training runs.
      - The threshold and the number of consecutive readings are chosen together on
        out-of-fold predictions across training and validation runs, by the outcome that
        matters per run: fewest false alarms, then most failures caught, then most warning.
      - The winning model is chosen the same way, and refit on those same runs.
    The test runs are only used to report, and not to choose anything.

Usage: python ml/train.py [--data ml/data] [--models ml/models] [--report results/ml-comparison.md]
"""
import argparse
import json
import pathlib

import numpy as np
import pandas as pd
from sklearn.base import clone
from sklearn.ensemble import RandomForestClassifier
from sklearn.linear_model import LogisticRegression
from sklearn.metrics import f1_score, precision_score, recall_score
from sklearn.model_selection import GridSearchCV, GroupKFold
from sklearn.naive_bayes import GaussianNB
from sklearn.pipeline import make_pipeline
from sklearn.preprocessing import StandardScaler
from sklearn.tree import DecisionTreeClassifier, export_text

FEATURES = ["rtt_mean_ms", "rtt_recent_ms", "rtt_jitter_ms", "rtt_max_ms", "rtt_slope_ms_s", "miss_ratio"]
HORIZON_S = 6.0  # "about to fail" means dying within this long
RAMP_SKIP = 0.25  # the fraction of each ramp left out as genuinely ambiguous
THRESHOLDS = np.round(np.arange(0.30, 0.96, 0.05), 2)
CONSECUTIVE = [3, 5, 10, 15, 20, 25, 30, 35]  # readings in a row before acting, at 10 per second
COOLDOWN_MS = 5000  # matches the Go monitor
SEED = 20261104


def load(data_dir: pathlib.Path) -> tuple[pd.DataFrame, pd.DataFrame]:
    runs = pd.read_csv(data_dir / "runs.csv")
    tel = pd.read_csv(data_dir / "telemetry.csv")
    # A run with no readings at all has nothing to contribute.
    runs = runs[runs.run_id.isin(tel.run_id)].reset_index(drop=True)
    df = tel.merge(runs, on="run_id")
    return runs, label(df)


def label(df: pd.DataFrame) -> pd.DataFrame:
    """Derive labels from event times. Returns only the rows that have a defensible label."""
    df = df.copy()
    df["label"] = 0
    keep = np.ones(len(df), dtype=bool)
    for run_id, group in df.groupby("run_id"):
        row = group.iloc[0]
        if not row.degrades:
            continue
        idx = group.index
        ramp = row.kill_ms - row.fault_start_ms
        ambiguous_until = row.fault_start_ms + RAMP_SKIP * ramp
        on_victim = group.node == row.victim
        in_fault = group.unix_ms >= row.fault_start_ms
        positive = on_victim & (group.unix_ms >= ambiguous_until) & (group.unix_ms < row.kill_ms) & (
            row.kill_ms - group.unix_ms <= HORIZON_S * 1000
        )
        df.loc[idx[positive], "label"] = 1
        # Readings during the early ramp are excluded, as are any taken on the victim after
        # its death by a stale process (there should be none). A different node leading after
        # the victim died is a healthy reading and stays as a 0.
        drop = on_victim & in_fault & ~positive
        keep[df.index.get_indexer(idx[drop])] = False
    return df[keep].reset_index(drop=True)


def split_runs(runs: pd.DataFrame) -> dict[str, set]:
    """Assign whole runs to train (60%), validation (20%) and test (20%), per scenario."""
    rng = np.random.default_rng(SEED)
    parts = {"train": set(), "val": set(), "test": set()}
    for _, group in runs.groupby("scenario"):
        ids = group.run_id.to_numpy().copy()
        rng.shuffle(ids)
        n = len(ids)
        n_val = max(1, round(0.2 * n))
        n_test = max(1, round(0.2 * n))
        parts["test"].update(ids[:n_test])
        parts["val"].update(ids[n_test:n_test + n_val])
        parts["train"].update(ids[n_test + n_val:])
    return parts


def candidates():
    """The four models from the design notes, with small grids that stay interpretable."""
    return {
        "decision_tree": (
            DecisionTreeClassifier(random_state=SEED, class_weight="balanced"),
            {"max_depth": [2, 3, 4, 5], "min_samples_leaf": [10, 30, 60]},
        ),
        "naive_bayes": (GaussianNB(), {"var_smoothing": [1e-9, 1e-6, 1e-3]}),
        "logistic_regression": (
            make_pipeline(StandardScaler(), LogisticRegression(max_iter=2000, class_weight="balanced")),
            {"logisticregression__C": [0.01, 0.1, 1, 10]},
        ),
        "random_forest": (
            RandomForestClassifier(random_state=SEED, class_weight="balanced", n_jobs=2),
            {"n_estimators": [50], "max_depth": [3, 5, 8], "min_samples_leaf": [5, 20]},
        ),
    }


def event_metrics(frame: pd.DataFrame, runs: pd.DataFrame, probs: np.ndarray, threshold: float, consecutive: int) -> dict:
    """Replay the Go monitor's decision rule over each run and score the outcome.

    Row metrics reward being right about each reading. What matters in operation is whether
    the rule fires before a death, and how often it fires when nothing is wrong. A firing
    before the fault has begun is a false alarm, even in a run that later fails.
    """
    frame = frame.assign(prob=probs).sort_values(["run_id", "unix_ms"])
    meta_by_id = runs.set_index("run_id")
    caught, degrading, false_runs, healthy_runs, healthy_seconds = 0, 0, 0, 0, 0.0
    false_fires, leads, by_scenario = 0, [], {}
    for run_id, group in frame.groupby("run_id"):
        meta = meta_by_id.loc[run_id]
        leader_rows = group[group.node == meta.victim]
        over, last_fire, fires = 0, -10**15, []
        for t, p in zip(leader_rows.unix_ms.to_numpy(), leader_rows.prob.to_numpy()):
            if p < threshold:
                over = 0
                continue
            over += 1
            if over >= consecutive and t - last_fire >= COOLDOWN_MS:
                fires.append(t)
                last_fire, over = t, 0
        if meta.degrades:
            degrading += 1
            early = [t for t in fires if t < meta.fault_start_ms]
            valid = [t for t in fires if meta.fault_start_ms <= t < meta.kill_ms]
            if valid:
                caught += 1
                leads.append((meta.kill_ms - valid[0]) / 1000)
            if early:
                false_runs += 1
                false_fires += len(early)
                by_scenario[meta.scenario] = by_scenario.get(meta.scenario, 0) + 1
        else:
            healthy_runs += 1
            healthy_seconds += (leader_rows.unix_ms.max() - leader_rows.unix_ms.min()) / 1000 if len(leader_rows) else 0
            if fires:
                false_runs += 1
                false_fires += len(fires)
                by_scenario[meta.scenario] = by_scenario.get(meta.scenario, 0) + 1
    return {
        "degrading_runs": degrading,
        "caught": caught,
        "catch_rate": caught / degrading if degrading else float("nan"),
        "median_lead_s": float(np.median(leads)) if leads else float("nan"),
        "healthy_runs": healthy_runs,
        "false_alarm_runs": false_runs,
        "false_alarms_per_hour": false_fires / (healthy_seconds / 3600) if healthy_seconds else float("nan"),
        "false_by_scenario": by_scenario,
    }


def rule_score(e: dict) -> tuple:
    """Lower is better. A false handover is a real cost on every occurrence, whereas a missed
    prediction costs nothing beyond what plain Raft already pays, so false alarms come first."""
    return (e["false_alarm_runs"], -e["caught"], -(0 if np.isnan(e["median_lead_s"]) else e["median_lead_s"]))


def tune_rule(frame: pd.DataFrame, runs: pd.DataFrame, probs: np.ndarray) -> tuple[float, int, dict]:
    best = None
    for threshold in THRESHOLDS:
        for consecutive in CONSECUTIVE:
            e = event_metrics(frame, runs, probs, float(threshold), consecutive)
            key = rule_score(e)
            if best is None or key < best[0]:
                best = (key, float(threshold), consecutive, e)
    return best[1], best[2], best[3]


def out_of_fold(est, frame: pd.DataFrame) -> np.ndarray:
    """Predictions for each run from a model that never saw that run."""
    probs = np.zeros(len(frame))
    for tr, te in GroupKFold(n_splits=5).split(frame, frame.label, frame.run_id):
        m = clone(est).fit(frame.iloc[tr][FEATURES], frame.label.iloc[tr])
        probs[te] = m.predict_proba(frame.iloc[te][FEATURES])[:, 1]
    return probs


def export_tree(tree) -> dict:
    t = tree.tree_
    # value holds per-class weighted counts or fractions; normalize to a probability.
    v = t.value[:, 0, :]
    prob = v[:, 1] / v.sum(axis=1)
    return {
        "left": t.children_left.tolist(),
        "right": t.children_right.tolist(),
        "feature": t.feature.tolist(),
        "threshold": t.threshold.tolist(),
        "prob": prob.tolist(),
    }


def export_model(name: str, est, threshold: float, consecutive: int, golden_x: np.ndarray) -> dict:
    m = {"kind": name, "features": FEATURES, "threshold": threshold, "consecutive": consecutive}
    if name == "decision_tree":
        m["trees"] = [export_tree(est)]
    elif name == "random_forest":
        m["trees"] = [export_tree(e) for e in est.estimators_]
    elif name == "logistic_regression":
        scaler, lr = est.named_steps["standardscaler"], est.named_steps["logisticregression"]
        m["mean"], m["scale"] = scaler.mean_.tolist(), scaler.scale_.tolist()
        m["coef"], m["intercept"] = lr.coef_[0].tolist(), float(lr.intercept_[0])
    elif name == "naive_bayes":
        m["theta"], m["var"] = est.theta_.tolist(), est.var_.tolist()
        m["prior"] = est.class_prior_.tolist()
    # scikit-learn's own predictions, for the Go test to check itself against.
    probs = est.predict_proba(pd.DataFrame(golden_x, columns=FEATURES))[:, 1]
    m["golden"] = [{"x": x.tolist(), "p": float(p)} for x, p in zip(golden_x, probs)]
    return m


def main() -> None:
    ap = argparse.ArgumentParser()
    ap.add_argument("--data", default="ml/data", type=pathlib.Path)
    ap.add_argument("--models", default="ml/models", type=pathlib.Path)
    ap.add_argument("--report", default="results/ml-comparison.md", type=pathlib.Path)
    args = ap.parse_args()

    runs, df = load(args.data)
    parts = split_runs(runs)
    sets = {k: df[df.run_id.isin(v)] for k, v in parts.items()}
    assert not (parts["train"] & parts["test"]) and not (parts["train"] & parts["val"]) and not (parts["val"] & parts["test"])

    print(f"{len(runs)} runs, {len(df)} labelled readings, {df.label.mean():.1%} positive")
    for k, v in sets.items():
        print(f"  {k}: {len(parts[k])} runs, {len(v)} readings, {v.label.mean():.1%} positive")

    X = {k: v[FEATURES] for k, v in sets.items()}
    y = {k: v.label.to_numpy() for k, v in sets.items()}

    dev = pd.concat([sets["train"], sets["val"]]).reset_index(drop=True)
    results, fitted = {}, {}
    for name, (est, grid) in candidates().items():
        # GroupKFold keeps each run wholly on one side of every fold.
        search = GridSearchCV(est, grid, scoring="f1", cv=GroupKFold(n_splits=5), n_jobs=2)
        search.fit(X["train"], y["train"], groups=sets["train"].run_id)
        oof = out_of_fold(search.best_estimator_, dev)
        threshold, consecutive, dev_events = tune_rule(dev, runs, oof)
        final = clone(search.best_estimator_).fit(dev[FEATURES], dev.label)
        fitted[name] = (final, threshold, consecutive)
        results[name] = {
            "params": search.best_params_,
            "threshold": threshold,
            "consecutive": consecutive,
            "dev_f1": f1_score(dev.label, oof >= threshold, zero_division=0),
            "dev_events": dev_events,
        }
        print(f"{name}: threshold {threshold}, {consecutive} in a row: {dev_events['caught']}/{dev_events['degrading_runs']} caught, "
              f"{dev_events['median_lead_s']:.1f}s warning, {dev_events['false_alarm_runs']}/{dev_events['healthy_runs']} false-alarm runs "
              f"{dev_events['false_by_scenario']} {search.best_params_}")

    # Choose on development runs, preferring the simpler model among ties: an explainable
    # tree beats a forest unless the forest is actually better.
    order = {"decision_tree": 0, "logistic_regression": 1, "naive_bayes": 2, "random_forest": 3}
    winner = min(results, key=lambda n: (rule_score(results[n]["dev_events"])[:2], order[n]))
    print("deployed (chosen without the test runs):", winner)

    # Report on the test runs.
    for name, (est, threshold, consecutive) in fitted.items():
        p = est.predict_proba(X["test"])[:, 1]
        pred = p >= threshold
        results[name]["test"] = {
            "precision": precision_score(y["test"], pred, zero_division=0),
            "recall": recall_score(y["test"], pred, zero_division=0),
            "f1": f1_score(y["test"], pred, zero_division=0),
            "events": event_metrics(sets["test"], runs, p, threshold, consecutive),
        }

    rng = np.random.default_rng(SEED)
    golden = X["test"].to_numpy()[rng.choice(len(X["test"]), size=min(300, len(X["test"])), replace=False)]
    # Add extreme inputs, which is where differently implemented arithmetic tends to disagree.
    golden = np.vstack([golden, np.zeros((1, 6)), np.full((1, 6), 1e6), X["test"].max().to_numpy()[None, :]])

    args.models.mkdir(parents=True, exist_ok=True)
    for name, (est, threshold, consecutive) in fitted.items():
        (args.models / f"{name}.json").write_text(json.dumps(export_model(name, est, threshold, consecutive, golden)))
    deployed = export_model(winner, *fitted[winner], golden)
    (args.models / "deployed.json").write_text(json.dumps(deployed))

    args.report.parent.mkdir(parents=True, exist_ok=True)
    args.report.write_text(report(runs, df, parts, sets, results, winner, fitted), encoding="utf-8")
    print("wrote", args.report)


def report(runs, df, parts, sets, results, winner, fitted) -> str:
    L = []
    L.append("# Failure predictor comparison\n")
    L.append(f"Deployed model: **{winner.replace('_', ' ')}**, chosen without using the test runs.\n")
    L.append("An earlier version of this pipeline chose by per-reading F1. It scored 0.976 on the test runs, yet false-alarmed in "
             "3 of 14 healthy test runs, all on steady slow networks. The causes were then diagnosed on development runs only, which led "
             "to tuning by per-run outcome and adding the `steady-slow-high` scenario. Those test runs have therefore been seen once, so "
             "their scores below are not perfectly clean, and the live evaluation in `results/evaluation.md`, on fresh runs, is the independent check.\n")
    L.append("## Data\n")
    L.append(f"{len(runs)} runs of real 5-node clusters, giving {len(df)} labelled readings "
             f"({df.label.mean():.1%} positive). Whole runs were assigned to one split, so no run contributes to both training and scoring.\n")
    L.append("| Scenario | Runs | Class |\n|---|---:|---|")
    for s, g in runs.groupby("scenario"):
        cls = "about to fail" if g.degrades.iloc[0] else ("no warning possible, labelled healthy" if g.kills_leader.iloc[0] else "healthy")
        L.append(f"| {s} | {len(g)} | {cls} |")
    L.append("\n| Split | Runs | Readings | Positive |\n|---|---:|---:|---:|")
    for k, v in sets.items():
        L.append(f"| {k} | {len(parts[k])} | {len(v)} | {v.label.mean():.1%} |")

    L.append("\n## Results on held-out test runs\n")
    L.append("Per reading. Each model's threshold was fixed on validation data.\n")
    L.append("| Model | Precision | Recall | F1 | Threshold | Readings in a row |\n|---|---:|---:|---:|---:|---:|")
    for name, r in sorted(results.items(), key=lambda kv: -kv[1]["test"]["f1"]):
        t = r["test"]
        mark = " (deployed)" if name == winner else ""
        L.append(f"| {name.replace('_', ' ')}{mark} | {t['precision']:.3f} | {t['recall']:.3f} | {t['f1']:.3f} | {r['threshold']:.2f} | {r['consecutive']} |")

    L.append("\nPer run, replaying the rule the system really uses (fire after that many readings in a row over the threshold, "
             "with a 5-second cooldown). This is what matters in operation, and what the settings were tuned for.\n")
    L.append("| Model | Failures caught | Median warning | False-alarm runs | False alarms per hour |\n|---|---:|---:|---:|---:|")
    for name, r in sorted(results.items(), key=lambda kv: -kv[1]["test"]["f1"]):
        e = r["test"]["events"]
        L.append(f"| {name.replace('_', ' ')} | {e['caught']}/{e['degrading_runs']} | {e['median_lead_s']:.1f}s | "
                 f"{e['false_alarm_runs']}/{e['healthy_runs']} | {e['false_alarms_per_hour']:.1f} |")

    L.append("\nThe test split is small, because runs are expensive and splitting by run is what makes the score honest. "
             "So small differences between models here are not meaningful. The live evaluation in `results/evaluation.md` is the larger test.\n")

    est = fitted["decision_tree"][0]
    L.append("## The decision tree, as rules\n")
    L.append("The reason to prefer an interpretable model: this is the entire reasoning, and it can be read out loud.\n")
    L.append("```\n" + export_text(est, feature_names=FEATURES, class_names=["healthy", "failing"]) + "```\n")

    forest = fitted["random_forest"][0]
    L.append("## What the forest relied on\n")
    L.append("| Feature | Importance |\n|---|---:|")
    for f, v in sorted(zip(FEATURES, forest.feature_importances_), key=lambda kv: -kv[1]):
        L.append(f"| {f} | {v:.3f} |")

    L.append("\n## Chosen settings\n")
    L.append("Per-run results are out-of-fold across the development (training plus validation) runs.\n")
    L.append("| Model | Hyperparameters | Caught | False-alarm runs | Median warning |\n|---|---|---:|---:|---:|")
    for name, r in results.items():
        e = r["dev_events"]
        L.append(f"| {name.replace('_', ' ')} | `{r['params']}` | {e['caught']}/{e['degrading_runs']} | "
                 f"{e['false_alarm_runs']}/{e['healthy_runs']} | {e['median_lead_s']:.1f}s |")
    return "\n".join(L) + "\n"


if __name__ == "__main__":
    main()
