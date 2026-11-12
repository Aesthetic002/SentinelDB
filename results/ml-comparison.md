# Failure predictor comparison

Deployed model: **random forest**, chosen without using the test runs.

An earlier version of this pipeline chose by per-reading F1. It scored 0.976 on the test runs, yet false-alarmed in 3 of 14 healthy test runs, all on steady slow networks. The causes were then diagnosed on development runs only, which led to tuning by per-run outcome and adding the `steady-slow-high` scenario. Those test runs have therefore been seen once, so their scores below are not perfectly clean, and the live evaluation in `results/evaluation.md`, on fresh runs, is the independent check.

## Data

144 runs of real 5-node clusters, giving 13789 labelled readings (24.6% positive). Whole runs were assigned to one split, so no run contributes to both training and scoring.

| Scenario | Runs | Class |
|---|---:|---|
| blip | 10 | healthy |
| degrade-delay | 16 | about to fail |
| degrade-jitter | 16 | about to fail |
| degrade-loss | 16 | about to fail |
| degrade-mixed | 16 | about to fail |
| follower-kill | 8 | healthy |
| hard-kill | 8 | no warning possible, labelled healthy |
| healthy | 10 | healthy |
| one-bad-link | 10 | healthy |
| steady-slow | 10 | healthy |
| steady-slow-high | 16 | healthy |
| two-follower-kill | 8 | healthy |

| Split | Runs | Readings | Positive |
|---|---:|---:|---:|
| train | 86 | 8178 | 25.9% |
| val | 29 | 2805 | 22.0% |
| test | 29 | 2806 | 23.2% |

## Results on held-out test runs

Per reading. Each model's threshold was fixed on validation data.

| Model | Precision | Recall | F1 | Threshold | Readings in a row |
|---|---:|---:|---:|---:|---:|
| random forest (deployed) | 0.972 | 0.971 | 0.972 | 0.75 | 10 |
| decision tree | 0.921 | 0.985 | 0.952 | 0.30 | 30 |
| logistic regression | 0.861 | 0.951 | 0.904 | 0.45 | 35 |
| naive bayes | 0.990 | 0.580 | 0.731 | 0.95 | 20 |

Per run, replaying the rule the system really uses (fire after that many readings in a row over the threshold, with a 5-second cooldown). This is what matters in operation, and what the settings were tuned for.

| Model | Failures caught | Median warning | False-alarm runs | False alarms per hour |
|---|---:|---:|---:|---:|
| random forest | 12/12 | 5.0s | 0/17 | 0.0 |
| decision tree | 12/12 | 2.8s | 0/17 | 0.0 |
| logistic regression | 11/12 | 1.8s | 0/17 | 0.0 |
| naive bayes | 7/12 | 3.2s | 0/17 | 0.0 |

The test split is small, because runs are expensive and splitting by run is what makes the score honest. So small differences between models here are not meaningful. The live evaluation in `results/evaluation.md` is the larger test.

## The decision tree, as rules

The reason to prefer an interpretable model: this is the entire reasoning, and it can be read out loud.

```
|--- rtt_slope_ms_s <= 11.56
|   |--- miss_ratio <= 0.03
|   |   |--- rtt_jitter_ms <= 41.11
|   |   |   |--- rtt_slope_ms_s <= 7.62
|   |   |   |   |--- miss_ratio <= 0.01
|   |   |   |   |   |--- class: healthy
|   |   |   |   |--- miss_ratio >  0.01
|   |   |   |   |   |--- class: healthy
|   |   |   |--- rtt_slope_ms_s >  7.62
|   |   |   |   |--- rtt_jitter_ms <= 27.37
|   |   |   |   |   |--- class: healthy
|   |   |   |   |--- rtt_jitter_ms >  27.37
|   |   |   |   |   |--- class: failing
|   |   |--- rtt_jitter_ms >  41.11
|   |   |   |--- rtt_recent_ms <= 158.85
|   |   |   |   |--- rtt_recent_ms <= 53.32
|   |   |   |   |   |--- class: healthy
|   |   |   |   |--- rtt_recent_ms >  53.32
|   |   |   |   |   |--- class: healthy
|   |   |   |--- rtt_recent_ms >  158.85
|   |   |   |   |--- rtt_mean_ms <= 183.13
|   |   |   |   |   |--- class: failing
|   |   |   |   |--- rtt_mean_ms >  183.13
|   |   |   |   |   |--- class: failing
|   |--- miss_ratio >  0.03
|   |   |--- rtt_mean_ms <= 304.77
|   |   |   |--- rtt_recent_ms <= 228.54
|   |   |   |   |--- rtt_recent_ms <= 3.72
|   |   |   |   |   |--- class: failing
|   |   |   |   |--- rtt_recent_ms >  3.72
|   |   |   |   |   |--- class: failing
|   |   |   |--- rtt_recent_ms >  228.54
|   |   |   |   |--- class: failing
|   |   |--- rtt_mean_ms >  304.77
|   |   |   |--- rtt_jitter_ms <= 27.07
|   |   |   |   |--- class: healthy
|   |   |   |--- rtt_jitter_ms >  27.07
|   |   |   |   |--- class: failing
|--- rtt_slope_ms_s >  11.56
|   |--- rtt_slope_ms_s <= 83.47
|   |   |--- rtt_mean_ms <= 32.12
|   |   |   |--- miss_ratio <= 0.01
|   |   |   |   |--- rtt_max_ms <= 86.85
|   |   |   |   |   |--- class: healthy
|   |   |   |   |--- rtt_max_ms >  86.85
|   |   |   |   |   |--- class: healthy
|   |   |   |--- miss_ratio >  0.01
|   |   |   |   |--- rtt_recent_ms <= 50.12
|   |   |   |   |   |--- class: failing
|   |   |   |   |--- rtt_recent_ms >  50.12
|   |   |   |   |   |--- class: failing
|   |   |--- rtt_mean_ms >  32.12
|   |   |   |--- rtt_recent_ms <= 55.26
|   |   |   |   |--- rtt_slope_ms_s <= 15.35
|   |   |   |   |   |--- class: healthy
|   |   |   |   |--- rtt_slope_ms_s >  15.35
|   |   |   |   |   |--- class: healthy
|   |   |   |--- rtt_recent_ms >  55.26
|   |   |   |   |--- rtt_slope_ms_s <= 66.50
|   |   |   |   |   |--- class: failing
|   |   |   |   |--- rtt_slope_ms_s >  66.50
|   |   |   |   |   |--- class: failing
|   |--- rtt_slope_ms_s >  83.47
|   |   |--- rtt_max_ms <= 417.89
|   |   |   |--- rtt_recent_ms <= 118.56
|   |   |   |   |--- class: healthy
|   |   |   |--- rtt_recent_ms >  118.56
|   |   |   |   |--- class: healthy
|   |   |--- rtt_max_ms >  417.89
|   |   |   |--- class: failing
```

## What the forest relied on

| Feature | Importance |
|---|---:|
| miss_ratio | 0.288 |
| rtt_slope_ms_s | 0.271 |
| rtt_jitter_ms | 0.193 |
| rtt_recent_ms | 0.108 |
| rtt_mean_ms | 0.104 |
| rtt_max_ms | 0.036 |

## Chosen settings

Per-run results are out-of-fold across the development (training plus validation) runs.

| Model | Hyperparameters | Caught | False-alarm runs | Median warning |
|---|---|---:|---:|---:|
| decision tree | `{'max_depth': 5, 'min_samples_leaf': 10}` | 51/52 | 0/63 | 2.7s |
| naive bayes | `{'var_smoothing': 1e-09}` | 30/52 | 0/63 | 3.3s |
| logistic regression | `{'logisticregression__C': 0.1}` | 46/52 | 0/63 | 1.7s |
| random forest | `{'max_depth': 8, 'min_samples_leaf': 5, 'n_estimators': 50}` | 52/52 | 0/63 | 4.4s |
