#!/usr/bin/env bash
# exp23 per-layer CPU cost attribution -- runner.  One benchdirect process at a time.
# LAB ONLY (runbook 1).  No git commands.  No field rig touched.
set -u
cd "$(dirname "$0")"
DIR="$(pwd)"
BIN=/Users/ali/Git/ShareBridge/.worktrees/benchdirect/agent/bin/benchdirect
PHASES="${1:-sanity layer uncap chunk prodchunk}"

if [ ! -f "$DIR/cells.tsv" ]; then
  printf 'label\tmode\trtt\tcap_bps\tmbps\twall_mbps\telapsed_ms\tgo_cpu_s\tgo_cores\tchrome_cores\tsent_bytes\treceived_bytes\tshim_fwd\tshim_drop\tshim_write_err\tdg_per_mib\twire_ratio\tdgram_bytes\tcs_per_gb\tcores_per_100mbps\tceil_mbps\n' > "$DIR/cells.tsv"
fi

snap() {  # snap <label> <before|after>
  { echo "=== $2 $1 $(date '+%H:%M:%S') ==="
    uptime
    sysctl vm.swapusage
    ps -Ao %cpu,comm= | sort -rn | head -8
  } >> "$DIR/system-snapshots.txt"
}

run_cell() {  # run_cell <label> <extra args...>
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

COMMON=(--size 64MiB --queue 5MB --deadline 300)
RTTS=(12 71)

echo "PHASES=$PHASES $(date '+%H:%M:%S')" >> "$DIR/runner.log"

if [[ "$PHASES" == *sanity* ]]; then
  # Runbook 3 sanity: expect ~100 Mbps.
  run_cell e23-sanity-raw-rtt71 --mode raw --rtt 71 --size 32MiB --chunk 64KiB
  run_cell e23-sanity-raw-rtt71-b --mode raw --rtt 71 --size 32MiB --chunk 64KiB
fi

if [[ "$PHASES" == *layer* ]]; then
  # prod vs raw at a pinned rate: 10MB = 80 Mbps.  raw uses 64KiB == prod's
  # hardcoded chunkSize (manager.go:24) so the delta is app-layer only.
  # Interleaved (rep outer, arms inner) so drift hits both arms equally.
  for rep in r1 r2 r3; do
    for rtt in "${RTTS[@]}"; do
      run_cell "e23-prod-rtt${rtt}-cap10MB-${rep}" "${COMMON[@]}" --mode prod --chunk 16KiB --rtt "$rtt" --bandwidth 10MB
      run_cell "e23-raw64-rtt${rtt}-cap10MB-${rep}" "${COMMON[@]}" --mode raw --chunk 64KiB --rtt "$rtt" --bandwidth 10MB
    done
  done
fi

if [[ "$PHASES" == *uncap* ]]; then
  for rep in r1 r2 r3; do
    run_cell "e23-prod-rtt12-uncap-${rep}" "${COMMON[@]}" --mode prod --chunk 16KiB --rtt 12
    run_cell "e23-raw64-rtt12-uncap-${rep}" "${COMMON[@]}" --mode raw --chunk 64KiB --rtt 12
  done
fi

if [[ "$PHASES" == *chunk* ]]; then
  # raw chunk sweep at a pinned rate, uncapped-regime check is the uncap phase.
  for rep in r1 r2 r3; do
    for ch in 16KiB 64KiB 256KiB; do
      run_cell "e23-rawchunk${ch}-rtt12-cap10MB-${rep}" "${COMMON[@]}" --mode raw --chunk "$ch" --rtt 12 --bandwidth 10MB
    done
  done
  for rep in r1 r2; do
    for ch in 16KiB 64KiB 256KiB; do
      run_cell "e23-rawchunk${ch}-rtt71-cap10MB-${rep}" "${COMMON[@]}" --mode raw --chunk "$ch" --rtt 71 --bandwidth 10MB
    done
  done
  # uncapped chunk check (the old E6 "64KiB wins" claim was in a rate-capped regime)
  for rep in r1 r2; do
    for ch in 16KiB 64KiB 256KiB; do
      run_cell "e23-rawchunk${ch}-rtt12-uncap-${rep}" "${COMMON[@]}" --mode raw --chunk "$ch" --rtt 12
    done
  done
fi

if [[ "$PHASES" == *prodchunk* ]]; then
  # prod ignores --chunk (manager.go hardcodes 64KB): two cells prove it.
  run_cell e23-prodchunk16KiB-rtt12-cap10MB --mode prod --chunk 16KiB --size 64MiB --queue 5MB --deadline 300 --rtt 12 --bandwidth 10MB
  run_cell e23-prodchunk256KiB-rtt12-cap10MB --mode prod --chunk 256KiB --size 64MiB --queue 5MB --deadline 300 --rtt 12 --bandwidth 10MB
fi

echo "DONE  $(date '+%H:%M:%S')" >> "$DIR/runner.log"
