#!/bin/bash
# Does patching pion/sctp actually fix the constrained-link collapse?
#
# Condition is the matched worst case from the TCP control: rtt=100ms,
# jitter=10ms, 64 Mbps cap, default 800KB queue. Unpatched SCTP gets 8.7 Mbps
# (N=1) where kernel TCP gets 54.7 Mbps on the same nominal link.
set -u
cd "$(dirname "$0")"
OUT="${1:-sctppatch_results.jsonl}"
SIZE="32MiB"
REPS=3

for v in none rtomin ssthresh both; do
  ./apply_sctp_patch.sh "$v" >/dev/null
  if ! go build -o bin/benchdirect ./cmd/benchdirect/ 2>/dev/null; then
    echo "== variant $v: BUILD FAILED =="
    continue
  fi
  echo "== variant $v =="
  for c in 1 4; do
    for r in $(seq 1 "$REPS"); do
      json=$(./bin/benchdirect --mode raw --conns "$c" --rtt 100 --loss 0 --jitter 10 \
        --bandwidth 8MB --size "$SIZE" --chunk 64KiB --backpressure poll \
        --deadline 180 --out - 2>/dev/null)
      if [ -n "$json" ]; then
        echo "$json" | jq -c --arg v "$v" --arg label "patch_${v}_c${c}" \
          --arg group "patch_${v}" '. + {variant: $v, label: $label, group: $group}' >> "$OUT"
        printf '  %-8s c%d rep%d: %7.1f Mbps\n' "$v" "$c" "$r" "$(echo "$json" | jq -r '.wall_mbps')"
      else
        printf '  %-8s c%d rep%d: FAILED\n' "$v" "$c" "$r"
      fi
    done
  done
done

# Always leave the tree carrying the pristine fork.
./apply_sctp_patch.sh none >/dev/null
go build -o bin/benchdirect ./cmd/benchdirect/ 2>/dev/null
echo "restored pristine fork; results in $OUT"
