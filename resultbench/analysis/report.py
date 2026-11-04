"""Aggregate repetitions and draw the figures for one or more experiments.

    python -m analysis.report runs/core                 # tables + figures in runs/core/report/
    python -m analysis.report runs/core --metric success_5s --metric cpu_s_per_1k_served

Each configuration (design, label, load multiple) is summarised by the
median over repetitions with a 95% percentile-bootstrap interval. Runs where
the load generator saturated are excluded and counted.
"""
from __future__ import annotations

import argparse
from pathlib import Path

import matplotlib

matplotlib.use("Agg")
import matplotlib.pyplot as plt  # noqa: E402
import numpy as np  # noqa: E402
import pandas as pd  # noqa: E402

from analysis import collect, stats  # noqa: E402

# Metrics reported per experiment (plan section 7.6). Missing columns are skipped.
DEFAULT_METRICS = {
    "core": ["success_2s", "success_5s", "success_10s", "served_ok", "latency_p99_ms", "cpu_s_per_1k_served",
             "peak_mem_mb", "db_queries_per_user", "prep_seconds"],
    "cache": ["success_5s", "db_queries_per_user", "portal_cache_miss_first_time_key", "portal_cache_miss_repeat_key",
              "portal_cache_miss_concurrent_duplicate", "portal_coalesced", "latency_p99_ms"],
    "waiting-room": ["success_10s", "served_ok", "wait_p50_ms", "wait_p95_ms", "kendall_tau", "kendall_tau_prerelease",
                     "refresh_penalty_ms", "backend_amplification", "cpu_s_per_1k_served", "wr_abandoned"],
    "refresh": ["amplification", "backend_amplification", "tail_amplification", "recovery_s", "success_10s",
                "served_ok"],
    "correctness": ["wrong", "stale", "missing", "soft_failures", "staleness_window_s", "served_ok"],
    "faults": ["recovery_s", "peak_failure_share", "success_10s", "served_ok"],
}
# Second paper: waiting-room mechanisms.
_P2 = ["served_ok", "success_10s", "unfinished_share", "wait_p50_ms", "wait_p95_ms", "time_to_result_p95_ms",
       "kendall_tau_prerelease", "backend_amplification", "cpu_s_per_1k_served", "wr_lease_expired", "wr_readmitted"]
DEFAULT_METRICS.update({
    "p2-lease": _P2,
    "p2-adaptive": _P2 + ["wr_rate_mean", "wr_rate_min", "wr_rate_max", "wr_rate_decreases"],
    "p2-eta": _P2 + ["queue_gave_up_share", "queue_gave_up_wait_p50_ms", "gave_up_queue", "gave_up_queue_eta"],
    "p2-bots": _P2 + ["bot_requests", "bot_backend_requests", "bot_fetched_share"],
    "p2-loads": _P2 + ["latency_p99_ms"],
})
FALLBACK_METRICS = ["success_5s", "served_ok", "latency_p99_ms", "amplification", "cpu_s_per_1k_served", "wrong", "stale"]

GROUP = ["design", "label", "multiple"]


def summarise(df: pd.DataFrame, metrics: list[str]) -> pd.DataFrame:
    rows = []
    for key, g in df.groupby(GROUP, dropna=False):
        row = dict(zip(GROUP, key))
        row["reps"] = len(g)
        for m in metrics:
            if m not in g:
                continue
            est, lo, hi = stats.bootstrap_ci(g[m].astype(float).to_numpy())
            row[m], row[f"{m}_lo"], row[f"{m}_hi"] = est, lo, hi
        rows.append(row)
    return pd.DataFrame(rows).sort_values(GROUP, na_position="first")


def series_name(design: str, label: str) -> str:
    return design if not label or label == "nan" else f"{design} ({label})"


def plot_metric(summary: pd.DataFrame, metric: str, out: Path, title: str) -> None:
    if metric not in summary or summary[metric].isna().all():
        return
    fig, ax = plt.subplots(figsize=(7, 4.2))
    has_x = summary["multiple"].notna().any() and summary["multiple"].nunique() > 1
    if has_x:
        for (design, label), g in summary.groupby(["design", "label"], dropna=False):
            g = g.sort_values("multiple")
            y = g[metric].to_numpy(float)
            yerr = np.vstack([y - g[f"{metric}_lo"].to_numpy(float), g[f"{metric}_hi"].to_numpy(float) - y])
            ax.errorbar(g["multiple"], y, yerr=np.nan_to_num(yerr), marker="o", capsize=3,
                        label=series_name(design, str(label)))
        ax.set_xscale("log")
        ticks = sorted(summary["multiple"].dropna().unique())
        ax.set_xticks(ticks)
        ax.set_xticklabels([f"{t:g}x" for t in ticks])
        ax.set_xlabel("peak arrivals as a multiple of design A's capacity")
    else:
        names = [series_name(d, str(l)) for d, l in zip(summary["design"], summary["label"])]
        y = summary[metric].to_numpy(float)
        yerr = np.vstack([y - summary[f"{metric}_lo"].to_numpy(float), summary[f"{metric}_hi"].to_numpy(float) - y])
        ax.bar(range(len(y)), y, yerr=np.nan_to_num(yerr), capsize=3)
        ax.set_xticks(range(len(y)))
        ax.set_xticklabels(names, rotation=30, ha="right")
    ax.set_ylabel(metric)
    ax.set_title(title)
    ax.grid(alpha=0.3)
    if has_x:
        ax.legend(fontsize=8)
    fig.tight_layout()
    fig.savefig(out / f"{metric}.png", dpi=150)
    plt.close(fig)


def to_markdown(summary: pd.DataFrame, metrics: list[str]) -> str:
    cols = [m for m in metrics if m in summary]
    head = "| design | label | multiple | reps | " + " | ".join(cols) + " |"
    sep = "|" + "---|" * (4 + len(cols))
    lines = [head, sep]
    for _, r in summary.iterrows():
        cells = []
        for m in cols:
            v, lo, hi = r[m], r.get(f"{m}_lo"), r.get(f"{m}_hi")
            if pd.isna(v):
                cells.append("–")
            elif lo == hi:
                cells.append(f"{v:.4g}")
            else:
                cells.append(f"{v:.4g} [{lo:.3g}, {hi:.3g}]")
        mult = "" if pd.isna(r["multiple"]) else f"{r['multiple']:g}x"
        label = "" if pd.isna(r["label"]) else r["label"]
        lines.append(f"| {r['design']} | {label} | {mult} | {r['reps']} | " + " | ".join(cells) + " |")
    return "\n".join(lines) + "\n"


def report(exp_dir: Path, metrics: list[str] | None = None) -> Path:
    runs_csv = exp_dir / "runs.csv"
    df = pd.read_csv(runs_csv) if runs_csv.exists() else collect.collect(exp_dir)
    if df.empty:
        raise SystemExit(f"no runs in {exp_dir}")
    df["label"] = df["label"].fillna("")
    metrics = metrics or DEFAULT_METRICS.get(exp_dir.name, FALLBACK_METRICS)
    saturated = int(df["generator_saturated"].sum())
    crashed = int(df.get("crashed", pd.Series(dtype=bool)).fillna(False).sum())
    clean = df[~df["generator_saturated"]]
    summary = summarise(clean, metrics)
    out = exp_dir / "report"
    out.mkdir(exist_ok=True)
    summary.to_csv(out / "summary.csv", index=False)
    for m in metrics:
        plot_metric(summary, m, out, f"{exp_dir.name}: {m}")
    md = [f"# {exp_dir.name}\n",
          f"{len(df)} runs; {saturated} excluded because the load generator saturated; "
          f"{crashed} runs had a crashed or OOM-killed service (kept: crashing is an outcome).\n",
          "Values are medians over repetitions with 95% bootstrap intervals.\n",
          to_markdown(summary, metrics)]
    md += [f"![{m}]({m}.png)\n" for m in metrics if (out / f"{m}.png").exists()]
    (out / "README.md").write_text("\n".join(md))
    return out


def main() -> None:
    p = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    p.add_argument("experiments", nargs="+", type=Path)
    p.add_argument("--metric", action="append", help="metric to report (repeatable)")
    p.add_argument("--recollect", action="store_true", help="rebuild runs.csv first")
    a = p.parse_args()
    for exp in a.experiments:
        if a.recollect or not (exp / "runs.csv").exists():
            collect.collect(exp).to_csv(exp / "runs.csv", index=False)
        print(f"report: {report(exp, a.metric)}")


if __name__ == "__main__":
    main()
