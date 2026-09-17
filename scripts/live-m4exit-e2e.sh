#!/usr/bin/env bash
# live-m4exit-e2e.sh — ShareBridge Phase 4a live relay end-to-end harness for the
# M4-exit acceptance scenarios (SDD task #15; ledger section "LIVE M4-EXIT E2E").
#
# This is the SIBLING of scripts/live-phase4a.sh (the M6 acceptance harness). It
# encodes the verified sequence of the 2026-09-17 live run against a deployed
# test stack (a control/relay host plus a home agent behind a NAT) so the same
# scenarios can be re-run repeatably. It does not replace the M6 harness and it
# does not target the M6 dark separate-VM topology.
#
# Cases (each separately scoped with --case, all runnable together):
#   enrollment_hydration_restart      agent enrollment live; registered share
#                                     count == the operator's declared count;
#                                     restart hydration ("loaded N sessions from
#                                     store") observed; per-share content
#                                     resolves over the relay afterwards.
#   relay_content_integrity           interstitial -> prepare-route -> relay;
#                                     gallery 200, /items manifest count,
#                                     /thumb image type, full /asset sha1 ==
#                                     the manifest sha1 + exact byte count,
#                                     video /asset/<id>/playback HEAD 200 +
#                                     200 full + >=2 in-range 206 byte-exact
#                                     seeks + the three documented 416 cases.
#   stun_observe_and_rechallenge      control STUN counters match>0 with zero
#                                     mismatch/timeout, and the §10.2 proactive
#                                     rechallenge cadence (4m + jitter[0,15s))
#                                     is measured from the accept timestamps.
#   direct_path_or_failclosed         either status=direct with a direct URL
#                                     that serves, OR a fail-closed relay
#                                     fallback whose control-side diagnostics
#                                     record direct_status=relay_fallback and
#                                     the expected direct_status_reason. Never
#                                     PASS when neither is observable.
#   lockdown_withdrawal_and_recovery  OPT-IN. baseline healthy -> lockdown ->
#                                     tunnel offline + frps proxy close +
#                                     prepare-route suppressed + relay fetch
#                                     yields no content -> unlock -> recovery
#                                     within a documented bound (recovery
#                                     seconds recorded).
#   revocation_midstream              OPT-IN. throttled in-flight download ->
#                                     DELETE the share mid-transfer -> transfer
#                                     truncated + gateway route-revocation drain
#                                     log line with streams>=1. Records the
#                                     Immich re-registration caveat.
#
# HONESTY CONTRACT (same semantics as scripts/live-phase4a.sh):
#   * A case reports PASS only if it MEASURED at least one passing `check`.
#     note() output is informational and can never carry a verdict; a
#     note-only (or otherwise check-less) case is MISSING, never PASS.
#   * A case function that exits nonzero after recording only PASSes is FAIL.
#   * A case with any SKIP sub-check is SKIP, never PASS.
#   * An unscoped run is GREEN only if every registered case PASSes.
#   * `--case a,b` scopes the verdict; unselected cases are NOT_RUN (excluded).
#   * `--dry-run` executes nothing and exits 3; it is NOT a pass.
#
# SAFETY:
#   * Read-only against remote systems (curl GET/POST prepare-route, ssh
#     journalctl/curl, dig, openssl). The ONLY state-changing calls are the
#     three explicitly opt-in actions: lockdown/unlock
#     (LIVE_M4EXIT_ALLOW_LOCKDOWN=1), share revocation
#     (LIVE_M4EXIT_ALLOW_REVOKE=1) and an explicit agent restart
#     (LIVE_M4EXIT_ALLOW_AGENT_RESTART=1, case 1 only). All are reversible and
#     agent-local (revoke never deletes an Immich share).
#   * The lockdown safety net is registered in the MAIN process (subshells
#     reset traps) and re-reads the locked state from a state file, so an
#     interrupted, failing or SIGTERM-ed run re-sends POST /api/unlock instead
#     of stranding a locked agent.
#   * Secrets are never printed or stored: every captured line and every
#     check() detail passes through sanitize(), which redacts configured
#     password/share-code literals, key/token/password/secret/cookie/
#     authorization/jti values, private-key blocks and /s/<code> paths.
#   * The default evidence directory is outside the worktree
#     (${TMPDIR:-/tmp}/sharebridge-m4exit-e2e) so a run never dirties the repo.
#     Override with --evidence-dir or LIVE_M4EXIT_EVIDENCE_DIR.
#   * Idempotent: repeated runs only create new timestamped evidence records.
#
# Usage:
#   scripts/live-m4exit-e2e.sh                       # run every case (live)
#   scripts/live-m4exit-e2e.sh --case relay_content_integrity
#   scripts/live-m4exit-e2e.sh --list                # print the case table
#   scripts/live-m4exit-e2e.sh --dry-run             # validate config + plan; runs nothing
#   scripts/live-m4exit-e2e.sh --selftest            # prove the pass/fail plumbing
#   scripts/live-m4exit-e2e.sh --help
#
# Exit codes:
#   0  GREEN   — every selected case PASS (and, unscoped, every registered case)
#   1  RED     — at least one selected case FAIL / MISSING, or the selftest
#                plumbing is broken
#   2  USAGE   — bad arguments, or --dry-run with incomplete configuration
#   3  PARTIAL — no failures, but at least one SKIP, or a dry run (nothing
#                executed is never a pass)
#
# Environment (all resolved at startup; a required value that is unset FAILs the
# affected case and is named in the diagnostic — never guessed). Values are
# URLs/hosts/paths/names, not secrets, except the explicit credential variables.
#
#   Control / interstitial
#     LIVE_M4EXIT_CONTROL_BASE_URL      control base URL, e.g. https://control.example (required: cases 2,4,5)
#     LIVE_M4EXIT_CONTROL_INSECURE_TLS  1 disables control TLS verification (default 0)
#     LIVE_M4EXIT_SHARE_CODE            share code for the relay content scenario (required: cases 1,2,4,5)
#     LIVE_M4EXIT_MANIFEST_FILE         JSON manifest supplying share_code/item_id/asset_id/video_id/expected_item_count/asset_bytes for case 2 (optional)
#
#   Relay content (case 2)
#     LIVE_M4EXIT_RELAY_HOST            relay gateway hostname; the per-share relay URL host must be it or a subdomain (required)
#     LIVE_M4EXIT_ITEM_ID               image item id whose /items sha1 is the authority (required)
#     LIVE_M4EXIT_ASSET_ID              full original asset id (== the manifest item id) (required)
#     LIVE_M4EXIT_VIDEO_ID              video item id for the playback/Range checks (required)
#     LIVE_M4EXIT_EXPECTED_ITEM_COUNT   expected /items count (required)
#     LIVE_M4EXIT_EXPECTED_ASSET_BYTES  expected byte count for /asset/<id> (optional; else the manifest size is used)
#     LIVE_M4EXIT_MIN_RANGE_REQUESTS    minimum in-range 206 seeks to prove (default 2)
#     LIVE_M4EXIT_RELAY_INSECURE_TLS    1 disables relay TLS verification (default 0)
#
#   Agent admin API (cases 1,5,6; typically a loopback admin URL)
#     LIVE_M4EXIT_AGENT_ADMIN_BASE_URL  agent admin base URL (required: cases 1,5,6)
#     LIVE_M4EXIT_AGENT_ADMIN_USER      agent admin Basic-auth user (required: cases 1,5,6)
#     LIVE_M4EXIT_AGENT_ADMIN_PASSWORD  agent admin Basic-auth password (required: cases 1,5,6; never logged)
#     LIVE_M4EXIT_AGENT_ADMIN_ORIGIN    Origin header for admin CSRF (default: the admin base URL)
#     LIVE_M4EXIT_AGENT_ADMIN_INSECURE_TLS 1 disables agent admin TLS verification (default 0)
#
#   Control metrics + STUN journal (case 3)
#     LIVE_M4EXIT_CONTROL_METRICS_URL   full control /metrics URL reachable from here (optional)
#     LIVE_M4EXIT_CONTROL_SSH_HOST      control SSH target (used when the metrics URL is absent; also the journal source)
#     LIVE_M4EXIT_CONTROL_UNIT          control systemd unit for the STUN timestamps (default sharebridge.service)
#     LIVE_M4EXIT_CONTROL_METRICS_ADDR  loopback metrics addr over SSH (default 127.0.0.1:9102)
#     LIVE_M4EXIT_STUN_JOURNAL_FILE     operator-supplied journal/evidence file containing "stun observation accepted" lines (preferred over SSH)
#     LIVE_M4EXIT_STUN_CADENCE_TOLERANCE_S   documented cadence tolerance seconds (default 5)
#     LIVE_M4EXIT_STUN_MIN_IN_BAND_DELTAS    minimum on-cadence deltas required (default 1)
#
#   Gateway metrics + journals (cases 1,5)
#     LIVE_M4EXIT_GATEWAY_HEALTH_URL    full gateway /healthz URL reachable from here (optional)
#     LIVE_M4EXIT_GATEWAY_METRICS_URL   full gateway /metrics URL reachable from here (optional)
#     LIVE_M4EXIT_GATEWAY_SSH_HOST      gateway SSH target (default: LIVE_M4EXIT_CONTROL_SSH_HOST)
#     LIVE_M4EXIT_GATEWAY_METRICS_ADDR  loopback metrics addr over SSH (default 127.0.0.1:9101)
#     LIVE_M4EXIT_GATEWAY_UNIT          gateway systemd unit (default sharebridge-relay-gateway.service)
#     LIVE_M4EXIT_GATEWAY_JOURNAL_FILE  operator-supplied gateway journal/evidence file (preferred over SSH; cases 5,6)
#     LIVE_M4EXIT_FRPS_UNIT             frps systemd unit (default sharebridge-relay-frps.service)
#     LIVE_M4EXIT_FRPS_JOURNAL_FILE     operator-supplied frps journal/evidence file (preferred over SSH; case 5)
#     LIVE_M4EXIT_FRPS_SSH_HOST         frps SSH target (default: the gateway SSH target)
#     LIVE_M4EXIT_JOURNAL_MAX_LINES     journal tail line budget for SSH reads (default 5000)
#
#   Agent log (case 1)
#     LIVE_M4EXIT_AGENT_LOG_FILE        operator-supplied agent log/evidence file (preferred)
#     LIVE_M4EXIT_AGENT_SSH_HOST        agent SSH target when no local file is given
#     LIVE_M4EXIT_AGENT_JOURNAL_UNIT    agent systemd unit for journalctl (default sharebridge-agent)
#     LIVE_M4EXIT_EXPECTED_SHARE_COUNT  declared registered share count (required: case 1)
#     LIVE_M4EXIT_EXPECTED_HYDRATED_SESSIONS  minimum restart-hydrated sessions (default 1)
#     (when no restart is observable, either point LIVE_M4EXIT_AGENT_LOG_FILE at
#      a startup log that contains the line, or use the opt-in restart vars below)
#
#   Control-side diagnostics (case 4)
#     LIVE_M4EXIT_AGENT_RECORD_FILE     operator-supplied agent record with direct_status/direct_status_reason (required for a relay fallback)
#     LIVE_M4EXIT_EXPECTED_DIRECT_REASON  expected direct_status_reason for the fail-closed fallback (default probe_failed)
#
#   Opt-in state-changing cases
#     LIVE_M4EXIT_ALLOW_AGENT_RESTART   1 restarts the agent inside case 1 so the "loaded N sessions from store" line is freshly observable (default 0 => no restart)
#     LIVE_M4EXIT_AGENT_RESTART_COMMAND exact restart command, required when _ALLOW_AGENT_RESTART=1 (run on LIVE_M4EXIT_AGENT_SSH_HOST when set, else locally)
#     LIVE_M4EXIT_AGENT_RESTART_WAIT_S  seconds to wait for the hydration line after an opted-in restart (default 30)
#     LIVE_M4EXIT_ALLOW_LOCKDOWN        1 enables case 5 (lockdown/unlock; default 0 => SKIP)
#     LIVE_M4EXIT_RECOVERY_BOUND_S      documented post-unlock recovery bound (default 120)
#     LIVE_M4EXIT_TUNNEL_OFFLINE_BOUND_S documented post-lockdown tunnel-offline bound (default 30)
#     LIVE_M4EXIT_ALLOW_REVOKE          1 enables case 6 (revoke a share; default 0 => SKIP)
#     LIVE_M4EXIT_REVOKE_SHARE_CODE     the share code case 6 revokes (required when ALLOW_REVOKE=1)
#     LIVE_M4EXIT_REVOKE_ASSET_ID       asset id to download mid-transfer (default: LIVE_M4EXIT_ASSET_ID)
#     LIVE_M4EXIT_REVOKE_ASSET_BYTES    full asset byte count (optional; else read from /items)
#     LIVE_M4EXIT_REVOKE_LIMIT_RATE     curl --limit-rate throttle (default 300k)
#     LIVE_M4EXIT_REVOKE_DELAY_S        seconds into the transfer before revoking (default 4)
#     LIVE_M4EXIT_REVOKE_TIMEOUT_S      bounded transfer wait before killing it (default 120)
#     LIVE_M4EXIT_REVOKE_WAIT_REREGISTER_S  >0 waits this long for the Immich poll to re-register (default 0 => note only)
#
#   Plumbing
#     LIVE_M4EXIT_CURL_TIMEOUT          per-request curl timeout seconds (default 30)
#     LIVE_M4EXIT_EVIDENCE_DIR          evidence root (default ${TMPDIR:-/tmp}/sharebridge-m4exit-e2e)
#     LIVE_M4EXIT_SSH_OPTS              extra ssh options, word-split
#     LIVE_M4EXIT_SSH_CONNECT_TIMEOUT   ssh ConnectTimeout seconds (default 8)
#     LIVE_M4EXIT_REMOTE_TIMEOUT        remote command timeout seconds (default 20)
#
# Evidence: <evidence-dir>/<run-id>/case-<name>.txt (timestamp, git sha, config
# summary, sanitised commands/output, per-check PASS/FAIL/SKIP/NOTE, the case
# result and a content hash), plus summary.txt, manifest.txt, run-metadata.txt
# and environment-facts.txt.

set -u -o pipefail

usage() {
  # Print the header comment block (everything after the shebang up to the
  # first non-comment line) so the help text cannot drift from the file.
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
    -h|--help)       usage ;;
    *) printf 'ERROR: unknown argument %s (see --help)\n' "$1" >&2; exit 2 ;;
  esac
done

# ---------------------------------------------------------------------------
# Configuration (documented env vars; no infra identifiers hard-coded)
# ---------------------------------------------------------------------------

CONTROL_BASE_URL="${LIVE_M4EXIT_CONTROL_BASE_URL:-}"
CONTROL_INSECURE_TLS="${LIVE_M4EXIT_CONTROL_INSECURE_TLS:-0}"
SHARE_CODE="${LIVE_M4EXIT_SHARE_CODE:-}"
MANIFEST_FILE="${LIVE_M4EXIT_MANIFEST_FILE:-}"

RELAY_HOST="${LIVE_M4EXIT_RELAY_HOST:-}"
ITEM_ID="${LIVE_M4EXIT_ITEM_ID:-}"
ASSET_ID="${LIVE_M4EXIT_ASSET_ID:-}"
VIDEO_ID="${LIVE_M4EXIT_VIDEO_ID:-}"
EXPECTED_ITEM_COUNT="${LIVE_M4EXIT_EXPECTED_ITEM_COUNT:-}"
EXPECTED_ASSET_BYTES="${LIVE_M4EXIT_EXPECTED_ASSET_BYTES:-}"
MIN_RANGE_REQUESTS="${LIVE_M4EXIT_MIN_RANGE_REQUESTS:-2}"
RELAY_INSECURE_TLS="${LIVE_M4EXIT_RELAY_INSECURE_TLS:-0}"

AGENT_ADMIN_BASE_URL="${LIVE_M4EXIT_AGENT_ADMIN_BASE_URL:-}"
AGENT_ADMIN_USER="${LIVE_M4EXIT_AGENT_ADMIN_USER:-}"
AGENT_ADMIN_PASSWORD="${LIVE_M4EXIT_AGENT_ADMIN_PASSWORD:-}"
AGENT_ADMIN_ORIGIN="${LIVE_M4EXIT_AGENT_ADMIN_ORIGIN:-}"
AGENT_ADMIN_INSECURE_TLS="${LIVE_M4EXIT_AGENT_ADMIN_INSECURE_TLS:-0}"

CONTROL_METRICS_URL="${LIVE_M4EXIT_CONTROL_METRICS_URL:-}"
CONTROL_SSH_HOST="${LIVE_M4EXIT_CONTROL_SSH_HOST:-}"
CONTROL_UNIT="${LIVE_M4EXIT_CONTROL_UNIT:-sharebridge.service}"
CONTROL_METRICS_ADDR="${LIVE_M4EXIT_CONTROL_METRICS_ADDR:-127.0.0.1:9102}"
STUN_JOURNAL_FILE="${LIVE_M4EXIT_STUN_JOURNAL_FILE:-}"
STUN_CADENCE_TOLERANCE_S="${LIVE_M4EXIT_STUN_CADENCE_TOLERANCE_S:-5}"
STUN_MIN_IN_BAND_DELTAS="${LIVE_M4EXIT_STUN_MIN_IN_BAND_DELTAS:-1}"

GATEWAY_HEALTH_URL="${LIVE_M4EXIT_GATEWAY_HEALTH_URL:-}"
GATEWAY_METRICS_URL="${LIVE_M4EXIT_GATEWAY_METRICS_URL:-}"
GATEWAY_SSH_HOST="${LIVE_M4EXIT_GATEWAY_SSH_HOST:-${CONTROL_SSH_HOST}}"
GATEWAY_METRICS_ADDR="${LIVE_M4EXIT_GATEWAY_METRICS_ADDR:-127.0.0.1:9101}"
GATEWAY_UNIT="${LIVE_M4EXIT_GATEWAY_UNIT:-sharebridge-relay-gateway.service}"
GATEWAY_JOURNAL_FILE="${LIVE_M4EXIT_GATEWAY_JOURNAL_FILE:-}"
FRPS_UNIT="${LIVE_M4EXIT_FRPS_UNIT:-sharebridge-relay-frps.service}"
FRPS_JOURNAL_FILE="${LIVE_M4EXIT_FRPS_JOURNAL_FILE:-}"
FRPS_SSH_HOST="${LIVE_M4EXIT_FRPS_SSH_HOST:-${GATEWAY_SSH_HOST}}"
JOURNAL_MAX_LINES="${LIVE_M4EXIT_JOURNAL_MAX_LINES:-5000}"

AGENT_LOG_FILE="${LIVE_M4EXIT_AGENT_LOG_FILE:-}"
AGENT_SSH_HOST="${LIVE_M4EXIT_AGENT_SSH_HOST:-}"
AGENT_JOURNAL_UNIT="${LIVE_M4EXIT_AGENT_JOURNAL_UNIT:-sharebridge-agent}"
EXPECTED_SHARE_COUNT="${LIVE_M4EXIT_EXPECTED_SHARE_COUNT:-}"
EXPECTED_HYDRATED_SESSIONS="${LIVE_M4EXIT_EXPECTED_HYDRATED_SESSIONS:-1}"

ALLOW_AGENT_RESTART="${LIVE_M4EXIT_ALLOW_AGENT_RESTART:-0}"
AGENT_RESTART_COMMAND="${LIVE_M4EXIT_AGENT_RESTART_COMMAND:-}"
AGENT_RESTART_WAIT_S="${LIVE_M4EXIT_AGENT_RESTART_WAIT_S:-30}"

AGENT_RECORD_FILE="${LIVE_M4EXIT_AGENT_RECORD_FILE:-}"
EXPECTED_DIRECT_REASON="${LIVE_M4EXIT_EXPECTED_DIRECT_REASON:-probe_failed}"

ALLOW_LOCKDOWN="${LIVE_M4EXIT_ALLOW_LOCKDOWN:-0}"
RECOVERY_BOUND_S="${LIVE_M4EXIT_RECOVERY_BOUND_S:-120}"
TUNNEL_OFFLINE_BOUND_S="${LIVE_M4EXIT_TUNNEL_OFFLINE_BOUND_S:-30}"

ALLOW_REVOKE="${LIVE_M4EXIT_ALLOW_REVOKE:-0}"
REVOKE_SHARE_CODE="${LIVE_M4EXIT_REVOKE_SHARE_CODE:-}"
REVOKE_ASSET_ID="${LIVE_M4EXIT_REVOKE_ASSET_ID:-}"
REVOKE_ASSET_BYTES="${LIVE_M4EXIT_REVOKE_ASSET_BYTES:-}"
REVOKE_LIMIT_RATE="${LIVE_M4EXIT_REVOKE_LIMIT_RATE:-300k}"
REVOKE_DELAY_S="${LIVE_M4EXIT_REVOKE_DELAY_S:-4}"
REVOKE_TIMEOUT_S="${LIVE_M4EXIT_REVOKE_TIMEOUT_S:-120}"
REVOKE_WAIT_REREGISTER_S="${LIVE_M4EXIT_REVOKE_WAIT_REREGISTER_S:-0}"

CURL_TIMEOUT="${LIVE_M4EXIT_CURL_TIMEOUT:-30}"
DEFAULT_EVIDENCE_ROOT="${TMPDIR:-/tmp}/sharebridge-m4exit-e2e"
EVIDENCE_DIR="${EVIDENCE_DIR_OVERRIDE:-${LIVE_M4EXIT_EVIDENCE_DIR:-$DEFAULT_EVIDENCE_ROOT}}"
SSH_CONNECT_TIMEOUT="${LIVE_M4EXIT_SSH_CONNECT_TIMEOUT:-8}"
REMOTE_TIMEOUT="${LIVE_M4EXIT_REMOTE_TIMEOUT:-20}"

SSH_EXTRA=()
if [[ -n "${LIVE_M4EXIT_SSH_OPTS:-}" ]]; then
  read -r -a SSH_EXTRA <<< "${LIVE_M4EXIT_SSH_OPTS}"
fi

if [[ -z "$AGENT_ADMIN_ORIGIN" && -n "$AGENT_ADMIN_BASE_URL" ]]; then
  AGENT_ADMIN_ORIGIN="$AGENT_ADMIN_BASE_URL"
fi

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SCRIPT_SELF="${SCRIPT_DIR}/$(basename -- "${BASH_SOURCE[0]}")"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

GIT_SHA="$(git -C "$REPO_ROOT" rev-parse --short=12 HEAD 2>/dev/null || echo unknown)"
RUN_ID="$(date -u +%Y%m%dT%H%M%SZ)"
RUN_DIR="${EVIDENCE_DIR}/${RUN_ID}"
uname_s="$(uname -s 2>/dev/null || echo unknown)"

# TLS argument arrays (empty unless the operator opts in).
CONTROL_TLS_ARGS=()
[[ "$CONTROL_INSECURE_TLS" == "1" ]] && CONTROL_TLS_ARGS=(-k)
RELAY_TLS_ARGS=()
[[ "$RELAY_INSECURE_TLS" == "1" ]] && RELAY_TLS_ARGS=(-k)
AGENT_TLS_ARGS=()
[[ "$AGENT_ADMIN_INSECURE_TLS" == "1" ]] && AGENT_TLS_ARGS=(-k)

# ---------------------------------------------------------------------------
# Case registry — ONE line per case.
# ---------------------------------------------------------------------------

CASE_TABLE=(
  "enrollment_hydration_restart|task #15|agent enrollment live; registered share count == declared; restart hydration observed from the agent log; per-share content resolves over the relay after the restart"
  "relay_content_integrity|task #15|full recipient relay path: interstitial -> prepare-route status=relay -> gallery/items/thumb/full asset (sha1 + byte count) -> playback HEAD/200/Range 206 byte-exact seeks + the three 416 cases"
  "stun_observe_and_rechallenge|task #15|control STUN counters match>0 with zero mismatch/timeout; §10.2 proactive rechallenge cadence (4m + jitter[0,15s)) measured from accept timestamps"
  "direct_path_or_failclosed|task #15|either status=direct with a serving direct URL, or a fail-closed relay fallback with direct_status=relay_fallback plus the expected reason; never PASS when neither is observable"
  "lockdown_withdrawal_and_recovery|task #15|OPT-IN: lockdown -> tunnel offline + frps proxy close + prepare-route suppressed + relay yields no content -> unlock -> recovery within bound (measured)"
  "revocation_midstream|task #15|OPT-IN: throttled in-flight download -> DELETE share mid-transfer -> transfer truncated + gateway route-revocation drain (streams>=1); records the Immich re-registration caveat"
)

case_names=()
case_owners=()
case_purposes=()
for entry in "${CASE_TABLE[@]}"; do
  case_names+=("$(printf '%s' "$entry" | cut -d'|' -f1)")
  case_owners+=("$(printf '%s' "$entry" | cut -d'|' -f2)")
  case_purposes+=("$(printf '%s' "$entry" | cut -d'|' -f3-)")
done

case_index_of() {
  local want="$1" i
  for i in "${!case_names[@]}"; do
    [[ "${case_names[$i]}" == "$want" ]] && { printf '%s' "$i"; return 0; }
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
    case_index_of "$want" >/dev/null || {
      printf 'ERROR: unknown case %s\n' "$want" >&2
      printf 'Registered cases:\n' >&2
      for n in "${case_names[@]}"; do printf '  %s\n' "$n" >&2; done
      exit 2
    }
  done
fi

# ---------------------------------------------------------------------------
# Manifest loading (optional; explicit env vars always win)
# ---------------------------------------------------------------------------

python_ok() { command -v python3 >/dev/null 2>&1; }

if [[ -n "$MANIFEST_FILE" ]]; then
  if [[ ! -r "$MANIFEST_FILE" ]]; then
    printf 'ERROR: LIVE_M4EXIT_MANIFEST_FILE=%s is not readable\n' "$MANIFEST_FILE" >&2
    exit 2
  fi
  if ! python_ok; then
    printf 'ERROR: python3 is required to read LIVE_M4EXIT_MANIFEST_FILE\n' >&2
    exit 2
  fi
  manifest_value() {
    python3 -c 'import json,sys
try:
    data = json.load(open(sys.argv[1]))
except Exception as exc:
    sys.stderr.write("manifest parse failed: %s\n" % exc); sys.exit(1)
value = data.get(sys.argv[2], "")
if value is None: value = ""
print(value)' "$MANIFEST_FILE" "$1"
  }
  [[ -z "$SHARE_CODE" ]]          && SHARE_CODE="$(manifest_value share_code)"
  [[ -z "$ITEM_ID" ]]             && ITEM_ID="$(manifest_value item_id)"
  [[ -z "$ASSET_ID" ]]            && ASSET_ID="$(manifest_value asset_id)"
  [[ -z "$VIDEO_ID" ]]            && VIDEO_ID="$(manifest_value video_id)"
  [[ -z "$EXPECTED_ITEM_COUNT" ]] && EXPECTED_ITEM_COUNT="$(manifest_value expected_item_count)"
  [[ -z "$EXPECTED_ASSET_BYTES" ]] && EXPECTED_ASSET_BYTES="$(manifest_value asset_bytes)"
  [[ -z "$REVOKE_SHARE_CODE" ]]   && REVOKE_SHARE_CODE="$(manifest_value revoke_share_code)"
  [[ -z "$REVOKE_ASSET_ID" ]]     && REVOKE_ASSET_ID="$(manifest_value revoke_asset_id)"
fi

# ---------------------------------------------------------------------------
# Sanitising
# ---------------------------------------------------------------------------

# Literal needles (configured secrets/share codes) are replaced verbatim; the
# generic pass then catches token/key/password-shaped text and /s/<code> paths.
SANITIZE_LITERALS=""
SANITIZE_SEP=$'\x1f'
add_secret_literal() {
  [[ -n "$1" ]] || return 0
  if [[ -z "$SANITIZE_LITERALS" ]]; then
    SANITIZE_LITERALS="$1"
  else
    SANITIZE_LITERALS="${SANITIZE_LITERALS}${SANITIZE_SEP}${1}"
  fi
}
add_secret_literal "$AGENT_ADMIN_PASSWORD"
add_secret_literal "$SHARE_CODE"
add_secret_literal "$REVOKE_SHARE_CODE"

sanitize() {
  # Literal needles are passed through the environment (not `awk -v`) so that
  # backslashes in a password are never interpreted as awk escapes.
  M4EXIT_SANITIZE_LITERALS="$SANITIZE_LITERALS" awk '
    function repl(s, n,   out, i, l) {
      l = length(n); out = ""
      while ((i = index(s, n)) > 0) { out = out substr(s, 1, i - 1) "[REDACTED]"; s = substr(s, i + l) }
      return out s
    }
    BEGIN { sep = sprintf("%c", 31); cnt = split(ENVIRON["M4EXIT_SANITIZE_LITERALS"], arr, sep) }
    /BEGIN [A-Z ]*PRIVATE KEY/ { print "[REDACTED: private key material]"; inkey = 1; next }
    inkey && /^[A-Za-z0-9+\/=]+$/ { next }
    {
      inkey = 0
      for (k = 1; k <= cnt; k++) if (arr[k] != "") $0 = repl($0, arr[k])
      print
    }
  ' | sed -E \
    -e "s#//[^/@[:space:]]+:[^/@[:space:]]+@#//[REDACTED]@#g" \
    -e "s#([Aa][Pp][Ii][_-]?[Kk][Ee][Yy]|[Aa]uthorization|[Bb]earer|[Pp]assword|[Ss]ecret|[Cc]ookie|[Tt]oken)([[:space:]]*[=:][[:space:]]*|[[:space:]]+)[^[:space:],;\"']+#\1=[REDACTED]#g" \
    -e "s#([^A-Za-z0-9]|^)(jti|JTI)([=:][[:space:]]*)?[A-Za-z0-9._-]{8,}#\1\2=[REDACTED]#g" \
    -e "s#/s/[A-Za-z0-9_-]{6,}#/s/[REDACTED-SHARE-CODE]#g"
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

sha1_file() {
  if command -v shasum >/dev/null 2>&1; then
    shasum -a 1 "$1" | awk '{print $1}'
  elif command -v sha1sum >/dev/null 2>&1; then
    sha1sum "$1" | awk '{print $1}'
  elif command -v openssl >/dev/null 2>&1; then
    openssl dgst -sha1 "$1" | awk '{print $NF}'
  else
    printf 'unavailable'
  fi
}

file_bytes() { wc -c < "$1" | tr -d ' '; }

# ---------------------------------------------------------------------------
# Check plumbing (mirrors scripts/live-phase4a.sh)
# ---------------------------------------------------------------------------

log() { printf '%s\n' "$*" >&2; }

check() {
  # check <check-name> <PASS|FAIL|SKIP|NOTE> <observed...>
  local cname="$1" verdict="$2"
  shift 2
  local detail
  detail="$(printf '%s' "$*" | sanitize)"
  if [[ -z "${GATE_CHECK_FILE:-}" ]]; then
    printf 'ERROR: check() called outside the case runner\n' >&2
    return 1
  fi
  printf '%s|%s|%s\n' "$verdict" "$cname" "$detail" >> "$GATE_CHECK_FILE"
  printf '  [%s] %s: %s\n' "$verdict" "$cname" "$detail"
}

note() { check "$1" NOTE "$2"; }

record_cmd() {
  [[ -n "${GATE_CMD_FILE:-}" ]] || return 0
  printf '%s\n' "$1" | sanitize >> "$GATE_CMD_FILE"
}

record_out() {
  [[ -n "${GATE_OUT_FILE:-}" ]] || return 0
  printf '%s\n' "$1" | sanitize >> "$GATE_OUT_FILE"
}

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

# need_cfg: fail the named check when a required env input is missing.
need_cfg() {
  local cname="$1" var="$2" value="$3" desc="$4"
  if [[ -z "$value" ]]; then
    check "$cname" FAIL "$var is unset — cannot verify: $desc"
    return 1
  fi
  return 0
}

# require_config: fail one config_<VAR> check per missing required input.
# Accepts arguments of the form "VAR=value|description".
require_config() {
  local entry rest var value desc missing=0
  for entry in "$@"; do
    var="${entry%%=*}"
    rest="${entry#*=}"
    desc="${rest#*|}"
    value="${rest%%|*}"
    if [[ -z "$value" ]]; then
      check "config_${var}" FAIL "${var} is unset — cannot verify: ${desc}"
      missing=1
    fi
  done
  return "$missing"
}

# ---------------------------------------------------------------------------
# Command runners
# ---------------------------------------------------------------------------

remote_exec() {
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
  local cmd="$1" out status
  record_cmd "sh -c :: ${cmd}"
  out="$(sh -c "$cmd" 2>&1)"
  status=$?
  record_out "--- local exit=${status} ---"$'\n'"${out}"
  printf '%s\n' "$out"
  return "$status"
}

# HTTP helpers: curl status in HTTP_CODE, body in HTTP_BODY_FILE, headers in
# HTTP_HDR_FILE, stderr in HTTP_ERR.
HTTP_CODE=""
HTTP_BODY_FILE=""
HTTP_HDR_FILE=""
HTTP_ERR=""

http_fetch() {
  # http_fetch <curl args...> ; the last argument is the URL
  local err
  err="$(mktemp "${TMPDIR:-/tmp}/m4exit-curl.XXXXXX")"
  HTTP_BODY_FILE="$(mktemp "${TMPDIR:-/tmp}/m4exit-body.XXXXXX")"
  HTTP_HDR_FILE="$(mktemp "${TMPDIR:-/tmp}/m4exit-hdr.XXXXXX")"
  HTTP_CODE="$(curl --silent --show-error --max-time "$CURL_TIMEOUT" \
    -D "$HTTP_HDR_FILE" -o "$HTTP_BODY_FILE" -w '%{http_code}' "$@" 2>"$err")"
  HTTP_ERR="$(cat "$err")"
  rm -f "$err"
  [[ -n "$HTTP_ERR" ]] && record_out "curl stderr: $(printf '%s' "$HTTP_ERR" | tr '\n' ' ' | cut -c1-300)"
  return 0
}

http_head() {
  local err
  err="$(mktemp "${TMPDIR:-/tmp}/m4exit-curl.XXXXXX")"
  HTTP_BODY_FILE="$(mktemp "${TMPDIR:-/tmp}/m4exit-body.XXXXXX")"
  HTTP_HDR_FILE="$(mktemp "${TMPDIR:-/tmp}/m4exit-hdr.XXXXXX")"
  HTTP_CODE="$(curl --silent --show-error --max-time "$CURL_TIMEOUT" \
    --head -D "$HTTP_HDR_FILE" -o "$HTTP_BODY_FILE" -w '%{http_code}' "$@" 2>"$err")"
  HTTP_ERR="$(cat "$err")"
  rm -f "$err"
  [[ -n "$HTTP_ERR" ]] && record_out "curl stderr: $(printf '%s' "$HTTP_ERR" | tr '\n' ' ' | cut -c1-300)"
  return 0
}

header_value() {
  # header_value <name> ; reads HTTP_HDR_FILE
  [[ -n "${HTTP_HDR_FILE:-}" && -r "$HTTP_HDR_FILE" ]] || return 0
  awk -v n="$1" 'BEGIN { FS = ": *" } tolower($1) == tolower(n) { v = $2; sub(/\r$/, "", v); print v }' "$HTTP_HDR_FILE" | tail -n1
}

json_str() {
  # json_str <json> <field>  (flat string fields only)
  printf '%s\n' "$1" | sed -n "s/.*\"$2\"[[:space:]]*:[[:space:]]*\"\([^\"]*\)\".*/\1/p" | head -n1
}

json_bool() {
  # json_bool <json> <field>
  printf '%s\n' "$1" | grep -o "\"$2\"[[:space:]]*:[[:space:]]*[a-z]*" | head -n1 | sed 's/.*:[[:space:]]*//'
}

url_host() {
  # url_host <url> -> host[:port] without the scheme/path
  local u="$1"
  u="${u#*://}"
  u="${u%%/*}"
  printf '%s' "$u"
}

url_origin() {
  # url_origin <url> -> scheme://host[:port]
  local u="$1" scheme path
  scheme="$(printf '%s' "$u" | sed -n 's#^\([a-zA-Z][a-zA-Z0-9+.-]*://\).*#\1#p')"
  path="${u#*://}"
  printf '%s%s' "$scheme" "${path%%/*}"
}

# JSON item helpers (require python3; callers fail closed when it is absent).
json_item_count() {
  python3 -c 'import json,sys
print(len(json.load(open(sys.argv[1])).get("items", [])))' "$1"
}

json_item_field() {
  # json_item_field <file> <item-id> <field>
  python3 -c 'import json,sys
data = json.load(open(sys.argv[1]))
for it in data.get("items", []):
    if it.get("id") == sys.argv[2]:
        print(it.get(sys.argv[3], ""))
        break' "$1" "$2" "$3"
}

# ---------------------------------------------------------------------------
# Metrics / journals
# ---------------------------------------------------------------------------

metric_value() {
  # metric_value <metrics-text> <exact-metric-with-labels>
  printf '%s\n' "$1" | grep -F "$2" | tail -n1 | awk '{print $NF}'
}

fetch_control_metrics() {
  CONTROL_METRICS_TEXT=""
  if [[ -n "$CONTROL_METRICS_URL" ]]; then
    record_cmd "curl ${CONTROL_METRICS_URL} (control /metrics)"
    http_fetch ${CONTROL_TLS_ARGS[@]+"${CONTROL_TLS_ARGS[@]}"} "$CONTROL_METRICS_URL"
    CONTROL_METRICS_TEXT="$(cat "$HTTP_BODY_FILE")"
    return 0
  fi
  if [[ -n "$CONTROL_SSH_HOST" ]]; then
    CONTROL_METRICS_TEXT="$(remote_exec "$CONTROL_SSH_HOST" "curl -fsS --max-time 5 'http://${CONTROL_METRICS_ADDR}/metrics'")"
    return 0
  fi
  return 1
}

fetch_gateway_health() {
  GATEWAY_HEALTH_TEXT=""
  if [[ -n "$GATEWAY_HEALTH_URL" ]]; then
    record_cmd "curl ${GATEWAY_HEALTH_URL} (gateway /healthz)"
    http_fetch "$GATEWAY_HEALTH_URL"
    GATEWAY_HEALTH_TEXT="$(cat "$HTTP_BODY_FILE")"
    return 0
  fi
  if [[ -n "$GATEWAY_SSH_HOST" ]]; then
    GATEWAY_HEALTH_TEXT="$(remote_exec "$GATEWAY_SSH_HOST" "curl -fsS --max-time 5 'http://${GATEWAY_METRICS_ADDR}/healthz'")"
    return 0
  fi
  return 1
}

fetch_gateway_metrics() {
  GATEWAY_METRICS_TEXT=""
  if [[ -n "$GATEWAY_METRICS_URL" ]]; then
    record_cmd "curl ${GATEWAY_METRICS_URL} (gateway /metrics)"
    http_fetch "$GATEWAY_METRICS_URL"
    GATEWAY_METRICS_TEXT="$(cat "$HTTP_BODY_FILE")"
    return 0
  fi
  if [[ -n "$GATEWAY_SSH_HOST" ]]; then
    GATEWAY_METRICS_TEXT="$(remote_exec "$GATEWAY_SSH_HOST" "curl -fsS --max-time 5 'http://${GATEWAY_METRICS_ADDR}/metrics'")"
    return 0
  fi
  return 1
}

gateway_tunnel_online() {
  # echoes the online gauge value (or empty when unobservable)
  fetch_gateway_metrics >/dev/null 2>&1 || return 1
  metric_value "$GATEWAY_METRICS_TEXT" 'sharebridge_relay_tunnel_state{state="online"}'
}

remote_journal() {
  # remote_journal <ssh-host> <unit> <fixed-grep-pattern>
  remote_exec "$1" "journalctl -u '$2' --no-pager -o short-iso -n ${JOURNAL_MAX_LINES} 2>/dev/null | grep -F '$3' || true"
}

gateway_journal_grep() {
  # gateway_journal_grep <fixed-pattern-or-empty-for-whole-journal>
  local pat="$1"
  if [[ -n "$GATEWAY_JOURNAL_FILE" ]]; then
    if [[ -r "$GATEWAY_JOURNAL_FILE" ]]; then
      if [[ -n "$pat" ]]; then grep -F "$pat" "$GATEWAY_JOURNAL_FILE" || true; else cat "$GATEWAY_JOURNAL_FILE"; fi
    fi
    return 0
  fi
  if [[ -n "$GATEWAY_SSH_HOST" ]]; then
    if [[ -n "$pat" ]]; then remote_journal "$GATEWAY_SSH_HOST" "$GATEWAY_UNIT" "$pat"; else remote_exec "$GATEWAY_SSH_HOST" "journalctl -u '$GATEWAY_UNIT' --no-pager -o short-iso -n ${JOURNAL_MAX_LINES} 2>/dev/null || true"; fi
    return 0
  fi
  return 1
}

frps_journal_grep() {
  local pat="$1"
  if [[ -n "$FRPS_JOURNAL_FILE" ]]; then
    if [[ -r "$FRPS_JOURNAL_FILE" ]]; then
      if [[ -n "$pat" ]]; then grep -F "$pat" "$FRPS_JOURNAL_FILE" || true; else cat "$FRPS_JOURNAL_FILE"; fi
    fi
    return 0
  fi
  if [[ -n "$FRPS_SSH_HOST" ]]; then
    if [[ -n "$pat" ]]; then remote_journal "$FRPS_SSH_HOST" "$FRPS_UNIT" "$pat"; else remote_exec "$FRPS_SSH_HOST" "journalctl -u '$FRPS_UNIT' --no-pager -o short-iso -n ${JOURNAL_MAX_LINES} 2>/dev/null || true"; fi
    return 0
  fi
  return 1
}

agent_log_grep() {
  # agent_log_grep <extended-regex>
  if [[ -n "$AGENT_LOG_FILE" ]]; then
    [[ -r "$AGENT_LOG_FILE" ]] || return 1
    grep -E "$1" "$AGENT_LOG_FILE" || true
    return 0
  fi
  if [[ -n "$AGENT_SSH_HOST" ]]; then
    remote_exec "$AGENT_SSH_HOST" "journalctl -u '$AGENT_JOURNAL_UNIT' --no-pager -o short-iso -n ${JOURNAL_MAX_LINES} 2>/dev/null | grep -E '$1' || true"
    return 0
  fi
  if command -v journalctl >/dev/null 2>&1; then
    journalctl -u "$AGENT_JOURNAL_UNIT" --no-pager -o short-iso -n "$JOURNAL_MAX_LINES" 2>/dev/null | grep -E "$1" || true
    return 0
  fi
  return 1
}

agent_log_source() {
  if [[ -n "$AGENT_LOG_FILE" ]]; then printf '%s' "$AGENT_LOG_FILE"
  elif [[ -n "$AGENT_SSH_HOST" ]]; then printf 'journalctl %s@%s' "$AGENT_JOURNAL_UNIT" "$AGENT_SSH_HOST"
  elif command -v journalctl >/dev/null 2>&1; then printf 'local journalctl -u %s' "$AGENT_JOURNAL_UNIT"
  else printf 'unavailable (set LIVE_M4EXIT_AGENT_LOG_FILE or LIVE_M4EXIT_AGENT_SSH_HOST)'
  fi
}

gateway_journal_available() {
  [[ -n "$GATEWAY_JOURNAL_FILE" || -n "$GATEWAY_SSH_HOST" ]]
}

frps_journal_available() {
  [[ -n "$FRPS_JOURNAL_FILE" || -n "$FRPS_SSH_HOST" ]]
}

# ---------------------------------------------------------------------------
# Agent admin API + prepare-route
# ---------------------------------------------------------------------------

agent_api() {
  # agent_api <method> <path> [data] ; sets AGENT_HTTP_CODE / AGENT_BODY_FILE
  local method="$1" path="$2" data="${3:-}"
  local url="${AGENT_ADMIN_BASE_URL%/}${path}"
  local -a args=(-X "$method" -H "Origin: ${AGENT_ADMIN_ORIGIN}" -H "X-Requested-With: XMLHttpRequest")
  if [[ -n "$AGENT_ADMIN_USER" ]]; then
    args+=(-u "${AGENT_ADMIN_USER}:${AGENT_ADMIN_PASSWORD}")
  fi
  args+=(${AGENT_TLS_ARGS[@]+"${AGENT_TLS_ARGS[@]}"})
  if [[ -n "$data" ]]; then args+=(--data "$data"); fi
  record_cmd "curl -X ${method} ${url} (Origin: ${AGENT_ADMIN_ORIGIN}, X-Requested-With: XMLHttpRequest, Basic auth [REDACTED], insecure_tls=${AGENT_ADMIN_INSECURE_TLS})"
  http_fetch "${args[@]}" "$url"
  AGENT_HTTP_CODE="$HTTP_CODE"
  AGENT_BODY_FILE="$HTTP_BODY_FILE"
  AGENT_HDR_FILE="$HTTP_HDR_FILE"
}

prepare_route() {
  # prepare_route <share-code> ; sets PREPARE_HTTP_CODE / PREPARE_STATUS /
  # PREPARE_RELAY_URL / PREPARE_DIRECT_URL / PREPARE_BODY_FILE
  local code="$1"
  local url="${CONTROL_BASE_URL%/}/api/shares/${code}/prepare-route"
  record_cmd "curl -X POST ${url} (interstitial prepare-route)"
  http_fetch -X POST ${CONTROL_TLS_ARGS[@]+"${CONTROL_TLS_ARGS[@]}"} "$url"
  PREPARE_HTTP_CODE="$HTTP_CODE"
  PREPARE_BODY_FILE="$HTTP_BODY_FILE"
  local body
  body="$(cat "$HTTP_BODY_FILE")"
  PREPARE_STATUS="$(json_str "$body" status)"
  PREPARE_RELAY_URL="$(json_str "$body" relay_url)"
  PREPARE_DIRECT_URL="$(json_str "$body" direct_url)"
}

relay_host_matches() {
  local host="$1"
  [[ -z "$RELAY_HOST" ]] && return 1
  [[ "$host" == "$RELAY_HOST" ]] && return 0
  case "$host" in
    *".${RELAY_HOST}") return 0 ;;
    *) return 1 ;;
  esac
}

prepare_relay_or_fail() {
  # prepare_relay_or_fail <code> <check-name-prefix> ; returns 0 when a relay URL is available
  local code="$1" prefix="$2"
  prepare_route "$code"
  if [[ "$PREPARE_HTTP_CODE" != "200" || "$PREPARE_STATUS" != "relay" || -z "$PREPARE_RELAY_URL" ]]; then
    check "${prefix}_prepare_status" FAIL "prepare-route -> ${PREPARE_HTTP_CODE} status='${PREPARE_STATUS:-<none>}' relay_url='${PREPARE_RELAY_URL:+present}' (body: $(sanitize < "$PREPARE_BODY_FILE" | tr -d '\n' | cut -c1-200))"
    return 1
  fi
  check "${prefix}_prepare_status" PASS "prepare-route -> 200 status=relay"
  local rhost
  rhost="$(url_host "$PREPARE_RELAY_URL")"
  if [[ -z "$RELAY_HOST" ]]; then
    check "${prefix}_relay_url" FAIL "LIVE_M4EXIT_RELAY_HOST unset — cannot verify the relay URL host '${rhost}'"
    return 1
  fi
  if relay_host_matches "$rhost"; then
    check "${prefix}_relay_url" PASS "relay_url host=${rhost} belongs to ${RELAY_HOST}"
  else
    check "${prefix}_relay_url" FAIL "relay_url host=${rhost} is not ${RELAY_HOST} or a subdomain of it"
    return 1
  fi
  return 0
}

# ===========================================================================
# Lockdown safety net (shared by the main process trap and case 5)
# ===========================================================================
#
# The lockdown case runs inside `( ... )` (see execute_case), and bash resets
# traps in subshells, so a flag set inside the case can never be read by a
# trap registered in the main process. It is also why the original
# `local locked=0` was unbound in the EXIT trap under `set -u`: the case's own
# function scope had already ended. The locked state therefore lives in a
# small state FILE that the case writes/removes, and the MAIN process owns the
# trap. That survives a normal EXIT, an `exit` from any code path, and INT or
# TERM sent to the harness PID (including a Ctrl-C delivered to the process
# group).

M4EXIT_LOCK_STATE=""

m4exit_lock_state_path() {
  # Resolve the per-run state-file path exactly once (in the main process, so
  # the case's subshell inherits the same path). The file only exists while the
  # harness believes the agent is locked.
  if [[ -z "${M4EXIT_LOCK_STATE:-}" ]]; then
    M4EXIT_LOCK_STATE="$(mktemp "${TMPDIR:-/tmp}/m4exit-lockdown.XXXXXX")"
    rm -f "$M4EXIT_LOCK_STATE"
  fi
  printf '%s' "$M4EXIT_LOCK_STATE"
}

m4exit_mark_locked() {
  printf 'locked\n' > "$(m4exit_lock_state_path)"
}

m4exit_mark_unlocked() {
  if [[ -n "${M4EXIT_LOCK_STATE:-}" ]]; then rm -f "$M4EXIT_LOCK_STATE"; fi
  return 0
}

m4exit_lockdown_maybe_active() {
  [[ -n "${M4EXIT_LOCK_STATE:-}" && -f "${M4EXIT_LOCK_STATE:-/nonexistent}" ]]
}

m4exit_emergency_unlock() {
  # Trap body: re-send POST /api/unlock whenever the harness still believes the
  # agent is locked. Never masks the original exit status (always returns 0)
  # and is defensive under `set -u` (the state global may be unset).
  if ! m4exit_lockdown_maybe_active; then
    return 0
  fi
  local rc=0 code=""
  if declare -f agent_api >/dev/null 2>&1; then
    agent_api POST "/api/unlock" >/dev/null 2>&1 || rc=$?
    code="${AGENT_HTTP_CODE:-}"
  else
    rc=127
  fi
  if [[ "$rc" -eq 0 && ( "$code" == "200" || "$code" == "204" ) ]]; then
    m4exit_mark_unlocked
    log "EMERGENCY UNLOCK: POST /api/unlock -> ${code} — the agent is no longer locked"
  else
    log "EMERGENCY UNLOCK FAILED: $(printf 'POST /api/unlock rc=%s http=%s via %s' "$rc" "${code:-<none>}" "${AGENT_ADMIN_BASE_URL:-<unset>}" | sanitize) — the agent may STILL be locked; unlock it by hand"
  fi
  return 0
}

m4exit_install_lockdown_trap() {
  # Main-process trap installation (live runs only). INT/TERM unlock first and
  # then exit with the conventional 128+signal status; the EXIT trap then runs
  # again but finds the state already cleared, so exactly one unlock is issued.
  m4exit_lock_state_path >/dev/null
  trap 'm4exit_emergency_unlock' EXIT
  trap 'm4exit_emergency_unlock; exit 130' INT
  trap 'm4exit_emergency_unlock; exit 143' TERM
}

# ===========================================================================
# CASE: enrollment_hydration_restart
# ===========================================================================

enrollment_hydration_restart() {
  # (1) Agent admin reachable and the registered share count == declared.
  if [[ -z "$AGENT_ADMIN_BASE_URL" ]]; then
    check enrollment_agent_admin_reachable FAIL "LIVE_M4EXIT_AGENT_ADMIN_BASE_URL is unset — cannot read the agent's registered shares"
  elif [[ -z "$AGENT_ADMIN_USER" || -z "$AGENT_ADMIN_PASSWORD" ]]; then
    check enrollment_agent_admin_reachable FAIL "LIVE_M4EXIT_AGENT_ADMIN_USER / _PASSWORD is unset — the agent admin API requires Basic auth"
  else
    agent_api GET "/api/shares"
    if [[ "$AGENT_HTTP_CODE" == "200" ]]; then
      check enrollment_agent_admin_reachable PASS "GET ${AGENT_ADMIN_BASE_URL}/api/shares -> 200"
      local count
      count="$(grep -o 'class="share-card"' "$AGENT_BODY_FILE" 2>/dev/null | wc -l | tr -d ' ')"
      count="${count:-0}"
      set_fact enrollment_registered_shares "$count"
      if [[ -z "$EXPECTED_SHARE_COUNT" ]]; then
        check enrollment_share_count FAIL "LIVE_M4EXIT_EXPECTED_SHARE_COUNT is unset — cannot compare the registered share count (observed ${count})"
      elif [[ "$count" == "$EXPECTED_SHARE_COUNT" ]]; then
        check enrollment_share_count PASS "registered shares=${count} matches the declared count"
      else
        check enrollment_share_count FAIL "registered shares=${count}, declared=${EXPECTED_SHARE_COUNT}"
      fi
    else
      check enrollment_agent_admin_reachable FAIL "GET ${AGENT_ADMIN_BASE_URL}/api/shares -> ${AGENT_HTTP_CODE:-000} ${HTTP_ERR:-}"
      check enrollment_share_count FAIL "not evaluated: the agent admin share list failed (${AGENT_HTTP_CODE:-000})"
    fi
  fi

  # (2) Enrollment is live on the relay side: route_ready + tunnel online.
  if fetch_gateway_health >/dev/null 2>&1 && [[ -n "$GATEWAY_HEALTH_TEXT" ]]; then
    local rr
    rr="$(json_bool "$GATEWAY_HEALTH_TEXT" route_ready)"
    if [[ "$rr" == "true" ]]; then
      check enrollment_route_ready PASS "gateway /healthz route_ready=true"
    else
      check enrollment_route_ready FAIL "gateway /healthz route_ready=${rr:-<absent>} (raw: $(printf '%s' "$GATEWAY_HEALTH_TEXT" | tr -d '\n' | cut -c1-160))"
    fi
  else
    check enrollment_route_ready FAIL "no gateway health access: set LIVE_M4EXIT_GATEWAY_HEALTH_URL or LIVE_M4EXIT_GATEWAY_SSH_HOST"
  fi
  if fetch_gateway_metrics >/dev/null 2>&1 && [[ -n "$GATEWAY_METRICS_TEXT" ]]; then
    local online
    online="$(metric_value "$GATEWAY_METRICS_TEXT" 'sharebridge_relay_tunnel_state{state="online"}')"
    set_fact gateway_tunnel_online "$online"
    if [[ "${online:-0}" -ge 1 ]]; then
      check enrollment_tunnel_online PASS "sharebridge_relay_tunnel_state{state=\"online\"}=${online}"
    else
      check enrollment_tunnel_online FAIL "sharebridge_relay_tunnel_state{state=\"online\"}=${online:-<absent>} (want >=1)"
    fi
  else
    check enrollment_tunnel_online FAIL "no gateway metrics access: set LIVE_M4EXIT_GATEWAY_METRICS_URL or LIVE_M4EXIT_GATEWAY_SSH_HOST"
  fi

  # (3) Restart hydration observed in the agent log. When the operator opts in,
  # first perform an explicit restart so the line is freshly observable. The
  # restart is ONLY an observability action: it can never substitute for the
  # real hydration line, so a genuine hydration failure still fails closed.
  local line n restart_action="no restart requested"
  if [[ "$ALLOW_AGENT_RESTART" == "1" ]]; then
    if [[ -z "$AGENT_RESTART_COMMAND" ]]; then
      check hydration_restart_action FAIL "LIVE_M4EXIT_ALLOW_AGENT_RESTART=1 requires LIVE_M4EXIT_AGENT_RESTART_COMMAND — set it to the exact agent restart command (e.g. 'systemctl restart sharebridge-agent')"
      restart_action="opted-in restart not executed (no command)"
    else
      local restart_out="" restart_rc=0
      if [[ -n "$AGENT_SSH_HOST" ]]; then
        restart_out="$(remote_exec "$AGENT_SSH_HOST" "$AGENT_RESTART_COMMAND")" || restart_rc=$?
      else
        restart_out="$(local_exec "$AGENT_RESTART_COMMAND")" || restart_rc=$?
      fi
      if [[ "$restart_rc" -eq 0 ]]; then
        check hydration_restart_action PASS "opted-in agent restart executed (${AGENT_SSH_HOST:+on ${AGENT_SSH_HOST} }rc=0)"
        restart_action="opted-in restart executed (rc=0)"
      else
        check hydration_restart_action FAIL "opted-in agent restart exited rc=${restart_rc}: $(printf '%s' "$restart_out" | tr '\n' ' ' | cut -c1-200)"
        restart_action="opted-in restart failed (rc=${restart_rc})"
      fi
      # Bounded wait for the (real) hydration line to appear after a restart.
      if [[ "$restart_rc" -eq 0 ]]; then
        local waited=0
        while [[ "$waited" -le "$AGENT_RESTART_WAIT_S" ]]; do
          line="$(agent_log_grep 'loaded [0-9]+ sessions from store' 2>/dev/null | tail -n1)"
          [[ -n "$line" ]] && break
          sleep 2
          waited=$(( waited + 2 ))
        done
      fi
    fi
  fi
  set_fact hydration_restart_action "$restart_action"
  line="$(agent_log_grep 'loaded [0-9]+ sessions from store' 2>/dev/null | tail -n1)"
  if [[ -z "$line" ]]; then
    check hydration_restart_observed FAIL "no 'loaded N sessions from store' line in the agent log (source: $(agent_log_source); ${restart_action}) — remedy: either set LIVE_M4EXIT_AGENT_LOG_FILE=<path to a startup log that contains the line>, or opt in to an agent restart with LIVE_M4EXIT_ALLOW_AGENT_RESTART=1 and LIVE_M4EXIT_AGENT_RESTART_COMMAND='<exact restart command>' (plus LIVE_M4EXIT_AGENT_SSH_HOST=<target> for a remote agent), then re-run --case enrollment_hydration_restart"
    check hydration_session_count FAIL "not evaluated: no restart hydration line"
  else
    n="$(printf '%s' "$line" | grep -oE 'loaded [0-9]+ sessions' | grep -oE '[0-9]+' | head -n1)"
    check hydration_restart_observed PASS "agent log shows 'loaded ${n} sessions from store'"
    set_fact hydration_loaded_sessions "$n"
    if [[ -z "$EXPECTED_HYDRATED_SESSIONS" ]]; then
      check hydration_session_count FAIL "LIVE_M4EXIT_EXPECTED_HYDRATED_SESSIONS is unset — cannot compare loaded=${n}"
    elif [[ "${n:-0}" -ge "$EXPECTED_HYDRATED_SESSIONS" ]]; then
      check hydration_session_count PASS "loaded ${n} sessions >= the declared minimum ${EXPECTED_HYDRATED_SESSIONS}"
    else
      check hydration_session_count FAIL "loaded ${n} sessions < the declared minimum ${EXPECTED_HYDRATED_SESSIONS}"
    fi
  fi

  # (4) Per-share content still resolves over the relay after the restart.
  if [[ -z "$SHARE_CODE" ]]; then
    check hydration_share_content FAIL "LIVE_M4EXIT_SHARE_CODE is unset — cannot fetch the share after the restart"
    return 0
  fi
  if [[ -z "$CONTROL_BASE_URL" ]]; then
    check hydration_share_content FAIL "LIVE_M4EXIT_CONTROL_BASE_URL is unset — cannot prepare the relay route"
    return 0
  fi
  if prepare_relay_or_fail "$SHARE_CODE" hydration_share; then
    http_fetch ${RELAY_TLS_ARGS[@]+"${RELAY_TLS_ARGS[@]}"} "$PREPARE_RELAY_URL"
    if [[ "$HTTP_CODE" == "200" ]]; then
      check hydration_share_content PASS "relay gallery over ${RELAY_HOST} -> 200 ($(file_bytes "$HTTP_BODY_FILE") bytes)"
    else
      check hydration_share_content FAIL "relay gallery -> ${HTTP_CODE:-000} ${HTTP_ERR:-}"
    fi
  else
    check hydration_share_content FAIL "not evaluated: no relay route for the configured share"
  fi
  return 0
}

# ===========================================================================
# CASE: relay_content_integrity
# ===========================================================================

parse_range_spec() {
  # parse_range_spec <start-end> ; sets RANGE_START / RANGE_END
  RANGE_START="${1%%-*}"
  RANGE_END="${1#*-}"
  [[ "$RANGE_START" =~ ^[0-9]+$ && "$RANGE_END" =~ ^[0-9]+$ ]]
}

relay_content_integrity() {
  require_config \
    "LIVE_M4EXIT_CONTROL_BASE_URL=${CONTROL_BASE_URL}|control base URL for the interstitial/prepare-route" \
    "LIVE_M4EXIT_SHARE_CODE=${SHARE_CODE}|share code for the relay content scenario" \
    "LIVE_M4EXIT_RELAY_HOST=${RELAY_HOST}|relay gateway hostname" \
    "LIVE_M4EXIT_ITEM_ID=${ITEM_ID}|image item id" \
    "LIVE_M4EXIT_ASSET_ID=${ASSET_ID}|full original asset id" \
    "LIVE_M4EXIT_VIDEO_ID=${VIDEO_ID}|video item id" \
    "LIVE_M4EXIT_EXPECTED_ITEM_COUNT=${EXPECTED_ITEM_COUNT}|expected /items count" || return 0

  if ! python_ok; then
    check relay_items_manifest FAIL "python3 is unavailable on the harness host — cannot parse the /items manifest"
    return 0
  fi

  # (1) Interstitial -> prepare-route -> relay.
  if ! prepare_relay_or_fail "$SHARE_CODE" relay_content; then
    return 0
  fi
  local relay_url="$PREPARE_RELAY_URL"

  local items_file="$GATE_WORK_DIR/items.json"
  local full_asset="$GATE_WORK_DIR/asset.bin"
  local full_playback="$GATE_WORK_DIR/playback.bin"
  local asset_expected_bytes="$EXPECTED_ASSET_BYTES"

  # (2) Gallery page.
  http_fetch ${RELAY_TLS_ARGS[@]+"${RELAY_TLS_ARGS[@]}"} "$relay_url"
  if [[ "$HTTP_CODE" == "200" ]]; then
    check relay_gallery_page PASS "GET ${relay_url#*://} -> 200 ($(file_bytes "$HTTP_BODY_FILE") bytes)"
  else
    check relay_gallery_page FAIL "GET ${relay_url#*://} -> ${HTTP_CODE:-000} ${HTTP_ERR:-}"
  fi

  # (3) /items manifest with the expected count.
  http_fetch ${RELAY_TLS_ARGS[@]+"${RELAY_TLS_ARGS[@]}"} "$relay_url/items"
  if [[ "$HTTP_CODE" != "200" ]]; then
    check relay_items_manifest FAIL "GET /items -> ${HTTP_CODE:-000} ${HTTP_ERR:-}"
    check relay_item_sha1 FAIL "not evaluated: /items manifest unavailable"
    check relay_asset_sha1 FAIL "not evaluated: /items manifest unavailable"
    check relay_asset_bytes FAIL "not evaluated: /items manifest unavailable"
  else
    cp "$HTTP_BODY_FILE" "$items_file"
    local count
    count="$(json_item_count "$items_file" 2>/dev/null || true)"
    set_fact relay_items_count "${count:-<unparseable>}"
    if [[ "${count:-x}" == "$EXPECTED_ITEM_COUNT" ]]; then
      check relay_items_manifest PASS "/items -> 200 with ${count} items (expected ${EXPECTED_ITEM_COUNT})"
    else
      check relay_items_manifest FAIL "/items -> 200 with ${count:-<unparseable>} items, expected ${EXPECTED_ITEM_COUNT}"
    fi

    # (4) /thumb image content type.
    http_fetch ${RELAY_TLS_ARGS[@]+"${RELAY_TLS_ARGS[@]}"} "$relay_url/thumb/$ITEM_ID"
    local ctype
    ctype="$(header_value Content-Type)"
    if [[ "$HTTP_CODE" == "200" && "$ctype" == image/* ]]; then
      check relay_thumb_image PASS "GET /thumb/${ITEM_ID} -> 200 Content-Type=${ctype}"
    else
      check relay_thumb_image FAIL "GET /thumb/${ITEM_ID} -> ${HTTP_CODE:-000} Content-Type='${ctype:-<none>}' ${HTTP_ERR:-}"
    fi

    # (5) FULL original asset: sha1 == the manifest sha1 for that item, and the
    # byte count is exact.
    local manifest_sha1 manifest_size
    manifest_sha1="$(json_item_field "$items_file" "$ASSET_ID" sha1 2>/dev/null || true)"
    manifest_size="$(json_item_field "$items_file" "$ASSET_ID" size 2>/dev/null || true)"
    http_fetch ${RELAY_TLS_ARGS[@]+"${RELAY_TLS_ARGS[@]}"} "$relay_url/asset/$ASSET_ID"
    if [[ "$HTTP_CODE" != "200" ]]; then
      check relay_asset_sha1 FAIL "GET /asset/${ASSET_ID} -> ${HTTP_CODE:-000} ${HTTP_ERR:-}"
      check relay_asset_bytes FAIL "not evaluated: asset fetch failed"
    else
      cp "$HTTP_BODY_FILE" "$full_asset"
      local asset_sha1 asset_bytes
      asset_sha1="$(sha1_file "$full_asset")"
      asset_bytes="$(file_bytes "$full_asset")"
      set_fact relay_asset_sha1 "$asset_sha1"
      set_fact relay_asset_bytes "$asset_bytes"
      if [[ -z "$manifest_sha1" ]]; then
        check relay_asset_sha1 FAIL "cannot compare: item ${ASSET_ID} has no sha1 in the /items manifest"
      elif [[ "$asset_sha1" == "$manifest_sha1" ]]; then
        check relay_asset_sha1 PASS "/asset sha1=${asset_sha1} equals the manifest sha1 exactly"
      else
        check relay_asset_sha1 FAIL "/asset sha1=${asset_sha1} != manifest sha1=${manifest_sha1}"
      fi
      local want_bytes
      [[ -z "$asset_expected_bytes" ]] && asset_expected_bytes="$manifest_size"
      want_bytes="$asset_expected_bytes"
      if [[ -z "$want_bytes" ]]; then
        check relay_asset_bytes FAIL "cannot compare: no LIVE_M4EXIT_EXPECTED_ASSET_BYTES and no manifest size for ${ASSET_ID}"
      elif [[ "$asset_bytes" == "$want_bytes" ]]; then
        check relay_asset_bytes PASS "/asset byte count=${asset_bytes} is exact"
      else
        check relay_asset_bytes FAIL "/asset byte count=${asset_bytes}, expected ${want_bytes}"
      fi
    fi
  fi

  # (6) Download path intentionally ignores Range: HEAD + Range must be 200 full.
  http_head ${RELAY_TLS_ARGS[@]+"${RELAY_TLS_ARGS[@]}"} -H "Range: bytes=0-1023" "$relay_url/asset/$ASSET_ID"
  local cl
  cl="$(header_value Content-Length)"
  if [[ "$HTTP_CODE" != "200" ]]; then
    check relay_asset_range_ignored FAIL "HEAD /asset/${ASSET_ID} with Range -> ${HTTP_CODE:-000}, want 200 full (the download path must ignore Range)"
  elif [[ -n "$cl" && "$cl" != "0" && -n "$asset_expected_bytes" && "$cl" != "$asset_expected_bytes" ]]; then
    check relay_asset_range_ignored FAIL "HEAD /asset/${ASSET_ID} with Range -> 200 but Content-Length=${cl} != the expected full size ${asset_expected_bytes} (Range must be ignored)"
  else
    check relay_asset_range_ignored PASS "HEAD /asset/${ASSET_ID} with Range -> 200 full (download path ignores Range; Content-Length=${cl:-<none>})"
  fi

  # (7) Video playback: HEAD 200, no-Range 200 full, then Range seeks.
  local playback_url="$relay_url/asset/$VIDEO_ID/playback"
  http_head ${RELAY_TLS_ARGS[@]+"${RELAY_TLS_ARGS[@]}"} "$playback_url"
  if [[ "$HTTP_CODE" == "200" ]]; then
    check relay_playback_head PASS "HEAD playback -> 200 (Content-Length=$(header_value Content-Length))"
  else
    check relay_playback_head FAIL "HEAD playback -> ${HTTP_CODE:-000} ${HTTP_ERR:-}"
  fi

  http_fetch ${RELAY_TLS_ARGS[@]+"${RELAY_TLS_ARGS[@]}"} "$playback_url"
  local total=0
  if [[ "$HTTP_CODE" == "200" ]]; then
    cp "$HTTP_BODY_FILE" "$full_playback"
    total="$(file_bytes "$full_playback")"
    set_fact relay_playback_bytes "$total"
    if [[ "$total" -gt 0 ]]; then
      check relay_playback_full PASS "no-Range playback -> 200 full (${total} bytes)"
    else
      check relay_playback_full FAIL "no-Range playback -> 200 but zero bytes"
    fi
  else
    check relay_playback_full FAIL "no-Range playback -> ${HTTP_CODE:-000} ${HTTP_ERR:-}"
  fi

  if [[ "$total" -gt 0 ]]; then
    local half r1 r2
    half=$(( total / 2 ))
    r1="0-$(( total > 1024 ? 1023 : total - 1 ))"
    r2="${half}-$(( half + 1023 < total ? half + 1023 : total - 1 ))"
    local specs=("$r1" "$r2")
    local spec ok_count=0
    for spec in "${specs[@]}"; do
      if ! parse_range_spec "$spec"; then
        check "relay_playback_range_${spec}" FAIL "internal range spec parse failure"
        continue
      fi
      http_fetch ${RELAY_TLS_ARGS[@]+"${RELAY_TLS_ARGS[@]}"} -H "Range: bytes=${spec}" "$playback_url"
      if [[ "$HTTP_CODE" != "206" ]]; then
        check "relay_playback_range_${spec}" FAIL "Range bytes=${spec} -> ${HTTP_CODE:-000}, want 206 ${HTTP_ERR:-}"
        continue
      fi
      local want_len=$(( RANGE_END - RANGE_START + 1 ))
      local got_len cr want_cr slice_file
      got_len="$(file_bytes "$HTTP_BODY_FILE")"
      cr="$(header_value Content-Range)"
      want_cr="bytes ${RANGE_START}-${RANGE_END}/${total}"
      slice_file="$GATE_WORK_DIR/slice.bin"
      tail -c "+$(( RANGE_START + 1 ))" "$full_playback" | head -c "$want_len" > "$slice_file"
      local body_sha slice_sha
      body_sha="$(sha1_file "$HTTP_BODY_FILE")"
      slice_sha="$(sha1_file "$slice_file")"
      if [[ "$got_len" == "$want_len" && "$cr" == "$want_cr" && "$body_sha" == "$slice_sha" ]]; then
        check "relay_playback_range_${spec}" PASS "Range bytes=${spec} -> 206 len=${got_len} Content-Range='${cr}' sha1=${body_sha} byte-exact vs the full body"
        ok_count=$(( ok_count + 1 ))
      else
        check "relay_playback_range_${spec}" FAIL "Range bytes=${spec} -> 206 len=${got_len} (want ${want_len}) Content-Range='${cr:-<none>}' (want '${want_cr}') sha1 match=$([[ "$body_sha" == "$slice_sha" ]] && echo yes || echo no)"
      fi
    done
    if [[ "$ok_count" -ge "$MIN_RANGE_REQUESTS" ]]; then
      check relay_playback_range_parity PASS "${ok_count} in-range 206 seek(s) byte-exact (>= ${MIN_RANGE_REQUESTS})"
    else
      check relay_playback_range_parity FAIL "only ${ok_count} byte-exact in-range 206 seek(s), want >= ${MIN_RANGE_REQUESTS}"
    fi

    # (8) The three documented 416 cases (fail-closed rejections).
    check_playback_416 "relay_playback_suffix_range_416" "bytes=-1024" "$playback_url"
    check_playback_416 "relay_playback_multirange_416" "bytes=0-10,20-30" "$playback_url"
    check_playback_416 "relay_playback_start_ge_total_416" "bytes=${total}-" "$playback_url"
  else
    check relay_playback_range_parity FAIL "not evaluated: no full playback body"
    check_playback_416 "relay_playback_suffix_range_416" "bytes=-1024" "$playback_url"
    check_playback_416 "relay_playback_multirange_416" "bytes=0-10,20-30" "$playback_url"
    check_playback_416 "relay_playback_start_ge_total_416" "bytes=1-" "$playback_url"
  fi
  return 0
}

check_playback_416() {
  local cname="$1" range="$2" url="$3"
  http_fetch ${RELAY_TLS_ARGS[@]+"${RELAY_TLS_ARGS[@]}"} -H "Range: ${range}" "$url"
  if [[ "$HTTP_CODE" == "416" ]]; then
    check "$cname" PASS "Range ${range} -> 416 (Content-Range=$(header_value Content-Range))"
  else
    check "$cname" FAIL "Range ${range} -> ${HTTP_CODE:-000}, want 416 ${HTTP_ERR:-}"
  fi
}

# ===========================================================================
# CASE: stun_observe_and_rechallenge
# ===========================================================================

stun_observe_and_rechallenge() {
  # (1) Counters.
  if fetch_control_metrics >/dev/null 2>&1 && [[ -n "$CONTROL_METRICS_TEXT" ]]; then
    local match mismatch timeout
    match="$(metric_value "$CONTROL_METRICS_TEXT" 'sharebridge_relay_stun_total{outcome="match"}')"
    mismatch="$(metric_value "$CONTROL_METRICS_TEXT" 'sharebridge_relay_stun_total{outcome="mismatch"}')"
    timeout="$(metric_value "$CONTROL_METRICS_TEXT" 'sharebridge_relay_stun_total{outcome="timeout"}')"
    set_fact stun_match_total "${match:-<absent>}"
    set_fact stun_mismatch_total "${mismatch:-<absent>}"
    set_fact stun_timeout_total "${timeout:-<absent>}"
    if [[ -n "$match" && "$match" -gt 0 ]]; then
      check stun_counters_match PASS "stun_total{match}=${match} (>0)"
    else
      check stun_counters_match FAIL "stun_total{match}=${match:-<absent>} (want >0)"
    fi
    if [[ -n "$mismatch" && "$mismatch" -eq 0 ]]; then
      check stun_counters_mismatch_zero PASS "stun_total{mismatch}=0"
    else
      check stun_counters_mismatch_zero FAIL "stun_total{mismatch}=${mismatch:-<absent>} (want 0)"
    fi
    if [[ -n "$timeout" && "$timeout" -eq 0 ]]; then
      check stun_counters_timeout_zero PASS "stun_total{timeout}=0"
    else
      check stun_counters_timeout_zero FAIL "stun_total{timeout}=${timeout:-<absent>} (want 0)"
    fi
  else
    check stun_counters_match FAIL "no control metrics access: set LIVE_M4EXIT_CONTROL_METRICS_URL or LIVE_M4EXIT_CONTROL_SSH_HOST"
    check stun_counters_mismatch_zero FAIL "not evaluated: no control metrics access"
    check stun_counters_timeout_zero FAIL "not evaluated: no control metrics access"
  fi

  # (2) Rechallenge cadence from the accept timestamps.
  local journal="" source=""
  if [[ -n "$STUN_JOURNAL_FILE" ]]; then
    if [[ -r "$STUN_JOURNAL_FILE" ]]; then
      journal="$(cat "$STUN_JOURNAL_FILE")"; source="$STUN_JOURNAL_FILE"
    else
      source="$STUN_JOURNAL_FILE (unreadable)"
    fi
  elif [[ -n "$CONTROL_SSH_HOST" ]]; then
    journal="$(remote_journal "$CONTROL_SSH_HOST" "$CONTROL_UNIT" 'stun observation accepted')"
    source="journalctl -u ${CONTROL_UNIT} @${CONTROL_SSH_HOST}"
  fi

  if [[ -z "$journal" ]]; then
    check stun_rechallenge_observed FAIL "no 'stun observation accepted' timestamps available (set LIVE_M4EXIT_STUN_JOURNAL_FILE or LIVE_M4EXIT_CONTROL_SSH_HOST; source=${source:-<unconfigured>}) — cadence unobservable"
    check stun_rechallenge_cadence FAIL "not evaluated: no accept timestamps"
    check stun_rechallenge_no_stall FAIL "not evaluated: no accept timestamps"
    return 0
  fi
  if ! python_ok; then
    check stun_rechallenge_observed FAIL "python3 is unavailable — cannot parse the accept timestamps"
    check stun_rechallenge_cadence FAIL "not evaluated: python3 unavailable"
    check stun_rechallenge_no_stall FAIL "not evaluated: python3 unavailable"
    return 0
  fi

  local deltas ts_count
  deltas="$(printf '%s\n' "$journal" | python3 -c '
import sys, re, datetime
ts = []
for line in sys.stdin:
    m = re.search(r"(\d{4}-\d{2}-\d{2})[T ](\d{2}):(\d{2}):(\d{2})", line)
    if m:
        ts.append(datetime.datetime(
            int(m.group(1)[0:4]), int(m.group(1)[5:7]), int(m.group(1)[8:10]),
            int(m.group(2)), int(m.group(3)), int(m.group(4))))
ts = sorted(set(ts))
for a, b in zip(ts, ts[1:]):
    print(int((b - a).total_seconds()))
')"
  ts_count="$(printf '%s\n' "$deltas" | grep -cE '^-?[0-9]+$' || true)"
  ts_count="${ts_count:-0}"

  if [[ "$ts_count" -eq 0 ]]; then
    check stun_rechallenge_observed FAIL "only one (or zero) 'stun observation accepted' timestamp(s) in ${source} — cadence unprovable"
    check stun_rechallenge_cadence FAIL "not evaluated: fewer than two accept timestamps"
    check stun_rechallenge_no_stall FAIL "not evaluated: fewer than two accept timestamps"
    return 0
  fi

  local tol band_min band_max min_in_band d in_band=0 bad=0 maxd=0
  tol="$STUN_CADENCE_TOLERANCE_S"
  band_min=$(( 240 - tol ))
  band_max=$(( 255 + tol ))
  min_in_band="$STUN_MIN_IN_BAND_DELTAS"
  local all_deltas=""
  while read -r d; do
    [[ -z "$d" ]] && continue
    all_deltas="${all_deltas}${d} "
    [[ "$d" -gt "$maxd" ]] && maxd="$d"
    if [[ "$d" -gt "$band_max" ]]; then bad=$(( bad + 1 )); fi
    if [[ "$d" -ge "$band_min" ]]; then in_band=$(( in_band + 1 )); fi
  done <<< "$deltas"
  set_fact stun_rechallenge_deltas "${all_deltas% }"
  set_fact stun_rechallenge_max_delta "$maxd"

  check stun_rechallenge_observed PASS "${ts_count} inter-arrival delta(s) measured from ${source}: ${all_deltas% }"
  if [[ "$in_band" -ge "$min_in_band" ]]; then
    check stun_rechallenge_cadence PASS "${in_band} delta(s) inside the ${band_min}-${band_max}s band (4m + jitter[0,15s) +/- ${tol}s)"
  else
    check stun_rechallenge_cadence FAIL "only ${in_band} delta(s) inside the ${band_min}-${band_max}s band, want >= ${min_in_band} (deltas: ${all_deltas% })"
  fi
  if [[ "$bad" -eq 0 ]]; then
    check stun_rechallenge_no_stall PASS "no delta exceeds ${band_max}s (proactive rechallenge never stalled; max=${maxd}s)"
  else
    check stun_rechallenge_no_stall FAIL "${bad} delta(s) exceed ${band_max}s (max=${maxd}s; deltas: ${all_deltas% })"
  fi
  return 0
}

# ===========================================================================
# CASE: direct_path_or_failclosed
# ===========================================================================

record_field() {
  # record_field <file> <key> ; accepts key=value, key: value or "key": "value"
  local file="$1" key="$2"
  sed -nE "s/.*\"?${key}\"?[[:space:]]*[:=][[:space:]]*\"?([^\"',}[:space:]]+).*/\1/p" "$file" | head -n1
}

direct_path_or_failclosed() {
  require_config \
    "LIVE_M4EXIT_CONTROL_BASE_URL=${CONTROL_BASE_URL}|control base URL for prepare-route" \
    "LIVE_M4EXIT_SHARE_CODE=${SHARE_CODE}|share code for the direct-path scenario" || return 0

  prepare_route "$SHARE_CODE"
  case "$PREPARE_STATUS" in
    direct)
      if [[ -z "$PREPARE_DIRECT_URL" ]]; then
        check direct_path_observable FAIL "prepare-route -> 200 status=direct but no direct_url"
        return 0
      fi
      check direct_path_observable PASS "prepare-route -> 200 status=direct with a direct_url"
      http_fetch ${CONTROL_TLS_ARGS[@]+"${CONTROL_TLS_ARGS[@]}"} "$PREPARE_DIRECT_URL"
      if [[ "$HTTP_CODE" == "200" ]]; then
        check direct_route_serves PASS "direct URL -> 200 ($(file_bytes "$HTTP_BODY_FILE") bytes)"
      else
        check direct_route_serves FAIL "direct URL -> ${HTTP_CODE:-000} ${HTTP_ERR:-} (status=direct must serve)"
      fi
      ;;
    relay)
      check direct_path_observable PASS "prepare-route -> 200 status=relay (fail-closed fallback)"
      if [[ -z "$AGENT_RECORD_FILE" ]]; then
        check direct_failclosed_diagnostics FAIL "prepare-route reported the fail-closed relay fallback but LIVE_M4EXIT_AGENT_RECORD_FILE is unset, so the control-side direct diagnostics cannot be verified. Expected file: the control agents record for this agent (PocketBase collection 'agents'), saved as JSON or key=value text containing direct_status and direct_status_reason (the parser accepts 'key=value', 'key: value' and JSON). Obtain it from the control admin UI (Collections -> agents -> the agent row -> copy/export) or via the control API. Verdicts: direct_status=relay_fallback AND direct_status_reason=<LIVE_M4EXIT_EXPECTED_DIRECT_REASON, default probe_failed> => PASS; a different direct_status or reason => FAIL; and this file is not needed when prepare-route returns status=direct (that is judged by direct_route_serves instead)"
        return 0
      fi
      if [[ ! -r "$AGENT_RECORD_FILE" ]]; then
        check direct_failclosed_diagnostics FAIL "LIVE_M4EXIT_AGENT_RECORD_FILE=${AGENT_RECORD_FILE} is not readable"
        return 0
      fi
      local ds dr
      ds="$(record_field "$AGENT_RECORD_FILE" direct_status)"
      dr="$(record_field "$AGENT_RECORD_FILE" direct_status_reason)"
      set_fact direct_status "${ds:-<absent>}"
      set_fact direct_status_reason "${dr:-<absent>}"
      if [[ "$ds" == "relay_fallback" ]]; then
        check direct_status_relay_fallback PASS "agent record direct_status=relay_fallback"
      else
        check direct_status_relay_fallback FAIL "agent record direct_status='${ds:-<absent>}' (want relay_fallback)"
      fi
      if [[ "$dr" == "$EXPECTED_DIRECT_REASON" ]]; then
        check direct_status_reason PASS "agent record direct_status_reason=${dr}"
      else
        check direct_status_reason FAIL "agent record direct_status_reason='${dr:-<absent>}' (want ${EXPECTED_DIRECT_REASON})"
      fi
      ;;
    "")
      check direct_path_observable FAIL "prepare-route -> ${PREPARE_HTTP_CODE:-000} produced no status (body: $(sanitize < "$PREPARE_BODY_FILE" | tr -d '\n' | cut -c1-160)) — neither direct nor relay is observable"
      ;;
    *)
      check direct_path_observable FAIL "prepare-route -> ${PREPARE_HTTP_CODE:-000} status='${PREPARE_STATUS}' — neither direct nor a relay fallback is observable"
      ;;
  esac
  return 0
}

# ===========================================================================
# CASE: lockdown_withdrawal_and_recovery  (OPT-IN)
# ===========================================================================

lockdown_withdrawal_and_recovery() {
  if [[ "$ALLOW_LOCKDOWN" != "1" ]]; then
    check lockdown_opt_in SKIP "LIVE_M4EXIT_ALLOW_LOCKDOWN!=1 — this case changes transport availability (lockdown/unlock); opt in explicitly"
    return 0
  fi
  require_config \
    "LIVE_M4EXIT_CONTROL_BASE_URL=${CONTROL_BASE_URL}|control base URL for prepare-route" \
    "LIVE_M4EXIT_SHARE_CODE=${SHARE_CODE}|share code for the lockdown scenario" \
    "LIVE_M4EXIT_AGENT_ADMIN_BASE_URL=${AGENT_ADMIN_BASE_URL}|agent admin base URL" \
    "LIVE_M4EXIT_AGENT_ADMIN_USER=${AGENT_ADMIN_USER}|agent admin Basic-auth user" \
    "LIVE_M4EXIT_AGENT_ADMIN_PASSWORD=${AGENT_ADMIN_PASSWORD}|agent admin Basic-auth password" || return 0

  # The main process owns the EXIT/INT/TERM unlock trap (installed by
  # m4exit_install_lockdown_trap before the cases run); this case only records
  # the locked state in the shared state file so the trap can see it.

  # (1) Baseline healthy: relay route + tunnel online.
  local baseline_url=""
  if prepare_relay_or_fail "$SHARE_CODE" lockdown_baseline; then
    baseline_url="$PREPARE_RELAY_URL"
  fi
  local online
  online="$(gateway_tunnel_online 2>/dev/null || true)"
  if [[ "${online:-0}" -ge 1 ]]; then
    check lockdown_baseline_tunnel PASS "baseline tunnel online=${online}"
  else
    check lockdown_baseline_tunnel FAIL "baseline tunnel online=${online:-<absent>} (want >=1)"
  fi

  # (2) frps close baseline: count matching journal lines first.
  local frps_before=0 frps_pat="proxy closing"
  if ! frps_journal_available; then
    check lockdown_frps_proxy_closed FAIL "no frps journal access: set LIVE_M4EXIT_FRPS_JOURNAL_FILE or LIVE_M4EXIT_FRPS_SSH_HOST"
  else
    frps_before="$(frps_journal_grep "$frps_pat" 2>/dev/null | grep -c . || true)"
    frps_before="${frps_before:-0}"
  fi
  local frps_err_before=0
  frps_err_before="$(frps_journal_grep "listener is closed" 2>/dev/null | grep -c . || true)"
  frps_err_before="${frps_err_before:-0}"

  # (3) Lockdown.
  agent_api POST "/api/lockdown"
  if [[ "$AGENT_HTTP_CODE" == "200" ]]; then
    m4exit_mark_locked
    check lockdown_applied PASS "POST /api/lockdown -> 200 ($(json_str "$(cat "$AGENT_BODY_FILE")" locked))"
  else
    check lockdown_applied FAIL "POST /api/lockdown -> ${AGENT_HTTP_CODE:-000} ${HTTP_ERR:-}"
    return 0
  fi

  # (4) Tunnel goes offline within the documented bound.
  local waited=0 offline_ok=0 secs=-1
  while [[ "$waited" -le "$TUNNEL_OFFLINE_BOUND_S" ]]; do
    online="$(gateway_tunnel_online 2>/dev/null || true)"
    if [[ -n "$online" && "$online" -eq 0 ]]; then offline_ok=1; secs="$waited"; break; fi
    sleep 1
    waited=$(( waited + 1 ))
  done
  if [[ "$offline_ok" == "1" ]]; then
    check lockdown_tunnel_offline PASS "tunnel went offline ${secs}s after lockdown (bound ${TUNNEL_OFFLINE_BOUND_S}s)"
    set_fact lockdown_tunnel_offline_seconds "$secs"
  else
    check lockdown_tunnel_offline FAIL "tunnel did not report online=0 within ${TUNNEL_OFFLINE_BOUND_S}s (last online=${online:-<absent>})"
  fi

  # (5) frps closes the proxy (journal line count increased).
  sleep 1
  local frps_after frps_err_after
  frps_after="$(frps_journal_grep "$frps_pat" 2>/dev/null | grep -c . || true)"
  frps_after="${frps_after:-0}"
  frps_err_after="$(frps_journal_grep "listener is closed" 2>/dev/null | grep -c . || true)"
  frps_err_after="${frps_err_after:-0}"
  note lockdown_frps_journal_source "$( [[ -n "$FRPS_JOURNAL_FILE" ]] && printf '%s' "$FRPS_JOURNAL_FILE" || printf 'journalctl -u %s @%s' "$FRPS_UNIT" "${FRPS_SSH_HOST:-<unconfigured>}" )"
  if ! frps_journal_available; then
    : # the FAIL was already recorded above
  elif [[ "$frps_after" -gt "$frps_before" || "$frps_err_after" -gt "$frps_err_before" ]]; then
    check lockdown_frps_proxy_closed PASS "frps journal gained a proxy-close line after lockdown (proxy closing ${frps_before}->${frps_after}, listener is closed ${frps_err_before}->${frps_err_after})"
  else
    check lockdown_frps_proxy_closed FAIL "frps journal shows no new proxy-close line after lockdown (proxy closing ${frps_before}->${frps_after}, listener is closed ${frps_err_before}->${frps_err_after})"
  fi

  # (6) prepare-route becomes suppressed/unavailable.
  prepare_route "$SHARE_CODE"
  if [[ "$PREPARE_HTTP_CODE" == "503" && "$PREPARE_STATUS" != "relay" ]]; then
    check lockdown_prepare_suppressed PASS "prepare-route while locked -> 503 status='${PREPARE_STATUS:-<none>}' ($(sanitize < "$PREPARE_BODY_FILE" | tr -d '\n' | cut -c1-80))"
  else
    check lockdown_prepare_suppressed FAIL "prepare-route while locked -> ${PREPARE_HTTP_CODE:-000} status='${PREPARE_STATUS:-<none>}' (want 503 suppressed)"
  fi

  # (7) A fetch of the previously-issued relay URL yields no content.
  if [[ -n "$baseline_url" ]]; then
    http_fetch ${RELAY_TLS_ARGS[@]+"${RELAY_TLS_ARGS[@]}"} "$baseline_url"
    local bytes=0
    [[ -f "$HTTP_BODY_FILE" ]] && bytes="$(file_bytes "$HTTP_BODY_FILE")"
    if [[ "$HTTP_CODE" != "200" && "$bytes" -eq 0 ]]; then
      check lockdown_relay_withdrawn PASS "relay fetch while locked -> ${HTTP_CODE:-000} with ${bytes} bytes (no content served)"
    elif [[ "$HTTP_CODE" != "200" ]]; then
      check lockdown_relay_withdrawn FAIL "relay fetch while locked -> ${HTTP_CODE:-000} but ${bytes} bytes were served"
    else
      check lockdown_relay_withdrawn FAIL "relay fetch while locked -> 200 with ${bytes} bytes — content leaked while locked"
    fi
  else
    check lockdown_relay_withdrawn FAIL "not evaluated: no baseline relay URL was issued"
  fi

  # (8) Unlock and measure recovery.
  agent_api POST "/api/unlock"
  if [[ "$AGENT_HTTP_CODE" != "200" ]]; then
    check lockdown_recovery FAIL "POST /api/unlock -> ${AGENT_HTTP_CODE:-000} ${HTTP_ERR:-}"
    return 0
  fi
  m4exit_mark_unlocked
  local rec_wait=0 rec_ok=0 rec_secs=-1 stage=""
  while [[ "$rec_wait" -le "$RECOVERY_BOUND_S" ]]; do
    online="$(gateway_tunnel_online 2>/dev/null || true)"
    prepare_route "$SHARE_CODE"
    if [[ -n "$online" && "$online" -ge 1 && "$PREPARE_HTTP_CODE" == "200" && "$PREPARE_STATUS" == "relay" ]]; then
      rec_ok=1; rec_secs="$rec_wait"; stage="tunnel online + prepare-route relay"; break
    fi
    sleep 1
    rec_wait=$(( rec_wait + 1 ))
  done
  set_fact lockdown_recovery_seconds "$rec_secs"
  if [[ "$rec_ok" == "1" ]]; then
    check lockdown_recovery PASS "recovered ${rec_secs}s after unlock (${stage}; bound ${RECOVERY_BOUND_S}s)"
  else
    check lockdown_recovery FAIL "no recovery within ${RECOVERY_BOUND_S}s after unlock (last tunnel online=${online:-<absent>}, prepare http=${PREPARE_HTTP_CODE:-000} status='${PREPARE_STATUS:-<none>}')"
  fi
  note lockdown_recovery_reference "the live run measured ~3s warm and ~60s cold for post-unlock recovery"
  return 0
}

# ===========================================================================
# CASE: revocation_midstream  (OPT-IN)
# ===========================================================================

revocation_midstream() {
  if [[ "$ALLOW_REVOKE" != "1" ]]; then
    check revoke_opt_in SKIP "LIVE_M4EXIT_ALLOW_REVOKE!=1 — this case revokes a share mid-transfer; opt in explicitly"
    return 0
  fi
  if [[ -z "$REVOKE_SHARE_CODE" ]]; then
    check revoke_share_code FAIL "LIVE_M4EXIT_REVOKE_SHARE_CODE is unset but LIVE_M4EXIT_ALLOW_REVOKE=1 — a share code is required"
    return 0
  fi
  require_config \
    "LIVE_M4EXIT_CONTROL_BASE_URL=${CONTROL_BASE_URL}|control base URL for prepare-route" \
    "LIVE_M4EXIT_AGENT_ADMIN_BASE_URL=${AGENT_ADMIN_BASE_URL}|agent admin base URL" \
    "LIVE_M4EXIT_AGENT_ADMIN_USER=${AGENT_ADMIN_USER}|agent admin Basic-auth user" \
    "LIVE_M4EXIT_AGENT_ADMIN_PASSWORD=${AGENT_ADMIN_PASSWORD}|agent admin Basic-auth password" || return 0

  # (1) Relay route for the share to revoke.
  if ! prepare_relay_or_fail "$REVOKE_SHARE_CODE" revoke; then
    return 0
  fi
  local relay_url="$PREPARE_RELAY_URL"

  # (2) Resolve the asset id and its full size.
  local asset_id="${REVOKE_ASSET_ID:-$ASSET_ID}"
  if [[ -z "$asset_id" ]]; then
    check revoke_asset_id FAIL "LIVE_M4EXIT_REVOKE_ASSET_ID and LIVE_M4EXIT_ASSET_ID are both unset — cannot choose the asset to download"
    return 0
  fi
  local full_size="$REVOKE_ASSET_BYTES"
  if [[ -z "$full_size" ]]; then
    if ! python_ok; then
      check revoke_asset_size FAIL "python3 is unavailable and LIVE_M4EXIT_REVOKE_ASSET_BYTES is unset — cannot read the asset size from /items"
      return 0
    fi
    http_fetch ${RELAY_TLS_ARGS[@]+"${RELAY_TLS_ARGS[@]}"} "$relay_url/items"
    if [[ "$HTTP_CODE" != "200" ]]; then
      check revoke_asset_size FAIL "GET /items -> ${HTTP_CODE:-000} ${HTTP_ERR:-} (needed for the asset size)"
      return 0
    fi
    full_size="$(json_item_field "$HTTP_BODY_FILE" "$asset_id" size 2>/dev/null || true)"
  fi
  if [[ -z "$full_size" || "$full_size" -le 0 ]]; then
    check revoke_asset_size FAIL "asset ${asset_id} full size is '${full_size:-<unknown>}' — cannot prove truncation"
    return 0
  fi
  check revoke_asset_size PASS "asset ${asset_id} full size=${full_size} bytes"

  # (3) Start a throttled in-flight download.
  local dl_body="$GATE_WORK_DIR/revoke-download.bin"
  local dl_code="$GATE_WORK_DIR/revoke-code.txt"
  local dl_err="$GATE_WORK_DIR/revoke-curl.err"
  record_cmd "curl --limit-rate ${REVOKE_LIMIT_RATE} ${relay_url#*://}/asset/${asset_id} (throttled background download)"
  curl --silent --show-error --max-time "$(( REVOKE_TIMEOUT_S + 30 ))" \
    --limit-rate "$REVOKE_LIMIT_RATE" -o "$dl_body" -w '%{http_code}' \
    ${RELAY_TLS_ARGS[@]+"${RELAY_TLS_ARGS[@]}"} "$relay_url/asset/$asset_id" \
    > "$dl_code" 2> "$dl_err" &
  local dl_pid=$!

  sleep "$REVOKE_DELAY_S"
  local started_bytes=0
  [[ -f "$dl_body" ]] && started_bytes="$(file_bytes "$dl_body")"

  # (4) Revoke mid-transfer.
  agent_api DELETE "/api/shares/${REVOKE_SHARE_CODE}"
  if [[ "$AGENT_HTTP_CODE" == "200" ]]; then
    check revoke_applied PASS "DELETE /api/shares/<code> -> 200 mid-transfer (${started_bytes} bytes downloaded at revoke)"
  else
    check revoke_applied FAIL "DELETE /api/shares/<code> -> ${AGENT_HTTP_CODE:-000} ${HTTP_ERR:-}"
  fi

  # (5) Bounded wait for the transfer to end (or kill it).
  local waited=0
  while kill -0 "$dl_pid" 2>/dev/null && [[ "$waited" -lt "$REVOKE_TIMEOUT_S" ]]; do
    sleep 1
    waited=$(( waited + 1 ))
  done
  if kill -0 "$dl_pid" 2>/dev/null; then
    kill "$dl_pid" 2>/dev/null || true
  fi
  wait "$dl_pid" 2>/dev/null || true
  local final_bytes=0
  [[ -f "$dl_body" ]] && final_bytes="$(file_bytes "$dl_body")"
  set_fact revoke_download_bytes "$final_bytes"
  set_fact revoke_asset_full_bytes "$full_size"
  record_out "revoke download: started=${started_bytes} final=${final_bytes} full=${full_size} http=$(cat "$dl_code" 2>/dev/null) err=$(tr '\n' ' ' < "$dl_err" 2>/dev/null | cut -c1-200)"

  # (6) The transfer must be truncated.
  if [[ "${final_bytes:-0}" -lt "$full_size" ]]; then
    check revoke_transfer_truncated PASS "download truncated at ${final_bytes}/${full_size} bytes ($(( final_bytes * 100 / full_size ))%)"
  else
    check revoke_transfer_truncated FAIL "download completed ${final_bytes}/${full_size} bytes — the mid-stream revocation was not enforced"
  fi

  # (7) Gateway route-revocation drain log line.
  local drain_line="" drain_wait=0
  if ! gateway_journal_available; then
    check revoke_gateway_drain FAIL "no gateway journal access: set LIVE_M4EXIT_GATEWAY_JOURNAL_FILE or LIVE_M4EXIT_GATEWAY_SSH_HOST"
  else
    while [[ "$drain_wait" -le 20 ]]; do
      drain_line="$(gateway_journal_grep 'revoked route closed established streams' 2>/dev/null | tail -n1)"
      [[ -n "$drain_line" ]] && break
      sleep 1
      drain_wait=$(( drain_wait + 1 ))
    done
    if [[ -n "$drain_line" ]]; then
      local streams
      streams="$(printf '%s' "$drain_line" | grep -oE 'streams=[0-9]+' | grep -oE '[0-9]+' | tail -n1)"
      set_fact revoke_drain_streams "${streams:-<absent>}"
      if [[ -n "$streams" && "$streams" -ge 1 ]]; then
        check revoke_gateway_drain PASS "gateway log: revoked route closed established streams ... streams=${streams}"
      else
        check revoke_gateway_drain FAIL "gateway drain line observed but streams='${streams:-<absent>}' (want >=1): $(printf '%s' "$drain_line" | cut -c1-200)"
      fi
    else
      check revoke_gateway_drain FAIL "no 'revoked route closed established streams' line in the gateway journal within 20s"
    fi
  fi

  # (8) Documented caveat.
  note revoke_immich_reregistration_caveat "for Immich-mirrored shares the agent's Immich poll re-registers the share within ~1 minute, so an agent-side DELETE is a temporary outage rather than a durable revocation (recorded product-semantics finding)"

  if [[ "$REVOKE_WAIT_REREGISTER_S" -gt 0 ]]; then
    local rr_wait=0 rr_ok=0
    while [[ "$rr_wait" -le "$REVOKE_WAIT_REREGISTER_S" ]]; do
      prepare_route "$REVOKE_SHARE_CODE"
      if [[ "$PREPARE_STATUS" == "relay" ]]; then rr_ok=1; break; fi
      sleep 2
      rr_wait=$(( rr_wait + 2 ))
    done
    if [[ "$rr_ok" == "1" ]]; then
      check revoke_immich_reregistration PASS "the Immich poll re-registered the share ~${rr_wait}s after the revoke"
    else
      check revoke_immich_reregistration FAIL "the share did not re-register within ${REVOKE_WAIT_REREGISTER_S}s"
    fi
  else
    note revoke_immich_reregistration "LIVE_M4EXIT_REVOKE_WAIT_REREGISTER_S=0 — re-registration not waited for (caveat recorded above)"
  fi
  return 0
}

# ===========================================================================
# Case runner (mirrors scripts/live-phase4a.sh)
# ===========================================================================

declare -a RESULT_NAMES=() RESULT_OWNERS=() RESULT_VALUES=() RESULT_DETAILS=()
declare -a RESULT_SELECTED=()

case_is_destructive() {
  case "$1" in
    lockdown_withdrawal_and_recovery|revocation_midstream) printf 'yes' ;;
    enrollment_hydration_restart) [[ "$ALLOW_AGENT_RESTART" == "1" ]] && printf 'yes' || printf 'no' ;;
    *) printf 'no' ;;
  esac
}

case_opt_in_flag() {
  case "$1" in
    enrollment_hydration_restart) printf 'LIVE_M4EXIT_ALLOW_AGENT_RESTART=%s' "$ALLOW_AGENT_RESTART" ;;
    lockdown_withdrawal_and_recovery) printf 'LIVE_M4EXIT_ALLOW_LOCKDOWN=%s' "$ALLOW_LOCKDOWN" ;;
    revocation_midstream) printf 'LIVE_M4EXIT_ALLOW_REVOKE=%s' "$ALLOW_REVOKE" ;;
    *) printf 'none' ;;
  esac
}

execute_case() {
  # execute_case <fn-name> ; sets EXECUTE_RESULT and EXECUTE_DETAIL
  local name="$1" tmp
  tmp="$(mktemp -d "${TMPDIR:-/tmp}/m4exit-case.XXXXXX")"
  GATE_CHECK_FILE="$tmp/checks"
  GATE_CMD_FILE="$tmp/cmds"
  GATE_OUT_FILE="$tmp/out"
  GATE_FACT_FILE="$tmp/facts"
  GATE_WORK_DIR="$tmp/work"
  mkdir -p "$GATE_WORK_DIR"
  : > "$GATE_CHECK_FILE"
  : > "$GATE_CMD_FILE"
  : > "$GATE_OUT_FILE"
  : > "$GATE_FACT_FILE"
  local status=0
  if declare -f "$name" >/dev/null 2>&1; then
    ( "$name" ) || status=$?
  else
    status=127
  fi

  # If the case left the agent marked locked (failed/early-returned explicit
  # unlock), re-unlock before any later case runs. The main-process EXIT trap
  # remains the final backstop for interruption right here.
  if m4exit_lockdown_maybe_active; then
    log "WARNING: case ${name} left the agent marked locked — attempting the emergency unlock before continuing"
    m4exit_emergency_unlock
  fi

  if [[ ! -s "$GATE_CHECK_FILE" ]]; then
    EXECUTE_RESULT="MISSING"
    EXECUTE_DETAIL="case executed zero checks (status=${status}) — refusing to report PASS"
  elif grep -q '^FAIL|' "$GATE_CHECK_FILE"; then
    EXECUTE_RESULT="FAIL"
    EXECUTE_DETAIL="$(grep '^FAIL|' "$GATE_CHECK_FILE" | head -n1 | cut -d'|' -f2)"
  elif [[ "$status" -ne 0 ]]; then
    EXECUTE_RESULT="FAIL"
    EXECUTE_DETAIL="case exited nonzero (status=${status}) after recording checks"
  elif grep -q '^SKIP|' "$GATE_CHECK_FILE"; then
    EXECUTE_RESULT="SKIP"
    EXECUTE_DETAIL="$(grep '^SKIP|' "$GATE_CHECK_FILE" | head -n1 | cut -d'|' -f2)"
  elif ! grep -q '^PASS|' "$GATE_CHECK_FILE"; then
    EXECUTE_RESULT="MISSING"
    EXECUTE_DETAIL="case recorded $(grep -c '^NOTE|' "$GATE_CHECK_FILE") note(s) but zero measured PASS checks — refusing to report PASS"
  else
    EXECUTE_RESULT="PASS"
    EXECUTE_DETAIL="$(grep -c '^PASS|' "$GATE_CHECK_FILE") PASS check(s)"
  fi
  EXECUTE_TMP="$tmp"
  EXECUTE_STATUS="$status"
}

write_case_evidence() {
  local dir="$1" name="$2" owner="$3" purpose="$4" result="$5" detail="$6" tmp="$7"
  local file="${dir}/case-${name}.txt"
  {
    printf '# Phase 4a M4-exit live e2e evidence — case record\n'
    printf 'run_id=%s\n' "$RUN_ID"
    printf 'utc=%s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)"
    printf 'case=%s\n' "$name"
    printf 'owner_task=%s\n' "$owner"
    printf 'purpose=%s\n' "$purpose"
    printf 'git_sha=%s\n' "$GIT_SHA"
    printf 'harness=%s\n' "scripts/live-m4exit-e2e.sh"
    printf 'destructive=%s\n' "$(case_is_destructive "$name")"
    printf 'opt_in=%s\n' "$(case_opt_in_flag "$name")"
    printf 'control_base_url=%s\n' "${CONTROL_BASE_URL:-<unset>}"
    printf 'share_code=%s\n' "${SHARE_CODE:+[REDACTED-SHARE-CODE]}"
    printf 'revoke_share_code=%s\n' "${REVOKE_SHARE_CODE:+[REDACTED-SHARE-CODE]}"
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

run_all_cases() {
  local name owner purpose i result detail
  printf 'run_id=%s\n' "$RUN_ID"
  printf 'git_sha=%s\n' "$GIT_SHA"
  printf 'mode=%s\n' "$MODE"
  printf 'scope=%s\n' "$([[ -z "$SELECTED_CASES" ]] && echo all-registered || echo "$SELECTED_CASES")"
  printf 'evidence_dir=%s\n' "$RUN_DIR"
  printf 'control_base_url=%s\n' "${CONTROL_BASE_URL:-<unset>}"
  printf 'relay_host=%s\n' "${RELAY_HOST:-<unset>}"
  printf 'allow_lockdown=%s\n' "$ALLOW_LOCKDOWN"
  printf 'allow_revoke=%s\n' "$ALLOW_REVOKE"
  printf 'allow_agent_restart=%s\n' "$ALLOW_AGENT_RESTART"

  if ! mkdir -p "$RUN_DIR"; then
    printf 'ERROR: cannot create evidence directory %s\n' "$RUN_DIR" >&2
    exit 1
  fi
  RUN_FACTS_FILE="$RUN_DIR/environment-facts.observed"
  : > "$RUN_FACTS_FILE"

  for i in "${!case_names[@]}"; do
    name="${case_names[$i]}"
    owner="${case_owners[$i]}"
    purpose="${case_purposes[$i]}"
    printf '\n=== CASE %s (%s) ===\n' "$name" "$owner"
    printf 'purpose: %s\n' "$purpose"
    if ! is_selected "$name"; then
      printf '  [NOT_RUN] not selected by --case — excluded from this verdict\n'
      RESULT_NAMES+=("$name"); RESULT_OWNERS+=("$owner")
      RESULT_VALUES+=("NOT_RUN"); RESULT_DETAILS+=("not selected by --case")
      RESULT_SELECTED+=("no")
      continue
    fi
    execute_case "$name"
    result="$EXECUTE_RESULT"
    detail="$EXECUTE_DETAIL"
    RESULT_NAMES+=("$name"); RESULT_OWNERS+=("$owner")
    RESULT_VALUES+=("$result"); RESULT_DETAILS+=("$detail")
    RESULT_SELECTED+=("yes")
    write_case_evidence "$RUN_DIR" "$name" "$owner" "$purpose" "$result" "$detail" "$EXECUTE_TMP"
    if [[ -s "$EXECUTE_TMP/facts" ]]; then cat "$EXECUTE_TMP/facts" >> "$RUN_FACTS_FILE"; fi
    printf '  => %s (%s)\n' "$result" "$detail"
    rm -rf "$EXECUTE_TMP"
  done
}

verdict_for() {
  # verdict_for <comma-separated results> -> GREEN|RED|PARTIAL
  case ",$1," in
    *,FAIL,*|*,MISSING,*) printf 'RED' ;;
    *,SKIP,*) printf 'PARTIAL' ;;
    *) printf 'GREEN' ;;
  esac
}

write_run_metadata() {
  local i
  {
    printf 'harness=scripts/live-m4exit-e2e.sh\n'
    printf 'run_id=%s\n' "$RUN_ID"
    printf 'utc=%s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)"
    printf 'git_sha=%s\n' "$GIT_SHA"
    printf 'harness_host_platform=%s_%s\n' "$uname_s" "$(uname -m 2>/dev/null || echo unknown)"
    printf 'mode=%s\n' "$MODE"
    printf 'scope=%s\n' "$([[ -z "$SELECTED_CASES" ]] && echo all-registered || echo "$SELECTED_CASES")"
    printf 'control_base_url=%s\n' "${CONTROL_BASE_URL:-<unset>}"
    printf 'relay_host=%s\n' "${RELAY_HOST:-<unset>}"
    printf 'control_ssh_host=%s\n' "${CONTROL_SSH_HOST:-<unset>}"
    printf 'gateway_ssh_host=%s\n' "${GATEWAY_SSH_HOST:-<unset>}"
    printf 'agent_admin_base_url=%s\n' "${AGENT_ADMIN_BASE_URL:-<unset>}"
    printf 'agent_admin_password=%s\n' "$([[ -n "$AGENT_ADMIN_PASSWORD" ]] && printf '[REDACTED]' || printf '<unset>')"
    printf 'share_code=%s\n' "$([[ -n "$SHARE_CODE" ]] && printf '[REDACTED-SHARE-CODE]' || printf '<unset>')"
    printf 'revoke_share_code=%s\n' "$([[ -n "$REVOKE_SHARE_CODE" ]] && printf '[REDACTED-SHARE-CODE]' || printf '<unset>')"
    printf 'allow_lockdown=%s (destructive-but-reversible: case lockdown_withdrawal_and_recovery)\n' "$ALLOW_LOCKDOWN"
    printf 'allow_revoke=%s (destructive-but-agent-local: case revocation_midstream)\n' "$ALLOW_REVOKE"
    printf 'allow_agent_restart=%s (opt-in agent restart inside case enrollment_hydration_restart)\n' "$ALLOW_AGENT_RESTART"
    printf 'evidence_dir=%s\n' "$RUN_DIR"
    printf 'config_origin=environment (no infra identifiers are embedded in this harness)\n'
  } > "$RUN_DIR/run-metadata.txt"
  printf 'run_metadata_sha256=%s\n' "$(sha256_file "$RUN_DIR/run-metadata.txt")" >> "$RUN_DIR/run-metadata.txt"
}

print_summary() {
  local i selected_results=""
  printf '\n=== M4-exit live relay e2e summary ===\n'
  printf '%-38s %-9s %-9s %s\n' "CASE" "OWNER" "RESULT" "DETAIL"
  for i in "${!RESULT_NAMES[@]}"; do
    printf '%-38s %-9s %-9s %s\n' "${RESULT_NAMES[$i]}" "${RESULT_OWNERS[$i]}" "${RESULT_VALUES[$i]}" "${RESULT_DETAILS[$i]}"
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
      printf 'case=%s owner=%s selected=%s destructive=%s result=%s detail=%s\n' \
        "${RESULT_NAMES[$i]}" "${RESULT_OWNERS[$i]}" "${RESULT_SELECTED[$i]}" \
        "$(case_is_destructive "${RESULT_NAMES[$i]}")" "${RESULT_VALUES[$i]}" "${RESULT_DETAILS[$i]}"
    done
  } > "$RUN_DIR/summary.txt"

  {
    for i in "${!RESULT_NAMES[@]}"; do
      printf '%s=%s|%s\n' "${RESULT_NAMES[$i]}" "${RESULT_VALUES[$i]}" "${RESULT_OWNERS[$i]}"
    done
  } > "$RUN_DIR/manifest.txt"

  write_environment_facts > "$RUN_DIR/environment-facts.txt"
  write_run_metadata

  printf '\nscope: %s\n' "$([[ -z "$SELECTED_CASES" ]] && echo "all registered cases" || echo "$SELECTED_CASES")"
  printf 'VERDICT: %s\n' "$VERDICT"
  printf 'evidence: %s\n' "$RUN_DIR"
  case "$VERDICT" in
    GREEN) printf 'RESULT: all selected cases PASS (exit 0)\n' ;;
    PARTIAL) printf 'RESULT: no failures, but at least one SKIP — skip is NOT a pass (exit 3)\n' ;;
    RED) printf 'RESULT: at least one selected case failed or was not executable (exit 1)\n' ;;
  esac
}

write_environment_facts() {
  printf 'run_id=%s\n' "$RUN_ID"
  printf 'utc=%s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  printf 'harness_git_sha=%s\n' "$GIT_SHA"
  printf 'control_base_url=%s\n' "${CONTROL_BASE_URL:-PENDING (LIVE_M4EXIT_CONTROL_BASE_URL unset)}"
  printf 'relay_host=%s\n' "${RELAY_HOST:-PENDING (LIVE_M4EXIT_RELAY_HOST unset)}"
  printf 'enrollment_registered_shares=%s\n' "$(fact_get enrollment_registered_shares)"
  printf 'hydrated_sessions=%s\n' "$(fact_get hydration_loaded_sessions)"
  printf 'hydration_restart_action=%s\n' "$(fact_get hydration_restart_action)"
  printf 'gateway_tunnel_online=%s\n' "$(fact_get gateway_tunnel_online)"
  printf 'relay_items_count=%s\n' "$(fact_get relay_items_count)"
  printf 'relay_asset_sha1=%s\n' "$(fact_get relay_asset_sha1)"
  printf 'relay_asset_bytes=%s\n' "$(fact_get relay_asset_bytes)"
  printf 'relay_playback_bytes=%s\n' "$(fact_get relay_playback_bytes)"
  printf 'stun_match_total=%s\n' "$(fact_get stun_match_total)"
  printf 'stun_mismatch_total=%s\n' "$(fact_get stun_mismatch_total)"
  printf 'stun_timeout_total=%s\n' "$(fact_get stun_timeout_total)"
  printf 'stun_rechallenge_deltas=%s\n' "$(fact_get stun_rechallenge_deltas)"
  printf 'direct_status=%s\n' "$(fact_get direct_status)"
  printf 'direct_status_reason=%s\n' "$(fact_get direct_status_reason)"
  printf 'lockdown_tunnel_offline_seconds=%s\n' "$(fact_get lockdown_tunnel_offline_seconds)"
  printf 'lockdown_recovery_seconds=%s\n' "$(fact_get lockdown_recovery_seconds)"
  printf 'revoke_download_bytes=%s\n' "$(fact_get revoke_download_bytes)"
  printf 'revoke_asset_full_bytes=%s\n' "$(fact_get revoke_asset_full_bytes)"
  printf 'revoke_drain_streams=%s\n' "$(fact_get revoke_drain_streams)"
}

# ---------------------------------------------------------------------------
# Config validation (used by --dry-run; live cases fail per-check)
# ---------------------------------------------------------------------------

report_missing() {
  # report_missing <var> <value> <description> ; increments MISSING_COUNT
  local var="$1" value="$2" desc="$3"
  if [[ -z "$value" ]]; then
    printf '  MISSING: %s — %s\n' "$var" "$desc"
    MISSING_COUNT=$(( MISSING_COUNT + 1 ))
  fi
  return 0
}

report_missing_any() {
  # report_missing_any <description> <var1=value> <var2=value> ... ; one of them required
  local desc="$1"; shift
  local entry var value any=""
  for entry in "$@"; do
    var="${entry%%=*}"; value="${entry#*=}"
    [[ -n "$value" ]] && any="1"
  done
  if [[ -z "$any" ]]; then
    local names=""
    for entry in "$@"; do names="${names}${entry%%=*} "; done
    printf '  MISSING: %s — %s\n' "${names% }" "$desc"
    MISSING_COUNT=$(( MISSING_COUNT + 1 ))
  fi
  return 0
}

validate_case_config() {
  local name="$1"
  case "$name" in
    enrollment_hydration_restart)
      report_missing LIVE_M4EXIT_AGENT_ADMIN_BASE_URL "$AGENT_ADMIN_BASE_URL" "agent admin base URL"
      report_missing LIVE_M4EXIT_AGENT_ADMIN_USER "$AGENT_ADMIN_USER" "agent admin Basic-auth user"
      report_missing LIVE_M4EXIT_AGENT_ADMIN_PASSWORD "$AGENT_ADMIN_PASSWORD" "agent admin Basic-auth password"
      report_missing LIVE_M4EXIT_EXPECTED_SHARE_COUNT "$EXPECTED_SHARE_COUNT" "declared registered share count"
      report_missing LIVE_M4EXIT_CONTROL_BASE_URL "$CONTROL_BASE_URL" "control base URL (per-share content check)"
      report_missing LIVE_M4EXIT_SHARE_CODE "$SHARE_CODE" "share code (per-share content check)"
      report_missing_any "gateway health access" "LIVE_M4EXIT_GATEWAY_HEALTH_URL=$GATEWAY_HEALTH_URL" "LIVE_M4EXIT_GATEWAY_SSH_HOST=$GATEWAY_SSH_HOST"
      report_missing_any "gateway metrics access" "LIVE_M4EXIT_GATEWAY_METRICS_URL=$GATEWAY_METRICS_URL" "LIVE_M4EXIT_GATEWAY_SSH_HOST=$GATEWAY_SSH_HOST"
      report_missing_any "agent log source" "LIVE_M4EXIT_AGENT_LOG_FILE=$AGENT_LOG_FILE" "LIVE_M4EXIT_AGENT_SSH_HOST=$AGENT_SSH_HOST"
      if [[ "$ALLOW_AGENT_RESTART" == "1" ]]; then
        report_missing LIVE_M4EXIT_AGENT_RESTART_COMMAND "$AGENT_RESTART_COMMAND" "restart command (required when LIVE_M4EXIT_ALLOW_AGENT_RESTART=1)"
      fi
      ;;
    relay_content_integrity)
      report_missing LIVE_M4EXIT_CONTROL_BASE_URL "$CONTROL_BASE_URL" "control base URL"
      report_missing LIVE_M4EXIT_SHARE_CODE "$SHARE_CODE" "share code"
      report_missing LIVE_M4EXIT_RELAY_HOST "$RELAY_HOST" "relay gateway hostname"
      report_missing LIVE_M4EXIT_ITEM_ID "$ITEM_ID" "image item id"
      report_missing LIVE_M4EXIT_ASSET_ID "$ASSET_ID" "full original asset id"
      report_missing LIVE_M4EXIT_VIDEO_ID "$VIDEO_ID" "video item id"
      report_missing LIVE_M4EXIT_EXPECTED_ITEM_COUNT "$EXPECTED_ITEM_COUNT" "expected /items count"
      if ! python_ok; then printf '  MISSING: python3 — required to parse the /items manifest\n'; MISSING_COUNT=$(( MISSING_COUNT + 1 )); fi
      ;;
    stun_observe_and_rechallenge)
      report_missing_any "control metrics access" "LIVE_M4EXIT_CONTROL_METRICS_URL=$CONTROL_METRICS_URL" "LIVE_M4EXIT_CONTROL_SSH_HOST=$CONTROL_SSH_HOST"
      report_missing_any "STUN accept-timestamp source" "LIVE_M4EXIT_STUN_JOURNAL_FILE=$STUN_JOURNAL_FILE" "LIVE_M4EXIT_CONTROL_SSH_HOST=$CONTROL_SSH_HOST"
      if ! python_ok; then printf '  MISSING: python3 — required to parse the STUN cadence\n'; MISSING_COUNT=$(( MISSING_COUNT + 1 )); fi
      ;;
    direct_path_or_failclosed)
      report_missing LIVE_M4EXIT_CONTROL_BASE_URL "$CONTROL_BASE_URL" "control base URL"
      report_missing LIVE_M4EXIT_SHARE_CODE "$SHARE_CODE" "share code"
      printf '  NOTE: LIVE_M4EXIT_AGENT_RECORD_FILE=%s is required only when prepare-route returns status=relay\n' "${AGENT_RECORD_FILE:-<unset>}"
      ;;
    lockdown_withdrawal_and_recovery)
      report_missing LIVE_M4EXIT_ALLOW_LOCKDOWN "$([[ "$ALLOW_LOCKDOWN" == "1" ]] && printf set)" "opt-in flag (requires =1)"
      report_missing LIVE_M4EXIT_CONTROL_BASE_URL "$CONTROL_BASE_URL" "control base URL"
      report_missing LIVE_M4EXIT_SHARE_CODE "$SHARE_CODE" "share code"
      report_missing LIVE_M4EXIT_AGENT_ADMIN_BASE_URL "$AGENT_ADMIN_BASE_URL" "agent admin base URL"
      report_missing LIVE_M4EXIT_AGENT_ADMIN_USER "$AGENT_ADMIN_USER" "agent admin Basic-auth user"
      report_missing LIVE_M4EXIT_AGENT_ADMIN_PASSWORD "$AGENT_ADMIN_PASSWORD" "agent admin Basic-auth password"
      report_missing_any "gateway metrics access" "LIVE_M4EXIT_GATEWAY_METRICS_URL=$GATEWAY_METRICS_URL" "LIVE_M4EXIT_GATEWAY_SSH_HOST=$GATEWAY_SSH_HOST"
      report_missing_any "frps journal access" "LIVE_M4EXIT_FRPS_JOURNAL_FILE=$FRPS_JOURNAL_FILE" "LIVE_M4EXIT_FRPS_SSH_HOST=$FRPS_SSH_HOST"
      ;;
    revocation_midstream)
      report_missing LIVE_M4EXIT_ALLOW_REVOKE "$([[ "$ALLOW_REVOKE" == "1" ]] && printf set)" "opt-in flag (requires =1)"
      report_missing LIVE_M4EXIT_REVOKE_SHARE_CODE "$REVOKE_SHARE_CODE" "share code to revoke"
      report_missing LIVE_M4EXIT_CONTROL_BASE_URL "$CONTROL_BASE_URL" "control base URL"
      report_missing LIVE_M4EXIT_AGENT_ADMIN_BASE_URL "$AGENT_ADMIN_BASE_URL" "agent admin base URL"
      report_missing LIVE_M4EXIT_AGENT_ADMIN_USER "$AGENT_ADMIN_USER" "agent admin Basic-auth user"
      report_missing LIVE_M4EXIT_AGENT_ADMIN_PASSWORD "$AGENT_ADMIN_PASSWORD" "agent admin Basic-auth password"
      report_missing_any "gateway journal access" "LIVE_M4EXIT_GATEWAY_JOURNAL_FILE=$GATEWAY_JOURNAL_FILE" "LIVE_M4EXIT_GATEWAY_SSH_HOST=$GATEWAY_SSH_HOST"
      ;;
  esac
}

run_dry_run() {
  printf '=== live-m4exit-e2e.sh dry run (nothing executed) ===\n'
  printf 'git_sha=%s\n' "$GIT_SHA"
  printf 'control_base_url=%s\n' "${CONTROL_BASE_URL:-<unset>}"
  printf 'relay_host=%s\n' "${RELAY_HOST:-<unset>}"
  printf 'control_ssh_host=%s\n' "${CONTROL_SSH_HOST:-<unset>}"
  printf 'gateway_ssh_host=%s\n' "${GATEWAY_SSH_HOST:-<unset>}"
  printf 'agent_admin_base_url=%s\n' "${AGENT_ADMIN_BASE_URL:-<unset>}"
  printf 'evidence_dir=%s\n' "$RUN_DIR"
  printf 'allow_agent_restart=%s allow_lockdown=%s allow_revoke=%s\n' "$ALLOW_AGENT_RESTART" "$ALLOW_LOCKDOWN" "$ALLOW_REVOKE"

  MISSING_COUNT=0
  local i name
  printf '\nConfiguration validation:\n'
  for i in "${!case_names[@]}"; do
    name="${case_names[$i]}"
    is_selected "$name" || continue
    printf '  case %s:\n' "$name"
    local before="$MISSING_COUNT"
    validate_case_config "$name"
    if [[ "$MISSING_COUNT" == "$before" ]]; then printf '    OK\n'; fi
  done

  printf '\nCases that would run:\n'
  for i in "${!case_names[@]}"; do
    if is_selected "${case_names[$i]}"; then
      printf '  %-38s (%s) destructive=%s\n' "${case_names[$i]}" "${case_owners[$i]}" "$(case_is_destructive "${case_names[$i]}")"
    else
      printf '  %-38s (%s) [not selected]\n' "${case_names[$i]}" "${case_owners[$i]}"
    fi
  done

  if [[ "$MISSING_COUNT" -gt 0 ]]; then
    printf '\nDRY RUN: %d missing required configuration value(s) named above — fix them and re-run (exit 2).\n' "$MISSING_COUNT"
    exit 2
  fi
  printf '\nDRY RUN: configuration complete; no case was executed — this is NOT a pass (exit 3).\n'
  exit 3
}

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
_selftest_crash() { check selftest_check PASS "recorded before crashing"; return 7; }
# A case that emits only note() must never pass.
_selftest_note_only() { note selftest_note "an informational note with no measured check"; }
# Notes do not invalidate a case that really measured a passing check.
_selftest_note_and_pass() { note selftest_note "an informational note"; check selftest_check PASS "a measured passing check"; }
# Secret-shaped wording in a check() detail must be redacted everywhere.
_selftest_secret() { check selftest_secret PASS "observed token=SUPERSECRETTOKEN123456 api_key=ABCDEFGHIJKLMNOP for /s/SECRETSHARE99"; }

run_selftest() {
  local failures=0 got
  printf '=== live-m4exit-e2e.sh selftest (pass/fail plumbing) ===\n'

  execute_case _selftest_pass;    selftest_check "trivially-true case" "$EXECUTE_RESULT" "PASS" || failures=$(( failures + 1 )); rm -rf "$EXECUTE_TMP"
  execute_case _selftest_fail;    selftest_check "trivially-false case" "$EXECUTE_RESULT" "FAIL" || failures=$(( failures + 1 )); rm -rf "$EXECUTE_TMP"
  execute_case _selftest_skip;    selftest_check "skipped case is not a pass" "$EXECUTE_RESULT" "SKIP" || failures=$(( failures + 1 )); rm -rf "$EXECUTE_TMP"
  execute_case _selftest_empty;   selftest_check "zero-check case is MISSING (never PASS)" "$EXECUTE_RESULT" "MISSING" || failures=$(( failures + 1 )); rm -rf "$EXECUTE_TMP"
  execute_case _selftest_crash;   selftest_check "case that records PASS then crashes is FAIL" "$EXECUTE_RESULT" "FAIL" || failures=$(( failures + 1 )); rm -rf "$EXECUTE_TMP"
  execute_case does_not_exist;    selftest_check "unregistered function is MISSING" "$EXECUTE_RESULT" "MISSING" || failures=$(( failures + 1 )); rm -rf "$EXECUTE_TMP"
  execute_case _selftest_note_only; selftest_check "note-only case is MISSING (never PASS)" "$EXECUTE_RESULT" "MISSING" || failures=$(( failures + 1 )); rm -rf "$EXECUTE_TMP"
  execute_case _selftest_note_and_pass; selftest_check "notes plus a measured PASS stay PASS" "$EXECUTE_RESULT" "PASS" || failures=$(( failures + 1 )); rm -rf "$EXECUTE_TMP"

  # Sanitisation negative test: a secret-shaped check() detail must come out
  # redacted in BOTH the console output and the recorded (evidence) check file,
  # including a configured literal secret.
  add_secret_literal "LITERALSECRETVALUE9876"
  local secret_out_file secret_out secret_checks
  secret_out_file="$(mktemp)"
  execute_case _selftest_secret > "$secret_out_file"
  secret_out="$(cat "$secret_out_file")"
  secret_checks="$(cat "$EXECUTE_TMP/checks")"
  rm -rf "$EXECUTE_TMP"; rm -f "$secret_out_file"
  got="redacted"
  printf '%s\n%s' "$secret_out" "$secret_checks" | grep -qE 'SUPERSECRETTOKEN123456|ABCDEFGHIJKLMNOP|SECRETSHARE99|LITERALSECRETVALUE9876' && got="raw secret leaked"
  selftest_check "check() detail has no raw secret (console + evidence)" "$got" "redacted" || failures=$(( failures + 1 ))
  got="redacted"
  printf '%s' "$secret_out" | grep -q 'REDACTED' || got="console detail not redacted"
  printf '%s' "$secret_checks" | grep -q 'REDACTED' || got="recorded detail not redacted"
  selftest_check "check() detail carries a redaction marker (console + evidence)" "$got" "redacted" || failures=$(( failures + 1 ))

  # Configured-secret literal redaction (independent of the generic patterns).
  got="$(printf 'password is %s here' "LITERALSECRETVALUE9876" | sanitize)"
  case "$got" in
    *LITERALSECRETVALUE9876*) selftest_check "configured literal secret is redacted" "$got" "redacted" || failures=$(( failures + 1 )) ;;
    *) selftest_check "configured literal secret is redacted" "redacted" "redacted" || failures=$(( failures + 1 )) ;;
  esac

  got="$(verdict_for "PASS,")";        selftest_check "verdict(all PASS)" "$got" "GREEN" || failures=$(( failures + 1 ))
  got="$(verdict_for "PASS,FAIL,")";   selftest_check "verdict(FAIL present)" "$got" "RED" || failures=$(( failures + 1 ))
  got="$(verdict_for "PASS,SKIP,")";   selftest_check "verdict(SKIP is not a pass)" "$got" "PARTIAL" || failures=$(( failures + 1 ))
  got="$(verdict_for "PASS,MISSING,")"; selftest_check "verdict(unrun/missing)" "$got" "RED" || failures=$(( failures + 1 ))

  # -------------------------------------------------------------------------
  # Lockdown safety net: the EXIT/INT/TERM unlock trap must issue exactly one
  # unlock attempt, clear the locked state, and never error under `set -u`.
  # -------------------------------------------------------------------------
  local unlock_calls=0 saved_lock_state="${M4EXIT_LOCK_STATE:-}"
  M4EXIT_LOCK_STATE="$(mktemp "${TMPDIR:-/tmp}/m4exit-selftest-lock.XXXXXX")"
  printf 'locked\n' > "$M4EXIT_LOCK_STATE"
  agent_api() { unlock_calls=$(( unlock_calls + 1 )); AGENT_HTTP_CODE=200; return 0; }
  m4exit_emergency_unlock
  selftest_check "lockdown trap issues exactly one unlock attempt" "$unlock_calls" "1" || failures=$(( failures + 1 ))
  selftest_check "lockdown trap clears the locked state on success" "$([[ -f "$M4EXIT_LOCK_STATE" ]] && printf present || printf cleared)" "cleared" || failures=$(( failures + 1 ))
  # A second trap firing in the same shutdown (INT handler, then EXIT) must not
  # unlock a second time once the state has been cleared.
  m4exit_emergency_unlock
  selftest_check "lockdown trap does not double-unlock" "$unlock_calls" "1" || failures=$(( failures + 1 ))
  # No unlock attempt when the agent was never marked locked.
  M4EXIT_LOCK_STATE="$(mktemp "${TMPDIR:-/tmp}/m4exit-selftest-lock.XXXXXX")"; rm -f "$M4EXIT_LOCK_STATE"
  m4exit_emergency_unlock
  selftest_check "lockdown trap is a no-op when not locked" "$unlock_calls" "1" || failures=$(( failures + 1 ))
  rm -f "$M4EXIT_LOCK_STATE"
  M4EXIT_LOCK_STATE="$saved_lock_state"

  # The trap body reads the state defensively, so an unset global (the exact
  # shape of the original `local`-scope bug) must not abort under `set -u`.
  local u_rc=0 u_err=""
  u_err="$( ( unset M4EXIT_LOCK_STATE; m4exit_emergency_unlock ) 2>&1 )" || u_rc=$?
  selftest_check "lockdown trap is safe under set -u with the state global unset" "${u_rc}:${u_err}" "0:" || failures=$(( failures + 1 ))

  # End-to-end proof of the REAL trap: run this script as a child with a stub
  # `curl` first on PATH and the internal trigger, then assert the trap fired
  # (conventional 128+signal status) and called the unlock endpoint exactly
  # once for each of EXIT, INT and TERM.
  local fake_bin trap_trigger child_rc child_seen fake_expected
  fake_bin="$(mktemp -d "${TMPDIR:-/tmp}/m4exit-selftest-bin.XXXXXX")"
  cat > "$fake_bin/curl" <<'FAKECURL'
#!/usr/bin/env bash
# Stub curl for the lockdown-trap selftest: count invocations and emit a 200.
n=0
[[ -f "$M4EXIT_FAKE_CURL_COUNT" ]] && n="$(cat "$M4EXIT_FAKE_CURL_COUNT")"
printf '%s' "$(( ${n:-0} + 1 ))" > "$M4EXIT_FAKE_CURL_COUNT"
out=""; hdr=""
while [[ $# -gt 0 ]]; do
  case "$1" in
    -o) out="$2"; shift 2 ;;
    -D) hdr="$2"; shift 2 ;;
    -X|-H|-u|--data|--max-time|-w) shift 2 ;;
    --silent|--show-error|--head) shift ;;
    *) shift ;;
  esac
done
[[ -n "$out" ]] && printf '{"locked":false}' > "$out"
[[ -n "$hdr" ]] && printf 'HTTP/1.1 200 OK\r\nContent-Type: application/json\r\n\r\n' > "$hdr"
printf '200'
FAKECURL
  chmod +x "$fake_bin/curl"
  for trap_trigger in exit int term; do
    case "$trap_trigger" in
      exit) fake_expected=7 ;;
      int)  fake_expected=130 ;;
      term) fake_expected=143 ;;
    esac
    printf '0' > "$fake_bin/count.$trap_trigger"
    child_rc=0
    ( PATH="$fake_bin:$PATH" M4EXIT_FAKE_CURL_COUNT="$fake_bin/count.$trap_trigger" \
        M4EXIT_INTERNAL_TRAP_SELFTEST="$trap_trigger" \
        M4EXIT_AGENT_ADMIN_BASE_URL="http://127.0.0.1:1" \
        M4EXIT_AGENT_ADMIN_USER=selftest M4EXIT_AGENT_ADMIN_PASSWORD=selftest \
        bash "$SCRIPT_SELF" ) >/dev/null 2>&1 || child_rc=$?
    child_seen="$(cat "$fake_bin/count.$trap_trigger" 2>/dev/null || printf '0')"
    selftest_check "real lockdown trap on $trap_trigger (rc=$child_rc, unlock attempts=$child_seen)" "$child_rc/$child_seen" "$fake_expected/1" || failures=$(( failures + 1 ))
  done
  rm -rf "$fake_bin"

  if [[ "$failures" -eq 0 ]]; then
    printf 'SELFTEST RESULT: PASS (0 failures) — the case runner refuses PASS for unexecuted, note-only, skipped or crashing cases, the sanitiser redacts secrets in console and evidence, and the lockdown EXIT/INT/TERM trap unlocks exactly once without aborting under set -u\n'
    exit 0
  fi
  printf 'SELFTEST RESULT: FAIL (%d assertions failed) — the harness plumbing is broken\n' "$failures"
  exit 1
}

# ---------------------------------------------------------------------------
# Main
# ---------------------------------------------------------------------------

# Internal selftest hook (never operator-facing): install the real lockdown
# trap, mark the agent locked, then terminate via EXIT/INT/TERM so --selftest
# can prove the safety net fires end-to-end with a stubbed curl on PATH.
if [[ -n "${M4EXIT_INTERNAL_TRAP_SELFTEST:-}" ]]; then
  m4exit_install_lockdown_trap
  m4exit_mark_locked
  case "$M4EXIT_INTERNAL_TRAP_SELFTEST" in
    exit) exit 7 ;;
    int)  kill -INT "$$"; sleep 5; exit 200 ;;
    term) kill -TERM "$$"; sleep 5; exit 201 ;;
    *) printf 'ERROR: unknown M4EXIT_INTERNAL_TRAP_SELFTEST=%s\n' "$M4EXIT_INTERNAL_TRAP_SELFTEST" >&2; exit 2 ;;
  esac
fi

if [[ "$LIST_ONLY" -eq 1 ]]; then
  printf '%-38s %-9s %s\n' "CASE" "OWNER" "PURPOSE"
  for i in "${!case_names[@]}"; do
    printf '%-38s %-9s %s\n' "${case_names[$i]}" "${case_owners[$i]}" "${case_purposes[$i]}"
  done
  exit 0
fi

if [[ "$MODE" == "selftest" ]]; then
  run_selftest
fi

if [[ "$DRY_RUN" -eq 1 ]]; then
  run_dry_run
fi

printf '=== ShareBridge Phase 4a M4-exit live relay e2e harness (task #15) ===\n'
printf 'Evidence root: %s (the worktree is never written)\n' "$EVIDENCE_DIR"
m4exit_install_lockdown_trap
run_all_cases
print_summary

case "$VERDICT" in
  GREEN) exit 0 ;;
  PARTIAL) exit 3 ;;
  *) exit 1 ;;
esac
