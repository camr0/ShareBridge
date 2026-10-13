#!/usr/bin/env bash
#
# deploy_bare_client.sh — deploy or revert the EXP44 client-side patches on the
# TESTBOX test web root (`/opt/sharebridge-test/web`). Never touches production.
#
# Recovered from EXP28 (2026-09-18-exp28-bare-receive-path.md), which applied the
# bare-receive patch by hand to TESTBOX and reverted it, leaving no artifact.
#
#   ./deploy_bare_client.sh deploy [bare|n5|all]      # default fileset: bare
#   ./deploy_bare_client.sh manifest [bare|n5|all]    # print the current remote manifest
#   ./deploy_bare_client.sh revert [session-id]       # restore the newest (or named) session
#
# Every run prints a sha256/size/mode/owner manifest of the affected files BEFORE
# and AFTER the change, so a revert is provable against the recorded pre-deploy
# state. `revert` fails loudly if the restored bytes do not match that record.
#
# The host is deliberately NOT hardcoded (runbook rule 5: no IPs in committed
# files). Read it from .worktrees/e2e-v1/INFRA.local.md and pass it in:
#
#   SB_TESTBOX_HOST=ubuntu@<TESTBOX-IP> ./deploy_bare_client.sh deploy bare
#
# Options (env):
#   SB_TESTBOX_HOST    required, e.g. ubuntu@203.0.113.9
#   SB_WEB_ROOT        remote web root           (default /opt/sharebridge-test/web)
#   SB_BACKUP_ROOT     remote backup root        (default /tmp/sb-bare-backup)
#   SB_HTTP_BASE       served origin on the box  (default http://127.0.0.1:8080)
#   SB_REMOTE_SUDO     privilege prefix          (default sudo; set to "" for root)
#   SB_SKIP_HTTP_VERIFY=1  skip the served-bytes cross-check
#   SB_SSH_OPTS        extra ssh/scp flags
#   SB_LOCAL_SIM=1     run the remote halves on this machine instead of over ssh
#                      (used by deploy_bare_client.selftest.sh; set SB_REMOTE_SUDO="")
#
# FILESETS
#   bare  src/app.js src/bareReceive.js src/bulkReorder.js
#         the EXP28 `?bare=1` bare receiver (app.js + its two new modules).
#         Inert without the query parameter.
#   n5    the bare set plus src/binaryEnvelope.js src/channelSet.js
#         src/directChannel.js — the SB_BULK_LANES client wiring. Inert unless
#         the agent offers `bulk-*` lanes / emits v2 chunk envelopes.
#   all   alias for n5.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
LOCAL_SRC="$REPO_ROOT/signaling-server/web/src"

WEB_ROOT="${SB_WEB_ROOT:-/opt/sharebridge-test/web}"
BACKUP_ROOT="${SB_BACKUP_ROOT:-/tmp/sb-bare-backup}"
HTTP_BASE="${SB_HTTP_BASE:-http://127.0.0.1:8080}"
REMOTE_SUDO="${SB_REMOTE_SUDO-sudo}"
SSH_OPTS=(${SB_SSH_OPTS:-})
STAGE_ROOT="${SB_STAGE_ROOT:-/tmp/sb-bare-stage}"
SIM="${SB_LOCAL_SIM:-0}"

BARE_FILES=(src/app.js src/bareReceive.js src/bulkReorder.js)
N5_FILES=(src/binaryEnvelope.js src/channelSet.js src/directChannel.js)

# NOTE: src/bulkReorder.js is in the `bare` set only because app.js imports it
# statically; the module is never reached unless a v2 chunk sequence arrives.
# TESTBOX's test web root is not known to carry it, so it must ship with any app.js deploy or
# the `import` fails and the page never loads.

die() { printf 'deploy_bare_client: %s\n' "$*" >&2; exit 1; }

require_host() {
  [ "$SIM" = "1" ] && return 0
  [ -n "${SB_TESTBOX_HOST:-}" ] || die \
    'SB_TESTBOX_HOST is not set. Look the TESTBOX IP up in .worktrees/e2e-v1/INFRA.local.md and run:
    SB_TESTBOX_HOST=ubuntu@<TESTBOX-IP> '"$0"' '"${1:-deploy}"' '"${2:-}"''
}

# Runs a shell script (on stdin) with positional args, on TESTBOX or locally.
remote_script() {
  if [ "$SIM" = "1" ]; then bash -s -- "$@"
  else ssh "${SSH_OPTS[@]}" "$SB_TESTBOX_HOST" bash -s -- "$@"
  fi
}

# Copies local files into a destination directory on TESTBOX (or locally).
remote_copy() {
  local dest="$1"; shift
  if [ "$SIM" = "1" ]; then mkdir -p "$dest" && cp "$@" "$dest/"
  else scp "${SSH_OPTS[@]}" -q "$@" "$SB_TESTBOX_HOST:$dest/"
  fi
}

fileset() {
  case "${1:-bare}" in
    bare) printf '%s\n' "${BARE_FILES[@]}" ;;
    n5)   printf '%s\n' "${BARE_FILES[@]}" "${N5_FILES[@]}" | sort -u ;;
    all)  fileset n5 ;;
    *)    die "unknown fileset '$1' (expected bare|n5|all)" ;;
  esac
}

local_hash() {
  if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1" | cut -d' ' -f1
  else shasum -a 256 "$1" | cut -d' ' -f1; fi
}

# ---------------------------------------------------------------------------
# Remote halves. Each sends a self-contained script so quoting stays in one place.
# The size/mode/owner helpers work on GNU coreutils and BSD/macOS stat alike, so
# SB_LOCAL_SIM=1 exercises exactly the same code paths.
# ---------------------------------------------------------------------------

# Emits one line per file: "<sha256|ABSENT> <size> <mode> <owner> <path>"
cmd_remote_manifest() {
  remote_script "$WEB_ROOT" "$1" <<'REMOTE'
set -euo pipefail
WEB="$1"; FILES="$2"
hash_file() {
  if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1" | cut -d' ' -f1
  else shasum -a 256 "$1" | cut -d' ' -f1; fi
}
if stat -c '%s' . >/dev/null 2>&1; then SIZE_FMT='%s'; MODE_FMT='%a'; OWNER_FMT='%U:%G'
else SIZE_FMT='%z'; MODE_FMT='%Lp'; OWNER_FMT='%Su:%Sg'; fi
while IFS= read -r rel; do
  [ -n "$rel" ] || continue
  target="$WEB/$rel"
  if [ -f "$target" ]; then
    printf '%s  %s  %s  %s  %s\n' \
      "$(hash_file "$target")" \
      "$(stat -c "$SIZE_FMT" "$target" 2>/dev/null || stat -f "$SIZE_FMT" "$target")" \
      "$(stat -c "$MODE_FMT" "$target" 2>/dev/null || stat -f "$MODE_FMT" "$target")" \
      "$(stat -c "$OWNER_FMT" "$target" 2>/dev/null || stat -f "$OWNER_FMT" "$target")" \
      "$rel"
  else
    printf 'ABSENT  -  -  -  %s\n' "$rel"
  fi
done <<< "$FILES"
REMOTE
}

cmd_remote_backup() {
  remote_script "$WEB_ROOT" "$BACKUP_ROOT" "$1" "$2" "$REMOTE_SUDO" <<'REMOTE'
set -euo pipefail
WEB="$1"; BACKUP_ROOT="$2"; SESSION="$3"; FILES="$4"; SUDO="$5"
dir="$BACKUP_ROOT/$SESSION"
$SUDO mkdir -p "$dir/src"
printf '%s\n' "$FILES" > /tmp/sb-bare-fileset.$$
$SUDO cp /tmp/sb-bare-fileset.$$ "$dir/fileset"
rm -f /tmp/sb-bare-fileset.$$
while IFS= read -r rel; do
  [ -n "$rel" ] || continue
  if [ -f "$WEB/$rel" ]; then
    $SUDO mkdir -p "$(dirname "$dir/$rel")"
    $SUDO cp -p "$WEB/$rel" "$dir/$rel"
    printf 'backed-up  %s\n' "$rel"
  else
    printf 'absent     %s\n' "$rel"
  fi
done <<< "$FILES"
REMOTE
}

cmd_remote_install() {
  remote_script "$WEB_ROOT" "$STAGE_ROOT/$1" "$REMOTE_SUDO" <<'REMOTE'
set -euo pipefail
WEB="$1"; STAGE="$2"; SUDO="$3"
[ -d "$STAGE" ] || { echo "staging dir missing: $STAGE" >&2; exit 1; }
cd "$STAGE"
find . -type f -print0 | while IFS= read -r -d '' staged; do
  rel="${staged#./}"
  $SUDO mkdir -p "$(dirname "$WEB/$rel")"
  if [ -n "$SUDO" ]; then
    $SUDO install -m 644 -o root -g root "$staged" "$WEB/$rel"
  else
    cp "$staged" "$WEB/$rel"
    chmod 644 "$WEB/$rel"
  fi
  printf 'installed  %s\n' "$rel"
done
REMOTE
}

cmd_remote_restore() {
  remote_script "$WEB_ROOT" "$BACKUP_ROOT" "$1" "$2" "$REMOTE_SUDO" <<'REMOTE'
set -euo pipefail
WEB="$1"; BACKUP_ROOT="$2"; SESSION="$3"; FILES="$4"; SUDO="$5"
dir="$BACKUP_ROOT/$SESSION"
[ -d "$dir" ] || { echo "no such backup session: $dir" >&2; exit 1; }
while IFS= read -r rel; do
  [ -n "$rel" ] || continue
  line="$(grep -E "  $rel\$" "$dir/manifest.before" || true)"
  if [ -z "$line" ] || printf '%s' "$line" | grep -q '^ABSENT'; then
    $SUDO rm -f "$WEB/$rel"
    printf 'removed    %s (absent before deploy)\n' "$rel"
    continue
  fi
  mode="$(printf '%s' "$line" | awk '{print $3}')"
  owner="$(printf '%s' "$line" | awk '{print $4}')"
  $SUDO mkdir -p "$(dirname "$WEB/$rel")"
  if [ -n "$SUDO" ]; then
    $SUDO install -m "$mode" -o "${owner%%:*}" -g "${owner##*:}" "$dir/$rel" "$WEB/$rel"
  else
    cp "$dir/$rel" "$WEB/$rel"
    chmod "$mode" "$WEB/$rel"
  fi
  printf 'restored   %s (%s %s)\n' "$rel" "$mode" "$owner"
done <<< "$FILES"
REMOTE
}

cmd_remote_latest_session() {
  remote_script "$BACKUP_ROOT" "$REMOTE_SUDO" <<'REMOTE'
set -euo pipefail
BACKUP_ROOT="$1"; SUDO="$2"
[ -d "$BACKUP_ROOT" ] || exit 0
$SUDO ls -1t "$BACKUP_ROOT" | head -n 1
REMOTE
}

cmd_remote_read_backup_file() {
  remote_script "$BACKUP_ROOT" "$1" "$2" "$REMOTE_SUDO" <<'REMOTE'
set -euo pipefail
BACKUP_ROOT="$1"; SESSION="$2"; NAME="$3"; SUDO="$4"
$SUDO cat "$BACKUP_ROOT/$SESSION/$NAME"
REMOTE
}

cmd_remote_http_hashes() {
  remote_script "$HTTP_BASE" "$1" <<'REMOTE'
set -euo pipefail
BASE="$1"; FILES="$2"
hash_stdin() {
  if command -v sha256sum >/dev/null 2>&1; then sha256sum | cut -d' ' -f1
  else shasum -a 256 | cut -d' ' -f1; fi
}
while IFS= read -r rel; do
  [ -n "$rel" ] || continue
  body="$(curl -fsS "$BASE/$rel" 2>/dev/null | hash_stdin || true)"
  if [ -n "$body" ]; then printf '%s  %s\n' "$body" "$rel"
  else printf 'HTTP-FAIL  %s\n' "$rel"; fi
done <<< "$FILES"
REMOTE
}

print_manifest() {
  local title="$1" manifest="$2"
  printf '\n== %s ==\n' "$title"
  printf '%-64s %12s %5s %-12s %s\n' sha256 size mode owner path
  printf '%s\n' "$manifest" | while IFS= read -r line; do
    [ -n "$line" ] || continue
    # shellcheck disable=SC2086
    set -- $line
    printf '%-64s %12s %5s %-12s %s\n' "$1" "$2" "$3" "$4" "$5"
  done
}

# ---------------------------------------------------------------------------
# Commands
# ---------------------------------------------------------------------------

cmd_manifest() {
  require_host manifest "${1:-}"
  local files; files="$(fileset "${1:-bare}")"
  print_manifest "current TESTBOX test web root ($SB_TESTBOX_HOST:$WEB_ROOT)" "$(cmd_remote_manifest "$files")"
}

cmd_deploy() {
  local set="${1:-bare}"
  require_host deploy "$set"
  local files; files="$(fileset "$set")"

  local rel
  while IFS= read -r rel; do
    [ -n "$rel" ] || continue
    [ -f "$LOCAL_SRC/${rel#src/}" ] || die "missing local file: $LOCAL_SRC/${rel#src/}"
  done <<< "$files"

  local session; session="$(date -u +%Y%m%dT%H%M%SZ)-$set"
  printf 'deploy_bare_client: fileset=%s session=%s host=%s\n' "$set" "$session" "${SB_TESTBOX_HOST:-<local-sim>}"

  # 1. Manifest BEFORE.
  local before; before="$(cmd_remote_manifest "$files")"
  print_manifest 'BEFORE (remote, pre-deploy)' "$before"

  # 2. Back up and record the pre-deploy manifest inside the session dir.
  cmd_remote_backup "$session" "$files" >/dev/null
  local manifest_dir; manifest_dir="$(mktemp -d)"
  printf '%s\n' "$before" > "$manifest_dir/manifest.before"
  remote_copy "$BACKUP_ROOT/$session" "$manifest_dir/manifest.before"
  rm -rf "$manifest_dir"
  printf 'backup written: %s/%s (manifest.before + file copies)\n' "$BACKUP_ROOT" "$session"

  # 3. Stage and install the patched files.
  local stage_dir
  if [ "$SIM" = "1" ]; then
    stage_dir="$STAGE_ROOT/$session"
    rm -rf "$stage_dir"
    mkdir -p "$stage_dir/src"
  else
    stage_dir="$STAGE_ROOT/$session"
    remote_script "$stage_dir" <<'REMOTE'
set -euo pipefail
rm -rf "$1" && mkdir -p "$1/src"
REMOTE
  fi
  local stage_files=()
  while IFS= read -r rel; do
    [ -n "$rel" ] || continue
    stage_files+=("$LOCAL_SRC/${rel#src/}")
  done <<< "$files"
  remote_copy "$stage_dir/src" "${stage_files[@]}"
  cmd_remote_install "$session"

  # 4. Manifest AFTER.
  local after; after="$(cmd_remote_manifest "$files")"
  print_manifest 'AFTER (remote, post-deploy)' "$after"

  # 5. The deployed bytes must equal the repo bytes.
  local verdict=0 want got
  while IFS= read -r rel; do
    [ -n "$rel" ] || continue
    want="$(local_hash "$LOCAL_SRC/${rel#src/}")"
    got="$(printf '%s\n' "$after" | awk -v p="$rel" '$5 == p {print $1}')"
    if [ "$want" = "$got" ]; then
      printf 'VERIFY  repo==deployed  %s  %s\n' "$rel" "$want"
    else
      printf 'VERIFY  MISMATCH       %s  repo=%s deployed=%s\n' "$rel" "$want" "$got" >&2
      verdict=1
    fi
  done <<< "$files"

  # 6. Cross-check the bytes the signalling server actually serves.
  if [ "${SB_SKIP_HTTP_VERIFY:-0}" != "1" ]; then
    local http; http="$(cmd_remote_http_hashes "$files")"
    printf '\n== served bytes (%s) ==\n%s\n' "$HTTP_BASE" "$http"
    while IFS= read -r rel; do
      [ -n "$rel" ] || continue
      want="$(local_hash "$LOCAL_SRC/${rel#src/}")"
      got="$(printf '%s\n' "$http" | awk -v p="$rel" '$2 == p {print $1}')"
      case "$got" in
        HTTP-FAIL|"") printf 'VERIFY  HTTP-FAIL       %s (server may not be on %s)\n' "$rel" "$HTTP_BASE" >&2; verdict=1 ;;
        "$want")      printf 'VERIFY  served==repo    %s  %s\n' "$rel" "$want" ;;
        *)            printf 'VERIFY  SERVED MISMATCH %s  repo=%s served=%s\n' "$rel" "$want" "$got" >&2; verdict=1 ;;
      esac
    done <<< "$files"
  fi

  printf '\nDEPLOY-%s session=%s\n' "$([ "$verdict" = 0 ] && echo OK || echo FAIL)" "$session"
  printf 'revert with: %s %s revert %s\n' \
    "$([ "$SIM" = 1 ] && echo 'SB_LOCAL_SIM=1 SB_TESTBOX_HOST=sim' || echo "SB_TESTBOX_HOST=$SB_TESTBOX_HOST")" \
    "$0" "$session"
  [ "$verdict" = 0 ] || die 'deployed bytes did not match the repo; revert with the session id above'
}

cmd_revert() {
  require_host revert "${1:-}"
  local session="${1:-}"
  if [ -z "$session" ]; then
    session="$(cmd_remote_latest_session)"
    [ -n "$session" ] || die "no backup sessions under $BACKUP_ROOT"
    printf 'deploy_bare_client: using newest session %s\n' "$session"
  fi

  local files recorded
  files="$(cmd_remote_read_backup_file "$session" fileset)"
  [ -n "$files" ] || die "no recorded fileset for session $session under $BACKUP_ROOT"
  printf '%s\n' "$files" | grep -qE '^src/' || die "recorded fileset for $session looks wrong"

  recorded="$(cmd_remote_read_backup_file "$session" manifest.before)"
  print_manifest "RECORDED pre-deploy state ($session)" "$recorded"

  print_manifest 'BEFORE (remote, pre-revert)' "$(cmd_remote_manifest "$files")"
  cmd_remote_restore "$session" "$files"
  local after; after="$(cmd_remote_manifest "$files")"
  print_manifest 'AFTER (remote, post-revert)' "$after"

  if [ "$(printf '%s\n' "$recorded" | sort)" = "$(printf '%s\n' "$after" | sort)" ]; then
    printf '\nREVERT-OK session=%s — post-revert manifest is identical to the recorded pre-deploy manifest\n' "$session"
  else
    printf '\nREVERT-MISMATCH session=%s\n--- recorded ---\n%s\n--- actual ---\n%s\n' "$session" "$recorded" "$after" >&2
    die 'revert did not restore the recorded state'
  fi
}

usage() { sed -n '2,40p' "$0" | sed 's/^# \{0,1\}//'; }

case "${1:-}" in
  deploy)   shift; cmd_deploy "$@" ;;
  revert)   shift; cmd_revert "$@" ;;
  manifest) shift; cmd_manifest "$@" ;;
  -h|--help|help|'') usage ;;
  *) die "unknown command '$1' (expected deploy|revert|manifest)" ;;
esac
