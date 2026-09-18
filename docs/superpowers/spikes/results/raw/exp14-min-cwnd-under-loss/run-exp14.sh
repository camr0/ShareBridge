#!/bin/bash
# E14 — SCTP congestion-window floor (--mincwnd) under loss. --mode prod, 240 Mbps cap.
# Usage: run-exp14.sh <phase>   phases: base sweep ctl tail
# One benchdirect at a time. Appends a row to the results file after EVERY cell.
set -u -o pipefail
ROOT=/Users/ali/Git/ShareBridge/.worktrees/benchdirect
OUT="$ROOT/docs/superpowers/spikes/results/raw/exp14-min-cwnd-under-loss"
RES="$ROOT/docs/superpowers/spikes/results/2026-09-18-exp14-min-cwnd-under-loss.md"
BIN="$ROOT/agent/bin/benchdirect"
LOG="$OUT/runner.log"
SYS="$OUT/system-snapshots.txt"
TSV="$OUT/cells.tsv"
PHASE="${1:-sweep}"

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

P=(--mode prod --rtt 12 --size 64MiB --chunk 16KiB --bandwidth 30MB --queue 5MB --deadline 300)

case "$PHASE" in
base)
  snapshot block-base
  run e14-base-loss0-mc0-r1 "${P[@]}" --loss 0
  run e14-base-loss0-mc0-r2 "${P[@]}" --loss 0
  ;;
sweep)
  snapshot block-sweep
  # Interleaved: reps outer so machine drift hits all four arms equally.
  for r in 1 2 3; do
    for loss in 0.001 0.0002; do
      for mc in 0 2MiB 4MiB 8MiB; do
        args=("${P[@]}" --loss "$loss")
        if [ "$mc" != 0 ]; then args+=(--mincwnd "$mc"); fi
        run "e14-loss${loss}-mc${mc}-r${r}" "${args[@]}"
      done
    done
  done
  ;;
ctl)
  snapshot block-clean-control
  # the floor on a clean path: must be a no-op if the floor is harmless
  run e14-loss0-mc2MiB-r1 "${P[@]}" --loss 0 --mincwnd 2MiB
  run e14-loss0-mc8MiB-r1 "${P[@]}" --loss 0 --mincwnd 8MiB
  ;;
fieldcap)
  snapshot block-fieldcap
  # E11's field-matched loss level (0.90 %): the collapse there was 8.6 Mbps.
  for r in 1 2; do
    for mc in 0 2MiB 4MiB 8MiB; do
      args=("${P[@]}" --loss 0.009)
      if [ "$mc" != 0 ]; then args+=(--mincwnd "$mc"); fi
      run "e14-loss0.009-mc${mc}-r${r}" "${args[@]}"
    done
  done
  ;;
tail)
  snapshot block-tail
  run e14-tail-loss0-mc0-r1 "${P[@]}" --loss 0
  run e14-tail-loss0-mc0-r2 "${P[@]}" --loss 0
  run e14-tail-loss0.001-mc0-r4 "${P[@]}" --loss 0.001
  run e14-tail-loss0.001-mc8MiB-r4 "${P[@]}" --loss 0.001 --mincwnd 8MiB
  ;;
*)
  echo "unknown phase $PHASE" >&2; exit 2;;
esac
snapshot block-end-$PHASE
echo "PHASE $PHASE done $(date -u '+%Y-%m-%dT%H:%M:%SZ')" | tee -a "$LOG"
