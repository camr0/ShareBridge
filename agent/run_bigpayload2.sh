#!/bin/bash
# Round 2 at 300 MB: decompose the patch and firm up the baseline.
#
# Round 1 showed the dominant unpatched pathology is the congestion-avoidance
# RAMP (+1 MTU/RTT ~ 12 KB/s, so ~66s to reach an 800KB BDP at 100ms RTT), not
# the ssthresh overshoot. So: which knob actually buys the throughput?
#
#   caonly     pristine fork + cwndCAStep 32KB   (fast ramp only)
#   ssthca     ssthresh 256K + cwndCAStep 32KB   (both)
#   unpatched  pristine fork + default CA        (neither)
#
# Same link: rtt=100ms, jitter=10ms, 64 Mbps cap, 800KB queue, 300MB payload.
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
      printf '  %-22s rep%d: %7.1f Mbps  (%ds)\n' "$label" "$r" \
        "$(echo "$json" | jq -r '.wall_mbps')" "$((SECONDS - t0))"
    else
      printf '  %-22s rep%d: FAILED\n' "$label" "$r"
    fi
  done
}

for spec in "caonly:none:32KB:2" "ssthca:ssthresh:32KB:1" "unpatched:none:0:2"; do
  variant=$(echo "$spec" | cut -d: -f1)
  patch=$(echo "$spec"   | cut -d: -f2)
  castep=$(echo "$spec"  | cut -d: -f3)
  REPS=$(echo "$spec"    | cut -d: -f4)
  ./apply_sctp_patch.sh "$patch" >/dev/null
  if ! go build -o bin/benchdirect ./cmd/benchdirect/ 2>/dev/null; then
    echo "BUILD FAILED for $variant"; continue
  fi
  for c in 1 4; do run "$variant" "$castep" "$c"; done
done

./apply_sctp_patch.sh none >/dev/null
go build -o bin/benchdirect ./cmd/benchdirect/ 2>/dev/null
echo "restored pristine fork; results in $OUT"
