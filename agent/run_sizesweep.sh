#!/bin/bash
# Does a STATIC policy hold across the whole payload range?
#
# This is the cheap test that decides whether an adaptive controller is worth
# building. If "N=4 + cwndCAStep 32KB" holds ~high% of kernel TCP at every
# payload size, an adaptive algorithm buys little and we skip the fork entirely.
# If it degrades at small or large payloads, that is the evidence to build one.
#
# Payload matters because the CA ramp is a fixed time cost (~66s to reach an
# 800KB BDP at 100ms RTT), so it dominates small transfers and amortises on big
# ones. Smaller payloads also finish in <1 RTT of data, where N cannot help.
set -u
cd "$(dirname "$0")"
OUT="${1:-sizesweep_results.jsonl}"
DEADLINE=900
PAYLOADS="8MiB 32MiB 128MiB 300MB 1GB"

run() {
  local variant="$1" castep="$2" size="$3" c="$4" json t0
  local label="${variant}_c${c}_${size}"
  t0=$SECONDS
  json=$(./bin/benchdirect --mode raw --conns "$c" --rtt 100 --loss 0 --jitter 10 \
    --bandwidth 8MB --size "$size" --chunk 64KiB --backpressure poll \
    --cwndcastep "$castep" --deadline "$DEADLINE" --out - 2>/dev/null)
  if [ -n "$json" ]; then
    echo "$json" | jq -c --arg v "$variant" --arg label "$label" --arg size "$size" \
      --argjson secs "$((SECONDS - t0))" \
      '. + {variant: $v, label: $label, size_label: $size, wall_secs: $secs}' >> "$OUT"
    printf '  %-24s %8.1f Mbps  (%ds)\n' "$label" \
      "$(echo "$json" | jq -r '.wall_mbps')" "$((SECONDS - t0))"
  else
    printf '  %-24s FAILED\n' "$label"
  fi
}

for spec in "unpatched:none:0" "caonly:none:32KB" "ssthca:ssthresh:32KB"; do
  variant=$(echo "$spec" | cut -d: -f1)
  patch=$(echo "$spec"   | cut -d: -f2)
  castep=$(echo "$spec"  | cut -d: -f3)
  ./apply_sctp_patch.sh "$patch" >/dev/null
  if ! go build -o bin/benchdirect ./cmd/benchdirect/ 2>/dev/null; then
    echo "BUILD FAILED for $variant"; continue
  fi
  for size in $PAYLOADS; do
    for c in 1 4; do run "$variant" "$castep" "$size" "$c"; done
  done
done

./apply_sctp_patch.sh none >/dev/null
go build -o bin/benchdirect ./cmd/benchdirect/ 2>/dev/null
echo "restored pristine fork; results in $OUT"
