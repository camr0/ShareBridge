#!/bin/bash
# E1 — sanity baseline: runbook §3 config, 5 repeats, poll backpressure.
set -u
cd /Users/ali/Git/ShareBridge/.worktrees/benchdirect/agent
BIN=./bin/benchdirect
OUTDIR=/tmp/exp1
mkdir -p "$OUTDIR"
for rep in 1 2 3 4 5; do
  tag="sanity_pre_${rep}"
  out="$OUTDIR/${tag}.json" err="$OUTDIR/${tag}.err"
  load0="$(uptime | sed -E 's/.*load averages?: //')"
  t0=$(date +%s)
  "$BIN" --mode raw --rtt 71 --loss 0 --size 32MiB --chunk 16KiB \
         --backpressure poll --deadline 120 --out "$out" >/dev/null 2>"$err"
  rc=$?
  t1=$(( $(date +%s) - t0 ))
  mbps="$(sed -nE 's/.*"mbps":([0-9.]+).*/\1/p' "$out" 2>/dev/null)"
  printf '[%s] %-14s rc=%d wall=%ss mbps=%s load0=%s\n' "$(date +%H:%M:%S)" "$tag" "$rc" "$t1" "${mbps:-NA}" "$load0"
  echo "{\"tag\":\"$tag\",\"status\":\"$([ $rc -eq 0 ] && echo ok || echo error)\",\"rc\":$rc,\"wall_s\":$t1,\"load0\":\"$load0\",\"result\":$(cat "$out" 2>/dev/null || echo null)}" >> "$OUTDIR/sanity_pre.jsonl"
done
