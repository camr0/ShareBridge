#!/bin/bash
# E11 gap-fill: (G1) low-loss cliff at rtt12 — the headline-critical region, n=5 plus
# baseline brackets; (G2) RTO interleaved with same-block baseline; (G3) rtt71 2e-4.
# Adds NEW files only (prefix "gap-"); does not touch run-exp11.sh's outputs.
set -u -o pipefail
ROOT=/Users/ali/Git/ShareBridge/.worktrees/benchdirect
BIN="$ROOT/agent/bin/benchdirect"
OUT="$ROOT/docs/superpowers/spikes/results/raw/exp11-loss-sensitivity"
LOG="$OUT/runner-gapfill.log"
SYS="$OUT/system-snapshots-gapfill.txt"

snapshot() {
  local label="$1"
  {
    echo "===== $label $(date -u '+%Y-%m-%dT%H:%M:%SZ') ====="
    uptime
    sysctl vm.swapusage
    ps -Ao pid,pcpu,pmem,comm | sort -rk2 | head -12
    echo
  } >> "$SYS"
}
run() {
  local label="$1"; shift
  snapshot "before-$label"
  echo "START $label $(date -u '+%Y-%m-%dT%H:%M:%SZ') $*" | tee -a "$LOG"
  if "$BIN" "$@" --out "$OUT/$label.json" >>"$LOG" 2>&1; then
    echo "OK $label $(date -u '+%Y-%m-%dT%H:%M:%SZ')" | tee -a "$LOG"
  else
    rc=$?
    echo "FAIL($rc) $label $(date -u '+%Y-%m-%dT%H:%M:%SZ')" | tee -a "$LOG"
  fi
  snapshot "after-$label"
}
base=(--mode raw --rtt 12 --size 64MiB --chunk 16KiB --backpressure poll --bandwidth 30MB --queue 5MB --deadline 180)

snapshot block-G1-start
# --- G1: low-loss cliff, rtt12, baseline bracket either side -----------------
for rep in 1 2 3; do run "gap-base-rtt12-loss0-r${rep}" "${base[@]}" --loss 0; done
for loss in 0.0001 0.0002 0.0003; do
  for rep in 1 2 3 4 5; do run "gap-rtt12-loss${loss}-r${rep}" "${base[@]}" --loss "$loss"; done
done
for rep in 1 2 3; do run "gap-base2-rtt12-loss0-r${rep}" "${base[@]}" --loss 0; done
snapshot block-G1-end

# --- G2: RTO interleaved with same-block baseline, rtt12 ---------------------
# Interleave default/200/500 within each loss level so any drift hits all three equally.
for rep in 1 2; do
  for loss in 0.001 0.009; do
    run "gap-rto-rtt12-loss${loss}-rtodefault-r${rep}" "${base[@]}" --loss "$loss"
    run "gap-rto-rtt12-loss${loss}-rto200ms-r${rep}"   "${base[@]}" --loss "$loss" --rtomax 200ms
    run "gap-rto-rtt12-loss${loss}-rto500ms-r${rep}"   "${base[@]}" --loss "$loss" --rtomax 500ms
  done
done
snapshot block-G2-end

# --- G3: rtt71 low-loss midpoint --------------------------------------------
for rep in 1 2 3; do
  run "gap-rtt71-loss0.0002-r${rep}" --mode raw --rtt 71 --size 64MiB --chunk 16KiB \
      --backpressure poll --bandwidth 30MB --queue 5MB --deadline 180 --loss 0.0002
done

snapshot block-gapfill-end
echo "GAPFILL DONE $(date -u '+%Y-%m-%dT%H:%M:%SZ')" | tee -a "$LOG"
