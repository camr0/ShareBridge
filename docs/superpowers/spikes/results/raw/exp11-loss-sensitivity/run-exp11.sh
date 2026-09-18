#!/bin/bash
set -u -o pipefail
ROOT=/Users/ali/Git/ShareBridge/.worktrees/benchdirect
AGENT="$ROOT/agent"
OUT="$ROOT/docs/superpowers/spikes/results/raw/exp11-loss-sensitivity"
BIN="$AGENT/bin/benchdirect"
LOG="$OUT/runner.log"
SYS="$OUT/system-snapshots.txt"

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

snapshot block-pre-sanity
run sanity-pre-2 --mode raw --rtt 71 --loss 0 --size 32MiB --chunk 16KiB --backpressure poll --deadline 120
run sanity-pre-3 --mode raw --rtt 71 --loss 0 --size 32MiB --chunk 16KiB --backpressure poll --deadline 120
snapshot block-pre-sweep

# Main deterministic-cap sweep: 30 MB/s = 240 Mbps bottleneck, 5 MB queue avoids tail-drops.
for rtt in 12 71; do
  for loss in 0 0.0001 0.0003 0.001 0.003 0.01; do
    for rep in 1 2 3; do
      run "sweep-rtt${rtt}-loss${loss}-r${rep}" --mode raw --rtt "$rtt" --loss "$loss" --size 64MiB --chunk 16KiB --backpressure poll --bandwidth 30MB --queue 5MB --deadline 180
    done
  done
done
snapshot block-post-sweep

# Field-cap headline: loss .009 at 30 MB/s, rtt 12 ms, n=3.
for rep in 1 2 3; do
  run "fieldcap-rtt12-loss0.009-r${rep}" --mode raw --rtt 12 --loss 0.009 --size 64MiB --chunk 16KiB --backpressure poll --bandwidth 30MB --queue 5MB --deadline 180
done
snapshot block-post-fieldcap

# RTO sensitivity at loss levels that diagnose recovery; same field-matched cap and RTT.
for loss in 0.001 0.009; do
  for rto in default 200ms 500ms; do
    for rep in 1 2 3; do
      args=(--mode raw --rtt 12 --loss "$loss" --size 64MiB --chunk 16KiB --backpressure poll --bandwidth 30MB --queue 5MB --deadline 180)
      if [ "$rto" != default ]; then args+=(--rtomax "$rto"); fi
      run "rto-rtt12-loss${loss}-rto${rto}-r${rep}" "${args[@]}"
    done
  done
done
snapshot block-post-rto

run sanity-post-1 --mode raw --rtt 71 --loss 0 --size 32MiB --chunk 16KiB --backpressure poll --deadline 120
run sanity-post-2 --mode raw --rtt 71 --loss 0 --size 32MiB --chunk 16KiB --backpressure poll --deadline 120
run sanity-post-3 --mode raw --rtt 71 --loss 0 --size 32MiB --chunk 16KiB --backpressure poll --deadline 120
snapshot block-post-sanity
