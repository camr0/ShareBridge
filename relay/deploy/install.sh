#!/usr/bin/env bash
#
# install.sh — provision the ShareBridge phase-4a relay VM (spec §4.6, §6,
# §17.1; plan Task 35).
#
# The relay VM runs two hardened systemd services as two distinct unprivileged
# users:
#
#   sharebridge-relay-gateway.service  public :443/tcp (SNI L4 gateway)
#                                      loopback :9001 (FRP plugin)
#                                      loopback :9101 (private metrics/health)
#   sharebridge-relay-frps.service     public :7000/tcp (pinned FRP transport)
#                                      loopback :10000-10099 (agent proxies)
#
# Only 443/tcp and the transport port are firewalled public. The FRP proxy
# range, the authorization plugin, metrics/health and any FRP dashboard stay
# private/loopback (spec §17.1).
#
# Secrets: this script never echoes a secret. Secret material is read from the
# environment (or from a root-only file) and installed as root:root mode 0600;
# the services read it through systemd credentials, never from a world- or
# group-readable path. No content certificate/key and no ACME/DNS credential
# is installed on the relay VM — the relay holds only the dedicated FRP
# transport certificate and the gateway control-sync client certificate.
#
# Usage:
#   install.sh --tunnel-host <fqdn> --sync-url <https://…> --sync-san <fqdn> \
#              --namespace <sbXXXXXXXX> --sync-ca <file> --sync-cert <file> \
#              --sync-key <file> --transport-cert <file> --transport-key <file> \
#              [--gateway-binary <file>] [--frps-binary <file>] \
#              [--frps-sha256 <64-hex>] [--dry-run]
#   install.sh --generate-transport-cert <fqdn> [--transport-out <prefix>]
#   install.sh --audit-dns --namespace <sbXXXXXXXX> --tunnel-host <fqdn> \
#              [--expect-ipv4 <ipv4>] [--expect-ipv6 <ipv6>]
#   install.sh --verify-frps <file> [--frps-sha256 <64-hex>]
#   install.sh --help
#
# A caller-supplied --frps-binary is integrity-checked before it is installed:
# its SHA-256 must equal --frps-sha256 when given, otherwise the SHA-256 of the
# manifest-pinned frps (relay/frp/manifest.json, resolved through the
# checksum-verifying relay/scripts/fetch-frp.sh). A mismatch or a missing pin
# fails closed.
#
# The DNS audit proves wildcard synthesis for the relay family and the absence
# of HTTPS/SVCB/ECH records for the queried relay names; it is not an
# authoritative zone-transfer proof (see the audit_dns comment).
#
# Required environment (never echoed):
#   SHAREBRIDGE_FRP_PLUGIN_SHARED_SECRET   hex secret shared by frps and gateway
#   SHAREBRIDGE_CONTROL_RELAY_PUBLIC_KEY   64-hex Ed25519 control signing key
#
# Exit: 0 on success, 1 on any configuration or verification failure.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"
FRP_MANIFEST="${REPO_ROOT}/relay/frp/manifest.json"
FRP_FETCH="${REPO_ROOT}/relay/scripts/fetch-frp.sh"
FRPS_TEMPLATE="${REPO_ROOT}/relay/config/frps.toml"
DEPLOY_DIR="${SCRIPT_DIR}"

CONFIG_DIR="/etc/sharebridge/relay"
LIB_DIR="/usr/local/lib/sharebridge/relay"
UNIT_DIR="/etc/systemd/system"
GATEWAY_USER="sharebridge-relay-gateway"
FRPS_USER="sharebridge-relay-frps"
DEFAULT_TRANSPORT_PORT=7000

MODE="install"
TUNNEL_HOST=""
SYNC_URL=""
SYNC_SAN=""
NAMESPACE=""
SYNC_CA=""
SYNC_CERT=""
SYNC_KEY=""
TRANSPORT_CERT=""
TRANSPORT_KEY=""
TRANSPORT_OUT="/tmp/sharebridge-relay-transport"
GATEWAY_BINARY=""
FRPS_BINARY=""
FRPS_SHA256=""
VERIFY_FRPS=""
EXPECT_IPV4=""
EXPECT_IPV6=""
FRP_STAGE=""
DRY_RUN=0

log() { printf '%s\n' "$*" >&2; }
fail() { log "ERROR: $*"; exit 1; }

usage() {
  awk 'NR>1 { if ($0 ~ /^#/) { sub(/^# ?/, ""); print } else if ($0 !~ /^$/) { exit } }' "${BASH_SOURCE[0]}"
}

# install_root_secret <src> <dst> — install a secret/config file root:root
# mode 0600. systemd reads EnvironmentFiles as root; process-readable configs
# (frps.toml, the transport key) are exposed to the service through
# LoadCredential= so the on-disk file never needs group/world access.
install_root_secret() {
  install -o root -g root -m 0600 -- "$1" "$2"
}

create_service_user() {
  local name="$1"
  if getent passwd "$name" >/dev/null 2>&1; then
    log "user ${name} already exists"
    return 0
  fi
  useradd --system --no-create-home --home-dir /nonexistent --shell /usr/sbin/nologin "$name"
  log "created system user ${name}"
}

# sha256_of <file> — print the lowercase-hex SHA-256 of a file.
sha256_of() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | awk '{print $1}'
  elif command -v shasum >/dev/null 2>&1; then
    shasum -a 256 "$1" | awk '{print $1}'
  else
    fail "neither sha256sum nor shasum is available to verify the frps binary"
  fi
}

# pinned_frps_digest — SHA-256 of the manifest-pinned frps executable. The
# manifest records the release tarball digest, so the pinned frps is resolved
# through fetch-frp.sh, which verifies the tarball against relay/frp/
# manifest.json and fails closed on a mismatch or a missing entry; the
# extracted executable's own digest is then the reference a caller-supplied
# binary must match.
pinned_frps_digest() {
  local stage="${FRP_STAGE:-}"
  if [[ -z "$stage" ]]; then
    stage="$("$FRP_FETCH")" || fail "could not stage the manifest-pinned frps (pin missing or verification failed)"
  fi
  [[ -f "${stage}/frps" ]] || fail "manifest-pinned frps is missing at ${stage}/frps"
  sha256_of "${stage}/frps"
}

# verify_frps_binary <path> — fail closed unless <path> is the pinned frps.
# An explicit --frps-sha256 wins (for a deliberate arbitrary build); otherwise
# the manifest-pinned digest is the reference.
verify_frps_binary() {
  local candidate="$1" expected actual
  [[ -f "$candidate" ]] || fail "frps binary not found: $candidate"
  if [[ -n "$FRPS_SHA256" ]]; then
    [[ "$FRPS_SHA256" =~ ^[0-9a-f]{64}$ ]] \
      || fail "--frps-sha256 must be 64 lowercase hex characters"
    expected="$FRPS_SHA256"
  else
    expected="$(pinned_frps_digest)"
  fi
  actual="$(sha256_of "$candidate")"
  [[ "$actual" == "$expected" ]] \
    || fail "refusing frps binary ${candidate}: SHA-256 ${actual} does not match the pinned ${expected}"
  log "verified frps binary ${candidate} (sha256 ${actual})"
}

# render_frps_config <dst> — render the committed pinned frps template. The
# plugin secret stays a {{ .Envs.… }} reference supplied by frps.env (the
# template file never contains it); the transport certificate/key paths are
# repointed at the systemd credential directory.
render_frps_config() {
  local dst="$1"
  sed \
    -e 's#^transport\.tls\.certFile = ".*"#transport.tls.certFile = "{{ .Envs.CREDENTIALS_DIRECTORY }}/transport-cert"#' \
    -e 's#^transport\.tls\.keyFile = ".*"#transport.tls.keyFile = "{{ .Envs.CREDENTIALS_DIRECTORY }}/transport-key"#' \
    "$FRPS_TEMPLATE" >"$dst"
  grep -q 'CREDENTIALS_DIRECTORY' "$dst" || fail "failed to render transport credential paths into $dst"
}

# generate_transport_cert <fqdn> <out-prefix> — create the dedicated
# self-signed FRP transport certificate/key used to authenticate the tunnel
# host. The certificate is copied to agents as their SHAREBRIDGE_RELAY_CA_FILE.
generate_transport_cert() {
  local host="$1" out="$2"
  command -v openssl >/dev/null 2>&1 || fail "openssl is required to generate the transport certificate"
  [[ "$host" =~ ^[A-Za-z0-9]([A-Za-z0-9.-]*[A-Za-z0-9])?$ ]] || fail "--generate-transport-cert needs an RFC 1123 hostname"
  install -d -m 0700 "$(dirname "$out")"
  openssl ecparam -genkey -name prime256v1 -noout -out "${out}.key"
  openssl req -x509 -new -key "${out}.key" -sha256 -days 825 \
    -subj "/CN=${host}" \
    -addext "subjectAltName=DNS:${host}" \
    -addext "extendedKeyUsage=serverAuth" \
    -addext "keyUsage=digitalSignature,keyEncipherment" \
    -out "${out}.crt"
  chmod 0600 "${out}.key"
  chmod 0644 "${out}.crt"
  log "generated dedicated transport certificate for ${host}: ${out}.crt / ${out}.key"
  log "copy ${out}.crt to each agent as SHAREBRIDGE_RELAY_CA_FILE before enrolling"
}

# audit_dns <namespace> <tunnel-host> [expected-ipv4] [expected-ipv6] — check
# the §6 DNS invariant for the relay names: they must be DNS-only A/AAAA names
# with no HTTPS/SVCB record, because an HTTPS/SVCB record (in particular one
# carrying an ech= parameter) would publish an ECH key that hides the SNI from
# the gateway and the agent Binder and breaks exact routing.
#
# The audit proves, for the names it queries: (a) the relay wildcard
# synthesizes a record for a random child label (a literal `*` query alone does
# not prove that browsers resolving arbitrary labels reach the relay), and
# (b) neither the relay wildcard family nor the tunnel host publishes an
# HTTPS/SVCB/ECH record. It does not transfer or dump the zone, so it does not
# prove zone-wide absence for names it did not query.
audit_dns() {
  local namespace="$1" tunnel_host="$2" expected_ipv4="${3:-}" expected_ipv6="${4:-}"
  local zone="sharebridgeusercontent.com"
  local relay_wildcard="*.relay.${namespace}.${zone}"
  local relay_family="relay.${namespace}.${zone}"
  local probe_label="probe-$(date +%s)-$$-${RANDOM}.${relay_family}"
  local status=0

  log "DNS audit: ${relay_wildcard} family and ${tunnel_host}"
  log "expected relay IPv4: ${expected_ipv4:-<not given>}; expected IPv6: ${expected_ipv6:-<not given>}"

  # 1. Wildcard synthesis: a random child label (what a browser actually
  #    resolves) must exist. A bare `*` query alone would not prove this.
  local probe_a
  probe_a="$(dig +short A "${probe_label}" 2>/dev/null || true)"
  log "A ${probe_label} -> ${probe_a:-<none>}"
  [[ -n "$probe_a" ]] \
    || { log "FAIL: random relay child label has no A record (wildcard synthesis unproven)"; status=1; }
  if [[ -n "$expected_ipv4" && "$probe_a" != *"$expected_ipv4"* ]]; then
    log "FAIL: random relay child label does not resolve to the expected IPv4 ${expected_ipv4}"; status=1
  fi

  # 2. The wildcard and tunnel host resolve as DNS-only A/AAAA records.
  local relay_a relay_aaaa tunnel_a tunnel_aaaa
  relay_a="$(dig +short A "${relay_wildcard}" 2>/dev/null || true)"
  relay_aaaa="$(dig +short AAAA "${relay_wildcard}" 2>/dev/null || true)"
  tunnel_a="$(dig +short A "${tunnel_host}" 2>/dev/null || true)"
  tunnel_aaaa="$(dig +short AAAA "${tunnel_host}" 2>/dev/null || true)"
  log "A    ${relay_wildcard} -> ${relay_a:-<none>}"
  log "AAAA ${relay_wildcard} -> ${relay_aaaa:-<none>}"
  log "A    ${tunnel_host} -> ${tunnel_a:-<none>}"
  log "AAAA ${tunnel_host} -> ${tunnel_aaaa:-<none>}"
  [[ -n "$relay_a" ]] || { log "FAIL: relay wildcard has no A record"; status=1; }
  [[ -n "$tunnel_a" ]] || { log "FAIL: tunnel host has no A record"; status=1; }
  if [[ -n "$expected_ipv4" && "$relay_a" != *"$expected_ipv4"* ]]; then
    log "FAIL: relay wildcard does not resolve to the expected IPv4 ${expected_ipv4}"; status=1
  fi
  # Every AAAA answer that is present must be a real IPv6 literal, and must
  # match --expect-ipv6 when supplied.
  local addr label value ipv6
  for addr in "${relay_wildcard}:${relay_aaaa}" "${tunnel_host}:${tunnel_aaaa}"; do
    label="${addr%%:*}"; value="${addr#*:}"
    while IFS= read -r ipv6; do
      [[ -z "$ipv6" ]] && continue
      if [[ ! "$ipv6" =~ ^[0-9A-Fa-f:]+$ || "$ipv6" != *:* ]]; then
        log "FAIL: ${label} AAAA answer '${ipv6}' is not an IPv6 literal"; status=1
      fi
      if [[ -n "$expected_ipv6" && "$ipv6" != "$expected_ipv6" ]]; then
        log "FAIL: ${label} AAAA '${ipv6}' does not match the expected IPv6 ${expected_ipv6}"; status=1
      fi
    done <<< "$value"
  done

  # 3. No HTTPS/SVCB (ECH-capable) record for the wildcard family, a random
  #    child label, or the tunnel host.
  local https_relay https_tunnel https_probe svcb_relay svcb_tunnel svcb_probe ech_probe
  https_relay="$(dig +short HTTPS "${relay_wildcard}" 2>/dev/null || true)"
  https_tunnel="$(dig +short HTTPS "${tunnel_host}" 2>/dev/null || true)"
  https_probe="$(dig +short HTTPS "${probe_label}" 2>/dev/null || true)"
  svcb_relay="$(dig +short SVCB "${relay_wildcard}" 2>/dev/null || true)"
  svcb_tunnel="$(dig +short SVCB "${tunnel_host}" 2>/dev/null || true)"
  svcb_probe="$(dig +short SVCB "${probe_label}" 2>/dev/null || true)"
  ech_probe="$(dig +short HTTPS "${relay_wildcard}" "${tunnel_host}" "${probe_label}" 2>/dev/null | grep -i 'ech=' || true)"
  if [[ -n "$https_relay" || -n "$https_tunnel" || -n "$https_probe" || \
        -n "$svcb_relay" || -n "$svcb_tunnel" || -n "$svcb_probe" || -n "$ech_probe" ]]; then
    log "FAIL: an HTTPS/SVCB (ECH-capable) record exists for a queried relay name"
    log "  HTTPS ${relay_wildcard} -> ${https_relay:-<none>}"
    log "  HTTPS ${tunnel_host} -> ${https_tunnel:-<none>}"
    log "  HTTPS ${probe_label} -> ${https_probe:-<none>}"
    log "  SVCB  ${relay_wildcard} -> ${svcb_relay:-<none>}"
    log "  SVCB  ${tunnel_host} -> ${svcb_tunnel:-<none>}"
    log "  SVCB  ${probe_label} -> ${svcb_probe:-<none>}"
    status=1
  else
    log "PASS: no HTTPS/SVCB/ECH records for the relay wildcard family (incl. a random child label) or the tunnel host"
  fi

  # Diagnostic context only; CT/CAA remains a separate release prerequisite.
  log "CAA ${zone}: $(dig +short CAA "${zone}" 2>/dev/null | tr '\n' ' ' || true)"
  return "$status"
}

# ---- argument parsing --------------------------------------------------------
while [[ $# -gt 0 ]]; do
  case "$1" in
    --tunnel-host) TUNNEL_HOST="${2:?}"; shift 2 ;;
    --sync-url) SYNC_URL="${2:?}"; shift 2 ;;
    --sync-san) SYNC_SAN="${2:?}"; shift 2 ;;
    --namespace) NAMESPACE="${2:?}"; shift 2 ;;
    --sync-ca) SYNC_CA="${2:?}"; shift 2 ;;
    --sync-cert) SYNC_CERT="${2:?}"; shift 2 ;;
    --sync-key) SYNC_KEY="${2:?}"; shift 2 ;;
    --transport-cert) TRANSPORT_CERT="${2:?}"; shift 2 ;;
    --transport-key) TRANSPORT_KEY="${2:?}"; shift 2 ;;
    --transport-out) TRANSPORT_OUT="${2:?}"; shift 2 ;;
    --gateway-binary) GATEWAY_BINARY="${2:?}"; shift 2 ;;
    --frps-binary) FRPS_BINARY="${2:?}"; shift 2 ;;
    --frps-sha256) FRPS_SHA256="${2:?}"; shift 2 ;;
    --verify-frps) MODE="verify-frps"; VERIFY_FRPS="${2:?}"; shift 2 ;;
    --expect-ipv4) EXPECT_IPV4="${2:?}"; shift 2 ;;
    --expect-ipv6) EXPECT_IPV6="${2:?}"; shift 2 ;;
    --dry-run) DRY_RUN=1; shift ;;
    --generate-transport-cert) MODE="generate-transport-cert"; TUNNEL_HOST="${2:?}"; shift 2 ;;
    --audit-dns) MODE="audit-dns"; shift ;;
    --help|-h) usage; exit 0 ;;
    *) fail "unknown argument: $1 (try --help)" ;;
  esac
done

case "$MODE" in
  audit-dns)
    [[ -n "$NAMESPACE" && -n "$TUNNEL_HOST" ]] || fail "--audit-dns requires --namespace and --tunnel-host"
    audit_dns "$NAMESPACE" "$TUNNEL_HOST" "$EXPECT_IPV4" "$EXPECT_IPV6"
    exit $?
    ;;
  verify-frps)
    [[ -n "$VERIFY_FRPS" ]] || fail "--verify-frps requires a binary path"
    verify_frps_binary "$VERIFY_FRPS"
    log "frps binary verified: ${VERIFY_FRPS}"
    exit 0
    ;;
  generate-transport-cert)
    generate_transport_cert "$TUNNEL_HOST" "$TRANSPORT_OUT"
    exit 0
    ;;
esac

# ---- install-mode prerequisites ----------------------------------------------
[[ -n "$TUNNEL_HOST" ]] || fail "--tunnel-host is required"
[[ -n "$NAMESPACE" ]] || fail "--namespace is required"
[[ -n "$SYNC_URL" && -n "$SYNC_SAN" && -n "$SYNC_CA" && -n "$SYNC_CERT" && -n "$SYNC_KEY" ]] \
  || fail "control sync mTLS is required: --sync-url --sync-san --sync-ca --sync-cert --sync-key"
[[ -n "$TRANSPORT_CERT" && -n "$TRANSPORT_KEY" ]] \
  || fail "--transport-cert and --transport-key are required (or generate them with --generate-transport-cert)"
: "${SHAREBRIDGE_FRP_PLUGIN_SHARED_SECRET:?required: hex secret shared by frps and the gateway plugin}"
: "${SHAREBRIDGE_CONTROL_RELAY_PUBLIC_KEY:?required: 64-hex Ed25519 control relay signing public key}"
[[ "$SHAREBRIDGE_FRP_PLUGIN_SHARED_SECRET" =~ ^[0-9a-f]{16,128}$ ]] \
  || fail "SHAREBRIDGE_FRP_PLUGIN_SHARED_SECRET must be 16-128 lowercase hex characters"
[[ "$SHAREBRIDGE_CONTROL_RELAY_PUBLIC_KEY" =~ ^[0-9a-f]{64}$ ]] \
  || fail "SHAREBRIDGE_CONTROL_RELAY_PUBLIC_KEY must be 64 lowercase hex characters"
[[ "$NAMESPACE" =~ ^sb[0-9a-f]{8}$ ]] || fail "--namespace must be sb followed by eight lowercase hex characters"
[[ "$SYNC_SAN" =~ ^[A-Za-z0-9.-]+$ ]] || fail "--sync-san must be a hostname"
[[ "$SYNC_URL" == https://* ]] || fail "--sync-url must be HTTPS"

if [[ "$DRY_RUN" == "1" ]]; then
  cat <<EOF
DRY RUN — no changes were made, no secrets were read or printed.
  users        : ${GATEWAY_USER}, ${FRPS_USER} (system, loginless)
  gateway bin  : ${GATEWAY_BINARY:-<built from ${REPO_ROOT}/relay>}
  frps binary  : ${FRPS_BINARY:-<checksum-verified via ${FRP_FETCH} using ${FRP_MANIFEST}>} (SHA-256 verified before install)
  config dir   : ${CONFIG_DIR} (root:root 0600)
  installed    : frps.toml, gateway.env, frps.env, tunnel-server.{crt,key},
                 gateway-sync.{crt,key}, control-ca.crt
  units        : ${UNIT_DIR}/sharebridge-relay-gateway.service
                 ${UNIT_DIR}/sharebridge-relay-frps.service
  firewall     : ${DEPLOY_DIR}/firewall.nft — public 443/tcp + ${DEFAULT_TRANSPORT_PORT}/tcp only
  sync         : mTLS client certificate to ${SYNC_SAN} (${SYNC_URL})
  transport    : dedicated certificate for ${TUNNEL_HOST}
  dns audit    : install.sh --audit-dns --namespace ${NAMESPACE} --tunnel-host ${TUNNEL_HOST}
  secret scan  : secrets are never echoed; only the paths above are printed
EOF
  exit 0
fi

[[ "$(uname -s)" == "Linux" ]] || fail "install.sh targets Linux; use --dry-run elsewhere"
[[ "$(id -u)" == "0" ]] || fail "run as root on the relay VM"

# ---- users, directories, binaries --------------------------------------------
create_service_user "$GATEWAY_USER"
create_service_user "$FRPS_USER"

install -d -o root -g root -m 0755 "$CONFIG_DIR"
install -d -o root -g root -m 0755 "$LIB_DIR"
install -d -o "$GATEWAY_USER" -g "$GATEWAY_USER" -m 0700 /var/lib/sharebridge-relay-gateway
install -d -o "$FRPS_USER" -g "$FRPS_USER" -m 0700 /var/lib/sharebridge-relay-frps

# The pinned frps is never committed; fetch-frp.sh downloads exactly the
# release recorded in relay/frp/manifest.json and verifies its SHA-256
# checksum before staging the binary.
if [[ -z "$FRPS_BINARY" ]]; then
  log "fetching checksum-verified frps from the pinned manifest ${FRP_MANIFEST}"
  FRP_STAGE="$("$FRP_FETCH")"
  FRPS_BINARY="${FRP_STAGE}/frps"
fi
# Integrity-check every frps binary before installing it: an explicit
# --frps-sha256, or the manifest-pinned digest, must match. A caller-supplied
# binary is never trusted on its own.
verify_frps_binary "$FRPS_BINARY"
[[ -x "$FRPS_BINARY" ]] || fail "frps binary is not executable: $FRPS_BINARY"
install -o root -g root -m 0755 -- "$FRPS_BINARY" "${LIB_DIR}/frps"

if [[ -z "$GATEWAY_BINARY" ]]; then
  log "building the gateway from ${REPO_ROOT}/relay"
  GATEWAY_BINARY="/tmp/sharebridge-relay-gateway"
  ( cd "${REPO_ROOT}/relay" && CGO_ENABLED=0 go build -o "$GATEWAY_BINARY" ./cmd/gateway )
fi
[[ -x "$GATEWAY_BINARY" ]] || fail "gateway binary is not executable: $GATEWAY_BINARY"
install -o root -g root -m 0755 -- "$GATEWAY_BINARY" "${LIB_DIR}/gateway"

# ---- config and secrets (root:root 0600) -------------------------------------
RENDERED_FRPS="$(mktemp)"
trap 'rm -f "$RENDERED_FRPS"' EXIT
render_frps_config "$RENDERED_FRPS"
install_root_secret "$RENDERED_FRPS" "${CONFIG_DIR}/frps.toml"
# The plugin secret is supplied to frps through its EnvironmentFile (read by
# systemd, which runs as root) so it never appears in the config file.
umask 077
FRPS_ENV="$(mktemp)"
printf 'SHAREBRIDGE_FRP_PLUGIN_SHARED_SECRET=%s\n' "$SHAREBRIDGE_FRP_PLUGIN_SHARED_SECRET" >"$FRPS_ENV"
install_root_secret "$FRPS_ENV" "${CONFIG_DIR}/frps.env"
rm -f "$FRPS_ENV"

GATEWAY_ENV="$(mktemp)"
cat >"$GATEWAY_ENV" <<EOF
# ShareBridge relay gateway operator environment. Root:root 0600; no secret
# value is ever emitted to a log. Listener binds are pinned here so a
# misconfiguration cannot silently expose a private surface.
SHAREBRIDGE_FRP_PLUGIN_LISTEN_ADDR=127.0.0.1:9001
SHAREBRIDGE_GATEWAY_METRICS_ADDR=127.0.0.1:9101
SHAREBRIDGE_RELAY_DATA_DIR=/var/lib/sharebridge-relay-gateway
SHAREBRIDGE_FRP_PLUGIN_SHARED_SECRET=${SHAREBRIDGE_FRP_PLUGIN_SHARED_SECRET}
SHAREBRIDGE_CONTROL_RELAY_PUBLIC_KEY=${SHAREBRIDGE_CONTROL_RELAY_PUBLIC_KEY}
SHAREBRIDGE_CONTROL_SYNC_URL=${SYNC_URL}
SHAREBRIDGE_CONTROL_SYNC_SAN=${SYNC_SAN}
SHAREBRIDGE_GATEWAY_NAMESPACE=${NAMESPACE}
# §14 defaults; unset variables keep these documented values. Any value that
# exceeds the hard ceiling (hello-bytes inspection budget, global 8192)
# refuses startup instead of running with a silently wrong bound.
SHAREBRIDGE_GATEWAY_MAX_STREAMS_PER_SOURCE_IP=16
SHAREBRIDGE_GATEWAY_MAX_STREAMS_PER_ORIGIN=32
SHAREBRIDGE_GATEWAY_MAX_STREAMS_PER_AGENT=64
SHAREBRIDGE_GATEWAY_MAX_STREAMS_GLOBAL=8192
SHAREBRIDGE_GATEWAY_MAX_HELLO_BYTES=65536
SHAREBRIDGE_GATEWAY_HELLO_TIMEOUT=5s
SHAREBRIDGE_GATEWAY_DIAL_TIMEOUT=2s
SHAREBRIDGE_GATEWAY_IDLE_TIMEOUT=5m
SHAREBRIDGE_GATEWAY_ABSOLUTE_LIFETIME=24h
SHAREBRIDGE_GATEWAY_MAX_TRACKED_AGENTS=4096
EOF
install_root_secret "$GATEWAY_ENV" "${CONFIG_DIR}/gateway.env"
rm -f "$GATEWAY_ENV"

install_root_secret "$SYNC_CA" "${CONFIG_DIR}/control-ca.crt"
install_root_secret "$SYNC_CERT" "${CONFIG_DIR}/gateway-sync.crt"
install_root_secret "$SYNC_KEY" "${CONFIG_DIR}/gateway-sync.key"
install_root_secret "$TRANSPORT_KEY" "${CONFIG_DIR}/tunnel-server.key"
# Certificates are public, but installing them 0600 keeps the whole secret
# directory uniform and accessible only through systemd credentials.
install_root_secret "$TRANSPORT_CERT" "${CONFIG_DIR}/tunnel-server.crt"

# ---- units + firewall --------------------------------------------------------
install -o root -g root -m 0644 -- "${DEPLOY_DIR}/sharebridge-relay-gateway.service" "${UNIT_DIR}/sharebridge-relay-gateway.service"
install -o root -g root -m 0644 -- "${DEPLOY_DIR}/sharebridge-relay-frps.service" "${UNIT_DIR}/sharebridge-relay-frps.service"

# Validate before enabling; a malformed unit never reaches systemd.
if command -v systemd-analyze >/dev/null 2>&1; then
  systemd-analyze verify \
    "${UNIT_DIR}/sharebridge-relay-gateway.service" \
    "${UNIT_DIR}/sharebridge-relay-frps.service"
else
  log "WARNING: systemd-analyze not found; skipping unit verification"
fi

if command -v nft >/dev/null 2>&1; then
  nft -f "${DEPLOY_DIR}/firewall.nft"
else
  log "WARNING: nft not found; apply ${DEPLOY_DIR}/firewall.nft manually"
fi

systemctl daemon-reload
# frps needs the gateway's loopback authorization plugin, so the gateway
# starts first (see the units' After=/Wants= ordering).
systemctl enable --now sharebridge-relay-gateway.service
systemctl enable --now sharebridge-relay-frps.service

for unit in sharebridge-relay-gateway.service sharebridge-relay-frps.service; do
  systemctl is-active --quiet "$unit" || fail "${unit} is not active"
done

# ---- DNS audit ---------------------------------------------------------------
audit_dns "$NAMESPACE" "$TUNNEL_HOST" || log "WARNING: DNS audit failed — fix the zone before enabling relay routes"

log "relay VM provisioning complete; verify with: bash ${DEPLOY_DIR}/deploy_test.sh"
