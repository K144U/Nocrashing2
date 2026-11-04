"""Statistics used by the analysis: bootstrap intervals, fairness, recovery
and staleness. Kept free of plotting so it can be unit-tested."""
from __future__ import annotations

import numpy as np
import pandas as pd
from scipy import stats as sps


def bootstrap_ci(values, stat=np.median, n_boot: int = 5000, level: float = 0.95, seed: int = 0):
    """Percentile bootstrap interval for a statistic of independent repetitions.

    Returns (estimate, low, high). With fewer than two values the interval
    collapses to the single value.
    """
    v = np.asarray([x for x in values if x is not None and not np.isnan(x)], dtype=float)
    if len(v) == 0:
        return (np.nan, np.nan, np.nan)
    est = float(stat(v))
    if len(v) < 2:
        return (est, est, est)
    rng = np.random.default_rng(seed)
    idx = rng.integers(0, len(v), size=(n_boot, len(v)))
    boots = np.apply_along_axis(stat, 1, v[idx])
    lo, hi = np.quantile(boots, [(1 - level) / 2, 1 - (1 - level) / 2])
    return (est, float(lo), float(hi))


def fairness(users: pd.DataFrame) -> dict:
    """How closely service order follows arrival order.

    Uses users who reached the backend (admitted_ms >= 0). Kendall's tau of
    arrival time vs admission time is 1 for perfect first-come-first-served
    and near 0 for random order. Also reports how much longer students who
    pressed refresh waited than those who did not.
    """
    if "bot" in users:
        users = users[~users["bot"].astype(bool)]
    u = users[(users["admitted_ms"] >= 0)].copy()
    out = {"fair_n": len(u), "kendall_tau": np.nan, "kendall_tau_prerelease": np.nan,
           "refresh_penalty_ms": np.nan, "wait_p50_ms": np.nan, "wait_p95_ms": np.nan}
    if len(u) < 3:
        return out
    first = u["first_try_ms"] if "first_try_ms" in u else pd.Series(-1, index=u.index)
    start = np.where(first >= 0, first, u["arrival_ms"].clip(lower=0))
    u["wait_ms"] = u["admitted_ms"] - start
    out["wait_p50_ms"] = float(u["wait_ms"].quantile(0.5))
    out["wait_p95_ms"] = float(u["wait_ms"].quantile(0.95))
    sample = u.sample(min(len(u), 20000), random_state=0)
    out["kendall_tau"] = float(sps.kendalltau(sample["arrival_ms"], sample["admitted_ms"]).statistic)
    pre = u[u["arrival_ms"] < 0]
    if len(pre) >= 3:
        pre = pre.sample(min(len(pre), 20000), random_state=0)
        out["kendall_tau_prerelease"] = float(sps.kendalltau(pre["arrival_ms"], pre["admitted_ms"]).statistic)
    r = u[u["manual_refreshes"] > 0]["wait_ms"]
    n = u[u["manual_refreshes"] == 0]["wait_ms"]
    if len(r) and len(n):
        out["refresh_penalty_ms"] = float(r.median() - n.median())
    return out


FAILURES = ["missing", "soft_failure", "rejected", "server_error", "client_error", "timeout", "net_error"]


def recovery(ts: pd.DataFrame, rise_s: float, window: int = 10, threshold: float = 0.01) -> dict:
    """Recovery after the spike, from the per-second time series.

    failure_share(t) = failed / backend requests in second t. Recovery time is
    the first second after the rise at which the failure share stays below
    `threshold` for `window` consecutive seconds. tail_amplification compares
    backend requests to new arrivals over the last quarter of the run: values
    well above 1 after arrivals have dropped indicate a self-sustaining
    (metastable) overload.
    """
    ts = ts.sort_values("t_s")
    fail = ts[[c for c in FAILURES if c in ts]].sum(axis=1)
    backend = ts["backend"].replace(0, np.nan)
    share = (fail / backend).fillna(0)
    post = ts["t_s"] >= rise_s
    out = {"peak_failure_share": float(share[post].max()) if post.any() else np.nan,
           "recovery_s": np.nan, "never_recovered": True, "tail_amplification": np.nan}
    t_post = ts["t_s"][post].to_numpy()
    s_post = share[post].to_numpy()
    for i in range(len(s_post) - window + 1):
        if (s_post[i:i + window] < threshold).all():
            out["recovery_s"] = float(t_post[i])
            out["never_recovered"] = False
            break
    if len(ts) >= 8:
        tail = ts[ts["t_s"] >= ts["t_s"].quantile(0.75)]
        arrivals = tail["arrivals"].sum()
        if arrivals > 0:
            out["tail_amplification"] = float(tail["backend"].sum() / arrivals)
    return out


def staleness(requests: pd.DataFrame, corrections_at_ms: float | None) -> dict:
    """How long stale results were served after corrections were published."""
    out = {"stale_responses": int((requests["outcome"] == "stale").sum()), "staleness_window_s": np.nan}
    if corrections_at_ms is None:
        return out
    st = requests[requests["outcome"] == "stale"]
    out["staleness_window_s"] = float((st["t_ms"].max() - corrections_at_ms) / 1000) if len(st) else 0.0
    return out
