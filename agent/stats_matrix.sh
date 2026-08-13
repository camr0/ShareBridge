#!/bin/bash
# Run N reps of a bench config and report distribution stats.
set -u
cd "$(dirname "$0")"
BIN=./bin/benchdirect
REPS="${REPS:-12}"

bench_stats() {
  local label="$1"; shift
  local tmp; tmp=$(mktemp)
  local i v
  printf '%-22s' "$label"
  for i in $(seq 1 "$REPS"); do
    v=$("$BIN" "$@" -out /dev/null 2>&1 1>/dev/null | sed -nE 's/.*throughput=([0-9.]+) Mbps.*/\1/p')
    printf ' %s' "${v:-FAIL}"
    echo "$v" >> "$tmp"
  done
  echo
  awk -v l="$label" '
    NF { v[n++]=$1+0; s+=$1; s2+=$1*$1; if(min==""||$1<min)min=$1; if($1>max)max=$1 }
    END {
      for(i=0;i<n;i++) for(j=i+1;j<n;j++) if(v[i]>v[j]){t=v[i];v[i]=v[j];v[j]=t}
      med = (n%2)? v[int(n/2)] : (v[n/2-1]+v[n/2])/2
      mean = s/n; sd = sqrt(s2/n - mean*mean)
      printf "  %s\n", l
      printf "  n=%d mean=%.1f median=%.1f min=%.1f max=%.1f sd=%.1f\n", n, mean, med, min, max, sd
      printf "  sorted:"
      for(i=0;i<n;i++) printf " %.0f", v[i]
      printf "\n"
    }' "$tmp"
  rm -f "$tmp"
}

echo "== rtt=100ms, loss=0, jitter=10ms, 100MiB, $REPS reps each =="
echo "--- RAW (single DataChannel) ---"
bench_stats "raw  default"        --mode raw  --rtt 100 --loss 0 --jitter 10 --size 100MiB --chunk 64KiB --backpressure poll --deadline 120
bench_stats "raw  mincwnd=4MiB"   --mode raw  --rtt 100 --loss 0 --jitter 10 --size 100MiB --chunk 64KiB --backpressure poll --mincwnd 4MiB --deadline 120
echo "--- PROD (real transfer.Manager) ---"
bench_stats "prod default"        --mode prod --rtt 100 --loss 0 --jitter 10 --size 100MiB --chunk 64KiB --deadline 120
bench_stats "prod mincwnd=4MiB"   --mode prod --rtt 100 --loss 0 --jitter 10 --size 100MiB --chunk 64KiB --mincwnd 4MiB --deadline 120
