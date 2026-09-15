#!/bin/bash
# Resume the sweep after the environmental failure: only the ssthca phase and the
# kernel-TCP reference were lost. Appends to the existing files; never overwrites.
set -u
cd "$(dirname "$0")"
OUT="sizesweep_results.jsonl"
DEADLINE=900
PAYLOADS="8MiB 32MiB 128MiB 300MB 1GB"

run() {
  local variant="$1" castep="$2" size="$3" c="$4" json t0 attempt
  local label="${variant}_c${c}_${size}"
  for attempt in 1 2; do
    t0=$SECONDS
    json=$(./bin/benchdirect --mode raw --conns "$c" --rtt 100 --loss 0 --jitter 10 \
      --bandwidth 8MB --size "$size" --chunk 64KiB --backpressure poll \
      --cwndcastep "$castep" --deadline "$DEADLINE" --out - 2>/dev/null)
    if [ -n "$json" ]; then
      echo "$json" | jq -c --arg v "$variant" --arg label "$label" --arg size "$size" \
        --argjson secs "$((SECONDS - t0))" \
        '. + {variant: $v, label: $label, size_label: $size, wall_secs: $secs}' >> "$OUT"
      printf '  %-24s %8.1f Mbps  (%ds)%s\n' "$label" \
        "$(echo "$json" | jq -r '.wall_mbps')" "$((SECONDS - t0))" \
        "$([ "$attempt" = 2 ] && echo '  [retry]')"
      return
    fi
    printf '  %-24s attempt %d empty, retrying...\n' "$label" "$attempt"
    sleep 5
  done
  printf '  %-24s FAILED\n' "$label"
}

./apply_sctp_patch.sh ssthresh >/dev/null
go build -o bin/benchdirect ./cmd/benchdirect/ || { echo "BUILD FAILED"; exit 1; }
for size in $PAYLOADS; do
  for c in 1 4; do run ssthca 32KB "$size" "$c"; done
done

./apply_sctp_patch.sh none >/dev/null
go build -o bin/benchdirect ./cmd/benchdirect/ 2>/dev/null
echo "ssthca phase done; now kernel TCP reference"
./run_tcp_sizesweep.sh
