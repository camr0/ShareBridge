#!/usr/bin/env bash
# live-phase4a.sh — ShareBridge Phase 4a M6 live acceptance harness
# (plan: docs/superpowers/plans/2026-09-03-phase4a-relay-mvp.md Tasks 37–44;
# spec §§19, 20, 23.9).
#
# Task 37 ships the SKELETON: the named gate `gate_23_9_dark_topology` plus
# registered placeholders for every later M6 acceptance case, so Tasks 38–44
# each add exactly ONE gate function + ONE registration line and nothing else.
#
#   Task 37  gate_23_9_dark_topology            §23.9 dark separate-VM topology
#   Task 38  acceptance_01_owner_hairpin        §19 #1 owner hairpin
#   Task 38  acceptance_04_interstitial_blackhole §19 #4 interstitial fallback
#   Task 39  acceptance_02_cellular_relay_video §19 #2 cellular relay + 206 seeks
#   Task 39  acceptance_11_content_parity       §19 #11 Phase 3 content parity
#   Task 40  acceptance_03_relay_only           §19 #3 relayOnly end to end
#   Task 41  l4_no_plaintext_capture            §19 #5 / §18.4 L4 passthrough proof
#   Task 42  acceptance_06_no_mapper_enrollment §19 #6 no-mapper enrollment
#   Task 42  acceptance_12_stun_mismatch        §19 #12 STUN mismatch -> relay
#   Task 42  acceptance_13_stun_cadence_cold_budget §19 #13 cadence/cold budget
#   Task 43  acceptance_07_no_relay_open_signal §19 #7 no relay open signal
#   Task 43  acceptance_08_exact_routing        §19 #8 exact routing
#   Task 43  acceptance_09_restart_recovery     §19 #9 restart recovery
#   Task 43  acceptance_10_lockdown             §19 #10 lockdown
#   Task 43  acceptance_15_heartbeat_tunnel_dns §19 #15 heartbeat + tunnel DNS
#   Task 44  release_go_no_go_rollback          §20 steps 5–7 + §23 blocking rule
#
# §19 #14 (no-JS/CSP fallback) is a Task 24 hermetic browser gate, not part of
# this harness; it is deliberately absent.
#
# HONESTY CONTRACT (the project has been bitten by skip-as-PASS twice):
#   * A gate that executes no checks is MISSING, never PASS.
#   * A gate needs at least one measured, PASSing `check`: note() output is
#     informational and never a measurement. A note-only (or otherwise
#     check-less) gate is MISSING, never PASS.
#   * A gate whose function exits nonzero after recording only PASSes is FAIL.
#   * A registered-but-unimplemented gate is NOT_IMPLEMENTED, never PASS.
#   * A gate with any SKIP sub-check is SKIP, never PASS.
#   * An unscoped (default) run is GREEN only if every registered gate is PASS.
#   * `--case a,b` scopes the verdict to the named gates and explicitly lists
#     the unselected ones as NOT_RUN / excluded — an unrun gate is never a pass.
#   * `--dry-run` executes nothing and exits PARTIAL; it is NOT a pass.
#
# SAFETY:
#   * Read-only remote commands only (systemctl show/cat, nft list, ss, curl,
#     dig, openssl s_client, ip addr, grep). Nothing is written remotely.
#   * The only writes are local evidence files under the evidence directory.
#   * The optional restart drill is off unless LIVE_PHASE4A_ALLOW_RESTART=1.
#     acceptance_09_restart_recovery uses the same flag to restart the pinned
#     frps unit (it never restarts the agent's frpc child).
#   * Secrets are never printed: every captured line and every check() detail
#     passes through sanitize(), which redacts key/token/password/secret/
#     cookie/authorization/jti values, private-key blocks and share codes.
#     `--dry-run` prints variable NAMES, never values that look secret.
#   * Idempotent: repeated runs only create new timestamped evidence records.
#
# THE M6 RELAY VM IS NOT THE COLLOCATED TEST VPS. The down test VPS
# (178.156.174.47 by default) is explicitly rejected as the M6 relay target
# (LIVE_PHASE4A_EXCLUDED_RELAY_IPS). If no M6 topology exists, the correct
# outcome is RED with a diagnostic naming each missing input — that is the
# Task 37 Step 2 RED, not a failure of this script.
#
# Usage:
#   scripts/live-phase4a.sh                      # run every registered gate (live)
#   scripts/live-phase4a.sh --case gate_23_9_dark_topology
#   scripts/live-phase4a.sh --list               # print the gate table
#   scripts/live-phase4a.sh --dry-run            # print config + plan; runs nothing
#   scripts/live-phase4a.sh --selftest           # prove the pass/fail plumbing
#   scripts/live-phase4a.sh --help
#
# Exit codes:
#   0  GREEN   — every selected gate PASS (and, unscoped, all registered gates)
#   1  RED     — at least one selected gate FAIL / MISSING / NOT_IMPLEMENTED,
#                or the selftest plumbing is broken
#   2  USAGE   — bad arguments
#   3  PARTIAL — no failures, but at least one SKIP (skip is not a pass), or a
#                dry run (nothing executed)
#
# Environment (all resolved at startup; unset required values FAIL the gate and
# are named in the diagnostic — never guessed). Values are paths/names, not
# secrets; never put credentials in them.
#   LIVE_PHASE4A_CONTROL_HOST          ssh target of the control VM (required)
#   LIVE_PHASE4A_RELAY_HOST            ssh target of the NEW relay VM (required).
#                                      It is ALSO used DIRECTLY as an SSH target for the
#                                      loopback /metrics read (when LIVE_PHASE4A_GATEWAY_METRICS_URL
#                                      is unset), so it must be user-qualified whenever the harness
#                                      host has no default user for that machine (e.g. root@10.0.0.5,
#                                      not a bare IP).
#   LIVE_PHASE4A_CONTROL_PUBLIC_HOST   optional public control host for probes
#   LIVE_PHASE4A_CONTROL_PORT          control public port for agent-connection info (default 8080)
#   LIVE_PHASE4A_RELAY_PUBLIC_IP       relay public IPv4 (required for firewall/DNS)
#   LIVE_PHASE4A_RELAY_TUNNEL_HOST     <relay-tunnel-host> (required for DNS/cert)
#   LIVE_PHASE4A_TRANSPORT_PORT        FRP transport port (default 7000)
#   LIVE_PHASE4A_BASE_DOMAIN           content base domain (default sharebridgeusercontent.com)
#   LIVE_PHASE4A_NAMESPACE             enrolled test namespace sbXXXXXXXX (wildcard DNS)
#   LIVE_PHASE4A_IMMICH_URL            real Immich base URL on the home Mac (required)
#   LIVE_PHASE4A_AGENT_PID_MATCH       pgrep -f pattern for the home Mac agent (default sharebridge-agent)
#   LIVE_PHASE4A_CONTROL_UNIT          control systemd unit (default sharebridge.service)
#   LIVE_PHASE4A_GATEWAY_UNIT          relay gateway unit (default sharebridge-relay-gateway.service)
#   LIVE_PHASE4A_FRPS_UNIT             relay frps unit (default sharebridge-relay-frps.service)
#   LIVE_PHASE4A_CONTROL_ENV_FILE      control env file on the control VM (default /opt/sharebridge/.env)
#   LIVE_PHASE4A_GATEWAY_ENV_FILE      gateway env file on the relay VM (default /etc/sharebridge/relay/gateway.env)
#   LIVE_PHASE4A_TRANSPORT_CA_FILE     transport CA (PEM) for tunnel cert validation (required for that check)
#   LIVE_PHASE4A_SYNC_CA_FILE          sync CA PEM, path ON THE RELAY VM (required for the mTLS handshake check)
#   LIVE_PHASE4A_GATEWAY_SYNC_CERT     gateway sync client cert, path ON THE RELAY VM
#   LIVE_PHASE4A_GATEWAY_SYNC_KEY      gateway sync client key, path ON THE RELAY VM
#   LIVE_PHASE4A_HEALTHZ_URL           gateway health URL probed on the relay VM (default http://127.0.0.1:9101/healthz)
#   LIVE_PHASE4A_HETZNER_METADATA_URL  Hetzner metadata endpoint (default http://169.254.169.254/hetzner/v1/metadata)
#   LIVE_PHASE4A_EXCLUDED_RELAY_IPS    comma list of IPs that must NOT be the relay (default 178.156.174.47)
#   LIVE_PHASE4A_EVIDENCE_DIR          evidence root (default docs/operations/evidence/runs)
#   LIVE_PHASE4A_ALLOW_RESTART         1 enables the guarded live restart drill (default 0)
#   LIVE_PHASE4A_SSH_OPTS              extra ssh options, word-split
#   LIVE_PHASE4A_SSH_CONNECT_TIMEOUT   ssh ConnectTimeout seconds (default 8)
#   LIVE_PHASE4A_REMOTE_TIMEOUT        remote command timeout seconds (default 20)
#   LIVE_PHASE4A_CONTROL_BASE_URL      control interstitial base URL for prepare-route
#                                      (required: acceptance_09_restart_recovery)
#   LIVE_PHASE4A_CONTROL_INSECURE_TLS  1 disables control TLS verification (default 0)
#   LIVE_PHASE4A_RELAY_INSECURE_TLS    1 disables relay TLS verification (default 0)
#   LIVE_PHASE4A_SHARE_CODE            share code for the restart-recovery relay fetch
#                                      (required: acceptance_09_restart_recovery)
#   LIVE_PHASE4A_GATEWAY_METRICS_URL   full gateway /metrics URL reachable from here
#                                      (optional; else fetched over SSH from LIVE_PHASE4A_RELAY_HOST)
#   LIVE_PHASE4A_GATEWAY_METRICS_ADDR  loopback gateway metrics addr over SSH (default 127.0.0.1:9101)
#   LIVE_PHASE4A_FRPS_SSH_HOST         ssh target hosting the pinned frps unit
#                                      (default: LIVE_PHASE4A_RELAY_HOST)
#   LIVE_PHASE4A_AGENT_SSH_HOST        ssh target of the home agent for the frpc child identity
#                                      (optional; unset means the harness host runs the agent)
#   LIVE_PHASE4A_FRPC_PID_MATCH        pgrep -f PRE-FILTER for the agent's frpc child
#                                      (default frpc). It is only a candidate filter: the
#                                      identity is restricted to processes whose exact name is
#                                      LIVE_PHASE4A_FRPC_PID_NAME, so the harness's own
#                                      shell/timeout/pgrep wrapper command lines can never enter
#                                      the recorded identity
#   LIVE_PHASE4A_FRPC_PID_NAME         exact process name (comm) of the frpc child for the default
#                                      identity discovery (default frpc)
#   LIVE_PHASE4A_FRPC_IDENTITY_CMD     OPTIONAL operator override: a shell command run on the agent
#                                      host (over LIVE_PHASE4A_AGENT_SSH_HOST when set, else locally)
#                                      whose stdout is EXACTLY ONE `PID:STARTTIME` line for the
#                                      agent's frpc child (e.g. `docker exec agent pgrep -x frpc`
#                                      plus a start-time read). Use this for containerised agents
#                                      where the default discovery cannot see the process. The
#                                      identity must be stable across reads and change on restart
#   LIVE_PHASE4A_FRPS_JOURNAL_FILE     operator-supplied frps journal/evidence file (preferred over
#                                      SSH; used for the post-restart fresh-session proof)
#   LIVE_PHASE4A_JOURNAL_MAX_LINES     journal tail line budget for SSH reads (default 5000)
#   LIVE_PHASE4A_AGENT_PROXY_NAME      the frps proxy name for the agent under test, used as the
#                                      agent-specific post-restart session proof (default:
#                                      sb-<LIVE_PHASE4A_NAMESPACE> when the namespace is set)
#   LIVE_PHASE4A_FRPS_RESTART_CONFIRM  REQUIRED for acceptance_09: the EXACT `<ssh-host>|<systemd-unit>`
#                                      the harness intends to restart (it must equal
#                                      `<LIVE_PHASE4A_FRPS_SSH_HOST>|<LIVE_PHASE4A_FRPS_UNIT>`),
#                                      e.g. `root@10.0.0.5|sharebridge-relay-frps.service`. Never
#                                      restarts an unconfirmed target
#   LIVE_PHASE4A_RECOVERY_BOUND_S      automatic restart-recovery bound seconds (default 120)
#   LIVE_PHASE4A_TUNNEL_OFFLINE_BOUND_S  post-restart tunnel-offline bound seconds (default 30)
#
# Evidence: every gate writes docs/operations/evidence/runs/<run-id>/gate-<name>.txt
# (timestamp, git sha, commands, sanitised output, per-check PASS/FAIL, result
# and a content hash) plus summary.txt, manifest.txt and environment-facts.txt.
# Copy the facts into docs/operations/evidence/phase4a-environment.md; never
# paste a value the run did not observe.

set -u -o pipefail

usage() {
  # Print the header comment block (everything after the shebang up to the
  # first non-comment line), so the help text cannot drift from the file.
  awk 'NR==1 { next } /^[^#]/ { exit } { sub(/^# ?/, ""); print }' "${BASH_SOURCE[0]}"
  exit 0
}

# ---------------------------------------------------------------------------
# Argument parsing
# ---------------------------------------------------------------------------

MODE="live"
DRY_RUN=0
SELECTED_CASES=""
EVIDENCE_DIR_OVERRIDE=""
LIST_ONLY=0

while [[ $# -gt 0 ]]; do
  case "$1" in
    --case)          SELECTED_CASES="$2"; shift 2 ;;
    --dry-run)       DRY_RUN=1; shift ;;
    --selftest)      MODE="selftest"; shift ;;
    --list)          LIST_ONLY=1; shift ;;
    --evidence-dir)  EVIDENCE_DIR_OVERRIDE="$2"; shift 2 ;;
    -h|--help)       usage; exit 0 ;;
    *) printf 'ERROR: unknown argument %s (see --help)\n' "$1" >&2; exit 2 ;;
  esac
done

# ---------------------------------------------------------------------------
# Configuration (documented env vars; no hard-coded hosts)
# ---------------------------------------------------------------------------

CONTROL_HOST="${LIVE_PHASE4A_CONTROL_HOST:-}"
RELAY_HOST="${LIVE_PHASE4A_RELAY_HOST:-}"
CONTROL_PUBLIC_HOST="${LIVE_PHASE4A_CONTROL_PUBLIC_HOST:-}"
CONTROL_PORT="${LIVE_PHASE4A_CONTROL_PORT:-8080}"
RELAY_PUBLIC_IP="${LIVE_PHASE4A_RELAY_PUBLIC_IP:-}"
RELAY_TUNNEL_HOST="${LIVE_PHASE4A_RELAY_TUNNEL_HOST:-}"
TRANSPORT_PORT="${LIVE_PHASE4A_TRANSPORT_PORT:-7000}"
BASE_DOMAIN="${LIVE_PHASE4A_BASE_DOMAIN:-sharebridgeusercontent.com}"
NAMESPACE="${LIVE_PHASE4A_NAMESPACE:-}"
IMMICH_URL="${LIVE_PHASE4A_IMMICH_URL:-}"
AGENT_PID_MATCH="${LIVE_PHASE4A_AGENT_PID_MATCH:-sharebridge-agent}"
CONTROL_UNIT="${LIVE_PHASE4A_CONTROL_UNIT:-sharebridge.service}"
GATEWAY_UNIT="${LIVE_PHASE4A_GATEWAY_UNIT:-sharebridge-relay-gateway.service}"
FRPS_UNIT="${LIVE_PHASE4A_FRPS_UNIT:-sharebridge-relay-frps.service}"
CONTROL_ENV_FILE="${LIVE_PHASE4A_CONTROL_ENV_FILE:-/opt/sharebridge/.env}"
GATEWAY_ENV_FILE="${LIVE_PHASE4A_GATEWAY_ENV_FILE:-/etc/sharebridge/relay/gateway.env}"
TRANSPORT_CA_FILE="${LIVE_PHASE4A_TRANSPORT_CA_FILE:-}"
SYNC_CA_FILE="${LIVE_PHASE4A_SYNC_CA_FILE:-}"
GATEWAY_SYNC_CERT="${LIVE_PHASE4A_GATEWAY_SYNC_CERT:-}"
GATEWAY_SYNC_KEY="${LIVE_PHASE4A_GATEWAY_SYNC_KEY:-}"
HEALTHZ_URL="${LIVE_PHASE4A_HEALTHZ_URL:-http://127.0.0.1:9101/healthz}"
HETZNER_METADATA_URL="${LIVE_PHASE4A_HETZNER_METADATA_URL:-http://169.254.169.254/hetzner/v1/metadata}"
EXCLUDED_RELAY_IPS="${LIVE_PHASE4A_EXCLUDED_RELAY_IPS:-178.156.174.47}"
EVIDENCE_DIR="${EVIDENCE_DIR_OVERRIDE:-${LIVE_PHASE4A_EVIDENCE_DIR:-docs/operations/evidence/runs}}"
ALLOW_RESTART="${LIVE_PHASE4A_ALLOW_RESTART:-0}"
SSH_CONNECT_TIMEOUT="${LIVE_PHASE4A_SSH_CONNECT_TIMEOUT:-8}"
REMOTE_TIMEOUT="${LIVE_PHASE4A_REMOTE_TIMEOUT:-20}"
CONTROL_BASE_URL="${LIVE_PHASE4A_CONTROL_BASE_URL:-}"
CONTROL_INSECURE_TLS="${LIVE_PHASE4A_CONTROL_INSECURE_TLS:-0}"
RELAY_INSECURE_TLS="${LIVE_PHASE4A_RELAY_INSECURE_TLS:-0}"
SHARE_CODE="${LIVE_PHASE4A_SHARE_CODE:-}"
GATEWAY_METRICS_URL="${LIVE_PHASE4A_GATEWAY_METRICS_URL:-}"
GATEWAY_METRICS_ADDR="${LIVE_PHASE4A_GATEWAY_METRICS_ADDR:-127.0.0.1:9101}"
FRPS_SSH_HOST="${LIVE_PHASE4A_FRPS_SSH_HOST:-}"
AGENT_SSH_HOST="${LIVE_PHASE4A_AGENT_SSH_HOST:-}"
FRPC_PID_MATCH="${LIVE_PHASE4A_FRPC_PID_MATCH:-frpc}"
FRPC_PID_NAME="${LIVE_PHASE4A_FRPC_PID_NAME:-frpc}"
FRPC_IDENTITY_CMD="${LIVE_PHASE4A_FRPC_IDENTITY_CMD:-}"
FRPS_JOURNAL_FILE="${LIVE_PHASE4A_FRPS_JOURNAL_FILE:-}"
JOURNAL_MAX_LINES="${LIVE_PHASE4A_JOURNAL_MAX_LINES:-5000}"
AGENT_PROXY_NAME="${LIVE_PHASE4A_AGENT_PROXY_NAME:-}"
FRPS_RESTART_CONFIRM="${LIVE_PHASE4A_FRPS_RESTART_CONFIRM:-}"
RECOVERY_BOUND_S="${LIVE_PHASE4A_RECOVERY_BOUND_S:-120}"
TUNNEL_OFFLINE_BOUND_S="${LIVE_PHASE4A_TUNNEL_OFFLINE_BOUND_S:-30}"

CONTROL_TLS_ARGS=()
[[ "$CONTROL_INSECURE_TLS" == "1" ]] && CONTROL_TLS_ARGS=(-k)
RELAY_TLS_ARGS=()
[[ "$RELAY_INSECURE_TLS" == "1" ]] && RELAY_TLS_ARGS=(-k)

SSH_EXTRA=()
if [[ -n "${LIVE_PHASE4A_SSH_OPTS:-}" ]]; then
  read -r -a SSH_EXTRA <<< "${LIVE_PHASE4A_SSH_OPTS}"
fi

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

GIT_SHA="$(git -C "$REPO_ROOT" rev-parse --short=12 HEAD 2>/dev/null || echo unknown)"
RUN_ID="$(date -u +%Y%m%dT%H%M%SZ)"
RUN_DIR="${EVIDENCE_DIR}/${RUN_ID}"

MSYS=0
uname_s="$(uname -s 2>/dev/null || echo unknown)"

# ---------------------------------------------------------------------------
# Gate registry — ONE line per gate; Tasks 38–44 replace one placeholder body
# and add nothing here unless they add a new named acceptance case.
# ---------------------------------------------------------------------------

GATE_TABLE=(
  "gate_23_9_dark_topology|Task 37|dark separate-VM topology: control/relay reachability, distinct same-region relay VM + private network, home agent + real Immich, minimal public firewall, private mTLS sync, tunnel DNS + transport cert, snapshot route_ready, restart ordering, dark selection flag"
  "acceptance_01_owner_hairpin|Task 38|§19 #1 owner hairpin: browser direct check times out and the same share loads over the exact relay origin"
  "acceptance_04_interstitial_blackhole|Task 38|§19 #4 deterministic interstitial fallback after blackholing direct post-preparation"
  "acceptance_02_cellular_relay_video|Task 39|§19 #2 cellular/no-direct-path gallery + video with at least two valid 206 seeks over relay"
  "acceptance_11_content_parity|Task 39|§19 #11 Phase 3 gallery/preview/original/archive/accounting/Range parity through relay"
  "acceptance_03_relay_only|Task 40|§19 #3 relayOnly end to end with zero direct DNS/probe/open-signal/mapper/browser activity"
  "l4_no_plaintext_capture|Task 41|§19 #5 / §18.4 no-plaintext L4 passthrough canary/capture proof (scripts/l4-canary-capture.sh)"
  "acceptance_06_no_mapper_enrollment|Task 42|§19 #6 CGNAT/UPnP-off agent enrolls and serves solely over the outbound tunnel"
  "acceptance_12_stun_mismatch|Task 42|§19 #12 STUN egress mismatch -> relay_fallback diagnostic, no public probe, relay serves"
  "acceptance_13_stun_cadence_cold_budget|Task 42|§19 #13 immediate post-reconnect + four-minute cadence, no warm repeat, cold budget within four seconds"
  "acceptance_07_no_relay_open_signal|Task 43|§19 #7 route=relay emits no open_signal/open_ack/direct probe/mapper call"
  "acceptance_08_exact_routing|Task 43|§19 #8 unknown/random/bare/tombstoned SNI never reaches an agent; exact route reaches only its owner"
  "acceptance_09_restart_recovery|Task 43|§19 #9 restart recovery: exact restart-target confirmation; baseline tunnel online + serving relay; restart frps with the frpc child ALIVE (identity re-measured after the restart); observe online=0; then require a REPLACED PID:STARTTIME child + a NEW agent-specific frps proxy-registration session (global counter corroborates) + online=1 + serving content, elapsed measured after all stages and within the monotonic bound, with no operator action"
  "acceptance_10_lockdown|Task 43|§19 #10 lockdown drops direct mapping + tunnel, closes both connection kinds, unlock with fresh credential"
  "acceptance_15_heartbeat_tunnel_dns|Task 43|§19 #15 tunnel DNS + dedicated transport cert, 10s Pings, one delayed Ping tolerated, true 45s expiry"
  "release_go_no_go_rollback|Task 44|§20 steps 5–7 + §23 blocking rule: release manifest gate + staged fallback + rollback drill"
)

gate_names=()
gate_owners=()
gate_purposes=()
for entry in "${GATE_TABLE[@]}"; do
  gate_names+=("$(printf '%s' "$entry" | cut -d'|' -f1)")
  gate_owners+=("$(printf '%s' "$entry" | cut -d'|' -f2)")
  gate_purposes+=("$(printf '%s' "$entry" | cut -d'|' -f3-)")
done

gate_index_of() {
  local want="$1" i
  for i in "${!gate_names[@]}"; do
    [[ "${gate_names[$i]}" == "$want" ]] && { printf '%s' "$i"; return 0; }
  done
  return 1
}

is_selected() {
  local name="$1"
  if [[ -z "$SELECTED_CASES" ]]; then return 0; fi
  case ",${SELECTED_CASES}," in
    *",${name},"*) return 0 ;;
    *) return 1 ;;
  esac
}

# Validate --case names before any work happens (typo safety).
if [[ -n "$SELECTED_CASES" ]]; then
  IFS=',' read -r -a requested_cases <<< "$SELECTED_CASES"
  for want in "${requested_cases[@]}"; do
    gate_index_of "$want" >/dev/null || {
      printf 'ERROR: unknown gate %s\n' "$want" >&2
      printf 'Registered gates:\n' >&2
      for n in "${gate_names[@]}"; do printf '  %s\n' "$n" >&2; done
      exit 2
    }
  done
fi

# ---------------------------------------------------------------------------
# Output / sanitising helpers
# ---------------------------------------------------------------------------

log() { printf '%s\n' "$*" >&2; }

# sanitize: stdin -> stdout. Redacts credentials, private-key blocks and share
# codes. Never removes data we rely on for a verdict (it only rewrites secrets).
sanitize() {
  awk '
    /BEGIN [A-Z ]*PRIVATE KEY/ { print "[REDACTED: private key material]"; inkey=1; next }
    inkey && /^[A-Za-z0-9+\/=]+$/ { next }
    { inkey=0; print }
  ' | sed -E \
    -e "s#([Aa][Pp][Ii][_-]?[Kk][Ee][Yy]|[Aa]uthorization|[Bb]earer|[Pp]assword|[Ss]ecret|[Cc]ookie|[Tt]oken)([[:space:]]*[=:][[:space:]]*|[[:space:]]+)[^[:space:],;\"']+#\1=[REDACTED]#g" \
    -e "s#([^A-Za-z0-9]|^)(jti|JTI)([=:][[:space:]]*)?[A-Za-z0-9._-]{8,}#\1\2=[REDACTED]#g" \
    -e "s#/s/[A-Za-z0-9_-]{6,}#/s/[REDACTED-SHARE-CODE]#g" \
    -e "s#(/api/shares/|/shares/)[A-Za-z0-9_-]{4,}#\1[REDACTED-SHARE-CODE]#g"
}

sha256_file() {
  if command -v shasum >/dev/null 2>&1; then
    shasum -a 256 "$1" | awk '{print $1}'
  elif command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | awk '{print $1}'
  else
    printf 'unavailable'
  fi
}

# ---------------------------------------------------------------------------
# Per-gate check plumbing. `check` is invoked inside the gate's subshell and
# appends to the per-gate temp files so a crash cannot lose recorded checks.
# ---------------------------------------------------------------------------

check() {
  # check <check-name> <PASS|FAIL|SKIP|NOTE> <observed...>
  # The detail is operator-visible evidence, so it passes through the same
  # sanitiser as captured output: a value that looks like a secret is redacted
  # in both the console line and the recorded check file.
  local cname="$1" verdict="$2"
  shift 2
  local detail
  detail="$(printf '%s' "$*" | sanitize)"
  if [[ -z "${GATE_CHECK_FILE:-}" ]]; then
    printf 'ERROR: check() called outside the gate runner\n' >&2
    return 1
  fi
  printf '%s|%s|%s\n' "$verdict" "$cname" "$detail" >> "$GATE_CHECK_FILE"
  printf '  [%s] %s: %s\n' "$verdict" "$cname" "$detail"
}

note() { check "$1" NOTE "$2"; }

record_cmd() {
  # Command evidence is secret-bearing: a recorded prepare-route URL carries the
  # share code, and the sanitiser redacts it (`/s/<code>` plus key/value-shaped
  # secrets) before anything reaches the evidence file.
  [[ -n "${GATE_CMD_FILE:-}" ]] || return 0
  printf '%s\n' "$1" | sanitize >> "$GATE_CMD_FILE"
}

record_out() {
  [[ -n "${GATE_OUT_FILE:-}" ]] || return 0
  printf '%s\n' "$1" | sanitize >> "$GATE_OUT_FILE"
}

gate_not_implemented() {
  local task="$1" proves="$2"
  if [[ -n "${GATE_NI_FILE:-}" ]]; then
    printf '%s|%s\n' "$task" "$proves" > "$GATE_NI_FILE"
  fi
  printf '  [NOT_IMPLEMENTED] owner=%s — %s\n' "$task" "$proves"
}

# need_cfg: fail the named check when a required env input is missing.
# Returns 0 when the value is present so the caller can proceed.
need_cfg() {
  local cname="$1" var="$2" value="$3" desc="$4"
  if [[ -z "$value" ]]; then
    check "$cname" FAIL "$var is unset — cannot verify: $desc"
    return 1
  fi
  return 0
}

# ---------------------------------------------------------------------------
# Command runners — every remote/local command is recorded for the evidence.
# ---------------------------------------------------------------------------

remote_exec() {
  # remote_exec <ssh-target> <remote-command>
  local target="$1" remote="$2" wrapped out status
  record_cmd "ssh ${target} :: ${remote}"
  wrapped="if command -v timeout >/dev/null 2>&1; then timeout ${REMOTE_TIMEOUT} sh -c $(printf '%q' "$remote"); else sh -c $(printf '%q' "$remote"); fi"
  out="$(ssh -o BatchMode=yes -o ConnectTimeout="$SSH_CONNECT_TIMEOUT" ${SSH_EXTRA[@]+"${SSH_EXTRA[@]}"} "$target" "$wrapped" 2>&1)"
  status=$?
  record_out "--- ssh ${target} exit=${status} ---"$'\n'"${out}"
  printf '%s\n' "$out"
  return "$status"
}

local_exec() {
  # local_exec <shell-command>
  local remote="$1" out status
  record_cmd "sh -c :: ${remote}"
  out="$(sh -c "$remote" 2>&1)"
  status=$?
  record_out "--- local exit=${status} ---"$'\n'"${out}"
  printf '%s\n' "$out"
  return "$status"
}

tcp_open() {
  # tcp_open <host> <port> -> 0 when a TCP connection is accepted
  local h="$1" p="$2"
  if command -v nc >/dev/null 2>&1; then
    nc -z -w 3 "$h" "$p" >/dev/null 2>&1
    return $?
  fi
  (exec 3<>"/dev/tcp/$h/$p") >/dev/null 2>&1
}

json_str() {
  # json_str <json> <field>  (flat string fields only)
  printf '%s\n' "$1" | sed -n "s/.*\"$2\"[[:space:]]*:[[:space:]]*\"\([^\"]*\)\".*/\1/p" | head -n1
}

env_value() {
  # env_value <env-text> <VAR>
  printf '%s\n' "$1" | sed -n "s/^$2=//p" | tail -n1 | sed -e 's/^"//' -e 's/"$//'
}

is_private_bind() {
  # loopback or RFC1918 / ULA / link-local — never a public or wildcard bind
  local addr="$1" host
  host="${addr%%:*}"
  host="${host#[}"
  case "$host" in
    127.*|localhost|::1|"[::1]") return 0 ;;
    10.*|192.168.*) return 0 ;;
    172.1[6-9].*|172.2[0-9].*|172.3[01].*) return 0 ;;
    fd*|fc*|fe80*) return 0 ;;
    *) return 1 ;;
  esac
}

private_v4_list() {
  # private_v4_list <multiline ip -4 -o addr output>
  printf '%s\n' "$1" | awk '{print $4}' | cut -d/ -f1 | grep -E '^(10\.|192\.168\.|172\.(1[6-9]|2[0-9]|3[01])\.)' | sort -u
}

same_subnet24() {
  local a="$1" b="$2"
  [[ -n "$a" && -n "$b" && "${a%.*}" == "${b%.*}" ]]
}

# ---------------------------------------------------------------------------
# Gate runner
# ---------------------------------------------------------------------------

declare -a RESULT_NAMES=() RESULT_OWNERS=() RESULT_VALUES=() RESULT_DETAILS=()
declare -a RESULT_SELECTED=()

# Environment facts observed by gates. Gates run in a subshell, so observed
# facts are written to a per-gate file and accumulated into RUN_FACTS_FILE,
# which the environment record reads. A value the run did not observe is never
# printed as measured.
set_fact() {
  local key="$1" value="$2"
  [[ -n "${GATE_FACT_FILE:-}" ]] || return 0
  value="$(printf '%s' "$value" | tr '\n' ' ' | sanitize)"
  [[ -n "$value" ]] || value="PENDING (not observed)"
  printf '%s=%s\n' "$key" "$value" >> "$GATE_FACT_FILE"
}

fact_get() {
  local key="$1" value=""
  if [[ -n "${RUN_FACTS_FILE:-}" && -f "$RUN_FACTS_FILE" ]]; then
    value="$(grep "^${key}=" "$RUN_FACTS_FILE" | tail -n1 | cut -d= -f2-)"
  fi
  if [[ -n "$value" ]]; then printf '%s' "$value"; else printf 'PENDING (not observed)'; fi
}

execute_gate() {
  # execute_gate <fn-name> ; sets EXECUTE_RESULT (PASS|FAIL|SKIP|NOT_IMPLEMENTED|MISSING)
  local name="$1"
  local tmp
  tmp="$(mktemp -d "${TMPDIR:-/tmp}/live-phase4a-gate.XXXXXX")"
  GATE_CHECK_FILE="$tmp/checks"
  GATE_CMD_FILE="$tmp/cmds"
  GATE_OUT_FILE="$tmp/out"
  GATE_NI_FILE="$tmp/notimpl"
  GATE_FACT_FILE="$tmp/facts"
  : > "$GATE_CHECK_FILE"
  : > "$GATE_CMD_FILE"
  : > "$GATE_OUT_FILE"
  : > "$GATE_NI_FILE"
  : > "$GATE_FACT_FILE"
  local status=0
  if declare -f "$name" >/dev/null 2>&1; then
    ( "$name" ) || status=$?
  else
    status=127
  fi

  if [[ -s "$GATE_NI_FILE" ]]; then
    EXECUTE_RESULT="NOT_IMPLEMENTED"
    EXECUTE_DETAIL="$(cut -d'|' -f1 "$GATE_NI_FILE")"
  elif [[ ! -s "$GATE_CHECK_FILE" ]]; then
    EXECUTE_RESULT="MISSING"
    EXECUTE_DETAIL="gate executed zero checks (status=${status}) — refusing to report PASS"
  elif grep -q '^FAIL|' "$GATE_CHECK_FILE"; then
    EXECUTE_RESULT="FAIL"
    EXECUTE_DETAIL="$(grep '^FAIL|' "$GATE_CHECK_FILE" | head -n1 | cut -d'|' -f2)"
  elif [[ "$status" -ne 0 ]]; then
    EXECUTE_RESULT="FAIL"
    EXECUTE_DETAIL="gate exited nonzero (status=${status}) after recording checks"
  elif grep -q '^SKIP|' "$GATE_CHECK_FILE"; then
    EXECUTE_RESULT="SKIP"
    EXECUTE_DETAIL="$(grep '^SKIP|' "$GATE_CHECK_FILE" | head -n1 | cut -d'|' -f2)"
  elif ! grep -q '^PASS|' "$GATE_CHECK_FILE"; then
    # Notes are informational, not measurements: a PASS verdict requires at
    # least one `check ... PASS`. A gate that emits only notes (or only notes
    # and non-PASS checks that did not already decide the verdict) is MISSING.
    EXECUTE_RESULT="MISSING"
    EXECUTE_DETAIL="gate recorded $(grep -c '^NOTE|' "$GATE_CHECK_FILE") note(s) but zero measured PASS checks — refusing to report PASS"
  else
    EXECUTE_RESULT="PASS"
    EXECUTE_DETAIL="$(grep -c '^PASS|' "$GATE_CHECK_FILE") PASS check(s)"
  fi

  EXECUTE_TMP="$tmp"
  EXECUTE_STATUS="$status"
}

write_gate_evidence() {
  # write_gate_evidence <run-dir> <name> <owner> <purpose> <result> <detail> <tmp>
  local dir="$1" name="$2" owner="$3" purpose="$4" result="$5" detail="$6" tmp="$7"
  local file="${dir}/gate-${name}.txt"
  {
    printf '# Phase 4a live acceptance evidence — gate record\n'
    printf 'run_id=%s\n' "$RUN_ID"
    printf 'utc=%s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)"
    printf 'gate=%s\n' "$name"
    printf 'owner_task=%s\n' "$owner"
    printf 'purpose=%s\n' "$purpose"
    printf 'git_sha=%s\n' "$GIT_SHA"
    printf 'harness=%s\n' "scripts/live-phase4a.sh"
    printf 'control_host=%s\n' "${CONTROL_HOST:-<unset>}"
    printf 'relay_host=%s\n' "${RELAY_HOST:-<unset>}"
    printf 'result=%s\n' "$result"
    printf 'result_detail=%s\n' "$detail"
    printf 'checks:\n'
    sed 's/^/  /' "$tmp/checks"
    printf 'commands:\n'
    if [[ -s "$tmp/cmds" ]]; then sed 's/^/  /' "$tmp/cmds"; else printf '  <none>\n'; fi
    printf 'sanitised_output:\n'
    if [[ -s "$tmp/out" ]]; then sed 's/^/  /' "$tmp/out"; else printf '  <none>\n'; fi
  } > "$file"
  printf 'evidence_sha256_over_header_and_body=%s\n' "$(sha256_file "$file")" >> "$file"
}

run_all_gates() {
  local entry name owner purpose i result detail
  printf 'run_id=%s\n' "$RUN_ID"
  printf 'git_sha=%s\n' "$GIT_SHA"
  printf 'mode=%s\n' "$MODE"
  printf 'scope=%s\n' "$([[ -z "$SELECTED_CASES" ]] && echo all-registered || echo "$SELECTED_CASES")"
  printf 'evidence_dir=%s\n' "$RUN_DIR"
  printf 'control_host=%s\n' "${CONTROL_HOST:-<unset>}"
  printf 'relay_host=%s\n' "${RELAY_HOST:-<unset>}"
  printf 'relay_tunnel_host=%s\n' "${RELAY_TUNNEL_HOST:-<unset>}"
  printf 'namespace=%s\n' "${NAMESPACE:-<unset>}"

  if ! mkdir -p "$RUN_DIR"; then
    printf 'ERROR: cannot create evidence directory %s\n' "$RUN_DIR" >&2
    exit 1
  fi
  RUN_FACTS_FILE="$RUN_DIR/environment-facts.observed"
  : > "$RUN_FACTS_FILE"

  for i in "${!gate_names[@]}"; do
    name="${gate_names[$i]}"
    owner="${gate_owners[$i]}"
    purpose="${gate_purposes[$i]}"
    printf '\n=== GATE %s (%s) ===\n' "$name" "$owner"
    printf 'purpose: %s\n' "$purpose"
    if ! is_selected "$name"; then
      printf '  [NOT_RUN] not selected by --case — excluded from this verdict\n'
      RESULT_NAMES+=("$name"); RESULT_OWNERS+=("$owner")
      RESULT_VALUES+=("NOT_RUN"); RESULT_DETAILS+=("not selected by --case")
      RESULT_SELECTED+=("no")
      continue
    fi
    execute_gate "$name"
    result="$EXECUTE_RESULT"
    detail="$EXECUTE_DETAIL"
    RESULT_NAMES+=("$name"); RESULT_OWNERS+=("$owner")
    RESULT_VALUES+=("$result"); RESULT_DETAILS+=("$detail")
    RESULT_SELECTED+=("yes")
    write_gate_evidence "$RUN_DIR" "$name" "$owner" "$purpose" "$result" "$detail" "$EXECUTE_TMP"
    if [[ -s "$EXECUTE_TMP/facts" ]]; then cat "$EXECUTE_TMP/facts" >> "$RUN_FACTS_FILE"; fi
    printf '  => %s (%s)\n' "$result" "$detail"
    rm -rf "$EXECUTE_TMP"
  done
}

verdict_for() {
  # verdict_for <comma-separated results> -> GREEN|RED|PARTIAL
  case ",$1," in
    *,FAIL,*|*,MISSING,*|*,NOT_IMPLEMENTED,*) printf 'RED' ;;
    *,SKIP,*) printf 'PARTIAL' ;;
    *) printf 'GREEN' ;;
  esac
}

print_summary() {
  local i selected_results=""
  printf '\n=== M6 live acceptance summary ===\n'
  printf '%-42s %-9s %-17s %s\n' "GATE" "OWNER" "RESULT" "DETAIL"
  for i in "${!RESULT_NAMES[@]}"; do
    printf '%-42s %-9s %-17s %s\n' "${RESULT_NAMES[$i]}" "${RESULT_OWNERS[$i]}" "${RESULT_VALUES[$i]}" "${RESULT_DETAILS[$i]}"
    if [[ "${RESULT_SELECTED[$i]}" == "yes" ]]; then
      selected_results+="${RESULT_VALUES[$i]},"
    fi
  done
  VERDICT="$(verdict_for "$selected_results")"

  {
    printf 'run_id=%s\n' "$RUN_ID"
    printf 'utc=%s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)"
    printf 'git_sha=%s\n' "$GIT_SHA"
    printf 'verdict=%s\n' "$VERDICT"
    printf 'scope=%s\n' "$([[ -z "$SELECTED_CASES" ]] && echo all-registered || echo "$SELECTED_CASES")"
    for i in "${!RESULT_NAMES[@]}"; do
      printf 'gate=%s owner=%s selected=%s result=%s detail=%s\n' \
        "${RESULT_NAMES[$i]}" "${RESULT_OWNERS[$i]}" "${RESULT_SELECTED[$i]}" "${RESULT_VALUES[$i]}" "${RESULT_DETAILS[$i]}"
    done
  } > "$RUN_DIR/summary.txt"

  {
    for i in "${!RESULT_NAMES[@]}"; do
      printf '%s=%s|%s\n' "${RESULT_NAMES[$i]}" "${RESULT_VALUES[$i]}" "${RESULT_OWNERS[$i]}"
    done
  } > "$RUN_DIR/manifest.txt"

  write_environment_facts > "$RUN_DIR/environment-facts.txt"

  printf '\nscope: %s\n' "$([[ -z "$SELECTED_CASES" ]] && echo "all registered gates" || echo "$SELECTED_CASES")"
  printf 'VERDICT: %s\n' "$VERDICT"
  printf 'evidence: %s\n' "$RUN_DIR"
  case "$VERDICT" in
    GREEN) printf 'RESULT: all selected gates PASS (exit 0)\n' ;;
    PARTIAL) printf 'RESULT: no failures, but at least one SKIP — skip is NOT a pass (exit 3)\n' ;;
    RED) printf 'RESULT: at least one selected gate failed or was not executable (exit 1)\n' ;;
  esac
}

write_environment_facts() {
  # Emits the fields docs/operations/evidence/phase4a-environment.md lists.
  # Measured-this-run values or an explicit PENDING — never an invented value.
  printf 'environment_record_template=docs/operations/evidence/phase4a-environment.md\n'
  printf 'run_id=%s\n' "$RUN_ID"
  printf 'utc=%s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  printf 'harness_git_sha=%s\n' "$GIT_SHA"
  printf 'harness_host_platform=%s_%s\n' "$uname_s" "$(uname -m 2>/dev/null || echo unknown)"
  printf 'control_host=%s\n' "${CONTROL_HOST:-PENDING (LIVE_PHASE4A_CONTROL_HOST unset)}"
  printf 'relay_host=%s\n' "${RELAY_HOST:-PENDING (LIVE_PHASE4A_RELAY_HOST unset)}"
  printf 'relay_public_ip=%s\n' "${RELAY_PUBLIC_IP:-PENDING (LIVE_PHASE4A_RELAY_PUBLIC_IP unset)}"
  printf 'relay_tunnel_host=%s\n' "${RELAY_TUNNEL_HOST:-PENDING (LIVE_PHASE4A_RELAY_TUNNEL_HOST unset)}"
  printf 'transport_port=%s\n' "$TRANSPORT_PORT"
  printf 'base_domain=%s\n' "$BASE_DOMAIN"
  printf 'namespace=%s\n' "${NAMESPACE:-PENDING (LIVE_PHASE4A_NAMESPACE unset)}"
  printf 'immich_url=%s\n' "${IMMICH_URL:-PENDING (LIVE_PHASE4A_IMMICH_URL unset)}"
  printf 'control_base_url=%s\n' "${CONTROL_BASE_URL:-PENDING (LIVE_PHASE4A_CONTROL_BASE_URL unset)}"
  printf 'allow_restart=%s\n' "$ALLOW_RESTART"
  printf 'recovery_bound_s=%s\n' "$RECOVERY_BOUND_S"
  printf 'hetzner_instance_id_control=%s\n' "$(fact_get hetzner_instance_id_control)"
  printf 'hetzner_instance_id_relay=%s\n' "$(fact_get hetzner_instance_id_relay)"
  printf 'hetzner_region_control=%s\n' "$(fact_get hetzner_region_control)"
  printf 'hetzner_region_relay=%s\n' "$(fact_get hetzner_region_relay)"
  printf 'private_ipv4_control=%s\n' "$(fact_get private_ipv4_control)"
  printf 'private_ipv4_relay=%s\n' "$(fact_get private_ipv4_relay)"
  printf 'nft_public_tcp_allowlist=%s\n' "$(fact_get nft_public_tcp_allowlist)"
  printf 'tunnel_dns_a=%s\n' "$(fact_get tunnel_dns_a)"
  printf 'tunnel_dns_aaaa=%s\n' "$(fact_get tunnel_dns_aaaa)"
  printf 'tunnel_dns_https=%s\n' "$(fact_get tunnel_dns_https)"
  printf 'tunnel_cert_verify=%s\n' "$(fact_get tunnel_cert_verify)"
  printf 'gateway_route_ready=%s\n' "$(fact_get gateway_route_ready)"
  printf 'gateway_frps_process_healthy=%s\n' "$(fact_get gateway_frps_process_healthy)"
  printf 'selection_flag_observed=%s\n' "$(fact_get selection_flag_observed)"
  printf 'restart_recovery_baseline_bytes=%s\n' "$(fact_get restart_recovery_baseline_bytes)"
  printf 'restart_recovery_offline_seconds=%s\n' "$(fact_get restart_recovery_offline_seconds)"
  printf 'restart_recovery_seconds=%s\n' "$(fact_get restart_recovery_seconds)"
}

# ---------------------------------------------------------------------------
# Shared probe helpers used by gate bodies
# ---------------------------------------------------------------------------

host_only() {
  local t="$1"
  t="${t##*@}"
  t="${t%%:*}"
  printf '%s' "$t"
}

url_host() {
  # url_host <url> -> host[:port] without the scheme/path
  local u="$1"
  u="${u#*://}"
  u="${u%%/*}"
  printf '%s' "$u"
}

metric_value() {
  # metric_value <metrics-text> <exact-metric-with-labels>
  printf '%s\n' "$1" | grep -F "$2" | tail -n1 | awk '{print $NF}'
}

gate_tmpdir() {
  # The runner's per-gate temp dir (removed by execute_gate); files written
  # here are cleaned up with the gate, so gates never leak captures in TMPDIR.
  printf '%s' "$(dirname "${GATE_CHECK_FILE:-${TMPDIR:-/tmp}/live-phase4a-gate}")"
}

fetch_gateway_metrics() {
  # fetch_gateway_metrics [max-time-seconds] ; sets GATEWAY_METRICS_TEXT (empty
  # on failure). A full URL wins; otherwise the loopback /metrics is read over
  # SSH from the relay VM. The optional max-time lets the bounded recovery loop
  # cap this stage by the time remaining before its monotonic deadline.
  GATEWAY_METRICS_TEXT=""
  local out st max_time="${1:-5}"
  [[ "$max_time" =~ ^[0-9]+$ && "$max_time" -ge 1 ]] || max_time=1
  if [[ -n "$GATEWAY_METRICS_URL" ]]; then
    record_cmd "curl ${GATEWAY_METRICS_URL} (gateway /metrics)"
    out="$(local_exec "curl -fsS --max-time ${max_time} '${GATEWAY_METRICS_URL}'")"; st=$?
    [[ "$st" -eq 0 ]] && GATEWAY_METRICS_TEXT="$out"
    return "$st"
  fi
  if [[ -n "$RELAY_HOST" ]]; then
    out="$(remote_exec "$RELAY_HOST" "curl -fsS --max-time ${max_time} 'http://${GATEWAY_METRICS_ADDR}/metrics'")"; st=$?
    [[ "$st" -eq 0 ]] && GATEWAY_METRICS_TEXT="$out"
    return "$st"
  fi
  return 1
}

gateway_tunnel_online() {
  # gateway_tunnel_online [max-time-seconds] ; echoes the online gauge value (or
  # empty when unobservable)
  fetch_gateway_metrics "${1:-5}" >/dev/null 2>&1 || return 1
  metric_value "$GATEWAY_METRICS_TEXT" 'sharebridge_relay_tunnel_state{state="online"}'
}

prepare_route() {
  # prepare_route <share-code> [max-time-seconds] ; sets PREPARE_HTTP_CODE /
  # PREPARE_STATUS / PREPARE_RELAY_URL / PREPARE_BODY_FILE. The body lives under
  # the gate temp dir so the runner's cleanup removes it. The optional max-time
  # lets the bounded recovery loop cap this stage by the remaining time.
  local code="$1" max_time="${2:-15}" url body_file
  [[ "$max_time" =~ ^[0-9]+$ && "$max_time" -ge 1 ]] || max_time=1
  url="${CONTROL_BASE_URL%/}/api/shares/${code}/prepare-route"
  body_file="$(gate_tmpdir)/prepare-route.json"
  record_cmd "curl -X POST ${url} (interstitial prepare-route)"
  PREPARE_HTTP_CODE="$(curl --silent --show-error --max-time "$max_time" -X POST ${CONTROL_TLS_ARGS[@]+"${CONTROL_TLS_ARGS[@]}"} -o "$body_file" -w '%{http_code}' "$url" 2>/dev/null)"
  PREPARE_BODY_FILE="$body_file"
  PREPARE_STATUS="$(json_str "$(cat "$body_file" 2>/dev/null)" status)"
  PREPARE_RELAY_URL="$(json_str "$(cat "$body_file" 2>/dev/null)" relay_url)"
  record_out "prepare-route http=${PREPARE_HTTP_CODE:-000} status=${PREPARE_STATUS:-<none>}"
}

# frpc_identity_cmd: prints the POSIX-sh snippet that emits one `PID:STARTTIME`
# line per matching frpc process on the agent host. It is built here so the live
# gate and the -f pre-filter share one definition, and an operator override
# (LIVE_PHASE4A_FRPC_IDENTITY_CMD) replaces it entirely for containerised agents.
frpc_identity_cmd() {
  if [[ -n "$FRPC_IDENTITY_CMD" ]]; then
    printf '%s' "$FRPC_IDENTITY_CMD"
    return 0
  fi
  # 'pgrep -f' is ONLY a candidate pre-filter: the harness's own shell/timeout/
  # pgrep wrappers inherit a command line that contains the pattern, so the
  # identity is then restricted to processes whose exact name (comm) is
  # FRPC_PID_NAME. STARTTIME prefers /proc/<pid>/stat field 22 (stable, no
  # restart collision) and falls back to `ps -o lstart=` on hosts without procfs.
  cat <<EOF
pids="\$(pgrep -f '$FRPC_PID_MATCH' 2>/dev/null)"
for p in \$pids; do
  c=""
  if [ -r "/proc/\$p/comm" ]; then c="\$(cat "/proc/\$p/comm" 2>/dev/null)"; else c="\$(ps -o comm= -p "\$p" 2>/dev/null)"; c="\${c##*/}"; fi
  [ "\$c" = '$FRPC_PID_NAME' ] || continue
  st=""
  if [ -r "/proc/\$p/stat" ]; then st="\$(sed 's/.*) //' "/proc/\$p/stat" 2>/dev/null | awk '{print \$20}')"; fi
  [ -n "\$st" ] || st="\$(ps -o lstart= -p "\$p" 2>/dev/null | tr -s ' ' '_')"
  [ -n "\$st" ] && printf '%s:%s\n' "\$p" "\$st"
done
EOF
}

# frpc_identities: newline-separated, sorted, unique `PID:STARTTIME` identities
# of the agent's frpc child. Empty means unobservable, never "no child".
frpc_identities() {
  local cmd out
  cmd="$(frpc_identity_cmd)"
  if [[ -n "$AGENT_SSH_HOST" ]]; then
    out="$(remote_exec "$AGENT_SSH_HOST" "$cmd" 2>/dev/null)"
  else
    out="$(local_exec "$cmd" 2>/dev/null)"
  fi
  printf '%s\n' "$out" | grep -E '^[0-9]+:[^[:space:]].*$' | sed -e 's/[[:space:]]*$//' | sort -u
}

# frpc_identity_status <identities-text> -> one | none | many. The identity is
# only usable when EXACTLY ONE frpc child exists; anything else fails closed.
frpc_identity_status() {
  local n
  n="$(printf '%s\n' "$1" | sed -e 's/^[[:space:]]*//' -e 's/[[:space:]]*$//' | grep -cE '^[0-9]+:[^[:space:]].*$' || true)"
  n="${n:-0}"
  case "$n" in
    1) printf 'one' ;;
    0) printf 'none' ;;
    *) printf 'many' ;;
  esac
}

# frps_journal_grep <fixed-pattern> ; empty pattern reads the whole journal.
# Uses LIVE_PHASE4A_FRPS_JOURNAL_FILE when supplied, else journalctl -u over SSH
# to the frps target.
frps_journal_grep() {
  local pat="$1" target="${FRPS_SSH_HOST:-$RELAY_HOST}"
  if [[ -n "$FRPS_JOURNAL_FILE" ]]; then
    [[ -r "$FRPS_JOURNAL_FILE" ]] || return 0
    if [[ -n "$pat" ]]; then grep -F "$pat" "$FRPS_JOURNAL_FILE" || true; else cat "$FRPS_JOURNAL_FILE"; fi
    return 0
  fi
  [[ -n "$target" ]] || return 1
  if [[ -n "$pat" ]]; then
    remote_exec "$target" "journalctl -u '$FRPS_UNIT' --no-pager -o short-iso -n ${JOURNAL_MAX_LINES} 2>/dev/null | grep -F '$pat' || true"
  else
    remote_exec "$target" "journalctl -u '$FRPS_UNIT' --no-pager -o short-iso -n ${JOURNAL_MAX_LINES} 2>/dev/null || true"
  fi
}

frps_journal_available() {
  [[ -n "$FRPS_JOURNAL_FILE" || -n "${FRPS_SSH_HOST:-$RELAY_HOST}" ]]
}

# counter_increased_verdict <before> <after> -> ok | unobservable | no-increase.
# One definition for BOTH the global reconnect counter and the agent-specific
# frps proxy-registration line count; the global counter is never the sole proof.
counter_increased_verdict() {
  local before="$1" after="$2"
  if [[ ! "$before" =~ ^[0-9]+$ || ! "$after" =~ ^[0-9]+$ ]]; then printf 'unobservable'; return 0; fi
  if [[ "$after" -gt "$before" ]]; then printf 'ok'; return 0; fi
  printf 'no-increase'
}

# bound_elapsed_verdict <elapsed-s> <bound-s> -> ok | over-bound | unobservable.
# IMPORTANT 5: the recovery verdict measures elapsed time AFTER every stage has
# completed and refuses a recovery that finished late, not merely one whose loop
# happened to start before the deadline.
bound_elapsed_verdict() {
  local elapsed="$1" bound="$2"
  if [[ ! "$elapsed" =~ ^[0-9]+$ || ! "$bound" =~ ^[0-9]+$ ]]; then printf 'unobservable'; return 0; fi
  if [[ "$elapsed" -le "$bound" ]]; then printf 'ok'; return 0; fi
  printf 'over-bound'
}

# deadline_remaining <deadline-epoch> -> seconds left (0 when the deadline has
# passed). Used to cap every stage of a bounded loop by the remaining time.
deadline_remaining() {
  local deadline="$1" now rem
  now="$(date +%s)"
  rem=$(( deadline - now ))
  if [[ "$rem" -lt 0 ]]; then rem=0; fi
  printf '%s' "$rem"
}

# frps_restart_target_verdict <confirm> <host> <unit> -> unset|target-unset|mismatch|ok.
# IMPORTANT 6: the destructive-target guard must cover the host + unit that will
# actually be restarted, not only the control URL. The operator must type the
# exact `<host>|<unit>` pair the harness intends to restart.
frps_restart_target_verdict() {
  local confirm="$1" host="$2" unit="$3"
  if [[ -z "$confirm" ]]; then printf 'unset'; return 0; fi
  if [[ -z "$host" || -z "$unit" ]]; then printf 'target-unset'; return 0; fi
  if [[ "$confirm" == "$host|$unit" ]]; then printf 'ok'; return 0; fi
  printf 'mismatch'
}

# restart_recovery_verdict <online> <pid-before> <pid-after> <reconn-before> <reconn-after> <content-ok 0|1> <agent-session-ok 0|1>
# Prints ok | offline | no-child | no-session | no-agent-session | no-content.
# A pure predicate so the live gate and --selftest share one definition of
# "automatically recovered". The global reconnect counter is a corroborating
# signal; the agent-specific frps proxy-registration proof is REQUIRED in
# addition, never replaced by it.
restart_recovery_verdict() {
  local online="$1" pbefore="$2" pafter="$3" rbefore="$4" rafter="$5" content="$6" agent_session="$7"
  if [[ ! "$online" =~ ^[0-9]+$ || "$online" -lt 1 ]]; then printf 'offline'; return 0; fi
  if [[ -z "$pafter" || "$pafter" == "$pbefore" ]]; then printf 'no-child'; return 0; fi
  if [[ ! "$rbefore" =~ ^[0-9]+$ || ! "$rafter" =~ ^[0-9]+$ || "$rafter" -le "$rbefore" ]]; then printf 'no-session'; return 0; fi
  if [[ "$agent_session" != "1" ]]; then printf 'no-agent-session'; return 0; fi
  if [[ "$content" != "1" ]]; then printf 'no-content'; return 0; fi
  printf 'ok'
}

# Record Hetzner metadata facts into FACT_* (used by the environment record).
probe_hetzner_facts() {
  local role="$1" host="$2" meta rid region az
  meta="$(remote_exec "$host" "curl -fsS --max-time 5 '${HETZNER_METADATA_URL}'")" || true
  rid="$(json_str "$meta" instance-id)"
  region="$(json_str "$meta" region)"
  az="$(json_str "$meta" availability-zone)"
  if [[ -z "$region" && -n "$az" ]]; then region="${az%%-dc*}"; fi
  if [[ "$role" == "control" ]]; then
    set_fact hetzner_instance_id_control "$rid"; set_fact hetzner_region_control "$region"
  else
    set_fact hetzner_instance_id_relay "$rid"; set_fact hetzner_region_relay "$region"
  fi
  printf '%s|%s|%s' "$rid" "$region" "$az"
}

# ===========================================================================
# GATE: gate_23_9_dark_topology — Task 37 (spec §20 steps 1–4, §23.9)
# ===========================================================================

# --- 1. Control VM reachable -------------------------------------------------
gate_chk_control_reachable() {
  local out st
  if ! need_cfg control_reachable LIVE_PHASE4A_CONTROL_HOST "$CONTROL_HOST" "ssh target of the control VM"; then
    return
  fi
  out="$(remote_exec "$CONTROL_HOST" "systemctl is-active $CONTROL_UNIT")"
  st=$?
  if [[ "$st" -eq 0 && "$(printf '%s' "$out" | tr -d '[:space:]')" == "active" ]]; then
    check control_reachable PASS "$CONTROL_HOST: $CONTROL_UNIT is active"
  else
    check control_reachable FAIL "$CONTROL_HOST: expected '$CONTROL_UNIT active', observed exit=${st} output='$(printf '%s' "$out" | tr -d '\n' | cut -c1-200)'"
  fi
}

# --- 2. New relay VM, same Hetzner region/private network, distinct box ------
gate_chk_relay_topology() {
  local relay_facts control_facts rid cid rregion cregion
  if ! need_cfg relay_vm_distinct LIVE_PHASE4A_RELAY_HOST "$RELAY_HOST" "ssh target of the new relay VM"; then
    return
  fi
  need_cfg relay_vm_distinct LIVE_PHASE4A_CONTROL_HOST "$CONTROL_HOST" "ssh target of the control VM (needed for the same-region comparison)" || return

  relay_facts="$(probe_hetzner_facts relay "$RELAY_HOST")"
  control_facts="$(probe_hetzner_facts control "$CONTROL_HOST")"
  rid="${relay_facts%%|*}"; relay_facts="${relay_facts#*|}"; rregion="${relay_facts%%|*}"
  cid="${control_facts%%|*}"; control_facts="${control_facts#*|}"; cregion="${control_facts%%|*}"

  if [[ -z "$rid" || -z "$cid" ]]; then
    check relay_vm_distinct FAIL "Hetzner metadata instance-id unavailable (relay='${rid:-<empty>}', control='${cid:-<empty>}') — cannot prove a NEW distinct VM in a Hetzner region"
  elif [[ "$rid" == "$cid" ]]; then
    check relay_vm_distinct FAIL "relay instance-id == control instance-id (${rid}) — the relay must be a NEW, distinct VM"
  else
    check relay_vm_distinct PASS "distinct instances: control=${cid} relay=${rid}"
  fi

  if [[ -z "$rregion" || -z "$cregion" ]]; then
    check relay_vm_same_region FAIL "Hetzner region unavailable (relay='${rregion:-<empty>}', control='${cregion:-<empty>}') — cannot prove same region"
  elif [[ "$rregion" == "$cregion" ]]; then
    check relay_vm_same_region PASS "both in region ${rregion}"
  else
    check relay_vm_same_region FAIL "relay region=${rregion} != control region=${cregion}"
  fi

  local rlist rpriv clist cpriv
  rlist="$(remote_exec "$RELAY_HOST" "ip -4 -o addr show scope global 2>/dev/null")" || true
  clist="$(remote_exec "$CONTROL_HOST" "ip -4 -o addr show scope global 2>/dev/null")" || true
  rpriv="$(private_v4_list "$rlist" | head -n1)"
  cpriv="$(private_v4_list "$clist" | head -n1)"
  set_fact private_ipv4_relay "$rpriv"; set_fact private_ipv4_control "$cpriv"
  if [[ -z "$rpriv" || -z "$cpriv" ]]; then
    check relay_vm_private_network FAIL "private IPv4 not found on both hosts (relay='${rpriv:-<none>}', control='${cpriv:-<none>}') — relay must share control's private network"
  elif same_subnet24 "$rpriv" "$cpriv"; then
    check relay_vm_private_network PASS "relay ${rpriv} and control ${cpriv} share a /24 private network"
  else
    check relay_vm_private_network FAIL "relay private ${rpriv} and control private ${cpriv} are not in the same /24"
  fi

  # The M6 relay must not be the collocated (and currently down) test VPS.
  local excluded hit
  hit=""
  IFS=',' read -r -a excluded <<< "$EXCLUDED_RELAY_IPS"
  for ip in "${excluded[@]}"; do
    [[ -z "$ip" ]] && continue
    if [[ -n "$RELAY_PUBLIC_IP" && "$RELAY_PUBLIC_IP" == "$ip" ]]; then hit="$ip"; fi
  done
  if [[ -n "$hit" ]]; then
    check relay_is_not_test_vps FAIL "relay public IP ${hit} is the excluded collocated test VPS — not the M6 dark relay VM"
  elif [[ -z "$RELAY_PUBLIC_IP" ]]; then
    check relay_is_not_test_vps FAIL "LIVE_PHASE4A_RELAY_PUBLIC_IP unset — cannot prove the relay is not the excluded test VPS"
  else
    check relay_is_not_test_vps PASS "relay public IP ${RELAY_PUBLIC_IP} is not in the excluded set (${EXCLUDED_RELAY_IPS})"
  fi
}

# --- 3. Home Mac agent + real Immich ----------------------------------------
gate_chk_home_side() {
  local pids out st
  pids="$(pgrep -f "$AGENT_PID_MATCH" 2>/dev/null | tr '\n' ' ')"
  if [[ -n "${pids// /}" ]]; then
    check home_agent_running PASS "process(es) matching '${AGENT_PID_MATCH}': ${pids}"
  else
    check home_agent_running FAIL "no local process matches '${AGENT_PID_MATCH}' on the harness host (the home Mac agent)"
  fi

  if ! need_cfg immich_reachable LIVE_PHASE4A_IMMICH_URL "$IMMICH_URL" "real Immich base URL on the home Mac"; then
    return
  fi
  out="$(local_exec "curl -fsS --max-time 5 '${IMMICH_URL%/}/api/server/ping'")"
  st=$?
  if [[ "$st" -eq 0 ]] && printf '%s' "$out" | grep -q '"res"[[:space:]]*:[[:space:]]*"pong"'; then
    check immich_reachable PASS "${IMMICH_URL%/}/api/server/ping -> pong"
  else
    check immich_reachable FAIL "Immich ping at ${IMMICH_URL%/}/api/server/ping failed (exit=${st}, output='$(printf '%s' "$out" | tr -d '\n' | cut -c1-160)')"
  fi

  # Informational: an established agent->control connection, when we can see it.
  local ctl_h
  ctl_h="${CONTROL_PUBLIC_HOST:-$(host_only "$CONTROL_HOST")}"
  if [[ -n "$ctl_h" ]] && command -v lsof >/dev/null 2>&1; then
    out="$(lsof -nP -iTCP -sTCP:ESTABLISHED 2>/dev/null | grep -F ":${CONTROL_PORT}" | grep -F "$ctl_h" | head -n3)"
    if [[ -n "$out" ]]; then
      note home_agent_control_connection "established connection(s) observed to ${ctl_h}:${CONTROL_PORT}"
    else
      note home_agent_control_connection "no established connection observed to ${ctl_h}:${CONTROL_PORT} (informational; the agent may be between reconnects)"
    fi
  else
    note home_agent_control_connection "skipped (lsof unavailable or control host unknown)"
  fi
}

# --- 4. Relay public firewall = 443/tcp + transport port only ----------------
gate_chk_relay_firewall() {
  local nft_out nft_st elements got want policy extra p
  if ! need_cfg relay_firewall_allowlist LIVE_PHASE4A_RELAY_HOST "$RELAY_HOST" "ssh target of the relay VM (needed to read the nftables ruleset)"; then
    return
  fi

  nft_out="$(remote_exec "$RELAY_HOST" "nft list table inet sharebridge_relay 2>/dev/null")"
  nft_st=$?
  elements="$(printf '%s' "$nft_out" | tr '\n' ' ' | grep -oE 'elements = \{[^}]*\}' | head -n1)"
  if [[ "$nft_st" -ne 0 || -z "$elements" ]]; then
    check relay_firewall_allowlist FAIL "could not read the inet sharebridge_relay public set on ${RELAY_HOST} (ssh/parse exit=${nft_st}; output='$(printf '%s' "$nft_out" | tr '\n' ' ' | cut -c1-160)')"
    check relay_firewall_input_policy FAIL "not evaluated: no parseable inet sharebridge_relay ruleset"
    check relay_firewall_no_extra_dport FAIL "not evaluated: no parseable inet sharebridge_relay ruleset"
  else
    got="$(printf '%s' "$elements" | tr -cs '0-9' ' ' | tr ' ' '\n' | grep -E '^[0-9]+$' | sort -nu | tr '\n' ' ' | sed 's/ *$//')"
    want="$(printf '%s\n%s\n' 443 "$TRANSPORT_PORT" | sort -nu | tr '\n' ' ' | sed 's/ *$//')"
    set_fact nft_public_tcp_allowlist "${got:-<none>}"
    if [[ "$got" == "$want" ]]; then
      check relay_firewall_allowlist PASS "public tcp allowlist = { ${got} } (expected 443 + transport ${TRANSPORT_PORT})"
    else
      check relay_firewall_allowlist FAIL "public tcp allowlist = { ${got:-<none>} } but expected exactly { ${want} }"
    fi

    policy="$(printf '%s' "$nft_out" | grep -o 'policy drop' | head -n1)"
    if [[ "$policy" == "policy drop" ]]; then
      check relay_firewall_input_policy PASS "input chain policy drop"
    else
      check relay_firewall_input_policy FAIL "input chain does not declare 'policy drop'"
    fi

    # Any dport number in the ruleset must be in the expected allowlist.
    extra="$(printf '%s' "$nft_out" | tr '\n' ' ' | grep -o 'dport [^ ]*' | tr -cs '0-9' ' ' | tr ' ' '\n' | grep -E '^[0-9]+$' | sort -nu | tr '\n' ' ' | sed 's/ *$//')"
    local bad=""
    for p in $extra; do
      case " 443 $TRANSPORT_PORT " in
        *" $p "*) : ;;
        *) bad="${bad}${p} " ;;
      esac
    done
    if [[ -z "$bad" ]]; then
      check relay_firewall_no_extra_dport PASS "every numeric dport in the ruleset is in { ${want} }"
    else
      check relay_firewall_no_extra_dport FAIL "unexpected accept dport(s): ${bad}"
    fi
  fi

  # External TCP surface probe from the harness host.
  if [[ -z "$RELAY_PUBLIC_IP" ]]; then
    check relay_public_443_open FAIL "LIVE_PHASE4A_RELAY_PUBLIC_IP unset — cannot probe the public surface"
    check relay_public_transport_open FAIL "LIVE_PHASE4A_RELAY_PUBLIC_IP unset — cannot probe the public surface"
    check relay_forbidden_ports_closed FAIL "LIVE_PHASE4A_RELAY_PUBLIC_IP unset — cannot probe the public surface"
  else
    if tcp_open "$RELAY_PUBLIC_IP" 443; then
      check relay_public_443_open PASS "${RELAY_PUBLIC_IP}:443 accepts TCP"
    else
      check relay_public_443_open FAIL "${RELAY_PUBLIC_IP}:443 did not accept TCP"
    fi
    if tcp_open "$RELAY_PUBLIC_IP" "$TRANSPORT_PORT"; then
      check relay_public_transport_open PASS "${RELAY_PUBLIC_IP}:${TRANSPORT_PORT} accepts TCP"
    else
      check relay_public_transport_open FAIL "${RELAY_PUBLIC_IP}:${TRANSPORT_PORT} did not accept TCP"
    fi
    local open_forbidden=""
    for p in 9001 9101 9102 7500 10000 10099; do
      if tcp_open "$RELAY_PUBLIC_IP" "$p"; then open_forbidden="${open_forbidden}${p} "; fi
    done
    if [[ -z "$open_forbidden" ]]; then
      check relay_forbidden_ports_closed PASS "no forbidden public port accepted TCP (checked 9001 9101 9102 7500 10000 10099)"
    else
      check relay_forbidden_ports_closed FAIL "forbidden public port(s) reachable: ${open_forbidden}"
    fi
  fi

  # No public UDP listener on the relay VM (spec §17.1). Any bound UDP socket
  # whose local address is not loopback is a public listener.
  if [[ -n "$RELAY_HOST" ]]; then
    local udp_all udp_st udp_public
    udp_all="$(remote_exec "$RELAY_HOST" "ss -lun 2>/dev/null | awk 'NR>1{print \$5}'")"
    udp_st=$?
    if [[ "$udp_st" -ne 0 ]]; then
      check relay_no_public_udp FAIL "could not read UDP listeners on ${RELAY_HOST} (ssh exit=${udp_st})"
    else
      udp_public="$(printf '%s\n' "$udp_all" | grep -vE '^(127\.|\[::1\]:|::1:)' | grep -vE '^[[:space:]]*$' | tr '\n' ' ' | sed 's/ *$//')"
      if [[ -z "$udp_public" ]]; then
        check relay_no_public_udp PASS "no non-loopback UDP listener on ${RELAY_HOST}"
      else
        check relay_no_public_udp FAIL "non-loopback UDP listener(s) on ${RELAY_HOST}: ${udp_public}"
      fi
    fi
  fi
}

# --- 5. Private mTLS control sync configured and healthy ---------------------
gate_chk_private_sync() {
  local ctl_env gw_env gw_unit bind url san ns
  if ! need_cfg sync_configured LIVE_PHASE4A_CONTROL_HOST "$CONTROL_HOST" "ssh target of the control VM"; then
    return
  fi

  ctl_env="$(remote_exec "$CONTROL_HOST" "cat '$CONTROL_ENV_FILE' 2>/dev/null")" || true
  bind="$(env_value "$ctl_env" CONTROL_SYNC_BIND_ADDR)"
  local missing=""
  for v in CONTROL_SYNC_CERT_FILE CONTROL_SYNC_KEY_FILE CONTROL_SYNC_CLIENT_CA_FILE CONTROL_SYNC_EXPECTED_CLIENT_SAN; do
    [[ -n "$(env_value "$ctl_env" "$v")" ]] || missing="${missing}${v} "
  done
  if [[ -n "$missing" ]]; then
    check sync_control_configured FAIL "${CONTROL_ENV_FILE} on ${CONTROL_HOST} is missing: ${missing}"
  elif [[ -n "$bind" ]] && ! is_private_bind "$bind"; then
    check sync_control_configured FAIL "CONTROL_SYNC_BIND_ADDR=${bind} is not a private/loopback bind"
  else
    check sync_control_configured PASS "CONTROL_SYNC_* material set${bind:+ ; bind=${bind}} (private)"
  fi

  if ! need_cfg sync_gateway_configured LIVE_PHASE4A_RELAY_HOST "$RELAY_HOST" "ssh target of the relay VM"; then
    return
  fi
  gw_env="$(remote_exec "$RELAY_HOST" "cat '$GATEWAY_ENV_FILE' 2>/dev/null")" || true
  gw_unit="$(remote_exec "$RELAY_HOST" "systemctl cat $GATEWAY_UNIT 2>/dev/null")" || true
  url="$(env_value "$gw_env" SHAREBRIDGE_CONTROL_SYNC_URL)"
  san="$(env_value "$gw_env" SHAREBRIDGE_CONTROL_SYNC_SAN)"
  ns="$(env_value "$gw_env" SHAREBRIDGE_GATEWAY_NAMESPACE)"
  local gw_missing=""
  [[ -n "$url" ]] || gw_missing="${gw_missing}SHAREBRIDGE_CONTROL_SYNC_URL "
  [[ -n "$san" ]] || gw_missing="${gw_missing}SHAREBRIDGE_CONTROL_SYNC_SAN "
  [[ -n "$(env_value "$gw_env" SHAREBRIDGE_CONTROL_SYNC_CA_FILE)" ]] || gw_missing="${gw_missing}SHAREBRIDGE_CONTROL_SYNC_CA_FILE "
  if [[ -n "$gw_missing" ]]; then
    check sync_gateway_configured FAIL "${GATEWAY_ENV_FILE} on ${RELAY_HOST} is missing: ${gw_missing}"
  else
    local creds=""
    printf '%s' "$gw_env" "$gw_unit" | grep -q 'SHAREBRIDGE_GATEWAY_SYNC_CERT_FILE' || creds="${creds}SHAREBRIDGE_GATEWAY_SYNC_CERT_FILE "
    printf '%s' "$gw_env" "$gw_unit" | grep -q 'SHAREBRIDGE_GATEWAY_SYNC_KEY_FILE' || creds="${creds}SHAREBRIDGE_GATEWAY_SYNC_KEY_FILE "
    printf '%s' "$gw_unit" | grep -q 'LoadCredential=' || creds="${creds}LoadCredential= "
    if [[ -n "$creds" ]]; then
      check sync_gateway_configured FAIL "gateway sync material incomplete on ${RELAY_HOST}: ${creds}"
    elif [[ -n "$ns" ]] && ! printf '%s' "$ns" | grep -Eq '^sb[0-9a-f]{8}$'; then
      check sync_gateway_configured FAIL "SHAREBRIDGE_GATEWAY_NAMESPACE=${ns} is not the §6 form sbXXXXXXXX"
    else
      check sync_gateway_configured PASS "gateway sync client material configured${ns:+ ; namespace=${ns}}"
    fi
  fi

  # Live mTLS handshake from the relay VM. The paths are ON the relay VM.
  local missing_paths=""
  [[ -n "$SYNC_CA_FILE" ]] || missing_paths="${missing_paths}LIVE_PHASE4A_SYNC_CA_FILE "
  [[ -n "$GATEWAY_SYNC_CERT" ]] || missing_paths="${missing_paths}LIVE_PHASE4A_GATEWAY_SYNC_CERT "
  [[ -n "$GATEWAY_SYNC_KEY" ]] || missing_paths="${missing_paths}LIVE_PHASE4A_GATEWAY_SYNC_KEY "
  if [[ -n "$missing_paths" ]] || [[ -z "$url" || -z "$san" ]]; then
    check sync_mtls_handshake FAIL "cannot run the live mTLS probe: ${missing_paths:-<sync URL/SAN missing from the gateway env>} (paths are on the relay VM)"
    return
  fi
  case "$url" in
    https://*) : ;;
    *) check sync_mtls_handshake FAIL "SHAREBRIDGE_CONTROL_SYNC_URL='${url}' is not an https:// URL"; return ;;
  esac
  local endpoint="${url#https://}"
  endpoint="${endpoint%%/*}"
  local out st
  out="$(remote_exec "$RELAY_HOST" "openssl s_client -connect '${endpoint}' -servername '${san}' -verify_hostname '${san}' -verify_return_error -CAfile '${SYNC_CA_FILE}' -cert '${GATEWAY_SYNC_CERT}' -key '${GATEWAY_SYNC_KEY}' -brief </dev/null")"
  st=$?
  if [[ "$st" -eq 0 ]] && printf '%s' "$out" | grep -q 'Verification: OK'; then
    check sync_mtls_handshake PASS "authenticated mTLS handshake to ${endpoint} (server SAN ${san}) succeeded"
  else
    check sync_mtls_handshake FAIL "authenticated mTLS handshake to ${endpoint} failed (exit=${st}): $(printf '%s' "$out" | tr '\n' ' ' | cut -c1-200)"
  fi
  out="$(remote_exec "$RELAY_HOST" "openssl s_client -connect '${endpoint}' -servername '${san}' -verify_return_error -CAfile '${SYNC_CA_FILE}' -brief </dev/null")"
  st=$?
  if [[ "$st" -ne 0 ]]; then
    check sync_mtls_enforced PASS "a client without a certificate was rejected (RequireAndVerifyClientCert)"
  else
    check sync_mtls_enforced FAIL "the sync listener accepted a client with no certificate — mTLS is not enforced"
  fi
}

# --- 6. Tunnel DNS + transport certificate validation ------------------------
gate_chk_tunnel_dns_cert() {
  if ! need_cfg tunnel_dns LIVE_PHASE4A_RELAY_TUNNEL_HOST "$RELAY_TUNNEL_HOST" "<relay-tunnel-host> for DNS and transport-certificate validation"; then
    return
  fi
  if ! command -v dig >/dev/null 2>&1; then
    check tunnel_dns FAIL "dig is not installed on the harness host — cannot validate relay DNS"
    return
  fi

  local a aaaa https svcb
  a="$(local_exec "dig +short A '${RELAY_TUNNEL_HOST}'" | tr '\n' ' ' | sed 's/ *$//')"
  aaaa="$(local_exec "dig +short AAAA '${RELAY_TUNNEL_HOST}'" | tr '\n' ' ' | sed 's/ *$//')"
  https="$(local_exec "dig +short HTTPS '${RELAY_TUNNEL_HOST}'" | tr '\n' ' ' | sed 's/ *$//')"
  svcb="$(local_exec "dig +short SVCB '${RELAY_TUNNEL_HOST}'" | tr '\n' ' ' | sed 's/ *$//')"
  set_fact tunnel_dns_a "${a:-<empty>}"; set_fact tunnel_dns_aaaa "${aaaa:-<empty>}"; set_fact tunnel_dns_https "${https:-<empty>}"

  if [[ -z "$a" ]]; then
    check tunnel_dns_a FAIL "A ${RELAY_TUNNEL_HOST} -> empty"
  elif [[ -n "$RELAY_PUBLIC_IP" ]] && ! printf '%s' "$a" | grep -qF "$RELAY_PUBLIC_IP"; then
    check tunnel_dns_a FAIL "A ${RELAY_TUNNEL_HOST} -> ${a} does not include the relay public IP ${RELAY_PUBLIC_IP}"
  else
    check tunnel_dns_a PASS "A ${RELAY_TUNNEL_HOST} -> ${a}"
  fi

  if [[ -z "$aaaa" ]]; then
    check tunnel_dns_aaaa PASS "AAAA ${RELAY_TUNNEL_HOST} -> empty (DNS-only A, acceptable)"
  elif printf '%s' "$aaaa" | grep -Eq '^[0-9A-Fa-f:]+$'; then
    check tunnel_dns_aaaa PASS "AAAA ${RELAY_TUNNEL_HOST} -> ${aaaa} (valid IPv6 literal)"
  else
    check tunnel_dns_aaaa FAIL "AAAA ${RELAY_TUNNEL_HOST} -> '${aaaa}' is not a valid IPv6 literal"
  fi

  if [[ -n "$https" || -n "$svcb" ]]; then
    check tunnel_dns_no_ech FAIL "HTTPS/SVCB present for ${RELAY_TUNNEL_HOST} (HTTPS='${https}' SVCB='${svcb}') — an ECH key would hide the SNI"
  elif printf '%s %s' "$https" "$svcb" | grep -qi 'ech='; then
    check tunnel_dns_no_ech FAIL "an ech= parameter is present for ${RELAY_TUNNEL_HOST}"
  else
    check tunnel_dns_no_ech PASS "no HTTPS/SVCB record and no ech= parameter for ${RELAY_TUNNEL_HOST}"
  fi

  if [[ -n "$NAMESPACE" ]]; then
    local child out
    child="probe-$(date +%s)-$$.relay.${NAMESPACE}.${BASE_DOMAIN}"
    out="$(local_exec "dig +short A '${child}'" | tr '\n' ' ' | sed 's/ *$//')"
    if [[ -z "$out" ]]; then
      check tunnel_dns_wildcard_synthesis FAIL "random child ${child} -> empty (wildcard does not synthesize)"
    elif [[ -n "$RELAY_PUBLIC_IP" ]] && ! printf '%s' "$out" | grep -qF "$RELAY_PUBLIC_IP"; then
      check tunnel_dns_wildcard_synthesis FAIL "random child ${child} -> ${out}, expected ${RELAY_PUBLIC_IP}"
    else
      check tunnel_dns_wildcard_synthesis PASS "random child ${child} -> ${out}"
    fi
  else
    check tunnel_dns_wildcard_synthesis FAIL "LIVE_PHASE4A_NAMESPACE unset — cannot prove wildcard synthesis for the test namespace"
  fi

  # Transport certificate: dedicated cert, verifies against the transport CA,
  # SAN matches the tunnel host, over the pinned transport port.
  if [[ -z "$TRANSPORT_CA_FILE" ]]; then
    check tunnel_transport_cert FAIL "LIVE_PHASE4A_TRANSPORT_CA_FILE unset — cannot validate the dedicated transport certificate"
    return
  fi
  local out st
  out="$(local_exec "openssl s_client -connect '${RELAY_TUNNEL_HOST}:${TRANSPORT_PORT}' -servername '${RELAY_TUNNEL_HOST}' -verify_hostname '${RELAY_TUNNEL_HOST}' -verify_return_error -CAfile '${TRANSPORT_CA_FILE}' -brief </dev/null")"
  st=$?
  if [[ "$st" -eq 0 ]] && printf '%s' "$out" | grep -q 'Verification: OK'; then
    check tunnel_transport_cert PASS "transport certificate at ${RELAY_TUNNEL_HOST}:${TRANSPORT_PORT} verifies against the configured CA with SAN match"
    set_fact tunnel_cert_verify "OK"
  else
    check tunnel_transport_cert FAIL "transport certificate validation failed at ${RELAY_TUNNEL_HOST}:${TRANSPORT_PORT} (exit=${st}): $(printf '%s' "$out" | tr '\n' ' ' | cut -c1-200)"
    set_fact tunnel_cert_verify "FAILED (exit=${st})"
  fi
}

# --- 7. Snapshot health (route_ready) ---------------------------------------
gate_chk_snapshot_health() {
  if ! need_cfg snapshot_route_ready LIVE_PHASE4A_RELAY_HOST "$RELAY_HOST" "ssh target of the relay VM (health endpoint is loopback-private)"; then
    return
  fi
  local out st rr rh
  out="$(remote_exec "$RELAY_HOST" "curl -fsS --max-time 5 '${HEALTHZ_URL}'")"
  st=$?
  rr="$(printf '%s' "$out" | grep -o '"route_ready":[a-z]*' | head -n1 | cut -d: -f2)"
  rh="$(printf '%s' "$out" | grep -o '"frps_process_healthy":[a-z]*' | head -n1 | cut -d: -f2)"
  set_fact gateway_route_ready "${rr:-PENDING (not observed)}"
  set_fact gateway_frps_process_healthy "${rh:-PENDING (not observed)}"

  if [[ "$st" -ne 0 ]]; then
    check snapshot_route_ready FAIL "GET ${HEALTHZ_URL} on ${RELAY_HOST} failed (exit=${st}) — route snapshot health unobservable"
  elif [[ "$rr" == "true" ]]; then
    check snapshot_route_ready PASS "route_ready=true (full route snapshot applied AND control sync live)"
  else
    check snapshot_route_ready FAIL "route_ready=${rr:-<absent>} (raw: $(printf '%s' "$out" | tr -d '\n' | cut -c1-200))"
  fi

  if [[ "$st" -ne 0 ]]; then
    check frps_process_healthy FAIL "frps process health unobservable (health endpoint failed)"
  elif [[ "$rh" == "true" ]]; then
    check frps_process_healthy PASS "frps_process_healthy=true (independent of route_ready)"
  else
    check frps_process_healthy FAIL "frps_process_healthy=${rh:-<absent>}"
  fi
}

# --- 8. Restart ordering -----------------------------------------------------
gate_chk_restart_ordering() {
  if ! need_cfg restart_order_frps_after_gateway LIVE_PHASE4A_RELAY_HOST "$RELAY_HOST" "ssh target of the relay VM (unit dependency metadata)"; then
    return
  fi
  local show frps_after frps_wants frps_bindsto frps_partof gw_after
  show="$(remote_exec "$RELAY_HOST" "for p in After Wants BindsTo PartOf; do printf '%s=%s\n' \"\$p\" \"\$(systemctl show -p \$p --value $FRPS_UNIT)\"; done; printf 'GATEWAY_AFTER=%s\n' \"\$(systemctl show -p After --value $GATEWAY_UNIT)\"; printf 'SHOW_OK\n'")"
  if ! printf '%s' "$show" | grep -q '^SHOW_OK$'; then
    check restart_order_frps_after_gateway FAIL "could not read $FRPS_UNIT dependency metadata on ${RELAY_HOST}"
    check restart_order_frps_wants_gateway FAIL "could not read $FRPS_UNIT dependency metadata on ${RELAY_HOST}"
    check restart_order_no_coupling FAIL "could not read $FRPS_UNIT dependency metadata on ${RELAY_HOST}"
    check restart_order_not_reversed FAIL "could not read $GATEWAY_UNIT dependency metadata on ${RELAY_HOST}"
    return
  fi
  frps_after="$(printf '%s' "$show" | sed -n 's/^After=//p' | head -n1)"
  frps_wants="$(printf '%s' "$show" | sed -n 's/^Wants=//p' | head -n1)"
  frps_bindsto="$(printf '%s' "$show" | sed -n 's/^BindsTo=//p' | head -n1)"
  frps_partof="$(printf '%s' "$show" | sed -n 's/^PartOf=//p' | head -n1)"
  gw_after="$(printf '%s' "$show" | sed -n 's/^GATEWAY_AFTER=//p' | head -n1)"

  case " $frps_after " in
    *" $GATEWAY_UNIT "*) check restart_order_frps_after_gateway PASS "$FRPS_UNIT After= includes $GATEWAY_UNIT" ;;
    *) check restart_order_frps_after_gateway FAIL "$FRPS_UNIT After='${frps_after:-<empty>}' does not include $GATEWAY_UNIT" ;;
  esac
  case " $frps_wants " in
    *" $GATEWAY_UNIT "*) check restart_order_frps_wants_gateway PASS "$FRPS_UNIT Wants= includes $GATEWAY_UNIT" ;;
    *) check restart_order_frps_wants_gateway FAIL "$FRPS_UNIT Wants='${frps_wants:-<empty>}' does not include $GATEWAY_UNIT" ;;
  esac
  if [[ -z "${frps_bindsto// /}" && -z "${frps_partof// /}" ]]; then
    check restart_order_no_coupling PASS "$FRPS_UNIT has no BindsTo=/PartOf= coupling to the gateway"
  else
    check restart_order_no_coupling FAIL "$FRPS_UNIT couples to the gateway (BindsTo='${frps_bindsto}' PartOf='${frps_partof}')"
  fi
  case " $gw_after " in
    *" $FRPS_UNIT "*) check restart_order_not_reversed FAIL "$GATEWAY_UNIT After= includes $FRPS_UNIT (ordering is reversed)" ;;
    *) check restart_order_not_reversed PASS "$GATEWAY_UNIT After= does not include $FRPS_UNIT" ;;
  esac

  if [[ "$ALLOW_RESTART" == "1" ]]; then
    local drill
    drill="$(remote_exec "$RELAY_HOST" "systemctl restart $GATEWAY_UNIT && sleep 2 && systemctl is-active $GATEWAY_UNIT $FRPS_UNIT")"
    if printf '%s' "$drill" | grep -q 'active'; then
      check restart_order_live_drill PASS "gateway-only restart left both units active: $(printf '%s' "$drill" | tr '\n' ' ')"
    else
      check restart_order_live_drill FAIL "gateway-only restart did not leave both units active: $(printf '%s' "$drill" | tr '\n' ' ')"
    fi
  else
    note restart_order_live_drill "LIVE_PHASE4A_ALLOW_RESTART!=1 — live restart drill not run (unit metadata is the gate contract; Task 43 owns the live recovery drill)"
  fi
}

# --- 9. Dark posture: the selection flag remains false ----------------------
# The flag must be OBSERVED in the deployed control configuration: the env file
# the unit loads, an explicit unit Environment=, or the running control
# process's own environment (authoritative). An absent flag FAILS — the code
# default is not evidence of the deployed state, and the dark posture is never
# assumed.
gate_chk_selection_flag() {
  if ! need_cfg selection_flag_remains_false LIVE_PHASE4A_CONTROL_HOST "$CONTROL_HOST" "ssh target of the control VM (the flag lives in the control deployment)"; then
    return
  fi
  local probe procenv unitenv envfile value source
  probe="$(remote_exec "$CONTROL_HOST" "if [ -f '$CONTROL_ENV_FILE' ]; then echo ENV_FILE_PRESENT; grep -E '^RELAY_SELECTION_ENABLED=' '$CONTROL_ENV_FILE' 2>/dev/null | tail -n1 | sed 's/^/ENVFILE:/'; else echo ENV_FILE_MISSING; fi
systemctl show -p Environment --value $CONTROL_UNIT 2>/dev/null | tr ' ' '\n' | grep -E '^RELAY_SELECTION_ENABLED=' | tail -n1 | sed 's/^/UNITENV:/'
pid=\$(systemctl show -p MainPID --value $CONTROL_UNIT 2>/dev/null)
if [ -n \"\$pid\" ] && [ \"\$pid\" != 0 ] && [ -r \"/proc/\$pid/environ\" ]; then tr '\0' '\n' < \"/proc/\$pid/environ\" | grep -E '^RELAY_SELECTION_ENABLED=' | tail -n1 | sed 's/^/PROCENV:/'; echo PROCENV_READABLE; else echo PROCENV_UNREADABLE; fi
echo PROBE_OK")"
  if printf '%s' "$probe" | grep -q '^ENV_FILE_MISSING'; then
    set_fact selection_flag_observed "unverifiable (env file missing)"
    check selection_flag_remains_false FAIL "${CONTROL_ENV_FILE} not found on ${CONTROL_HOST} — cannot verify the dark posture (never assume it)"
    return
  fi
  if ! printf '%s' "$probe" | grep -q '^ENV_FILE_PRESENT' || ! printf '%s' "$probe" | grep -q '^PROBE_OK'; then
    set_fact selection_flag_observed "unverifiable (ssh/read failure)"
    check selection_flag_remains_false FAIL "could not read the deployed control configuration on ${CONTROL_HOST} — cannot verify the dark posture"
    return
  fi

  # Precedence: what the running process actually uses > the unit's explicit
  # Environment= > the env file the unit loads. Every source is observed.
  procenv="$(printf '%s\n' "$probe" | sed -n 's/^PROCENV://p' | tail -n1)"
  unitenv="$(printf '%s\n' "$probe" | sed -n 's/^UNITENV://p' | tail -n1)"
  envfile="$(printf '%s\n' "$probe" | sed -n 's/^ENVFILE://p' | tail -n1)"
  value=""; source=""
  if [[ -n "$procenv" ]]; then
    value="$(env_value "$procenv" RELAY_SELECTION_ENABLED)"; source="the running ${CONTROL_UNIT} process environment"
  elif [[ -n "$unitenv" ]]; then
    value="$(env_value "$unitenv" RELAY_SELECTION_ENABLED)"; source="the ${CONTROL_UNIT} unit Environment="
  elif [[ -n "$envfile" ]]; then
    value="$(env_value "$envfile" RELAY_SELECTION_ENABLED)"; source="the deployed env file ${CONTROL_ENV_FILE}"
  fi

  if [[ -z "$source" ]]; then
    set_fact selection_flag_observed "not observed"
    check selection_flag_remains_false FAIL "RELAY_SELECTION_ENABLED was not observed in the deployed control configuration (env file ${CONTROL_ENV_FILE}, ${CONTROL_UNIT} unit Environment=, or the running process environment on ${CONTROL_HOST}) — cannot verify the dark posture (never assume the code default)"
    return
  fi
  set_fact selection_flag_observed "${value:-<empty>} (${source})"
  case "$value" in
    false) check selection_flag_remains_false PASS "RELAY_SELECTION_ENABLED=false observed in ${source} (dark posture held)" ;;
    "")    check selection_flag_remains_false FAIL "RELAY_SELECTION_ENABLED is set but empty in ${source} — cannot verify the dark posture (never assume it)" ;;
    true)  check selection_flag_remains_false FAIL "RELAY_SELECTION_ENABLED=true observed in ${source} — production selection is ON; the dark posture is violated and a missing topology would be masked" ;;
    *)     check selection_flag_remains_false FAIL "RELAY_SELECTION_ENABLED='${value}' observed in ${source} is not a boolean — cannot verify the dark posture" ;;
  esac
}

gate_23_9_dark_topology() {
  gate_chk_control_reachable
  gate_chk_relay_topology
  gate_chk_home_side
  gate_chk_relay_firewall
  gate_chk_private_sync
  gate_chk_tunnel_dns_cert
  gate_chk_snapshot_health
  gate_chk_restart_ordering
  gate_chk_selection_flag
}

# ===========================================================================
# Placeholders for Tasks 38–44. Each later task replaces exactly one body.
# ===========================================================================

acceptance_01_owner_hairpin()        { gate_not_implemented "Task 38" "§19 #1 owner hairpin on the confirmed non-hairpin router, three browsers, exact relay origin, zero CSP violations"; }
acceptance_04_interstitial_blackhole() { gate_not_implemented "Task 38" "§19 #4 deterministic interstitial fallback: blackhole direct after successful preparation, prove relay navigation within the documented bound and same content"; }
acceptance_02_cellular_relay_video() { gate_not_implemented "Task 39" "§19 #2 phone on cellular loads gallery + video over relay with at least two valid 206 seeks"; }
acceptance_11_content_parity()       { gate_not_implemented "Task 39" "§19 #11 Phase 3 gallery/items/thumb/preview/original/archive/accounting/Range parity through relay vs the direct baseline"; }
acceptance_03_relay_only()           { gate_not_implemented "Task 40" "§19 #3 relayOnly share: zero direct DNS mutation/lookup, probe, open signal, mapper call or browser direct request; exact relay URL; content succeeds"; }
l4_no_plaintext_capture()            { gate_not_implemented "Task 41" "§19 #5 / §18.4 canary capture proving the relay sees only TLS ciphertext (scripts/l4-canary-capture.sh), any plaintext is NO-GO"; }
acceptance_06_no_mapper_enrollment() { gate_not_implemented "Task 42" "§19 #6 no-mapper agent enrolls, registers a public share, serves it solely over the outbound tunnel"; }
acceptance_12_stun_mismatch()        { gate_not_implemented "Task 42" "§19 #12 egress mismatch marks the direct diagnostic relay_fallback, suppresses the public probe, routes through relay"; }
acceptance_13_stun_cadence_cold_budget() { gate_not_implemented "Task 42" "§19 #13 immediate post-reconnect challenge, ~4-minute refresh, no warm repeat, cold preparation within four seconds or relay fallback, later warm direct succeeds"; }
acceptance_07_no_relay_open_signal() { gate_not_implemented "Task 43" "§19 #7 route=relay generates no open_signal/open_ack/direct probe/port-mapper call"; }
acceptance_08_exact_routing()        { gate_not_implemented "Task 43" "§19 #8 random/bare/tombstoned SNI never reaches any agent; a valid exact route reaches only its owning agent"; }
# ===========================================================================
# GATE: acceptance_09_restart_recovery — Task 43 (spec §19 #9)
#
# The release-blocking defect this gate exists to prevent: an frps restart
# drops the frpc session, but frp v0.71 never exits the rejected child (its
# reconnect path hard-codes loginFailExit=false), so the agent retries a
# single-use, now-burned credential forever and the relay stays down until an
# operator locks down and unlocks. The corrected fix keys recovery on the real
# frpc client rejection line and replaces the still-running child with a fresh
# credential. This gate restarts the pinned frps unit while leaving the agent's
# frpc child ALIVE and then requires, WITHOUT any operator action, a NEW child,
# a NEW tunnel session, gateway `online=1` and serving relay content within the
# documented bound. `LIVE_PHASE4A_ALLOW_RESTART=1` is the operator opt-in for
# the state-changing restart (the same guarded flag as the ordering drill);
# without it the gate fails closed rather than skipping, because
# acceptance_09 is a required M6 gate and cannot be measured without the drop.
# ===========================================================================

acceptance_09_restart_recovery() {
  # (0) Operator opt-in and required inputs.
  if [[ "$ALLOW_RESTART" != "1" ]]; then
    check restart_recovery_opt_in FAIL "LIVE_PHASE4A_ALLOW_RESTART!=1 — acceptance_09 restarts the pinned frps unit to force the exact dropped-session scenario; set LIVE_PHASE4A_ALLOW_RESTART=1 to opt in (the harness never restarts the agent's frpc child)"
    return 0
  fi
  check restart_recovery_opt_in PASS "LIVE_PHASE4A_ALLOW_RESTART=1 — operator opted in to the frps restart (the agent's frpc child is never restarted by the harness)"

  local frps_target="${FRPS_SSH_HOST:-$RELAY_HOST}"
  local proxy_name="${AGENT_PROXY_NAME:-}"
  if [[ -z "$proxy_name" && -n "$NAMESPACE" ]]; then proxy_name="sb-${NAMESPACE}"; fi
  local cfg_ok=1
  need_cfg restart_recovery_control LIVE_PHASE4A_CONTROL_BASE_URL "$CONTROL_BASE_URL" "control interstitial base URL for prepare-route" || cfg_ok=0
  need_cfg restart_recovery_share LIVE_PHASE4A_SHARE_CODE "$SHARE_CODE" "share code for the baseline and post-recovery relay fetch" || cfg_ok=0
  need_cfg restart_recovery_frps_target LIVE_PHASE4A_FRPS_SSH_HOST "$frps_target" "ssh target hosting the pinned frps unit (LIVE_PHASE4A_FRPS_SSH_HOST unset and LIVE_PHASE4A_RELAY_HOST unset)" || cfg_ok=0
  need_cfg restart_recovery_proxy LIVE_PHASE4A_AGENT_PROXY_NAME "$proxy_name" "frps proxy name for the agent under test (set LIVE_PHASE4A_AGENT_PROXY_NAME, or LIVE_PHASE4A_NAMESPACE to derive sb-<namespace>) — required for the agent-specific fresh-session proof" || cfg_ok=0
  if ! frps_journal_available; then
    check restart_recovery_frps_journal FAIL "no frps journal source: set LIVE_PHASE4A_FRPS_JOURNAL_FILE, or LIVE_PHASE4A_FRPS_SSH_HOST / LIVE_PHASE4A_RELAY_HOST for `journalctl -u ${FRPS_UNIT}` — the agent-specific post-restart session proof is unobservable without it"
    cfg_ok=0
  fi
  # IMPORTANT 6: the frps SSH host + systemd unit that will be restarted each
  # need a SEPARATE, exact operator confirmation. A control-URL confirmation
  # alone does not cover the destructive target.
  local target_verdict
  target_verdict="$(frps_restart_target_verdict "$FRPS_RESTART_CONFIRM" "$frps_target" "$FRPS_UNIT")"
  case "$target_verdict" in
    ok)
      check restart_recovery_target_confirmed PASS "operator confirmed the exact restart target '${frps_target}|${FRPS_UNIT}' (LIVE_PHASE4A_FRPS_RESTART_CONFIRM)" ;;
    unset)
      check restart_recovery_target_confirmed FAIL "LIVE_PHASE4A_FRPS_RESTART_CONFIRM is unset — refusing to restart frps without an explicit assertion of the SSH host + systemd unit; set it to the EXACT '${frps_target}|${FRPS_UNIT}'"
      cfg_ok=0 ;;
    target-unset)
      check restart_recovery_target_confirmed FAIL "LIVE_PHASE4A_FRPS_RESTART_CONFIRM is set but the restart target is unset (host='${frps_target}' unit='${FRPS_UNIT}') — cannot verify the confirmation"
      cfg_ok=0 ;;
    *)
      check restart_recovery_target_confirmed FAIL "LIVE_PHASE4A_FRPS_RESTART_CONFIRM='${FRPS_RESTART_CONFIRM}' does not equal the intended restart target '${frps_target}|${FRPS_UNIT}' — refusing the state-changing restart"
      cfg_ok=0 ;;
  esac
  set_fact restart_recovery_frps_restart_target "${frps_target}|${FRPS_UNIT}"
  set_fact restart_recovery_agent_proxy_name "${proxy_name:-<unset>}"
  if ! fetch_gateway_metrics >/dev/null 2>&1; then
    check restart_recovery_metrics FAIL "gateway /metrics unobservable — set LIVE_PHASE4A_GATEWAY_METRICS_URL, or LIVE_PHASE4A_RELAY_HOST for the loopback addr ${GATEWAY_METRICS_ADDR}"
    cfg_ok=0
  fi
  [[ "$cfg_ok" == "1" ]] || return 0

  # (1) Baseline: tunnel online AND the configured share actually serves over
  # the relay. A broken baseline cannot prove a recovery, so it refuses the
  # state change outright.
  local online reconnects_before frpc_before_raw frpc_before_status frpc_pid_before
  local proxy_lines_before="" proxy_pat="new proxy [${proxy_name}] type [tcp] success"
  online="$(metric_value "$GATEWAY_METRICS_TEXT" 'sharebridge_relay_tunnel_state{state="online"}')"
  reconnects_before="$(metric_value "$GATEWAY_METRICS_TEXT" 'sharebridge_relay_tunnel_reconnects_total')"
  if [[ "$online" =~ ^[0-9]+$ && "$online" -ge 1 ]]; then
    check restart_recovery_baseline_tunnel PASS "baseline sharebridge_relay_tunnel_state{state=\"online\"}=${online}"
  else
    check restart_recovery_baseline_tunnel FAIL "baseline online='${online:-<absent>}' (want >=1) — refusing to restart frps without an online baseline tunnel"
    return 0
  fi

  prepare_route "$SHARE_CODE"
  if [[ "$PREPARE_HTTP_CODE" != "200" || "$PREPARE_STATUS" != "relay" || -z "$PREPARE_RELAY_URL" ]]; then
    check restart_recovery_baseline_prepare FAIL "prepare-route -> ${PREPARE_HTTP_CODE:-000} status='${PREPARE_STATUS:-<none>}' relay_url='${PREPARE_RELAY_URL:+present}' — refusing to restart frps without a serving baseline"
    return 0
  fi
  check restart_recovery_baseline_prepare PASS "prepare-route -> 200 status=relay host=$(url_host "$PREPARE_RELAY_URL")"

  local baseline_url="$PREPARE_RELAY_URL" baseline_file baseline_bytes=0 baseline_code
  baseline_file="$(gate_tmpdir)/baseline.bin"
  baseline_code="$(curl --silent --show-error --max-time 20 ${RELAY_TLS_ARGS[@]+"${RELAY_TLS_ARGS[@]}"} -o "$baseline_file" -w '%{http_code}' "$baseline_url" 2>/dev/null)"
  [[ -f "$baseline_file" ]] && baseline_bytes="$(wc -c < "$baseline_file" | tr -d ' ')"
  if [[ "$baseline_code" == "200" && "${baseline_bytes:-0}" -gt 0 ]]; then
    check restart_recovery_baseline_relay PASS "baseline relay fetch $(url_host "$baseline_url") -> 200 with ${baseline_bytes} bytes"
    set_fact restart_recovery_baseline_bytes "$baseline_bytes"
  else
    check restart_recovery_baseline_relay FAIL "baseline relay fetch $(url_host "$baseline_url") -> ${baseline_code:-000} with ${baseline_bytes:-0} bytes (want 200 with content) — refusing to restart frps"
    return 0
  fi

  # BLOCKING 1: a STABLE single-child identity. The pre-filter may return wrapper
  # PIDs, so the identity is restricted to the exact frpc process name and carries
  # its start time; more than one (or zero) candidate fails closed instead of
  # comparing whatever the pgrep happened to return.
  frpc_before_raw="$(frpc_identities)"
  frpc_before_status="$(frpc_identity_status "$frpc_before_raw")"
  if [[ "$frpc_before_status" != "one" ]]; then
    check restart_recovery_child_before FAIL "expected EXACTLY ONE frpc child identity on ${AGENT_SSH_HOST:-the harness host} (pre-filter '${FRPC_PID_MATCH}', exact name '${FRPC_PID_NAME}'), observed status=${frpc_before_status} identities='$(printf '%s' "$frpc_before_raw" | tr '\n' ' ')' — refusing the restart because the child identity is ambiguous; set LIVE_PHASE4A_FRPC_PID_NAME, LIVE_PHASE4A_FRPC_PID_MATCH, or LIVE_PHASE4A_FRPC_IDENTITY_CMD"
    return 0
  fi
  frpc_pid_before="$(printf '%s\n' "$frpc_before_raw" | head -n1)"
  check restart_recovery_child_before PASS "exactly one agent frpc child identity before the restart: ${frpc_pid_before}"

  # Baseline agent-specific session evidence: frps logs a post-authorization
  # `new proxy [<proxy>] type [tcp] success` line only after the authorization
  # plugin admitted the login for THIS agent's proxy name (§7.2/§7.3).
  proxy_lines_before="$(frps_journal_grep "$proxy_pat" 2>/dev/null | grep -F "$proxy_pat" | grep -c . || true)"
  proxy_lines_before="${proxy_lines_before:-0}"
  set_fact restart_recovery_frps_proxy_lines_before "$proxy_lines_before"
  # A zero baseline is not itself a failure: the agent's registration may simply
  # be older than the journal tail window, and the proof is the INCREASE after
  # the restart (a genuinely unreadable journal yields 0 after as well and still
  # fails the agent-specific check).
  if [[ "$proxy_lines_before" -ge 1 ]]; then
    check restart_recovery_baseline_agent_session PASS "baseline frps proxy-registration count for '${proxy_name}' is ${proxy_lines_before}"
  else
    note restart_recovery_baseline_agent_session "no '${proxy_pat}' line in the current frps journal window (count=0) — the agent-specific proof is the increase after the restart"
  fi

  # (2) Force the EXACT drop: restart the pinned frps unit over SSH while the
  # agent's frpc child keeps running. The harness never restarts the child.
  local t_restart restart_out restart_rc
  t_restart="$(date +%s)"
  restart_out="$(remote_exec "$frps_target" "systemctl restart $FRPS_UNIT")"; restart_rc=$?
  if [[ "$restart_rc" -ne 0 ]]; then
    check restart_recovery_frps_restart FAIL "systemctl restart ${FRPS_UNIT} on ${frps_target} exit=${restart_rc}: $(printf '%s' "$restart_out" | tr '\n' ' ' | cut -c1-200)"
    return 0
  fi
  check restart_recovery_frps_restart PASS "restarted ${FRPS_UNIT} on ${frps_target}"

  # BLOCKING 1: the 'child left alive' claim is RE-MEASURED immediately after the
  # restart (never inferred from the pre-restart value). Recovery can replace the
  # child very quickly, so the sample is taken as the first readable identity
  # (retrying only while it is unobservable) and must still be the pre-restart one.
  local alive_raw="" alive_status="none" alive_id="" alive_try
  for alive_try in 1 2 3; do
    alive_raw="$(frpc_identities)"
    alive_status="$(frpc_identity_status "$alive_raw")"
    [[ "$alive_status" == "one" ]] && break
    sleep 0.2
  done
  alive_id="$(printf '%s\n' "$alive_raw" | head -n1)"
  if [[ "$alive_status" != "one" ]]; then
    check restart_recovery_child_alive FAIL "could not observe EXACTLY ONE frpc child identity immediately after the restart (status=${alive_status}, identities='$(printf '%s' "$alive_raw" | tr '\n' ' ')')"
  elif [[ "$alive_id" == "$frpc_pid_before" ]]; then
    check restart_recovery_child_alive PASS "the pre-restart frpc child (${frpc_pid_before}) was still alive when first re-measured after the frps restart — the harness left it running"
  else
    check restart_recovery_child_alive FAIL "the frpc child identity changed to '${alive_id}' before it could be re-measured after the restart (was '${frpc_pid_before}') — cannot prove the pre-restart child was left alive"
  fi

  # (3) Observe the tunnel go offline, bounded, with a clear failure.
  local offline_ok=0 offline_secs=-1 offline_deadline=$(( t_restart + TUNNEL_OFFLINE_BOUND_S ))
  while [[ "$(deadline_remaining "$offline_deadline")" -gt 0 ]]; do
    online="$(gateway_tunnel_online "$(deadline_remaining "$offline_deadline")" 2>/dev/null || true)"
    if [[ "$online" =~ ^[0-9]+$ && "$online" -eq 0 ]]; then
      offline_ok=1; offline_secs="$(( $(date +%s) - t_restart ))"; break
    fi
    sleep 0.5
  done
  set_fact restart_recovery_offline_seconds "$offline_secs"
  if [[ "$offline_ok" == "1" ]]; then
    check restart_recovery_tunnel_offline PASS "tunnel reported online=0 ${offline_secs}s after the frps restart (bound ${TUNNEL_OFFLINE_BOUND_S}s)"
  else
    check restart_recovery_tunnel_offline FAIL "tunnel never reported online=0 within ${TUNNEL_OFFLINE_BOUND_S}s of the frps restart (last online='${online:-<absent>}') — the dropped-session scenario was unobservable"
  fi

  # (4) Without any operator action, require a REPLACED child, a NEW tunnel
  # session, an agent-specific fresh admitted session for proxy '${proxy_name}',
  # gateway online=1 and serving relay content, all within a MONOTONIC bound
  # measured from the frps restart. Every stage is capped by the remaining time,
  # and the elapsed time is re-measured after all stages complete.
  local rec_ok=0 rec_secs=-1 content_ok=0 frpc_after_raw="" frpc_after_status="none" frpc_after=""
  local reconnects_after="" proxy_lines_after="" agent_session_ok=0 remaining
  local deadline=$(( t_restart + RECOVERY_BOUND_S ))
  while :; do
    remaining="$(deadline_remaining "$deadline")"
    [[ "$remaining" -gt 0 ]] || break
    # fetch_gateway_metrics must run in THIS shell (not a command substitution),
    # or its GATEWAY_METRICS_TEXT assignment would be lost to the subshell and
    # the reconnect counter would be read stale.
    if fetch_gateway_metrics "$remaining" >/dev/null 2>&1; then
      online="$(metric_value "$GATEWAY_METRICS_TEXT" 'sharebridge_relay_tunnel_state{state="online"}')"
    else
      online=""
    fi
    frpc_after_raw="$(frpc_identities)"
    frpc_after_status="$(frpc_identity_status "$frpc_after_raw")"
    frpc_after=""
    [[ "$frpc_after_status" == "one" ]] && frpc_after="$(printf '%s\n' "$frpc_after_raw" | head -n1)"
    reconnects_after="$(metric_value "${GATEWAY_METRICS_TEXT:-}" 'sharebridge_relay_tunnel_reconnects_total')"
    content_ok=0
    if [[ "$online" =~ ^[0-9]+$ && "$online" -ge 1 ]]; then
      # Only attempt the content fetch once presence is back, so the bounded
      # loop stays cheap; the fetch is still capped by the remaining time.
      remaining="$(deadline_remaining "$deadline")"
      if [[ "$remaining" -gt 0 ]]; then
        prepare_route "$SHARE_CODE" "$remaining"
      fi
      if [[ "$PREPARE_HTTP_CODE" == "200" && "$PREPARE_STATUS" == "relay" && -n "$PREPARE_RELAY_URL" ]]; then
        remaining="$(deadline_remaining "$deadline")"
        if [[ "$remaining" -gt 0 ]]; then
          local rec_file rec_code rec_bytes=0
          rec_file="$(gate_tmpdir)/recovered.bin"
          rec_code="$(curl --silent --show-error --max-time "$remaining" ${RELAY_TLS_ARGS[@]+"${RELAY_TLS_ARGS[@]}"} -o "$rec_file" -w '%{http_code}' "$PREPARE_RELAY_URL" 2>/dev/null)"
          [[ -f "$rec_file" ]] && rec_bytes="$(wc -c < "$rec_file" | tr -d ' ')"
          [[ "$rec_code" == "200" && "${rec_bytes:-0}" -gt 0 ]] && content_ok=1
        fi
      fi
      remaining="$(deadline_remaining "$deadline")"
      if [[ "$remaining" -gt 0 ]]; then
        proxy_lines_after="$(frps_journal_grep "$proxy_pat" 2>/dev/null | grep -F "$proxy_pat" | grep -c . || true)"
        proxy_lines_after="${proxy_lines_after:-0}"
      fi
    fi
    agent_session_ok=0
    [[ "$(counter_increased_verdict "$proxy_lines_before" "${proxy_lines_after:-}")" == "ok" ]] && agent_session_ok=1
    if [[ "$(restart_recovery_verdict "$online" "$frpc_pid_before" "$frpc_after" "${reconnects_before:-}" "${reconnects_after:-}" "$content_ok" "$agent_session_ok")" == "ok" ]]; then
      rec_ok=1; break
    fi
    sleep 1
  done
  # IMPORTANT 5: measure elapsed AFTER every stage has completed, so a recovery
  # that only finished after the bound cannot be reported as within it.
  rec_secs="$(( $(date +%s) - t_restart ))"

  set_fact restart_recovery_seconds "$rec_secs"
  set_fact restart_recovery_frps_proxy_lines_after "${proxy_lines_after:-<absent>}"
  verdict="$(restart_recovery_verdict "$online" "$frpc_pid_before" "$frpc_after" "${reconnects_before:-}" "${reconnects_after:-}" "$content_ok" "$agent_session_ok")"

  if [[ "$frpc_after_status" == "one" && -n "$frpc_after" && "$frpc_after" != "$frpc_pid_before" ]]; then
    check restart_recovery_new_child PASS "frpc child replaced automatically: ${frpc_pid_before} -> ${frpc_after}"
  elif [[ "$frpc_after_status" != "one" ]]; then
    check restart_recovery_new_child FAIL "the post-recovery frpc child identity is ambiguous (status=${frpc_after_status}, identities='$(printf '%s' "$frpc_after_raw" | tr '\n' ' ')')"
  else
    check restart_recovery_new_child FAIL "frpc child identity did not change (before='${frpc_pid_before}' after='${frpc_after:-<absent>}') — the child was not replaced without operator action"
  fi
  if [[ "$(counter_increased_verdict "${reconnects_before:-}" "${reconnects_after:-}")" == "ok" ]]; then
    check restart_recovery_new_session PASS "a NEW tunnel session was established after the restart (sharebridge_relay_tunnel_reconnects_total ${reconnects_before} -> ${reconnects_after}) — corroborating evidence that a fresh credential was requested and accepted"
  else
    check restart_recovery_new_session FAIL "no new tunnel session observed (reconnects ${reconnects_before:-<absent>} -> ${reconnects_after:-<absent>})"
  fi
  if [[ "$agent_session_ok" == "1" ]]; then
    check restart_recovery_agent_session PASS "agent-specific fresh session: a NEW frps '${proxy_pat}' line was logged after the restart (count ${proxy_lines_before} -> ${proxy_lines_after}); frps logs this only after the authorization plugin admitted a login for this agent's proxy name"
  else
    check restart_recovery_agent_session FAIL "no NEW frps '${proxy_pat}' line after the restart (before=${proxy_lines_before} after='${proxy_lines_after:-<unobservable>}') — the global reconnect counter alone can be another agent's session, so the agent-specific admission proof is required; check LIVE_PHASE4A_AGENT_PROXY_NAME and the frps journal source"
  fi
  if [[ "$online" =~ ^[0-9]+$ && "$online" -ge 1 ]]; then
    check restart_recovery_tunnel_online PASS "gateway tunnel presence restored: sharebridge_relay_tunnel_state{state=\"online\"}=${online}"
  else
    check restart_recovery_tunnel_online FAIL "gateway tunnel still offline at the end of the bound (online='${online:-<absent>}')"
  fi
  if [[ "$content_ok" == "1" ]]; then
    check restart_recovery_relay_content PASS "relay content served after automatic recovery ($(url_host "$PREPARE_RELAY_URL") -> 200 with content)"
  else
    check restart_recovery_relay_content FAIL "relay content did not serve after recovery (prepare http=${PREPARE_HTTP_CODE:-000} status='${PREPARE_STATUS:-<none>}')"
  fi
  local bound_verdict
  bound_verdict="$(bound_elapsed_verdict "$rec_secs" "$RECOVERY_BOUND_S")"
  if [[ "$rec_ok" == "1" && "$bound_verdict" == "ok" ]]; then
    check restart_recovery_within_bound PASS "automatic recovery (new child + fresh session + agent-specific session + online + serving content) completed ${rec_secs}s after the frps restart, measured after all stages (bound ${RECOVERY_BOUND_S}s)"
  elif [[ "$rec_ok" == "1" ]]; then
    check restart_recovery_within_bound FAIL "all recovery conditions were observed, but the measured elapsed time (${rec_secs}s, after every stage completed) exceeds the bound ${RECOVERY_BOUND_S}s — refusing a late recovery"
  else
    check restart_recovery_within_bound FAIL "no full automatic recovery within ${RECOVERY_BOUND_S}s of the frps restart (elapsed ${rec_secs}s, verdict='${verdict}')"
  fi
  return 0
}
acceptance_10_lockdown()             { gate_not_implemented "Task 43" "§19 #10 lockdown drops direct mapping + FRP tunnel, closes both active connection kinds, unlock requires fresh credential"; }
acceptance_15_heartbeat_tunnel_dns() { gate_not_implemented "Task 43" "§19 #15 tunnel DNS + dedicated transport cert, 10s Pings, one delayed Ping tolerated, unavailable after a true 45s lease expiry"; }
release_go_no_go_rollback()          { gate_not_implemented "Task 44" "§20 steps 5–7 + §23 blocking rule: release-manifest gate, staged automatic fallback, rollback drill with RELAY_SELECTION_ENABLED=false"; }

# ===========================================================================
# Selftest — proves the runner's pass/fail/skip/missing plumbing is real.
# ===========================================================================

selftest_check() {
  local label="$1" got="$2" want="$3"
  if [[ "$got" == "$want" ]]; then
    printf 'SELFTEST [PASS] %s: got %s\n' "$label" "$got"
    return 0
  fi
  printf 'SELFTEST [FAIL] %s: got %s, want %s\n' "$label" "$got" "$want"
  return 1
}

_selftest_pass() { check selftest_check PASS "a trivially true check"; }
_selftest_fail() { check selftest_check FAIL "a trivially false check"; }
_selftest_skip() { check selftest_check SKIP "a deliberately skipped check"; }
_selftest_empty() { :; }
_selftest_notimpl() { gate_not_implemented "selftest" "a not-implemented gate"; }
_selftest_crash() { check selftest_check PASS "recorded before crashing"; return 7; }

# A gate that emits only note() must never pass: notes are informational, a
# PASS verdict requires at least one measured `check ... PASS`.
_selftest_note_only() { note selftest_note "an informational note with no measured check"; }
# Notes do not invalidate a gate that really measured a passing check.
_selftest_note_and_pass() { note selftest_note "an informational note"; check selftest_check PASS "a measured passing check"; }
# Secret-shaped wording in a check() detail must be redacted everywhere.
_selftest_secret() { check selftest_secret PASS "observed token=SUPERSECRETTOKEN123456 api_key=ABCDEFGHIJKLMNOP for /s/SECRETSHARE99"; }

run_selftest() {
  local failures=0 got verdict
  printf '=== live-phase4a.sh selftest (pass/fail plumbing) ===\n'

  execute_gate _selftest_pass;    selftest_check "trivially-true gate" "$EXECUTE_RESULT" "PASS" || failures=$((failures + 1)); rm -rf "$EXECUTE_TMP"
  execute_gate _selftest_fail;    selftest_check "trivially-false gate" "$EXECUTE_RESULT" "FAIL" || failures=$((failures + 1)); rm -rf "$EXECUTE_TMP"
  execute_gate _selftest_skip;    selftest_check "skipped gate is not a pass" "$EXECUTE_RESULT" "SKIP" || failures=$((failures + 1)); rm -rf "$EXECUTE_TMP"
  execute_gate _selftest_empty;   selftest_check "zero-check gate is MISSING (never PASS)" "$EXECUTE_RESULT" "MISSING" || failures=$((failures + 1)); rm -rf "$EXECUTE_TMP"
  execute_gate _selftest_notimpl; selftest_check "placeholder gate is NOT_IMPLEMENTED" "$EXECUTE_RESULT" "NOT_IMPLEMENTED" || failures=$((failures + 1)); rm -rf "$EXECUTE_TMP"
  execute_gate _selftest_crash;   selftest_check "gate that records PASS then crashes is FAIL" "$EXECUTE_RESULT" "FAIL" || failures=$((failures + 1)); rm -rf "$EXECUTE_TMP"
  execute_gate does_not_exist;    selftest_check "unregistered function is MISSING" "$EXECUTE_RESULT" "MISSING" || failures=$((failures + 1)); rm -rf "$EXECUTE_TMP"
  execute_gate _selftest_note_only; selftest_check "note-only gate is MISSING (never PASS)" "$EXECUTE_RESULT" "MISSING" || failures=$((failures + 1)); rm -rf "$EXECUTE_TMP"
  execute_gate _selftest_note_and_pass; selftest_check "notes plus a measured PASS stay PASS" "$EXECUTE_RESULT" "PASS" || failures=$((failures + 1)); rm -rf "$EXECUTE_TMP"

  # B2 negative test: a secret-shaped check() detail must come out redacted in
  # BOTH the console output and the recorded (evidence) check file. Capture the
  # gate's stdout to a file so EXECUTE_TMP stays visible in this shell.
  local secret_out_file secret_out secret_checks
  secret_out_file="$(mktemp)"
  execute_gate _selftest_secret > "$secret_out_file"
  secret_out="$(cat "$secret_out_file")"
  secret_checks="$(cat "$EXECUTE_TMP/checks")"
  rm -rf "$EXECUTE_TMP"; rm -f "$secret_out_file"
  got="redacted"
  printf '%s\n%s' "$secret_out" "$secret_checks" | grep -qE 'SUPERSECRETTOKEN123456|ABCDEFGHIJKLMNOP|SECRETSHARE99' && got="raw secret leaked"
  selftest_check "check() detail has no raw secret (console + evidence)" "$got" "redacted" || failures=$((failures + 1))
  got="redacted"
  printf '%s' "$secret_out" | grep -q 'REDACTED' || got="console detail not redacted"
  printf '%s' "$secret_checks" | grep -q 'REDACTED' || got="recorded detail not redacted"
  selftest_check "check() detail carries a redaction marker (console + evidence)" "$got" "redacted" || failures=$((failures + 1))

  got="$(verdict_for "PASS,")";                 selftest_check "verdict(all PASS)" "$got" "GREEN" || failures=$((failures + 1))
  got="$(verdict_for "PASS,FAIL,")";            selftest_check "verdict(FAIL present)" "$got" "RED" || failures=$((failures + 1))
  got="$(verdict_for "PASS,SKIP,")";            selftest_check "verdict(SKIP is not a pass)" "$got" "PARTIAL" || failures=$((failures + 1))
  got="$(verdict_for "PASS,NOT_IMPLEMENTED,")"; selftest_check "verdict(placeholder present)" "$got" "RED" || failures=$((failures + 1))
  got="$(verdict_for "PASS,MISSING,")";         selftest_check "verdict(unrun/missing)" "$got" "RED" || failures=$((failures + 1))

  # Restart-recovery verdict predicate: the live gate and this selftest share
  # one definition of "automatically recovered", so every branch is proven.
  # The 7th argument is the agent-specific fresh-session proof (BLOCKING 2).
  got="$(restart_recovery_verdict 0 old new 5 6 1 1)";   selftest_check "recovery verdict refuses an offline tunnel" "$got" "offline" || failures=$((failures + 1))
  got="$(restart_recovery_verdict 1 old old 5 6 1 1)";   selftest_check "recovery verdict refuses an unchanged child" "$got" "no-child" || failures=$((failures + 1))
  got="$(restart_recovery_verdict 1 old new 5 5 1 1)";   selftest_check "recovery verdict refuses an unchanged session counter" "$got" "no-session" || failures=$((failures + 1))
  got="$(restart_recovery_verdict 1 old new 5 6 1 0)";   selftest_check "recovery verdict refuses a session not proven against the agent under test" "$got" "no-agent-session" || failures=$((failures + 1))
  got="$(restart_recovery_verdict 1 old new 5 6 0 1)";   selftest_check "recovery verdict refuses a non-serving relay" "$got" "no-content" || failures=$((failures + 1))
  got="$(restart_recovery_verdict 1 old new 5 6 1 1)";   selftest_check "recovery verdict accepts a full automatic recovery" "$got" "ok" || failures=$((failures + 1))

  # BLOCKING 1: one stable frpc identity (PID:STARTTIME). Wrapper command lines
  # must never count, and 0 or >1 identities must fail closed.
  got="$(frpc_identity_status '878807:1700000000')";        selftest_check "one frpc identity is usable" "$got" "one" || failures=$((failures + 1))
  got="$(frpc_identity_status '')";                          selftest_check "zero frpc identities is ambiguous (none)" "$got" "none" || failures=$((failures + 1))
  got="$(frpc_identity_status '1:10
2:20')";                selftest_check "two frpc identities is ambiguous (many)" "$got" "many" || failures=$((failures + 1))
  got="$(frpc_identity_status '917087:1700000000
917088:1700000001
917089:1700000002')"; selftest_check "three frpc identities is ambiguous (many)" "$got" "many" || failures=$((failures + 1))
  got="$(frpc_identity_status '  917087:1700000000  ')";     selftest_check "a whitespace-padded identity is still one" "$got" "one" || failures=$((failures + 1))

  # BLOCKING 2: increases are a pure predicate, shared by the global counter and
  # the agent-specific frps proxy-registration line count.
  got="$(counter_increased_verdict 5 6)";       selftest_check "an increased counter is proven" "$got" "ok" || failures=$((failures + 1))
  got="$(counter_increased_verdict 6 6)";       selftest_check "an unchanged counter is not a new session" "$got" "no-increase" || failures=$((failures + 1))
  got="$(counter_increased_verdict x 6)";       selftest_check "an unparseable counter fails closed" "$got" "unobservable" || failures=$((failures + 1))
  got="$(counter_increased_verdict 1 '')";      selftest_check "a missing after-count fails closed" "$got" "unobservable" || failures=$((failures + 1))

  # IMPORTANT 5: the bound is enforced after all stages, not only before an
  # iteration, and each stage can be capped by the remaining time.
  got="$(bound_elapsed_verdict 12 120)";        selftest_check "an in-bound elapsed time is accepted" "$got" "ok" || failures=$((failures + 1))
  got="$(bound_elapsed_verdict 121 120)";       selftest_check "a late recovery fails the bound" "$got" "over-bound" || failures=$((failures + 1))
  got="$(bound_elapsed_verdict '' 120)";        selftest_check "an unmeasurable elapsed time fails closed" "$got" "unobservable" || failures=$((failures + 1))
  local past_deadline future_deadline rem_past rem_future
  past_deadline=$(( $(date +%s) - 5 )); future_deadline=$(( $(date +%s) + 30 ))
  rem_past="$(deadline_remaining "$past_deadline")"; rem_future="$(deadline_remaining "$future_deadline")"
  selftest_check "an expired deadline leaves no time" "$rem_past" "0" || failures=$((failures + 1))
  got="$([[ "$rem_future" =~ ^[0-9]+$ && "$rem_future" -gt 0 && "$rem_future" -le 30 ]] && printf ok || printf bad)"
  selftest_check "a live deadline yields the remaining seconds" "$got" "ok" || failures=$((failures + 1))

  # IMPORTANT 6: the frps SSH host + systemd unit need their own exact
  # confirmation, separate from the control-URL target assertion.
  got="$(frps_restart_target_verdict '' root@10.0.0.5 sharebridge-relay-frps.service)"; selftest_check "restart target guard refuses no confirmation" "$got" "unset" || failures=$((failures + 1))
  got="$(frps_restart_target_verdict 'root@10.0.0.9|sharebridge-relay-frps.service' root@10.0.0.5 sharebridge-relay-frps.service)"; selftest_check "restart target guard refuses a different host" "$got" "mismatch" || failures=$((failures + 1))
  got="$(frps_restart_target_verdict 'root@10.0.0.5|other.service' root@10.0.0.5 sharebridge-relay-frps.service)"; selftest_check "restart target guard refuses a different unit" "$got" "mismatch" || failures=$((failures + 1))
  got="$(frps_restart_target_verdict 'root@10.0.0.5|sharebridge-relay-frps.service' root@10.0.0.5 sharebridge-relay-frps.service)"; selftest_check "restart target guard accepts the exact host|unit" "$got" "ok" || failures=$((failures + 1))

  # IMPORTANT 7: command evidence must be sanitised before it is recorded; the
  # prepare-route URL carries the share code.
  local cmd_evidence cmd_file="$(mktemp)" cmd_saved="${GATE_CMD_FILE:-}"
  GATE_CMD_FILE="$cmd_file"
  record_cmd "curl -X POST https://control.example/api/shares/SECRETSHARE99/prepare-route"
  cmd_evidence="$(cat "$cmd_file")"
  GATE_CMD_FILE="$cmd_saved"; rm -f "$cmd_file"
  got="redacted"
  printf '%s' "$cmd_evidence" | grep -q 'SECRETSHARE99' && got="raw share code leaked into command evidence"
  selftest_check "recorded command evidence redacts the share code" "$got" "redacted" || failures=$((failures + 1))
  got="redacted"
  printf '%s' "$cmd_evidence" | grep -q 'REDACTED' || got="no redaction marker in command evidence"
  selftest_check "recorded command evidence carries a redaction marker" "$got" "redacted" || failures=$((failures + 1))

  if [[ "$failures" -eq 0 ]]; then
    printf 'SELFTEST RESULT: PASS (0 failures) — the gate runner refuses PASS for unexecuted, note-only, skipped, crashing or unimplemented gates\n'
    exit 0
  fi
  printf 'SELFTEST RESULT: FAIL (%d assertions failed) — the harness plumbing is broken\n' "$failures"
  exit 1
}

# ---------------------------------------------------------------------------
# Main
# ---------------------------------------------------------------------------

if [[ "$LIST_ONLY" -eq 1 ]]; then
  printf '%-42s %-9s %s\n' "GATE" "OWNER" "PURPOSE"
  for i in "${!gate_names[@]}"; do
    printf '%-42s %-9s %s\n' "${gate_names[$i]}" "${gate_owners[$i]}" "${gate_purposes[$i]}"
  done
  exit 0
fi

if [[ "$MODE" == "selftest" ]]; then
  run_selftest
fi

if [[ "$DRY_RUN" -eq 1 ]]; then
  printf '=== live-phase4a.sh dry run (nothing executed) ===\n'
  printf 'git_sha=%s\n' "$GIT_SHA"
  printf 'control_host=%s\n' "${CONTROL_HOST:-<unset>}"
  printf 'relay_host=%s\n' "${RELAY_HOST:-<unset>}"
  printf 'relay_public_ip=%s\n' "${RELAY_PUBLIC_IP:-<unset>}"
  printf 'relay_tunnel_host=%s\n' "${RELAY_TUNNEL_HOST:-<unset>}"
  printf 'namespace=%s\n' "${NAMESPACE:-<unset>}"
  printf 'transport_port=%s\n' "$TRANSPORT_PORT"
  printf 'control_base_url=%s\n' "${CONTROL_BASE_URL:-<unset>}"
  printf 'share_code=%s\n' "$([[ -n "$SHARE_CODE" ]] && printf 'set' || printf '<unset>')"
  printf 'gateway_metrics_url=%s\n' "${GATEWAY_METRICS_URL:-<unset>}"
  printf 'allow_restart=%s\n' "$ALLOW_RESTART"
  printf 'frps_restart_target=%s|%s\n' "${FRPS_SSH_HOST:-$RELAY_HOST}" "$FRPS_UNIT"
  printf 'frps_restart_confirm=%s\n' "$([[ -n "$FRPS_RESTART_CONFIRM" ]] && printf asserted || printf unset)"
  printf 'agent_proxy_name=%s\n' "${AGENT_PROXY_NAME:-$([[ -n "$NAMESPACE" ]] && printf 'sb-%s' "$NAMESPACE" || printf '<unset>')}"
  printf 'frps_journal_file=%s\n' "${FRPS_JOURNAL_FILE:-<unset>}"
  printf 'frpc_identity_cmd=%s\n' "$([[ -n "$FRPC_IDENTITY_CMD" ]] && printf override || printf default)"
  printf 'frpc_prefilter=%s frpc_exact_name=%s\n' "$FRPC_PID_MATCH" "$FRPC_PID_NAME"
  printf 'recovery_bound_s=%s\n' "$RECOVERY_BOUND_S"
  printf 'evidence_dir=%s\n' "$RUN_DIR"
  printf '\nGates that would run:\n'
  for i in "${!gate_names[@]}"; do
    if is_selected "${gate_names[$i]}"; then
      printf '  %-42s (%s)\n' "${gate_names[$i]}" "${gate_owners[$i]}"
    else
      printf '  %-42s (%s) [not selected]\n' "${gate_names[$i]}" "${gate_owners[$i]}"
    fi
  done
  printf '\nDRY RUN: no gate was executed; this is NOT a pass (exit 3).\n'
  exit 3
fi

printf '=== ShareBridge Phase 4a M6 live acceptance harness (Tasks 37–44) ===\n'
printf 'This run targets the M6 dark topology. The collocated test VPS (%s) is explicitly excluded.\n' "$EXCLUDED_RELAY_IPS"
run_all_gates
print_summary

case "$VERDICT" in
  GREEN) exit 0 ;;
  PARTIAL) exit 3 ;;
  *) exit 1 ;;
esac
