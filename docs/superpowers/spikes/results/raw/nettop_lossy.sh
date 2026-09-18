#!/bin/bash
# Instrumented lossy runs for direction-separated byte accounting.
# bw 2MB + queue 5MB: rate-capped (token bucket = byte meter), no queue drops,
# only --loss random loss. One at a time.
set -u
cd /Users/ali/Git/ShareBridge/.worktrees/benchdirect/agent
BIN=./bin/benchdirect
OUTDIR=/tmp/exp1
run() {
  local rtt="$1" loss="$2"
  local tag="nettop_bw2MB_q5MB_rtt${rtt}_loss${loss}"
  local out="$OUTDIR/${tag}.json" err="$OUTDIR/${tag}.err" nt="$OUTDIR/${tag}.nettop"
  rm -f "$out" "$err" "$nt"
  "$BIN" --mode raw --rtt "$rtt" --loss "$loss" --size 64MiB --chunk 16KiB \
         --backpressure poll --deadline 180 --bandwidth 2MB --queue 5MB \
         --out "$out" >/dev/null 2>"$err" &
  local pid=$!
  ( while kill -0 "$pid" 2>/dev/null; do
      echo "SNAP $(date +%s.%N)" >> "$nt"
      nettop -P -p "$pid" -l 1 -x -n 2>/dev/null | grep -v '^time' >> "$nt"
      sleep 3
    done ) &
  local sp=$!
  wait "$pid"; local rc=$?
  sleep 1; kill "$sp" 2>/dev/null; wait "$sp" 2>/dev/null
  echo "{\"tag\":\"$tag\",\"rtt\":$rtt,\"loss\":\"$loss\",\"status\":\"ok\",\"rc\":$rc,\"result\":$(cat "$out" 2>/dev/null || echo null)}" >> "$OUTDIR/results.jsonl"
  python3 - "$out" <<'EOF'
import json,sys
d=json.load(open(sys.argv[1]))
print("mbps",round(d["mbps"],2),"elapsed_ms",d["elapsed_ms"],
      "fwd",d["per_conn"][0]["shim_fwd"],"drop",d["per_conn"][0]["shim_drop"])
EOF
}
run 12 0.001
run 12 0.01
