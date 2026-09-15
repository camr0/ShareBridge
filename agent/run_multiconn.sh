#!/bin/bash
# Multi-connection matrix runner. Appends one JSON object per run to OUT so
# partial results survive an interruption.
#
# Usage: ./run_multiconn.sh [outfile] [group]
#   groups: ceiling | highrtt | jitter | bwcap | bwindep | loss | all
set -u
cd "$(dirname "$0")"
BIN=./bin/benchdirect
OUT="${1:-multiconn_results.jsonl}"
WANT="${2:-all}"

COMMON=(--mode raw --chunk 64KiB --backpressure poll)

run_one() {
  local group="$1" c="$2" size="$3" reps="$4"; shift 4
  local label="${group}_c${c}" i json mbps
  for i in $(seq 1 "$reps"); do
    json=$("$BIN" --conns "$c" "$@" "${COMMON[@]}" --size "$size" --out - 2>/dev/null)
    if [ -z "$json" ]; then
      printf '  %-14s rep%d: FAILED\n' "$label" "$i"
      continue
    fi
    echo "$json" | jq -c --arg label "$label" --arg group "$group" \
      '. + {label: $label, group: $group}' >> "$OUT"
    mbps=$(echo "$json" | jq -r '.wall_mbps')
    printf '  %-14s rep%d: %8.1f Mbps%s\n' "$label" "$i" "$mbps" \
      "$(echo "$json" | jq -r 'if .error then "  [" + .error + "]" else "" end')"
  done
}

want() { [ "$WANT" = "all" ] || [ "$WANT" = "$1" ]; }

# rtt=0 ceiling: how far does aggregate throughput scale before something
# other than the network caps it (shim drain loop, CPU)?
if want ceiling; then
  echo "== ceiling: rtt=0, clean, 64 MiB, 5 reps =="
  for c in 1 2 4; do
    run_one ceiling "$c" 64MiB 5 --rtt 0 --loss 0 --deadline 120
  done
fi

# high RTT, no jitter: does N fix the bimodal collapse on its own?
if want highrtt; then
  echo "== highrtt: rtt=100, clean, 64 MiB, 5 reps =="
  for c in 1 2 4; do
    run_one highrtt "$c" 64MiB 5 --rtt 100 --loss 0 --deadline 120
  done
fi

# high RTT + jitter: the documented worst case (zero loss, spurious RTOs).
if want jitter; then
  echo "== jitter: rtt=100 jitter=10, 64 MiB, 5 reps =="
  for c in 1 2 4; do
    run_one jitter "$c" 64MiB 5 --rtt 100 --loss 0 --jitter 10 --deadline 120
  done
fi

# Shared 8 MB/s bottleneck: the regime where the minCwnd floor REGRESSED.
# All N flows contend for one capped link.
if want bwcap; then
  echo "== bwcap: rtt=100 jitter=10 bw=8MB SHARED, 32 MiB, 3 reps =="
  for c in 1 2 4; do
    run_one bwcap "$c" 32MiB 3 --rtt 100 --loss 0 --jitter 10 --bandwidth 8MB --sharing shared --deadline 180
  done
fi

# Same cap but one bottleneck per flow: does N scale when capacity is not shared?
if want bwindep; then
  echo "== bwindep: rtt=100 jitter=10 bw=8MB INDEPENDENT, 32 MiB, 3 reps =="
  for c in 1 2 4; do
    run_one bwindep "$c" 32MiB 3 --rtt 100 --loss 0 --jitter 10 --bandwidth 8MB --sharing independent --deadline 180
  done
fi

# 1% uniform loss at low RTT: the documented catastrophic case.
if want loss; then
  echo "== loss: rtt=25 loss=1%, 8 MiB, 3 reps =="
  for c in 1 2 4; do
    run_one loss "$c" 8MiB 3 --rtt 25 --loss 0.01 --deadline 120
  done
fi

# Loss sweep at the same rtt/jitter baseline as bwcap: where is the cliff between
# 0.01% (a decent link) and 1% (the documented catastrophe)?
if want lossmid; then
  echo "== lossmid: rtt=100 jitter=10, loss sweep, 8 MiB, 3 reps =="
  for spec in "0.0001:0.01pct" "0.001:0.1pct" "0.003:0.3pct" "0.01:1pct"; do
    loss="${spec%%:*}"; name="${spec##*:}"
    for c in 1 4; do
      run_one "loss_${name}" "$c" 8MiB 3 --rtt 100 --loss "$loss" --jitter 10 --deadline 120
    done
  done
fi

# Bandwidth sweep: locate the cliff where SCTP stops collapsing and becomes
# window-limited. Buffer defaults to 100ms of buffering at each rate.
if want bwsweep; then
  echo "== bwsweep: rtt=100 jitter=10, bandwidth sweep, 16 MiB, 2 reps =="
  for bw in 2MB 4MB 8MB 16MB 32MB; do
    mbps=$(( ${bw%MB} * 8 ))
    for c in 1 4; do
      run_one "bw_${mbps}mbps" "$c" 16MiB 2 --rtt 100 --loss 0 --jitter 10 --bandwidth "$bw" --deadline 180
    done
  done
fi

# Buffer-depth sweep: is the collapse caused by the bottleneck buffer being
# smaller than SCTP's slow-start target (the peer's ~5 MB rwnd)?
# queue_800KB is the default for bw=8MB and should reproduce the bwcap numbers.
if want queue; then
  echo "== queue: rtt=100 jitter=10 bw=8MB, buffer depth sweep, 32 MiB, 3 reps =="
  for q in 800KB 2MB 5MB 16MB; do
    for c in 1 4; do
      run_one "queue_${q}" "$c" 32MiB 3 --rtt 100 --loss 0 --jitter 10 --bandwidth 8MB --queue "$q" --deadline 180
    done
  done
fi

# Is a buffer >= N x rwnd enough at ALL rates, or is there a separate
# rate-dependent ceiling? Compare a fat buffer at a low and a high rate.
if want bfix; then
  echo "== bfix: explicit large buffer at low and high rate, 16 MiB, 2 reps =="
  for spec in "2MB:5MB:16mbps" "32MB:32MB:256mbps"; do
    bwc="${spec%%:*}"; rest="${spec#*:}"; q="${rest%%:*}"; name="${rest##*:}"
    for c in 1 4; do
      run_one "bfix_${name}" "$c" 16MiB 2 --rtt 100 --loss 0 --jitter 10 --bandwidth "$bwc" --queue "$q" --deadline 180
    done
  done
fi

echo "results appended to $OUT"
