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
# The assertion IDs (A1…A14) are referenced by the Task 35 report so each
# published hardening claim maps to the check that enforces it.

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

# is_positive_int <value>
is_positive_int() {
  [[ "$1" =~ ^[0-9]+$ ]] && (( $1 > 0 ))
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

# Nothing below can be meaningful when the units are absent (the RED state).
if [[ ! -f "$gateway_unit" || ! -f "$frps_unit" || ! -f "$firewall" || ! -f "$install_sh" ]]; then
  printf '\n== %d checks, %d failure(s) ==\n' "$checks" "$failures"
  printf 'RESULT: RED — deployment artifacts are incomplete\n'
  exit 1
fi

printf -- '-- A2: distinct unprivileged service users\n'
gateway_user="$(unit_field_one "$gateway_unit" User)"
frps_user="$(unit_field_one "$frps_unit" User)"
if [[ -z "$gateway_user" ]]; then fail "gateway unit has no User="; else pass; fi
if [[ -z "$frps_user" ]]; then fail "frps unit has no User="; else pass; fi
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
# install.sh creates both accounts as system (loginless) users.
require_contains "$install_sh" 'useradd[^#]*--system|--system[[:space:]]' "installer must create system accounts"
require_contains "$install_sh" "$gateway_user" "installer must create the gateway user"
require_contains "$install_sh" "$frps_user" "installer must create the frps user"

printf -- '-- A3: root-owned 0600 secrets and root config\n'
require_contains "$install_sh" 'install[[:space:]]+-o[[:space:]]+root[[:space:]]+-g[[:space:]]+root[[:space:]]+-m[[:space:]]+0?600' \
  "installer must install secrets 0600 root:root"
for secret in 'frps\.toml' 'gateway\.env' 'frps\.env' 'tunnel-server\.key' 'gateway-sync\.key' 'control-ca\.crt'; do
  require_contains "$install_sh" "install_root_secret[^\n]*${secret}" \
    "installer must install ${secret} as a 0600 root secret"
done
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
  require_eq "$(unit_field_one "$unit" ProtectSystem)" "strict" "ProtectSystem=strict in $unit"
  require_eq "$(unit_field_one "$unit" NoNewPrivileges)" "true" "NoNewPrivileges=true in $unit"
  require_eq "$(unit_field_one "$unit" PrivateTmp)" "true" "PrivateTmp=true in $unit"
  require_eq "$(unit_field_one "$unit" ProtectHome)" "true" "ProtectHome=true in $unit"
  require_contains "$unit" '^ProtectKernelTunables=true$' "ProtectKernelTunables in $unit"
  require_contains "$unit" '^ProtectKernelModules=true$' "ProtectKernelModules in $unit"
  require_contains "$unit" '^ProtectControlGroups=true$' "ProtectControlGroups in $unit"
  require_contains "$unit" '^RestrictAddressFamilies=' "RestrictAddressFamilies in $unit"
  require_contains "$unit" '^RestrictNamespaces=true$' "RestrictNamespaces in $unit"
  require_contains "$unit" '^LockPersonality=true$' "LockPersonality in $unit"
  require_contains "$unit" '^MemoryDenyWriteExecute=true$' "MemoryDenyWriteExecute in $unit"
  require_contains "$unit" '^SystemCallFilter=' "SystemCallFilter in $unit"
  require_contains "$unit" '^CapabilityBoundingSet=' "CapabilityBoundingSet in $unit"
done
require_not_contains "$gateway_unit" 'CapabilityBoundingSet=[^\n]*CAP_SYS_ADMIN|CapabilityBoundingSet=[^\n]*CAP_NET_ADMIN' \
  "gateway must not hold CAP_SYS_ADMIN/CAP_NET_ADMIN"
require_not_contains "$frps_unit" 'CapabilityBoundingSet=[^\n]*CAP_SYS_ADMIN|CapabilityBoundingSet=[^\n]*CAP_NET_ADMIN' \
  "frps must not hold CAP_SYS_ADMIN/CAP_NET_ADMIN"
# The gateway owns public 443; it gets exactly one capability for it.
require_contains "$gateway_unit" '^AmbientCapabilities=CAP_NET_BIND_SERVICE$' \
  "gateway needs CAP_NET_BIND_SERVICE for :443"
require_contains "$gateway_unit" '^CapabilityBoundingSet=CAP_NET_BIND_SERVICE$' \
  "gateway capability set is exactly CAP_NET_BIND_SERVICE"
# Every writable path is an explicit allowlisted state/run directory.
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
  require_contains "$unit" '^(StateDirectory|RuntimeDirectory)=sharebridge-relay-' \
    "$unit must declare an explicit state/run directory"
done

printf -- '-- A5: LimitNOFILE and MemoryMax\n'
for unit in "$gateway_unit" "$frps_unit"; do
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

printf -- '-- A6: restart and log-rate policy\n'
for unit in "$gateway_unit" "$frps_unit"; do
  require_contains "$unit" '^Restart=(always|on-failure)$' "$unit bounded Restart= policy"
  restart_sec="$(unit_field_one "$unit" RestartSec)"
  if is_positive_int "$restart_sec" && (( restart_sec <= 30 )); then
    pass
  else
    fail "$unit RestartSec must be a small positive integer (got '$restart_sec')"
  fi
  require_contains "$unit" '^StartLimitIntervalSec=[0-9]+' "$unit restart-storm interval"
  require_contains "$unit" '^StartLimitBurst=[0-9]+' "$unit restart-storm burst"
  require_contains "$unit" '^LogRateLimitIntervalSec=[0-9]+' "$unit bound log rate"
  require_contains "$unit" '^LogRateLimitBurst=[0-9]+' "$unit bound log burst"
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

printf -- '-- A9: firewall allowlist is exactly 443/tcp + the pinned transport port\n'
require_contains "$firewall" 'type filter hook input priority [^;]*;[[:space:]]*policy drop' \
  "nftables input policy must default to drop"
transport_port="$(awk -F'=' '/^[[:space:]]*bindPort[[:space:]]*=/ {gsub(/[^0-9]/, "", $2); print $2; exit}' "$frps_config")"
if is_positive_int "$transport_port"; then
  pass
else
  fail "could not read the pinned transport port from $frps_config"
fi
allowlist="$(grep -E '^[[:space:]]*elements[[:space:]]*=' "$firewall" | head -n 1 | sed 's/.*{//; s/}.*//' | tr ',' '\n' | tr -d ' ' | sort -n | tr '\n' ' ')"
require_eq "$allowlist" "443 ${transport_port} " \
  "public tcp allowlist must be exactly 443 and the pinned transport port"
require_contains "$firewall" 'iifname "lo" accept' "loopback traffic must be accepted"

printf -- '-- A10: no public proxy, plugin, metrics, or admin ports\n'
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
# No accept line anywhere may expose a forbidden dport literal.
forbidden_hit=0
while IFS= read -r accept_line; do
  [[ -z "$accept_line" ]] && continue
  for port in $forbidden; do
    if printf '%s' "$accept_line" | grep -Eq "(^|[^0-9])${port}([^0-9]|$)"; then
      fail "forbidden public port ${port} appears in an accept rule: ${accept_line}"
      forbidden_hit=1
    fi
  done
done < <(grep -E 'accept' "$firewall")
if [[ $forbidden_hit -eq 0 ]]; then pass; fi
# The private plugin/metrics/admin surfaces must be bound to loopback by the
# binaries that own them; assert install.sh never opens them and that config
# pins the loopback binds.
require_contains "$install_sh" '127\.0\.0\.1:9001|127\.0\.0\.1:9101' \
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

printf -- '-- A12: pinned, checksum-verified frps packaging\n'
require_contains "$install_sh" 'fetch-frp\.sh' "installer must use the committed pinned-FRP fetch path"
require_contains "$install_sh" 'frp/manifest\.json|manifest' "installer must resolve the pinned manifest"
require_contains "$install_sh" 'sha256|SHA-256|checksum' "installer must verify the pinned checksum"
require_contains "$frps_config" 'auth\.method = "token"' "frps must require authenticated clients"

printf -- '-- A13: DNS audit commands prove DNS-only and no HTTPS/SVCB/ECH records\n'
for audit_file in "$install_sh" "$ops_doc"; do
  require_contains "$audit_file" 'dig ' "DNS audit uses dig ($audit_file)"
  require_contains "$audit_file" '\bHTTPS\b' "DNS audit checks the HTTPS RR type ($audit_file)"
  require_contains "$audit_file" '\bSVCB\b' "DNS audit checks the SVCB RR type ($audit_file)"
  require_contains "$audit_file" 'relay\.' "DNS audit names the relay wildcard ($audit_file)"
  require_contains "$audit_file" 'tunnel' "DNS audit names the tunnel host ($audit_file)"
done
require_contains "$ops_doc" '[Ee][Cc][Hh]' "runbook must call out the ECH invariant"

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
