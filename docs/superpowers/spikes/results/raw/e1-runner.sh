#!/bin/bash
# E1 — wire-overhead / retransmit diagnosis.
# Cells: rtt {12,71} x loss {0,0.001,0.01} plain; rtt {12,71} x bandwidth caps
# (8MB/5MB, 8MB/100KB, 2MB/64KB). All: --mode raw --size 64MiB --chunk 16KiB
# --backpressure poll --deadline 120, n=3 (n=2 for the 2MB meter cells).
# Strictly one benchdirect process at a time. Optional NETTOP=1 -> snapshot
# benchdirect per-process counters right when the trace line hits stderr.
set -u
cd /Users/ali/Git/ShareBridge/.worktrees/benchdirect/agent
BIN=./bin/benchdirect
OUTDIR=/tmp/exp1
JSONL="$OUTDIR/results.jsonl"
mkdir -p "$OUTDIR"

run_one() {
  local rtt="$1" loss="$2" bw="$3" q="$4" rep="$5" n="$6"
  local tag="e1_rtt${rtt}_loss${loss}_bw${bw}_q${q}_r${rep}"
  local out="$OUTDIR/${tag}.json" err="$OUTDIR/${tag}.err"
  local args=(--mode raw --rtt "$rtt" --loss "$loss" --size 64MiB --chunk 16KiB
              --backpressure poll --deadline 120 --bandwidth "$bw" --queue "$q")
  rm -f "$out" "$err"
  local load0; load0="$(uptime | sed -E 's/.*load averages?: //')"
  local t0; t0=$(date +%s)
  "$BIN" "${args[@]}" --out "$out" >/dev/null 2>"$err" &
  local pid=$! waited=0 max=300
  if [ "${NETTOP:-0}" = "1" ]; then
    # wait for the final stderr trace line, then snapshot counters (may race
    # with process exit; failure tolerated)
    ( for i in $(seq 1 "$max"); do
        grep -q "^trace raw" "$err" 2>/dev/null && break
        kill -0 "$pid" 2>/dev/null || break
        sleep 0.2
      done
      nettop -P -p "$pid" -l 1 -x -n 2>/dev/null >> "$OUTDIR/${tag}.nettop" ) &
    local ntpid=$!
  fi
  while kill -0 "$pid" 2>/dev/null && [ "$waited" -lt "$max" ]; do
    sleep 1; waited=$((waited+1))
  done
  local status rc=0
  if kill -0 "$pid" 2>/dev/null; then
    kill -9 "$pid" 2>/dev/null; wait "$pid" 2>/dev/null; status="timeout"
  else
    wait "$pid"; rc=$?
    if [ -s "$out" ]; then status="ok"; else status="error"; fi
  fi
  [ "${NETTOP:-0}" = "1" ] && wait "$ntpid" 2>/dev/null
  local t1=$(( $(date +%s) - t0 ))
  local json; json="$(cat "$out" 2>/dev/null)"
  local tail_err; tail_err="$(tr '\n' '|' <"$err" | sed 's/\\/\\\\/g; s/"/\\"/g')"
  if [ "$status" = "ok" ]; then
    printf '{"tag":"%s","rtt":%s,"loss":"%s","bw":"%s","queue":"%s","rep":%s,"status":"ok","rc":%d,"wall_s":%d,"load0":"%s","result":%s}\n' \
      "$tag" "$rtt" "$loss" "$bw" "$q" "$rep" "$rc" "$t1" "$load0" "$json" >>"$JSONL"
  else
    printf '{"tag":"%s","rtt":%s,"loss":"%s","bw":"%s","queue":"%s","rep":%s,"status":"%s","rc":%d,"wall_s":%d,"load0":"%s","stderr":"%s"}\n' \
      "$tag" "$rtt" "$loss" "$bw" "$q" "$rep" "$status" "$rc" "$t1" "$load0" "$tail_err" >>"$JSONL"
  fi
  local mbps; mbps="$(printf '%s' "$json" | sed -nE 's/.*"mbps":([0-9.]+).*/\1/p')"
  printf '[%s] %-32s status=%s rc=%d wall=%ss mbps=%s load0=%s\n' \
    "$(date +%H:%M:%S)" "$tag" "$status" "$rc" "$t1" "${mbps:-NA}" "$load0"
}

# ---- block 1: plain loss cells ----
for rtt in 12 71; do
  for loss in 0 0.001 0.01; do
    for rep in 1 2 3; do run_one "$rtt" "$loss" 0 0 "$rep" 3; done
  done
done

# ---- block 2: bandwidth/queue cells (shim-induced tail drops) ----
for rtt in 12 71; do
  for rep in 1 2 3; do run_one "$rtt" 0 8MB 5MB "$rep" 3; done
done
for rtt in 12 71; do
  for rep in 1 2 3; do run_one "$rtt" 0 8MB 100KB "$rep" 3; done
done

# ---- block 3: 2MB byte-meter calibration cells ----
for rtt in 12 71; do
  for rep in 1 2; do run_one "$rtt" 0 2MB 64KB "$rep" 2; done
done

echo "=== DONE $(date) ==="
