#!/bin/bash
# Does the 32 MiB result survive a realistic payload?
#
# 32 MiB at ~40 Mbps is ~6 seconds, so a fast startup burst into the 5 MB rwnd
# could be inflating size/wall. 300 MB amortises that away. If throughput falls
# at 300 MB, the 32 MiB numbers were a burst artifact.
#
# Same link as run_bbr.sh: rtt=100ms, jitter=10ms, 64 Mbps cap, 800KB queue.
set -u
cd "$(dirname "$0")"
OUT="${1:-bigpayload_results.jsonl}"
SIZE=300MB
DEADLINE=900

run() {
  local variant="$1" castep="$2" c="$3" r json
  local label="${variant}_c${c}_300MB"
  for r in $(seq 1 "$REPS"); do
    local t0=$SECONDS
    json=$(./bin/benchdirect --mode raw --conns "$c" --rtt 100 --loss 0 --jitter 10 \
      --bandwidth 8MB --size "$SIZE" --chunk 64KiB --backpressure poll \
      --cwndcastep "$castep" --deadline "$DEADLINE" --out - 2>/dev/null)
    if [ -n "$json" ]; then
      echo "$json" | jq -c --arg v "$variant" --arg label "$label" --arg size "$SIZE" \
        --argjson secs "$((SECONDS - t0))" \
        '. + {variant: $v, label: $label, size_label: $size, wall_secs: $secs}' >> "$OUT"
      printf '  %-20s rep%d: %7.1f Mbps  (%ds wall)\n' "$label" "$r" \
        "$(echo "$json" | jq -r '.wall_mbps')" "$((SECONDS - t0))"
    else
      printf '  %-20s rep%d: FAILED\n' "$label" "$r"
    fi
  done
}

for spec in "ssthca:ssthresh:32KB:2" "unpatched:none:0:1" "bbr:bbr:0:1"; do
  variant=$(echo "$spec" | cut -d: -f1)
  patch=$(echo "$spec"   | cut -d: -f2)
  castep=$(echo "$spec"  | cut -d: -f3)
  REPS=$(echo "$spec"    | cut -d: -f4)
  ./apply_sctp_patch.sh "$patch" >/dev/null
  if ! go build -o bin/benchdirect ./cmd/benchdirect/ 2>/dev/null; then
    echo "BUILD FAILED for $variant"; continue
  fi
  for c in 1 4; do
    # BBR-lite at c1 would be ~5min for a variant already rejected; skip it.
    [ "$variant" = bbr ] && [ "$c" = 1 ] && continue
    run "$variant" "$castep" "$c"
  done
done

./apply_sctp_patch.sh none >/dev/null
go build -o bin/benchdirect ./cmd/benchdirect/ 2>/dev/null
echo "restored pristine fork; results in $OUT"
