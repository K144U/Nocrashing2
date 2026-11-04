# ResultBench

ResultBench is the software for the thesis *Surviving Result Day* (see
[`../result-portal-research-plan.md`](../result-portal-research-plan.md)). It
contains everything needed to run the plan's three studies:

| Plan section | What | Where |
|---|---|---|
| 6, Study 1 | Field monitor for the 2027 result season, RTI template | [`monitor/`](monitor), [`docs/field-study.md`](docs/field-study.md) |
| 7, Study 2 | Portal designs A, M, B0–B3, C1, C2, D, the surge load generator, correctness oracle, runner, experiment grids, analysis | [`cmd/rb`](cmd/rb), [`internal/`](internal), [`deploy/compose`](deploy/compose), [`bench/`](bench), [`analysis/`](analysis) |
| 8, Study 3 | Capacity-planning model, reference deployment | [`model/`](model), [`deploy/result-in-a-box`](deploy/result-in-a-box) |

## Quick start

Requirements: Docker with Compose v2, Python 3.10+, about 4 CPU cores and 8 GB
RAM. Go 1.24 is needed only to run the unit tests outside Docker.

```bash
pip install -r requirements.txt
```

```bash
python bench/rb.py build
```

```bash
python bench/rb.py calibrate --design a --scenario bench/scenarios/quick.yaml
```

```bash
python bench/rb.py run --design c2 --multiple 2 --scenario bench/scenarios/quick.yaml
```

The last command brings up design C2, publishes 50,000 synthetic results as
static files, runs a two-minute result-day surge at twice design A's capacity,
and prints a summary. Everything about the run is in `runs/adhoc/<run>/`.

To check every design in about 20 minutes:

```bash
python bench/rb.py grid bench/experiments/smoke.yaml
```

```bash
python -m analysis.report runs/smoke --recollect
```

## The designs

All designs serve `GET /result?roll=<number>` with the same JSON body, start
from the same synthetic dataset, and get the same 2-CPU budget for the read
path ([`bench/designs.yaml`](bench/designs.yaml)).

| ID | Design | Read path |
|---|---|---|
| A | Database-backed | App queries PostgreSQL per request (4-table join) and builds the page |
| M | Multi-mirror | Two copies of A with separate databases; students pick a link and may switch |
| B0 | Cache-aside | Redis first; on a miss, PostgreSQL then fill |
| B1 | + coalescing | One database load per key in flight |
| B2 | + XFetch, TTL jitter | Probabilistic early refresh (Vattani et al., PVLDB 2015) |
| B3 | + pre-warm | Cache filled before release |
| C1 | Pre-computed KV | All results in Redis before release; no database on the read path |
| C2 | Pre-computed static | One JSON file per roll number served by Nginx |
| D-A, D-C1, D-C2 | Waiting room | Admission layer in front of A, C1 or C2; FIFO or random policy; naive or token tickets; optional request lease, closed-loop admission rate and wait estimates ([paper 2](docs/paper2.md)) |

## What a run records

`runs/<experiment>/<run>/`:

- `load/summary.json`: users, success within 2/5/10 s, outcome of every request
  (correct, stale, wrong, missing, soft failure, timeout, …), latency and
  time-to-result percentiles, amplification, generator health
- `load/requests.csv.gz`, `load/users.csv.gz`, `load/timeseries.csv`: raw data
- `resources.csv`, `meta.json`: CPU-seconds and memory of every container,
  preparation time, crashes, configuration, git commit
- `stats/`: counters from the portal (database queries, miss types) and waiting room

## Layout

```
cmd/rb/            single binary: portal, waitingroom, surge, calibrate, dbload,
                   materialize, correct, release, publish-csv, sample, fetch
internal/          dataset, oracle, db, portal, publish, wroom, surge, metrics
deploy/compose/    one compose file per design + monitoring overlay
deploy/nginx/      static serving for C2
deploy/result-in-a-box/  production reference deployment
bench/             runner (rb.py), designs, scenarios, experiment grids
analysis/          collect runs, bootstrap intervals, fairness, recovery, figures
model/             capacity-planning model
monitor/           field monitor for real result days
k6/                cross-check of the load generator with k6
docs/              methodology, experiments, field study, RTI template, reproduction
tests/             Python tests (Go tests live next to the code)
```

## Tests

```bash
go test ./...
```

```bash
python -m pytest tests
```

Continuous integration (Go and Python tests, plus an end-to-end run of two
designs) is defined in [`ci/github-workflow.yml`](ci/github-workflow.yml).
Copy it to `.github/workflows/resultbench.yml` at the repository root to
enable it.

## Documentation

- [Methodology](docs/methodology.md): budgets, workload model, assumptions, metrics
- [Experiments](docs/experiments.md): which grid answers which research question
- [Field study](docs/field-study.md) and [RTI template](docs/rti-template.md)
- [Reproducing the results](docs/reproduce.md)
- [Second paper: waiting-room mechanisms](docs/paper2.md)
