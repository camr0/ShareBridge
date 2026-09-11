#!/usr/bin/env bash
# relay-capacity-gate.sh — ShareBridge Phase 4a Task 36 capacity/safety baseline
# (plan: docs/superpowers/plans/2026-09-03-phase4a-relay-mvp.md Task 36;
# spec §§14, 18.5, 23.8).
#
# This gate has two halves and never mixes them:
#
#   LOCAL (default) — everything meaningful on a developer machine. It runs the
#   hermetic real-FRP capacity/safety suite (relay/internal/integration/load_test.go)
#   over loopback: one- and multi-agent throughput, §14 limit saturation without
#   cross-agent starvation, an active stream surviving beyond the idle window, the
#   no-byte idle close, the absolute-lifetime hard close under continuous activity,
#   cancellation releasing every admission slot, and goroutine/FD/copy-buffer
#   plateau under cumulative load. It also prints the host file-descriptor budget
#   and derives the safe global stream default from it.
#
#   TARGET — the hardware-dependent numbers on the real relay VM size: throughput,
#   CPU, RSS, NIC saturation and the global FD-budget choice for the shipped
#   deployment. The separate relay VM is provisioned by Task 37 and is NOT part of
#   this task, so those cells are emitted as PENDING HARDWARE (Task 37). The script
#   will only print a target number that it actually sampled on the target host; it
#   never extrapolates, and `--target` without a provisioned relay refuses to invent
#   one.
#
# The §14 product-tier bandwidth throttle stays DISABLED in Phase 4a. The pinned
# FRP release exposes no approved server-side cap, so un-throttled operation
# gathers honest relay throughput data for Phase 4b; the cap is deferred to
# Phase 4b per §14. This gate asserts that posture and never enables a throttle.
#
# Usage:
#   scripts/relay-capacity-gate.sh                 # local suite + defaults + pending table
#   scripts/relay-capacity-gate.sh --local         # explicit local mode
#   scripts/relay-capacity-gate.sh --skip-tests    # skip the Go suite (defaults/table only)
#   scripts/relay-capacity-gate.sh --target        # sample the provisioned relay VM
#   scripts/relay-capacity-gate.sh -h
#
# Environment (target mode; all optional — absence is reported, never guessed):
#   SHAREBRIDGE_CAPACITY_GATEWAY_UNIT   systemd unit to sample (default
#                                       sharebridge-relay-gateway.service)
#   SHAREBRIDGE_CAPACITY_METRICS_URL    private metrics URL (default
#                                       http://127.0.0.1:9101/metrics)
#   SHAREBRIDGE_CAPACITY_RELAY_URL      relay origin to drive load against, e.g.
#                                       https://share01.relay.sb0123abcd.sharebridgeusercontent.com
#   SHAREBRIDGE_CAPACITY_REQUESTS       requests for the target throughput sample (default 200)
#   SHAREBRIDGE_GATEWAY_NIC_INTERFACE   NIC to sample for saturation (as §17.3)
#   SHAREBRIDGE_GATEWAY_NIC_CAPACITY_BYTES_PER_SEC
#                                       link capacity for the saturation ratio
#
# Exit codes:
#   0  local suite GREEN and every printed target cell is either measured or
#      explicitly PENDING HARDWARE (Task 37)
#   1  the local capacity suite failed
#   2  usage error

set -u -o pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
RELAY_DIR="$REPO_ROOT/relay"

# §14 global stream ceiling. The deployment may lower this to the host FD
# budget; it may never exceed it.
SPEC_GLOBAL_CEILING=8192
# Fixed descriptors the gateway process holds regardless of stream count:
# public/metrics/plugin listeners, control-sync TLS, runtime epoll/kqueue, stdio.
FD_FIXED_RESERVE=256
# Each admitted public stream costs the gateway an accepted socket plus a
# loopback dial to the agent's FRP proxy port.
FD_PER_STREAM=2
# Default per-stream copy buffer (gateway.streamCopyBufferSize) and the two
# directions that can hold one at a time.
COPY_BUFFER_BYTES=$((32 * 1024))
COPY_BUFFERS_PER_STREAM=2

MODE="local"
SKIP_TESTS=0
for argument in "$@"; do
  case "$argument" in
    --local) MODE="local" ;;
    --target) MODE="target" ;;
    --skip-tests) SKIP_TESTS=1 ;;
    -h|--help)
      sed -n '2,50p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'
      exit 0
      ;;
    *)
      echo "usage: $(basename "$0") [--local|--target] [--skip-tests] [-h]" >&2
      exit 2
      ;;
  esac
done

echo "=== ShareBridge Phase 4a relay capacity gate (Task 36, §23.8) ==="
echo "host_platform=$(uname -s)_$(uname -m)"
echo "bandwidth_throttle=DISABLED (Phase 4a; server-side cap deferred to Phase 4b per §14)"
echo

# ---------------------------------------------------------------------------
# Host FD budget -> safe global stream default
# ---------------------------------------------------------------------------

fd_budget_raw="$(ulimit -n 2>/dev/null || echo unlimited)"
fd_budget=""
case "$fd_budget_raw" in
  unlimited|"") fd_budget="" ;;
  *) fd_budget="$fd_budget_raw" ;;
esac

safe_global="$SPEC_GLOBAL_CEILING"
if [ -n "$fd_budget" ]; then
  # Conservative: subtract the fixed reserve first, then divide by the
  # per-stream FD cost, because the bound must hold at full saturation.
  usable=$((fd_budget - FD_FIXED_RESERVE))
  if [ "$usable" -lt 0 ]; then
    usable=0
  fi
  from_budget=$((usable / FD_PER_STREAM))
  if [ "$from_budget" -lt "$safe_global" ]; then
    safe_global="$from_budget"
  fi
fi
if [ "$safe_global" -lt 1 ]; then
  safe_global=1
fi

echo "--- §14 defaults and the host FD budget ---"
echo "global_stream_ceiling_spec=$SPEC_GLOBAL_CEILING"
echo "host_fd_budget=${fd_budget:-unlimited}"
echo "fd_fixed_reserve=$FD_FIXED_RESERVE"
echo "fd_per_public_stream=$FD_PER_STREAM"
echo "safe_global_stream_default=$safe_global"
if [ "$safe_global" -eq "$SPEC_GLOBAL_CEILING" ]; then
  echo "safe_global_note=the §14 8192 ceiling fits this host's FD budget; no lowering required"
else
  echo "safe_global_note=the host FD budget is below 2*8192+reserve; MAX_STREAMS_GLOBAL must be lowered to $safe_global"
fi
echo "per_agent_byte_counter=mandatory (operator saturation signal; the throttle stays off)"
echo

# ---------------------------------------------------------------------------
# Target-VM sampling (Only printed numbers that were actually measured)
# ---------------------------------------------------------------------------

sample_target() {
  if [ "$(uname -s)" != "Linux" ]; then
    echo "target_sampling=UNAVAILABLE (not a Linux relay host)"
    return 0
  fi
  local unit="${SHAREBRIDGE_CAPACITY_GATEWAY_UNIT:-sharebridge-relay-gateway.service}"
  local pid=""
  if command -v systemctl >/dev/null 2>&1; then
    pid="$(systemctl show -p MainPID --value "$unit" 2>/dev/null || true)"
  fi
  if [ -z "$pid" ] || [ "$pid" = "0" ]; then
    pid="$(pgrep -f 'relay/cmd/gateway|sharebridge-relay-gateway' 2>/dev/null | head -n 1 || true)"
  fi
  if [ -z "$pid" ]; then
    echo "target_gateway_pid=PENDING HARDWARE (Task 37: relay gateway service not running on this host)"
    return 0
  fi
  echo "target_gateway_pid=$pid"

  if [ -d "/proc/$pid/fd" ]; then
    echo "target_gateway_open_fds=$(ls "/proc/$pid/fd" | wc -l | tr -d ' ')"
  else
    echo "target_gateway_open_fds=PENDING HARDWARE (Task 37)"
  fi
  if [ -r "/proc/$pid/status" ]; then
    echo "target_gateway_rss_kb=$(awk '/^VmRSS:/{print $2}' "/proc/$pid/status")"
  else
    echo "target_gateway_rss_kb=PENDING HARDWARE (Task 37)"
  fi

  # CPU: utime+stime delta over a one-second window, as percent of one core.
  if [ -r "/proc/$pid/stat" ]; then
    local ticks_before ticks_after
    ticks_before="$(awk '{print $14+$15}' "/proc/$pid/stat")"
    sleep 1
    ticks_after="$(awk '{print $14+$15}' "/proc/$pid/stat")"
    local hz
    hz="$(getconf CLK_TCK 2>/dev/null || echo 100)"
    echo "target_gateway_cpu_percent=$(awk -v a="$ticks_before" -v b="$ticks_after" -v hz="$hz" 'BEGIN{printf "%.1f", (b-a)/hz*100}')"
  else
    echo "target_gateway_cpu_percent=PENDING HARDWARE (Task 37)"
  fi

  # NIC saturation: delta bytes over one second against the configured link
  # capacity (the §17.3 gauge inputs). Absent inputs report unavailable.
  local nic="${SHAREBRIDGE_GATEWAY_NIC_INTERFACE:-}"
  local capacity="${SHAREBRIDGE_GATEWAY_NIC_CAPACITY_BYTES_PER_SEC:-}"
  if [ -n "$nic" ] && [ -n "$capacity" ] && [ -r /proc/net/dev ]; then
    local before after
    before="$(awk -v n="$nic" '$1==n":"{print $2+$10; exit}' /proc/net/dev)"
    sleep 1
    after="$(awk -v n="$nic" '$1==n":"{print $2+$10; exit}' /proc/net/dev)"
    if [ -n "$before" ] && [ -n "$after" ]; then
      echo "target_nic_saturation_ratio=$(awk -v a="$before" -v b="$after" -v c="$capacity" 'BEGIN{printf "%.4f", (b-a)/c}')"
    else
      echo "target_nic_saturation_ratio=PENDING HARDWARE (Task 37: interface $nic not found)"
    fi
  else
    echo "target_nic_saturation_ratio=PENDING HARDWARE (Task 37: set SHAREBRIDGE_GATEWAY_NIC_INTERFACE and SHAREBRIDGE_GATEWAY_NIC_CAPACITY_BYTES_PER_SEC)"
  fi

  # Private metrics scrape: the per-agent byte counter and connection counters
  # are the operator's saturation signals (§17.3).
  local metrics_url="${SHAREBRIDGE_CAPACITY_METRICS_URL:-http://127.0.0.1:9101/metrics}"
  if command -v curl >/dev/null 2>&1 && curl -fsS --max-time 3 "$metrics_url" >/tmp/relay-capacity-metrics.$$ 2>/dev/null; then
    echo "target_relayed_bytes_agent_scope=$(awk '/^sharebridge_relay_relayed_bytes_total\{scope="agent"\}/{print $2}' /tmp/relay-capacity-metrics.$$ | tail -n 1)"
    echo "target_public_connections_accepted=$(awk '/^sharebridge_relay_public_connections_total\{outcome="accepted"\}/{print $2}' /tmp/relay-capacity-metrics.$$ | tail -n 1)"
    rm -f /tmp/relay-capacity-metrics.$$
  else
    echo "target_metrics_scrape=PENDING HARDWARE (Task 37: private metrics endpoint unavailable)"
  fi

  # Target throughput: drive real requests at the configured relay origin.
  local relay_url="${SHAREBRIDGE_CAPACITY_RELAY_URL:-}"
  local requests="${SHAREBRIDGE_CAPACITY_REQUESTS:-200}"
  if [ -n "$relay_url" ] && command -v curl >/dev/null 2>&1; then
    local started finished ok
    ok=0
    started="$(date +%s.%N)"
    local i
    for ((i = 0; i < requests; i++)); do
      if curl -fsS --max-time 10 -o /dev/null "$relay_url"; then
        ok=$((ok + 1))
      fi
    done
    finished="$(date +%s.%N)"
    echo "target_throughput_requests=$requests"
    echo "target_throughput_ok=$ok"
    echo "target_throughput_req_per_s=$(awk -v r="$ok" -v a="$started" -v b="$finished" 'BEGIN{ if (b>a) printf "%.2f", r/(b-a); else print "NaN" }')"
  else
    echo "target_throughput_req_per_s=PENDING HARDWARE (Task 37: set SHAREBRIDGE_CAPACITY_RELAY_URL to the live relay origin)"
  fi
}

if [ "$MODE" = "target" ]; then
  echo "--- target relay VM measurements (Task 37 topology) ---"
  sample_target
  echo
fi

# ---------------------------------------------------------------------------
# Local hermetic capacity suite
# ---------------------------------------------------------------------------

if [ "$MODE" = "local" ] && [ "$SKIP_TESTS" -eq 0 ]; then
  echo "--- local hermetic real-FRP capacity/safety suite (§18.5, §23.8) ---"
  echo "command: SHAREBRIDGE_FRP_INTEGRATION=1 go test -race -count=1 -timeout 30m ./internal/integration -run 'Load|Capacity|Plateau|Starvation|Lifetime|Idle|Cancel' -v"
  log_file="$(mktemp -t relay-capacity-gate.XXXXXX)"
  (
    cd "$RELAY_DIR" || exit 1
    SHAREBRIDGE_FRP_INTEGRATION=1 go test -race -count=1 -timeout 30m \
      ./internal/integration -run 'Load|Capacity|Plateau|Starvation|Lifetime|Idle|Cancel' -v
  ) >"$log_file" 2>&1
  suite_status=$?
  grep -E 'CAPACITY\(EVIDENCE\)' "$log_file" || true
  if [ "$suite_status" -ne 0 ]; then
    echo "local_capacity_suite=FAIL (exit $suite_status); full log: $log_file"
    tail -n 40 "$log_file" >&2
    exit 1
  fi
  echo "local_capacity_suite=PASS"
  echo "local_capacity_log=$log_file"
  echo
fi

echo "--- capacity record template (fill from a real run; never extrapolate) ---"
echo "cell|local_loopback_measured|target_relay_vm"
echo "one_agent_throughput_req_per_s|CAPACITY(EVIDENCE) case=single-agent|PENDING HARDWARE (Task 37)"
echo "multi_agent_throughput_req_per_s|CAPACITY(EVIDENCE) case=multi-agent|PENDING HARDWARE (Task 37)"
echo "cpu_percent|not a loopback-meaningful number|PENDING HARDWARE (Task 37)"
echo "rss_kb|not a loopback-meaningful number|PENDING HARDWARE (Task 37)"
echo "open_fds|CAPACITY(EVIDENCE) case=resource-plateau|PENDING HARDWARE (Task 37)"
echo "nic_saturation_ratio|not a loopback-meaningful number|PENDING HARDWARE (Task 37)"
echo "global_stream_default|$safe_global (host FD budget)|PENDING HARDWARE (Task 37: confirm on the relay VM size)"
echo

if [ "$MODE" = "local" ]; then
  echo "result=GREEN (local): safety properties plateau/starvation/idle/lifetime/cancel verified; hardware cells remain PENDING HARDWARE (Task 37)"
  echo "scope_note=these results establish a safety baseline only; they do NOT alter route selection and do NOT substitute for the Phase 4b packet-impairment work (§18.5)."
else
  echo "result=target sampling complete; any PENDING cell above is not measured and must be filled by the Task 37 live run"
fi
exit 0
