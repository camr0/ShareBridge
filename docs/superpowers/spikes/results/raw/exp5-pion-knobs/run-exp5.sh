#!/bin/bash
# E5 — three never-swept pion knobs (fastRtxWnd / maxRxBuf / maxMsg) in --mode prod.
# Usage: run-exp5.sh <phase>   phases: sanity base fw rx msg loss tail
# One benchdirect at a time. Appends a row to the results file after EVERY cell.
set -u -o pipefail
ROOT=/Users/ali/Git/ShareBridge/.worktrees/benchdirect
AGENT="$ROOT/agent"
OUT="$ROOT/docs/superpowers/spikes/results/raw/exp5-pion-knobs"
RES="$ROOT/docs/superpowers/spikes/results/2026-09-18-exp5-pion-knobs.md"
BIN="$AGENT/bin/benchdirect"
LOG="$OUT/runner.log"
SYS="$OUT/system-snapshots.txt"
TSV="$OUT/cells.tsv"
PHASE="${1:-base}"

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
    printf '%s\t\t\t\t\t\t\t\t\t\t\t\t\t\trow.py failed\n' "$label" > "$OUT/$label.row"
  fi
  cat "$OUT/$label.row" >> "$TSV"
  awk -F'\t' '{printf "| %s | %s | %s | %s | %s | %s | %s | %s | %s | %s | %s | %s | %s | %s | %s | %s |\n",$1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16}' "$OUT/$label.row" >> "$RES"
}

# prod-mode common args: 240 Mbps cap, 5 MB queue (no tail drops), 64 MiB.
P=(--mode prod --size 64MiB --chunk 16KiB --bandwidth 30MB --queue 5MB)

case "$PHASE" in
sanity)
  snapshot block-sanity
  run sanity-raw-rtt71-r1 --mode raw --rtt 71 --loss 0 --size 32MiB --chunk 16KiB --backpressure poll --deadline 120
  run sanity-raw-rtt71-r2 --mode raw --rtt 71 --loss 0 --size 32MiB --chunk 16KiB --backpressure poll --deadline 120
  ;;
base)
  snapshot block-base
  # interleaved baseline bracket, rtt 12 vs 71
  for r in 1 2 3; do
    run e5-base-rtt12-r$r "${P[@]}" --rtt 12 --loss 0 --deadline 180
    run e5-base-rtt71-r$r "${P[@]}" --rtt 71 --loss 0 --deadline 180
  done
  ;;
fw)
  snapshot block-fastrtxwnd
  # FastRtxWnd is a BYTE budget, floored at the MTU (pion: max(MTU,fw)); default 0 == 1200 B.
  for r in 1 2; do
    for rtt in 12 71; do
      for v in 16KiB 64KiB 256KiB 1MiB; do
        run "e5-fw${v}-rtt${rtt}-r${r}" "${P[@]}" --rtt "$rtt" --loss 0 --fastrtxwnd "$v" --deadline 180
      done
    done
  done
  ;;
rx)
  snapshot block-maxrxbuf
  # pion default max receive buffer = 1 MiB.
  for r in 1 2; do
    for rtt in 12 71; do
      for v in 256KiB 4MiB 16MiB 64MiB; do
        run "e5-rx${v}-rtt${rtt}-r${r}" "${P[@]}" --rtt "$rtt" --loss 0 --maxrxbuf "$v" --deadline 180
      done
    done
  done
  ;;
msg)
  snapshot block-maxmsg
  # receive-side limit advertised to the peer; Chrome advertises 256 KiB, so 16/64 KiB shrink ours.
  for r in 1 2; do
    for rtt in 12 71; do
      for v in 16KiB 64KiB 256KiB 1MiB; do
        run "e5-mm${v}-rtt${rtt}-r${r}" "${P[@]}" --rtt "$rtt" --loss 0 --maxmsg "$v" --deadline 180
      done
    done
  done
  ;;
loss)
  snapshot block-loss-arm
  # The E11 collapse region: 2e-4 is the cliff, 1e-3 is the stable low-rate branch.
  for r in 1 2; do
    for loss in 0.001 0.0002; do
      for v in default 64KiB 256KiB 1MiB; do
        args=("${P[@]}" --rtt 12 --loss "$loss" --deadline 300)
        if [ "$v" != default ]; then args+=(--fastrtxwnd "$v"); fi
        run "e5-loss${loss}-fw${v}-rtt12-r${r}" "${args[@]}"
      done
    done
  done
  for r in 1 2; do
    for loss in 0.0002 0.001; do
      for v in default 256KiB; do
        args=("${P[@]}" --rtt 71 --loss "$loss" --deadline 300)
        if [ "$v" != default ]; then args+=(--fastrtxwnd "$v"); fi
        run "e5-loss${loss}-fw${v}-rtt71-r${r}" "${args[@]}"
      done
    done
  done
  ;;
tail)
  snapshot block-tail
  for r in 1 2 3; do
    run e5-tail-rtt12-r$r "${P[@]}" --rtt 12 --loss 0 --deadline 180
    run e5-tail-rtt71-r$r "${P[@]}" --rtt 71 --loss 0 --deadline 180
  done
  ;;
topup)
  snapshot block-topup
  # third rep on the FastRtxWnd loss-arm headline cells, interleaved
  for v in default 256KiB 1MiB; do
    args=("${P[@]}" --rtt 71 --loss 0.001 --deadline 300)
    if [ "$v" != default ]; then args+=(--fastrtxwnd "$v"); fi
    run "e5-topup-loss0.001-fw${v}-rtt71-r3" "${args[@]}"
  done
  run e5-topup-loss0.001-fwdefault-rtt12-r3 "${P[@]}" --rtt 12 --loss 0.001 --deadline 300
  for v in 64KiB 256KiB 1MiB; do
    run "e5-topup-loss0.001-fw${v}-rtt12-r3" "${P[@]}" --rtt 12 --loss 0.001 --fastrtxwnd "$v" --deadline 300
  done
  ;;
*)
  echo "unknown phase $PHASE" >&2; exit 2;;
esac
snapshot block-end-$PHASE
echo "PHASE $PHASE done $(date -u '+%Y-%m-%dT%H:%M:%SZ')" | tee -a "$LOG"
