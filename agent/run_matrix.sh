#!/bin/bash
# Benchdirect matrix runner. Each run: label + JSON result (or error/timeout) -> bench_results.jsonl
set -u
cd "$(dirname "$0")"
BIN=./bin/benchdirect
RESULTS=bench_results.jsonl
: > "$RESULTS"

run() {
  local label="$1"; shift
  local out="/tmp/bench_${label}_$$.json" err="/tmp/bench_${label}_$$.err"
  printf '[run] %-22s' "$label"
  "$BIN" "$@" -out "$out" 2>"$err" &
  local pid=$! waited=0 max=150
  while kill -0 "$pid" 2>/dev/null && [ "$waited" -lt "$max" ]; do
    sleep 1; waited=$((waited+1))
  done
  if kill -0 "$pid" 2>/dev/null; then
    kill -9 "$pid" 2>/dev/null; wait "$pid" 2>/dev/null
    printf '{"label":"%s","status":"timeout","params":"%s"}\n' "$label" "$*" >> "$RESULTS"
    printf ' TIMEOUT (>%ds)\n' "$max"
  else
    wait "$pid"; local rc=$?
    local json; json="$(cat "$out" 2>/dev/null)"
    if [ -n "$json" ]; then
      printf '{"label":"%s","status":"ok","rc":%d,"result":%s}\n' "$label" "$rc" "$json" >> "$RESULTS"
      printf ' OK %s\n' "$(cat "$err")"
    else
      printf '{"label":"%s","status":"error","rc":%d,"stderr":"%s"}\n' "$label" "$rc" "$(tr '\n' ' ' < "$err" | sed 's/"/\\"/g')" >> "$RESULTS"
      printf ' ERR(rc=%d) %s\n' "$rc" "$(cat "$err")"
    fi
  fi
  rm -f "$out" "$err"
}

# ---- Core: ceiling comparison (rtt=0, loss=0) ----
run raw_event_64k_128m   --mode raw  --rtt 0 --loss 0 --size 128MiB --chunk 64KiB --backpressure event
run raw_poll_64k_128m    --mode raw  --rtt 0 --loss 0 --size 128MiB --chunk 64KiB --backpressure poll
run prod_128m            --mode prod --rtt 0 --loss 0 --size 128MiB --chunk 64KiB
run raw_event_16k_128m   --mode raw  --rtt 0 --loss 0 --size 128MiB --chunk 16KiB --backpressure event
run raw_poll_16k_128m    --mode raw  --rtt 0 --loss 0 --size 128MiB --chunk 16KiB --backpressure poll

# ---- Latency sweep (loss=0) ----
run raw_poll_rtt25_32m   --mode raw  --rtt 25  --loss 0 --size 32MiB --chunk 64KiB --backpressure poll
run prod_rtt25_32m       --mode prod --rtt 25  --loss 0 --size 32MiB --chunk 64KiB
run raw_poll_rtt50_16m   --mode raw  --rtt 50  --loss 0 --size 16MiB --chunk 64KiB --backpressure poll
run prod_rtt50_16m       --mode prod --rtt 50  --loss 0 --size 16MiB --chunk 64KiB
run raw_poll_rtt100_8m   --mode raw  --rtt 100 --loss 0 --size 8MiB  --chunk 64KiB --backpressure poll
run prod_rtt100_8m       --mode prod --rtt 100 --loss 0 --size 8MiB  --chunk 64KiB

# ---- Loss sweep (rtt=25) ----
run raw_poll_loss1_16m   --mode raw  --rtt 25 --loss 0.01 --size 16MiB --chunk 64KiB --backpressure poll
run prod_loss1_16m       --mode prod --rtt 25 --loss 0.01 --size 16MiB --chunk 64KiB
run raw_poll_loss5_16m   --mode raw  --rtt 25 --loss 0.05 --size 16MiB --chunk 64KiB --backpressure poll
run prod_loss5_16m       --mode prod --rtt 25 --loss 0.05 --size 16MiB --chunk 64KiB

echo "=== done ==="
cat "$RESULTS"
