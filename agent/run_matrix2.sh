#!/bin/bash
# Averaging benchmark runner. Usage: ./run_matrix2.sh <1gb|latency|loss>
set -u
cd "$(dirname "$0")"
BIN=./bin/benchdirect

bench() {
  local label="$1" reps="$2"; shift 2
  local tmp; tmp=$(mktemp)
  local i v
  for i in $(seq 1 "$reps"); do
    v=$("$BIN" "$@" -out /dev/null 2>&1 1>/dev/null | sed -nE 's/.*throughput=([0-9.]+) Mbps.*/\1/p')
    printf '  %-26s rep%d: %s\n' "$label" "$i" "${v:-FAIL}"
    echo "$v" >> "$tmp"
  done
  awk -v l="$label" 'NF{n++; s+=$1; if(min==""||$1<min)min=$1; if($1>max)max=$1}
    END{if(n)printf "%-26s n=%d mean=%.2f min=%.2f max=%.2f Mbps\n", l, n, s/n, min, max;
        else printf "%-26s ALL FAIL\n", l}' "$tmp"
  rm -f "$tmp"
}

case "${1:-all}" in
1gb)
  echo "== 1 GiB ceiling (rtt=0, loss=0, 64 KiB, deadline=120) =="
  bench "raw_poll_1g"   2 --mode raw  --rtt 0 --loss 0 --size 1GiB --chunk 64KiB --backpressure poll  --deadline 120
  bench "raw_event_1g"  2 --mode raw  --rtt 0 --loss 0 --size 1GiB --chunk 64KiB --backpressure event --deadline 120
  bench "prod_1g"       2 --mode prod --rtt 0 --loss 0 --size 1GiB --chunk 64KiB --deadline 120
  ;;
latency)
  echo "== latency sweep (loss=0, 64 MiB, 4 reps, deadline=120) =="
  for r in 25 50 100; do
    bench "raw_poll_rtt${r}_64m" 4 --mode raw  --rtt "$r" --loss 0 --size 64MiB --chunk 64KiB --backpressure poll --deadline 120
    bench "prod_rtt${r}_64m"     4 --mode prod --rtt "$r" --loss 0 --size 64MiB --chunk 64KiB --deadline 120
  done
  ;;
loss)
  echo "== loss (rtt=25, 8 MiB, deadline=45) =="
  bench "raw_poll_loss1_8m" 2 --mode raw  --rtt 25 --loss 0.01 --size 8MiB --chunk 64KiB --backpressure poll --deadline 45
  bench "prod_loss1_8m"     2 --mode prod --rtt 25 --loss 0.01 --size 8MiB --chunk 64KiB --deadline 45
  bench "raw_poll_loss5_8m" 1 --mode raw  --rtt 25 --loss 0.05 --size 8MiB --chunk 64KiB --backpressure poll --deadline 45
  bench "prod_loss5_8m"     1 --mode prod --rtt 25 --loss 0.05 --size 8MiB --chunk 64KiB --deadline 45
  ;;
*)
  echo "usage: $0 <1gb|latency|loss|all>"
  exit 1
  ;;
esac
