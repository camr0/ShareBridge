#!/usr/bin/env bash
#
# deploy_test.sh — static deployment-hardening gate for the separate relay VM
# (spec §4.6, §6, §17.1; plan Task 35).
#
# This test does NOT need systemd, root, or a relay VM: it parses the
# committed deployment artifacts (the two systemd units, the nftables ruleset,
# install.sh and the operator runbook) and asserts the hardening properties
# the deployment must prove. `systemd-analyze verify` is run separately in a
# Linux container to check the units are syntactically valid.
#
# Usage:  bash relay/deploy/deploy_test.sh
# Exit:   0 = every assertion passed (GREEN); 1 = at least one failed (RED).
#
# The assertion IDs (A1…A16) are referenced by the Task 35 report so each
# published hardening claim maps to the check that enforces it. A3/A4/A6/A15
# are strict-parsing assertions: a duplicate, redefined or extra directive is a
# failure, never silently ignored, and A12/A13 prove their properties by
# executing the installer (a poisoned cache, a tampered installed artifact, a
# wrong checksum and a garbage AAAA answer must all fail closed).

set -u

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd "${script_dir}/../.." && pwd)"
deploy_dir="${repo_root}/relay/deploy"
frps_config="${repo_root}/relay/config/frps.toml"
ops_doc="${repo_root}/docs/operations/phase4a-relay.md"
readme="${repo_root}/relay/README.md"

gateway_unit="${deploy_dir}/sharebridge-relay-gateway.service"
frps_unit="${deploy_dir}/sharebridge-relay-frps.service"
firewall="${deploy_dir}/firewall.nft"
install_sh="${deploy_dir}/install.sh"
fetch_sh="${repo_root}/relay/scripts/fetch-frp.sh"
frp_manifest="${repo_root}/relay/frp/manifest.json"
plan_doc="${repo_root}/docs/superpowers/plans/2026-09-03-phase4a-relay-mvp.md"

failures=0
checks=0

fail() {
  printf 'FAIL: %s\n' "$*" >&2
  failures=$((failures + 1))
}

pass() {
  checks=$((checks + 1))
}

# unit_field <file> <Key> — print every value of the systemd key, one per line.
# Section headers and comments are skipped; `Key=value` is matched exactly.
unit_field() {
  awk -v want="$2" '
    { line = $0 }
    line ~ /^[[:space:]]*\[/ { next }
    {
      sub(/[[:space:]]*#.*$/, "", line)
      eq = index(line, "=")
      if (eq == 0) { next }
      key = substr(line, 1, eq - 1)
      val = substr(line, eq + 1)
      gsub(/^[[:space:]]+|[[:space:]]+$/, "", key)
      gsub(/^[[:space:]]+|[[:space:]]+$/, "", val)
      if (key == want && val != "") { print val }
    }
  ' "$1"
}

# unit_field_one <file> <Key> — the single value, or empty when absent.
unit_field_one() {
  unit_field "$1" "$2" | head -n 1
}

# require_file <path> <description>
require_file() {
  if [[ -f "$1" ]]; then
    pass
  else
    fail "$2 missing: $1"
  fi
}

# require_contains <file> <extended-regex> <description>
require_contains() {
  if [[ -f "$1" ]] && grep -Eq -- "$2" "$1"; then
    pass
  else
    fail "$3 (expected /$2/ in $1)"
  fi
}

# require_not_contains <file> <extended-regex> <description>
require_not_contains() {
  if [[ -f "$1" ]] && grep -Eq -- "$2" "$1"; then
    fail "$3 (forbidden /$2/ found in $1)"
  else
    pass
  fi
}

# require_eq <actual> <expected> <description>
require_eq() {
  if [[ "$1" == "$2" ]]; then
    pass
  else
    fail "$3 (got '$1', want '$2')"
  fi
}

# shell_var_values <file> <NAME> — every non-comment `NAME=<value>` (or
# `export NAME=<value>`) assignment in a shell file, printing the value with one
# layer of matching single/double quotes stripped. Used to cross-check the
# installer's account/library constants against the units: a rename in
# install.sh must fail the gate even though the units are checked separately.
shell_var_values() {
  local raw
  while IFS= read -r raw; do
    [[ -z "$raw" ]] && continue
    case "$raw" in
      \"*\") raw="${raw#\"}"; raw="${raw%\"}" ;;
      \'*\') raw="${raw#\'}"; raw="${raw%\'}" ;;
    esac
    printf '%s\n' "$raw"
  done < <(parsed_lines "$1" \
    | grep -E "^[[:space:]]*(export[[:space:]]+)?$2=" \
    | sed -E "s/^[[:space:]]*(export[[:space:]]+)?$2=//")
}

# require_shell_var <file> <NAME> <expected> <description> — the installer must
# assign NAME exactly once and to exactly the expected value. A second,
# countermanding assignment (a later `GATEWAY_USER=daemon`) is a failure, never
# a silently-used override.
require_shell_var() {
  local count value
  count="$(shell_var_values "$1" "$2" | grep -c . || true)"
  value="$(shell_var_values "$1" "$2" | head -n 1)"
  if [[ "$count" != "1" ]]; then
    fail "$4 ($1 assigns $2 ${count}x; exactly one is required)"
  elif [[ "$value" != "$3" ]]; then
    fail "$4 ($1 has $2='$value', want '$3')"
  else
    pass
  fi
}

# is_positive_int <value>
is_positive_int() {
  [[ "$1" =~ ^[0-9]+$ ]] && (( $1 > 0 ))
}

# unit_key_lines <file> <Key> — every non-comment `Key=` assignment line, in
# order, whether or not the value is empty.
unit_key_lines() {
  awk -v want="$2" '
    { line = $0 }
    line ~ /^[[:space:]]*\[/ { next }
    {
      sub(/[[:space:]]*#.*$/, "", line)
      eq = index(line, "=")
      if (eq == 0) { next }
      key = substr(line, 1, eq - 1)
      gsub(/^[[:space:]]+|[[:space:]]+$/, "", key)
      if (key == want) { print line }
    }
  ' "$1"
}

# unit_key_count <file> <Key> — the number of `Key=` assignments in a unit.
unit_key_count() {
  unit_key_lines "$1" "$2" | grep -c . || true
}

# unit_duplicate_keys <file> — every non-comment unit key assigned more than once.
unit_duplicate_keys() {
  awk '
    { line = $0 }
    line ~ /^[[:space:]]*\[/ { next }
    {
      sub(/[[:space:]]*#.*$/, "", line)
      eq = index(line, "=")
      if (eq == 0) { next }
      key = substr(line, 1, eq - 1)
      gsub(/^[[:space:]]+|[[:space:]]+$/, "", key)
      if (key != "") { print key }
    }
  ' "$1" | sort | uniq -d
}

# unit_environment_var_duplicates <file> — `Environment=` variable names
# assigned more than once in a unit. The later assignment would win in systemd
# and could silently override a checked listener bind.
unit_environment_var_duplicates() {
  awk '
    { line = $0 }
    line ~ /^[[:space:]]*\[/ { next }
    {
      sub(/[[:space:]]*#.*$/, "", line)
      eq = index(line, "=")
      if (eq == 0) { next }
      key = substr(line, 1, eq - 1)
      gsub(/^[[:space:]]+|[[:space:]]+$/, "", key)
      if (key != "Environment") { next }
      val = substr(line, eq + 1)
      gsub(/^[[:space:]]+|[[:space:]]+$/, "", val)
      veq = index(val, "=")
      if (veq == 0) { next }
      name = substr(val, 1, veq - 1)
      gsub(/^[[:space:]]+|[[:space:]]+$/, "", name)
      if (name != "") { print name }
    }
  ' "$1" | sort | uniq -d
}

# unit_credential_id_duplicates <file> — `LoadCredential=` ids declared more
# than once in a unit (a duplicate id can redirect a credential read).
unit_credential_id_duplicates() {
  awk '
    { line = $0 }
    line ~ /^[[:space:]]*\[/ { next }
    {
      sub(/[[:space:]]*#.*$/, "", line)
      eq = index(line, "=")
      if (eq == 0) { next }
      key = substr(line, 1, eq - 1)
      gsub(/^[[:space:]]+|[[:space:]]+$/, "", key)
      if (key != "LoadCredential") { next }
      val = substr(line, eq + 1)
      gsub(/^[[:space:]]+|[[:space:]]+$/, "", val)
      cidx = index(val, ":")
      if (cidx == 0) { next }
      name = substr(val, 1, cidx - 1)
      gsub(/^[[:space:]]+|[[:space:]]+$/, "", name)
      if (name != "") { print name }
    }
  ' "$1" | sort | uniq -d
}

# require_unit_scalar <file> <Key> <expected> <description> — the key must be
# assigned EXACTLY once and carry exactly the expected value. A duplicate (a
# later/countermanding definition) is a failure, never "any one of them is fine".
require_unit_scalar() {
  local count value
  count="$(unit_key_count "$1" "$2")"
  value="$(unit_field_one "$1" "$2")"
  if [[ "$count" != "1" ]]; then
    fail "$4 ($1 assigns $2 ${count}x; exactly one is required)"
  elif [[ "$value" != "$3" ]]; then
    fail "$4 ($1 has $2='$value', want '$3')"
  else
    pass
  fi
}

# function_def_count <file> <name> — the number of shell function definitions,
# covering both `name() {` and `function name {`/`function name() {` forms.
function_def_count() {
  grep -cE "^[[:space:]]*(function[[:space:]]+)?$2[[:space:]]*(\(\))?[[:space:]]*\{" "$1" || true
}

# require_unique_function_def <file> <name> <description> — a redefined
# security-critical function must fail the gate, not silently shadow the first.
require_unique_function_def() {
  local count
  count="$(function_def_count "$1" "$2")"
  if [[ "$count" == "1" ]]; then
    pass
  else
    fail "$3 (found ${count:-0} definitions of $2 in $1; exactly one is required)"
  fi
}

# function_body <file> <name> — the non-comment, non-blank body lines.
function_body() {
  awk -v want="$2" '
    $0 ~ "^[[:space:]]*(function[[:space:]]+)?" want "[[:space:]]*(\\(\\))?[[:space:]]*\\{" { inc=1; next }
    inc && $0 ~ /^\}/ { inc=0; next }
    inc {
      sub(/[[:space:]]*#.*$/, "", $0)
      gsub(/^[[:space:]]+|[[:space:]]+$/, "", $0)
      if ($0 != "") { print }
    }
  ' "$1"
}

# ---------------------------------------------------------------------------
# Hardening-directive discipline (A4).
#
# Presence is NOT proof. In systemd an emptied/reset sandbox directive is a
# no-op (`SystemCallFilter=`, `CapabilityBoundingSet=`, `RestrictNamespaces=`,
# `RestrictAddressFamilies=`, … all disable the restriction when emptied), so a
# gate that only asks "is the key present?" lets a real hardening violation
# ship while reporting GREEN. The rule this gate applies to every hardening
# directive is:
#
#   (1) every directive pinned in HARDENING_TABLE_<unit> is assigned EXACTLY
#       once with the exact expected value — an empty, reset, weakened,
#       duplicated or countermanding definition is a failure. Empty is the
#       expected value only where the table deliberately expects empty (the
#       frps capability-free set);
#   (2) every directive the unit actually carries whose name is in
#       HARDENING_VOCABULARY (the systemd sandboxing vocabulary) MUST be pinned
#       in that unit's table, so a newly added hardening directive cannot ship
#       unasserted; and
#   (3) no carried hardening directive may be empty unless it is explicitly
#       allowlisted in HARDENING_EMPTY_OK.
#
# How a NEW hardening directive gets covered: add its `Key=expected` row to the
# unit table, and if its name is not already in HARDENING_VOCABULARY add it
# there too. Forgetting either fails the gate — a carried-but-unpinned
# directive fails (2), a pinned-but-missing/emptied/duplicated directive fails
# (1), and a table row whose name is absent from the vocabulary fails the
# consistency check. The table is therefore both the assertion set and the
# allowlist of hardening directives the units may carry.
HARDENING_VOCABULARY='NoNewPrivileges SecureBits CapabilityBoundingSet AmbientCapabilities PrivateTmp PrivateDevices PrivateNetwork PrivateUsers PrivateMounts PrivateIPC ProtectSystem ProtectHome ProtectKernelTunables ProtectKernelModules ProtectKernelLogs ProtectControlGroups ProtectClock ProtectHostname ProtectProc ProcSubset RestrictAddressFamilies RestrictNamespaces RestrictRealtime RestrictSUIDSGID LockPersonality MemoryDenyWriteExecute SystemCallArchitectures SystemCallFilter SystemCallErrorNumber SystemCallLog RemoveIPC UMask KeyringMode NoExecPaths DevicePolicy DeviceAllow DeviceDeny IPAddressAllow IPAddressDeny SocketBindAllow SocketBindDeny MountFlags ReadWritePaths ReadOnlyPaths InaccessiblePaths BindPaths BindReadOnlyPaths TemporaryFileSystem RootDirectory RootImage StateDirectory RuntimeDirectory CacheDirectory LogsDirectory ConfigurationDirectory MemoryMax LimitNOFILE TasksMax LimitNPROC LogRateLimitIntervalSec LogRateLimitBurst'

# Directives whose EXPECTED hardened value is the empty string: these units are
# intentionally capability-free, so an empty value here is the assertion ("no
# capabilities"), not a neutering. Any other empty hardening value fails (3).
HARDENING_EMPTY_OK=' CapabilityBoundingSet AmbientCapabilities '

# One `Key=expected value` per hardening directive, per unit. The expected
# value is everything after the first '=' (so multi-word sets such as
# ReadWritePaths= and RestrictAddressFamilies= are compared verbatim).
HARDENING_TABLE_GATEWAY='ProtectSystem=strict
NoNewPrivileges=true
PrivateTmp=true
PrivateDevices=true
ProtectHome=true
ProtectKernelTunables=true
ProtectKernelModules=true
ProtectKernelLogs=true
ProtectControlGroups=true
ProtectClock=true
ProtectHostname=true
ProtectProc=invisible
ProcSubset=pid
RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6 AF_NETLINK
RestrictNamespaces=true
RestrictRealtime=true
RestrictSUIDSGID=true
LockPersonality=true
MemoryDenyWriteExecute=true
SystemCallArchitectures=native
SystemCallFilter=@system-service
SystemCallErrorNumber=EPERM
RemoveIPC=true
UMask=0077
StateDirectory=sharebridge-relay-gateway
RuntimeDirectory=sharebridge-relay-gateway
ReadWritePaths=/var/lib/sharebridge-relay-gateway /run/sharebridge-relay-gateway
AmbientCapabilities=CAP_NET_BIND_SERVICE
CapabilityBoundingSet=CAP_NET_BIND_SERVICE
LimitNOFILE=65536
MemoryMax=512M
LogRateLimitIntervalSec=30
LogRateLimitBurst=200'

HARDENING_TABLE_FRPS='ProtectSystem=strict
NoNewPrivileges=true
PrivateTmp=true
PrivateDevices=true
ProtectHome=true
ProtectKernelTunables=true
ProtectKernelModules=true
ProtectKernelLogs=true
ProtectControlGroups=true
ProtectClock=true
ProtectHostname=true
ProtectProc=invisible
ProcSubset=pid
RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6 AF_NETLINK
RestrictNamespaces=true
RestrictRealtime=true
RestrictSUIDSGID=true
LockPersonality=true
MemoryDenyWriteExecute=true
SystemCallArchitectures=native
SystemCallFilter=@system-service
SystemCallErrorNumber=EPERM
RemoveIPC=true
UMask=0077
StateDirectory=sharebridge-relay-frps
RuntimeDirectory=sharebridge-relay-frps
ReadWritePaths=/run/sharebridge-relay-frps
CapabilityBoundingSet=
AmbientCapabilities=
LimitNOFILE=65536
MemoryMax=256M
LogRateLimitIntervalSec=30
LogRateLimitBurst=200'

# check_hardening_table <unit> <table> — every pinned directive exactly once,
# exactly the expected value (rule 1).
check_hardening_table() {
  local unit="$1" table="$2" entry key expected
  while IFS= read -r entry; do
    [[ -z "$entry" ]] && continue
    key="${entry%%=*}"
    expected="${entry#*=}"
    require_unit_scalar "$unit" "$key" "$expected" \
      "$unit hardening directive must be exactly ${key}=${expected}"
  done <<< "$table"
}

# hardening_pinned_keys <table> — the space-separated keys pinned by a table.
hardening_pinned_keys() {
  local table="$1" entry out=""
  while IFS= read -r entry; do
    [[ -z "$entry" ]] && continue
    out="${out} ${entry%%=*}"
  done <<< "$table"
  printf '%s\n' "$out"
}

# hardening_keys_in_unit <unit> — every directive key the unit carries whose
# name belongs to the systemd hardening vocabulary, sorted and unique.
hardening_keys_in_unit() {
  awk -v vocab="$HARDENING_VOCABULARY" '
    BEGIN { n = split(vocab, v, " "); for (i = 1; i <= n; i++) hard[v[i]] = 1 }
    { line = $0 }
    line ~ /^[[:space:]]*\[/ { next }
    {
      sub(/[[:space:]]*#.*$/, "", line)
      eq = index(line, "=")
      if (eq == 0) { next }
      key = substr(line, 1, eq - 1)
      gsub(/^[[:space:]]+|[[:space:]]+$/, "", key)
      if (key in hard) { print key }
    }
  ' "$1" | sort -u
}

# require_table_in_vocabulary <unit> <table> — a table row for a name that is
# not in the hardening vocabulary is a maintenance error (the vocabulary and
# the table must stay in sync).
require_table_in_vocabulary() {
  local unit="$1" table="$2" entry key
  while IFS= read -r entry; do
    [[ -z "$entry" ]] && continue
    key="${entry%%=*}"
    case " $HARDENING_VOCABULARY " in
      *" $key "*) pass ;;
      *) fail "$unit hardening table pins '${key}' which is absent from HARDENING_VOCABULARY" ;;
    esac
  done <<< "$table"
}

# require_pinned_hardening <unit> <pinned keys> — rule (2): a carried hardening
# directive that is not pinned fails (an unasserted hardening directive must not
# be able to ship).
require_pinned_hardening() {
  local unit="$1" pinned=" $2 " key
  while IFS= read -r key; do
    [[ -z "$key" ]] && continue
    case "$pinned" in
      *" $key "*) pass ;;
      *) fail "$unit carries hardening directive ${key}= that is not pinned in the gate's hardening table" ;;
    esac
  done < <(hardening_keys_in_unit "$unit")
}

# require_no_empty_hardening <unit> — rule (3): no carried hardening directive
# may be empty unless it is explicitly allowlisted as intentionally empty. This
# is the direct anti-neutering invariant: emptying ANY hardening directive (a
# known one, or a future one already listed in the vocabulary) fails here even
# if it were somehow missed by the exact-value table.
require_no_empty_hardening() {
  local unit="$1" key value
  while IFS= read -r key; do
    [[ -z "$key" ]] && continue
    case "$HARDENING_EMPTY_OK" in
      *" $key "*) pass ; continue ;;
    esac
    value="$(unit_field_one "$unit" "$key")"
    if [[ -z "$value" ]]; then
      fail "$unit has an empty/reset hardening directive ${key}= (neutered)"
    else
      pass
    fi
  done < <(hardening_keys_in_unit "$unit")
}

# The strict IPv6 validator under test lives in install.sh (is_ipv6_literal).
# deploy_test.sh deliberately does NOT keep a second copy: a duplicate, dead
# copy can drift and, as an unexercised parser, would silently hide a regression
# in the executed one. A13 proves the installer's validator behaviourally.

# parsed_lines <file> — the file with comments removed (a `#` starts a comment).
# The deployment artifacts contain no literal `#` inside a quoted value, so
# this is the shell/nft/unit directive stream. A commented-out directive or a
# value that only appears in prose can no longer satisfy an assertion.
parsed_lines() {
  awk '{ sub(/[[:space:]]*#.*/, ""); print }' "$1"
}

# require_parsed_contains <file> <extended-regex> <description>
require_parsed_contains() {
  if [[ -f "$1" ]] && parsed_lines "$1" | grep -Eq -- "$2"; then
    pass
  else
    fail "$3 (expected /$2/ in the parsed directives of $1)"
  fi
}

# sha256_of <file> — the lowercase-hex SHA-256, via sha256sum or shasum.
sha256_of() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | awk '{print $1}'
  else
    shasum -a 256 "$1" | awk '{print $1}'
  fi
}

# nft_chain <file> <chain> — print the body of one nftables chain.
nft_chain() {
  awk -v want="$2" '
    $0 ~ "^[[:space:]]*chain[[:space:]]+" want "[[:space:]]*\\{" { inc=1; next }
    inc && $0 ~ /^[[:space:]]*\}/ { inc=0; next }
    inc { print }
  ' "$1"
}

# nft_set_elements <file> <set> — print every element of one nftables set, one
# per line, across a multi-line `elements = { ... }` block.
nft_set_elements() {
  awk -v want="$2" '
    {
      line=$0
      sub(/[[:space:]]*#.*/, "", line)
      if (inel) {
        buf = buf " " line
        if (line ~ /}/) { emit(); inel=0; buf="" }
        next
      }
      if (cur != "" && line ~ /^[[:space:]]*elements[[:space:]]*=/) {
        inel=1; buf=line
        if (line ~ /}/) { emit(); inel=0; buf="" }
        next
      }
      if (line ~ /^[[:space:]]*set[[:space:]]+/) {
        t=line; sub(/^[[:space:]]*set[[:space:]]+/, "", t); sub(/[[:space:]].*$/, "", t); cur=t; next
      }
      if (line ~ /^[[:space:]]*\}/) { cur=""; next }
    }
    function emit(   n, i, arr) {
      if (cur != want) return
      sub(/^[^{]*\{/, "", buf); sub(/\}.*$/, "", buf)
      n=split(buf, arr, ",")
      for (i=1; i<=n; i++) { gsub(/[[:space:]]/, "", arr[i]); if (arr[i] != "") print arr[i] }
    }
  ' "$1"
}

printf '== Task 35 relay deployment gate ==\n'
printf -- '-- A1: deployment artifacts present\n'
require_file "$gateway_unit" "gateway systemd unit"
require_file "$frps_unit" "frps systemd unit"
require_file "$firewall" "nftables ruleset"
require_file "$install_sh" "installer"
require_file "$readme" "relay README"
require_file "$ops_doc" "relay operations runbook"
require_file "$frps_config" "pinned frps config template"
require_file "$fetch_sh" "pinned-FRP fetch script"
require_file "$frp_manifest" "pinned FRP manifest"
require_file "$plan_doc" "phase-4a relay plan"

# Nothing below can be meaningful when the units are absent (the RED state).
if [[ ! -f "$gateway_unit" || ! -f "$frps_unit" || ! -f "$firewall" || ! -f "$install_sh" ]]; then
  printf '\n== %d checks, %d failure(s) ==\n' "$checks" "$failures"
  printf 'RESULT: RED — deployment artifacts are incomplete\n'
  exit 1
fi

printf -- '-- A2: exact dedicated service identities and accounts (pinned, not properties)\n'
# The deployment property is not "non-root, non-nobody, distinct": it is that
# each unit names exactly the dedicated account the installer creates, and that
# the account is the one the rest of the deployment assumes. The gateway
# account owns /var/lib/sharebridge-relay-gateway and reads the gateway's
# systemd credentials; the frps account owns /var/lib/sharebridge-relay-frps.
# Neither service may run as a shared/generic host account (root, nobody,
# daemon, www-data, ...): running the gateway as a generic account would let it
# reach the frps state (or the host) with rights it was never granted, and vice
# versa. The expected values below are the single source of truth for the
# installer cross-check, so neither side can be renamed alone.
EXPECTED_GATEWAY_ACCOUNT="sharebridge-relay-gateway"
EXPECTED_FRPS_ACCOUNT="sharebridge-relay-frps"
EXPECTED_LIB_DIR="/usr/local/lib/sharebridge/relay"
gateway_user="$(unit_field_one "$gateway_unit" User)"
frps_user="$(unit_field_one "$frps_unit" User)"
require_unit_scalar "$gateway_unit" User "$EXPECTED_GATEWAY_ACCOUNT" \
  "gateway must run as the dedicated sharebridge-relay-gateway account"
require_unit_scalar "$gateway_unit" Group "$EXPECTED_GATEWAY_ACCOUNT" \
  "gateway must run in its matching sharebridge-relay-gateway group"
require_unit_scalar "$frps_unit" User "$EXPECTED_FRPS_ACCOUNT" \
  "frps must run as the dedicated sharebridge-relay-frps account"
require_unit_scalar "$frps_unit" Group "$EXPECTED_FRPS_ACCOUNT" \
  "frps must run in its matching sharebridge-relay-frps group"
# A further identity directive would override or add to the pinned pair.
for unit in "$gateway_unit" "$frps_unit"; do
  require_not_contains "$unit" '^[[:space:]]*(DynamicUser|SupplementaryGroups)=' \
    "$unit must not override its pinned identity with an implicit/extra identity directive"
done
# Defence in depth: keep the weak-property checks too, so a future edit that
# loosens the exact pin to a property still cannot name root/nobody/a shared
# account. (With the pins above these pass whenever the pin passes.)
if [[ "$gateway_user" == "root" || "$frps_user" == "root" ]]; then
  fail "services must not run as root (gateway='$gateway_user' frps='$frps_user')"
else
  pass
fi
if [[ "$gateway_user" == "nobody" || "$frps_user" == "nobody" ]]; then
  fail "services must use dedicated accounts, not 'nobody'"
else
  pass
fi
require_eq "$([[ "$gateway_user" != "$frps_user" ]] && echo distinct)" "distinct" \
  "gateway and frps must run as distinct users"
# install.sh must create exactly the two accounts the units name, as system
# (loginless) users with a matching dedicated group, and must own each service
# state directory with that account. A rename on either side fails: the units'
# User=/Group= are pinned above and the installer's constants are pinned here.
require_shell_var "$install_sh" GATEWAY_USER "$EXPECTED_GATEWAY_ACCOUNT" \
  "installer must create the gateway account the unit runs as"
require_shell_var "$install_sh" FRPS_USER "$EXPECTED_FRPS_ACCOUNT" \
  "installer must create the frps account the unit runs as"
require_shell_var "$install_sh" LIB_DIR "$EXPECTED_LIB_DIR" \
  "installer must install the service binaries where the units execute them"
require_unique_function_def "$install_sh" create_service_user \
  "the service-account creator must be defined exactly once"
require_eq "$(parsed_lines "$install_sh" | grep -cE '^[[:space:]]*useradd([[:space:]]|$)')" "1" \
  "installer must invoke useradd exactly once (a single account-creation path)"
require_parsed_contains "$install_sh" \
  '^[[:space:]]*useradd[[:space:]]+--system[[:space:]]+--user-group[[:space:]]+--no-create-home[[:space:]]+--home-dir[[:space:]]+/nonexistent[[:space:]]+--shell[[:space:]]+/usr/sbin/nologin[[:space:]]+"\$name"[[:space:]]*$' \
  "create_service_user must create a --system loginless account with a matching group"
require_parsed_contains "$install_sh" '^[[:space:]]*create_service_user[[:space:]]+"\$GATEWAY_USER"[[:space:]]*$' \
  "installer must create the gateway account via the pinned constant"
require_parsed_contains "$install_sh" '^[[:space:]]*create_service_user[[:space:]]+"\$FRPS_USER"[[:space:]]*$' \
  "installer must create the frps account via the pinned constant"
require_eq "$(parsed_lines "$install_sh" | grep -cE '^[[:space:]]*create_service_user[[:space:]]+')" "2" \
  "installer must create exactly the two pinned service accounts"
require_parsed_contains "$install_sh" \
  'install[[:space:]]+-d[[:space:]]+-o[[:space:]]+"\$GATEWAY_USER"[[:space:]]+-g[[:space:]]+"\$GATEWAY_USER"[[:space:]]+-m[[:space:]]+0700[[:space:]]+/var/lib/sharebridge-relay-gateway' \
  "installer must own the gateway state directory by the pinned dedicated account/group"
require_parsed_contains "$install_sh" \
  'install[[:space:]]+-d[[:space:]]+-o[[:space:]]+"\$FRPS_USER"[[:space:]]+-g[[:space:]]+"\$FRPS_USER"[[:space:]]+-m[[:space:]]+0700[[:space:]]+/var/lib/sharebridge-relay-frps' \
  "installer must own the frps state directory by the pinned dedicated account/group"
unit_accounts="$(printf '%s\n' "$gateway_user" "$frps_user" | sort -u | tr '\n' ' ')"
installer_accounts="$( { shell_var_values "$install_sh" GATEWAY_USER; shell_var_values "$install_sh" FRPS_USER; } | sort -u | tr '\n' ' ')"
require_eq "$unit_accounts" "$installer_accounts" \
  "the units' User= accounts must be exactly the accounts install.sh creates"

printf -- '-- A16: exact ExecStart command lines (binary path and argument vector)\n'
# The gate must pin the exact command each unit runs, not merely "some non-empty
# command that is not a shell": a service that starts as /bin/true, or that
# executes the other service's binary, is a nonfunctional service the gate must
# reject. frps additionally MUST be given its credential-directory config flag
# (-c ${CREDENTIALS_DIRECTORY}/frps-config); without it frps starts with no
# transport configuration. The binaries named here are the artifacts install.sh
# installs at ${LIB_DIR} (cross-checked below), so the unit and the installer
# cannot drift apart.
require_unit_scalar "$gateway_unit" ExecStart "${EXPECTED_LIB_DIR}/gateway" \
  "gateway ExecStart must be exactly the installed gateway binary with no arguments"
require_unit_scalar "$frps_unit" ExecStart \
  "${EXPECTED_LIB_DIR}/frps -c \${CREDENTIALS_DIRECTORY}/frps-config" \
  "frps ExecStart must be exactly the installed frps binary with its credential config flag"
require_parsed_contains "$install_sh" '"\$\{LIB_DIR\}/gateway"' \
  "installer must install the gateway binary at the path the unit executes"
require_parsed_contains "$install_sh" '"\$\{LIB_DIR\}/frps"' \
  "installer must install the frps binary at the path the unit executes"

printf -- '-- A3: root-owned 0600 secrets and root config\n'
require_parsed_contains "$install_sh" 'install[[:space:]]+-o[[:space:]]+root[[:space:]]+-g[[:space:]]+root[[:space:]]+-m[[:space:]]+0?600' \
  "installer must install secrets 0600 root:root"
# The exact set of files installed through the 0600 root helper, derived from
# the parsed (non-comment) directives. A commented-out call or a mention in
# prose cannot satisfy this.
# The secret installer must have exactly one definition and its body must be
# exactly the 0600 root:root helper — a later redefinition/shadowing anywhere
# in the file is a failure, not a silently-used implementation.
require_unique_function_def "$install_sh" install_root_secret \
  "the root-secret installer must be defined exactly once"
require_eq "$(function_body "$install_sh" install_root_secret)" \
  'install -o root -g root -m 0600 -- "$1" "$2"' \
  "install_root_secret must install root:root mode 0600 and nothing else"
require_not_contains "$install_sh" '^[[:space:]]*install_root_secret=' \
  "installer must not shadow install_root_secret with a variable assignment"
require_not_contains "$install_sh" '^[[:space:]]*alias[[:space:]]+install_root_secret=' \
  "installer must not alias install_root_secret"
# Exactly the eight expected calls, no more and no fewer.
secret_call_count="$(parsed_lines "$install_sh" | grep -cE '^[[:space:]]*install_root_secret[[:space:]]+"')"
require_eq "$secret_call_count" "8" \
  "installer must call install_root_secret exactly 8 times (one per config/secret)"
secret_dests="$(parsed_lines "$install_sh" \
  | sed -n 's|.*install_root_secret[[:space:]].*/\([^"/]*\)"[[:space:]]*$|\1|p' | sort -u)"
expected_secret_dests="$(printf '%s\n' \
  control-ca.crt frps.env frps.toml gateway.env \
  gateway-sync.crt gateway-sync.key tunnel-server.crt tunnel-server.key | sort -u)"
require_eq "$secret_dests" "$expected_secret_dests" \
  "installer must install exactly the expected config/secrets via install_root_secret as 0600 root"
# Units must never carry a plaintext secret value or an inline cacert/token.
require_not_contains "$gateway_unit" 'SHAREBRIDGE_FRP_PLUGIN_SHARED_SECRET=|SHAREBRIDGE_CONTROL_RELAY_PUBLIC_KEY=|CLOUDFLARE_TOKEN|ACME_' \
  "gateway unit must not carry plaintext secrets or ACME credentials"
require_not_contains "$frps_unit" 'SHAREBRIDGE_FRP_PLUGIN_SHARED_SECRET=|CLOUDFLARE_TOKEN|ACME_' \
  "frps unit must not carry plaintext secrets or ACME credentials"
# No PEM private-key material may be committed in the deployment tree.
for artifact in "$gateway_unit" "$frps_unit" "$firewall" "$install_sh" "$readme" "$ops_doc" "$frps_config"; do
  require_not_contains "$artifact" '-----BEGIN' "no committed private-key material"
done

printf -- '-- A4: read-only filesystem except explicit run/state dirs\n'
for unit in "$gateway_unit" "$frps_unit"; do
  if [[ "$unit" == "$gateway_unit" ]]; then
    hardening_table="$HARDENING_TABLE_GATEWAY"
  else
    hardening_table="$HARDENING_TABLE_FRPS"
  fi
  check_hardening_table "$unit" "$hardening_table"
  require_table_in_vocabulary "$unit" "$hardening_table"
  require_pinned_hardening "$unit" "$(hardening_pinned_keys "$hardening_table")"
  require_no_empty_hardening "$unit"
done
require_not_contains "$gateway_unit" 'CapabilityBoundingSet=[^\n]*CAP_SYS_ADMIN|CapabilityBoundingSet=[^\n]*CAP_NET_ADMIN' \
  "gateway must not hold CAP_SYS_ADMIN/CAP_NET_ADMIN"
require_not_contains "$frps_unit" 'CapabilityBoundingSet=[^\n]*CAP_SYS_ADMIN|CapabilityBoundingSet=[^\n]*CAP_NET_ADMIN' \
  "frps must not hold CAP_SYS_ADMIN/CAP_NET_ADMIN"
# Every writable path is an explicit allowlisted state/run directory. The exact
# ReadWritePaths= set is additionally pinned in the hardening table above; this
# per-path loop names a non-allowlisted addition readably and still fails closed
# if the table is ever loosened.
for unit in "$gateway_unit" "$frps_unit"; do
  writable="$(unit_field "$unit" ReadWritePaths)"
  if [[ -z "$writable" ]]; then
    pass
  else
    for path in $writable; do
      path="${path#-}"
      case "$path" in
        /var/lib/sharebridge-relay-gateway|/run/sharebridge-relay-gateway|/var/lib/sharebridge-relay-frps|/run/sharebridge-relay-frps)
          pass ;;
        *)
          fail "$unit exposes a non-allowlisted writable path: $path" ;;
      esac
    done
  fi
done

printf -- '-- A5: LimitNOFILE and MemoryMax\n'
for unit in "$gateway_unit" "$frps_unit"; do
  require_eq "$(unit_key_count "$unit" LimitNOFILE)" "1" "$unit must assign LimitNOFILE exactly once"
  require_eq "$(unit_key_count "$unit" MemoryMax)" "1" "$unit must assign MemoryMax exactly once"
  nofile="$(unit_field_one "$unit" LimitNOFILE)"
  nofile="${nofile%%:*}"
  if is_positive_int "$nofile" && (( nofile >= 8192 )); then
    pass
  else
    fail "$unit LimitNOFILE must be a positive integer >= 8192 (the §14 global ceiling); got '$nofile'"
  fi
  memmax="$(unit_field_one "$unit" MemoryMax)"
  if [[ "$memmax" =~ ^[0-9]+([KMGTkmgt])?$ ]] || [[ "$memmax" =~ ^[0-9]+%$ ]]; then
    pass
  else
    fail "$unit MemoryMax must be a systemd size; got '$memmax'"
  fi
done

printf -- '-- A6: restart and log-rate policy (exactly one instance each)\n'
for unit in "$gateway_unit" "$frps_unit"; do
  require_eq "$(unit_key_count "$unit" Restart)" "1" "$unit must assign Restart= exactly once"
  require_contains "$unit" '^Restart=(always|on-failure)$' "$unit bounded Restart= policy"
  for key in RestartSec StartLimitIntervalSec StartLimitBurst LogRateLimitIntervalSec LogRateLimitBurst; do
    require_eq "$(unit_key_count "$unit" "$key")" "1" \
      "$unit must assign $key exactly once (a later duplicate/countermanding value must fail)"
    rate_value="$(unit_field_one "$unit" "$key")"
    if is_positive_int "$rate_value"; then
      pass
    else
      fail "$unit $key must be an effective positive integer, not '$rate_value' (0 disables the bound)"
    fi
  done
  restart_sec="$(unit_field_one "$unit" RestartSec)"
  if is_positive_int "$restart_sec" && (( restart_sec <= 30 )); then
    pass
  else
    fail "$unit RestartSec must be a small positive integer (got '$restart_sec')"
  fi
  # The LogRateLimit* pair must actually be in force.
  log_interval="$(unit_field_one "$unit" LogRateLimitIntervalSec)"
  log_burst="$(unit_field_one "$unit" LogRateLimitBurst)"
  if is_positive_int "$log_interval" && is_positive_int "$log_burst"; then
    pass
  else
    fail "$unit must bound its journal write rate (LogRateLimitIntervalSec/Burst > 0)"
  fi
done

printf -- '-- A15: no duplicate directives and exact multi-valued directive sets\n'
repeatable_unit_keys=" Environment LoadCredential "
for unit in "$gateway_unit" "$frps_unit"; do
  unit_duplicates="$(unit_duplicate_keys "$unit")"
  if [[ -z "$unit_duplicates" ]]; then
    pass
  else
    while IFS= read -r duplicate_key; do
      [[ -z "$duplicate_key" ]] && continue
      case "$repeatable_unit_keys" in
        *" ${duplicate_key} "*) pass ;;
        *) fail "$unit assigns ${duplicate_key} more than once; a duplicate directive can silently override a checked value" ;;
      esac
    done <<< "$unit_duplicates"
  fi
  require_eq "$(unit_environment_var_duplicates "$unit" | tr '\n' ' ')" "" \
    "$unit must not assign the same Environment= variable twice"
  require_eq "$(unit_credential_id_duplicates "$unit" | tr '\n' ' ')" "" \
    "$unit must not declare the same LoadCredential= id twice"
done
# A15 is an EXACT-SET check, not merely a duplicate check. The repeatable
# directives in these units are design-fixed, so an added but DISTINCT entry
# (an extra credential id, a different source path, an extra or overridden
# Environment= variable) must fail: a single extra directive can grant the
# service an unasserted host credential or override a checked bind. Both id
# and source are pinned for LoadCredential=; both name and value for
# Environment=. Directives that are not in the repeatable allowlist are already
# rejected above when repeated, so no multi-valued directive is left
# open-ended (ReadWritePaths/ExecStart/etc. are single-valued here, and every
# ReadWritePaths value is separately allowlisted in A4).
exact_gateway_credentials="$(printf '%s\n' \
  'control-ca:/etc/sharebridge/relay/control-ca.crt' \
  'sync-client-cert:/etc/sharebridge/relay/gateway-sync.crt' \
  'sync-client-key:/etc/sharebridge/relay/gateway-sync.key' | sort | tr '\n' '|')"
require_eq "$(unit_field "$gateway_unit" LoadCredential | sort | tr '\n' '|')" \
  "$exact_gateway_credentials" \
  "gateway LoadCredential set must be exactly the three design-fixed id:source credentials"
require_eq "$(unit_key_count "$gateway_unit" LoadCredential)" "3" \
  "gateway must declare exactly 3 LoadCredential= directives"
exact_frps_credentials="$(printf '%s\n' \
  'frps-config:/etc/sharebridge/relay/frps.toml' \
  'transport-cert:/etc/sharebridge/relay/tunnel-server.crt' \
  'transport-key:/etc/sharebridge/relay/tunnel-server.key' | sort | tr '\n' '|')"
require_eq "$(unit_field "$frps_unit" LoadCredential | sort | tr '\n' '|')" \
  "$exact_frps_credentials" \
  "frps LoadCredential set must be exactly the three design-fixed id:source credentials"
require_eq "$(unit_key_count "$frps_unit" LoadCredential)" "3" \
  "frps must declare exactly 3 LoadCredential= directives"
exact_gateway_environment="$(printf '%s\n' \
  'SHAREBRIDGE_CONTROL_SYNC_CA_FILE=%d/control-ca' \
  'SHAREBRIDGE_FRP_PLUGIN_LISTEN_ADDR=127.0.0.1:9001' \
  'SHAREBRIDGE_GATEWAY_LISTEN_ADDR=:443' \
  'SHAREBRIDGE_GATEWAY_METRICS_ADDR=127.0.0.1:9101' \
  'SHAREBRIDGE_GATEWAY_SYNC_CERT_FILE=%d/sync-client-cert' \
  'SHAREBRIDGE_GATEWAY_SYNC_KEY_FILE=%d/sync-client-key' \
  'SHAREBRIDGE_RELAY_PORT_MAX=10099' \
  'SHAREBRIDGE_RELAY_PORT_MIN=10000' | sort | tr '\n' '|')"
require_eq "$(unit_field "$gateway_unit" Environment | sort | tr '\n' '|')" \
  "$exact_gateway_environment" \
  "gateway Environment= set must be exactly the eight design-fixed name=value variables"
require_eq "$(unit_key_count "$gateway_unit" Environment)" "8" \
  "gateway must declare exactly 8 Environment= directives"
require_eq "$(unit_key_count "$frps_unit" Environment)" "0" \
  "frps must declare no unit-level Environment= directives (it uses EnvironmentFile=)"
# The only credential mechanism is the exact LoadCredential= set above. Other
# single-valued credential/environment-injection directives would evade the
# duplicate and exact-set checks (a new key, seen once), so forbid them
# explicitly: an unasserted host credential must not be able to ship.
for unit in "$gateway_unit" "$frps_unit"; do
  require_not_contains "$unit" '^[[:space:]]*(SetCredential|SetCredentialEncrypted|LoadCredentialEncrypted|PassEnvironment)=' \
    "$unit must not use an unasserted credential/environment-injection directive"
done

printf -- '-- A7: frps started after the gateway (restart ordering)\n'
require_contains "$frps_unit" '^After=.*sharebridge-relay-gateway\.service' \
  "frps must order itself after the gateway plugin"
require_contains "$frps_unit" '^Wants=.*sharebridge-relay-gateway\.service' \
  "frps must want the gateway plugin"
require_not_contains "$gateway_unit" 'BindsTo=|PartOf=|Requires=.*frps' \
  "gateway restart must not be coupled to frps lifetime"

printf -- '-- A8: frps config and transport key reach the service via systemd credentials\n'
require_contains "$frps_unit" '^LoadCredential=[^:]*:/etc/sharebridge/relay/frps\.toml' \
  "frps must load frps.toml as a credential"
require_contains "$frps_unit" '^LoadCredential=[^:]*:/etc/sharebridge/relay/tunnel-server\.key' \
  "frps must load the transport key as a credential"
require_contains "$frps_unit" '^LoadCredential=[^:]*:/etc/sharebridge/relay/tunnel-server\.crt' \
  "frps must load the transport certificate as a credential"
require_contains "$frps_unit" 'CREDENTIALS_DIRECTORY' \
  "frps ExecStart must read its config from the credential directory"
require_not_contains "$frps_unit" 'ExecStart=[^\n]*/etc/sharebridge/relay/frps\.toml' \
  "frps must not open the root 0600 config directly"
require_contains "$gateway_unit" '^LoadCredential=[^:]*:/etc/sharebridge/relay/gateway-sync\.key' \
  "gateway must load the sync client key as a credential"
require_contains "$gateway_unit" '^LoadCredential=[^:]*:/etc/sharebridge/relay/gateway-sync\.crt' \
  "gateway must load the sync client certificate as a credential"
require_contains "$gateway_unit" '^LoadCredential=[^:]*:/etc/sharebridge/relay/control-ca\.crt' \
  "gateway must load the control sync CA as a credential"
require_contains "$gateway_unit" 'Environment=SHAREBRIDGE_GATEWAY_SYNC_KEY_FILE=%d/' \
  "gateway sync key path must point at its credential directory"

printf -- '-- A9/A10: firewall public TCP allowlist is exactly 443 + the pinned transport port\n'
input_policy="$(nft_chain "$firewall" input | sed -n 's/.*policy[[:space:]][[:space:]]*\([a-z][a-z]*\).*/\1/p' | head -n 1)"
require_eq "$input_policy" "drop" "nftables input policy must default to drop"
forward_policy="$(nft_chain "$firewall" forward | sed -n 's/.*policy[[:space:]][[:space:]]*\([a-z][a-z]*\).*/\1/p' | head -n 1)"
require_eq "$forward_policy" "drop" "nftables forward policy must default to drop"
transport_port="$(awk -F'=' '/^[[:space:]]*bindPort[[:space:]]*=/ {gsub(/[^0-9]/, "", $2); print $2; exit}' "$frps_config")"
if is_positive_int "$transport_port"; then
  pass
else
  fail "could not read the pinned transport port from $frps_config"
fi

# Parse the WHOLE ruleset, not just the first elements= line: every accept rule
# must be one of the known-safe forms, and the only TCP port rule must
# reference an allowlist set whose elements are exactly 443 + the transport
# port. Any additional public accept (SSH, admin, metrics, proxy/plugin,
# arbitrary) fails here, including one added as a new set element.
public_sets=""
while IFS= read -r raw_accept; do
  norm="$(printf '%s' "$raw_accept" | sed -e 's/^[[:space:]]*//' -e 's/[[:space:]]*$//')"
  [[ -z "$norm" ]] && continue
  case "$norm" in
    'iifname "lo" accept') : ;;
    'ct state established,related accept') : ;;
    'ct state related,established accept') : ;;
    'ip protocol icmp accept') : ;;
    'ip6 nexthdr ipv6-icmp accept') : ;;
    'meta l4proto icmp accept') : ;;
    'meta l4proto ipv6-icmp accept') : ;;
    'tcp dport @'*' accept')
      setname="${norm#tcp dport @}"; setname="${setname% accept}"
      public_sets="${public_sets} ${setname}" ;;
    *)
      fail "unexpected accept rule exposes an unproven public surface: ${norm}" ;;
  esac
done < <(parsed_lines "$firewall" | grep -E 'accept' | grep -Ev 'policy[[:space:]]*accept')

allow_ports=""
if [[ -z "${public_sets// /}" ]]; then
  fail "input chain has no public TCP allowlist rule (tcp dport @<set> accept)"
else
  for setname in $public_sets; do
    set_elements="$(nft_set_elements "$firewall" "$setname")"
    if [[ -z "$set_elements" ]]; then
      fail "public allowlist set '$setname' has no elements"
    fi
    allow_ports="${allow_ports} ${set_elements}"
  done
  allow_sorted="$(printf '%s\n' $allow_ports | sort -n -u | tr '\n' ' ')"
  require_eq "$allow_sorted" "443 ${transport_port} " \
    "public tcp allowlist must be exactly 443 and the pinned transport port ${transport_port}"
fi
require_parsed_contains "$firewall" 'iifname "lo" accept' "loopback traffic must be accepted"
require_parsed_contains "$firewall" 'ct state established,related accept' "established/related traffic must be accepted"

printf -- '-- A10: no public proxy, plugin, metrics, or admin port\n'
# The FRP proxy range comes from the pinned frps config; the plugin, gateway
# metrics, control metrics and the frps dashboard are the private surfaces.
proxy_start="$(grep -E '^[[:space:]]*allowPorts' "$frps_config" | grep -oE '[0-9]+' | head -n 1)"
proxy_end="$(grep -E '^[[:space:]]*allowPorts' "$frps_config" | grep -oE '[0-9]+' | tail -n 1)"
forbidden="9001 9101 9102 7500"
if is_positive_int "$proxy_start"; then
  forbidden="${forbidden} ${proxy_start} ${proxy_end}"
else
  fail "could not read the proxy port range from $frps_config"
fi
# The exact allowlist above already forbids every extra port; assert each
# forbidden surface explicitly too so a regression names the port.
for port in $forbidden; do
  if printf '%s\n' $allow_ports | grep -qx "$port"; then
    fail "forbidden public port ${port} is in the resolved public allowlist"
  else
    pass
  fi
done
# The private plugin/metrics/admin surfaces must be bound to loopback by the
# binaries that own them; assert install.sh never opens them and that config
# pins the loopback binds.
require_parsed_contains "$install_sh" '127\.0\.0\.1:9001|127\.0\.0\.1:9101' \
  "private plugin/metrics listeners must be loopback-only"
require_contains "$gateway_unit" 'SHAREBRIDGE_GATEWAY_METRICS_ADDR=127\.0\.0\.1:9101' \
  "gateway metrics must bind loopback 9101"
require_contains "$gateway_unit" 'SHAREBRIDGE_FRP_PLUGIN_LISTEN_ADDR=127\.0\.0\.1:9001' \
  "FRP plugin must bind loopback 9001"
require_contains "$gateway_unit" 'SHAREBRIDGE_GATEWAY_LISTEN_ADDR=:443' \
  "gateway owns public 443 only"

printf -- '-- A11: dedicated transport certificate only; no content cert or ACME on the relay\n'
require_contains "$install_sh" 'tunnel-server\.crt' "installer provisions the dedicated transport certificate"
require_contains "$install_sh" 'tunnel-server\.key' "installer provisions the dedicated transport key"
require_not_contains "$install_sh" 'fullchain|privkey|ACME_EMAIL|ACME_CA_DIR|CLOUDFLARE_TOKEN' \
  "no ACME/content certificate material on the relay VM"
require_contains "$frps_config" '^transport\.tls\.force = true$' "FRP transport TLS must be forced"
require_contains "$frps_config" '^transport\.tls\.certFile = ' "frps must use the dedicated transport certificate"
require_contains "$frps_config" '^transport\.tls\.keyFile = ' "frps must use the dedicated transport key"

printf -- '-- A12: pinned, checksum-verified frps packaging and cache-independent verification\n'
require_parsed_contains "$install_sh" 'fetch-frp\.sh' "installer must use the committed pinned-FRP fetch path"
require_parsed_contains "$install_sh" 'FRP_MANIFEST=' "installer must resolve the pinned manifest"
require_parsed_contains "$install_sh" 'verify_frps_binary' "installer must verify a caller-supplied frps binary"
require_parsed_contains "$install_sh" 'frps-sha256' "installer must offer an explicit checksum for arbitrary frps binaries"
require_contains "$frps_config" 'auth\.method = "token"' "frps must require authenticated clients"
# The reference digest is the committed manifest pin and never a staged file,
# cache marker or label: fetch-frp.sh prints it without touching the cache.
require_parsed_contains "$fetch_sh" 'frps_sha256' "fetch script must read the manifest-pinned frps digest"
require_parsed_contains "$fetch_sh" '\-\-print-frps-sha256' "fetch script must expose the manifest pin without trusting the cache"
require_parsed_contains "$install_sh" 'print-frps-sha256' "installer must resolve the frps reference from the committed manifest pin"
require_unique_function_def "$install_sh" pinned_frps_digest "the pinned-digest resolver must be defined exactly once"
require_unique_function_def "$install_sh" verify_frps_binary "the frps verifier must be defined exactly once"
require_unique_function_def "$install_sh" install_binary_verified "the verified-copy helper must be defined exactly once"
require_parsed_contains "$fetch_sh" 'verify_staged_binaries' "fetch script must re-verify staged binaries against the pinned tarball"
require_parsed_contains "$fetch_sh" '\-\-print-pins' "fetch script must expose its hermetic manifest pins"
require_parsed_contains "$install_sh" 'install_binary_verified[[:space:]]+"\$FRPS_BINARY"[[:space:]]+"\$\{LIB_DIR\}/frps"' \
  "installer must re-verify the frps binary at its install location after the copy"
require_eq "$(grep -c '"frps_sha256":' "$frp_manifest")" "3" \
  "manifest must pin the frps executable digest for all three platforms"
printed_frps_pin="$(bash "$fetch_sh" --print-frps-sha256 2>/dev/null | tail -n 1)"
if [[ "$printed_frps_pin" =~ ^[0-9a-f]{64}$ ]]; then
  pass
else
  fail "fetch script --print-frps-sha256 must print a 64-hex digest (got '${printed_frps_pin}')"
fi
if [[ -f "$frp_manifest" ]] && grep -q "\"frps_sha256\": \"${printed_frps_pin}\"" "$frp_manifest"; then
  pass
else
  fail "the printed frps digest is not a committed manifest pin"
fi
read -r frp_version_pin platform_key_pin tarball_pin frps_pin <<< "$(bash "$fetch_sh" --print-pins 2>/dev/null | tail -n 1)"
require_eq "$frps_pin" "$printed_frps_pin" "fetch --print-pins must agree with --print-frps-sha256"
if [[ "$tarball_pin" =~ ^[0-9a-f]{64}$ && "$frp_version_pin" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ && "$platform_key_pin" =~ ^[a-z0-9_]+$ ]]; then
  pass
else
  fail "fetch --print-pins must print '<version> <platform> <tarball-sha256> <frps-sha256>' (got '${frp_version_pin} ${platform_key_pin} ${tarball_pin} ${frps_pin}')"
fi

# Behavioural proof that a mismatched binary or an altered checksum FAILS: run
# the installer's own verifier on a throwaway binary with the matching digest
# (must accept) and with a wrong digest (must reject). This is the property a
# prose-only grep could not enforce.
frps_probe_dir="$(mktemp -d)"
dig_shim_dir="$(mktemp -d)"
install_shim_dir="$(mktemp -d)"
trap 'rm -rf "$frps_probe_dir" "$dig_shim_dir" "$install_shim_dir"' EXIT
printf '#!/bin/sh\nexit 0\n' > "${frps_probe_dir}/frps-probe"
chmod 0755 "${frps_probe_dir}/frps-probe"
probe_digest="$(sha256_of "${frps_probe_dir}/frps-probe")"
if bash "$install_sh" --verify-frps "${frps_probe_dir}/frps-probe" --frps-sha256 "$probe_digest" >/dev/null 2>&1; then
  pass
else
  fail "installer rejected a frps binary whose SHA-256 matches its explicit --frps-sha256"
fi
if bash "$install_sh" --verify-frps "${frps_probe_dir}/frps-probe" --frps-sha256 "0000000000000000000000000000000000000000000000000000000000000000" >/dev/null 2>&1; then
  fail "installer accepted a frps binary whose SHA-256 does not match --frps-sha256"
else
  pass
fi
# A poisoned cache can no longer become the reference, and the fetch path can
# no longer trust the `.verified` marker. Both are exercised hermetically: the
# committed pins are read from the manifest, so no network or host cache state
# is involved.
poison_fetch_root="${frps_probe_dir}/poison-fetch-cache"
poison_key_dir="${poison_fetch_root}/v${frp_version_pin}-sha256-${tarball_pin}"
mkdir -p "$poison_key_dir" "${poison_fetch_root}/downloads"
printf '#!/bin/sh\nexit 0\n' > "${poison_key_dir}/frps"
printf '#!/bin/sh\nexit 0\n' > "${poison_key_dir}/frpc"
chmod 0755 "${poison_key_dir}/frps" "${poison_key_dir}/frpc"
printf '%s\n' "$tarball_pin" > "${poison_key_dir}/.verified"
printf 'this is not the pinned tarball\n' > "${poison_fetch_root}/downloads/frp_${frp_version_pin}_${platform_key_pin}.tar.gz"
# The reference must not depend on cache state.
poisoned_pin="$(SHAREBRIDGE_FRP_CACHE="$poison_fetch_root" bash "$fetch_sh" --print-frps-sha256 2>/dev/null | tail -n 1)"
require_eq "$poisoned_pin" "$printed_frps_pin" "the frps reference must not depend on cache state"
# A `.verified` marker plus a poisoned staged binary must not be accepted as a
# cache hit when the tarball cannot vouch for it.
if SHAREBRIDGE_FRP_CACHE="$poison_fetch_root" bash "$fetch_sh" >/dev/null 2>&1; then
  fail "fetch script accepted a poisoned cache marker over a corrupt tarball (must fail closed)"
else
  pass
fi
# Without an explicit checksum the candidate is compared against the committed
# manifest pin, so the poisoned staged binary is rejected.
if SHAREBRIDGE_FRP_CACHE="$poison_fetch_root" bash "$install_sh" --verify-frps "${poison_key_dir}/frps" >/dev/null 2>&1; then
  fail "installer accepted a poisoned cache marker plus poisoned frps as the pinned binary"
else
  pass
fi
# A caller-supplied --frps-binary must carry an explicit checksum. Both probes
# pass a fully valid dry-run so the only difference is the missing checksum.
# The dry-run credential values are format-valid but deliberately all-zero
# placeholders (never real secrets); they are kept in variables so a
# NAME=<hex> secret scan cannot mistake the test fixture for a committed key.
probe_plugin_secret="0000000000000000"
probe_control_pubkey="0000000000000000000000000000000000000000000000000000000000000000"
relay_probe_args=(
  --dry-run --tunnel-host relay-tunnel.example.test --namespace sb0123abcd
  --sync-url https://control.example.test --sync-san control.example.test
  --sync-ca /tmp/sb-ca --sync-cert /tmp/sb-cert --sync-key /tmp/sb-key
  --transport-cert /tmp/sb-transport-crt --transport-key /tmp/sb-transport-key
)
if env SHAREBRIDGE_FRP_PLUGIN_SHARED_SECRET="$probe_plugin_secret" \
      SHAREBRIDGE_CONTROL_RELAY_PUBLIC_KEY="$probe_control_pubkey" \
      bash "$install_sh" "${relay_probe_args[@]}" --frps-binary "${frps_probe_dir}/frps-probe" >/dev/null 2>&1; then
  fail "installer accepted --frps-binary without an explicit --frps-sha256"
else
  pass
fi
if env SHAREBRIDGE_FRP_PLUGIN_SHARED_SECRET="$probe_plugin_secret" \
      SHAREBRIDGE_CONTROL_RELAY_PUBLIC_KEY="$probe_control_pubkey" \
      bash "$install_sh" "${relay_probe_args[@]}" --frps-binary "${frps_probe_dir}/frps-probe" --frps-sha256 "$probe_digest" >/dev/null 2>&1; then
  pass
else
  fail "installer rejected --frps-binary with a matching explicit --frps-sha256"
fi
# Post-copy verification: the installed artifact is what runs, so a copy that
# diverges from the verified source must fail closed.
copy_dst="${frps_probe_dir}/installed-frps"
if bash "$install_sh" --copy-frps "${frps_probe_dir}/frps-probe" --frps-dest "$copy_dst" --frps-sha256 "$probe_digest" >/dev/null 2>&1 \
   && [[ "$(sha256_of "$copy_dst")" == "$probe_digest" ]]; then
  pass
else
  fail "installer rejected (or mis-copied) a faithful copy of a verified frps binary"
fi
real_install_path="$(command -v install)"
cat > "${install_shim_dir}/install" <<SHIM
#!/bin/sh
last=""
for last_arg in "\$@"; do last="\$last_arg"; done
"${real_install_path}" "\$@" || exit \$?
printf 'tampered' >> "\$last"
SHIM
chmod 0755 "${install_shim_dir}/install"
if PATH="${install_shim_dir}:${PATH}" bash "$install_sh" --copy-frps "${frps_probe_dir}/frps-probe" --frps-dest "${frps_probe_dir}/tamper-dst" --frps-sha256 "$probe_digest" >/dev/null 2>&1; then
  fail "installer accepted a tampered post-copy frps artifact at the install location"
else
  pass
fi

printf -- '-- A13: DNS audit proves wildcard synthesis and per-name HTTPS/SVCB/ECH absence\n'
for audit_file in "$install_sh" "$ops_doc"; do
  require_contains "$audit_file" 'dig ' "DNS audit uses dig ($audit_file)"
  require_contains "$audit_file" '\bHTTPS\b' "DNS audit checks the HTTPS RR type ($audit_file)"
  require_contains "$audit_file" '\bSVCB\b' "DNS audit checks the SVCB RR type ($audit_file)"
  require_contains "$audit_file" '\bAAAA\b' "DNS audit validates AAAA ($audit_file)"
  require_contains "$audit_file" 'relay\.' "DNS audit names the relay wildcard ($audit_file)"
  require_contains "$audit_file" 'tunnel' "DNS audit names the tunnel host ($audit_file)"
done
# Wildcard synthesis is only proven by querying a random child label; a literal
# `*` query alone would pass even if browsers resolving arbitrary labels got
# nothing.
require_parsed_contains "$install_sh" 'dig[[:space:]][[:space:]]*\+short[[:space:]][[:space:]]*A[[:space:]][[:space:]]*"\$\{probe_label\}"' \
  "installer audit must resolve A for a random relay child label (wildcard synthesis)"
require_contains "$ops_doc" 'probe-' "runbook must show the random-child-label query"
# AAAA answers are validated with install.sh's strict explicit IPv6 parser
# (not a hex-and-colon pattern that accepts `::::` or overlapping `::` such as
# `:::`), so every malformed answer fails the audit closed.
require_unique_function_def "$install_sh" is_ipv6_literal "the IPv6 validator must be defined exactly once"
require_parsed_contains "$install_sh" 'is_ipv6_literal[[:space:]]+"\$ipv6"' \
  "the DNS audit must validate every AAAA answer with is_ipv6_literal"
# The claim must be scoped to the queried names: a zone-wide absence claim is
# not provable by these queries and must not be published in ANY document.
require_not_contains "$ops_doc" 'no HTTPS/SVCB records at all' "runbook must not overclaim zone-wide HTTPS/SVCB absence"
require_not_contains "$ops_doc" 'zone publishes no HTTPS/SVCB' "runbook must not claim the zone publishes no HTTPS/SVCB records"
require_contains "$ops_doc" 'AXFR|zone transfer|zone-wide' "runbook must state the DNS-audit scope honestly"
require_contains "$ops_doc" '[Ee][Cc][Hh]' "runbook must call out the ECH invariant"
require_not_contains "$plan_doc" 'zone publishes no HTTPS/SVCB' "plan must not overclaim zone-wide HTTPS/SVCB/ECH absence"
require_contains "$plan_doc" 'no HTTPS/SVCB/ECH record' "plan must state the narrowed per-name HTTPS/SVCB/ECH claim"
require_contains "$plan_doc" 'not a zone-wide' "plan must state the DNS-audit scope honestly"

# Behavioural proof of the AAAA validator: a fake `dig` controls the answers, so
# a garbage AAAA must make the audit fail closed and a real IPv6 literal must
# pass.
cat > "${dig_shim_dir}/dig" <<'SHIM'
#!/bin/sh
for arg in "$@"; do
  if [ "$arg" = "AAAA" ]; then
    printf '%s\n' "${FAKE_AAAA:-}"
    exit 0
  fi
done
for arg in "$@"; do
  if [ "$arg" = "A" ]; then
    printf '203.0.113.10\n'
    exit 0
  fi
done
exit 0
SHIM
chmod 0755 "${dig_shim_dir}/dig"
run_fake_dns_audit() {
  PATH="${dig_shim_dir}:${PATH}" FAKE_AAAA="$1" bash "$install_sh" --audit-dns \
    --namespace sb0123abcd --tunnel-host relay-tunnel.example.test >/dev/null 2>&1
}
if run_fake_dns_audit "2001:db8::1"; then
  pass
else
  fail "DNS audit rejected a valid IPv6 AAAA literal"
fi
# Valid forms that must be accepted: `::` as the whole address, `::` at either
# edge, a full uncompressed address, and uppercase hex.
for good_aaaa in "::" "::1" "1::" "1:2:3:4:5:6:7:8" "FE80::1" "2001:DB8:0:0:0:0:0:1"; do
  if run_fake_dns_audit "$good_aaaa"; then
    pass
  else
    fail "DNS audit rejected the valid IPv6 answer '${good_aaaa}'"
  fi
done
# Malformed forms that must fail closed. The `:::` family is the overlapping-`::`
# class the old `%%::`/`##::` split wrongly accepted; the rest cover a
# leading/trailing single colon, duplicate compression, wrong group counts and
# non-hex junk.
for bad_aaaa in \
  "::::" \
  ":::" \
  "1:::2" \
  ":::1" \
  "1:::" \
  "1::2::3" \
  "::1:2:3:4:5:6:7:8" \
  "1:2:3:4:5:6:7:8::" \
  "12345::" \
  "1:2:3:4:5:6:7" \
  "1:2:3:4:5:6:7:8:9" \
  "2001:db8::1%eth0" \
  "1:2:3" \
  "::ffff:192.0.2.1" \
  "not-an-ip" \
  " 2001:db8::1" \
  "2001:db8::1 "; do
  if run_fake_dns_audit "$bad_aaaa"; then
    fail "DNS audit accepted the invalid IPv6 answer '${bad_aaaa}'"
  else
    pass
  fi
done

printf -- '-- A14: operator runbook carries the deferred operator surface\n'
for name in \
  SHAREBRIDGE_GATEWAY_MAX_STREAMS_PER_SOURCE_IP \
  SHAREBRIDGE_GATEWAY_MAX_STREAMS_PER_ORIGIN \
  SHAREBRIDGE_GATEWAY_MAX_STREAMS_PER_AGENT \
  SHAREBRIDGE_GATEWAY_MAX_STREAMS_GLOBAL \
  SHAREBRIDGE_GATEWAY_MAX_HELLO_BYTES \
  SHAREBRIDGE_GATEWAY_HELLO_TIMEOUT \
  SHAREBRIDGE_GATEWAY_DIAL_TIMEOUT \
  SHAREBRIDGE_GATEWAY_IDLE_TIMEOUT \
  SHAREBRIDGE_GATEWAY_ABSOLUTE_LIFETIME \
  SHAREBRIDGE_GATEWAY_MAX_TRACKED_AGENTS \
  SHAREBRIDGE_GATEWAY_METRICS_ADDR \
  CONTROL_METRICS_ADDR \
  SHAREBRIDGE_CONTROL_SYNC_URL \
  SHAREBRIDGE_CONTROL_SYNC_SAN \
  SHAREBRIDGE_CONTROL_SYNC_CA_FILE \
  SHAREBRIDGE_GATEWAY_SYNC_CERT_FILE \
  SHAREBRIDGE_GATEWAY_SYNC_KEY_FILE \
  SHAREBRIDGE_GATEWAY_NAMESPACE \
  SHAREBRIDGE_GATEWAY_NIC_INTERFACE \
  SHAREBRIDGE_GATEWAY_NIC_CAPACITY_BYTES_PER_SEC \
  DefaultFRPSFreshnessWindow; do
  require_contains "$ops_doc" "$name" "runbook documents $name"
done
require_contains "$ops_doc" '30[[:space:]]*(s|seconds)|30-second|30s' "runbook states the 30-second frps freshness window"
require_contains "$ops_doc" '[Pp]resence-transport' "runbook records the open presence-transport gap"
require_contains "$readme" 'deploy/install\.sh' "relay README points at the deployment installer"

printf '\n== %d checks, %d failure(s) ==\n' "$checks" "$failures"
if [[ $failures -gt 0 ]]; then
  printf 'RESULT: RED\n'
  exit 1
fi
printf 'RESULT: GREEN — relay deployment hardening verified\n'
exit 0
