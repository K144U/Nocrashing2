"""Collect every run of an experiment into one table (runs.csv).

    python -m analysis.collect runs/core            # writes runs/core/runs.csv
    python -m analysis.collect runs/core runs/cache  # several experiments
"""
from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path

import numpy as np
import pandas as pd

from analysis import stats

PORTAL_COUNTERS = ["db_queries", "pages_built", "cache_hits", "cache_miss_first_time_key", "cache_miss_repeat_key",
                   "cache_miss_concurrent_duplicate", "coalesced", "xfetch_early_refresh", "redis_errors",
                   "db_errors", "responses_503_queue_full", "dropped_before_start"]
WR_COUNTERS = ["admitted", "abandoned", "tickets_created", "queued_responses", "proxied", "proxy_errors",
               "client_canceled", "fast_path_admits", "lease_expired", "readmitted", "rate_increases",
               "rate_decreases"]


def _read_json(p: Path) -> dict:
    return json.loads(p.read_text()) if p.exists() else {}


def load_run(run_dir: Path, details: bool = True) -> dict | None:
    s = _read_json(run_dir / "load" / "summary.json")
    if not s:
        return None
    meta = _read_json(run_dir / "meta.json")
    opts = meta.get("options", {})
    sc = s["scenario"]
    row = {
        "experiment": run_dir.parent.name, "run": run_dir.name, "design": meta.get("design"),
        "label": opts.get("label") or "", "variant": opts.get("variant", ""), "rep": opts.get("rep"),
        "multiple": opts.get("multiple"), "peak_users_per_s": sc["surge"]["peak_users_per_s"],
        "key_dist": sc["keys"]["dist"], "commit": meta.get("commit"),
        "users": s["users"], "dropped_users": s["dropped_users"], "unfinished_users": s["unfinished_users"],
        "served_ok": s["user_served_ok"], "requests": s["requests"], "backend_requests": s["backend_requests"],
        "amplification": s["amplification"], "backend_amplification": s["backend_amplification"],
        "wrong": s["wrong_requests"], "stale": s["stale_requests"], "missing": s["missing_requests"],
        "soft_failures": s["soft_failures"],
        "latency_p50_ms": s["latency_all_ms"]["p50"], "latency_p95_ms": s["latency_all_ms"]["p95"],
        "latency_p99_ms": s["latency_all_ms"]["p99"],
        "time_to_result_p50_ms": s["time_to_result_ms"]["p50"], "time_to_result_p95_ms": s["time_to_result_ms"]["p95"],
        "generator_saturated": s["generator"]["saturated"], "generator_cpu_max": s["generator"]["cpu_max_cores"],
    }
    for k, v in s["user_success_within"].items():
        row[f"success_{k}"] = v
    # Fields added for the second paper (absent in older runs).
    if s["users"]:
        row["unfinished_share"] = s["unfinished_users"] / s["users"]
        row["queue_gave_up_share"] = s.get("queue_gave_up", 0) / s["users"]
    row["queue_gave_up_wait_p50_ms"] = s.get("queue_gave_up_wait_ms", {}).get("p50", np.nan)
    for reason, n in s.get("user_gave_up", {}).items():
        row[f"gave_up_{reason}"] = n / s["users"] if s["users"] else np.nan
    bots = s.get("bots") or {}
    row["bot_users"] = bots.get("users", 0)
    row["bot_requests"] = bots.get("requests", 0)
    row["bot_backend_requests"] = bots.get("backend_requests", 0)
    row["bot_fetched_share"] = bots.get("fetched_share", np.nan)
    for k, v in s["requests_by_outcome"].items():
        row[f"req_{k}"] = v
    res = meta.get("resources", {})
    row["cpu_seconds"] = res.get("_total", {}).get("cpu_seconds", np.nan)
    row["peak_mem_mb"] = res.get("_total", {}).get("peak_mem_mb", np.nan)
    served = s["user_served_ok"] * s["users"]
    row["cpu_s_per_1k_served"] = row["cpu_seconds"] / served * 1000 if served else np.nan
    for svc, r in res.items():
        if not svc.startswith("_"):
            row[f"cpu_s_{svc}"] = r.get("cpu_seconds")
    mat = meta.get("materialize") or {}
    row["prep_seconds"] = (mat.get("before_release") or mat).get("seconds", 0.0) if isinstance(mat, dict) else 0.0
    row["crashed"] = any(c.get("oom_killed") or c.get("state") != "running"
                         for c in meta.get("containers", {}).values()) if meta.get("containers") else False

    portal = {}
    for f in (run_dir / "stats").glob("portal*.json"):
        for k, v in _read_json(f).items():
            portal[k] = portal.get(k, 0) + v
    for k in PORTAL_COUNTERS:
        row[f"portal_{k}"] = portal.get(k, 0)
    if s["users"]:
        row["db_queries_per_user"] = portal.get("db_queries", 0) / s["users"]
    wr = _read_json(run_dir / "stats" / "wr.json")
    for k in WR_COUNTERS:
        row[f"wr_{k}"] = wr.get(k, np.nan) if wr else np.nan
    ratelog = run_dir / "stats" / "wr_ratelog.json"
    if ratelog.exists():
        points = json.loads(ratelog.read_text() or "[]") or []
        rates = [p["rate"] for p in points]
        if rates:
            row["wr_rate_mean"] = float(np.mean(rates))
            row["wr_rate_min"] = float(np.min(rates))
            row["wr_rate_max"] = float(np.max(rates))

    if details:
        ts_file = run_dir / "load" / "timeseries.csv"
        if ts_file.exists():
            row.update(stats.recovery(pd.read_csv(ts_file), sc["surge"].get("rise_s", 5)))
        users_file = run_dir / "load" / "users.csv.gz"
        if users_file.exists():
            row.update(stats.fairness(pd.read_csv(users_file)))
        if sc.get("corrections", {}).get("enabled"):
            req = pd.read_csv(run_dir / "load" / "requests.csv.gz", usecols=["t_ms", "outcome"])
            row.update(stats.staleness(req, sc["corrections"]["at_s"] * 1000))
    return row


def collect(exp_dir: Path, details: bool = True) -> pd.DataFrame:
    rows = []
    for run_dir in sorted(p for p in exp_dir.iterdir() if p.is_dir()):
        try:
            row = load_run(run_dir, details)
        except Exception as e:  # one broken run must not stop the analysis
            print(f"skipping {run_dir}: {e}", file=sys.stderr)
            continue
        if row:
            rows.append(row)
    return pd.DataFrame(rows)


def main() -> None:
    p = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    p.add_argument("experiments", nargs="+", type=Path)
    p.add_argument("--no-details", action="store_true", help="skip per-request files (faster)")
    a = p.parse_args()
    for exp in a.experiments:
        df = collect(exp, not a.no_details)
        out = exp / "runs.csv"
        df.to_csv(out, index=False)
        print(f"{exp}: {len(df)} runs -> {out}")


if __name__ == "__main__":
    main()
