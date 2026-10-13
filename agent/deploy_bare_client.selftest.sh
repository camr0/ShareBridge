#!/usr/bin/env bash
#
# deploy_bare_client.selftest.sh — proves deploy_bare_client.sh is reversible,
# entirely on this machine, without touching TESTBOX.
#
# It builds a throwaway "web root" with sentinel pristine files (and NO
# bareReceive.js), runs `SB_LOCAL_SIM=1 deploy all`, checks every deployed file
# byte-equals the repo, then runs `revert` and checks every file is back to its
# sentinel bytes and bareReceive.js is gone again.
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SRC="$HERE/../signaling-server/web/src"

fail() { printf 'SELFTEST FAIL: %s\n' "$*" >&2; exit 1; }
pass() { printf 'SELFTEST ok: %s\n' "$*"; }

hash_file() {
  if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1" | cut -d' ' -f1
  else shasum -a 256 "$1" | cut -d' ' -f1; fi
}

SANDBOX="$(mktemp -d "${TMPDIR:-/tmp}/sb-bare-selftest.XXXXXX")"
trap 'rm -rf "$SANDBOX"' EXIT

WEB="$SANDBOX/web"
BACKUP="$SANDBOX/backup"
STAGE="$SANDBOX/stage"
mkdir -p "$WEB/src"

# Pristine sentinels for every file the script may deploy. bareReceive.js is
# deliberately absent: the revert must delete files that did not exist before.
for f in app.js binaryEnvelope.js channelSet.js directChannel.js bulkReorder.js; do
  printf '/* pristine EXP-selftest %s */\n' "$f" > "$WEB/src/$f"
  chmod 644 "$WEB/src/$f"
done

before_hashes="$SANDBOX/before.hashes"
( cd "$WEB" && find . -type f | sort | while IFS= read -r f; do printf '%s  %s\n' "$(hash_file "$f")" "$f"; done ) > "$before_hashes"
[ -f "$WEB/src/bareReceive.js" ] && fail 'sandbox is not pristine'

run() {
  SB_LOCAL_SIM=1 SB_TESTBOX_HOST=sim \
  SB_WEB_ROOT="$WEB" SB_BACKUP_ROOT="$BACKUP" SB_STAGE_ROOT="$STAGE" \
  SB_REMOTE_SUDO="" SB_SKIP_HTTP_VERIFY=1 \
  "$HERE/deploy_bare_client.sh" "$@"
}

printf '=== deploy all (simulated, no network) ===\n'
deploy_out="$SANDBOX/deploy.out"
run deploy all | tee "$deploy_out"
grep -q '^DEPLOY-OK' "$deploy_out" || fail 'deploy did not report DEPLOY-OK'
SESSION="$(sed -n 's/^DEPLOY-OK session=//p' "$deploy_out")"
[ -n "$SESSION" ] || fail 'deploy did not print a session id'
pass "deploy reported DEPLOY-OK for session $SESSION"

# Every deployed file must equal the repo copy.
for f in app.js bareReceive.js binaryEnvelope.js channelSet.js directChannel.js bulkReorder.js; do
  [ -f "$WEB/src/$f" ] || fail "deploy left $f missing"
  a="$(hash_file "$WEB/src/$f")"; b="$(hash_file "$SRC/$f")"
  [ "$a" = "$b" ] || fail "$f not byte-identical to the repo after deploy"
done
pass 'all six deployed files are byte-identical to the repo'

printf '\n=== revert ===\n'
revert_out="$SANDBOX/revert.out"
run revert "$SESSION" | tee "$revert_out"
grep -q '^REVERT-OK' "$revert_out" || fail 'revert did not report REVERT-OK'
pass 'revert reported REVERT-OK'

# Every pre-existing file must be back to its sentinel bytes, and the new file gone.
after_hashes="$SANDBOX/after.hashes"
( cd "$WEB" && find . -type f | sort | while IFS= read -r f; do printf '%s  %s\n' "$(hash_file "$f")" "$f"; done ) > "$after_hashes"
diff -u "$before_hashes" "$after_hashes" || fail 'post-revert file set/hashes differ from pre-deploy'
[ -f "$WEB/src/bareReceive.js" ] && fail 'bareReceive.js survived the revert'
pass 'post-revert manifest is byte-identical to the pre-deploy manifest'

printf '\n=== revert of the newest session (no explicit id) ===\n'
run deploy bare >/dev/null
run revert | tee "$SANDBOX/revert2.out" >/dev/null
grep -q '^REVERT-OK' "$SANDBOX/revert2.out" || fail 'newest-session revert did not report REVERT-OK'
diff -u "$before_hashes" <( cd "$WEB" && find . -type f | sort | while IFS= read -r f; do printf '%s  %s\n' "$(hash_file "$f")" "$f"; done ) \
  || fail 'newest-session revert left the tree changed'
pass 'newest-session revert also round-trips'

# A bare-only deploy must install app.js and its own modules, and leave the N5
# modules untouched.
printf '\n=== bare fileset deploys app.js + its modules, not the N5 extras ===\n'
run deploy bare >/dev/null
[ -f "$WEB/src/bareReceive.js" ] || fail 'bare deploy did not install bareReceive.js'
[ -f "$WEB/src/bulkReorder.js" ] || fail 'bare deploy must install bulkReorder.js (app.js imports it)'
if grep -q 'pristine EXP-selftest' "$WEB/src/binaryEnvelope.js"; then pass 'bare fileset left binaryEnvelope.js untouched'; else fail 'bare fileset overwrote binaryEnvelope.js'; fi
if grep -q 'pristine EXP-selftest' "$WEB/src/directChannel.js"; then pass 'bare fileset left directChannel.js untouched'; else fail 'bare fileset overwrote directChannel.js'; fi
run revert >/dev/null
diff -u "$before_hashes" <( cd "$WEB" && find . -type f | sort | while IFS= read -r f; do printf '%s  %s\n' "$(hash_file "$f")" "$f"; done ) \
  || fail 'bare-fileset revert left the tree changed'

# A module app.js imports but the web root does not have must be installed by the
# deploy and removed again by the revert.
printf '\n=== a module missing from the web root round-trips too ===\n'
rm -f "$WEB/src/bulkReorder.js"
rm -f "$before_hashes"
( cd "$WEB" && find . -type f | sort | while IFS= read -r f; do printf '%s  %s\n' "$(hash_file "$f")" "$f"; done ) > "$before_hashes"
run deploy bare >/dev/null
[ -f "$WEB/src/bulkReorder.js" ] || fail 'deploy did not replace the missing bulkReorder.js'
run revert >/dev/null
[ -f "$WEB/src/bulkReorder.js" ] && fail 'bulkReorder.js survived a revert to a web root that did not have it'
diff -u "$before_hashes" <( cd "$WEB" && find . -type f | sort | while IFS= read -r f; do printf '%s  %s\n' "$(hash_file "$f")" "$f"; done ) \
  || fail 'missing-module revert left the tree changed'
pass 'a module absent before the deploy is removed again by the revert'

printf '\nSELFTEST PASS\n'
