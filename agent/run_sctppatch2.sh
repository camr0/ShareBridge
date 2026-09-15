#!/bin/bash
# Round 2: the ssthresh cap stops the collapse but plateaus at 34% because
# congestion avoidance only adds ~1 MTU per RTT and never reaches the 800KB BDP.
# SetSCTPCwndCAStep is exposed, so we can raise the CA step without forking for it.
# Also test ssthresh = 768KB (~ the BDP at 64 Mbps x 100ms) to find the ceiling.
set -u
cd "$(dirname "$0")"
OUT="${1:-sctppatch2_results.jsonl}"
SIZE="32MiB"
REPS=3

run_case() {
  local variant="$1" castep="$2" c="$3" r json
  local label="p2_${variant}_ca${castep}_c${c}"
  for r in $(seq 1 "$REPS"); do
    json=$(./bin/benchdirect --mode raw --conns "$c" --rtt 100 --loss 0 --jitter 10 \
      --bandwidth 8MB --size "$SIZE" --chunk 64KiB --backpressure poll \
      --cwndcastep "$castep" --deadline 180 --out - 2>/dev/null)
    if [ -n "$json" ]; then
      echo "$json" | jq -c --arg v "$variant" --arg ca "$castep" --arg label "$label" \
        --arg group "p2_${variant}_ca${castep}" \
        '. + {variant: $v, castep: $ca, label: $label, group: $group}' >> "$OUT"
      printf '  %-24s rep%d: %7.1f Mbps\n' "$label" "$r" "$(echo "$json" | jq -r '.wall_mbps')"
    else
      printf '  %-24s rep%d: FAILED\n' "$label" "$r"
    fi
  done
}

for ca in 0 8KB 32KB 128KB; do
  ./apply_sctp_patch.sh ssthresh >/dev/null
  if ! go build -o bin/benchdirect ./cmd/benchdirect/ 2>/dev/null; then
    echo "BUILD FAILED (ssthresh, ca=$ca)"; continue
  fi
  for c in 1 4; do run_case ssth256 "$ca" "$c"; done
done

./apply_sctp_patch.sh ssth768 >/dev/null
if go build -o bin/benchdirect ./cmd/benchdirect/ 2>/dev/null; then
  for c in 1 4; do run_case ssth768 0 "$c"; done
fi

./apply_sctp_patch.sh none >/dev/null
go build -o bin/benchdirect ./cmd/benchdirect/ 2>/dev/null
echo "restored pristine fork; results in $OUT"
