#!/usr/bin/env bash
# Runs everything the second paper (waiting rooms) needs, in priority order.
# Resumable: finished runs are skipped. About 14 hours on the paper machine.
set -u
cd "$(dirname "$0")/.."
S=bench/scenarios/paper2.yaml
[ -f runs/calibration/a/capacity.json ] && grep -q "n: 200000" runs/calibration/a/scenario.yaml 2>/dev/null || \
  python3 bench/rb.py calibrate --design a --scenario $S --start 50 --step 50 --growth 1.5 --step-s 10
for g in p2-lease p2-roomcpu p2-adaptive p2-eta p2-bots p2-loads; do
  python3 bench/rb.py grid bench/experiments/paper2/$g.yaml
  python3 -m analysis.report runs/$g --recollect || true
done
echo PAPER2-RUN-DONE
