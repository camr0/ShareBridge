#!/usr/bin/env bash
#
# deploy-testing-relay.sh — deploy the phase-4a relay stack (control + relay
# gateway + frps + STUN) to the throwaway TEST VPS.
#
# This is the DEV/TEST collocated deployment allowed by spec §4.6 ("second
# bound IP or nonstandard public port"): control, the SNI relay gateway and
# frps share ONE box. Production acceptance (separate relay VM, public 443
# parity) is M6 and is NOT what this script deploys.
#
# What it deploys/updates (idempotent — safe to re-run):
#   sharebridge.service              control (signaling, :8080 plain HTTP,
#                                    STUN listener on UDP 3478)
#   sharebridge-relay-gateway.service  SNI gateway (:443) + loopback-only FRP
#                                    authorization plugin (127.0.0.1:9001)
#   sharebridge-relay-frps.service   pinned frps v0.71.0 (transport on
#                                    RELAY_GATEWAY_PORT, default 7000; proxy
#                                    ports loopback-only, narrow allowPorts,
#                                    maxPortsPerClient=1, mandatory
#                                    authorization plugin, no dashboard)
#   UFW rules                        443/tcp, 3478/udp, transport/tcp, :8080/tcp
#
# The frps.toml is generated from the config shape proven by the hermetic
# gate harness (relay/internal/frptest, gate_fixture_test.go writeFrpsConfig)
# with the transport bind moved from loopback to the public interface — the
# proxy ports stay loopback-only because the collocated gateway is their only
# client (spec §15.1).
#
# Upgrade-in-place: the test VPS may still carry the phase-3 deployment
# (/opt/sharebridge/{server,.env,web/,pb_data/}). Re-running this script
# replaces binary, assets, env and units in place and PRESERVES pb_data.
# The stale phase-3 RELAY_JWT_SECRET line disappears with the rewritten .env
# (current config code does not read it).
#
# Usage:
#   ./deploy-testing-relay.sh <user@host>               # deploy + verify (idempotent)
#   ./deploy-testing-relay.sh <user@host> --bootstrap   # also create a test user + API key, print agent + gate env
#   ./deploy-testing-relay.sh <user@host> --teardown    # stop + disable all three services
#
# Secrets: read from .env.testing (gitignored), forwarded to the box over ssh
# stdin and NEVER echoed. Required keys:
#   CLOUDFLARE_TOKEN                    DNS-edit token for the test base domain
#   RELAY_GATEWAY_HOST                  public hostname agents' frpc connects to
#   RELAY_GATEWAY_IPV4                  public IPv4 of this VPS (relay DNS wildcard)
#   RELAY_AUTH_KEY_SEED                 64-hex-char Ed25519 seed (control signs,
#                                       gateway verifies the derived public key;
#                                       keep STABLE across re-runs)
#   SHAREBRIDGE_FRP_PLUGIN_SHARED_SECRET hex secret shared by frps and the
#                                       gateway's plugin endpoint (URL-safe hex)
# See .env.testing.example for every key with comments; the runbook is
# docs/operations/phase4a-test-vps-deploy.md.
#
# The relay transport CA lives in .env.testing.d/ (gitignored via the .env.*
# pattern): the CA certificate must be copied to the home server so the
# agent's frpc can verify frps transport TLS (SHAREBRIDGE_RELAY_CA_FILE).
set -euo pipefail

HOST="${1:?usage: deploy-testing-relay.sh <user@host> [--bootstrap|--teardown]}"
MODE="${2:-deploy}"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
REMOTE_DIR="/opt/sharebridge"
SVC_CONTROL="sharebridge"
SVC_GATEWAY="sharebridge-relay-gateway"
SVC_FRPS="sharebridge-relay-frps"
ENV_FILE="$SCRIPT_DIR/.env.testing"
CA_DIR="$SCRIPT_DIR/.env.testing.d"
HOSTNAME_ONLY="${HOST#*@}"

# frps plugin endpoint of the collocated gateway: the gateway binary enforces
# a numeric loopback address for SHAREBRIDGE_FRP_PLUGIN_LISTEN_ADDR
# (relay/cmd/gateway/main.go), and 127.0.0.1:9001 is its default.
PLUGIN_LISTEN="127.0.0.1:9001"
PLUGIN_API_PATH="/frp/authorize"   # frpplugin.APIPath (relay/internal/frpplugin/server.go)
PLUGIN_USER="sharebridge-frps"     # frpplugin.PluginAuthUsername

# ---- teardown ----------------------------------------------------------------
if [[ "$MODE" == "--teardown" ]]; then
  ssh "$HOST" "systemctl disable --now $SVC_FRPS 2>/dev/null || true
systemctl disable --now $SVC_GATEWAY 2>/dev/null || true
systemctl disable --now $SVC_CONTROL 2>/dev/null || true
echo 'stopped + disabled: $SVC_FRPS $SVC_GATEWAY $SVC_CONTROL'
echo 'left in place: $REMOTE_DIR (pb_data, relay-data, binaries, frps.toml). Snapshot/delete the box in the cloud console to stop billing.'"
  exit 0
fi

# ---- secrets + config (sourced, never echoed) --------------------------------
[[ -f "$ENV_FILE" ]] || { echo "error: $ENV_FILE not found (see .env.testing.example; needs: CLOUDFLARE_TOKEN, RELAY_GATEWAY_HOST, RELAY_GATEWAY_IPV4, RELAY_AUTH_KEY_SEED, SHAREBRIDGE_FRP_PLUGIN_SHARED_SECRET)" >&2; exit 1; }
# shellcheck disable=SC1090
source "$ENV_FILE"

: "${CLOUDFLARE_TOKEN:?"CLOUDFLARE_TOKEN not set in $ENV_FILE"}"
: "${RELAY_GATEWAY_HOST:?"RELAY_GATEWAY_HOST not set in $ENV_FILE (public hostname agents' frpc connects to)"}"
: "${RELAY_GATEWAY_IPV4:?"RELAY_GATEWAY_IPV4 not set in $ENV_FILE (public IPv4 of the VPS)"}"
: "${RELAY_AUTH_KEY_SEED:?"RELAY_AUTH_KEY_SEED not set in $ENV_FILE (openssl rand -hex 32; keep stable across re-runs)"}"
: "${SHAREBRIDGE_FRP_PLUGIN_SHARED_SECRET:?"SHAREBRIDGE_FRP_PLUGIN_SHARED_SECRET not set in $ENV_FILE (openssl rand -hex 16)"}"

BASE_DOMAIN="${CONTENT_BASE_DOMAIN:-sharebridgeusercontent.com}"
ACME_EMAIL="${ACME_EMAIL:-ali@sharebridge.app}"
# Production CA by default: agents validate content certs against the system
# trust store, so Let's Encrypt staging roots are rejected.
ACME_CA="${ACME_CA_DIR:-https://acme-v02.api.letsencrypt.org/directory}"
PORT="${PORT:-8080}"
# One knob feeds three consumers: control's relay_config.gateway_port, the
# agents' frpc transport target, and frps bindPort. Keep them equal.
TRANSPORT_PORT="${RELAY_GATEWAY_PORT:-7000}"
RELAY_PORT_MIN="${RELAY_PORT_MIN:-10000}"
RELAY_PORT_MAX="${RELAY_PORT_MAX:-10099}"
# Test default true: the Task 24 Phase B checklist and the Task 20 release
# gate need live selection. Set false to return control to rollback mode
# (relay is then never selected; direct candidates still get the interstitial).
RELAY_SELECTION_ENABLED="${RELAY_SELECTION_ENABLED:-true}"
# Publicly resolvable host:port advertised in stun_challenge. Defaults to the
# ssh host argument; override with the VPS public IPv4 or its DNS name if the
# ssh alias is not publicly resolvable.
STUN_ADVERTISE="${STUN_ADVERTISE_ADDR:-${HOSTNAME_ONLY}:3478}"

fail() { echo "error: $*" >&2; exit 1; }

is_uint() { [[ "$1" =~ ^[0-9]+$ ]]; }

# RELAY_AUTH_KEY_SEED: 64 hex chars (control/internal/relayctl/credentials.go
# NewSignerFromHexSeed).
[[ "$RELAY_AUTH_KEY_SEED" =~ ^[0-9a-f]{64}$ ]] || fail "RELAY_AUTH_KEY_SEED must be 64 lowercase hex chars (openssl rand -hex 32)"
# Plugin secret: hex only — it is embedded in the frps httpPlugins URL
# userinfo, so URL-unsafe characters would corrupt the endpoint.
[[ "$SHAREBRIDGE_FRP_PLUGIN_SHARED_SECRET" =~ ^[0-9a-f]{16,128}$ ]] || fail "SHAREBRIDGE_FRP_PLUGIN_SHARED_SECRET must be 16-128 hex chars (openssl rand -hex 16)"
# Gateway IPv4: plain IPv4 literal (control/internal/directctl/enroll.go
# validRelayGatewayIPv4 rejects anything else).
[[ "$RELAY_GATEWAY_IPV4" =~ ^[0-9.]+$ ]] || fail "RELAY_GATEWAY_IPV4 must be a plain IPv4 literal, got a value containing other characters"
# Gateway host: conservative RFC 1123 charset (config.STUNAdvertise applies
# the same rule to STUN hosts; relayctl.Settings.Validate requires non-empty).
[[ "$RELAY_GATEWAY_HOST" =~ ^[A-Za-z0-9]([A-Za-z0-9-]*[A-Za-z0-9])?(\.[A-Za-z0-9]([A-Za-z0-9-]*[A-Za-z0-9])?)+$ ]] || fail "RELAY_GATEWAY_HOST must be an RFC 1123 hostname (letters, digits, hyphens, dots)"
# Ports (relayctl.NewPortRange bounds: 1024-65535 inclusive).
is_uint "$PORT" && is_uint "$TRANSPORT_PORT" && is_uint "$RELAY_PORT_MIN" && is_uint "$RELAY_PORT_MAX" || fail "PORT / RELAY_GATEWAY_PORT / RELAY_PORT_MIN / RELAY_PORT_MAX must be integers"
(( TRANSPORT_PORT >= 1024 )) || fail "RELAY_GATEWAY_PORT must be >= 1024 (relayctl.NewPortRange lower bound)"
(( RELAY_PORT_MIN >= 1024 && RELAY_PORT_MAX <= 65535 && RELAY_PORT_MIN <= RELAY_PORT_MAX )) || fail "RELAY_PORT_MIN/MAX must satisfy 1024 <= MIN <= MAX <= 65535 (relayctl.NewPortRange)"
(( TRANSPORT_PORT != 443 && TRANSPORT_PORT != PORT && TRANSPORT_PORT != 3478 )) || fail "RELAY_GATEWAY_PORT must not collide with 443 (gateway), $PORT (control) or 3478 (STUN)"
(( TRANSPORT_PORT < RELAY_PORT_MIN || TRANSPORT_PORT > RELAY_PORT_MAX )) || fail "RELAY_GATEWAY_PORT ($TRANSPORT_PORT) must stay outside the proxy port range [${RELAY_PORT_MIN}, ${RELAY_PORT_MAX}]"
# STUN advertise: host:port; the listener port is pinned to 3478 by
# stun.ParseBindAddr, and control re-validates the full public-class rule at
# startup (config.STUNAdvertise fails the process closed on a bad value).
[[ "$STUN_ADVERTISE" =~ ^[A-Za-z0-9.-]+:3478$ ]] || fail "STUN_ADVERTISE_ADDR must be <host>:3478 (the STUN port is fixed by stun.ParseBindAddr)"

# ---- derive the gateway's verification key from the relay seed ----------------
# Control signs with RELAY_AUTH_KEY_SEED (control/internal/relayctl/
# credentials.go); the gateway holds ONLY the matching Ed25519 public key in
# SHAREBRIDGE_CONTROL_RELAY_PUBLIC_KEY (relay/cmd/gateway/main.go requires 64
# hex chars). No committed tool prints the derived key yet, so this deploy
# script derives it with the Go stdlib on the machine that already builds the
# binaries. The seed itself never leaves this machine unencrypted.
PUBKEY_TMP="$(mktemp -d)"
trap 'rm -rf "$PUBKEY_TMP"' EXIT
cat > "$PUBKEY_TMP/main.go" <<'EOF'
package main

import (
	"crypto/ed25519"
	"encoding/hex"
	"fmt"
	"os"
)

func main() {
	seed, err := hex.DecodeString(os.Args[1])
	if err != nil || len(seed) != ed25519.SeedSize {
		fmt.Fprintln(os.Stderr, "seed must be 64 hex chars")
		os.Exit(1)
	}
	priv := ed25519.NewKeyFromSeed(seed)
	pub, _ := priv.Public().(ed25519.PublicKey)
	fmt.Println(hex.EncodeToString(pub))
}
EOF
CONTROL_PUBLIC_KEY_HEX="$(go run "$PUBKEY_TMP/main.go" "$RELAY_AUTH_KEY_SEED" | tr -d '[:space:]')"
[[ "$CONTROL_PUBLIC_KEY_HEX" =~ ^[0-9a-f]{64}$ ]] || fail "derived gateway public key is not 64 hex chars"

# ---- build (pure Go, no cgo) ---------------------------------------------------
BIN_CONTROL="/tmp/sharebridge-server-linux"
BIN_GATEWAY="/tmp/sharebridge-relay-gateway-linux"
( cd "$SCRIPT_DIR" && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o "$BIN_CONTROL" ./cmd/server )
( cd "$REPO_ROOT/relay" && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o "$BIN_GATEWAY" ./cmd/gateway )
echo "built $BIN_CONTROL + $BIN_GATEWAY"

# ---- fetch pinned frps (linux_amd64) with manifest SHA-256 verification --------
# Mirrors relay/scripts/fetch-frp.sh (which stages for the RUNNING platform;
# the VPS needs linux_amd64) using the same pinned manifest and cache root.
MANIFEST="$REPO_ROOT/relay/frp/manifest.json"
FRP_VERSION="$(sed -n 's/^[[:space:]]*"version":[[:space:]]*"\([^"]*\)".*/\1/p' "$MANIFEST" | head -n 1)"
FRP_URL="$(sed -n '/^[[:space:]]*"linux_amd64":/,/^[[:space:]]*}/ s/^[[:space:]]*"url":[[:space:]]*"\([^"]*\)".*/\1/p' "$MANIFEST" | head -n 1)"
FRP_SHA="$(sed -n '/^[[:space:]]*"linux_amd64":/,/^[[:space:]]*}/ s/^[[:space:]]*"sha256":[[:space:]]*"\([^"]*\)".*/\1/p' "$MANIFEST" | head -n 1)"
[[ "$FRP_VERSION" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] || fail "manifest version unreadable: $MANIFEST"
[[ "$FRP_URL" == "https://github.com/fatedier/frp/releases/download/v${FRP_VERSION}/frp_${FRP_VERSION}_linux_amd64.tar.gz" ]] || fail "manifest linux_amd64 URL is not the exact pinned asset: $FRP_URL"
[[ "$FRP_SHA" =~ ^[0-9a-f]{64}$ ]] || fail "manifest linux_amd64 sha256 unreadable"
FRP_CACHE="$REPO_ROOT/relay/.cache/frp/deploy-testing-relay/v${FRP_VERSION}-sha256-${FRP_SHA}"
FRPS_BIN="$FRP_CACHE/frps"
if [[ ! -x "$FRPS_BIN" ]]; then
  mkdir -p "$FRP_CACHE"
  TARBALL="$FRP_CACHE/frp_${FRP_VERSION}_linux_amd64.tar.gz"
  curl -fsSL -o "$TARBALL" "$FRP_URL"
  ACTUAL="$(shasum -a 256 "$TARBALL" 2>/dev/null | awk '{print $1}' || sha256sum "$TARBALL" | awk '{print $1}')"
  [[ "$ACTUAL" == "$FRP_SHA" ]] || fail "frps tarball SHA-256 mismatch: got $ACTUAL want $FRP_SHA (refusing to deploy an unpinned binary)"
  tar -xzf "$TARBALL" -C "$FRP_CACHE" "frp_${FRP_VERSION}_linux_amd64/frps" "frp_${FRP_VERSION}_linux_amd64/frpc"
  mv "$FRP_CACHE/frp_${FRP_VERSION}_linux_amd64/frps" "$FRPS_BIN"
  mv "$FRP_CACHE/frp_${FRP_VERSION}_linux_amd64/frpc" "$FRP_CACHE/frpc"
  rmdir "$FRP_CACHE/frp_${FRP_VERSION}_linux_amd64"
  chmod +x "$FRPS_BIN" "$FRP_CACHE/frpc"
fi
echo "frps $FRP_VERSION ready (sha256 verified): $FRPS_BIN"
echo "NOTE: $FRP_CACHE/frpc is the linux_amd64 pinned frpc for the home-server agent image (see runbook)."

# ---- relay transport CA + server certificate ----------------------------------
# The agent's generated frpc verifies frps transport TLS with an explicit CA
# bundle and serverName = relay_config.gateway_addr (agent/internal/tunnel/
# config.go Render), so the leaf SAN must be RELAY_GATEWAY_HOST. The CA is
# created once and reused so re-runs never invalidate a CA already copied to
# the home server. .env.testing.d is covered by the .gitignore .env.* pattern.
mkdir -p "$CA_DIR"
CA_KEY="$CA_DIR/relay-transport-ca.key"
CA_PEM="$CA_DIR/relay-transport-ca.pem"
LEAF_KEY="$CA_DIR/frps-transport.key"
LEAF_CRT="$CA_DIR/frps-transport.crt"
if [[ ! -f "$CA_PEM" || ! -f "$CA_KEY" ]]; then
  openssl ecparam -genkey -name prime256v1 -out "$CA_KEY" 2>/dev/null
  openssl req -new -x509 -key "$CA_KEY" -out "$CA_PEM" -days 3650 \
    -subj "/CN=sharebridge-relay-transport-test-ca" 2>/dev/null
  echo "created relay transport CA (test-only, 10y): $CA_PEM"
fi
LEAF_CSR="$CA_DIR/frps-transport.csr"
EXT_FILE="$CA_DIR/san.ext"
cat > "$EXT_FILE" <<EOF
basicConstraints=CA:FALSE
keyUsage=digitalSignature,keyEncipherment
extendedKeyUsage=serverAuth
subjectAltName=DNS:${RELAY_GATEWAY_HOST},IP:${RELAY_GATEWAY_IPV4}
EOF
openssl ecparam -genkey -name prime256v1 -out "$LEAF_KEY" 2>/dev/null
openssl req -new -key "$LEAF_KEY" -out "$LEAF_CSR" -subj "/CN=${RELAY_GATEWAY_HOST}" 2>/dev/null
openssl x509 -req -in "$LEAF_CSR" -CA "$CA_PEM" -CAkey "$CA_KEY" -CAcreateserial \
  -out "$LEAF_CRT" -days 3650 -extfile "$EXT_FILE" 2>/dev/null
rm -f "$LEAF_CSR" "$EXT_FILE" "$CA_DIR/relay-transport-ca.srl"
echo "issued frps transport certificate for ${RELAY_GATEWAY_HOST} (SAN also includes ${RELAY_GATEWAY_IPV4})"

# ---- stop any running units BEFORE copying binaries: Linux refuses to open
# ---- a running binary for write (ETXTBSY), so an in-place upgrade must not
# ---- scp over live executables. Units are restarted below after the copy.
ssh "$HOST" "systemctl stop $SVC_FRPS $SVC_GATEWAY $SVC_CONTROL 2>/dev/null || true"

# ---- copy binaries + assets + certs --------------------------------------------
ssh "$HOST" "mkdir -p $REMOTE_DIR/web $REMOTE_DIR/relay-bin $REMOTE_DIR/frps $REMOTE_DIR/relay-transport $REMOTE_DIR/relay-data && chmod 700 $REMOTE_DIR/relay-data $REMOTE_DIR/relay-transport"
scp -q "$BIN_CONTROL" "$HOST:$REMOTE_DIR/server"
scp -q "$BIN_GATEWAY" "$HOST:$REMOTE_DIR/relay-bin/gateway"
scp -q "$FRPS_BIN" "$HOST:$REMOTE_DIR/frps/frps"
scp -q "$LEAF_CRT" "$HOST:$REMOTE_DIR/relay-transport/frps.crt"
scp -q "$LEAF_KEY" "$HOST:$REMOTE_DIR/relay-transport/frps.key"
# Control web assets: startup fails closed without the Task 22
# route-interstitial.* assets (directctl.LoadInterstitialAssets("./web")).
scp -q "$SCRIPT_DIR/web/home.html" "$SCRIPT_DIR/web/login.html" \
       "$SCRIPT_DIR/web/register.html" "$SCRIPT_DIR/web/account.html" \
       "$SCRIPT_DIR/web/route-interstitial.html" "$SCRIPT_DIR/web/route-interstitial.js" \
       "$SCRIPT_DIR/web/route-interstitial.css" \
       "$HOST:$REMOTE_DIR/web/"
ssh "$HOST" "chmod 600 $REMOTE_DIR/relay-transport/frps.key $REMOTE_DIR/relay-transport/frps.crt"

# ---- remote control .env (ssh stdin; every name verified against
# ---- control/internal/config/config.go Load()) ----------------------------------
ssh "$HOST" "cat > $REMOTE_DIR/.env && chmod 600 $REMOTE_DIR/.env" <<EOF
# control (sharebridge.service)
CLOUDFLARE_TOKEN=${CLOUDFLARE_TOKEN}
CONTENT_BASE_DOMAIN=${BASE_DOMAIN}
ACME_EMAIL=${ACME_EMAIL}
ACME_CA_DIR=${ACME_CA}
PORT=${PORT}
DATA_DIR=${REMOTE_DIR}/pb_data
# relay policy — RelayPolicyEnabled() needs RELAY_GATEWAY_HOST + RELAY_AUTH_KEY_SEED
RELAY_GATEWAY_HOST=${RELAY_GATEWAY_HOST}
RELAY_GATEWAY_PORT=${TRANSPORT_PORT}
RELAY_PORT_MIN=${RELAY_PORT_MIN}
RELAY_PORT_MAX=${RELAY_PORT_MAX}
RELAY_AUTH_KEY_SEED=${RELAY_AUTH_KEY_SEED}
RELAY_GATEWAY_IPV4=${RELAY_GATEWAY_IPV4}
# STUN listener (UDP 3478 is pinned by stun.ParseBindAddr; "off" would disable)
STUN_BIND_ADDR=0.0.0.0:3478
STUN_ADVERTISE_ADDR=${STUN_ADVERTISE}
# Task 20 flag (getEnvBool, fail-closed): false = rollback mode
RELAY_SELECTION_ENABLED=${RELAY_SELECTION_ENABLED}
EOF

# ---- remote gateway env (relay/cmd/gateway/main.go names) -----------------------
ssh "$HOST" "cat > $REMOTE_DIR/relay-gateway.env && chmod 600 $REMOTE_DIR/relay-gateway.env" <<EOF
SHAREBRIDGE_GATEWAY_LISTEN_ADDR=:443
SHAREBRIDGE_FRP_PLUGIN_LISTEN_ADDR=${PLUGIN_LISTEN}
SHAREBRIDGE_FRP_PLUGIN_SHARED_SECRET=${SHAREBRIDGE_FRP_PLUGIN_SHARED_SECRET}
SHAREBRIDGE_CONTROL_RELAY_PUBLIC_KEY=${CONTROL_PUBLIC_KEY_HEX}
SHAREBRIDGE_RELAY_PORT_MIN=${RELAY_PORT_MIN}
SHAREBRIDGE_RELAY_PORT_MAX=${RELAY_PORT_MAX}
SHAREBRIDGE_RELAY_DATA_DIR=${REMOTE_DIR}/relay-data
EOF

# ---- remote frps.toml (shape proven by relay/internal/frptest/
# ---- gate_fixture_test.go writeFrpsConfig; binds adapted per spec §4.6/§15.1:
# transport public, proxy ports loopback-only, plugin mandatory, no dashboard)
ssh "$HOST" "cat > $REMOTE_DIR/frps.toml && chmod 600 $REMOTE_DIR/frps.toml" <<EOF
# Generated by deploy-testing-relay.sh — pinned frps ${FRP_VERSION} (spec §15.1).
bindAddr = "0.0.0.0"
bindPort = ${TRANSPORT_PORT}
# Proxy ports are loopback-only: the collocated SNI gateway is their only
# client (browsers never dial these ports; spec §15.1).
proxyBindAddr = "127.0.0.1"
allowPorts = [{ start = ${RELAY_PORT_MIN}, end = ${RELAY_PORT_MAX} }]
maxPortsPerClient = 1
userConnTimeout = 10
transport.tcpMux = false
transport.heartbeatTimeout = 45
transport.tls.force = true
transport.tls.certFile = "${REMOTE_DIR}/relay-transport/frps.crt"
transport.tls.keyFile = "${REMOTE_DIR}/relay-transport/frps.key"

# Mandatory fail-closed authorization + presence plugin: the collocated
# gateway's loopback endpoint (relay/internal/frpplugin).
[[httpPlugins]]
name = "sharebridge-authorize-presence"
addr = "http://${PLUGIN_USER}:${SHAREBRIDGE_FRP_PLUGIN_SHARED_SECRET}@${PLUGIN_LISTEN}${PLUGIN_API_PATH}"
path = "${PLUGIN_API_PATH}"
ops = ["Login", "NewProxy", "CloseProxy", "Ping", "NewUserConn"]
EOF

# ---- systemd units (base64 through ssh, phase-3 pattern; idempotent restart) ----
write_unit() {
  local unit_name="$1" unit_text="$2"
  local b64
  b64="$(printf '%s' "$unit_text" | base64)"
  ssh "$HOST" "printf '%s' '$b64' | base64 -d > /etc/systemd/system/${unit_name}.service && systemctl daemon-reload && systemctl enable ${unit_name} >/dev/null 2>&1 && systemctl restart ${unit_name}"
}

UNIT_CONTROL=$(cat <<EOF
[Unit]
Description=ShareBridge Signaling Server + STUN listener (testing)
After=network-online.target
Wants=network-online.target

[Service]
WorkingDirectory=${REMOTE_DIR}
EnvironmentFile=${REMOTE_DIR}/.env
ExecStart=${REMOTE_DIR}/server serve
Restart=always
RestartSec=2

[Install]
WantedBy=multi-user.target
EOF
)

UNIT_GATEWAY=$(cat <<EOF
[Unit]
Description=ShareBridge Relay Gateway: SNI :443 + FRP auth plugin (testing)
After=network-online.target
Wants=network-online.target

[Service]
WorkingDirectory=${REMOTE_DIR}
EnvironmentFile=${REMOTE_DIR}/relay-gateway.env
ExecStart=${REMOTE_DIR}/relay-bin/gateway
Restart=always
RestartSec=2

[Install]
WantedBy=multi-user.target
EOF
)

UNIT_FRPS=$(cat <<EOF
[Unit]
Description=ShareBridge pinned frps ${FRP_VERSION} (testing)
After=network-online.target
Wants=network-online.target

[Service]
WorkingDirectory=${REMOTE_DIR}
ExecStart=${REMOTE_DIR}/frps/frps -c ${REMOTE_DIR}/frps.toml
Restart=always
RestartSec=2

[Install]
WantedBy=multi-user.target
EOF
)

write_unit "$SVC_CONTROL" "$UNIT_CONTROL"
write_unit "$SVC_GATEWAY" "$UNIT_GATEWAY"
write_unit "$SVC_FRPS" "$UNIT_FRPS"
echo "systemd units installed + restarted: $SVC_CONTROL $SVC_GATEWAY $SVC_FRPS"

# ---- UFW (add rules only; never enable ufw or touch SSH from a script) ----------
if ssh "$HOST" "command -v ufw >/dev/null 2>&1 && ufw status 2>/dev/null | head -n 1 | grep -q 'Status: active'"; then
  ssh "$HOST" "ufw allow 443/tcp >/dev/null; ufw allow 3478/udp >/dev/null; ufw allow ${TRANSPORT_PORT}/tcp >/dev/null; ufw allow ${PORT}/tcp >/dev/null; ufw status | grep -E '443/tcp|3478/udp|${TRANSPORT_PORT}/tcp|${PORT}/tcp'"
else
  echo "WARNING: ufw not active on the box — no firewall rules applied (SSH access unaffected)."
fi

# ---- wait for control health, then print verification evidence -------------------
BOX_HTTP="http://${HOSTNAME_ONLY}:${PORT}"
CONTROL_HEALTHY=""
for _ in {1..20}; do
  if curl -sf -o /dev/null "$BOX_HTTP/_/" 2>/dev/null; then CONTROL_HEALTHY=yes; break; fi
  sleep 1
done

echo
echo "=== on-box service + listener checks ==="
ssh "$HOST" "
for svc in $SVC_CONTROL $SVC_GATEWAY $SVC_FRPS; do
  printf '%-28s %s\n' \"\$svc\" \"\$(systemctl is-active \$svc)\"
done
ss -ltn | awk 'NR>1 {print \$4}' | grep -E '(:443|:${TRANSPORT_PORT}|:${PORT})\$' | sort -u | sed 's/^/tcp listening: /'
ss -lun | awk 'NR>1 {print \$4}' | grep -E ':3478\$' | sort -u | sed 's/^/udp listening: /'
journalctl -u $SVC_CONTROL -n 3 --no-pager -o cat | sed 's/^/control: /'
journalctl -u $SVC_GATEWAY -n 3 --no-pager -o cat | sed 's/^/gateway: /'
journalctl -u $SVC_FRPS -n 3 --no-pager -o cat | sed 's/^/frps: /'
"
if [[ "$CONTROL_HEALTHY" == "yes" ]]; then
  echo "control health: PASS ($BOX_HTTP/_/ answered)"
else
  echo "control health: FAIL ($BOX_HTTP/_/ never answered — check journalctl -u $SVC_CONTROL on the box)"
fi

echo
echo "=== verification checklist (run these before declaring the deploy good) ==="
cat <<EOF
1. control:      curl -sf $BOX_HTTP/_/                     (PocketBase health)
2. gateway 443:  nc -vz ${HOSTNAME_ONLY} 443                 (SNI listener; TLS is passthrough, a bare connect is fine)
3. frps transport: nc -vz ${HOSTNAME_ONLY} ${TRANSPORT_PORT}   (TLS-required FRP transport)
4. STUN UDP:     nc -uvz ${HOSTNAME_ONLY} 3478               (from OUTSIDE the VPS — e.g. the home server; from home is the real Task 25 precheck)
5. plugin loopback only: on the box, ss -ltn | grep 9001 must show 127.0.0.1:9001, never 0.0.0.0
6. frps is strict: check 'journalctl -u $SVC_FRPS' has no config errors; frps with transport.tls.force rejects plaintext clients
7. agent enrollment: run the agent on the home server (runbook §5) and confirm enrollment_ready in its logs
8. relay DNS: after enrollment, dig +short '*.relay.<namespace>.${BASE_DOMAIN}' must answer ${RELAY_GATEWAY_IPV4}
9. interstitial: open $BOX_HTTP/s/<code> in a real browser (runbook §7)
Control logs:    ssh $HOST journalctl -u $SVC_CONTROL -f
Gateway logs:    ssh $HOST journalctl -u $SVC_GATEWAY -f
frps logs:       ssh $HOST journalctl -u $SVC_FRPS -f
EOF

# ---- bootstrap: throwaway user + API key + printed agent/gate env ----------------
if [[ "$MODE" == "--bootstrap" ]]; then
  EMAIL="test-$(openssl rand -hex 4)@sharebridge.app"
  PASSWORD="$(openssl rand -hex 16)"
  curl -sf -X POST "$BOX_HTTP/api/collections/users/records" -H 'Content-Type: application/json' \
    -d "{\"email\":\"$EMAIL\",\"password\":\"$PASSWORD\",\"passwordConfirm\":\"$PASSWORD\"}" >/dev/null
  JWT="$(curl -sf -X POST "$BOX_HTTP/api/collections/users/auth-with-password" -H 'Content-Type: application/json' \
    -d "{\"identity\":\"$EMAIL\",\"password\":\"$PASSWORD\"}" | python3 -c 'import sys,json;print(json.load(sys.stdin)["token"])')"
  APIKEY="$(curl -sf -X POST "$BOX_HTTP/api/keys" -H "Authorization: Bearer $JWT" -H 'Content-Type: application/json' \
    -d '{"label":"testing"}' | python3 -c 'import sys,json;print(json.load(sys.stdin)["key"])')"
  echo
  echo "=== agent env (run on the agent host / compose .env) ==="
  echo "export SIGNALING_SERVER=ws://${HOSTNAME_ONLY}:${PORT}"
  echo "export SHAREBRIDGE_API_KEY=${APIKEY}"
  echo "export CONTENT_BASE_DOMAIN=${BASE_DOMAIN}"
  echo "export ALLOWED_SHAREBRIDGE_HOST=demo.example.com   # or your WebDAV host"
  echo
  echo "=== relay transport CA for the agent (copy to the home server, then into the container; runbook §5) ==="
  echo "scp $CA_PEM <homeserver>:~/sharebridge/agent/relay-transport-ca.pem"
  echo
  echo "=== Task 25 STUN gate env (run from the home network; runbook §8) ==="
  echo "export STUN_GATE_API_KEY=${APIKEY}   # env-only, never a flag"
  echo "scripts/stun-nat-gate.sh --target remote \\"
  echo "  --server ws://${HOSTNAME_ONLY}:${PORT} \\"
  echo "  --stun-addr ${STUN_ADVERTISE} \\"
  echo "  --expected-public-ip <your-home-egress-IPv4>"
  echo "# Optional fast path: STUN_GATE_CERT_FINGERPRINT=<fingerprint of the already-enrolled agent cert> skips CSR re-issuance."
fi
