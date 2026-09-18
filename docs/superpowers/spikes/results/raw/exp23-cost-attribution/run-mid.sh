#!/usr/bin/env bash
# exp23 phase "mid": prod vs raw at 64MiB, cap 30MB (240 Mbps) -- puts the
# app-layer delta at a rate between the pinned 80 Mbps pair and uncapped.
set -u
cd "$(dirname "$0")"
DIR="$(pwd)"
BIN=/Users/ali/Git/ShareBridge/.worktrees/benchdirect/agent/bin/benchdirect
snap() { { echo "=== $2 $1 $(date '+%H:%M:%S') ==="; uptime; sysctl vm.swapusage; ps -Ao %cpu,comm= | sort -rn | head -8; } >> "$DIR/system-snapshots.txt"; }
run_cell() {
  local label="$1"; shift
  snap "$label" before
  echo "START $label $(date '+%H:%M:%S') $*" >> "$DIR/runner.log"
  ( cd /tmp && "$BIN" --out "$DIR/$label.json" "$@" > "$DIR/$label.log" 2>&1 )
  local rc=$?
  snap "$label" after
  if [ $rc -eq 0 ] && [ -s "$DIR/$label.json" ]; then
    python3 "$DIR/row.py" "$label" >> "$DIR/cells.tsv"
    echo "OK    $label $(date '+%H:%M:%S')" >> "$DIR/runner.log"
  else
    echo "FAIL  $label rc=$rc $(date '+%H:%M:%S')" >> "$DIR/runner.log"
    printf '%s\tERR\n' "$label" >> "$DIR/cells.tsv"
  fi
}
for rep in r1 r2 r3; do
  run_cell "e23-mid-prod-rtt12-cap30MB-${rep}" --mode prod --chunk 16KiB --size 64MiB --queue 5MB --deadline 300 --rtt 12 --bandwidth 30MB
  run_cell "e23-mid-raw64-rtt12-cap30MB-${rep}" --mode raw --chunk 64KiB --size 64MiB --queue 5MB --deadline 300 --rtt 12 --bandwidth 30MB
done
echo "DONE mid $(date '+%H:%M:%S')" >> "$DIR/runner.log"
