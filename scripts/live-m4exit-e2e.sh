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
#                                     is measured from the accept timestamps of
#                                     THE AGENT UNDER TEST (LIVE_M4EXIT_STUN_AGENT_ID),
#                                     with the newest acceptance proven FRESH
#                                     (no older than the 4m+jitter ceiling + the
#                                     documented tolerance) so a stale journal or
#                                     an interleaved agent cannot pass.
#   direct_path_or_failclosed         either status=direct with a direct URL
#                                     that serves, OR a fail-closed relay
#                                     fallback whose control-side diagnostics are
#                                     provably CURRENT for this request (updated
#                                     after the prepare-route call and tied to
#                                     LIVE_M4EXIT_AGENT_RECORD_AGENT_ID) and
#                                     record direct_status=relay_fallback and
#                                     the expected direct_status_reason. Never
#                                     PASS when neither is observable.
#   tunnel_recovery_frps_restart      OPT-IN. exact restart-target confirmation;
#                                     baseline tunnel online + the configured
#                                     share SERVES over the relay -> restart the
#                                     pinned frps unit while the agent's frpc
#                                     child stays ALIVE (identity re-measured
#                                     after the restart) -> observe the tunnel go
#                                     offline -> require a REPLACED PID:STARTTIME
#                                     child + a NEW agent-specific frps
#                                     proxy-registration session + online=1 +
#                                     serving content, elapsed measured after
#                                     every stage and within the monotonic bound,
#                                     with no operator action; records the
#                                     measured offline and recovery seconds. The
#                                     exact release-blocking defect this encodes:
#                                     a burned single-use credential was retried
#                                     forever until a human locked down/unlocked.
#
# HARDENING / FALSE-POSITIVE DISCIPLINE (added after the adversarial review):
#   * A state-changing case refuses to run without an explicit operator target
#     assertion (LIVE_M4EXIT_TARGET_CONFIRM == the exact control base URL).
#   * revocation_midstream requires PROOF of an in-flight transfer (0 < bytes
#     downloaded < full size AND the download process still alive) before it
#     issues the DELETE, and a NEWLY OBSERVED gateway drain line whose hostname
#     is the exact relay URL host for this run and whose streams>=1.
#   * lockdown marks the conservative "may be locked" state BEFORE the lockdown
#     request, so a signal in the response window still triggers the unlock.
#   * The lockdown withdrawal baseline requires the relay URL to SERVE (200 with
#     content) BEFORE locking, so an already-broken URL cannot "prove" it.
#
# Deliberate duplication: the runner, sanitiser and helpers below are close
# siblings of scripts/live-phase4a.sh (the M6 harness). They are NOT shared on
# purpose: each harness is an independent acceptance artifact that must be
# checkable in isolation (a shared library would couple the M4-exit evidence to
# M6 changes and make a failure ambiguous). Divergence is expected and any fix
# must be applied deliberately to one harness or the other, never assumed.
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
# RESULT / EXIT-CODE DIVERGENCE FROM scripts/live-phase4a.sh (deliberate):
#   * With a COMPLETE configuration `--dry-run` exits 3 in both harnesses.
#   * With an INCOMPLETE configuration this harness exits 2 (USAGE) after naming
#     every missing variable, whereas live-phase4a.sh always exits 3 because it
#     does not validate configuration in --dry-run. We keep 2 here on purpose:
#     an unset required input is an operator-usage error (nothing can run, so
#     "nothing executed is not a pass" would hide the real problem), and the
#     distinct code lets a wrapper tell "fix your env" from "a dry run".
#
# SAFETY:
#   * Read-only against remote systems (curl GET/POST prepare-route, ssh
#     journalctl/curl, dig, openssl). The ONLY state-changing calls are the
#     four explicitly opt-in actions: lockdown/unlock
#     (LIVE_M4EXIT_ALLOW_LOCKDOWN=1), share revocation
#     (LIVE_M4EXIT_ALLOW_REVOKE=1), an explicit agent restart
#     (LIVE_M4EXIT_ALLOW_AGENT_RESTART=1, case 1 only) and an frps restart
#     (LIVE_M4EXIT_ALLOW_FRPS_RESTART=1, tunnel_recovery_frps_restart only,
#     which never restarts the agent's frpc child). All are reversible and
#     agent-local (revoke never deletes an Immich share). Every one of them
#     additionally refuses to run unless the operator has asserted the target
#     with LIVE_M4EXIT_TARGET_CONFIRM (see the env section).
#   * The lockdown safety net is registered in the MAIN process (subshells
#     reset traps) and re-reads the locked state from a state file, so an
#     interrupted, failing or SIGTERM-ed run re-sends POST /api/unlock instead
#     of stranding a locked agent. The state is marked BEFORE the lockdown
#     request is issued and cleared only after a definitively unapplied request
#     (a 4xx rejection) or a successful unlock — the safe "may be locked"
#     direction.
#   * HYGIENE (accurate contract, not an overstatement): configured secrets
#     (admin password, share codes) and token/key/password/secret/cookie/
#     authorization/jti-shaped text — including JSON `"key": "value"` pairs —
#     are redacted before they reach the console, the evidence files or the
#     captured command/output records; private-key blocks and `/s/<code>`
#     paths are redacted too. Redaction is BEST-EFFORT pattern matching, not a
#     cryptographic guarantee, so treat evidence as operator-internal. Raw HTTP
#     capture bodies and headers live only in a per-run 0700 scratch directory
#     under umask 077 and are deleted by the EXIT/INT/TERM cleanup hook (also on
#     error); nothing is meant to survive the run.
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
#                executed is never a pass); with an incomplete configuration
#                --dry-run exits 2 (USAGE) instead — see the divergence note
#                under USAGE below
#
# Environment (all resolved at startup; a required value that is unset FAILs the
# affected case and is named in the diagnostic — never guessed). Values are
# URLs/hosts/paths/names, not secrets, except the explicit credential variables.
#
#   Control / interstitial
#     LIVE_M4EXIT_CONTROL_BASE_URL      control base URL, e.g. https://control.example (required: cases 1,2,4,5,6)
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
#     LIVE_M4EXIT_STUN_AGENT_ID         the agent api_key_id under test; ONLY its "stun observation accepted for <id>" lines are analysed (REQUIRED for case 3)
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
#     LIVE_M4EXIT_AGENT_RECORD_FILE     operator-supplied agent record with direct_status/direct_status_reason (required for a relay fallback when no command is set)
#     LIVE_M4EXIT_AGENT_RECORD_COMMAND  shell command whose STDOUT is the live agent record; the harness runs it AFTER the case's prepare-route, so a fresh record can postdate that request (preferred over the static file when both are set)
#     LIVE_M4EXIT_AGENT_RECORD_AGENT_ID the agent identifier (api_key_id or record id) under test; the record must carry it as an EXACT FIELD (REQUIRED whenever a record source is used)
#     LIVE_M4EXIT_AGENT_RECORD_FRESHNESS_TOLERANCE_S  clock-skew allowance for "updated after the request" in seconds (default 0 = strict: the record's chosen timestamp must not predate the prepare-route floor). A positive value widens the window BEFORE the request and is not recommended
#     LIVE_M4EXIT_EXPECTED_DIRECT_REASON  expected direct_status_reason for the fail-closed fallback (default probe_failed)
#       Record shape (from the control PocketBase 'agents' collection for the agent
#       under test, as JSON or key=value text): direct_status,
#       direct_status_reason, an api_key_id (or the record id) and a timestamp in
#       updated / stun_observed_at / relay_last_seen_at ("YYYY-MM-DD HH:MM:SS").
#       Exactly ONE record is required (a multi-record dump, a JSON list with more
#       than one item, or a key=value dump repeating an identity/timestamp key is
#       refused as ambiguous), the agent id must be an exact FIELD, and the chosen
#       timestamp must not predate the prepare-route request (minus the tolerance).
#       direct_status/direct_status_reason are read from that same record only.
#
#   Opt-in state-changing cases
#     LIVE_M4EXIT_TARGET_CONFIRM        REQUIRED for every case that changes state: set it to the EXACT control base URL of the disposable test stack (it must equal LIVE_M4EXIT_CONTROL_BASE_URL). Typing the target asserts that this is a disposable test deployment, not production or the M6 dark topology, before any lockdown/revoke/restart.
#     LIVE_M4EXIT_ALLOW_AGENT_RESTART   1 restarts the agent inside case 1 so the "loaded N sessions from store" line is freshly observable (default 0 => no restart)
#     LIVE_M4EXIT_AGENT_RESTART_COMMAND exact restart command, required when _ALLOW_AGENT_RESTART=1 (run on LIVE_M4EXIT_AGENT_SSH_HOST when set, else locally)
#     LIVE_M4EXIT_AGENT_RESTART_WAIT_S  seconds to wait for the hydration line after an opted-in restart (default 30)
#     LIVE_M4EXIT_ALLOW_LOCKDOWN        1 enables case 5 (lockdown/unlock; default 0 => SKIP)
#     LIVE_M4EXIT_RECOVERY_BOUND_S      documented post-unlock recovery bound (default 120)
#     LIVE_M4EXIT_TUNNEL_OFFLINE_BOUND_S documented post-lockdown tunnel-offline bound (default 30)
#     LIVE_M4EXIT_ALLOW_REVOKE          1 enables case 6 (revoke a share; default 0 => SKIP)
#     LIVE_M4EXIT_REVOKE_SHARE_CODE     the share code case 6 revokes (required when ALLOW_REVOKE=1; MUST differ from LIVE_M4EXIT_SHARE_CODE)
#     LIVE_M4EXIT_REVOKE_ASSET_ID       asset id to download mid-transfer (default: LIVE_M4EXIT_ASSET_ID)
#     LIVE_M4EXIT_REVOKE_ASSET_BYTES    full asset byte count (optional; else read from /items)
#     LIVE_M4EXIT_REVOKE_LIMIT_RATE     curl --limit-rate throttle (default 300k)
#     LIVE_M4EXIT_REVOKE_DELAY_S        seconds into the transfer before revoking (default 4)
#     LIVE_M4EXIT_REVOKE_TIMEOUT_S      bounded transfer wait before killing it (default 120)
#     LIVE_M4EXIT_REVOKE_WAIT_REREGISTER_S  >0 waits this long for the Immich poll to re-register (default 0 => note only)
#     LIVE_M4EXIT_ALLOW_FRPS_RESTART    1 enables tunnel_recovery_frps_restart (restart the pinned frps unit; default 0 => SKIP)
#     LIVE_M4EXIT_FRPS_RESTART_CONFIRM  REQUIRED for tunnel_recovery_frps_restart: the EXACT
#                                       `<ssh-host>|<systemd-unit>` the harness intends to restart
#                                       (must equal `<LIVE_M4EXIT_FRPS_SSH_HOST>|<LIVE_M4EXIT_FRPS_UNIT>`),
#                                       e.g. `root@10.0.0.5|sharebridge-relay-frps.service`. Never
#                                       restarts an unconfirmed target
#     LIVE_M4EXIT_FRPC_PID_MATCH        pgrep -f PRE-FILTER for the agent's frpc child (default frpc);
#                                       the identity is restricted to processes whose exact name is
#                                       LIVE_M4EXIT_FRPC_PID_NAME, so the harness's own wrapper
#                                       command lines can never enter the recorded identity
#     LIVE_M4EXIT_FRPC_PID_NAME         exact process name (comm) of the frpc child (default frpc)
#     LIVE_M4EXIT_FRPC_IDENTITY_CMD     OPTIONAL operator override: a shell command run on the agent
#                                       host whose stdout is EXACTLY ONE `PID:STARTTIME` line for the
#                                       agent's frpc child (use for containerised agents)
#     LIVE_M4EXIT_AGENT_PROXY_NAME      frps proxy name for the agent under test (default: derived from
#                                       the relay URL namespace as sb-<namespace>) — required for the
#                                       agent-specific post-restart fresh-session proof (the frps
#                                       `new proxy [<name>] type [tcp] success` line, logged only after
#                                       the authorization plugin admitted a fresh login)
#     LIVE_M4EXIT_WITHDRAWAL_MAX_ARTIFACT_BYTES  maximum non-2xx body bytes tolerated as a TLS teardown
#                                       artifact while locked (default 64)
#     LIVE_M4EXIT_TUNNEL_RECOVERY_BOUND_S  automatic frps-restart recovery bound seconds (default 120; LIVE_M4EXIT_TUNNEL_OFFLINE_BOUND_S bounds the offline observation)
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

# Everything this harness writes (scratch captures, the lock-state marker,
# evidence files) can contain secret-bearing bytes. Restrict the umask before
# the first mktemp; the owner can still read everything.
umask 077

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
STUN_AGENT_ID="${LIVE_M4EXIT_STUN_AGENT_ID:-}"
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
AGENT_RECORD_COMMAND="${LIVE_M4EXIT_AGENT_RECORD_COMMAND:-}"
AGENT_RECORD_AGENT_ID="${LIVE_M4EXIT_AGENT_RECORD_AGENT_ID:-}"
AGENT_RECORD_FRESHNESS_TOLERANCE_S="${LIVE_M4EXIT_AGENT_RECORD_FRESHNESS_TOLERANCE_S:-0}"
EXPECTED_DIRECT_REASON="${LIVE_M4EXIT_EXPECTED_DIRECT_REASON:-probe_failed}"

# Operator-asserted target identity for EVERY state-changing case (lockdown,
# revoke, opt-in restart). It must equal the configured control base URL, so
# the operator has to type the exact deployment they intend to mutate. This is
# the guard against accidentally pointing a destructive case at production or
# at the M6 dark topology.
TARGET_CONFIRM="${LIVE_M4EXIT_TARGET_CONFIRM:-}"

ALLOW_LOCKDOWN="${LIVE_M4EXIT_ALLOW_LOCKDOWN:-0}"
RECOVERY_BOUND_S="${LIVE_M4EXIT_RECOVERY_BOUND_S:-120}"
TUNNEL_OFFLINE_BOUND_S="${LIVE_M4EXIT_TUNNEL_OFFLINE_BOUND_S:-30}"

# Opt-in frps restart (tunnel_recovery_frps_restart): the dropped-session
# recovery scenario. It never restarts the agent's frpc child; it only restarts
# the pinned frps unit and then requires automatic recovery.
ALLOW_FRPS_RESTART="${LIVE_M4EXIT_ALLOW_FRPS_RESTART:-0}"
TUNNEL_RECOVERY_BOUND_S="${LIVE_M4EXIT_TUNNEL_RECOVERY_BOUND_S:-120}"
FRPC_PID_MATCH="${LIVE_M4EXIT_FRPC_PID_MATCH:-frpc}"
FRPC_PID_NAME="${LIVE_M4EXIT_FRPC_PID_NAME:-frpc}"
FRPC_IDENTITY_CMD="${LIVE_M4EXIT_FRPC_IDENTITY_CMD:-}"
AGENT_PROXY_NAME="${LIVE_M4EXIT_AGENT_PROXY_NAME:-}"
FRPS_RESTART_CONFIRM="${LIVE_M4EXIT_FRPS_RESTART_CONFIRM:-}"
WITHDRAWAL_MAX_ARTIFACT_BYTES="${LIVE_M4EXIT_WITHDRAWAL_MAX_ARTIFACT_BYTES:-64}"

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
  "tunnel_recovery_frps_restart|task #15|OPT-IN: exact restart-target confirmation; baseline tunnel online + serving relay -> restart the pinned frps unit with the frpc child ALIVE (identity re-measured after the restart) -> observe offline -> require a REPLACED PID:STARTTIME child + a NEW agent-specific frps proxy-registration session (global counter corroborates) + online=1 + serving content, elapsed measured after all stages and within the monotonic bound; records the measured offline and recovery seconds"
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
    -e 's#("[^"]*([Pp]assword|[Pp]asswd|[Ss]ecret|[Tt]oken|[Cc]ookie|[Aa]uthorization|[Aa][Pp][Ii][_-]?[Kk][Ee][Yy]|[Cc]lient[_-]?[Ss]ecret|[Rr]efresh[_-]?[Tt]oken|[Aa]ccess[_-]?[Tt]oken|[Pp]rivate[_-]?[Kk]ey|[Jj][Tt][Ii])[^"]*"[[:space:]]*:[[:space:]]*")[^"]*"#\1[REDACTED]"#g' \
    -e "s#([Aa][Pp][Ii][_-]?[Kk][Ee][Yy]|[Aa]uthorization|[Bb]earer|[Pp]assword|[Ss]ecret|[Cc]ookie|[Tt]oken)([[:space:]]*[=:][[:space:]]*|[[:space:]]+)[^[:space:],;\"']+#\1=[REDACTED]#g" \
    -e "s#([^A-Za-z0-9]|^)(jti|JTI)([=:][[:space:]]*)?[A-Za-z0-9._-]{8,}#\1\2=[REDACTED]#g" \
    -e "s#/s/[A-Za-z0-9_-]{6,}#/s/[REDACTED-SHARE-CODE]#g" \
    -e "s#(/api/shares/|/shares/)[A-Za-z0-9_-]{4,}#\1[REDACTED-SHARE-CODE]#g"
}

# sv: sanitise a single value for a `key=value` metadata line (newlines folded).
sv() { printf '%s' "$1" | sanitize | tr '\n' ' ' | sed -e 's/[[:space:]]*$//'; }

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

# require_target_confirmation: REFUSE a state-changing case unless the operator
# has explicitly asserted the target by typing its exact control base URL into
# LIVE_M4EXIT_TARGET_CONFIRM. This is the guard against pointing a lockdown,
# revocation or restart at production or at the M6 dark topology: the harness
# cannot know which deployment it is talking to, so the operator must confirm
# it deliberately, and the confirmation must match the configured target.
# Returns 1 (recording the FAIL) when the confirmation cannot be established.
require_target_confirmation() {
  local prefix="$1"
  if [[ -z "$TARGET_CONFIRM" ]]; then
    check "${prefix}_target_confirmed" FAIL "LIVE_M4EXIT_TARGET_CONFIRM is unset — refusing to change state without an explicit operator assertion of the target; set it to the EXACT control base URL of the disposable test stack (currently '${CONTROL_BASE_URL:-<unset>}') to confirm you intend to mutate THIS deployment and not production or the M6 dark topology"
    return 1
  fi
  if [[ -z "$CONTROL_BASE_URL" ]]; then
    check "${prefix}_target_confirmed" FAIL "LIVE_M4EXIT_TARGET_CONFIRM is set but LIVE_M4EXIT_CONTROL_BASE_URL is unset — cannot verify the confirmed target; set the control base URL first"
    return 1
  fi
  if [[ "$TARGET_CONFIRM" != "$CONTROL_BASE_URL" ]]; then
    check "${prefix}_target_confirmed" FAIL "LIVE_M4EXIT_TARGET_CONFIRM='$(sv "$TARGET_CONFIRM")' does not equal the configured target '$(sv "$CONTROL_BASE_URL")' — refusing the state-changing case (type the exact control base URL to confirm the target)"
    return 1
  fi
  check "${prefix}_target_confirmed" PASS "operator confirmed the test-stack target (LIVE_M4EXIT_TARGET_CONFIRM == the configured control base URL)"
  return 0
}

# revoke_codes_distinct <integrity-code> <revoke-code> -> equal|ok
# A pure predicate so --dry-run, the revocation case and --selftest all share
# one definition of "the integrity-test share must never be the revoke target".
revoke_codes_distinct() {
  if [[ -n "$1" && "$1" == "$2" ]]; then printf 'equal'; else printf 'ok'; fi
}

# ---------------------------------------------------------------------------
# Scratch capture files (hygiene)
# ---------------------------------------------------------------------------
#
# HTTP bodies and headers are secret-bearing (Set-Cookie, tokens, share-code
# HTML) so they must never accumulate in TMPDIR. They live in ONE per-run 0700
# scratch directory, are overwritten by later fetches, and the directory is
# removed by the EXIT/INT/TERM cleanup hook (also on error). This is the
# retention half of the hygiene contract documented in the header.

M4EXIT_SCRATCH_DIR=""

m4exit_scratch_dir() {
  # Create the per-run scratch directory in the MAIN process (called from
  # m4exit_install_lockdown_trap). It is deliberately NOT created inside a
  # command substitution: `x="$(m4exit_scratch_dir)"` would set the variable in
  # a throwaway subshell and the main-process cleanup trap would never find the
  # directory. Case subshells inherit the path by forking, and http_fetch only
  # writes into it, so exactly one directory exists per run and the trap can
  # remove it.
  if [[ -z "${M4EXIT_SCRATCH_DIR:-}" ]]; then
    M4EXIT_SCRATCH_DIR="$(mktemp -d "${TMPDIR:-/tmp}/m4exit-scratch.XXXXXX")"
    chmod 700 "$M4EXIT_SCRATCH_DIR" 2>/dev/null || true
  fi
  return 0
}

m4exit_cleanup_scratch() {
  if [[ -n "${M4EXIT_SCRATCH_DIR:-}" && -d "${M4EXIT_SCRATCH_DIR}" ]]; then
    rm -rf "$M4EXIT_SCRATCH_DIR"
  fi
  M4EXIT_SCRATCH_DIR=""
  return 0
}

m4exit_cleanup_state() {
  if [[ -n "${M4EXIT_LOCK_STATE:-}" ]]; then rm -f "$M4EXIT_LOCK_STATE"; fi
  return 0
}

# Per-case work directory (execute_case) holds copies of asset bodies and
# manifests, so it too must be removed on an interrupted run, not only on the
# normal path where run_all_cases deletes it.
M4EXIT_CASE_TMP=""

m4exit_cleanup_case_tmp() {
  if [[ -n "${M4EXIT_CASE_TMP:-}" && -d "${M4EXIT_CASE_TMP}" ]]; then
    rm -rf "$M4EXIT_CASE_TMP"
  fi
  M4EXIT_CASE_TMP=""
  return 0
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
  # http_fetch <curl args...> ; the last argument is the URL. HTTP_FETCH_MAX_TIME,
  # when set by a bounded recovery loop, caps this call by the time remaining
  # before that loop's monotonic deadline (defaults to CURL_TIMEOUT).
  local sdir err max_time="${HTTP_FETCH_MAX_TIME:-$CURL_TIMEOUT}"
  [[ "$max_time" =~ ^[0-9]+$ && "$max_time" -ge 1 ]] || max_time="$CURL_TIMEOUT"
  m4exit_scratch_dir
  sdir="$M4EXIT_SCRATCH_DIR"
  err="$sdir/curl.err"
  HTTP_BODY_FILE="$sdir/body"
  HTTP_HDR_FILE="$sdir/hdr"
  : > "$err"
  HTTP_CODE="$(curl --silent --show-error --max-time "$max_time" \
    -D "$HTTP_HDR_FILE" -o "$HTTP_BODY_FILE" -w '%{http_code}' "$@" 2>"$err")"
  HTTP_ERR="$(cat "$err")"
  rm -f "$err"
  [[ -n "$HTTP_ERR" ]] && record_out "curl stderr: $(printf '%s' "$HTTP_ERR" | tr '\n' ' ' | cut -c1-300)"
  return 0
}

http_head() {
  local sdir err
  m4exit_scratch_dir
  sdir="$M4EXIT_SCRATCH_DIR"
  err="$sdir/curl.err"
  HTTP_BODY_FILE="$sdir/body"
  HTTP_HDR_FILE="$sdir/hdr"
  : > "$err"
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
  # fetch_gateway_metrics [max-time-seconds]
  GATEWAY_METRICS_TEXT=""
  local max_time="${1:-5}"
  [[ "$max_time" =~ ^[0-9]+$ && "$max_time" -ge 1 ]] || max_time=1
  if [[ -n "$GATEWAY_METRICS_URL" ]]; then
    record_cmd "curl ${GATEWAY_METRICS_URL} (gateway /metrics)"
    HTTP_FETCH_MAX_TIME="$max_time" http_fetch "$GATEWAY_METRICS_URL"
    GATEWAY_METRICS_TEXT="$(cat "$HTTP_BODY_FILE")"
    return 0
  fi
  if [[ -n "$GATEWAY_SSH_HOST" ]]; then
    GATEWAY_METRICS_TEXT="$(remote_exec "$GATEWAY_SSH_HOST" "curl -fsS --max-time ${max_time} 'http://${GATEWAY_METRICS_ADDR}/metrics'")"
    return 0
  fi
  return 1
}

gateway_tunnel_online() {
  # gateway_tunnel_online [max-time-seconds] ; echoes the online gauge value (or
  # empty when unobservable)
  fetch_gateway_metrics "${1:-5}" >/dev/null 2>&1 || return 1
  metric_value "$GATEWAY_METRICS_TEXT" 'sharebridge_relay_tunnel_state{state="online"}'
}

# frpc_identity_cmd: prints the POSIX-sh snippet that emits one `PID:STARTTIME`
# line per matching frpc process on the agent host. `pgrep -f` is ONLY a
# candidate pre-filter (the harness's own shell/timeout/pgrep wrappers inherit a
# command line containing the pattern); the identity is restricted to processes
# whose exact name (comm) is FRPC_PID_NAME. STARTTIME prefers /proc/<pid>/stat
# field 22 and falls back to `ps -o lstart=`. LIVE_M4EXIT_FRPC_IDENTITY_CMD
# replaces the default discovery entirely for containerised agents.
frpc_identity_cmd() {
  if [[ -n "$FRPC_IDENTITY_CMD" ]]; then
    printf '%s' "$FRPC_IDENTITY_CMD"
    return 0
  fi
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

# counter_increased_verdict <before> <after> -> ok | unobservable | no-increase.
# Shared by the global reconnect counter and the agent-specific frps proxy
# registration line count; the global counter is never the sole proof.
counter_increased_verdict() {
  local before="$1" after="$2"
  if [[ ! "$before" =~ ^[0-9]+$ || ! "$after" =~ ^[0-9]+$ ]]; then printf 'unobservable'; return 0; fi
  if [[ "$after" -gt "$before" ]]; then printf 'ok'; return 0; fi
  printf 'no-increase'
}

# bound_elapsed_verdict <elapsed-s> <bound-s> -> ok | over-bound | unobservable.
# IMPORTANT 5: elapsed is measured AFTER every stage completes, so a recovery
# that only finished after the bound cannot be reported as within it.
bound_elapsed_verdict() {
  local elapsed="$1" bound="$2"
  if [[ ! "$elapsed" =~ ^[0-9]+$ || ! "$bound" =~ ^[0-9]+$ ]]; then printf 'unobservable'; return 0; fi
  if [[ "$elapsed" -le "$bound" ]]; then printf 'ok'; return 0; fi
  printf 'over-bound'
}

# deadline_remaining <deadline-epoch> -> seconds left (0 once passed). Every
# stage of a bounded loop is capped by this remaining time.
deadline_remaining() {
  local deadline="$1" now rem
  now="$(date +%s)"
  rem=$(( deadline - now ))
  if [[ "$rem" -lt 0 ]]; then rem=0; fi
  printf '%s' "$rem"
}

# frps_restart_target_verdict <confirm> <host> <unit> -> unset|target-unset|mismatch|ok.
# IMPORTANT 6: the state-changing frps restart needs its OWN exact host|unit
# confirmation, separate from the control-URL target assertion.
frps_restart_target_verdict() {
  local confirm="$1" host="$2" unit="$3"
  if [[ -z "$confirm" ]]; then printf 'unset'; return 0; fi
  if [[ -z "$host" || -z "$unit" ]]; then printf 'target-unset'; return 0; fi
  if [[ "$confirm" == "$host|$unit" ]]; then printf 'ok'; return 0; fi
  printf 'mismatch'
}

# namespace_from_relay_host <host> -> the namespace label immediately after the
# `relay` label of `<origin>.relay.<namespace>.<zone>`, or empty.
namespace_from_relay_host() {
  local host="$1" i rest
  local -a labels
  IFS='.' read -r -a labels <<< "$host"
  for i in "${!labels[@]}"; do
    if [[ "${labels[$i]}" == "relay" ]]; then
      rest="${labels[$((i + 1))]:-}"
      printf '%s' "$rest"
      return 0
    fi
  done
  printf ''
}

# tunnel_recovery_verdict <online> <pid-before> <pid-after> <reconn-before> <reconn-after> <content-ok 0|1> <agent-session-ok 0|1>
# Prints ok | offline | no-child | no-session | no-agent-session | no-content.
# A pure predicate so the live case and --selftest share one definition of
# "automatically recovered". The global reconnect counter is corroborating;
# the agent-specific frps proxy-registration proof is REQUIRED in addition.
tunnel_recovery_verdict() {
  local online="$1" pbefore="$2" pafter="$3" rbefore="$4" rafter="$5" content="$6" agent_session="$7"
  if [[ ! "$online" =~ ^[0-9]+$ || "$online" -lt 1 ]]; then printf 'offline'; return 0; fi
  if [[ -z "$pafter" || "$pafter" == "$pbefore" ]]; then printf 'no-child'; return 0; fi
  if [[ ! "$rbefore" =~ ^[0-9]+$ || ! "$rafter" =~ ^[0-9]+$ || "$rafter" -le "$rbefore" ]]; then printf 'no-session'; return 0; fi
  if [[ "$agent_session" != "1" ]]; then printf 'no-agent-session'; return 0; fi
  if [[ "$content" != "1" ]]; then printf 'no-content'; return 0; fi
  printf 'ok'
}

# lockdown_withdrawal_verdict <http-code> <body-bytes> <baseline-exact 0|1> <baseline-prefix 0|1> <baseline-same-length 0|1> <baseline-marker 0|1> <baseline-bytes> <max-artifact-bytes>
# Prints withdrawn | leaked | served | oversize | unreadable. A pure predicate so
# the live case and --selftest share one definition of "the locked relay URL no
# longer serves content".
#
# The REAL property is: while locked, a previously-issued relay URL must not
# serve its content. A curl transport failure reports http=000 but can still
# leave a small TLS-level teardown artifact in the body (observed live: 30
# bytes, curl exit 35, TLS handshake refused), so the check must NOT require
# exactly zero bytes. But the tolerance is BOUNDED (BLOCKING 4): the predicate
# requires (a) no HTTP success (status not 2xx), (b) the body not to equal the
# serving baseline, (c) the body not to be a non-empty PREFIX of the baseline
# (a truncated copy), (d) the body not to have the baseline's exact length, and
# (e) the body not to contain a baseline content marker (its first 64 bytes),
# AND (f) a byte count no larger than the documented small maximum. Anything
# bigger, or carrying baseline content in any of those shapes, is a leak.
lockdown_withdrawal_verdict() {
  local code="${1:-000}" bytes="$2" exact="$3" prefix="$4" same_length="$5" marker="$6" baseline_bytes="${7:-0}" max_bytes="${8:-64}"
  if [[ ! "$bytes" =~ ^[0-9]+$ || ! "$baseline_bytes" =~ ^[0-9]+$ || ! "$max_bytes" =~ ^[0-9]+$ ]]; then printf 'unreadable'; return 0; fi
  local flag
  for flag in "$exact" "$prefix" "$same_length" "$marker"; do
    if [[ "$flag" != "0" && "$flag" != "1" ]]; then printf 'unreadable'; return 0; fi
  done
  if [[ "$exact" == "1" || "$prefix" == "1" || "$marker" == "1" || ( "$same_length" == "1" && "$baseline_bytes" -gt 0 ) ]]; then printf 'leaked'; return 0; fi
  if [[ "$code" =~ ^2[0-9][0-9]$ ]]; then printf 'served'; return 0; fi
  if [[ "$bytes" -gt "$max_bytes" ]]; then printf 'oversize'; return 0; fi
  printf 'withdrawn'
}

# withdrawal_body_flags <body-file> <baseline-file> ; prints one `key=value` per
# line: bytes, exact, prefix, same_length, marker, baseline_bytes. Byte-safe
# (python reads bytes), so binary baselines are handled without shell quoting.
withdrawal_body_flags() {
  python3 -c '
import sys
try:
    body = open(sys.argv[1], "rb").read()
except OSError:
    print("bytes=0"); print("exact=0"); print("prefix=0"); print("same_length=0"); print("marker=0"); print("baseline_bytes=0"); sys.exit(0)
try:
    base = open(sys.argv[2], "rb").read()
except OSError:
    print("bytes=%d" % len(body)); print("exact=0"); print("prefix=0"); print("same_length=0"); print("marker=0"); print("baseline_bytes=0"); sys.exit(0)
print("bytes=%d" % len(body))
print("exact=%d" % (1 if (body and body == base) else 0))
print("prefix=%d" % (1 if (0 < len(body) < len(base) and base.startswith(body)) else 0))
print("same_length=%d" % (1 if (len(body) > 0 and len(body) == len(base)) else 0))
marker = base[:64]
print("marker=%d" % (1 if (len(body) > 0 and len(marker) > 0 and marker in body) else 0))
print("baseline_bytes=%d" % len(base))
' "$1" "$2" 2>/dev/null
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

log_line_epoch_after() {
  # log_line_epoch_after <line> <floor-epoch> <tolerance-s>
  # Prints yes|no|notimestamp (or python_unavailable). Used to prove a captured
  # gateway line is post-revoke rather than a leftover from an earlier run.
  if ! python_ok; then printf 'python_unavailable'; return 0; fi
  python3 -c '
import sys, re, datetime
line, floor, tol = sys.argv[1], int(sys.argv[2]), int(sys.argv[3])
m = re.search(r"(\d{4})-(\d{2})-(\d{2})[T ](\d{2}):(\d{2}):(\d{2})(?:\.\d+)?(Z|[+-]\d{2}:?\d{2})?", line)
if not m:
    print("notimestamp")
    sys.exit(0)
dt = datetime.datetime(int(m.group(1)), int(m.group(2)), int(m.group(3)), int(m.group(4)), int(m.group(5)), int(m.group(6)))
tz = m.group(7)
if tz and tz != "Z":
    sign = -1 if tz[0] == "-" else 1
    d = tz[1:].replace(":", "")
    dt = dt - sign * datetime.timedelta(hours=int(d[0:2]), minutes=int(d[2:4]))
epoch = int((dt - datetime.datetime(1970, 1, 1)).total_seconds())
print("yes" if epoch >= floor - tol else "no")
' "$1" "$2" "$3"
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
  # Main-process trap installation. INT/TERM unlock first and then exit with
  # the conventional 128+signal status; the EXIT trap then runs again but finds
  # the state already cleared, so exactly one unlock is issued. The scratch
  # cleanup runs after the unlock (the unlock itself performs an HTTP fetch).
  m4exit_lock_state_path >/dev/null
  m4exit_scratch_dir
  trap 'm4exit_on_exit' EXIT
  trap 'm4exit_on_exit; exit 130' INT
  trap 'm4exit_on_exit; exit 143' TERM
}

m4exit_on_exit() {
  m4exit_emergency_unlock
  m4exit_cleanup_state
  m4exit_cleanup_case_tmp
  m4exit_cleanup_scratch
}

m4exit_lockdown_request() {
  # Issue POST /api/lockdown with the conservative state marked FIRST.
  #
  # A SIGINT/SIGTERM that arrives after the server applied the lockdown but
  # before the harness read the response must still find the state file, or the
  # EXIT/INT/TERM trap would not unlock an agent that IS locked. Marking before
  # the request is the safe (over-approximating) direction; the caller clears
  # the marker only for a definitively unapplied request (a 4xx rejection) or
  # after a successful unlock.
  m4exit_mark_locked
  agent_api POST "/api/lockdown"
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
    elif ! require_target_confirmation hydration_restart; then
      restart_action="opted-in restart REFUSED (the operator target assertion failed)"
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

stun_analyze() {
  # stun_analyze <agent-id> <tolerance-s> ; journal text on stdin.
  #
  # Analyses ONLY the lines naming the agent under test and reports the newest
  # acceptance age, so a stale journal (rechallenging stopped) or an interleaved
  # unrelated agent cannot satisfy the cadence. Prints parseable key=value lines:
  #   agent_lines, accepted, newest_age_s, deltas, max_delta, in_band,
  #   over_band, band_min, band_max, fresh, freshness_reason, error
  # Kept as a pure function so --selftest can exercise freshness and isolation
  # refusal without a live control host.
  python3 -c '
import sys, re, datetime
agent = sys.argv[1]
tol = int(sys.argv[2])
def out(k, v):
    print("%s=%s" % (k, v))
lines = [l for l in sys.stdin.read().splitlines() if agent and agent in l]
out("agent_lines", len(lines))
pat = re.compile(r"(\d{4})-(\d{2})-(\d{2})[T ](\d{2}):(\d{2}):(\d{2})(?:\.\d+)?(Z|[+-]\d{2}:?\d{2})?")
ts = []
for l in lines:
    m = pat.search(l)
    if not m:
        continue
    dt = datetime.datetime(int(m.group(1)), int(m.group(2)), int(m.group(3)), int(m.group(4)), int(m.group(5)), int(m.group(6)))
    tz = m.group(7)
    if tz and tz != "Z":
        sign = -1 if tz[0] == "-" else 1
        digits = tz[1:].replace(":", "")
        off = datetime.timedelta(hours=int(digits[0:2]), minutes=int(digits[2:4]))
        dt = dt - sign * off
    ts.append(dt)
ts = sorted(set(ts))
out("accepted", len(ts))
band_min = 240 - tol
band_max = 255 + tol
out("band_min", band_min)
out("band_max", band_max)
if ts:
    age = int((datetime.datetime.utcnow() - ts[-1]).total_seconds())
    out("newest_age_s", age)
    if age < -tol:
        out("fresh", "no")
        out("freshness_reason", "newest acceptance is in the future by %ds (beyond the %ds clock-skew tolerance)" % (-age, tol))
    elif age > band_max:
        out("fresh", "no")
        out("freshness_reason", "newest acceptance is %ds old (> the %ds upper cadence bound); rechallenging may have stopped" % (age, band_max))
    else:
        out("fresh", "yes")
        out("freshness_reason", "newest acceptance is %ds old (<= the %ds upper cadence bound)" % (age, band_max))
else:
    out("newest_age_s", "-")
    out("fresh", "no")
    out("freshness_reason", "no parseable acceptance timestamp for the agent under test")
deltas = [int((b - a).total_seconds()) for a, b in zip(ts, ts[1:])]
out("deltas", " ".join(str(d) for d in deltas))
out("max_delta", max(deltas) if deltas else 0)
out("in_band", sum(1 for d in deltas if band_min <= d <= band_max))
out("over_band", sum(1 for d in deltas if d > band_max))
' "$1" "$2"
}

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
    check stun_agent_isolation FAIL "no journal available — cannot isolate the agent under test (source=${source:-<unconfigured>})"
    check stun_rechallenge_observed FAIL "no 'stun observation accepted' timestamps available (set LIVE_M4EXIT_STUN_JOURNAL_FILE or LIVE_M4EXIT_CONTROL_SSH_HOST; source=${source:-<unconfigured>}) — cadence unobservable"
    check stun_rechallenge_cadence FAIL "not evaluated: no accept timestamps"
    check stun_rechallenge_no_stall FAIL "not evaluated: no accept timestamps"
    check stun_rechallenge_fresh FAIL "not evaluated: no accept timestamps"
    return 0
  fi
  if [[ -z "$STUN_AGENT_ID" ]]; then
    check stun_agent_isolation FAIL "LIVE_M4EXIT_STUN_AGENT_ID is unset — cannot isolate the agent under test in ${source}; set it to the agent api_key_id (the value in 'stun observation accepted for <api_key_id>')"
    check stun_rechallenge_observed FAIL "not evaluated: the agent under test is not identified"
    check stun_rechallenge_cadence FAIL "not evaluated: the agent under test is not identified"
    check stun_rechallenge_no_stall FAIL "not evaluated: the agent under test is not identified"
    check stun_rechallenge_fresh FAIL "not evaluated: the agent under test is not identified"
    return 0
  fi
  if ! python_ok; then
    check stun_agent_isolation FAIL "python3 is unavailable — cannot isolate the agent under test"
    check stun_rechallenge_observed FAIL "python3 is unavailable — cannot parse the accept timestamps"
    check stun_rechallenge_cadence FAIL "not evaluated: python3 unavailable"
    check stun_rechallenge_no_stall FAIL "not evaluated: python3 unavailable"
    check stun_rechallenge_fresh FAIL "not evaluated: python3 unavailable"
    return 0
  fi

  # Analyse ONLY the lines naming the agent under test, and require the NEWEST
  # acceptance to be fresh (within 4m + jitter<=15s + tolerance). This closes
  # both holes: a stale journal can no longer pass, and interleaved agents are
  # no longer merged into one timeline.
  local analysis agent_lines accepted newest_age deltas band_min band_max in_band over_band maxd fresh fresh_reason serr
  analysis="$(printf '%s\n' "$journal" | stun_analyze "$STUN_AGENT_ID" "$STUN_CADENCE_TOLERANCE_S" 2>/dev/null || true)"
  agent_lines="$(printf '%s\n' "$analysis" | sed -n 's/^agent_lines=//p')"
  accepted="$(printf '%s\n' "$analysis" | sed -n 's/^accepted=//p')"
  newest_age="$(printf '%s\n' "$analysis" | sed -n 's/^newest_age_s=//p')"
  deltas="$(printf '%s\n' "$analysis" | sed -n 's/^deltas=//p')"
  band_min="$(printf '%s\n' "$analysis" | sed -n 's/^band_min=//p')"
  band_max="$(printf '%s\n' "$analysis" | sed -n 's/^band_max=//p')"
  in_band="$(printf '%s\n' "$analysis" | sed -n 's/^in_band=//p')"
  over_band="$(printf '%s\n' "$analysis" | sed -n 's/^over_band=//p')"
  maxd="$(printf '%s\n' "$analysis" | sed -n 's/^max_delta=//p')"
  fresh="$(printf '%s\n' "$analysis" | sed -n 's/^fresh=//p')"
  fresh_reason="$(printf '%s\n' "$analysis" | sed -n 's/^freshness_reason=//p')"

  if [[ -z "$analysis" || -z "$accepted" ]]; then
    check stun_agent_isolation FAIL "could not analyse the journal for the agent under test '$STUN_AGENT_ID' (parse failure; source=${source})"
    check stun_rechallenge_observed FAIL "not evaluated: journal analysis failed"
    check stun_rechallenge_cadence FAIL "not evaluated: journal analysis failed"
    check stun_rechallenge_no_stall FAIL "not evaluated: journal analysis failed"
    check stun_rechallenge_fresh FAIL "not evaluated: journal analysis failed"
    return 0
  fi

  if [[ "${agent_lines:-0}" -eq 0 ]]; then
    check stun_agent_isolation FAIL "no 'stun observation accepted' line mentions LIVE_M4EXIT_STUN_AGENT_ID='$(sv "$STUN_AGENT_ID")' in ${source} — the agent id is wrong or the journal has no acceptance for it"
    check stun_rechallenge_observed FAIL "not evaluated: the agent under test is absent from the source"
    check stun_rechallenge_cadence FAIL "not evaluated: the agent under test is absent from the source"
    check stun_rechallenge_no_stall FAIL "not evaluated: the agent under test is absent from the source"
    check stun_rechallenge_fresh FAIL "not evaluated: the agent under test is absent from the source"
    return 0
  fi
  check stun_agent_isolation PASS "isolated the agent under test (LIVE_M4EXIT_STUN_AGENT_ID=$(sv "$STUN_AGENT_ID")): ${agent_lines} accept line(s) in ${source}"

  if [[ "${accepted:-0}" -lt 2 ]]; then
    check stun_rechallenge_observed FAIL "only ${accepted} 'stun observation accepted' timestamp(s) for the agent under test in ${source} — cadence unprovable"
    check stun_rechallenge_cadence FAIL "not evaluated: fewer than two accept timestamps for the agent under test"
    check stun_rechallenge_no_stall FAIL "not evaluated: fewer than two accept timestamps for the agent under test"
    check stun_rechallenge_fresh "$([[ "$fresh" == "yes" ]] && printf PASS || printf FAIL)" "${fresh_reason:-no freshness information}"
    return 0
  fi

  local tol min_in_band
  tol="$STUN_CADENCE_TOLERANCE_S"
  min_in_band="$STUN_MIN_IN_BAND_DELTAS"
  local all_deltas="${deltas}"
  set_fact stun_rechallenge_deltas "${all_deltas}"
  set_fact stun_rechallenge_max_delta "$maxd"
  set_fact stun_newest_accept_age_s "$newest_age"

  check stun_rechallenge_observed PASS "$(printf '%s\n' "$deltas" | wc -w | tr -d ' ') inter-arrival delta(s) measured for the agent under test from ${source}: ${all_deltas}"
  if [[ "${in_band:-0}" -ge "$min_in_band" ]]; then
    check stun_rechallenge_cadence PASS "${in_band} delta(s) inside the ${band_min}-${band_max}s band (4m + jitter[0,15s) +/- ${tol}s)"
  else
    check stun_rechallenge_cadence FAIL "only ${in_band:-0} delta(s) inside the ${band_min}-${band_max}s band, want >= ${min_in_band} (deltas: ${all_deltas})"
  fi
  if [[ "${over_band:-0}" -eq 0 ]]; then
    check stun_rechallenge_no_stall PASS "no delta exceeds ${band_max}s (proactive rechallenge never stalled; max=${maxd}s)"
  else
    check stun_rechallenge_no_stall FAIL "${over_band} delta(s) exceed ${band_max}s (max=${maxd}s; deltas: ${all_deltas})"
  fi
  if [[ "$fresh" == "yes" ]]; then
    check stun_rechallenge_fresh PASS "${fresh_reason}"
  else
    check stun_rechallenge_fresh FAIL "${fresh_reason:-the newest acceptance could not be proven fresh}"
  fi
  return 0
}

# ===========================================================================
# CASE: direct_path_or_failclosed
# ===========================================================================

direct_record_current() {
  # direct_record_current <file> <agent-id> <floor-epoch> <tolerance-s> [field-priority]
  #
  # The direct_status/direct_status_reason fields are the LAST persisted
  # evaluation, so a stale record can otherwise "prove" a different, current
  # request. This parses EXACTLY ONE structured record and proves it is both
  # CURRENT and CORRELATED (BLOCKING 3):
  #   * a multi-record JSON list, a PocketBase `{"items":[...]}` list with more
  #     than one item, or a key=value dump repeating an identity/timestamp key
  #     is refused as `ambiguous`;
  #   * the agent under test must be an EXACT FIELD (`api_key_id` or `id`), never
  #     a substring of some other text;
  #   * the timestamp is taken from ONE field in priority order (default
  #     updated, stun_observed_at, relay_last_seen_at) — never the maximum found
  #     anywhere in the text — and must not predate the prepare-route floor
  #     (minus the documented clock-skew tolerance, default 0 = strict);
  #   * direct_status/direct_status_reason are emitted from THAT record only.
  # Prints status=<ok|ambiguous|no_agent_id|agent_mismatch|no_timestamp|stale|
  # unreadable> plus the observed values; the caller fails closed on anything but
  # ok. `field-priority` lets the frps-restart case require relay_last_seen_at.
  python3 -c '
import sys, re, json, datetime

path, want, floor, tol = sys.argv[1], sys.argv[2], int(sys.argv[3]), int(sys.argv[4])
priority = sys.argv[5].split(",") if len(sys.argv) > 5 else ["updated", "stun_observed_at", "relay_last_seen_at"]

def finish(status, count=None, observed=None, agent_field=None, ts_field=None, epoch=None, ds="", dr=""):
    out = ["status=%s" % status]
    if count is not None:
        out.append("record_count=%s" % count)
    out.append("observed_agent=%s" % (observed if observed else "?"))
    out.append("agent_field=%s" % (agent_field if agent_field else "?"))
    out.append("timestamp_field=%s" % (ts_field if ts_field else "?"))
    out.append("updated_epoch=%s" % (epoch if epoch is not None else "-"))
    out.append("direct_status=%s" % (ds if ds is not None else ""))
    out.append("direct_status_reason=%s" % (dr if dr is not None else ""))
    print("\n".join(out))
    sys.exit(0)

try:
    text = open(path, "r", errors="replace").read()
except OSError:
    finish("unreadable")

record = None
count = None
parsed = None
try:
    parsed = json.loads(text)
except Exception:
    parsed = None

if parsed is not None:
    if isinstance(parsed, dict) and isinstance(parsed.get("items"), list):
        items = parsed["items"]
        count = len(items)
        if count == 1:
            record = items[0]
    elif isinstance(parsed, list):
        count = len(parsed)
        if count == 1:
            record = parsed[0]
    elif isinstance(parsed, dict):
        count = 1
        record = parsed
    else:
        count = 0
    if record is not None and not isinstance(record, dict):
        record = None
    if count != 1 or record is None:
        finish("ambiguous", count=(count if count is not None else 0))
else:
    kv = {}
    counts = {}
    for line in text.splitlines():
        line = line.strip()
        if not line or line.startswith("#"):
            continue
        m = re.match(r"^\"?([A-Za-z_][A-Za-z0-9_]*)\"?[ \t]*[:=][ \t]*(.*)$", line)
        if not m:
            continue
        k = m.group(1)
        v = m.group(2).strip().strip(",").strip()
        if len(v) >= 2 and v[0] == "\"" and v[-1] == "\"":
            v = v[1:-1]
        counts[k] = counts.get(k, 0) + 1
        kv[k] = v
    key_fields = ["api_key_id", "id", "updated", "stun_observed_at", "relay_last_seen_at"]
    repeats = [counts.get(k, 0) for k in key_fields]
    if any(n > 1 for n in repeats):
        finish("ambiguous", count=max(repeats))
    if not kv:
        finish("unreadable", count=0)
    count = 1
    record = kv

def field(name):
    v = record.get(name)
    if v is None:
        return None
    if isinstance(v, str):
        return v
    return str(v)

agent_field = None
observed = None
matched_field = None
for f in ("api_key_id", "id"):
    v = field(f)
    if v is not None and observed is None:
        observed = v
        agent_field = f
    if want and v == want:
        matched_field = f
if want:
    if observed is None:
        finish("no_agent_id", count=count)
    if matched_field is None:
        finish("agent_mismatch", count=count, observed=observed, agent_field=agent_field)
    agent_field = matched_field
    observed = want

ts_field = None
ts_val = None
for f in priority:
    v = field(f)
    if v:
        ts_field = f
        ts_val = v
        break
ds = field("direct_status") or ""
dr = field("direct_status_reason") or ""
if not ts_val:
    finish("no_timestamp", count=count, observed=observed, agent_field=agent_field, ds=ds, dr=dr)
m = re.match(r"^(\d{4}-\d{2}-\d{2})[ T](\d{2}:\d{2}:\d{2})", ts_val.strip())
if not m:
    finish("no_timestamp", count=count, observed=observed, agent_field=agent_field, ds=ds, dr=dr)
try:
    dt = datetime.datetime.strptime(m.group(1) + " " + m.group(2), "%Y-%m-%d %H:%M:%S")
except ValueError:
    finish("no_timestamp", count=count, observed=observed, agent_field=agent_field, ds=ds, dr=dr)
epoch = int((dt - datetime.datetime(1970, 1, 1)).total_seconds())
if epoch < floor - tol:
    finish("stale", count=count, observed=observed, agent_field=agent_field, ts_field=ts_field, epoch=epoch, ds=ds, dr=dr)
finish("ok", count=count, observed=observed, agent_field=agent_field, ts_field=ts_field, epoch=epoch, ds=ds, dr=dr)
' "$1" "$2" "$3" "$4" "${5:-updated,stun_observed_at,relay_last_seen_at}"
}

# direct_record_fetch_live <command> <destination-file>
# Runs an operator-supplied command whose STDOUT is the live control-side agent
# record (PocketBase collection 'agents', as JSON or key=value text — the same
# shape record_field/direct_record_current accept: direct_status,
# direct_status_reason, an api_key_id (or record id) and an updated/
# stun_observed_at/relay_last_seen_at timestamp) and writes it to destination.
# Prints ok | failed | empty | write-failed. It is run AFTER the case's
# prepare-route so the record it returns can postdate the request floor. Any
# unusable result refuses; the caller must not fall back to a stale artifact.
direct_record_fetch_live() {
  local cmd="$1" dest="$2" out rc
  out="$(sh -c "$cmd" 2>/dev/null)"; rc=$?
  if [[ "$rc" -ne 0 ]]; then printf 'failed'; return 0; fi
  if [[ -z "$out" ]]; then printf 'empty'; return 0; fi
  if ! printf '%s' "$out" > "$dest" 2>/dev/null; then printf 'write-failed'; return 0; fi
  printf 'ok'
}

direct_path_or_failclosed() {
  require_config \
    "LIVE_M4EXIT_CONTROL_BASE_URL=${CONTROL_BASE_URL}|control base URL for prepare-route" \
    "LIVE_M4EXIT_SHARE_CODE=${SHARE_CODE}|share code for the direct-path scenario" || return 0

  # The diagnostics the fallback branch switches on represent the LAST
  # evaluation, so the freshness floor is taken immediately BEFORE the request
  # that must have produced them (see direct_record_current).
  local prepare_floor_epoch
  prepare_floor_epoch="$(date -u +%s)"
  prepare_route "$SHARE_CODE"
  if [[ "$PREPARE_HTTP_CODE" != "200" ]]; then
    check direct_prepare_status FAIL "prepare-route -> ${PREPARE_HTTP_CODE:-000} (want 200; body: $(sanitize < "$PREPARE_BODY_FILE" | tr -d '\n' | cut -c1-160)) ${HTTP_ERR:-}"
    check direct_path_observable FAIL "not evaluated: the prepare-route request did not succeed"
    check direct_failclosed_diagnostics FAIL "not evaluated: the prepare-route request did not succeed"
    return 0
  fi
  check direct_prepare_status PASS "prepare-route -> 200"
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
      # Live-fetch path: when an operator supplies LIVE_M4EXIT_AGENT_RECORD_COMMAND
      # it is run HERE (after the prepare-route above) and its stdout is the
      # record under test, so the freshness requirement is satisfiable without
      # hand-exporting a transient record. It takes precedence over a static
      # LIVE_M4EXIT_AGENT_RECORD_FILE when both are set.
      if [[ -n "$AGENT_RECORD_COMMAND" ]]; then
        local live_record_file="$M4EXIT_SCRATCH_DIR/agent-record-live" fetch_status
        fetch_status="$(direct_record_fetch_live "$AGENT_RECORD_COMMAND" "$live_record_file")"
        if [[ "$fetch_status" != "ok" ]]; then
          check direct_failclosed_diagnostics FAIL "LIVE_M4EXIT_AGENT_RECORD_COMMAND did not produce a usable live record (${fetch_status}); expected its stdout to be the control agent record (PocketBase 'agents') for this agent, fetched AFTER the prepare-route call"
          check direct_failclosed_current FAIL "not evaluated: live record fetch ${fetch_status}"
          check direct_status_relay_fallback FAIL "not evaluated: live record fetch ${fetch_status}"
          check direct_status_reason FAIL "not evaluated: live record fetch ${fetch_status}"
          return 0
        fi
        AGENT_RECORD_FILE="$live_record_file"
        note direct_record_source "live fetch via LIVE_M4EXIT_AGENT_RECORD_COMMAND (run after prepare-route)"
      elif [[ -n "$AGENT_RECORD_FILE" ]]; then
        note direct_record_source "operator-supplied LIVE_M4EXIT_AGENT_RECORD_FILE (must already be current for this request)"
      fi
      if [[ -z "$AGENT_RECORD_FILE" ]]; then
        check direct_failclosed_diagnostics FAIL "prepare-route reported the fail-closed relay fallback but neither LIVE_M4EXIT_AGENT_RECORD_COMMAND (live fetch, preferred) nor LIVE_M4EXIT_AGENT_RECORD_FILE (operator-supplied static record) is set, so the control-side direct diagnostics cannot be verified. Expected record shape: JSON or key=value text from the control agents record (PocketBase collection 'agents') containing direct_status, direct_status_reason, an api_key_id (or the record id) and an updated/stun_observed_at/relay_last_seen_at timestamp. EXACTLY ONE record is required (a multi-record JSON list, an {\"items\":[...]} list with more than one item, or a key=value dump repeating an identity/timestamp key is refused as ambiguous); the agent id must be an exact api_key_id/id FIELD. With the command, its stdout is written to a temp file and validated after the prepare-route above so the timestamp can postdate the request; with the file, the operator must export it IMMEDIATELY AFTER the prepare-route call. Verdicts: the record must carry the agent id given in LIVE_M4EXIT_AGENT_RECORD_AGENT_ID and the chosen timestamp field (updated first) must not predate the request floor minus LIVE_M4EXIT_AGENT_RECORD_FRESHNESS_TOLERANCE_S (default 0 = strict; else direct_failclosed_current FAILs); then direct_status=relay_fallback AND direct_status_reason=<LIVE_M4EXIT_EXPECTED_DIRECT_REASON, default probe_failed> => PASS; a different direct_status, reason, stale timestamp, foreign agent id or ambiguous dump => FAIL; and neither is needed when prepare-route returns status=direct (that is judged by direct_route_serves instead)"
        return 0
      fi
      if [[ ! -r "$AGENT_RECORD_FILE" ]]; then
        check direct_failclosed_diagnostics FAIL "the agent record source is not readable (LIVE_M4EXIT_AGENT_RECORD_FILE=${AGENT_RECORD_FILE:-<unset>})"
        return 0
      fi
      if [[ -z "$AGENT_RECORD_AGENT_ID" ]]; then
        check direct_failclosed_current FAIL "LIVE_M4EXIT_AGENT_RECORD_AGENT_ID is unset — the supplied record cannot be tied to the agent under test, so its diagnostics could belong to a different agent; set it to the agent api_key_id (or the agent record id) under test"
        check direct_status_relay_fallback FAIL "not evaluated: the agent under test is not identified"
        check direct_status_reason FAIL "not evaluated: the agent under test is not identified"
        return 0
      fi
      local ds dr cur cur_status updated_epoch observed_agent timestamp_field record_count
      if ! python_ok; then
        check direct_failclosed_current FAIL "python3 is unavailable — cannot prove the supplied record is current for this request; install python3 or run on a host that has it"
        check direct_status_relay_fallback FAIL "not evaluated: freshness/correlation unprovable"
        check direct_status_reason FAIL "not evaluated: freshness/correlation unprovable"
        return 0
      fi
      cur="$(direct_record_current "$AGENT_RECORD_FILE" "$AGENT_RECORD_AGENT_ID" "$prepare_floor_epoch" "$AGENT_RECORD_FRESHNESS_TOLERANCE_S" 2>/dev/null || true)"
      cur_status="$(printf '%s\n' "$cur" | sed -n 's/^status=//p')"
      updated_epoch="$(printf '%s\n' "$cur" | sed -n 's/^updated_epoch=//p')"
      observed_agent="$(printf '%s\n' "$cur" | sed -n 's/^observed_agent=//p')"
      timestamp_field="$(printf '%s\n' "$cur" | sed -n 's/^timestamp_field=//p')"
      record_count="$(printf '%s\n' "$cur" | sed -n 's/^record_count=//p')"
      # BLOCKING 3: direct_status/direct_status_reason come from the SAME parsed
      # record the freshness/correlation proof validated, never from a separate
      # first-match scan over the whole file.
      ds="$(printf '%s\n' "$cur" | sed -n 's/^direct_status=//p')"
      dr="$(printf '%s\n' "$cur" | sed -n 's/^direct_status_reason=//p')"
      set_fact direct_record_updated_epoch "${updated_epoch:-<absent>}"
      set_fact direct_record_observed_agent "${observed_agent:-<absent>}"
      set_fact direct_status "${ds:-<absent>}"
      set_fact direct_status_reason "${dr:-<absent>}"
      case "$cur_status" in
        ok)
          check direct_failclosed_current PASS "the single record's ${timestamp_field} timestamp (updated_epoch=${updated_epoch}) is at/after the prepare-route floor ${prepare_floor_epoch} (tolerance ${AGENT_RECORD_FRESHNESS_TOLERANCE_S}s) and its agent id is the exact field value for the agent under test"
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
        agent_mismatch)
          check direct_failclosed_current FAIL "the supplied record's agent field ('${observed_agent:-<none>}') is not the agent under test '$(sv "$AGENT_RECORD_AGENT_ID")' — refusing to attribute another agent's diagnostics; supply the record for the agent under test"
          check direct_status_relay_fallback FAIL "not evaluated: the record belongs to a different agent"
          check direct_status_reason FAIL "not evaluated: the record belongs to a different agent"
          ;;
        no_agent_id)
          check direct_failclosed_current FAIL "the supplied record carries no api_key_id/id field, so it cannot be tied to the agent under test '$(sv "$AGENT_RECORD_AGENT_ID")'"
          check direct_status_relay_fallback FAIL "not evaluated: the record has no agent field"
          check direct_status_reason FAIL "not evaluated: the record has no agent field"
          ;;
        ambiguous)
          check direct_failclosed_current FAIL "the supplied record source is not exactly ONE structured record (record_count='${record_count:-?}') — a multi-record dump or a repeated identity/timestamp key cannot be attributed to this request; export the single agents record for the agent under test"
          check direct_status_relay_fallback FAIL "not evaluated: the record source is ambiguous"
          check direct_status_reason FAIL "not evaluated: the record source is ambiguous"
          ;;
        stale)
          check direct_failclosed_current FAIL "the record's ${timestamp_field:-chosen} timestamp (updated_epoch='${updated_epoch}') predates the prepare-route floor ${prepare_floor_epoch} (tolerance ${AGENT_RECORD_FRESHNESS_TOLERANCE_S}s) — it reflects an EARLIER evaluation, not this request; export the agent record immediately after the prepare-route call (probe_failed requires the probe to have run during that request)"
          check direct_status_relay_fallback FAIL "not evaluated: the diagnostics are not provably current for this request"
          check direct_status_reason FAIL "not evaluated: the diagnostics are not provably current for this request"
          ;;
        no_timestamp)
          check direct_failclosed_current FAIL "the single record carries no usable updated/stun_observed_at/relay_last_seen_at timestamp — freshness cannot be established; export the live PocketBase agents record (it carries an `updated` stamp) rather than a hand-written key=value file"
          check direct_status_relay_fallback FAIL "not evaluated: freshness unprovable"
          check direct_status_reason FAIL "not evaluated: freshness unprovable"
          ;;
        *)
          check direct_failclosed_current FAIL "could not evaluate the supplied record '${AGENT_RECORD_FILE}' (status='${cur_status:-<none>}') — is it the agents record for the deployment under test?"
          check direct_status_relay_fallback FAIL "not evaluated: freshness/correlation unprovable"
          check direct_status_reason FAIL "not evaluated: freshness/correlation unprovable"
          ;;
      esac
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

  # (0) Operator target assertion: refuse to change state without it. A lockdown
  # pointed at production or the M6 dark topology is exactly the accident this
  # guard exists to prevent.
  require_target_confirmation lockdown || return 0

  # (1) Baseline healthy: relay route + tunnel online + the relay URL SERVES.
  # Without a serving baseline the post-lock "no content" result proves
  # nothing, so a broken baseline refuses the state change outright.
  local baseline_url="" baseline_bytes=0 baseline_body_file=""
  if ! prepare_relay_or_fail "$SHARE_CODE" lockdown_baseline; then
    check lockdown_baseline_serves FAIL "not evaluated: no baseline relay URL was issued (prepare-route failed) — refusing to lock down without a serving baseline"
    return 0
  fi
  baseline_url="$PREPARE_RELAY_URL"
  http_fetch ${RELAY_TLS_ARGS[@]+"${RELAY_TLS_ARGS[@]}"} "$baseline_url"
  [[ -f "$HTTP_BODY_FILE" ]] && baseline_bytes="$(file_bytes "$HTTP_BODY_FILE")"
  if [[ "$HTTP_CODE" == "200" && "${baseline_bytes:-0}" -gt 0 ]]; then
    # Preserve the SERVING baseline body: the withdrawal check compares the
    # post-lock body against it, and HTTP_BODY_FILE is overwritten by the next
    # fetch.
    baseline_body_file="$M4EXIT_SCRATCH_DIR/lockdown-baseline-body"
    cp "$HTTP_BODY_FILE" "$baseline_body_file"
    check lockdown_baseline_serves PASS "pre-lockdown relay URL serves: 200 with ${baseline_bytes} bytes (baseline for the withdrawal comparison)"
  else
    check lockdown_baseline_serves FAIL "pre-lockdown relay URL -> ${HTTP_CODE:-000} with ${baseline_bytes:-0} bytes (want 200 with content) — an already-broken URL cannot prove withdrawal; refusing to lock down ${HTTP_ERR:-}"
    return 0
  fi
  local online
  online="$(gateway_tunnel_online 2>/dev/null || true)"
  if [[ "${online:-0}" -ge 1 ]]; then
    check lockdown_baseline_tunnel PASS "baseline tunnel online=${online}"
  else
    check lockdown_baseline_tunnel FAIL "baseline tunnel online=${online:-<absent>} (want >=1) — refusing to lock down without an online baseline tunnel"
    return 0
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

  # (3) Lockdown. The conservative "may be locked" marker is written BEFORE the
  # request (m4exit_lockdown_request), so a SIGINT/SIGTERM arriving after the
  # server applied the lockdown but before the response was read still leaves
  # the state file for the EXIT/INT/TERM trap to unlock. The marker is cleared
  # only for a definitively unapplied request (a 4xx rejection) or a successful
  # unlock later in this case.
  m4exit_lockdown_request
  if [[ "$AGENT_HTTP_CODE" == "200" ]]; then
    check lockdown_applied PASS "POST /api/lockdown -> 200 ($(json_str "$(cat "$AGENT_BODY_FILE")" locked))"
  else
    check lockdown_applied FAIL "POST /api/lockdown -> ${AGENT_HTTP_CODE:-000} ${HTTP_ERR:-}"
    case "$AGENT_HTTP_CODE" in
      4*)
        m4exit_mark_unlocked
        note lockdown_lock_state "lockdown request was rejected with ${AGENT_HTTP_CODE} (definitively not applied) — conservative locked marker cleared"
        ;;
      *)
        note lockdown_lock_state "lockdown result was ambiguous (${AGENT_HTTP_CODE:-000}) — keeping the conservative locked marker so the EXIT/INT/TERM trap still attempts an unlock"
        ;;
    esac
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

  # (7) A fetch of the previously-issued relay URL yields no content. The REAL
  # property is asserted by lockdown_withdrawal_verdict: no HTTP success AND no
  # serving-baseline body (a small TLS-level teardown artifact is tolerated —
  # the pre-fix byte-exact-zero requirement false-REDded on it).
  if [[ -n "$baseline_url" ]]; then
    http_fetch ${RELAY_TLS_ARGS[@]+"${RELAY_TLS_ARGS[@]}"} "$baseline_url"
    # BLOCKING 4: a bounded tolerance, and any shape that carries (or truncates)
    # the real baseline content is a leak regardless of HTTP status.
    local bytes=0 flags="" exact=0 prefix=0 same_length=0 marker=0 baseline_b=0
    if [[ -f "$HTTP_BODY_FILE" && -n "$baseline_body_file" && -r "$baseline_body_file" ]] && python_ok; then
      flags="$(withdrawal_body_flags "$HTTP_BODY_FILE" "$baseline_body_file")"
      bytes="$(printf '%s\n' "$flags" | sed -n 's/^bytes=//p')"
      exact="$(printf '%s\n' "$flags" | sed -n 's/^exact=//p')"
      prefix="$(printf '%s\n' "$flags" | sed -n 's/^prefix=//p')"
      same_length="$(printf '%s\n' "$flags" | sed -n 's/^same_length=//p')"
      marker="$(printf '%s\n' "$flags" | sed -n 's/^marker=//p')"
      baseline_b="$(printf '%s\n' "$flags" | sed -n 's/^baseline_bytes=//p')"
    fi
    local withdrawal_verdict
    withdrawal_verdict="$(lockdown_withdrawal_verdict "${HTTP_CODE:-000}" "$bytes" "$exact" "$prefix" "$same_length" "$marker" "$baseline_b" "$WITHDRAWAL_MAX_ARTIFACT_BYTES")"
    case "$withdrawal_verdict" in
      withdrawn)
        check lockdown_relay_withdrawn PASS "relay fetch while locked -> ${HTTP_CODE:-000} with ${bytes} bytes (<= ${WITHDRAWAL_MAX_ARTIFACT_BYTES} tolerated), no ${baseline_bytes}-byte baseline content served (a small TLS-level teardown artifact is tolerated)" ;;
      leaked)
        check lockdown_relay_withdrawn FAIL "relay fetch while locked still carried baseline content (http ${HTTP_CODE:-000}, ${bytes} bytes, baseline=${baseline_b:-${baseline_bytes}} bytes, exact=${exact} prefix=${prefix} same_length=${same_length} marker=${marker}) — content leaked while locked" ;;
      served)
        check lockdown_relay_withdrawn FAIL "relay fetch while locked -> ${HTTP_CODE:-000} with ${bytes} bytes: an HTTP success while locked (want non-2xx/absent)" ;;
      oversize)
        check lockdown_relay_withdrawn FAIL "relay fetch while locked -> ${HTTP_CODE:-000} with ${bytes} bytes, larger than the documented ${WITHDRAWAL_MAX_ARTIFACT_BYTES}-byte teardown-artifact maximum — refusing to tolerate a large non-2xx body" ;;
      *)
        check lockdown_relay_withdrawn FAIL "relay fetch while locked was not evaluable (http='${HTTP_CODE:-}' bytes='${bytes}' exact='${exact}' prefix='${prefix}' marker='${marker}') — failing closed" ;;
    esac
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
  # IMPORTANT 5 (same defect class as the frps-recovery loop): enforce a
  # monotonic deadline, cap each stage by the remaining time, and measure the
  # elapsed seconds AFTER every stage has completed.
  local rec_ok=0 rec_secs=-1 stage="" remaining t_unlock
  t_unlock="$(date +%s)"
  local rec_deadline=$(( t_unlock + RECOVERY_BOUND_S ))
  while :; do
    remaining="$(deadline_remaining "$rec_deadline")"
    [[ "$remaining" -gt 0 ]] || break
    online="$(gateway_tunnel_online "$remaining" 2>/dev/null || true)"
    remaining="$(deadline_remaining "$rec_deadline")"
    if [[ "$remaining" -gt 0 ]]; then
      HTTP_FETCH_MAX_TIME="$remaining" prepare_route "$SHARE_CODE"
    fi
    if [[ -n "$online" && "$online" -ge 1 && "$PREPARE_HTTP_CODE" == "200" && "$PREPARE_STATUS" == "relay" ]]; then
      rec_ok=1; stage="tunnel online + prepare-route relay"; break
    fi
    sleep 1
  done
  rec_secs="$(( $(date +%s) - t_unlock ))"
  set_fact lockdown_recovery_seconds "$rec_secs"
  local rec_bound_verdict
  rec_bound_verdict="$(bound_elapsed_verdict "$rec_secs" "$RECOVERY_BOUND_S")"
  if [[ "$rec_ok" == "1" && "$rec_bound_verdict" == "ok" ]]; then
    check lockdown_recovery PASS "recovered ${rec_secs}s after unlock (${stage}; measured after all stages, bound ${RECOVERY_BOUND_S}s)"
  elif [[ "$rec_ok" == "1" ]]; then
    check lockdown_recovery FAIL "all recovery conditions were observed, but the measured elapsed time (${rec_secs}s, after every stage completed) exceeds the bound ${RECOVERY_BOUND_S}s — refusing a late recovery"
  else
    check lockdown_recovery FAIL "no recovery within ${RECOVERY_BOUND_S}s after unlock (elapsed ${rec_secs}s, last tunnel online=${online:-<absent>}, prepare http=${PREPARE_HTTP_CODE:-000} status='${PREPARE_STATUS:-<none>}')"
  fi
  note lockdown_recovery_reference "the live run measured ~3s warm and ~60s cold for post-unlock recovery"
  return 0
}

# ===========================================================================
# CASE: revocation_midstream  (OPT-IN)
# ===========================================================================

# revoke_inflight_verdict <started-bytes> <full-bytes> <alive 0|1>
# Prints ok | zero-bytes | completed | not-alive. A pure predicate for the
# BLOCKING-1 rule: a mid-stream revocation can only be proven when the
# throttled download had transferred some bytes, had not finished, and the curl
# process was still running at revoke time. Shared with --selftest.
revoke_inflight_verdict() {
  if [[ "${1:-0}" -le 0 ]]; then printf 'zero-bytes'; return 0; fi
  if [[ "${1:-0}" -ge "${2:-0}" ]]; then printf 'completed'; return 0; fi
  if [[ "${3:-0}" != "1" ]]; then printf 'not-alive'; return 0; fi
  printf 'ok'
}

# drain_new_lines <exact-host> <baseline-text> ; candidate lines on stdin.
# Emits only the candidate lines that mention the exact host AND are absent
# verbatim from the pre-revoke baseline, i.e. genuinely newly observed.
drain_new_lines() {
  local host="$1" baseline="$2" l
  while IFS= read -r l; do
    [[ -n "$l" && "$l" == *"$host"* ]] || continue
    printf '%s\n' "$baseline" | grep -qxF -- "$l" && continue
    printf '%s\n' "$l"
  done
}

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

  # (0a) The integrity-test share must never be the revocation target.
  if [[ "$(revoke_codes_distinct "$SHARE_CODE" "$REVOKE_SHARE_CODE")" == "equal" ]]; then
    check revoke_share_code_distinct FAIL "LIVE_M4EXIT_REVOKE_SHARE_CODE equals LIVE_M4EXIT_SHARE_CODE — refusing to revoke the integrity-test share; set LIVE_M4EXIT_REVOKE_SHARE_CODE to a DIFFERENT share"
    return 0
  fi
  check revoke_share_code_distinct PASS "the revocation share code differs from the integrity share code"

  # (0b) Operator target assertion before any state change.
  require_target_confirmation revoke || return 0

  # (1) Relay route for the share to revoke.
  if ! prepare_relay_or_fail "$REVOKE_SHARE_CODE" revoke; then
    return 0
  fi
  local relay_url="$PREPARE_RELAY_URL"
  local revoke_relay_host
  revoke_relay_host="$(url_host "$relay_url")"
  if [[ -z "$revoke_relay_host" ]]; then
    check revoke_gateway_drain FAIL "cannot determine the relay host from relay_url='$(sv "$relay_url")' — the drain line cannot be attributed to this run's route"
    return 0
  fi
  # The pre-revoke baseline of matching drain lines is captured immediately
  # BEFORE the DELETE below, so a leftover line from an earlier run (or a line
  # emitted for any other reason during setup) can never be mistaken for the
  # NEW close caused by this mutation.
  local drain_baseline=""

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
  local dl_alive=0
  if kill -0 "$dl_pid" 2>/dev/null; then dl_alive=1; fi
  record_out "revoke preflight: started=${started_bytes} alive=${dl_alive} full=${full_size} host=${revoke_relay_host}"

  # PROOF OF AN IN-FLIGHT TRANSFER is a precondition for the state-changing
  # DELETE: 0 bytes (the stale case the review found) or an already-finished
  # transfer cannot demonstrate mid-stream revocation. Fail closed and do NOT
  # revoke when the precondition is not met.
  local inflight_verdict inflight_fail=""
  inflight_verdict="$(revoke_inflight_verdict "${started_bytes:-0}" "$full_size" "$dl_alive")"
  case "$inflight_verdict" in
    zero-bytes)
      inflight_fail="the throttled download had downloaded ${started_bytes} bytes after ${REVOKE_DELAY_S}s (want 0 < bytes < ${full_size}) — no in-flight transfer was established; raise LIVE_M4EXIT_REVOKE_DELAY_S or lower LIVE_M4EXIT_REVOKE_LIMIT_RATE" ;;
    completed)
      inflight_fail="the transfer had already completed (started=${started_bytes} >= full=${full_size}) — it was not mid-stream; decrease LIVE_M4EXIT_REVOKE_DELAY_S or lower LIVE_M4EXIT_REVOKE_LIMIT_RATE" ;;
    not-alive)
      inflight_fail="the download process had already exited before the revoke (started=${started_bytes}/${full_size}) — no transfer was in flight at revoke time; raise LIVE_M4EXIT_REVOKE_TIMEOUT_S or lower LIVE_M4EXIT_REVOKE_DELAY_S" ;;
    *)
      inflight_fail="" ;;
  esac
  if [[ -n "$inflight_fail" ]]; then
    kill "$dl_pid" 2>/dev/null || true
    wait "$dl_pid" 2>/dev/null || true
    check revoke_inflight_transfer FAIL "${inflight_fail}. Refusing the state-changing DELETE because a mid-stream revocation cannot be proven"
    check revoke_transfer_truncated FAIL "not evaluated: no in-flight transfer was established"
    check revoke_gateway_drain FAIL "not evaluated: the DELETE was not issued"
    return 0
  fi
  check revoke_inflight_transfer PASS "in-flight transfer proven: ${started_bytes}/${full_size} bytes downloaded and the curl process is still alive at revoke time"

  # (4) Revoke mid-transfer.
  local revoke_epoch
  drain_baseline="$(gateway_journal_grep 'revoked route closed established streams' 2>/dev/null | grep -F "$revoke_relay_host" || true)"
  revoke_epoch="$(date -u +%s)"
  agent_api DELETE "/api/shares/${REVOKE_SHARE_CODE}"
  if [[ "$AGENT_HTTP_CODE" == "200" ]]; then
    check revoke_applied PASS "DELETE /api/shares/<code> -> 200 mid-transfer (${started_bytes} bytes downloaded and the transfer still running at revoke)"
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

  # (7) Gateway route-revocation drain log line. The line must be NEWLY
  # OBSERVED (absent from the pre-revoke baseline), name the EXACT relay host
  # for this run, report streams>=1 and - when it carries a timestamp - be
  # post-revoke. A leftover `streams=1` line can no longer complete the case.
  local drain_line="" drain_wait=0
  if ! gateway_journal_available; then
    check revoke_gateway_drain FAIL "no gateway journal access: set LIVE_M4EXIT_GATEWAY_JOURNAL_FILE or LIVE_M4EXIT_GATEWAY_SSH_HOST"
  else
    while [[ "$drain_wait" -le 20 ]]; do
      drain_line="$(gateway_journal_grep 'revoked route closed established streams' 2>/dev/null | drain_new_lines "$revoke_relay_host" "$drain_baseline" | tail -n1)"
      [[ -n "$drain_line" ]] && break
      sleep 1
      drain_wait=$(( drain_wait + 1 ))
    done
    if [[ -z "$drain_line" ]]; then
      check revoke_gateway_drain FAIL "no NEWLY OBSERVED 'revoked route closed established streams' line for hostname '${revoke_relay_host}' in the gateway journal within 20s (the pre-revoke baseline already held $(printf '%s\n' "$drain_baseline" | grep -c . || true) matching line(s), so an old line is not proof); remedy: capture the gateway journal during this run or verify that the exact relay URL host '${revoke_relay_host}' is the hostname logged on the drain line"
    else
      local streams ts_verdict
      streams="$(printf '%s' "$drain_line" | grep -oE 'streams=[0-9]+' | grep -oE '[0-9]+' | tail -n1)"
      ts_verdict="$(log_line_epoch_after "$drain_line" "$revoke_epoch" 10)"
      set_fact revoke_drain_streams "${streams:-<absent>}"
      set_fact revoke_drain_host "$revoke_relay_host"
      if [[ "$ts_verdict" != "yes" ]]; then
        check revoke_gateway_drain FAIL "gateway drain line for hostname '${revoke_relay_host}' is not provably post-revoke (timestamp verdict='${ts_verdict}'; revoke floor=${revoke_epoch}): $(printf '%s' "$drain_line" | cut -c1-200) — capture the journal with timestamps (journalctl -o short-iso) so recency can be proven"
      elif [[ -n "$streams" && "$streams" -ge 1 ]]; then
        check revoke_gateway_drain PASS "gateway log: NEW drain line for the exact hostname '${revoke_relay_host}' post-revoke: streams=${streams}"
      else
        check revoke_gateway_drain FAIL "gateway drain line observed for hostname '${revoke_relay_host}' but streams='${streams:-<absent>}' (want >=1): $(printf '%s' "$drain_line" | cut -c1-200)"
      fi
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
# CASE: tunnel_recovery_frps_restart  (OPT-IN)
#
# Encodes the release-blocking dropped-session defect: an frps restart drops
# the frpc session, but frp v0.71 never exits the rejected child (the reconnect
# path hard-codes loginFailExit=false), so the agent retries a single-use,
# now-burned credential forever and the relay stays down until an operator
# locks down/unlocks. The corrected agent keys recovery on the real frpc client
# rejection line and replaces the still-running child with a fresh credential.
# This case restarts the pinned frps unit while the agent's frpc child stays
# ALIVE and requires automatic recovery within the bound. It NEVER restarts the
# agent or its child.
# ===========================================================================

tunnel_recovery_frps_restart() {
  if [[ "$ALLOW_FRPS_RESTART" != "1" ]]; then
    check tunnel_recovery_opt_in SKIP "LIVE_M4EXIT_ALLOW_FRPS_RESTART!=1 — this case restarts the pinned frps unit (a state-changing action) to force the dropped-session scenario; opt in explicitly"
    return 0
  fi
  require_config \
    "LIVE_M4EXIT_CONTROL_BASE_URL=${CONTROL_BASE_URL}|control base URL for prepare-route" \
    "LIVE_M4EXIT_SHARE_CODE=${SHARE_CODE}|share code for the baseline and post-recovery relay fetch" || return 0

  # (0) Operator target assertion before any state change (a frps restart IS a
  # state-changing action).
  require_target_confirmation tunnel_recovery || return 0

  # (0b) IMPORTANT 6: a SEPARATE, exact confirmation of the SSH host + systemd
  # unit that will actually be restarted. The control-URL assertion above does
  # not cover the destructive target.
  local frps_target="${FRPS_SSH_HOST:-${GATEWAY_SSH_HOST:-}}"
  local target_verdict
  target_verdict="$(frps_restart_target_verdict "$FRPS_RESTART_CONFIRM" "$frps_target" "$FRPS_UNIT")"
  case "$target_verdict" in
    ok)
      check tunnel_recovery_frps_target_confirmed PASS "operator confirmed the exact restart target '${frps_target}|${FRPS_UNIT}' (LIVE_M4EXIT_FRPS_RESTART_CONFIRM)" ;;
    unset)
      check tunnel_recovery_frps_target_confirmed FAIL "LIVE_M4EXIT_FRPS_RESTART_CONFIRM is unset — refusing to restart frps without an explicit assertion of the SSH host + systemd unit; set it to the EXACT '${frps_target}|${FRPS_UNIT}'"
      return 0 ;;
    target-unset)
      check tunnel_recovery_frps_target_confirmed FAIL "LIVE_M4EXIT_FRPS_RESTART_CONFIRM is set but no frps SSH target is configured (set LIVE_M4EXIT_FRPS_SSH_HOST or LIVE_M4EXIT_GATEWAY_SSH_HOST)"
      return 0 ;;
    *)
      check tunnel_recovery_frps_target_confirmed FAIL "LIVE_M4EXIT_FRPS_RESTART_CONFIRM='$(sv "$FRPS_RESTART_CONFIRM")' does not equal the intended restart target '${frps_target}|${FRPS_UNIT}' — refusing the state-changing restart"
      return 0 ;;
  esac
  set_fact tunnel_recovery_frps_restart_target "${frps_target}|${FRPS_UNIT}"
  if ! frps_journal_available; then
    check tunnel_recovery_frps_journal FAIL "no frps journal source: set LIVE_M4EXIT_FRPS_JOURNAL_FILE, or LIVE_M4EXIT_FRPS_SSH_HOST / LIVE_M4EXIT_GATEWAY_SSH_HOST for `journalctl -u ${FRPS_UNIT}` — the agent-specific post-restart session proof is unobservable without it"
    return 0
  fi

  # (1) Baseline: tunnel online AND the configured share actually SERVES over
  # the relay. A broken baseline cannot prove a recovery.
  local online reconnects_before baseline_url baseline_bytes=0
  local frpc_before_raw frpc_before_status frpc_before
  online="$(gateway_tunnel_online 2>/dev/null || true)"
  if [[ ! "$online" =~ ^[0-9]+$ || "$online" -lt 1 ]]; then
    check tunnel_recovery_baseline_tunnel FAIL "baseline sharebridge_relay_tunnel_state{state=\"online\"}='${online:-<absent>}' (want >=1) — refusing to restart frps without an online baseline tunnel (metrics source: ${GATEWAY_METRICS_URL:-ssh ${GATEWAY_SSH_HOST:-<unconfigured>} ${GATEWAY_METRICS_ADDR}})"
    return 0
  fi
  check tunnel_recovery_baseline_tunnel PASS "baseline sharebridge_relay_tunnel_state{state=\"online\"}=${online}"

  if ! prepare_relay_or_fail "$SHARE_CODE" tunnel_recovery_baseline; then
    check tunnel_recovery_baseline_relay FAIL "not evaluated: no serving baseline relay URL (prepare-route failed) — refusing to restart frps"
    return 0
  fi
  baseline_url="$PREPARE_RELAY_URL"
  http_fetch ${RELAY_TLS_ARGS[@]+"${RELAY_TLS_ARGS[@]}"} "$baseline_url"
  [[ -f "$HTTP_BODY_FILE" ]] && baseline_bytes="$(file_bytes "$HTTP_BODY_FILE")"
  if [[ "$HTTP_CODE" == "200" && "${baseline_bytes:-0}" -gt 0 ]]; then
    check tunnel_recovery_baseline_relay PASS "baseline relay fetch $(url_host "$baseline_url") -> 200 with ${baseline_bytes} bytes"
    set_fact tunnel_recovery_baseline_bytes "$baseline_bytes"
  else
    check tunnel_recovery_baseline_relay FAIL "baseline relay fetch -> ${HTTP_CODE:-000} with ${baseline_bytes:-0} bytes (want 200 with content) ${HTTP_ERR:-} — refusing to restart frps"
    return 0
  fi

  # BLOCKING 1: a STABLE single-child identity. The -f pre-filter can return the
  # harness's own wrapper PIDs, so the identity is restricted to the exact frpc
  # process name and carries its start time; 0 or >1 candidates fail closed.
  frpc_before_raw="$(frpc_identities)"
  frpc_before_status="$(frpc_identity_status "$frpc_before_raw")"
  if [[ "$frpc_before_status" != "one" ]]; then
    check tunnel_recovery_child_before FAIL "expected EXACTLY ONE frpc child identity on ${AGENT_SSH_HOST:-the harness host} (pre-filter '${FRPC_PID_MATCH}', exact name '${FRPC_PID_NAME}'), observed status=${frpc_before_status} identities='$(printf '%s' "$frpc_before_raw" | tr '\n' ' ')' — refusing the restart because the child identity is ambiguous; set LIVE_M4EXIT_FRPC_PID_NAME, LIVE_M4EXIT_FRPC_PID_MATCH, or LIVE_M4EXIT_FRPC_IDENTITY_CMD"
    return 0
  fi
  frpc_before="$(printf '%s\n' "$frpc_before_raw" | head -n1)"
  check tunnel_recovery_child_before PASS "exactly one agent frpc child identity before the restart: ${frpc_before}"

  # BLOCKING 2: the agent-specific fresh-session proof. frps logs
  # `new proxy [<proxy>] type [tcp] success` only AFTER the authorization plugin
  # admitted the login for THIS agent's proxy name. The proxy name defaults to
  # sb-<namespace> derived from the baseline relay URL host.
  local proxy_name="${AGENT_PROXY_NAME:-}" namespace="" proxy_pat proxy_lines_before=""
  if [[ -z "$proxy_name" ]]; then
    namespace="$(namespace_from_relay_host "$(url_host "$baseline_url")")"
    [[ -n "$namespace" ]] && proxy_name="sb-${namespace}"
  fi
  if [[ -z "$proxy_name" ]]; then
    check tunnel_recovery_agent_proxy_name FAIL "could not determine the frps proxy name for the agent under test: set LIVE_M4EXIT_AGENT_PROXY_NAME (expected sb-<namespace>), or use a relay URL host of the form <origin>.relay.<namespace>.<zone> — required for the agent-specific post-restart session proof"
    return 0
  fi
  proxy_pat="new proxy [${proxy_name}] type [tcp] success"
  set_fact tunnel_recovery_agent_proxy_name "$proxy_name"
  proxy_lines_before="$(frps_journal_grep "$proxy_pat" 2>/dev/null | grep -F "$proxy_pat" | grep -c . || true)"
  proxy_lines_before="${proxy_lines_before:-0}"
  set_fact tunnel_recovery_frps_proxy_lines_before "$proxy_lines_before"
  # A zero baseline is not itself a failure: the agent's registration may simply
  # be older than the journal tail window, and the proof is the INCREASE after
  # the restart (a genuinely unreadable journal yields 0 after as well and still
  # fails the agent-specific check).
  if [[ "$proxy_lines_before" -ge 1 ]]; then
    check tunnel_recovery_baseline_agent_session PASS "baseline frps proxy-registration count for '${proxy_name}' is ${proxy_lines_before}"
  else
    note tunnel_recovery_baseline_agent_session "no '${proxy_pat}' line in the current frps journal window (count=0) — the agent-specific proof is the increase after the restart"
  fi

  fetch_gateway_metrics >/dev/null 2>&1 || true
  reconnects_before="$(metric_value "${GATEWAY_METRICS_TEXT:-}" 'sharebridge_relay_tunnel_reconnects_total')"

  # (2) Force the exact drop: restart the pinned frps unit over SSH while the
  # agent's frpc child keeps running.
  local t_restart restart_out restart_rc
  t_restart="$(date +%s)"
  restart_out="$(remote_exec "$frps_target" "systemctl restart $FRPS_UNIT")"; restart_rc=$?
  if [[ "$restart_rc" -ne 0 ]]; then
    check tunnel_recovery_frps_restart FAIL "systemctl restart ${FRPS_UNIT} on ${frps_target} exit=${restart_rc}: $(printf '%s' "$restart_out" | tr '\n' ' ' | cut -c1-200)"
    return 0
  fi
  check tunnel_recovery_frps_restart PASS "restarted ${FRPS_UNIT} on ${frps_target}"

  # BLOCKING 1: re-measure the child identity immediately after the restart
  # (never infer 'left alive' from the stale pre-restart value).
  local alive_raw="" alive_status="none" alive_id="" alive_try
  for alive_try in 1 2 3; do
    alive_raw="$(frpc_identities)"
    alive_status="$(frpc_identity_status "$alive_raw")"
    [[ "$alive_status" == "one" ]] && break
    sleep 0.2
  done
  alive_id="$(printf '%s\n' "$alive_raw" | head -n1)"
  if [[ "$alive_status" != "one" ]]; then
    check tunnel_recovery_child_alive FAIL "could not observe EXACTLY ONE frpc child identity immediately after the restart (status=${alive_status}, identities='$(printf '%s' "$alive_raw" | tr '\n' ' ')')"
  elif [[ "$alive_id" == "$frpc_before" ]]; then
    check tunnel_recovery_child_alive PASS "the pre-restart frpc child (${frpc_before}) was still alive when first re-measured after the frps restart — the harness left it running"
  else
    check tunnel_recovery_child_alive FAIL "the frpc child identity changed to '${alive_id}' before it could be re-measured after the restart (was '${frpc_before}') — cannot prove the pre-restart child was left alive"
  fi

  # (3) Observe the tunnel go offline, bounded, with every stage capped by the
  # remaining time.
  local offline_ok=0 offline_secs=-1 offline_deadline=$(( t_restart + TUNNEL_OFFLINE_BOUND_S ))
  while [[ "$(deadline_remaining "$offline_deadline")" -gt 0 ]]; do
    online="$(gateway_tunnel_online "$(deadline_remaining "$offline_deadline")" 2>/dev/null || true)"
    if [[ "$online" =~ ^[0-9]+$ && "$online" -eq 0 ]]; then
      offline_ok=1; offline_secs="$(( $(date +%s) - t_restart ))"; break
    fi
    sleep 0.5
  done
  set_fact tunnel_recovery_offline_seconds "$offline_secs"
  if [[ "$offline_ok" == "1" ]]; then
    check tunnel_recovery_tunnel_offline PASS "tunnel reported online=0 ${offline_secs}s after the frps restart (bound ${TUNNEL_OFFLINE_BOUND_S}s)"
  else
    check tunnel_recovery_tunnel_offline FAIL "tunnel never reported online=0 within ${TUNNEL_OFFLINE_BOUND_S}s of the frps restart (last online='${online:-<absent>}')"
  fi

  # (4) Require automatic recovery within a MONOTONIC bound, with no operator
  # action. prepare_route (no check recording) is used inside the polling loop
  # so a transient offline response cannot record a spurious FAIL; the measured
  # assertions are recorded once, after the loop. Every stage is capped by the
  # remaining time and the elapsed seconds are measured after all stages.
  local rec_ok=0 rec_secs=-1 content_ok=0 frpc_after_raw="" frpc_after_status="none" frpc_after=""
  local reconnects_after="" proxy_lines_after="" agent_session_ok=0 remaining verdict
  local deadline=$(( t_restart + TUNNEL_RECOVERY_BOUND_S ))
  while :; do
    remaining="$(deadline_remaining "$deadline")"
    [[ "$remaining" -gt 0 ]] || break
    # fetch_gateway_metrics must run in THIS shell (not a command substitution),
    # or its GATEWAY_METRICS_TEXT assignment would be lost to the subshell and
    # the reconnect counter would be read stale.
    fetch_gateway_metrics "$remaining" >/dev/null 2>&1 || true
    online="$(metric_value "${GATEWAY_METRICS_TEXT:-}" 'sharebridge_relay_tunnel_state{state="online"}')"
    frpc_after_raw="$(frpc_identities)"
    frpc_after_status="$(frpc_identity_status "$frpc_after_raw")"
    frpc_after=""
    [[ "$frpc_after_status" == "one" ]] && frpc_after="$(printf '%s\n' "$frpc_after_raw" | head -n1)"
    reconnects_after="$(metric_value "${GATEWAY_METRICS_TEXT:-}" 'sharebridge_relay_tunnel_reconnects_total')"
    content_ok=0
    if [[ "$online" =~ ^[0-9]+$ && "$online" -ge 1 ]]; then
      remaining="$(deadline_remaining "$deadline")"
      if [[ "$remaining" -gt 0 ]]; then
        HTTP_FETCH_MAX_TIME="$remaining" prepare_route "$SHARE_CODE"
      fi
      if [[ "$PREPARE_HTTP_CODE" == "200" && "$PREPARE_STATUS" == "relay" && -n "$PREPARE_RELAY_URL" ]]; then
        remaining="$(deadline_remaining "$deadline")"
        if [[ "$remaining" -gt 0 ]]; then
          http_fetch ${RELAY_TLS_ARGS[@]+"${RELAY_TLS_ARGS[@]}"} "$PREPARE_RELAY_URL"
          local rec_bytes=0
          [[ -f "$HTTP_BODY_FILE" ]] && rec_bytes="$(file_bytes "$HTTP_BODY_FILE")"
          [[ "$HTTP_CODE" == "200" && "${rec_bytes:-0}" -gt 0 ]] && content_ok=1
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
    if [[ "$(tunnel_recovery_verdict "$online" "$frpc_before" "$frpc_after" "${reconnects_before:-}" "${reconnects_after:-}" "$content_ok" "$agent_session_ok")" == "ok" ]]; then
      rec_ok=1; break
    fi
    sleep 1
  done
  # IMPORTANT 5: measure elapsed AFTER every stage has completed.
  rec_secs="$(( $(date +%s) - t_restart ))"

  set_fact tunnel_recovery_seconds "$rec_secs"
  set_fact tunnel_recovery_frps_proxy_lines_after "${proxy_lines_after:-<absent>}"
  verdict="$(tunnel_recovery_verdict "$online" "$frpc_before" "$frpc_after" "${reconnects_before:-}" "${reconnects_after:-}" "$content_ok" "$agent_session_ok")"

  if [[ "$frpc_after_status" == "one" && -n "$frpc_after" && "$frpc_after" != "$frpc_before" ]]; then
    check tunnel_recovery_new_child PASS "frpc child replaced automatically: ${frpc_before} -> ${frpc_after}"
  elif [[ "$frpc_after_status" != "one" ]]; then
    check tunnel_recovery_new_child FAIL "the post-recovery frpc child identity is ambiguous (status=${frpc_after_status}, identities='$(printf '%s' "$frpc_after_raw" | tr '\n' ' ')')"
  else
    check tunnel_recovery_new_child FAIL "frpc child identity did not change (before='${frpc_before}' after='${frpc_after:-<absent>}') — the child was not replaced without operator action"
  fi
  if [[ "$(counter_increased_verdict "${reconnects_before:-}" "${reconnects_after:-}")" == "ok" ]]; then
    check tunnel_recovery_new_session PASS "a NEW tunnel session was established (sharebridge_relay_tunnel_reconnects_total ${reconnects_before} -> ${reconnects_after}) — corroborating evidence that a fresh credential was requested and accepted"
  else
    check tunnel_recovery_new_session FAIL "no new tunnel session observed (reconnects ${reconnects_before:-<absent>} -> ${reconnects_after:-<absent>})"
  fi
  if [[ "$agent_session_ok" == "1" ]]; then
    check tunnel_recovery_agent_session PASS "agent-specific fresh session: a NEW frps '${proxy_pat}' line was logged after the restart (count ${proxy_lines_before} -> ${proxy_lines_after}); frps logs this only after the authorization plugin admitted a login for this agent's proxy name"
  else
    check tunnel_recovery_agent_session FAIL "no NEW frps '${proxy_pat}' line after the restart (before=${proxy_lines_before} after='${proxy_lines_after:-<unobservable>}') — the global reconnect counter alone can be another agent's session, so the agent-specific admission proof is required; check LIVE_M4EXIT_AGENT_PROXY_NAME and the frps journal source"
  fi
  if [[ "$online" =~ ^[0-9]+$ && "$online" -ge 1 ]]; then
    check tunnel_recovery_tunnel_online PASS "gateway tunnel presence restored: sharebridge_relay_tunnel_state{state=\"online\"}=${online}"
  else
    check tunnel_recovery_tunnel_online FAIL "gateway tunnel still offline at the end of the bound (online='${online:-<absent>}')"
  fi
  if [[ "$content_ok" == "1" ]]; then
    check tunnel_recovery_relay_content PASS "relay content served after automatic recovery ($(url_host "$PREPARE_RELAY_URL") -> 200 with content)"
  else
    check tunnel_recovery_relay_content FAIL "relay content did not serve after recovery (prepare http=${PREPARE_HTTP_CODE:-000} status='${PREPARE_STATUS:-<none>}')"
  fi
  local bound_verdict
  bound_verdict="$(bound_elapsed_verdict "$rec_secs" "$TUNNEL_RECOVERY_BOUND_S")"
  if [[ "$rec_ok" == "1" && "$bound_verdict" == "ok" ]]; then
    check tunnel_recovery_within_bound PASS "automatic recovery (new child + fresh session + agent-specific session + online + serving content) completed ${rec_secs}s after the frps restart, measured after all stages (bound ${TUNNEL_RECOVERY_BOUND_S}s)"
  elif [[ "$rec_ok" == "1" ]]; then
    check tunnel_recovery_within_bound FAIL "all recovery conditions were observed, but the measured elapsed time (${rec_secs}s, after every stage completed) exceeds the bound ${TUNNEL_RECOVERY_BOUND_S}s — refusing a late recovery"
  else
    check tunnel_recovery_within_bound FAIL "no full automatic recovery within ${TUNNEL_RECOVERY_BOUND_S}s of the frps restart (elapsed ${rec_secs}s, verdict='${verdict}')"
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
    tunnel_recovery_frps_restart) [[ "$ALLOW_FRPS_RESTART" == "1" ]] && printf 'yes' || printf 'no' ;;
    *) printf 'no' ;;
  esac
}

case_opt_in_flag() {
  case "$1" in
    enrollment_hydration_restart) printf 'LIVE_M4EXIT_ALLOW_AGENT_RESTART=%s' "$ALLOW_AGENT_RESTART" ;;
    lockdown_withdrawal_and_recovery) printf 'LIVE_M4EXIT_ALLOW_LOCKDOWN=%s' "$ALLOW_LOCKDOWN" ;;
    revocation_midstream) printf 'LIVE_M4EXIT_ALLOW_REVOKE=%s' "$ALLOW_REVOKE" ;;
    tunnel_recovery_frps_restart) printf 'LIVE_M4EXIT_ALLOW_FRPS_RESTART=%s' "$ALLOW_FRPS_RESTART" ;;
    *) printf 'none' ;;
  esac
}

execute_case() {
  # execute_case <fn-name> ; sets EXECUTE_RESULT and EXECUTE_DETAIL
  local name="$1" tmp
  tmp="$(mktemp -d "${TMPDIR:-/tmp}/m4exit-case.XXXXXX")"
  M4EXIT_CASE_TMP="$tmp"
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
    printf 'control_base_url=%s\n' "$(sv "${CONTROL_BASE_URL:-<unset>}")"
    printf 'share_code=%s\n' "${SHARE_CODE:+[REDACTED-SHARE-CODE]}"
    printf 'revoke_share_code=%s\n' "${REVOKE_SHARE_CODE:+[REDACTED-SHARE-CODE]}"
    printf 'target_confirm=%s\n' "$([[ -n "$TARGET_CONFIRM" ]] && printf 'asserted' || printf 'unset')"
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
  printf 'control_base_url=%s\n' "$(sv "${CONTROL_BASE_URL:-<unset>}")"
  printf 'relay_host=%s\n' "$(sv "${RELAY_HOST:-<unset>}")"
  printf 'allow_lockdown=%s\n' "$ALLOW_LOCKDOWN"
  printf 'allow_revoke=%s\n' "$ALLOW_REVOKE"
  printf 'allow_agent_restart=%s\n' "$ALLOW_AGENT_RESTART"
  printf 'allow_frps_restart=%s\n' "$ALLOW_FRPS_RESTART"

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
    M4EXIT_CASE_TMP=""
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
    printf 'control_base_url=%s\n' "$(sv "${CONTROL_BASE_URL:-<unset>}")"
    printf 'relay_host=%s\n' "$(sv "${RELAY_HOST:-<unset>}")"
    printf 'control_ssh_host=%s\n' "$(sv "${CONTROL_SSH_HOST:-<unset>}")"
    printf 'gateway_ssh_host=%s\n' "$(sv "${GATEWAY_SSH_HOST:-<unset>}")"
    printf 'agent_admin_base_url=%s\n' "$(sv "${AGENT_ADMIN_BASE_URL:-<unset>}")"
    printf 'agent_admin_password=%s\n' "$([[ -n "$AGENT_ADMIN_PASSWORD" ]] && printf '[REDACTED]' || printf '<unset>')"
    printf 'share_code=%s\n' "$([[ -n "$SHARE_CODE" ]] && printf '[REDACTED-SHARE-CODE]' || printf '<unset>')"
    printf 'revoke_share_code=%s\n' "$([[ -n "$REVOKE_SHARE_CODE" ]] && printf '[REDACTED-SHARE-CODE]' || printf '<unset>')"
    printf 'target_confirm=%s\n' "$([[ -n "$TARGET_CONFIRM" ]] && printf 'asserted (equal to the configured control base URL)' || printf 'unset')"
    printf 'destructive_target_confirmed=%s\n' "$([[ -n "$TARGET_CONFIRM" && "$TARGET_CONFIRM" == "$CONTROL_BASE_URL" ]] && printf yes || printf no)"
    printf 'allow_lockdown=%s (destructive-but-reversible: case lockdown_withdrawal_and_recovery)\n' "$ALLOW_LOCKDOWN"
    printf 'allow_revoke=%s (destructive-but-agent-local: case revocation_midstream)\n' "$ALLOW_REVOKE"
    printf 'allow_agent_restart=%s (opt-in agent restart inside case enrollment_hydration_restart)\n' "$ALLOW_AGENT_RESTART"
    printf 'allow_frps_restart=%s (opt-in frps restart inside case tunnel_recovery_frps_restart; never restarts the agent child)\n' "$ALLOW_FRPS_RESTART"
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
  printf 'control_base_url=%s\n' "$(sv "${CONTROL_BASE_URL:-PENDING (LIVE_M4EXIT_CONTROL_BASE_URL unset)}")"
  printf 'relay_host=%s\n' "$(sv "${RELAY_HOST:-PENDING (LIVE_M4EXIT_RELAY_HOST unset)}")"
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
  printf 'stun_newest_accept_age_s=%s\n' "$(fact_get stun_newest_accept_age_s)"
  printf 'direct_status=%s\n' "$(fact_get direct_status)"
  printf 'direct_status_reason=%s\n' "$(fact_get direct_status_reason)"
  printf 'direct_record_updated_epoch=%s\n' "$(fact_get direct_record_updated_epoch)"
  printf 'direct_record_observed_agent=%s\n' "$(fact_get direct_record_observed_agent)"
  printf 'lockdown_tunnel_offline_seconds=%s\n' "$(fact_get lockdown_tunnel_offline_seconds)"
  printf 'lockdown_recovery_seconds=%s\n' "$(fact_get lockdown_recovery_seconds)"
  printf 'tunnel_recovery_baseline_bytes=%s\n' "$(fact_get tunnel_recovery_baseline_bytes)"
  printf 'tunnel_recovery_offline_seconds=%s\n' "$(fact_get tunnel_recovery_offline_seconds)"
  printf 'tunnel_recovery_seconds=%s\n' "$(fact_get tunnel_recovery_seconds)"
  printf 'revoke_download_bytes=%s\n' "$(fact_get revoke_download_bytes)"
  printf 'revoke_asset_full_bytes=%s\n' "$(fact_get revoke_asset_full_bytes)"
  printf 'revoke_drain_streams=%s\n' "$(fact_get revoke_drain_streams)"
  printf 'revoke_drain_host=%s\n' "$(fact_get revoke_drain_host)"
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

# _validate_target_confirm_match: dry-run validation helper. Increments
# MISSING_COUNT when the asserted target does not equal the configured control
# base URL (both set).
_validate_target_confirm_match() {
  if [[ -n "$TARGET_CONFIRM" && -n "$CONTROL_BASE_URL" && "$TARGET_CONFIRM" != "$CONTROL_BASE_URL" ]]; then
    printf '  INVALID: LIVE_M4EXIT_TARGET_CONFIRM=%s does not equal LIVE_M4EXIT_CONTROL_BASE_URL=%s\n' "$(sv "$TARGET_CONFIRM")" "$(sv "$CONTROL_BASE_URL")"
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
        report_missing LIVE_M4EXIT_TARGET_CONFIRM "$TARGET_CONFIRM" "operator target assertion for the opted-in restart (must equal LIVE_M4EXIT_CONTROL_BASE_URL)"
        _validate_target_confirm_match
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
      report_missing LIVE_M4EXIT_STUN_AGENT_ID "$STUN_AGENT_ID" "agent api_key_id under test (only its accept lines are analysed)"
      if ! python_ok; then printf '  MISSING: python3 — required to parse the STUN cadence\n'; MISSING_COUNT=$(( MISSING_COUNT + 1 )); fi
      ;;
    direct_path_or_failclosed)
      report_missing LIVE_M4EXIT_CONTROL_BASE_URL "$CONTROL_BASE_URL" "control base URL"
      report_missing LIVE_M4EXIT_SHARE_CODE "$SHARE_CODE" "share code"
      printf '  NOTE: LIVE_M4EXIT_AGENT_RECORD_FILE=%s is required only when prepare-route returns status=relay\n' "${AGENT_RECORD_FILE:-<unset>}"
      printf '  NOTE: LIVE_M4EXIT_AGENT_RECORD_AGENT_ID=%s is required with that file so the record can be tied to the agent under test\n' "${AGENT_RECORD_AGENT_ID:-<unset>}"
      if ! python_ok; then printf '  NOTE: python3 is unavailable — required to prove the relay-fallback record is current\n'; fi
      ;;
    lockdown_withdrawal_and_recovery)
      report_missing LIVE_M4EXIT_ALLOW_LOCKDOWN "$([[ "$ALLOW_LOCKDOWN" == "1" ]] && printf set)" "opt-in flag (requires =1)"
      report_missing LIVE_M4EXIT_CONTROL_BASE_URL "$CONTROL_BASE_URL" "control base URL"
      report_missing LIVE_M4EXIT_SHARE_CODE "$SHARE_CODE" "share code"
      report_missing LIVE_M4EXIT_TARGET_CONFIRM "$TARGET_CONFIRM" "operator target assertion (must equal LIVE_M4EXIT_CONTROL_BASE_URL)"
      _validate_target_confirm_match
      report_missing LIVE_M4EXIT_AGENT_ADMIN_BASE_URL "$AGENT_ADMIN_BASE_URL" "agent admin base URL"
      report_missing LIVE_M4EXIT_AGENT_ADMIN_USER "$AGENT_ADMIN_USER" "agent admin Basic-auth user"
      report_missing LIVE_M4EXIT_AGENT_ADMIN_PASSWORD "$AGENT_ADMIN_PASSWORD" "agent admin Basic-auth password"
      report_missing_any "gateway metrics access" "LIVE_M4EXIT_GATEWAY_METRICS_URL=$GATEWAY_METRICS_URL" "LIVE_M4EXIT_GATEWAY_SSH_HOST=$GATEWAY_SSH_HOST"
      report_missing_any "frps journal access" "LIVE_M4EXIT_FRPS_JOURNAL_FILE=$FRPS_JOURNAL_FILE" "LIVE_M4EXIT_FRPS_SSH_HOST=$FRPS_SSH_HOST"
      ;;
    revocation_midstream)
      report_missing LIVE_M4EXIT_ALLOW_REVOKE "$([[ "$ALLOW_REVOKE" == "1" ]] && printf set)" "opt-in flag (requires =1)"
      report_missing LIVE_M4EXIT_REVOKE_SHARE_CODE "$REVOKE_SHARE_CODE" "share code to revoke"
      if [[ "$(revoke_codes_distinct "$SHARE_CODE" "$REVOKE_SHARE_CODE")" == "equal" ]]; then
        printf '  INVALID: LIVE_M4EXIT_REVOKE_SHARE_CODE equals LIVE_M4EXIT_SHARE_CODE — the integrity-test share must never be revoked\n'
        MISSING_COUNT=$(( MISSING_COUNT + 1 ))
      fi
      report_missing LIVE_M4EXIT_TARGET_CONFIRM "$TARGET_CONFIRM" "operator target assertion (must equal LIVE_M4EXIT_CONTROL_BASE_URL)"
      _validate_target_confirm_match
      report_missing LIVE_M4EXIT_CONTROL_BASE_URL "$CONTROL_BASE_URL" "control base URL"
      report_missing LIVE_M4EXIT_AGENT_ADMIN_BASE_URL "$AGENT_ADMIN_BASE_URL" "agent admin base URL"
      report_missing LIVE_M4EXIT_AGENT_ADMIN_USER "$AGENT_ADMIN_USER" "agent admin Basic-auth user"
      report_missing LIVE_M4EXIT_AGENT_ADMIN_PASSWORD "$AGENT_ADMIN_PASSWORD" "agent admin Basic-auth password"
      report_missing_any "gateway journal access" "LIVE_M4EXIT_GATEWAY_JOURNAL_FILE=$GATEWAY_JOURNAL_FILE" "LIVE_M4EXIT_GATEWAY_SSH_HOST=$GATEWAY_SSH_HOST"
      ;;
    tunnel_recovery_frps_restart)
      report_missing LIVE_M4EXIT_ALLOW_FRPS_RESTART "$([[ "$ALLOW_FRPS_RESTART" == "1" ]] && printf set)" "opt-in flag (requires =1)"
      report_missing LIVE_M4EXIT_TARGET_CONFIRM "$TARGET_CONFIRM" "operator target assertion (must equal LIVE_M4EXIT_CONTROL_BASE_URL)"
      _validate_target_confirm_match
      report_missing LIVE_M4EXIT_FRPS_RESTART_CONFIRM "$FRPS_RESTART_CONFIRM" "exact '<frps-ssh-host>|<frps-unit>' restart-target assertion (must equal LIVE_M4EXIT_FRPS_SSH_HOST|LIVE_M4EXIT_FRPS_UNIT)"
      report_missing LIVE_M4EXIT_CONTROL_BASE_URL "$CONTROL_BASE_URL" "control base URL"
      report_missing LIVE_M4EXIT_SHARE_CODE "$SHARE_CODE" "share code for the baseline and post-recovery relay fetch"
      report_missing_any "gateway metrics access" "LIVE_M4EXIT_GATEWAY_METRICS_URL=$GATEWAY_METRICS_URL" "LIVE_M4EXIT_GATEWAY_SSH_HOST=$GATEWAY_SSH_HOST"
      report_missing_any "frps SSH target" "LIVE_M4EXIT_FRPS_SSH_HOST=$FRPS_SSH_HOST" "LIVE_M4EXIT_GATEWAY_SSH_HOST=$GATEWAY_SSH_HOST"
      report_missing_any "frps journal access" "LIVE_M4EXIT_FRPS_JOURNAL_FILE=$FRPS_JOURNAL_FILE" "LIVE_M4EXIT_FRPS_SSH_HOST=$FRPS_SSH_HOST"
      if [[ -z "$AGENT_PROXY_NAME" ]]; then
        printf '  NOTE: LIVE_M4EXIT_AGENT_PROXY_NAME is unset; the case derives sb-<namespace> from the relay URL host (set it explicitly when the host shape is non-standard)\n'
      fi
      ;;
  esac
}

run_dry_run() {
  printf '=== live-m4exit-e2e.sh dry run (nothing executed) ===\n'
  printf 'git_sha=%s\n' "$GIT_SHA"
  printf 'control_base_url=%s\n' "$(sv "${CONTROL_BASE_URL:-<unset>}")"
  printf 'relay_host=%s\n' "$(sv "${RELAY_HOST:-<unset>}")"
  printf 'control_ssh_host=%s\n' "$(sv "${CONTROL_SSH_HOST:-<unset>}")"
  printf 'gateway_ssh_host=%s\n' "$(sv "${GATEWAY_SSH_HOST:-<unset>}")"
  printf 'agent_admin_base_url=%s\n' "$(sv "${AGENT_ADMIN_BASE_URL:-<unset>}")"
  printf 'evidence_dir=%s\n' "$RUN_DIR"
  printf 'target_confirm=%s\n' "$([[ -n "$TARGET_CONFIRM" ]] && printf 'asserted' || printf 'unset')"
  printf 'frps_restart_target=%s|%s\n' "${FRPS_SSH_HOST:-${GATEWAY_SSH_HOST:-<unset>}}" "$FRPS_UNIT"
  printf 'frps_restart_confirm=%s\n' "$([[ -n "$FRPS_RESTART_CONFIRM" ]] && printf asserted || printf unset)"
  printf 'agent_proxy_name=%s\n' "${AGENT_PROXY_NAME:-<derive sb-<namespace> from the relay URL host>}"
  printf 'frpc_prefilter=%s frpc_exact_name=%s identity_cmd=%s\n' "$FRPC_PID_MATCH" "$FRPC_PID_NAME" "$([[ -n "$FRPC_IDENTITY_CMD" ]] && printf override || printf default)"
  printf 'withdrawal_max_artifact_bytes=%s\n' "$WITHDRAWAL_MAX_ARTIFACT_BYTES"
  printf 'allow_agent_restart=%s allow_lockdown=%s allow_revoke=%s allow_frps_restart=%s\n' "$ALLOW_AGENT_RESTART" "$ALLOW_LOCKDOWN" "$ALLOW_REVOKE" "$ALLOW_FRPS_RESTART"

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

  # IMPORTANT 7: JSON-shaped secrets (`"key":"value"`) must be redacted
  # structurally, key by key, not only by the whitespace text patterns.
  local json_raw json_red
  json_raw='{"token":"SUPERSECRETTOKEN123456","password":"p@ss","nested":{"api_key":"ABCDEFGHIJKLMNOP"},"safe":"keepme"}'
  json_red="$(printf '%s' "$json_raw" | sanitize)"
  got="redacted"
  printf '%s' "$json_red" | grep -qE 'SUPERSECRETTOKEN123456|p@ss|ABCDEFGHIJKLMNOP' && got="raw JSON secret leaked"
  selftest_check "JSON key-based redaction removes secret values" "$got" "redacted" || failures=$(( failures + 1 ))
  got="preserved"
  printf '%s' "$json_red" | grep -q 'keepme' || got="non-secret value lost"
  selftest_check "JSON redaction preserves non-secret values" "$got" "preserved" || failures=$(( failures + 1 ))

  got="$(verdict_for "PASS,")";        selftest_check "verdict(all PASS)" "$got" "GREEN" || failures=$(( failures + 1 ))
  got="$(verdict_for "PASS,FAIL,")";   selftest_check "verdict(FAIL present)" "$got" "RED" || failures=$(( failures + 1 ))
  got="$(verdict_for "PASS,SKIP,")";   selftest_check "verdict(SKIP is not a pass)" "$got" "PARTIAL" || failures=$(( failures + 1 ))
  got="$(verdict_for "PASS,MISSING,")"; selftest_check "verdict(unrun/missing)" "$got" "RED" || failures=$(( failures + 1 ))

  # -------------------------------------------------------------------------
  # IMPORTANT 4: the destructive-target confirmation guard.
  # -------------------------------------------------------------------------
  local tc_tmp tc_rc saved_confirm="$TARGET_CONFIRM" saved_control="$CONTROL_BASE_URL" saved_check="${GATE_CHECK_FILE:-}"
  tc_tmp="$(mktemp -d "${TMPDIR:-/tmp}/m4exit-selftest-tc.XXXXXX")"
  GATE_CHECK_FILE="$tc_tmp/checks"; : > "$GATE_CHECK_FILE"
  TARGET_CONFIRM=""; CONTROL_BASE_URL="https://control.example"
  tc_rc=0; require_target_confirmation tc >/dev/null 2>&1 || tc_rc=$?
  got="$(grep '^FAIL|' "$GATE_CHECK_FILE" | head -n1 | cut -d'|' -f2)"
  selftest_check "target guard refuses an unconfirmed state change" "$tc_rc/$got" "1/tc_target_confirmed" || failures=$(( failures + 1 ))
  : > "$GATE_CHECK_FILE"; TARGET_CONFIRM="https://wrong.example"
  tc_rc=0; require_target_confirmation tc >/dev/null 2>&1 || tc_rc=$?
  got="$(grep '^FAIL|' "$GATE_CHECK_FILE" | head -n1 | cut -d'|' -f2)"
  selftest_check "target guard refuses a mismatched target assertion" "$tc_rc/$got" "1/tc_target_confirmed" || failures=$(( failures + 1 ))
  : > "$GATE_CHECK_FILE"; TARGET_CONFIRM="https://control.example"
  tc_rc=0; require_target_confirmation tc >/dev/null 2>&1 || tc_rc=$?
  got="$(grep '^PASS|' "$GATE_CHECK_FILE" | head -n1 | cut -d'|' -f2)"
  selftest_check "target guard accepts an exact target assertion" "$tc_rc/$got" "0/tc_target_confirmed" || failures=$(( failures + 1 ))
  GATE_CHECK_FILE="$saved_check"; TARGET_CONFIRM="$saved_confirm"; CONTROL_BASE_URL="$saved_control"
  rm -rf "$tc_tmp"

  # BLOCKING 1 / IMPORTANT 4: the integrity share must never be the revoke target.
  got="$(revoke_codes_distinct "shareA" "shareA")"
  selftest_check "revoke code equal to the integrity code is rejected" "$got" "equal" || failures=$(( failures + 1 ))
  got="$(revoke_codes_distinct "shareA" "shareB")"
  selftest_check "a distinct revoke code is accepted" "$got" "ok" || failures=$(( failures + 1 ))
  got="$(revoke_codes_distinct "" "")"
  selftest_check "two empty codes are not treated as equal" "$got" "ok" || failures=$(( failures + 1 ))

  # BLOCKING 1: the in-flight precondition and the newness/timestamp proof for
  # the gateway drain line.
  got="$(revoke_inflight_verdict 0 100 1)";   selftest_check "in-flight guard refuses a zero-byte transfer" "$got" "zero-bytes" || failures=$(( failures + 1 ))
  got="$(revoke_inflight_verdict 100 100 1)"; selftest_check "in-flight guard refuses an already-finished transfer" "$got" "completed" || failures=$(( failures + 1 ))
  got="$(revoke_inflight_verdict 50 100 0)";  selftest_check "in-flight guard refuses a dead download process" "$got" "not-alive" || failures=$(( failures + 1 ))
  got="$(revoke_inflight_verdict 50 100 1)";  selftest_check "in-flight guard accepts a live partial transfer" "$got" "ok" || failures=$(( failures + 1 ))

  # -------------------------------------------------------------------------
  # tunnel_recovery_frps_restart: the automatic-recovery verdict predicate and
  # the frps-restart opt-in guard.
  # -------------------------------------------------------------------------
  got="$(tunnel_recovery_verdict 0 old new 5 6 1 1)";   selftest_check "recovery verdict refuses an offline tunnel" "$got" "offline" || failures=$(( failures + 1 ))
  got="$(tunnel_recovery_verdict 1 old old 5 6 1 1)";   selftest_check "recovery verdict refuses an unchanged child" "$got" "no-child" || failures=$(( failures + 1 ))
  got="$(tunnel_recovery_verdict 1 old new 5 5 1 1)";   selftest_check "recovery verdict refuses an unchanged session counter" "$got" "no-session" || failures=$(( failures + 1 ))
  got="$(tunnel_recovery_verdict 1 old new 5 6 1 0)";   selftest_check "recovery verdict refuses a session not proven against the agent under test" "$got" "no-agent-session" || failures=$(( failures + 1 ))
  got="$(tunnel_recovery_verdict 1 old new 5 6 0 1)";   selftest_check "recovery verdict refuses a non-serving relay" "$got" "no-content" || failures=$(( failures + 1 ))
  got="$(tunnel_recovery_verdict 1 old new 5 6 1 1)";   selftest_check "recovery verdict accepts a full automatic recovery" "$got" "ok" || failures=$(( failures + 1 ))

  # BLOCKING 1: one stable frpc identity (PID:STARTTIME); wrapper command lines
  # must never count and 0 or >1 identities must fail closed.
  got="$(frpc_identity_status '878807:1700000000')";        selftest_check "one frpc identity is usable" "$got" "one" || failures=$(( failures + 1 ))
  got="$(frpc_identity_status '')";                          selftest_check "zero frpc identities is ambiguous (none)" "$got" "none" || failures=$(( failures + 1 ))
  got="$(frpc_identity_status $'1:10\n2:20')";              selftest_check "two frpc identities is ambiguous (many)" "$got" "many" || failures=$(( failures + 1 ))
  got="$(frpc_identity_status '  917087:1700000000  ')";     selftest_check "a whitespace-padded identity is still one" "$got" "one" || failures=$(( failures + 1 ))

  # BLOCKING 2: increases are a pure predicate; the global counter and the
  # agent-specific frps proxy-registration line count share it.
  got="$(counter_increased_verdict 5 6)";   selftest_check "an increased counter is proven" "$got" "ok" || failures=$(( failures + 1 ))
  got="$(counter_increased_verdict 6 6)";   selftest_check "an unchanged counter is not a new session" "$got" "no-increase" || failures=$(( failures + 1 ))
  got="$(counter_increased_verdict x 6)";   selftest_check "an unparseable counter fails closed" "$got" "unobservable" || failures=$(( failures + 1 ))
  got="$(counter_increased_verdict 1 '')";  selftest_check "a missing after-count fails closed" "$got" "unobservable" || failures=$(( failures + 1 ))

  # IMPORTANT 5: the bound is enforced after all stages (not just before an
  # iteration) and each stage can be capped by the remaining time.
  got="$(bound_elapsed_verdict 12 120)";   selftest_check "an in-bound elapsed time is accepted" "$got" "ok" || failures=$(( failures + 1 ))
  got="$(bound_elapsed_verdict 121 120)";  selftest_check "a late recovery fails the bound" "$got" "over-bound" || failures=$(( failures + 1 ))
  got="$(bound_elapsed_verdict '' 120)";   selftest_check "an unmeasurable elapsed time fails closed" "$got" "unobservable" || failures=$(( failures + 1 ))
  local past_deadline future_deadline rem_past rem_future
  past_deadline=$(( $(date +%s) - 5 )); future_deadline=$(( $(date +%s) + 30 ))
  rem_past="$(deadline_remaining "$past_deadline")"; rem_future="$(deadline_remaining "$future_deadline")"
  selftest_check "an expired deadline leaves no time" "$rem_past" "0" || failures=$(( failures + 1 ))
  got="$([[ "$rem_future" =~ ^[0-9]+$ && "$rem_future" -gt 0 && "$rem_future" -le 30 ]] && printf ok || printf bad)"
  selftest_check "a live deadline yields the remaining seconds" "$got" "ok" || failures=$(( failures + 1 ))

  # IMPORTANT 6: a separate, exact confirmation of the SSH host + systemd unit
  # that will be restarted.
  got="$(frps_restart_target_verdict '' root@10.0.0.5 sharebridge-relay-frps.service)"; selftest_check "restart target guard refuses no confirmation" "$got" "unset" || failures=$(( failures + 1 ))
  got="$(frps_restart_target_verdict 'root@10.0.0.9|sharebridge-relay-frps.service' root@10.0.0.5 sharebridge-relay-frps.service)"; selftest_check "restart target guard refuses a different host" "$got" "mismatch" || failures=$(( failures + 1 ))
  got="$(frps_restart_target_verdict 'root@10.0.0.5|other.service' root@10.0.0.5 sharebridge-relay-frps.service)"; selftest_check "restart target guard refuses a different unit" "$got" "mismatch" || failures=$(( failures + 1 ))
  got="$(frps_restart_target_verdict 'root@10.0.0.5|sharebridge-relay-frps.service' root@10.0.0.5 sharebridge-relay-frps.service)"; selftest_check "restart target guard accepts the exact host|unit" "$got" "ok" || failures=$(( failures + 1 ))

  # -------------------------------------------------------------------------
  # BLOCKING 4: lockdown withdrawal predicate (BOUNDED TLS-artifact tolerance).
  # Signature: <code> <bytes> <exact> <prefix> <same-length> <marker> <baseline-bytes> <max-bytes>
  # -------------------------------------------------------------------------
  got="$(lockdown_withdrawal_verdict 000 30 0 0 0 0 4096 64)";   selftest_check "withdrawal accepts a small 000 TLS artifact (body != baseline)" "$got" "withdrawn" || failures=$(( failures + 1 ))
  got="$(lockdown_withdrawal_verdict 000 0 0 0 0 0 4096 64)";    selftest_check "withdrawal accepts a clean 000 no-content result" "$got" "withdrawn" || failures=$(( failures + 1 ))
  got="$(lockdown_withdrawal_verdict 503 0 0 0 0 0 4096 64)";    selftest_check "withdrawal accepts a non-2xx status with no baseline body" "$got" "withdrawn" || failures=$(( failures + 1 ))
  got="$(lockdown_withdrawal_verdict 200 4096 1 0 0 0 4096 64)"; selftest_check "withdrawal rejects a 200 serving the baseline body" "$got" "leaked" || failures=$(( failures + 1 ))
  got="$(lockdown_withdrawal_verdict 000 4096 1 0 0 0 4096 64)"; selftest_check "withdrawal rejects the baseline body even at http 000" "$got" "leaked" || failures=$(( failures + 1 ))
  got="$(lockdown_withdrawal_verdict 000 20 0 1 0 0 4096 64)";   selftest_check "withdrawal rejects a truncated PREFIX of the baseline" "$got" "leaked" || failures=$(( failures + 1 ))
  got="$(lockdown_withdrawal_verdict 000 4096 0 0 1 0 4096 64)"; selftest_check "withdrawal rejects a body with the baseline length" "$got" "leaked" || failures=$(( failures + 1 ))
  got="$(lockdown_withdrawal_verdict 000 200 0 0 0 1 4096 64)";  selftest_check "withdrawal rejects a body containing a baseline content marker" "$got" "leaked" || failures=$(( failures + 1 ))
  got="$(lockdown_withdrawal_verdict 000 100000 0 0 0 0 4096 64)"; selftest_check "withdrawal rejects a non-2xx body over the artifact cap" "$got" "oversize" || failures=$(( failures + 1 ))
  got="$(lockdown_withdrawal_verdict 200 512 0 0 0 0 4096 64)";  selftest_check "withdrawal rejects any 2xx while locked" "$got" "served" || failures=$(( failures + 1 ))
  got="$(lockdown_withdrawal_verdict 000 not-a-number 0 0 0 0 4096 64)"; selftest_check "withdrawal fails closed on an unparseable byte count" "$got" "unreadable" || failures=$(( failures + 1 ))
  got="$(lockdown_withdrawal_verdict 000 30 bogus 0 0 0 4096 64)"; selftest_check "withdrawal fails closed on an unknown baseline flag" "$got" "unreadable" || failures=$(( failures + 1 ))
  got="$(lockdown_withdrawal_verdict 000 30 0 0 0 0 4096 bogus)"; selftest_check "withdrawal fails closed on an unparseable cap" "$got" "unreadable" || failures=$(( failures + 1 ))
  # The byte-level flag computation is real (not just the predicate): an exact
  # copy, a truncated prefix, a same-length body and a body containing the
  # baseline's first 64 bytes each register.
  local wf_dir="$(mktemp -d "${TMPDIR:-/tmp}/m4exit-selftest-wf.XXXXXX")" wf_flags
  printf 'BASELINE-CONTENT-0123456789-abcdefghijklmnopqrstuvwxyz-ABCDEFGHIJKLMNOPQRSTUVWXYZ-END' > "$wf_dir/base"
  cp "$wf_dir/base" "$wf_dir/exact"
  printf 'BASELINE-CONTENT-0123' > "$wf_dir/prefix"
  wf_flags="$(withdrawal_body_flags "$wf_dir/exact" "$wf_dir/base")"
  got="$(printf '%s\n' "$wf_flags" | sed -n 's/^exact=//p')"
  selftest_check "withdrawal flags detect an exact baseline copy" "$got" "1" || failures=$(( failures + 1 ))
  wf_flags="$(withdrawal_body_flags "$wf_dir/prefix" "$wf_dir/base")"
  got="$(printf '%s\n' "$wf_flags" | sed -n 's/^prefix=//p')"
  selftest_check "withdrawal flags detect a truncated baseline prefix" "$got" "1" || failures=$(( failures + 1 ))
  printf 'XXXX-' > "$wf_dir/marker"; head -c 64 "$wf_dir/base" >> "$wf_dir/marker"; printf -- '-TAIL' >> "$wf_dir/marker"
  wf_flags="$(withdrawal_body_flags "$wf_dir/marker" "$wf_dir/base")"
  got="$(printf '%s\n' "$wf_flags" | sed -n 's/^marker=//p')"
  selftest_check "withdrawal flags detect a baseline content marker" "$got" "1" || failures=$(( failures + 1 ))
  rm -rf "$wf_dir"

  # IMPORTANT 7 (sibling): a share-code-bearing command URL must be redacted in
  # the recorded command evidence.
  local m4_cmd_file="$(mktemp)" m4_cmd_saved="${GATE_CMD_FILE:-}" m4_cmd_evidence
  GATE_CMD_FILE="$m4_cmd_file"
  record_cmd "curl -X POST https://control.example/api/shares/SECRETSHARE99/prepare-route"
  m4_cmd_evidence="$(cat "$m4_cmd_file")"
  GATE_CMD_FILE="$m4_cmd_saved"; rm -f "$m4_cmd_file"
  got="redacted"
  printf '%s' "$m4_cmd_evidence" | grep -q 'SECRETSHARE99' && got="raw share code leaked into command evidence"
  selftest_check "recorded command evidence redacts the share code" "$got" "redacted" || failures=$(( failures + 1 ))
  got="redacted"
  printf '%s' "$m4_cmd_evidence" | grep -q 'REDACTED' || got="no redaction marker in command evidence"
  selftest_check "recorded command evidence carries a redaction marker" "$got" "redacted" || failures=$(( failures + 1 ))
  local ns_get
  ns_get="$(namespace_from_relay_host 'photo.relay.sb12345678.example.com')"
  selftest_check "the relay namespace is derived from the relay URL host" "$ns_get" "sb12345678" || failures=$(( failures + 1 ))
  got="$(namespace_from_relay_host 'photo.example.com')"
  selftest_check "a host without a relay label derives no namespace" "$got" "" || failures=$(( failures + 1 ))
  local saved_allow_frps="$ALLOW_FRPS_RESTART"
  ALLOW_FRPS_RESTART=0
  got="$(case_is_destructive tunnel_recovery_frps_restart)/$(case_opt_in_flag tunnel_recovery_frps_restart)"
  selftest_check "frps-restart case is non-destructive without the opt-in" "$got" "no/LIVE_M4EXIT_ALLOW_FRPS_RESTART=0" || failures=$(( failures + 1 ))
  ALLOW_FRPS_RESTART=1
  got="$(case_is_destructive tunnel_recovery_frps_restart)/$(case_opt_in_flag tunnel_recovery_frps_restart)"
  selftest_check "frps-restart case is destructive with the opt-in" "$got" "yes/LIVE_M4EXIT_ALLOW_FRPS_RESTART=1" || failures=$(( failures + 1 ))
  ALLOW_FRPS_RESTART="$saved_allow_frps"
  local drain_bl drain_cand drain_out recent_ts
  drain_bl="2026-01-01T00:00:00Z revoked route closed established streams hostname=h.example streams=1"
  drain_cand="$(printf '%s\n%s\n%s\n' "$drain_bl" \
    '2026-01-01T00:01:00Z revoked route closed established streams hostname=h.example streams=2' \
    '2026-01-01T00:01:00Z revoked route closed established streams hostname=other.example streams=9')"
  drain_out="$(printf '%s\n' "$drain_cand" | drain_new_lines h.example "$drain_bl")"
  got="$(printf '%s\n' "$drain_out" | grep -c . || true)"; got="${got:-0}"
  selftest_check "drain newness keeps only the new line for the exact host" "$got" "1" || failures=$(( failures + 1 ))
  got="$(printf '%s\n' "$drain_out" | grep -c 'streams=2' || true)"; got="${got:-0}"
  selftest_check "drain newness rejects the pre-baseline line" "$got" "1" || failures=$(( failures + 1 ))
  recent_ts="$(date -u +'%Y-%m-%dT%H:%M:%SZ')"
  got="$(log_line_epoch_after "$recent_ts revoked route closed established streams hostname=h.example streams=2" "$(( $(date -u +%s) - 60 ))" 10)"
  selftest_check "post-revoke drain timestamp is accepted" "$got" "yes" || failures=$(( failures + 1 ))
  got="$(log_line_epoch_after "2001-01-01T00:00:00Z revoked route closed established streams hostname=h.example streams=1" "$(date -u +%s)" 10)"
  selftest_check "old drain timestamp is refused" "$got" "no" || failures=$(( failures + 1 ))
  got="$(log_line_epoch_after "revoked route closed established streams hostname=h.example streams=1" "$(date -u +%s)" 10)"
  selftest_check "drain line without a timestamp is refused" "$got" "notimestamp" || failures=$(( failures + 1 ))

  # -------------------------------------------------------------------------
  # BLOCKING 3: direct-diagnostics freshness and correlation refusal.
  # -------------------------------------------------------------------------
  local rec_dir rec_now rec_floor rec_old rec_now_epoch
  rec_dir="$(mktemp -d "${TMPDIR:-/tmp}/m4exit-selftest-rec.XXXXXX")"
  rec_now="$(date -u +'%Y-%m-%d %H:%M:%S')"
  rec_now_epoch="$(date -u +%s)"
  rec_floor="$(( rec_now_epoch - 1 ))"
  if date -u -v-5S +%s >/dev/null 2>&1; then
    rec_old="$(date -u -v-5S +'%Y-%m-%d %H:%M:%S')"
  else
    rec_old="$(date -u -d '5 seconds ago' +'%Y-%m-%d %H:%M:%S')"
  fi
  printf '{"api_key_id":"agent-1","direct_status":"relay_fallback","direct_status_reason":"probe_failed","updated":"%s"}' "$rec_now" > "$rec_dir/fresh.json"
  got="$(direct_record_current "$rec_dir/fresh.json" agent-1 "$rec_floor" 10 | sed -n 's/^status=//p')"
  selftest_check "fresh + correlated agent record is accepted" "$got" "ok" || failures=$(( failures + 1 ))
  printf '{"api_key_id":"agent-1","updated":"2001-01-01 00:00:00"}' > "$rec_dir/stale.json"
  got="$(direct_record_current "$rec_dir/stale.json" agent-1 "$rec_floor" 10 | sed -n 's/^status=//p')"
  selftest_check "stale agent record is refused (last-evaluation is not current)" "$got" "stale" || failures=$(( failures + 1 ))
  printf '{"api_key_id":"agent-1","direct_status":"relay_fallback"}' > "$rec_dir/nots.json"
  got="$(direct_record_current "$rec_dir/nots.json" agent-1 "$rec_floor" 10 | sed -n 's/^status=//p')"
  selftest_check "record without an update timestamp is refused" "$got" "no_timestamp" || failures=$(( failures + 1 ))
  printf '{"api_key_id":"agent-2","updated":"%s"}' "$rec_now" > "$rec_dir/other.json"
  got="$(direct_record_current "$rec_dir/other.json" agent-1 "$rec_floor" 10 | sed -n 's/^status=//p')"
  selftest_check "record for a different agent is refused" "$got" "agent_mismatch" || failures=$(( failures + 1 ))
  # BLOCKING 3a: EXACTLY ONE structured record. A multi-record dump must never
  # be scanned for convenient fields.
  printf '[{"api_key_id":"agent-1","updated":"%s"},{"api_key_id":"agent-2","updated":"%s"}]' "$rec_now" "$rec_now" > "$rec_dir/list2.json"
  got="$(direct_record_current "$rec_dir/list2.json" agent-1 "$rec_floor" 10 | sed -n 's/^status=//p')"
  selftest_check "a two-record JSON list is refused as ambiguous" "$got" "ambiguous" || failures=$(( failures + 1 ))
  printf '{"items":[{"api_key_id":"agent-1","updated":"%s"},{"api_key_id":"agent-2","updated":"%s"}]}' "$rec_now" "$rec_now" > "$rec_dir/items2.json"
  got="$(direct_record_current "$rec_dir/items2.json" agent-1 "$rec_floor" 10 | sed -n 's/^status=//p')"
  selftest_check "a PocketBase items list with two records is refused as ambiguous" "$got" "ambiguous" || failures=$(( failures + 1 ))
  printf '{"items":[{"api_key_id":"agent-1","direct_status":"relay_fallback","updated":"%s"}]}' "$rec_now" > "$rec_dir/items1.json"
  got="$(direct_record_current "$rec_dir/items1.json" agent-1 "$rec_floor" 10 | sed -n 's/^status=//p')"
  selftest_check "a PocketBase items list with exactly one record is accepted" "$got" "ok" || failures=$(( failures + 1 ))
  # BLOCKING 3b: the agent id must be an exact FIELD, not a substring anywhere.
  printf '{"api_key_id":"agent-2","note":"this record is about agent-1","updated":"%s"}' "$rec_now" > "$rec_dir/substr.json"
  got="$(direct_record_current "$rec_dir/substr.json" agent-1 "$rec_floor" 10 | sed -n 's/^status=//p')"
  selftest_check "an agent id appearing only in a substring is refused" "$got" "agent_mismatch" || failures=$(( failures + 1 ))
  printf '{"record_id":"agent-1","updated":"%s"}' "$rec_now" > "$rec_dir/nofield.json"
  got="$(direct_record_current "$rec_dir/nofield.json" agent-1 "$rec_floor" 10 | sed -n 's/^status=//p')"
  selftest_check "a record with no api_key_id/id field is refused" "$got" "no_agent_id" || failures=$(( failures + 1 ))
  # BLOCKING 3c: STRICT freshness — a pre-request timestamp is refused at
  # tolerance 0 even by one second.
  printf '{"api_key_id":"agent-1","updated":"%s"}' "$rec_old" > "$rec_dir/pre.json"
  got="$(direct_record_current "$rec_dir/pre.json" agent-1 "$rec_now_epoch" 0 | sed -n 's/^status=//p')"
  selftest_check "a timestamp before the request floor is refused at tolerance 0" "$got" "stale" || failures=$(( failures + 1 ))
  got="$(direct_record_current "$rec_dir/pre.json" agent-1 "$rec_now_epoch" 10 | sed -n 's/^status=//p')"
  selftest_check "a small clock-skew tolerance can admit the same record" "$got" "ok" || failures=$(( failures + 1 ))
  # BLOCKING 3d: no MAX-anywhere. A stale `updated` must lose even when a
  # different (recent) timestamp field is present, unless the field priority
  # explicitly puts that field first.
  printf '{"api_key_id":"agent-1","direct_status":"relay_fallback","updated":"2001-01-01 00:00:00","stun_observed_at":"%s"}' "$rec_now" > "$rec_dir/max.json"
  got="$(direct_record_current "$rec_dir/max.json" agent-1 "$rec_now_epoch" 0 | sed -n 's/^status=//p')"
  selftest_check "a stale updated field loses even with a recent stun_observed_at" "$got" "stale" || failures=$(( failures + 1 ))
  got="$(direct_record_current "$rec_dir/max.json" agent-1 "$rec_now_epoch" 0 'stun_observed_at,updated' | sed -n 's/^status=//p')"
  selftest_check "an explicit field priority can select stun_observed_at" "$got" "ok" || failures=$(( failures + 1 ))
  # Pretty-printed single-record JSON and key=value text are both accepted; a
  # key=value dump repeating an identity key is ambiguous.
  printf '{\n  "api_key_id": "agent-1",\n  "direct_status": "relay_fallback",\n  "direct_status_reason": "probe_failed",\n  "updated": "%s"\n}\n' "$rec_now" > "$rec_dir/pretty.json"
  got="$(direct_record_current "$rec_dir/pretty.json" agent-1 "$rec_floor" 0 | sed -n 's/^status=//p')"
  selftest_check "a pretty-printed single-record JSON is accepted" "$got" "ok" || failures=$(( failures + 1 ))
  printf 'api_key_id=agent-1\ndirect_status=relay_fallback\ndirect_status_reason=probe_failed\nupdated=%s\n' "$rec_now" > "$rec_dir/kv.txt"
  got="$(direct_record_current "$rec_dir/kv.txt" agent-1 "$rec_floor" 0 | sed -n 's/^status=//p')"
  selftest_check "a single key=value record is accepted" "$got" "ok" || failures=$(( failures + 1 ))
  printf 'api_key_id=agent-1\napi_key_id=agent-2\nupdated=%s\n' "$rec_now" > "$rec_dir/kv2.txt"
  got="$(direct_record_current "$rec_dir/kv2.txt" agent-1 "$rec_floor" 0 | sed -n 's/^status=//p')"
  selftest_check "a key=value dump repeating the agent key is refused as ambiguous" "$got" "ambiguous" || failures=$(( failures + 1 ))

  # -------------------------------------------------------------------------
  # BLOCKING 5: live agent-record fetch makes the freshness requirement
  # satisfiable (and still refuses what it cannot prove).
  # -------------------------------------------------------------------------
  local live_rec_file="$rec_dir/live.json" live_status
  live_status="$(direct_record_fetch_live "printf '{\"api_key_id\":\"agent-1\",\"direct_status\":\"relay_fallback\",\"direct_status_reason\":\"probe_failed\",\"updated\":\"%s\"}' '$rec_now'" "$live_rec_file")"
  selftest_check "live-record fetch accepts a command that prints the record" "$live_status" "ok" || failures=$(( failures + 1 ))
  got="$(direct_record_current "$live_rec_file" agent-1 "$rec_floor" 10 | sed -n 's/^status=//p')"
  selftest_check "a live-fetched fresh record passes the currency/correlation gate" "$got" "ok" || failures=$(( failures + 1 ))
  live_status="$(direct_record_fetch_live "exit 3" "$live_rec_file")"
  selftest_check "live-record fetch refuses a failing command" "$live_status" "failed" || failures=$(( failures + 1 ))
  live_status="$(direct_record_fetch_live "true" "$live_rec_file")"
  selftest_check "live-record fetch refuses empty output" "$live_status" "empty" || failures=$(( failures + 1 ))
  live_status="$(direct_record_fetch_live "printf '{\"api_key_id\":\"agent-1\",\"updated\":\"2001-01-01 00:00:00\"}'" "$live_rec_file")"
  selftest_check "a live-fetched record is still fetched ok" "$live_status" "ok" || failures=$(( failures + 1 ))
  got="$(direct_record_current "$live_rec_file" agent-1 "$rec_floor" 10 | sed -n 's/^status=//p')"
  selftest_check "a live-fetched STALE record is still refused" "$got" "stale" || failures=$(( failures + 1 ))
  live_status="$(direct_record_fetch_live "printf '{\"api_key_id\":\"agent-2\",\"updated\":\"%s\"}' '$rec_now'" "$live_rec_file")"
  got="$(direct_record_current "$live_rec_file" agent-1 "$rec_floor" 10 | sed -n 's/^status=//p')"
  selftest_check "a live-fetched record for another agent is still refused" "$got" "agent_mismatch" || failures=$(( failures + 1 ))
  rm -rf "$rec_dir"

  # -------------------------------------------------------------------------
  # IMPORTANT 5: STUN freshness and per-agent isolation.
  # -------------------------------------------------------------------------
  local stun_recent stun_4m stun_out2
  stun_recent="$(date -u +'%Y-%m-%dT%H:%M:%S')"
  if date -u -v-4M +%s >/dev/null 2>&1; then
    stun_4m="$(date -u -v-4M +'%Y-%m-%dT%H:%M:%S')"
  else
    stun_4m="$(date -u -d '4 minutes ago' +'%Y-%m-%dT%H:%M:%S')"
  fi
  stun_out2="$(printf '%s\n%s\n' "$stun_4m stun observation accepted for agent-1" "$stun_recent stun observation accepted for agent-1" | stun_analyze agent-1 5)"
  got="$(printf '%s\n' "$stun_out2" | sed -n 's/^fresh=//p')"
  selftest_check "fresh agent acceptance yields fresh=yes" "$got" "yes" || failures=$(( failures + 1 ))
  got="$(printf '%s\n' "$stun_out2" | sed -n 's/^agent_lines=//p')"
  selftest_check "isolation counts only the agent's accept lines" "$got" "2" || failures=$(( failures + 1 ))
  stun_out2="$(printf '%s\n%s\n' "2001-01-01T00:00:00 stun observation accepted for agent-1" "2001-01-01T04:00:00 stun observation accepted for agent-1" | stun_analyze agent-1 5)"
  got="$(printf '%s\n' "$stun_out2" | sed -n 's/^fresh=//p')"
  selftest_check "stale journal is refused (rechallenging stopped)" "$got" "no" || failures=$(( failures + 1 ))
  stun_out2="$(printf '%s\n%s\n' "$stun_recent stun observation accepted for agent-1" "$stun_recent stun observation accepted for agent-2" | stun_analyze agent-1 5)"
  got="$(printf '%s\n' "$stun_out2" | sed -n 's/^accepted=//p')"
  selftest_check "interleaved agents do not merge into one timeline" "$got" "1" || failures=$(( failures + 1 ))

  # -------------------------------------------------------------------------
  # IMPORTANT 7: the scratch capture directory is removed by the cleanup hook.
  # -------------------------------------------------------------------------
  local cleanup_dir saved_scratch="${M4EXIT_SCRATCH_DIR:-}"
  M4EXIT_SCRATCH_DIR="$(mktemp -d "${TMPDIR:-/tmp}/m4exit-selftest-scratch.XXXXXX")"
  cleanup_dir="$M4EXIT_SCRATCH_DIR"
  printf 'secret-bearing body' > "$M4EXIT_SCRATCH_DIR/body"
  m4exit_cleanup_scratch
  selftest_check "scratch capture dir removed by the cleanup hook" "$([[ -d "$cleanup_dir" ]] && printf present || printf removed)" "removed" || failures=$(( failures + 1 ))
  M4EXIT_SCRATCH_DIR="$saved_scratch"

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
if [[ -n "${M4EXIT_FAKE_CURL_LOG:-}" ]]; then printf '%s\n' "$*" >> "$M4EXIT_FAKE_CURL_LOG"; fi
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

  # BLOCKING 2 window: the state is marked BEFORE the lockdown request, the
  # request reaches the server, and the signal arrives before the response is
  # read. The trap must still issue exactly one unlock, and the child's TMPDIR
  # must be left empty (IMPORTANT 7 cleanup contract).
  local tmp_cleanup child_log unlock_attempts lockdown_attempts
  tmp_cleanup="$(mktemp -d "${TMPDIR:-/tmp}/m4exit-selftest-tmp.XXXXXX")"
  child_log="$fake_bin/log.before_request"
  printf '0' > "$fake_bin/count.before_request"; : > "$child_log"
  child_rc=0
  ( PATH="$fake_bin:$PATH" TMPDIR="$tmp_cleanup" M4EXIT_FAKE_CURL_COUNT="$fake_bin/count.before_request" \
      M4EXIT_FAKE_CURL_LOG="$child_log" \
      M4EXIT_INTERNAL_TRAP_SELFTEST=before_request \
      M4EXIT_AGENT_ADMIN_BASE_URL="http://127.0.0.1:1" \
      M4EXIT_AGENT_ADMIN_USER=selftest M4EXIT_AGENT_ADMIN_PASSWORD=selftest \
      bash "$SCRIPT_SELF" ) >/dev/null 2>&1 || child_rc=$?
  unlock_attempts="$(grep -c '/api/unlock' "$child_log" 2>/dev/null || true)"; unlock_attempts="${unlock_attempts:-0}"
  lockdown_attempts="$(grep -c '/api/lockdown' "$child_log" 2>/dev/null || true)"; lockdown_attempts="${lockdown_attempts:-0}"
  selftest_check "real trap unlocks when interrupted in the mark-before-request window (rc=$child_rc, lockdown=$lockdown_attempts, unlock=$unlock_attempts)" "$child_rc/$lockdown_attempts/$unlock_attempts" "143/1/1" || failures=$(( failures + 1 ))
  got="$(find "$tmp_cleanup" -mindepth 1 2>/dev/null | wc -l | tr -d ' ')"
  selftest_check "no harness temp files survive a trapped run (TMPDIR empty)" "$got" "0" || failures=$(( failures + 1 ))
  rm -rf "$tmp_cleanup"
  rm -rf "$fake_bin"

  if [[ "$failures" -eq 0 ]]; then
    printf 'SELFTEST RESULT: PASS (0 failures) — the case runner refuses PASS for unexecuted, note-only, skipped or crashing cases; the sanitiser redacts configured literals, JSON key values and text-pattern secrets in console and evidence; the destructive-target guard, revoke-code guard, revoke in-flight/newness guards, direct-diagnostics freshness/correlation refusal and STUN freshness/isolation all refuse unproven cases; the lockdown EXIT/INT/TERM trap (including the mark-before-request window) unlocks exactly once; and trapped runs leave no temp files behind\n'
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
  case "$M4EXIT_INTERNAL_TRAP_SELFTEST" in
    exit) m4exit_mark_locked; exit 7 ;;
    int)  m4exit_mark_locked; kill -INT "$$"; sleep 5; exit 200 ;;
    term) m4exit_mark_locked; kill -TERM "$$"; sleep 5; exit 201 ;;
    before_request)
      # BLOCKING-2 window: the conservative locked state is marked BEFORE the
      # lockdown request, the request reaches the server (the stub answers
      # 200), and the signal arrives before the response is evaluated. The trap
      # must still issue exactly one unlock.
      m4exit_lockdown_request
      kill -TERM "$$"; sleep 5; exit 202 ;;
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

# Install the main-process safety net for every non-list mode. It unlocks a
# possibly-locked agent (a no-op unless the lockdown case marked it) and removes
# the scratch capture directory on EXIT/INT/TERM, including on error.
m4exit_install_lockdown_trap

if [[ "$MODE" == "selftest" ]]; then
  run_selftest
fi

if [[ "$DRY_RUN" -eq 1 ]]; then
  run_dry_run
fi

printf '=== ShareBridge Phase 4a M4-exit live relay e2e harness (task #15) ===\n'
printf 'Evidence root: %s (the worktree is never written)\n' "$EVIDENCE_DIR"
run_all_cases
print_summary

case "$VERDICT" in
  GREEN) exit 0 ;;
  PARTIAL) exit 3 ;;
  *) exit 1 ;;
esac
