#!/bin/bash
# Two questions:
#   1. Does BBR-lite (BDP-capped cwnd) beat the dumb ssthresh cap + CA step?
#   2. Do the patches help under PACKET LOSS, which was never measured patched?
#
# Condition: rtt=100ms, jitter=10ms, 64 Mbps cap, default 800KB queue, 32 MiB.
# Reference points on this link: unpatched 14.5 Mbps, kernel TCP 54.7 Mbps.
set -u
cd "$(dirname "$0")"
OUT="${1:-bbr_results.jsonl}"
SIZE=32MiB
REPS=3

run() {
  local variant="$1" castep="$2" loss="$3" c="$4" r json
  local label="${variant}_l${loss}_c${c}"
  for r in $(seq 1 "$REPS"); do
    json=$(./bin/benchdirect --mode raw --conns "$c" --rtt 100 --loss "$loss" --jitter 10 \
      --bandwidth 8MB --size "$SIZE" --chunk 64KiB --backpressure poll \
      --cwndcastep "$castep" --deadline 180 --out - 2>/dev/null)
    if [ -n "$json" ]; then
      echo "$json" | jq -c --arg v "$variant" --arg label "$label" --arg group "$variant" \
        '. + {variant: $v, label: $label, group: $group}' >> "$OUT"
      printf '  %-24s rep%d: %7.1f Mbps\n' "$label" "$r" "$(echo "$json" | jq -r '.wall_mbps')"
    else
      printf '  %-24s rep%d: FAILED\n' "$label" "$r"
    fi
  done
}

for spec in "ssthca:ssthresh:32KB" "bbr:bbr:0"; do
  variant="${spec%%:*}"; rest="${spec#*:}"; patch="${rest%%:*}"; castep="${rest##*:}"
  ./apply_sctp_patch.sh "$patch" >/dev/null
  if ! go build -o bin/benchdirect ./cmd/benchdirect/ 2>/dev/null; then
    echo "BUILD FAILED for $variant"; continue
  fi
  for loss in 0 0.0001 0.001; do
    for c in 1 4; do
      run "$variant" "$castep" "$loss" "$c"
    done
  done
done

./apply_sctp_patch.sh none >/dev/null
go build -o bin/benchdirect ./cmd/benchdirect/ 2>/dev/null
echo "restored pristine fork; results in $OUT"
