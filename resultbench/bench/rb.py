#!/usr/bin/env python3
"""ResultBench experiment runner.

Brings up one design with Docker Compose under its resource budget, prepares
it (dataset, materialization), runs the surge load generator in a separate
container on separate cores, samples the CPU and memory of every container,
and stores everything for one run in runs/<experiment>/<run id>/.

    python bench/rb.py build
    python bench/rb.py seed --n 1000000
    python bench/rb.py calibrate --design a
    python bench/rb.py run --design c1 --multiple 5
    python bench/rb.py grid bench/experiments/core.yaml
    python bench/rb.py down

Run `python bench/rb.py <command> --help` for options.
"""
from __future__ import annotations

import argparse
import copy
import datetime as dt
import hashlib
import http.client
import itertools
import json
import os
import platform
import random
import shutil
import socket
import subprocess
import sys
import threading
import time
from pathlib import Path

import yaml

ROOT = Path(__file__).resolve().parent.parent  # resultbench/
COMPOSE_DIR = ROOT / "deploy" / "compose"
DESIGNS_FILE = ROOT / "bench" / "designs.yaml"
DEFAULT_SCENARIO = ROOT / "bench" / "scenarios" / "core.yaml"
IMAGE = "resultbench:local"
PROJECT = "rb"
# Raise the load generator's open-file limit only when asked; Docker's default
# (usually 1048576) is enough on most hosts.
NOFILE = os.environ.get("RB_LOADGEN_NOFILE", "")


def loadgen_cmd(cpuset: str) -> list[str]:
    # Every student is a separate client, and requests cancelled after a timeout
    # leave their port in TIME_WAIT. With Linux's default range (32768-60999) the
    # generator ran out of ports in waiting-room runs at 5x, so widen the range and
    # let new connections reuse TIME_WAIT ports. Neither changes a run that does not
    # run out of ports.
    cmd = ["docker", "run", "--rm", "--name", "rb-loadgen", "--network", "rb-net", "--cpuset-cpus", cpuset,
           "--sysctl", "net.ipv4.ip_local_port_range=1024 65535", "--sysctl", "net.ipv4.tcp_tw_reuse=1",
           "-v", f"{runs_dir()}:/runs"]
    if NOFILE:
        cmd += ["--ulimit", f"nofile={NOFILE}:{NOFILE}"]
    return cmd + [IMAGE]


def log(msg: str) -> None:
    print(f"[{dt.datetime.now().strftime('%H:%M:%S')}] {msg}", flush=True)


# ---------------------------------------------------------------- designs ---

def load_designs() -> tuple[dict, dict]:
    raw = yaml.safe_load(DESIGNS_FILE.read_text())
    designs = {}
    for name, d in raw["designs"].items():
        if "extends" in d:
            base = copy.deepcopy(raw["designs"][d["extends"]])
            base.update({k: v for k, v in d.items() if k not in ("env", "extends")})
            base["env"] = {**base.get("env", {}), **d.get("env", {})}
            d = base
        d.setdefault("databases", ["postgres"])
        designs[name] = d
    defaults = raw["defaults"]
    defaults["sut_cpuset"] = os.environ.get("RB_SUT_CPUSET", defaults["sut_cpuset"])
    defaults["loadgen_cpuset"] = os.environ.get("RB_LOADGEN_CPUSET", defaults["loadgen_cpuset"])
    return designs, defaults


def cpuset(spec: str) -> set[int]:
    out: set[int] = set()
    for part in spec.split(","):
        if "-" in part:
            a, b = part.split("-")
            out.update(range(int(a), int(b) + 1))
        elif part.strip():
            out.add(int(part))
    return out


# ----------------------------------------------------------- docker utils ---

def sh(cmd: list[str], env: dict | None = None, check: bool = True, capture: bool = False,
       cwd: Path | None = None, timeout: float | None = None) -> subprocess.CompletedProcess:
    full_env = {**os.environ, **(env or {})}
    return subprocess.run(cmd, env=full_env, check=check, cwd=cwd, timeout=timeout,
                          stdout=subprocess.PIPE if capture else None,
                          stderr=subprocess.STDOUT if capture else None, text=True)


class Compose:
    """docker compose with a fixed set of files and environment."""

    def __init__(self, files: list[str], env: dict):
        self.files = files
        self.env = {k: str(v) for k, v in env.items()}

    def cmd(self, *args: str) -> list[str]:
        out = ["docker", "compose", "-p", PROJECT]
        for f in self.files:
            out += ["-f", str(COMPOSE_DIR / f)]
        return out + list(args)

    def run(self, *args: str, check: bool = True, capture: bool = False, timeout: float | None = None):
        return sh(self.cmd(*args), env=self.env, check=check, capture=capture, cwd=COMPOSE_DIR, timeout=timeout)

    def up(self, services: list[str]) -> None:
        self.run("up", "-d", "--wait", "--wait-timeout", "180", *services)

    def tools(self, *args: str, env: dict | None = None, detach: bool = False, capture: bool = False):
        extra = []
        for k, v in (env or {}).items():
            extra += ["-e", f"{k}={v}"]
        flags = ["--rm", "-d"] if detach else ["--rm"]
        return self.run("--profile", "tools", "run", *flags, *extra, "tools", *args, capture=capture)

    def down(self) -> None:
        self.run("--profile", "tools", "down", "--remove-orphans", check=False)


def runs_dir() -> Path:
    d = Path(os.environ.get("RB_RUNS_DIR", ROOT / "runs")).resolve()
    d.mkdir(parents=True, exist_ok=True)
    return d


def base_env(design: dict, defaults: dict) -> dict:
    env = {"SUT_CPUSET": defaults["sut_cpuset"], "RUNS_DIR": str(runs_dir()), "RELEASE_AT_MS": ""}
    env.update(design.get("env", {}))
    return env


# ------------------------------------------------------ resource sampling ---

class _UnixHTTP(http.client.HTTPConnection):
    def __init__(self, path: str):
        super().__init__("localhost", timeout=5)
        self.path = path

    def connect(self):
        self.sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        self.sock.settimeout(5)
        self.sock.connect(self.path)


def _docker_get(path: str):
    c = _UnixHTTP("/var/run/docker.sock")
    try:
        c.request("GET", path)
        r = c.getresponse()
        return json.loads(r.read())
    finally:
        c.close()


class ResourceSampler(threading.Thread):
    """Samples CPU time and memory of every container in the project once a
    second, using the Docker Engine API (falls back to `docker stats`)."""

    def __init__(self, out: Path, release_ms: int):
        super().__init__(daemon=True)
        self.out = out
        self.release_ms = release_ms
        self.stop_evt = threading.Event()
        self.rows: list[tuple] = []
        self.use_api = Path("/var/run/docker.sock").exists()

    def containers(self) -> dict[str, str]:
        flt = json.dumps({"label": [f"com.docker.compose.project={PROJECT}"]})
        from urllib.parse import quote
        lst = _docker_get(f"/containers/json?filters={quote(flt)}")
        return {c["Id"]: c["Labels"].get("com.docker.compose.service", c["Names"][0]) for c in lst}

    def run(self):
        try:
            ids = self.containers() if self.use_api else {}
        except OSError:
            self.use_api, ids = False, {}
        while not self.stop_evt.is_set():
            t = int(time.time() * 1000) - self.release_ms
            try:
                if self.use_api:
                    for cid, svc in ids.items():
                        s = _docker_get(f"/containers/{cid}/stats?stream=false&one-shot=true")
                        cpu = s.get("cpu_stats", {}).get("cpu_usage", {}).get("total_usage", 0)
                        mem = s.get("memory_stats", {})
                        usage = mem.get("usage", 0) - mem.get("stats", {}).get("inactive_file", 0)
                        self.rows.append((t, svc, cpu / 1e9, usage))
                else:
                    self._sample_cli(t)
            except Exception as e:  # sampling must never break a run
                log(f"resource sampling error: {e}")
            self.stop_evt.wait(1.0)

    def _sample_cli(self, t: int):
        out = sh(["docker", "stats", "--no-stream", "--format", "{{json .}}"], capture=True, check=False).stdout
        for line in out.splitlines():
            try:
                s = json.loads(line)
            except json.JSONDecodeError:
                continue
            if not s.get("Name", "").startswith(PROJECT + "-"):
                continue
            svc = s["Name"][len(PROJECT) + 1:].rsplit("-", 1)[0]
            cpu_frac = float(s["CPUPerc"].rstrip("%")) / 100
            mem = _parse_size(s["MemUsage"].split("/")[0].strip())
            # Store CPU as a rate; integrated in summarize().
            self.rows.append((t, svc, -cpu_frac, mem))

    def stop(self) -> dict:
        self.stop_evt.set()
        self.join(timeout=10)
        with open(self.out / "resources.csv", "w") as f:
            f.write("t_ms,service,cpu_seconds_total,mem_bytes\n")
            for r in self.rows:
                f.write(f"{r[0]},{r[1]},{r[2]:.4f},{r[3]}\n")
        return self.summarize()

    def summarize(self) -> dict:
        per: dict[str, dict] = {}
        for t, svc, cpu, mem in self.rows:
            d = per.setdefault(svc, {"first": None, "last": None, "peak_mem_bytes": 0, "cpu_rate_integral": 0.0,
                                     "prev_t": None, "cpu_pos": 0.0, "restarts": 0})
            if cpu >= 0:
                if d["first"] is None:
                    d["first"] = cpu
                elif cpu >= d["last"]:
                    d["cpu_pos"] += cpu - d["last"]
                else:
                    d["restarts"] += 1  # counter went backwards: the container restarted or died
                d["last"] = cpu
            else:  # CLI fallback: integrate the rate
                if d["prev_t"] is not None:
                    d["cpu_rate_integral"] += -cpu * (t - d["prev_t"]) / 1000
                d["prev_t"] = t
            d["peak_mem_bytes"] = max(d["peak_mem_bytes"], mem)
        out = {}
        for svc, d in per.items():
            cpu_s = d["cpu_pos"] if d["first"] is not None else d["cpu_rate_integral"]
            out[svc] = {"cpu_seconds": round(cpu_s, 3), "peak_mem_mb": round(d["peak_mem_bytes"] / 2**20, 1),
                        "counter_resets": d["restarts"]}
        out["_total"] = {"cpu_seconds": round(sum(v["cpu_seconds"] for v in out.values()), 3),
                         "peak_mem_mb": round(sum(v["peak_mem_mb"] for k, v in out.items()), 1)}
        return out


def _parse_size(s: str) -> int:
    units = {"B": 1, "KiB": 2**10, "MiB": 2**20, "GiB": 2**30, "kB": 1e3, "MB": 1e6, "GB": 1e9}
    for u in sorted(units, key=len, reverse=True):
        if s.endswith(u):
            return int(float(s[: -len(u)]) * units[u])
    return 0


# -------------------------------------------------------------- scenario ---

def set_path(d: dict, path: str, value) -> None:
    keys = path.split(".")
    for k in keys[:-1]:
        d = d.setdefault(k, {})
    if isinstance(value, str):
        try:
            value = yaml.safe_load(value)
        except yaml.YAMLError:
            pass
    d[keys[-1]] = value


def load_scenario(path: Path, sets: dict) -> dict:
    sc = yaml.safe_load(Path(path).read_text()) or {}
    for k, v in sets.items():
        set_path(sc, k, v)
    return sc


def dataset_args(sc: dict) -> list[str]:
    ds = sc.get("dataset", {})
    args = []
    for key, flag in (("seed", "-seed"), ("n", "-n"), ("roll_base", "-roll-base"),
                      ("correction_per_mille", "-correction-per-mille"), ("num_schools", "-schools")):
        if key in ds:
            args += [flag, str(ds[key])]
    return args


def capacity_of(design: str) -> float | None:
    f = runs_dir() / "calibration" / design / "capacity.json"
    if not f.exists():
        return None
    return float(json.loads(f.read_text())["capacity_users_per_s"])


def git_commit() -> str:
    r = sh(["git", "rev-parse", "--short", "HEAD"], capture=True, check=False, cwd=ROOT)
    dirty = sh(["git", "status", "--porcelain"], capture=True, check=False, cwd=ROOT).stdout.strip()
    return r.stdout.strip() + ("-dirty" if dirty else "")


# ------------------------------------------------------------- commands ---

class FaultInjector(threading.Thread):
    """Injects one fault during a run (plan section 7.4).

    kill:    stop a service at at_s (seconds after release) and start it again
             after duration_s, e.g. {kind: kill, service: redis, at_s: 10, duration_s: 20}
    slow_db: cut PostgreSQL's CPU limit by `factor` for duration_s,
             e.g. {kind: slow_db, at_s: 10, duration_s: 60, factor: 10}
    """

    def __init__(self, spec: dict | None, release_ms: int, run_dir: Path):
        super().__init__(daemon=True)
        self.spec = spec or {}
        self.release_ms = release_ms
        self.events: list[dict] = []
        self.stop_evt = threading.Event()

    def _wait_until(self, t_s: float) -> bool:
        delay = self.release_ms / 1000 + t_s - time.time()
        return not self.stop_evt.wait(max(0.0, delay))

    def _note(self, what: str) -> None:
        self.events.append({"t_ms": int(time.time() * 1000) - self.release_ms, "event": what})
        log(f"fault: {what}")

    def _container(self, service: str) -> str:
        return f"{PROJECT}-{service}-1"

    def run(self):
        if not self.spec:
            return
        kind, at, dur = self.spec["kind"], float(self.spec.get("at_s", 10)), float(self.spec.get("duration_s", 30))
        if not self._wait_until(at):
            return
        if kind == "kill":
            c = self._container(self.spec["service"])
            sh(["docker", "kill", c], check=False, capture=True)
            self._note(f"killed {c}")
            if self._wait_until(at + dur):
                sh(["docker", "start", c], check=False, capture=True)
                self._note(f"restarted {c}")
        elif kind == "slow_db":
            c = self._container(self.spec.get("service", "postgres"))
            cpus = sh(["docker", "inspect", "-f", "{{.HostConfig.NanoCpus}}", c], capture=True).stdout.strip()
            orig = int(cpus) / 1e9 if cpus.isdigit() and int(cpus) > 0 else 1.0
            slow = orig / float(self.spec.get("factor", 10))
            sh(["docker", "update", "--cpus", f"{slow:.3f}", c], check=False, capture=True)
            self._note(f"{c} cpus {orig} -> {slow:.3f}")
            if self._wait_until(at + dur):
                sh(["docker", "update", "--cpus", f"{orig:.3f}", c], check=False, capture=True)
                self._note(f"{c} cpus back to {orig}")
        else:
            raise ValueError(f"unknown fault kind {kind}")

    def finish(self) -> dict:
        self.stop_evt.set()
        self.join(timeout=30)
        return {"spec": self.spec, "events": self.events}


def cmd_build(args) -> None:
    sh(["docker", "build", "-t", IMAGE, str(ROOT)])


def ensure_dataset(compose: Compose, design: dict, sc: dict, force: bool = False) -> None:
    for db in design["databases"]:
        env = {"RB_DSN": f"postgres://rb:rb@{db}:5432/rb"}
        log(f"checking dataset in {db}")
        compose.tools("dbload", *dataset_args(sc), *(["-force"] if force else []), env=env)
        # Always start from original results, whatever an earlier run left.
        compose.tools("correct", "-target", "none", "-revert", env=env, capture=True)


def cmd_seed(args) -> None:
    designs, defaults = load_designs()
    sc = load_scenario(args.scenario, dict(s.split("=", 1) for s in args.set))
    if args.n:
        set_path(sc, "dataset.n", args.n)
    d = designs["m"]  # has both databases
    compose = Compose(d["files"], base_env(d, defaults))
    compose.up(d["databases"])
    ensure_dataset(compose, d, sc, force=args.force)
    log("dataset ready")


def prepare(compose: Compose, design: dict, sc: dict, run_dir: Path | None, fraction: float) -> dict:
    """Pre-release work for designs that need it. Returns timing reports."""
    prep = design.get("prepare")
    if not prep:
        return {}
    target = prep["target"]
    report = f"/runs/{run_dir.relative_to(runs_dir())}/materialize.json" if run_dir else ""
    args = ["materialize", "-target", target]
    if target in ("kv", "files"):
        args += ["-verify-step", "997"]  # completeness check on a sample
    if target == "cache":
        args += ["-cache-ttl", str(sc.get("cache_ttl", "10m"))]
    if report:
        args += ["-report", report]
    if fraction < 1:
        return {"deferred": True, "args": args + ["-fraction", str(fraction)]}
    if target == "kv" or target == "cache":
        compose.up(["redis"])
    if target == "files":
        # Clear any earlier output so preparation time is always measured from scratch.
        sh(["docker", "run", "--rm", "-v", f"{PROJECT}_static:/data", "alpine:3.20", "rm", "-rf", "/data/results"], check=True)
    t0 = time.time()
    compose.tools(*args)
    log(f"materialized to {target} in {time.time() - t0:.1f}s")
    if run_dir and (run_dir / "materialize.json").exists():
        return json.loads((run_dir / "materialize.json").read_text())
    return {"seconds": time.time() - t0}


def collect_stats(compose: Compose, design: dict, run_dir: Path) -> None:
    stats_dir = run_dir / "stats"
    stats_dir.mkdir(exist_ok=True)
    for svc in design["services"]:
        if svc.startswith("portal") or svc == "wr":
            r = compose.run("--profile", "tools", "run", "--rm", "tools", "fetch", f"http://{svc}:8080/stats",
                            capture=True, check=False)
            if r.returncode != 0:
                log(f"could not read /stats from {svc} (did it crash?)")
            text = r.stdout.strip().splitlines()
            body = next((l for l in reversed(text) if l.startswith("{")), "{}")
            (stats_dir / f"{svc}.json").write_text(body + "\n")
        if svc == "wr":
            r = compose.run("--profile", "tools", "run", "--rm", "tools", "fetch", "http://wr:8080/ratelog",
                            capture=True, check=False)
            text = r.stdout.strip().splitlines()
            body = next((l for l in reversed(text) if l.startswith("[") or l == "null"), "[]")
            (stats_dir / "wr_ratelog.json").write_text(body + "\n")
        if svc == "static":
            r = compose.run("--profile", "tools", "run", "--rm", "tools", "fetch", "http://static:8080/nginx_status",
                            capture=True, check=False)
            (stats_dir / "nginx_status.txt").write_text(r.stdout)


def run_one(design_name: str, sc: dict, run_dir: Path, opts: dict) -> dict:
    """Run one configuration end to end. Returns the meta record."""
    designs, defaults = load_designs()
    design = designs[design_name]
    run_dir.mkdir(parents=True, exist_ok=True)
    env = base_env(design, defaults)
    env.update({k: str(v) for k, v in opts.get("env", {}).items()})
    if "waiting_room" in design:
        wr = design["waiting_room"]
        if "WR_ADMIT_RATE" not in env:
            cap = capacity_of(wr["admit_from"])
            if cap is None:
                raise SystemExit(f"design {design_name} needs the calibrated capacity of {wr['admit_from']}; "
                                 f"run: python bench/rb.py calibrate --design {wr['admit_from']}")
            # WR_ADMIT_FRACTION (a grid variant) overrides the design's fraction,
            # e.g. to start an adaptive room from a wrong capacity estimate.
            frac = float(env.pop("WR_ADMIT_FRACTION", wr["admit_fraction"]))
            env["WR_ADMIT_RATE"] = f"{frac * cap:.1f}"
        sc.setdefault("waiting_room", {})["mode"] = env.get("WR_MODE", "token")

    if cpuset(env["SUT_CPUSET"]) & cpuset(defaults["loadgen_cpuset"]):
        raise SystemExit(f"system-under-test cores {env['SUT_CPUSET']} overlap load generator cores "
                         f"{defaults['loadgen_cpuset']}; set RB_LOADGEN_CPUSET to other cores")
    compose = Compose(design["files"], env)
    meta = {"design": design_name, "description": design["description"], "env": env, "options": opts,
            "commit": git_commit(), "host": platform.node(), "cpus": os.cpu_count(),
            "started": dt.datetime.now(dt.timezone.utc).isoformat()}
    compose.down()
    try:
        compose.up(design["databases"])
        ensure_dataset(compose, design, sc)
        fraction = float(opts.get("fraction", 1.0))
        meta["materialize"] = prepare(compose, design, sc, run_dir, fraction)

        pre = float(sc.get("surge", {}).get("pre_release_s", 30))
        release_ms = int((time.time() + defaults["lead_s"] + pre) * 1000)
        env["RELEASE_AT_MS"] = str(release_ms)
        compose.env["RELEASE_AT_MS"] = str(release_ms)
        meta["release_at_ms"] = release_ms

        if meta["materialize"].get("deferred"):
            # Early release: publish part now, the rest after release time.
            target = design["prepare"]["target"]
            if target in ("kv", "cache"):
                compose.up(["redis"])
            args = meta["materialize"]["args"] + ["-release-at", str(release_ms)]
            compose.tools(*args, detach=True)
            deadline = time.time() + 600
            while not (run_dir / "materialize.json").exists() and time.time() < deadline:
                time.sleep(1)
            meta["materialize"] = json.loads((run_dir / "materialize.json").read_text())

        compose.up([s for s in design["services"] if s not in design["databases"]])

        if sc.get("corrections", {}).get("enabled"):
            at_ms = release_ms + int(float(sc["corrections"].get("at_s", 120)) * 1000)
            target = design.get("corrections_target", "none")
            invalidate = str(opts.get("invalidate", True)).lower()
            for i, db in enumerate(design["databases"]):
                compose.tools("correct", "-at", str(at_ms), "-target", target if i == 0 else "none",
                              f"-invalidate={invalidate}",
                              "-report", f"/runs/{run_dir.relative_to(runs_dir())}/corrections-{db}.json",
                              env={"RB_DSN": f"postgres://rb:rb@{db}:5432/rb"}, detach=True)

        sc = copy.deepcopy(sc)
        sc["targets"] = design["targets"]
        sc["release_at_unix_ms"] = release_ms
        (run_dir / "scenario.yaml").write_text(yaml.safe_dump(sc, sort_keys=False))

        sampler = ResourceSampler(run_dir, release_ms)
        sampler.start()
        fault = FaultInjector(opts.get("fault"), release_ms, run_dir)
        fault.start()
        rel = run_dir.relative_to(runs_dir())
        log(f"running load: {design_name}, peak {sc.get('surge', {}).get('peak_users_per_s')}/s -> {rel}")
        sh(loadgen_cmd(defaults["loadgen_cpuset"]) + ["surge", "-scenario", f"/runs/{rel}/scenario.yaml",
            "-out", f"/runs/{rel}/load"])
        meta["resources"] = sampler.stop()
        meta["fault"] = fault.finish()
        collect_stats(compose, design, run_dir)
        (run_dir / "compose.log").write_text(compose.run("logs", "--no-color", check=False, capture=True).stdout)
        meta["containers"] = container_states(compose)
        if sc.get("corrections", {}).get("enabled") and design.get("corrections_target") == "files":
            compose.tools("correct", "-target", "files", "-revert", capture=True)
    finally:
        if (run_dir / "materialize.json").exists():
            meta["materialize"] = json.loads((run_dir / "materialize.json").read_text())
        meta["finished"] = dt.datetime.now(dt.timezone.utc).isoformat()
        (run_dir / "meta.json").write_text(json.dumps(meta, indent=2) + "\n")
        sh(["docker", "rm", "-f", "rb-loadgen"], check=False, capture=True)
        compose.down()
    return meta


def container_states(compose: Compose) -> dict:
    """Exit codes and OOM kills, so a crashed service is visible in the results."""
    r = compose.run("ps", "-a", "--format", "json", check=False, capture=True)
    out = {}
    for line in r.stdout.splitlines():
        try:
            c = json.loads(line)
        except json.JSONDecodeError:
            continue
        info = sh(["docker", "inspect", "-f", "{{.State.OOMKilled}} {{.State.ExitCode}} {{.RestartCount}}", c["Name"]],
                  capture=True, check=False).stdout.split()
        if len(info) == 3:
            out[c["Service"]] = {"state": c.get("State"), "oom_killed": info[0] == "true",
                                 "exit_code": int(info[1]), "restarts": int(info[2])}
    return out


def parse_sets(items: list[str]) -> dict:
    out = {}
    for s in items:
        k, _, v = s.partition("=")
        out[k] = v
    return out


def cmd_run(args) -> None:
    sc = load_scenario(args.scenario, parse_sets(args.set))
    peak = resolve_peak(sc, args.multiple, args.capacity, args.capacity_from)
    if peak:
        set_path(sc, "surge.peak_users_per_s", peak)
    opts = {"env": parse_sets(args.env), "fraction": args.fraction, "invalidate": args.invalidate,
            "multiple": args.multiple}
    if args.fault:
        opts["fault"] = yaml.safe_load(args.fault)
    name = args.name or f"{args.design}-x{args.multiple or 'abs'}-{dt.datetime.now().strftime('%Y%m%d-%H%M%S')}"
    run_dir = runs_dir() / args.experiment / name
    meta = run_one(args.design, sc, run_dir, opts)
    print_summary(run_dir)


def resolve_peak(sc: dict, multiple, capacity, capacity_from) -> float | None:
    if multiple is None:
        return None
    cap = capacity or capacity_of(capacity_from)
    if cap is None:
        raise SystemExit(f"no calibrated capacity for design {capacity_from}; "
                         f"run: python bench/rb.py calibrate --design {capacity_from}, or pass --capacity")
    return float(multiple) * cap


def print_summary(run_dir: Path) -> None:
    f = run_dir / "load" / "summary.json"
    if not f.exists():
        log(f"no summary in {run_dir}")
        return
    s = json.loads(f.read_text())
    meta = json.loads((run_dir / "meta.json").read_text())
    log(f"{run_dir.name}: users={s['users']} served_ok={s['user_served_ok']:.4f} "
        f"within={s['user_success_within']} amplification={s['amplification']:.2f} "
        f"wrong={s['wrong_requests']} stale={s['stale_requests']} "
        f"cpu_s={meta.get('resources', {}).get('_total', {}).get('cpu_seconds')}"
        + ("  [GENERATOR SATURATED]" if s["generator"]["saturated"] else ""))


def cmd_calibrate(args) -> None:
    designs, defaults = load_designs()
    design = designs[args.design]
    sc = load_scenario(args.scenario, parse_sets(args.set))
    env = base_env(design, defaults)
    if "waiting_room" in design:
        raise SystemExit("calibrate the backend design, not the waiting room")
    compose = Compose(design["files"], env)
    out = runs_dir() / "calibration" / args.design
    if out.exists():
        shutil.rmtree(out)
    out.mkdir(parents=True)
    compose.down()
    try:
        compose.up(design["databases"])
        ensure_dataset(compose, design, sc)
        prepare(compose, design, sc, None, 1.0)
        # RELEASE_AT_MS is empty, so results are released as soon as services start.
        compose.up([s for s in design["services"] if s not in design["databases"]])
        sc["targets"] = design["targets"]
        (out / "scenario.yaml").write_text(yaml.safe_dump(sc, sort_keys=False))
        rel = out.relative_to(runs_dir())
        sh(loadgen_cmd(defaults["loadgen_cpuset"]) + ["calibrate", "-scenario", f"/runs/{rel}/scenario.yaml",
            "-out", f"/runs/{rel}", "-start", str(args.start), "-step", str(args.step),
            "-growth", str(args.growth), "-step-s", str(args.step_s), "-deadline", str(args.deadline),
            "-threshold", str(args.threshold), "-refine", str(args.refine)])
    finally:
        sh(["docker", "rm", "-f", "rb-loadgen"], check=False, capture=True)
        compose.down()
    cap = capacity_of(args.design)
    log(f"capacity of {args.design}: {cap} users/s (saved in {out / 'capacity.json'})")


def expand_grid(spec: dict) -> list[dict]:
    """Expand an experiment file into a list of run configurations."""
    runs = []
    reps = int(spec.get("reps", 5))
    for block in spec["runs"]:
        variants = block.get("variants", [{}])
        for design, multiple, variant, rep in itertools.product(
                block["designs"], block.get("multiples", [None]), variants, range(1, reps + 1)):
            sets = {**spec.get("set", {}), **block.get("set", {}), **variant.get("set", {})}
            env = {**spec.get("env", {}), **block.get("env", {}), **variant.get("env", {})}
            options = {**spec.get("options", {}), **block.get("options", {}), **variant.get("options", {})}
            label = variant.get("label", "")
            key = json.dumps([design, multiple, sets, env, options], sort_keys=True)
            vid = hashlib.sha1(key.encode()).hexdigest()[:6]
            name = f"{design}-x{multiple}" + (f"-{label}" if label else "") + f"-{vid}-r{rep}"
            sets = {**sets, "generator.seed": 1000 * rep + 7}
            runs.append({"design": design, "multiple": multiple, "sets": sets, "env": env, "options": options,
                         "label": label, "rep": rep, "name": name, "variant": vid})
    return runs


def cmd_grid(args) -> None:
    spec = yaml.safe_load(Path(args.file).read_text())
    if args.reps:
        spec["reps"] = args.reps
    runs = expand_grid(spec)
    # Randomise order so slow drift (thermal, background load) does not line up with one design.
    random.Random(spec.get("order_seed", 1)).shuffle(runs)
    exp = spec["name"]
    scenario = ROOT / spec.get("scenario", "bench/scenarios/core.yaml")
    log(f"experiment {exp}: {len(runs)} runs")
    if args.dry_run:
        for r in runs:
            print(r["name"], r["sets"], r["env"], r["options"])
        return
    done = 0
    for r in runs:
        run_dir = runs_dir() / exp / r["name"]
        if (run_dir / "load" / "summary.json").exists():
            done += 1
            continue
        sc = load_scenario(scenario, r["sets"])
        peak = resolve_peak(sc, r["multiple"], None, spec.get("capacity_from", "a"))
        if peak:
            set_path(sc, "surge.peak_users_per_s", peak)
        opts = {"env": r["env"], "multiple": r["multiple"], "label": r["label"], "rep": r["rep"],
                "variant": r["variant"], **r["options"]}
        try:
            run_one(r["design"], sc, run_dir, opts)
            print_summary(run_dir)
        except subprocess.CalledProcessError as e:
            log(f"run {r['name']} failed: {e}; continuing")
        done += 1
        log(f"progress: {done}/{len(runs)}")


def cmd_down(args) -> None:
    designs, defaults = load_designs()
    files = sorted({f for d in designs.values() for f in d["files"]}) + ["monitoring.yml"]
    Compose(files, base_env({}, defaults)).down()
    if args.volumes:
        for v in ("pgdata", "pgdata-m2", "static"):
            sh(["docker", "volume", "rm", "-f", f"{PROJECT}_{v}"], check=False)


def cmd_monitor(args) -> None:
    designs, defaults = load_designs()
    Compose(["common.yml", "monitoring.yml"], base_env({}, defaults)).up(["prometheus", "grafana"])
    log("Grafana on http://localhost:3000, Prometheus on http://localhost:9090")


def main() -> None:
    p = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    sub = p.add_subparsers(dest="cmd", required=True)

    sub.add_parser("build", help="build the resultbench:local image").set_defaults(fn=cmd_build)

    s = sub.add_parser("seed", help="load the dataset into both databases")
    s.add_argument("--scenario", default=DEFAULT_SCENARIO)
    s.add_argument("--n", type=int)
    s.add_argument("--set", action="append", default=[])
    s.add_argument("--force", action="store_true")
    s.set_defaults(fn=cmd_seed)

    c = sub.add_parser("calibrate", help="find a design's capacity (users/s)")
    c.add_argument("--design", required=True)
    c.add_argument("--scenario", default=DEFAULT_SCENARIO)
    c.add_argument("--set", action="append", default=[])
    c.add_argument("--start", type=float, default=50)
    c.add_argument("--step", type=float, default=50)
    c.add_argument("--growth", type=float, default=1.5)
    c.add_argument("--step-s", type=float, default=20)
    c.add_argument("--deadline", type=float, default=2)
    c.add_argument("--threshold", type=float, default=0.99)
    c.add_argument("--refine", type=int, default=3)
    c.set_defaults(fn=cmd_calibrate)

    r = sub.add_parser("run", help="run one configuration")
    r.add_argument("--design", required=True)
    r.add_argument("--scenario", default=DEFAULT_SCENARIO)
    r.add_argument("--multiple", type=float, help="peak arrivals as a multiple of the calibrated capacity")
    r.add_argument("--capacity", type=float, help="capacity to use instead of the calibrated one")
    r.add_argument("--capacity-from", default="a", help="design whose capacity defines 1x (default: a)")
    r.add_argument("--set", action="append", default=[], help="scenario override, e.g. keys.dist=zipf")
    r.add_argument("--env", action="append", default=[], help="compose env override, e.g. WR_POLICY=random")
    r.add_argument("--fraction", type=float, default=1.0, help="share pre-computed before release")
    r.add_argument("--invalidate", type=lambda s: s.lower() == "true", default=True)
    r.add_argument("--fault", help='fault to inject, e.g. "{kind: kill, service: redis, at_s: 10, duration_s: 20}"')
    r.add_argument("--experiment", default="adhoc")
    r.add_argument("--name")
    r.set_defaults(fn=cmd_run)

    g = sub.add_parser("grid", help="run an experiment file (resumable)")
    g.add_argument("file")
    g.add_argument("--reps", type=int)
    g.add_argument("--dry-run", action="store_true")
    g.set_defaults(fn=cmd_grid)

    d = sub.add_parser("down", help="stop everything")
    d.add_argument("--volumes", action="store_true", help="also delete the dataset volumes")
    d.set_defaults(fn=cmd_down)

    sub.add_parser("monitor", help="start Prometheus and Grafana").set_defaults(fn=cmd_monitor)

    args = p.parse_args()
    args.fn(args)


if __name__ == "__main__":
    main()
