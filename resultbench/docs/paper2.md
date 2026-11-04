# Second paper: waiting-room mechanisms

The first paper found that both of its waiting rooms fail under students who
refresh: naive tickets send refreshing students to the back, and token
tickets let admitted, impatient students overload the backend (74.5% served,
3.21 backend requests per student). This paper adds and evaluates three
mechanisms, plus bot traffic and a measured student model. Draft:
[`../../paper2/`](../../paper2). Background: [gap analysis](../../reports/next-paper-gap-analysis.md).

## New waiting-room options

Set through compose environment variables (grid `env:`) or `rb waitingroom` flags.

| Env | Flag | Default | Meaning |
|---|---|---|---|
| `WR_LEASE_REQUESTS` | `-lease-requests` | 0 (no limit) | Most backend requests per admission; then the user rejoins the queue |
| `WR_REENTRY` | `-reentry` | `front` | Where a user whose lease is used up rejoins: `front` or `back` |
| `WR_ADAPTIVE` | `-adaptive` | false | Adjust the admission rate every second from backend response times |
| `WR_TARGET_LATENCY` | `-target-latency` | 500ms | Adaptive: response time to stay under |
| `WR_SLOW_SHARE` | `-slow-share` | 0.1 | Adaptive: share of slow, failed or abandoned requests that triggers a decrease |
| `WR_DECREASE_FACTOR` | `-decrease-factor` | 0.8 | Adaptive: rate multiplier on a decrease |
| — | `-increase-step`, `-min-rate`, `-max-rate` | 5%, 10%, 4× of the starting rate | Adaptive bounds |
| `WR_ADMIT_FRACTION` | — | design's 0.8 | Runner only: starting rate as a fraction of the calibrated capacity |

Queued responses now include `eta_ms` (−1 when unknown, e.g. lottery order
before release). The room serves its admission-rate history at `/ratelog`;
the runner saves it as `stats/wr_ratelog.json`.

## New load-generator options

| Scenario key | Default | Meaning |
|---|---|---|
| `users.queue_patience_s` | `[0, 0]` (no limit) | How long a student stays in a queue, counted from release or from joining |
| `users.eta_balk` | 0 (ignore estimates) | Leave as soon as time waited + estimate > `eta_balk` × queue patience |
| `bots.count` | 0 | Scrapers that walk `bots.rolls_each` consecutive roll numbers |
| `bots.poll_s`, `bots.gap_s`, `bots.timeout_s` | 1, 0, 10 | Bot pause after a queued/failed answer, between rolls, and request timeout |
| `bots.keep_cookie` | true | One waiting-room session for all of a bot's requests |
| `bots.start_s`, `bots.spread_s` | 0, 0 | When bots start relative to release, and over how long |

Bots appear in `requests`, `backend_requests` and the time series (what the
server sees), and in `summary.json` under `bots`; every per-student metric
excludes them. `users.csv.gz` has new columns `queue_patience_ms`,
`queue_wait_ms`, `bot` and `rolls_fetched`; `gave_up` can now be `queue` or
`queue_eta`.

## Grids

All in [`bench/experiments/paper2/`](../bench/experiments/paper2), scenario
[`bench/scenarios/paper2.yaml`](../bench/scenarios/paper2.yaml) (the paper-1
scenario with 240 s for queues to empty), 3 repetitions.

| Grid | RQ | Runs | What varies |
|---|---|---|---|
| `p2-lease` | 1 | 18 | naive; token with lease 0 (paper 1), 1, 2, 4; front vs back re-entry |
| `p2-adaptive` | 2 | 24 | fixed vs closed-loop rate from 0.5×, 0.8×, 1.2× capacity; portal slower than calibrated |
| `p2-eta` | 3 | 9 | queue patience 30–300 s; estimates off/on; with closed loop |
| `p2-bots` | 4 | 15 | 20 scrapers; A alone; room with lease 0 or 2; bots with fresh sessions |
| `p2-loads` | all | 36 | 2×, 5×, 10×: A, C1, paper-1 room, lease 2 + closed loop |

Run everything (about 13 hours, resumable):

```bash
bash bench/paper2-run.sh
```

## Notes

- Building the image behind a TLS-intercepting proxy fails at `go mod
  download`; run `go mod vendor` first (the Dockerfile uses `vendor/` when it
  exists; it is git-ignored).
- The student model's queue patience and refresh rates are assumptions until
  the [behaviour study](../../behaviour-study/README.md) has data; re-run
  `p2-eta` and `p2-loads` with its suggested `users:` values.
