#!/bin/bash
# Kernel TCP under tc/netem, matched to the conditions where the SCTP shim
# collapsed. Runs entirely inside one Linux container over lo, so both
# directions traverse the shaped qdisc (delay 50ms => ~100ms RTT).
set -u

run_case() {
  local label="$1" setup="$2" secs="$3"
  echo "=== $label ==="
  docker run --rm --cap-add=NET_ADMIN alpine:3.20 sh -c "
    apk add --no-cache iproute2 iperf3 >/dev/null 2>&1
    $setup
    echo -n '  rtt: '; ping -c 3 -q 127.0.0.1 2>/dev/null | tail -1
    iperf3 -s -D >/dev/null 2>&1
    sleep 1
    iperf3 -c 127.0.0.1 -t $secs -f m 2>&1 | grep -E 'sender|receiver' | sed 's/^/  /'
    tc -s qdisc show dev lo | grep -E 'Sent|backlog' | sed 's/^/  /'
  " 2>&1 | grep -v '^$'
}

run_case "baseline (no shaping), 5s" "" 5
run_case "100ms RTT / 64Mbit / queue 530pkt (~800KB, = shim default)" \
  "tc qdisc add dev lo root netem delay 50ms rate 64mbit limit 530" 15
run_case "100ms RTT / 64Mbit / queue 3500pkt (~5MB, = rwnd)" \
  "tc qdisc add dev lo root netem delay 50ms rate 64mbit limit 3500" 15
run_case "100ms RTT / 64Mbit / queue 530pkt + 0.1% loss" \
  "tc qdisc add dev lo root netem delay 50ms rate 64mbit limit 530 loss 0.1%" 15
