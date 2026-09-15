#!/bin/bash
# Kernel TCP (CUBIC) reference at each payload size, matched to the SCTP sweep.
# Byte counts are exact decimal/2^n values so they match the SCTP --size args.
set -u
cd "$(dirname "$0")"
OUT="${1:-tcp_sizesweep.jsonl}"

for pair in "8MiB:8388608" "32MiB:33554432" "128MiB:134217728" "300MB:300000000" "1GB:1000000000"; do
  label="${pair%%:*}"; bytes="${pair##*:}"
  out=$(docker run --rm --cap-add=NET_ADMIN alpine:3.20 sh -c "
    apk add --no-cache iproute2 iperf3 >/dev/null 2>&1
    tc qdisc add dev lo root netem delay 50ms rate 64mbit limit 530
    iperf3 -s -D >/dev/null 2>&1; sleep 1
    iperf3 -c 127.0.0.1 -n $bytes -J 2>/dev/null
  " 2>/dev/null)
  mbps=$(echo "$out" | jq -r '.end.sum_received.bits_per_second / 1e6' 2>/dev/null)
  secs=$(echo "$out" | jq -r '.end.sum_received.seconds' 2>/dev/null)
  retx=$(echo "$out" | jq -r '.end.sum_sent.retransmits' 2>/dev/null)
  if [ -n "$mbps" ] && [ "$mbps" != "null" ]; then
    jq -cn --arg label "kernel_tcp_c1_${label}" --arg size "$label" \
      --argjson mbps "$mbps" --argjson secs "$secs" --argjson retx "${retx:-0}" \
      '{variant:"kernel_tcp", label:$label, conns:1, size_label:$size,
        wall_mbps:$mbps, wall_secs:($secs|round), retransmits:$retx,
        tool:"iperf3", cc:"cubic"}' >> "$OUT"
    printf '  kernel_tcp c1 %-8s %8.1f Mbps  (%ss, retx %s)\n' "$label" "$mbps" "$secs" "$retx"
  else
    printf '  kernel_tcp c1 %-8s FAILED\n' "$label"
  fi
done
echo "kernel TCP reference in $OUT"
