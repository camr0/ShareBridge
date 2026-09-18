#!/bin/bash
# Slow instrumented run: bw 2MB/queue 64KB, periodic nettop snapshots of the
# benchdirect process (cumulative socket byte counters) with precise timestamps.
set -u
cd /Users/ali/Git/ShareBridge/.worktrees/benchdirect/agent
BIN=./bin/benchdirect
OUTDIR=/tmp/exp1
tag="nettop_bw2MB_rtt12_r1"
out="$OUTDIR/${tag}.json" err="$OUTDIR/${tag}.err"
nt="$OUTDIR/${tag}.nettop"
rm -f "$out" "$err" "$nt"
"$BIN" --mode raw --rtt 12 --loss 0 --size 64MiB --chunk 16KiB \
       --backpressure poll --deadline 120 --bandwidth 2MB --queue 64KB \
       --out "$out" >/dev/null 2>"$err" &
pid=$!
( # sampler: snapshot every ~4s while the process lives
  while kill -0 "$pid" 2>/dev/null; do
    echo "SNAP $(date +%s.%N)" >> "$nt"
    nettop -P -p "$pid" -l 1 -x -n 2>/dev/null | grep -v '^time' >> "$nt"
  done ) &
sampler=$!
wait "$pid"; rc=$?
kill $sampler 2>/dev/null; wait $sampler 2>/dev/null
echo "rc=$rc"
python3 - "$out" <<'EOF'
import json,sys
d=json.load(open(sys.argv[1]))
print("mbps",d["mbps"],"wall_mbps",d["wall_mbps"],"elapsed_ms",d["elapsed_ms"],
      "recv",d["received_bytes"],"fwd",d["per_conn"][0]["shim_fwd"],
      "drop",d["per_conn"][0]["shim_drop"],"we",d["per_conn"][0]["shim_write_err"])
EOF
