#!/bin/bash
# E6 — chunk size at high RTT. --mode raw --backpressure poll, 240 Mbps cap.
# Usage: run-exp6.sh <phase>   phases: zero loss25 loss71 loss100
# One benchdirect at a time. Appends a row to the results file after EVERY cell.
set -u -o pipefail
ROOT=/Users/ali/Git/ShareBridge/.worktrees/benchdirect
OUT="$ROOT/docs/superpowers/spikes/results/raw/exp6-chunk-size-high-rtt"
RES="$ROOT/docs/superpowers/spikes/results/2026-09-18-exp6-chunk-size-high-rtt.md"
BIN="$ROOT/agent/bin/benchdirect"
LOG="$OUT/runner.log"
SYS="$OUT/system-snapshots.txt"
TSV="$OUT/cells.tsv"
PHASE="${1:-zero}"

snapshot() {
  local label="$1"
  {
    echo "===== $label $(date -u '+%Y-%m-%dT%H:%M:%SZ') ====="
    uptime
    sysctl vm.swapusage
    ps -Ao pid,pcpu,pmem,comm | sort -rk2 | head -8
    echo
  } >> "$SYS"
}

run() {
  local label="$1"; shift
  snapshot "before-$label"
  echo "START $label $(date -u '+%Y-%m-%dT%H:%M:%SZ') $*" | tee -a "$LOG"
  if "$BIN" "$@" --out "$OUT/$label.json" >"$OUT/$label.log" 2>&1; then
    echo "OK $label $(date -u '+%Y-%m-%dT%H:%M:%SZ')" | tee -a "$LOG"
  else
    echo "FAIL($?) $label $(date -u '+%Y-%m-%dT%H:%M:%SZ')" | tee -a "$LOG"
  fi
  cat "$OUT/$label.log" >> "$LOG"
  snapshot "after-$label"
  if ! python3 "$OUT/row.py" "$label" "$OUT/$label.json" "$OUT/$label.log" > "$OUT/$label.row" 2>>"$OUT/row-errors.log"; then
    printf '%s\t\t\t\t\t\t\t\t\t\t\t\t\t\t\trow.py failed\n' "$label" > "$OUT/$label.row"
  fi
  cat "$OUT/$label.row" >> "$TSV"
  awk -F'\t' '{printf "| %s | %s | %s | %s | %s | %s | %s | %s | %s | %s | %s | %s | %s | %s | %s | %s |\n",$1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16}' "$OUT/$label.row" >> "$RES"
}

P=(--mode raw --size 64MiB --backpressure poll --bandwidth 30MB --queue 5MB --deadline 300)

phase_cells() {  # $1 = loss, $2 = rtt
  local loss="$1" rtt="$2"
  for r in 1 2 3; do
    for ck in 16KiB 64KiB 256KiB; do
      local tag="loss${loss}-rtt${rtt}-ck${ck}-r${r}"
      if [ "$loss" = 0 ]; then tag="zero-rtt${rtt}-ck${ck}-r${r}"; else tag="loss${loss}-rtt${rtt}-ck${ck}-r${r}"; fi
      run "e6-$tag" "${P[@]}" --rtt "$rtt" --loss "$loss" --chunk "$ck"
    done
  done
}

case "$PHASE" in
zero)
  snapshot block-zero
  for rtt in 25 71 100; do phase_cells 0 "$rtt"; done
  ;;
loss25)  snapshot block-loss-rtt25;  phase_cells 0.001 25 ;;
loss71)  snapshot block-loss-rtt71;  phase_cells 0.001 71 ;;
loss100) snapshot block-loss-rtt100; phase_cells 0.001 100 ;;
*) echo "unknown phase $PHASE" >&2; exit 2;;
esac
snapshot block-end-$PHASE
echo "PHASE $PHASE done $(date -u '+%Y-%m-%dT%H:%M:%SZ')" | tee -a "$LOG"
