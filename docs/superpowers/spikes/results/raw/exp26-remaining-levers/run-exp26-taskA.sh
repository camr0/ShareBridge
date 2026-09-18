#!/usr/bin/env bash
# exp26 Task A runner.  One sink process, sender cell-by-cell.
# Host is swap-thrashing: every block records uptime + swap.
set -u
R="$(cd "$(dirname "$0")" && pwd)"
BIN=/tmp/udpbench
PORT=9301
N=${N:-918000}
RUNS=${RUNS:-5}

syssnap() { echo "--- $1 $(date -u +%FT%TZ)"; uptime; sysctl vm.swapusage | tail -1; }

run_cell() { # label smode dg batch total
  local label=$1 smode=$2 dg=$3 batch=$4 total=$5
  syssnap "$label" >> "$R/runner.log"
  "$BIN" -mode sink -port $PORT -smode "$smode" -dg "$dg" -batch "$batch" \
      > /dev/null 2>> "$R/$label.sink.log" &
  local sinkpid=$!
  sleep 0.2
  "$BIN" -mode send -smode "$smode" -port $PORT -dg "$dg" -batch "$batch" \
      -total "$total" -runs "$RUNS" >> "$R/$label.jsonl" 2>> "$R/$label.log"
  wait $sinkpid 2>/dev/null
  syssnap "$label-after" >> "$R/runner.log"
  python3 - "$R/$label.jsonl" "$label" <<'PY' >> "$R/runner.log"
import json,sys
rows=[json.loads(l) for l in open(sys.argv[1]) if l.strip()]
ok=[r for r in rows if 'cpu_s' in r]
if not ok:
    print("SKIP %s %s"%(sys.argv[2], rows[0].get('note',''))); raise SystemExit
c=[r['cpu_s_per_gb_payload'] for r in ok]
s=[r['ns_per_syscall'] for r in ok]
print("OK %-22s n=%d ns/syscall mean=%.1f min=%.1f max=%.1f | cpu_s/GBpayload mean=%.2f min=%.2f max=%.2f"%(
  sys.argv[2], len(ok), sum(s)/len(s), min(s), max(s), sum(c)/len(c), min(c), max(c)))
PY
}

rm -f "$R"/*.jsonl
run_cell "A1-write"      write     1237 1  "$N"
run_cell "A2-connwrite"  connwrite 1237 1  "$N"
run_cell "A3-mmsg"       mmsg      1237 8  "$N"
run_cell "A4-gso"        gso       1237 8  "$N"
# syscall-count sweep: same total datagrams, 1/K syscalls each
run_cell "A5-writebig-b2"  writebig 1237 2  "$N"
run_cell "A6-writebig-b4"  writebig 1237 4  "$N"
run_cell "A7-writebig-b8"  writebig 1237 8  "$N"
run_cell "A8-writebig-b16" writebig 1237 16 "$N"
run_cell "A9-writebig-b32" writebig 1237 32 "$N"
echo DONE
