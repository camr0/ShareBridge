#!/bin/bash
# E2 — RTO floor on the clean path.
# Sweeps rtt {12,25,71,100} x rtomax {0,500ms,200ms} in raw (poll) and prod,
# size 64MiB, n=3. Strictly sequential: exactly one benchdirect process at a time.
set -u
cd /Users/ali/Git/ShareBridge/.worktrees/benchdirect/agent
BIN=./bin/benchdirect
MODE_LIST="${MODE_LIST:-raw prod}"
OUTDIR="${OUTDIR:-/tmp/exp2}"
JSONL="$OUTDIR/results.jsonl"
mkdir -p "$OUTDIR"

run_one() {
  local mode="$1" rtt="$2" rtomax="$3" rep="$4"
  local tag="${mode}_rtt${rtt}_rto${rtomax}_r${rep}"
  local out="$OUTDIR/${tag}.json" err="$OUTDIR/${tag}.err"
  local args=(--mode "$mode" --rtt "$rtt" --loss 0 --size 64MiB --chunk 16KiB
              --backpressure poll --deadline 120)
  if [ "$rtomax" != "0" ]; then args+=(--rtomax "$rtomax"); fi
  rm -f "$out"
  local t0 load0
  load0="$(uptime | sed -E 's/.*load averages?: //')"
  t0=$(date +%s)
  "$BIN" "${args[@]}" --out "$out" >/dev/null 2>"$err" &
  local pid=$! waited=0 max=240
  while kill -0 "$pid" 2>/dev/null && [ "$waited" -lt "$max" ]; do
    sleep 1; waited=$((waited+1))
  done
  local status rc=0
  if kill -0 "$pid" 2>/dev/null; then
    kill -9 "$pid" 2>/dev/null; wait "$pid" 2>/dev/null
    status="timeout"
  else
    wait "$pid"; rc=$?
    if [ -s "$out" ]; then status="ok"; else status="error"; fi
  fi
  local t1=$(( $(date +%s) - t0 ))
  local json; json="$(cat "$out" 2>/dev/null)"
  local tail_err; tail_err="$(tr '\n' '|' <"$err" | sed 's/\\/\\\\/g; s/"/\\"/g')"
  if [ "$status" = "ok" ]; then
    printf '{"tag":"%s","mode":"%s","rtt":%s,"rtomax":"%s","rep":%s,"status":"ok","rc":%d,"wall_s":%d,"load0":"%s","result":%s}\n' \
      "$tag" "$mode" "$rtt" "$rtomax" "$rep" "$rc" "$t1" "$load0" "$json" >>"$JSONL"
  else
    printf '{"tag":"%s","mode":"%s","rtt":%s,"rtomax":"%s","rep":%s,"status":"%s","rc":%d,"wall_s":%d,"load0":"%s","stderr":"%s"}\n' \
      "$tag" "$mode" "$rtt" "$rtomax" "$rep" "$status" "$rc" "$t1" "$load0" "$tail_err" >>"$JSONL"
  fi
  local mbps
  mbps="$(printf '%s' "$json" | sed -nE 's/.*"mbps":([0-9.]+).*/\1/p')"
  printf '[%s] %-26s status=%s rc=%d wall=%ss mbps=%s load0=%s\n' \
    "$(date +%H:%M:%S)" "$tag" "$status" "$rc" "$t1" "${mbps:-NA}" "$load0"
}

RTOM=("0" "500ms" "200ms")
for mode in $MODE_LIST; do
  rtt_i=0
  for rtt in 12 25 71 100; do
    # rotate the rtomax order per rtt so no variant is always first
    off=$(( rtt_i % 3 ))
    order=("${RTOM[$off]}" "${RTOM[$(((off+1)%3))]}" "${RTOM[$(((off+2)%3))]}")
    for rep in 1 2 3; do
      for rt in "${order[@]}"; do
        run_one "$mode" "$rtt" "$rt" "$rep"
      done
    done
    rtt_i=$((rtt_i+1))
  done
done
echo "=== DONE $(date) ==="
