#!/bin/bash
# E2 part 2 — extended reps, randomized order to spread drift across cells.
set -u
cd /Users/ali/Git/ShareBridge/.worktrees/benchdirect/agent
BIN=./bin/benchdirect
OUTDIR=/tmp/exp2
JSONL="$OUTDIR/results.jsonl"

run_one() {
  local mode="$1" rtt="$2" rtomax="$3" rep="$4"
  local tag="${mode}_rtt${rtt}_rto${rtomax}_r${rep}"
  local out="$OUTDIR/${tag}.json" err="$OUTDIR/${tag}.err"
  local args=(--mode "$mode" --rtt "$rtt" --loss 0 --size 64MiB --chunk 16KiB
              --backpressure poll --deadline 120)
  if [ "$rtomax" != "0" ]; then args+=(--rtomax "$rtomax"); fi
  rm -f "$out"
  local load0 t0 t1
  load0="$(uptime | sed -E 's/.*load averages?: //' | awk '{print $1}')"
  t0=$(date +%s)
  "$BIN" "${args[@]}" --out "$out" >/dev/null 2>"$err" &
  local pid=$! waited=0 max=240
  while kill -0 "$pid" 2>/dev/null && [ "$waited" -lt "$max" ]; do sleep 1; waited=$((waited+1)); done
  local status rc=0
  if kill -0 "$pid" 2>/dev/null; then
    kill -9 "$pid" 2>/dev/null; wait "$pid" 2>/dev/null; status="timeout"
  else
    wait "$pid"; rc=$?
    if [ -s "$out" ]; then status="ok"; else status="error"; fi
  fi
  t1=$(( $(date +%s) - t0 ))
  local json tail_err
  json="$(cat "$out" 2>/dev/null)"
  tail_err="$(tr '\n' '|' <"$err" | sed 's/\\/\\\\/g; s/"/\\"/g')"
  if [ "$status" = "ok" ]; then
    printf '{"tag":"%s","mode":"%s","rtt":%s,"rtomax":"%s","rep":%s,"status":"ok","rc":%d,"wall_s":%d,"load0":"%s","result":%s}\n' \
      "$tag" "$mode" "$rtt" "$rtomax" "$rep" "$rc" "$t1" "$load0" "$json" >>"$JSONL"
  else
    printf '{"tag":"%s","mode":"%s","rtt":%s,"rtomax":"%s","rep":%s,"status":"%s","rc":%d,"wall_s":%d,"load0":"%s","stderr":"%s"}\n' \
      "$tag" "$mode" "$rtt" "$rtomax" "$rep" "$status" "$rc" "$t1" "$load0" "$tail_err" >>"$JSONL"
  fi
  local mbps
  mbps="$(printf '%s' "$json" | sed -nE 's/.*"mbps":([0-9.]+).*/\1/p')"
  printf '[%s] %-26s %s wall=%ss mbps=%s load=%s\n' "$(date +%H:%M:%S)" "$tag" "$status" "$t1" "${mbps:-NA}" "$load0"
}

while read -r mode rtt rt rep; do
  [ -z "${mode:-}" ] && continue
  run_one "$mode" "$rtt" "$rt" "$rep"
done < "$OUTDIR/plan_b.txt"
echo "=== DONE $(date) ==="
