"""Turn the study's event log into per-condition statistics and suggested
values for ResultBench's student model (`users:` in a scenario file).

    python3 analyze.py events.jsonl              # tables on stdout
    python3 analyze.py events.jsonl --yaml out.yaml

Definitions (all times in seconds from the participant's first result request):

- end: the first "result_shown" or "leave" event; if neither, the last event
  of a participant who closed the page ("closed").
- reload: a page load after the first one in the tab (n_load > 1).
- patience sample (slow condition): time from a result request to a reload or
  "leave" while that request was still pending.
- retry delay (error condition): time from an error answer to the next request.
- giving up: "leave" or "closed" before the result. Participants who reached
  the result are censored at that time; waiting times are summarised with a
  Kaplan-Meier estimate.

Standard library only.
"""
from __future__ import annotations

import argparse
import json
import math
from collections import defaultdict
from pathlib import Path


def load(path: Path) -> dict[str, list[dict]]:
    by_p: dict[str, list[dict]] = defaultdict(list)
    for line in path.read_text().splitlines():
        if line.strip():
            ev = json.loads(line)
            if ev.get("p"):
                by_p[ev["p"]].append(ev)
    for evs in by_p.values():
        evs.sort(key=lambda e: e["server_t"])
    return by_p


def participant(evs: list[dict]) -> dict | None:
    reqs = [e for e in evs if e["type"] == "request"]
    if not reqs:
        return None
    t0 = reqs[0]["server_t"]
    cond = reqs[0]["c"]
    end, outcome = None, "closed"
    for e in evs:
        if e["type"] in ("result_shown", "leave"):
            end, outcome = e["server_t"], "result" if e["type"] == "result_shown" else "leave"
            break
    if end is None:
        end = max(e["server_t"] for e in evs)
    waiting = [e for e in evs if t0 <= e["server_t"] <= end]
    reloads = [e["server_t"] - t0 for e in waiting if e["type"] == "load" and (e.get("n_load") or 0) > 1]
    retries = [e["server_t"] - t0 for e in waiting if e["type"] == "retry_click"]

    patience, retry_delays, errors, errors_retried = [], [], 0, 0
    for i, r in enumerate(reqs):
        if r["server_t"] > end:
            break
        answered = r["server_t"] + r.get("delay_s", 0)
        nxt = next((e for e in evs if e["server_t"] > r["server_t"] and
                    (e["type"] == "request" or (e["type"] == "load" and (e.get("n_load") or 0) > 1)
                     or e["type"] in ("leave", "result_shown"))), None)
        if cond == "slow" and r["status"] != 200 and nxt is not None and nxt["server_t"] < answered:
            patience.append(nxt["server_t"] - r["server_t"])
        if r["status"] >= 500:
            errors += 1
            later = next((e for e in reqs[i + 1:] if e["server_t"] >= answered), None)
            if later is not None and later["server_t"] <= end:
                errors_retried += 1
                retry_delays.append(later["server_t"] - answered)
    survey = next((e.get("answers") for e in evs if e["type"] == "survey"), None)
    return {"condition": cond, "outcome": outcome, "duration": end - t0, "reloads": reloads, "retries": retries,
            "patience": patience, "retry_delays": retry_delays, "errors": errors, "errors_retried": errors_retried,
            "survey": survey}


def quantile(xs: list[float], q: float) -> float:
    if not xs:
        return math.nan
    s = sorted(xs)
    pos = q * (len(s) - 1)
    lo, hi = math.floor(pos), math.ceil(pos)
    return s[lo] + (s[hi] - s[lo]) * (pos - lo)


def kaplan_meier(times: list[float], events: list[bool]) -> list[tuple[float, float]]:
    """Survival curve [(t, S(t))] of time to giving up; events False = censored."""
    pts = sorted(zip(times, events))
    at_risk, s, curve = len(pts), 1.0, [(0.0, 1.0)]
    i = 0
    while i < len(pts):
        t = pts[i][0]
        d = n = 0
        while i < len(pts) and pts[i][0] == t:
            d += pts[i][1]
            n += 1
            i += 1
        if d:
            s *= 1 - d / at_risk
            curve.append((t, s))
        at_risk -= n
    return curve


def km_quantile(curve: list[tuple[float, float]], q: float) -> float:
    """Time by which a share q has given up (nan if never reached)."""
    for t, s in curve:
        if 1 - s >= q:
            return t
    return math.nan


def summarise(parts: list[dict]) -> dict:
    out = {"n": len(parts)}
    if not parts:
        return out
    dur = [p["duration"] for p in parts]
    gave_up = [p["outcome"] != "result" for p in parts]
    curve = kaplan_meier(dur, gave_up)
    minutes = sum(dur) / 60
    out.update({
        "gave_up_share": sum(gave_up) / len(parts),
        "give_up_p25_s": km_quantile(curve, 0.25), "give_up_p50_s": km_quantile(curve, 0.5),
        "reloads_per_min": sum(len(p["reloads"]) for p in parts) / minutes if minutes else math.nan,
        "first_reload_p50_s": quantile([p["reloads"][0] for p in parts if p["reloads"]], 0.5),
        "share_who_reloaded": sum(bool(p["reloads"]) for p in parts) / len(parts),
    })
    pat = [x for p in parts for x in p["patience"]]
    if pat:
        out.update({"patience_n": len(pat), "patience_p10_s": quantile(pat, 0.1), "patience_p90_s": quantile(pat, 0.9)})
    errs = sum(p["errors"] for p in parts)
    if errs:
        delays = [x for p in parts for x in p["retry_delays"]]
        out.update({"errors": errs, "retry_after_error_share": sum(p["errors_retried"] for p in parts) / errs,
                    "retry_delay_p10_s": quantile(delays, 0.1), "retry_delay_p90_s": quantile(delays, 0.9)})
    return out


def suggest(by_cond: dict[str, dict]) -> dict:
    """Map the measurements onto ResultBench's `users:` parameters."""
    users, notes = {}, []
    slow, error = by_cond.get("slow", {}), by_cond.get("error", {})
    queue, eta = by_cond.get("queue", {}), by_cond.get("queue-eta", {})
    if not math.isnan(slow.get("patience_p10_s", math.nan)):
        users["patience_s"] = [round(slow["patience_p10_s"], 1), round(slow["patience_p90_s"], 1)]
    if "retry_after_error_share" in error:
        users["retry_prob"] = round(error["retry_after_error_share"], 2)
        if not math.isnan(error["retry_delay_p10_s"]):
            users["retry_delay_s"] = [round(error["retry_delay_p10_s"], 1), round(error["retry_delay_p90_s"], 1)]
    if "reloads_per_min" in queue:
        users["queue_manual_refresh_per_min"] = round(queue["reloads_per_min"], 2)
    p25, p50 = queue.get("give_up_p25_s", math.nan), queue.get("give_up_p50_s", math.nan)
    if not math.isnan(p25):
        hi = p50 if not math.isnan(p50) else None
        users["queue_patience_s"] = [round(p25, 0), round(hi, 0)] if hi else [round(p25, 0), round(p25 * 4, 0)]
        if hi is None:
            notes.append("fewer than half of queued participants gave up before the result: the upper bound of "
                         "queue_patience_s is a guess (4x the 25th percentile)")
    if queue and eta and not math.isnan(queue.get("give_up_p50_s", math.nan)) and not math.isnan(eta.get("give_up_p50_s", math.nan)):
        notes.append(f"median give-up time: {queue['give_up_p50_s']:.0f} s without an estimate, "
                     f"{eta['give_up_p50_s']:.0f} s with one")
    return {"users": users, "notes": notes}


def to_yaml(d: dict) -> str:
    lines = ["# Suggested by behaviour-study/analyze.py; check before use.", "users:"]
    for k, v in d["users"].items():
        lines.append(f"  {k}: {json.dumps(v)}")
    for n in d["notes"]:
        lines.append(f"# note: {n}")
    return "\n".join(lines) + "\n"


def main() -> None:
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("log", type=Path)
    ap.add_argument("--yaml", type=Path, help="write suggested scenario parameters here")
    a = ap.parse_args()
    parts = [p for p in (participant(e) for e in load(a.log).values()) if p]
    by_cond = {c: summarise([p for p in parts if p["condition"] == c])
               for c in sorted({p["condition"] for p in parts})}
    for c, s in by_cond.items():
        print(f"\n[{c}]")
        for k, v in s.items():
            print(f"  {k:28s} {v:.2f}" if isinstance(v, float) else f"  {k:28s} {v}")
    sug = suggest(by_cond)
    print("\n" + to_yaml(sug))
    if a.yaml:
        a.yaml.write_text(to_yaml(sug))


if __name__ == "__main__":
    main()
