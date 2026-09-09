#!/usr/bin/env bash
# stun-nat-gate.sh — ShareBridge Phase 4a Task 25 real-NAT STUN receipt gate
# (plan: docs/superpowers/plans/2026-09-03-phase4a-relay-mvp.md Task 25;
# spec §§10.1, 10.3, 23.5 — BLOCKING gate 5).
#
# Executes the seven named gate cases against the REAL authenticated STUN
# protocol (one-use short-term credential, real MESSAGE-INTEGRITY-protected
# Binding exchange, receipt echo, §10.3 policy):
#
#   owner-router-nat    happy path through the owner router NAT
#   phone-hotspot       happy path through a phone hotspot/cellular NAT
#   blocked-udp         UDP to the listener blocked -> fail closed, no obs
#   spoof               wrong-integrity request burns the one-use credential;
#                       no observation can be manufactured from the spoofed
#                       packet and the honest exchange then fails closed
#   mismatched-egress   observation != required surface IP -> §10.3 stops
#                       BEFORE the public HTTPS reachability probe
#   receipt-replay      a replayed stun_result is rejected (single use)
#   expired-challenge   a Binding request after the 60 s TTL gets 401 and
#                       no observation; a fresh challenge recovers
#
# Helper path (decided): the Go helper is driven via `go test -run` on
# control/internal/directctl/stun_test.go — NOT a `go run` cmd. The gate needs
# in-process access to the real Task 16 listener, the Task 18 controller and
# the shared clock seam (impossible from outside the module), and the agent
# client library (agent/internal/stun) is another module's internal package.
# The test's UDP half sends byte-identical wire messages to the Task 17
# client (pion short-term MESSAGE-INTEGRITY, USERNAME=<challenge id>,
# receipt attribute 0xFF01).
#
# Modes:
#   --target local   (default) spins the REAL listener + controller
#                    in-process (loopback stands in for the NAT) and proves
#                    every case including all fail-closed negatives and the
#                    §10.3 no-probe stop. This is the normative protocol
#                    proof; it is a precondition for, not a substitute for,
#                    the remote real-NAT runs.
#   --target remote  drives the DEPLOYED control: a minimal test-agent WS
#                    client (api_key auth, hello/csr/tls_ready handshake,
#                    same wire shapes as the shipped agent) receives the real
#                    stun_challenge, performs the real UDP exchange from
#                    behind the NAT under test, and echoes stun_result.
#                    Requires STUN_GATE_SERVER and STUN_GATE_API_KEY.
#
# Secrets policy (spec §23.5 / §16.6): the challenge secret, API key and
# packed credential are NEVER printed. Receipts and transaction IDs appear
# ONLY as SHA-256 hashes via STUN_GATE_EVIDENCE lines. The NAT-observed
# public mapping is printed for the evidence template (it is the operator's
# own public address, a template field, never a credential).
#
# Usage:
#   scripts/stun-nat-gate.sh [--target local|remote] [--case name[,name...]]
#                            [--server wss://control.example.com]
#                            [--stun-addr control.example.com:3478]
#                            [--expected-public-ip 203.0.113.7]
#                            [--no-wait-rechallenge] [-h]
#
# Environment (remote mode; the API key is env-only so it never appears in
# `ps` output or shell history):
#   STUN_GATE_API_KEY            agent API key for the deployed control
#   STUN_GATE_SERVER             deployed control wss:// URL (same as --server)
#   STUN_GATE_STUN_ADDR          host:port for the UDP exchange if not the WS host (same as --stun-addr)
#   STUN_GATE_EXPECTED_PUBLIC_IP this network's expected egress IP for the mismatched-egress case (same as --expected-public-ip)
#   STUN_GATE_CERT_FINGERPRINT   optional: fingerprint of the already enrolled
#                                agent cert (persisted agents row) to skip CSR
#                                issuance on the deployed control
#   STUN_GATE_WAIT_RECHALLENGE   1 (default for the two happy-path cases):
#                                wait ~4m15s for the §10.2 rechallenge after
#                                acceptance; the arrival of a rechallenge
#                                (>=4m) rather than a backoff retry (~65s)
#                                is wire-visible PROOF that control accepted
#                                the observation
#   STUN_GATE_GOFLAGS            extra `go test` flags (do not override -run)
#
# Exit: 0 iff no case FAILed (SKIP cases are reported and marked as partial
# evidence; the §23.5 blocking GO requires a full all-PASS matrix). Any
# missing case result (e.g. the named test does not exist yet) is a FAIL.

set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd "${script_dir}/.." && pwd)"
control_dir="${repo_root}/control"

target="local"
selected_cases=""
gate_server="${STUN_GATE_SERVER:-}"
gate_api_key="${STUN_GATE_API_KEY:-}"
gate_stun_addr="${STUN_GATE_STUN_ADDR:-}"
gate_expected_ip="${STUN_GATE_EXPECTED_PUBLIC_IP:-}"
gate_cert_fp="${STUN_GATE_CERT_FINGERPRINT:-}"
wait_rechallenge="${STUN_GATE_WAIT_RECHALLENGE:-}"

usage() {
  sed -n '2,78p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'
  exit 0
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --target)              target="$2"; shift 2 ;;
    --case)                selected_cases="$2"; shift 2 ;;
    --server)              gate_server="$2"; shift 2 ;;
    --stun-addr)           gate_stun_addr="$2"; shift 2 ;;
    --expected-public-ip)  gate_expected_ip="$2"; shift 2 ;;
    --no-wait-rechallenge) wait_rechallenge="0" ;;
    -h|--help)             usage ;;
    *) echo "ERROR: unknown argument $1 (see --help)" >&2; exit 1 ;;
  esac
done

log() { printf '%s\n' "$*" >&2; }
fail() { log "ERROR: $*"; exit 1; }

case "$target" in
  local)  ;;
  remote) [[ -n "$gate_server" ]]  || fail "remote mode requires --server wss://<control host> (or STUN_GATE_SERVER)"
          [[ -n "$gate_api_key" ]] || fail "remote mode requires STUN_GATE_API_KEY in the environment (never a flag)"
          [[ "$gate_server" == ws://* || "$gate_server" == wss://* ]] \
                                   || fail "STUN_GATE_SERVER must be an explicit ws:// or wss:// URL, got: ${gate_server}" ;;
  *)      fail "--target must be 'local' or 'remote', got: ${target}" ;;
esac

# The named case matrix is the gate contract: the script always expects a
# result for exactly these seven cases; anything missing is a FAIL.
ALL_CASES=(owner-router-nat phone-hotspot blocked-udp spoof mismatched-egress receipt-replay expired-challenge)

# Validate requested case names against the contract (typo safety).
if [[ -n "$selected_cases" ]]; then
  IFS=',' read -r -a requested <<< "$selected_cases"
  for want in "${requested[@]}"; do
    ok=""
    for name in "${ALL_CASES[@]}"; do [[ "$want" == "$name" ]] && ok=1; done
    [[ -n "$ok" ]] || fail "unknown case '${want}'; valid cases: ${ALL_CASES[*]}"
  done
fi

# Export the knobs the Go helper consumes.
export STUN_GATE_TARGET="$target"
export STUN_GATE_CASES="$selected_cases"
export STUN_GATE_SERVER="$gate_server"
export STUN_GATE_API_KEY="$gate_api_key"
export STUN_GATE_STUN_ADDR="$gate_stun_addr"
export STUN_GATE_EXPECTED_PUBLIC_IP="$gate_expected_ip"
export STUN_GATE_CERT_FINGERPRINT="$gate_cert_fp"
export STUN_GATE_WAIT_RECHALLENGE="$wait_rechallenge"

log "stun-nat-gate: target=${target} cases=${selected_cases:-all}"

output_file="$(mktemp "${TMPDIR:-/tmp}/stun-nat-gate-output.XXXXXX")"
# Preserve the helper output for diagnosis when the run fails (its path is
# printed and its tail shown below); remove it only on a clean exit.
trap 'if [[ "${go_status:-1}" -eq 0 ]]; then rm -f -- "$output_file"; fi' EXIT

# Run the helper. Markers on stdout:
#   STUN_GATE_CASE     <name> <PASS|FAIL|SKIP> <detail...>
#   STUN_GATE_EVIDENCE <case> <key>=<hash-or-value>...   (never a secret)
# Everything else is pass-through `go test -v` output.
pushd "$control_dir" >/dev/null
  set +e
  # shellcheck disable=SC2086
  go test ./internal/directctl -run '^TestSTUNGateLocalAllCasesPass$|^TestSTUNGateRemoteCases$' \
    -race -count=1 -v ${STUN_GATE_GOFLAGS:-} >"$output_file" 2>&1
  go_status=$?
  set -e
popd >/dev/null

if [[ $go_status -ne 0 ]]; then
  log "go test exited nonzero (status ${go_status}); full helper output preserved at: ${output_file}"
  log "last 40 lines of helper output:"
  tail -n 40 "$output_file" >&2 || true
fi

# Parse markers into the gate table. Missing results are synthesized FAILs so
# a missing/broken named test can never silently pass the gate (RED-safe).
# Parallel indexed arrays (not declare -A): macOS still ships bash 3.2.
GATE_NAMES=("${ALL_CASES[@]}")
for idx in "${!GATE_NAMES[@]}"; do
  GATE_RESULTS[$idx]="MISSING"
  GATE_DETAILS[$idx]="no result marker (named test missing, filtered out, or helper crashed)"
done
evidence_lines=""

gate_index_of() {
  local want="$1" i
  for i in "${!GATE_NAMES[@]}"; do
    [[ "${GATE_NAMES[$i]}" == "$want" ]] && { printf '%s' "$i"; return 0; }
  done
  return 1
}

while IFS= read -r line; do
  if [[ "$line" == *"STUN_GATE_CASE "* ]]; then
    rest="${line##*STUN_GATE_CASE }"
    name="${rest%% *}"
    rest="${rest#* }"
    result="${rest%% *}"
    detail="${rest#* }"
    case "$result" in
      PASS|FAIL|SKIP)
        if idx="$(gate_index_of "$name")"; then
          GATE_RESULTS[$idx]="$result"
          GATE_DETAILS[$idx]="$detail"
        fi
        ;;
    esac
  elif [[ "$line" == *"STUN_GATE_EVIDENCE "* ]]; then
    evidence_lines+="${line##*STUN_GATE_EVIDENCE }"$'\n'
  fi
done < "$output_file"

# Per-case PASS/FAIL table (stdout; details kept short, hashes only).
printf '\n'
printf 'STUN NAT gate (target=%s) — spec §§10.1/10.3/23.5\n' "$target"
printf '%-20s %-7s %s\n' "CASE" "RESULT" "DETAIL"
gate_failed=0
gate_skipped=0
for idx in "${!GATE_NAMES[@]}"; do
  name="${GATE_NAMES[$idx]}"
  result="${GATE_RESULTS[$idx]}"
  [[ "$result" == SKIP ]] && gate_skipped=$((gate_skipped + 1))
  [[ "$result" == FAIL || "$result" == MISSING ]] && gate_failed=$((gate_failed + 1))
  printf '%-20s %-7s %s\n' "$name" "$result" "${GATE_DETAILS[$idx]}"
done
printf '\n'

if [[ -n "$evidence_lines" ]]; then
  printf 'Evidence (SHA-256 hashes only — never secrets):\n'
  printf '%s' "$evidence_lines"
  printf '\n'
fi

total=${#ALL_CASES[@]}
passed=$((total - gate_failed - gate_skipped))
printf 'SUMMARY: %d/%d PASS, %d SKIP, %d FAIL/MISSING\n' "$passed" "$total" "$gate_skipped" "$gate_failed"

if [[ $gate_skipped -gt 0 ]]; then
  log "NOTE: ${gate_skipped} case(s) SKIPed — this run is PARTIAL evidence; the blocking §23.5 GO needs the full all-PASS matrix (docs/operations/evidence/phase4a-stun-nat-gate.md)"
fi

if [[ $go_status -ne 0 && $gate_failed -eq 0 ]]; then
  # A crashed helper with parsed FAILs is covered by the FAIL count; a
  # nonzero go test with no FAIL markers (compile error, panic before any
  # marker) must still fail the gate.
  log "NOTE: go test status ${go_status} without FAIL markers (compile error or crash before any case completed)"
  exit 1
fi

if [[ $gate_failed -gt 0 ]]; then
  exit 1
fi
exit 0
