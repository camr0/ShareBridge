#!/usr/bin/env bash
# exp22 send-path CPU cost -- runner.  One benchdirect process at a time.
# LAB ONLY (runbook §1): this script never touches VERSA, containers or VMs.
set -u
cd "$(dirname "$0")"
DIR="$(pwd)"
BIN=/Users/ali/Git/ShareBridge/.worktrees/benchdirect/agent/bin/benchdirect
PHASES="${1:-sanity sweep uncap tail}"

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

COMMON=(--mode prod --size 64MiB --chunk 16KiB --queue 5MB --deadline 300)
CAPS=(5MB 10MB 20MB 30MB)
RTTS=(12 71)

echo "PHASES=$PHASES $(date '+%H:%M:%S')" >> "$DIR/runner.log"

if [[ "$PHASES" == *sanity* ]]; then
  # Runbook §3 sanity check: expect ~100 Mbps.
  run_cell e22-sanity-raw-rtt71 --mode raw --rtt 71 --size 32MiB
fi

if [[ "$PHASES" == *sweep* ]]; then
  # Interleaved: rep outer, arms inner -- drift hits all arms equally (exp14 method).
  for rep in r1 r2 r3; do
    for rtt in "${RTTS[@]}"; do
      for cap in "${CAPS[@]}"; do
        run_cell "e22-prod-rtt${rtt}-cap${cap}-${rep}" "${COMMON[@]}" --rtt "$rtt" --bandwidth "$cap"
      done
    done
  done
fi

if [[ "$PHASES" == *uncap* ]]; then
  for rep in r1 r2 r3; do
    for rtt in "${RTTS[@]}"; do
      run_cell "e22-prod-rtt${rtt}-uncap-${rep}" "${COMMON[@]}" --rtt "$rtt"
    done
  done
fi

if [[ "$PHASES" == *size* ]]; then
  # Size sweep at a fixed deterministic cap: 64MiB points already exist in the
  # cap30MB cells; add 16/32/128 MiB. Per-byte cost => go_cpu proportional to
  # size; fixed overhead => a positive intercept.
  for rep in r1 r2; do
    for sz in 16MiB 32MiB 128MiB; do
      run_cell "e22-prod-sz${sz}-rtt12-cap30MB-${rep}" --mode prod --size "$sz" --chunk 16KiB \
        --queue 5MB --deadline 300 --rtt 12 --bandwidth 30MB
    done
  done
fi

if [[ "$PHASES" == *tail* ]]; then
  # Drift bracket: the same sanity config + one prod reference cell, at the end.
  run_cell e22-tail-raw-rtt71 --mode raw --rtt 71 --size 32MiB
  run_cell e22-tail-prod-rtt12-uncap --rtt 12 "${COMMON[@]}"
fi

echo "DONE  $(date '+%H:%M:%S')" >> "$DIR/runner.log"
