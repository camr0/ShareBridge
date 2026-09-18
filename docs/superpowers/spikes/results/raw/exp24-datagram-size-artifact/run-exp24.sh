#!/usr/bin/env bash
# exp24 runner: rate-capped cells only (the host is swap-thrashing, so uncapped
# throughput is meaningless; the token bucket makes datagram counts deterministic).
# Usage: run-exp24.sh <block-label> <mode> <chunk> <size> <n>
set -u
R="$(cd "$(dirname "$0")" && pwd)"
cd /Users/ali/Git/ShareBridge/.worktrees/benchdirect/agent || exit 1
LABEL=$1; MODE=$2; CHUNK=$3; SIZE=$4; N=${5:-3}
for i in $(seq 1 "$N"); do
  L="$LABEL-r$i"
  { echo "=== $L $(date -u +%FT%TZ)"; uptime; sysctl vm.swapusage | tail -1; } >> "$R/runner.log"
  SB_SHIM_HIST="$R/$L.hist" ./bin/benchdirect \
    --mode "$MODE" --rtt 12 --bandwidth 10MB --size "$SIZE" --chunk "$CHUNK" \
    --out "$R/$L.json" 2> "$R/$L.log"
  python3 - "$R/$L.json" "$L" <<'PY' >> "$R/runner.log"
import json,sys
d=json.load(open(sys.argv[1])); c=d.get('per_conn',[{}])[0]
print("OK %s mbps=%.2f sent=%d recv=%d fwd=%s drop=%s go_cpu=%.4f chrome_cpu=%.3f"%(
 sys.argv[2],d['mbps'],d['sent_bytes'],d['received_bytes'],c.get('shim_fwd','n/a'),
 c.get('shim_drop','n/a'),d['go_cpu_seconds'],d['chrome_cpu_seconds']))
PY
done
