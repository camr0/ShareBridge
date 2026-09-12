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
# §11.3 control↔gateway sync channel: this collocated box runs control's
# PRIVATE loopback mTLS sync listener (127.0.0.1:9443) and the relay gateway,
# its only client, sharing one sync CA. Control's five CONTROL_SYNC_* variables
# and the gateway's six SHAREBRIDGE_*_SYNC_*/NAMESPACE variables are
# all-or-nothing on both sides (a partial set refuses startup). The mTLS
# material is generated locally by this script (never committed) and installed
# root:root 0600. See docs/operations/phase4a-relay.md §8 for the recipe and
# docs/operations/phase4a-test-vps-deploy.md for the operator steps.
#
# Namespace: SHAREBRIDGE_GATEWAY_NAMESPACE must equal the (single) enrolled
# agent's §6 namespace, which control GENERATES at enrollment ("sb" + 8 hex).
# It cannot be known at deploy time, so this script deliberately ships the
# gateway DARK (no sync variables at all) until the operator pins it after
# enrollment with --pin-namespace. Pinning one namespace is a deliberate
# single-agent TEST simplification: the gateway applier is single-namespace
# and refuses any other (relay/internal/controlsync/reconcile.go).
#
# Usage:
#   ./deploy-testing-relay.sh <user@host>               # deploy + verify (idempotent)
#   ./deploy-testing-relay.sh <user@host> --bootstrap   # also create a test user + API key and
#                                                       # write the agent + gate env block to
#                                                       # .env.testing.d/bootstrap-<host>.env (0600)
#   ./deploy-testing-relay.sh <user@host> --pin-namespace <sbXXXXXXXX>
#                                                       # deploy with the gateway's sync
#                                                       # namespace pinned (post-enrollment)
#   ./deploy-testing-relay.sh <user@host> --dry-run     # validate + render locally, no ssh/scp
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
# Every locally generated secret (sync CA/leaf keys, rendered env files) is
# 0600 from creation; explicit chmod covers files copied to the box.
umask 077

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
REMOTE_DIR="/opt/sharebridge"
SVC_CONTROL="sharebridge"
SVC_GATEWAY="sharebridge-relay-gateway"
SVC_FRPS="sharebridge-relay-frps"
ENV_FILE="$SCRIPT_DIR/.env.testing"
CA_DIR="$SCRIPT_DIR/.env.testing.d"

fail() { echo "error: $*" >&2; exit 1; }

usage() {
  echo "usage: deploy-testing-relay.sh <user@host> [--bootstrap|--teardown|--dry-run] [--pin-namespace <sbXXXXXXXX>]" >&2
  exit 2
}

HOST="${1:-}"
[[ -n "$HOST" ]] || usage
shift
MODE="deploy"
DRY_RUN=0
PIN_NAMESPACE=""
while [[ $# -gt 0 ]]; do
  case "$1" in
    --bootstrap|--teardown) MODE="$1"; shift ;;
    --dry-run) DRY_RUN=1; shift ;;
    --pin-namespace)
      [[ -n "${2:-}" ]] || fail "--pin-namespace requires the agent namespace (sbXXXXXXXX)"
      PIN_NAMESPACE="$2"; shift 2 ;;
    *) usage ;;
  esac
done
HOSTNAME_ONLY="${HOST#*@}"

# §11.3 sync channel — fixed for the collocated test topology so a rename or a
# drifted address fails the static deploy gate (relay/deploy/deploy_test.sh A18).
CONTROL_SYNC_BIND_ADDR="127.0.0.1:9443"         # private numeric loopback (control config default)
CONTROL_SYNC_PORT="${CONTROL_SYNC_BIND_ADDR##*:}"
CONTROL_SYNC_SERVER_SAN="control-sync.internal" # control leaf SAN the gateway pins
GATEWAY_SYNC_CLIENT_SAN="sharebridge-relay-gateway.sync.internal" # exact client SAN control accepts
# On-box material directory. The production container topology mounts a
# repository `./control-sync` at `/run/sharebridge-sync:ro` (control/
# docker-compose.yml); this systemd test topology has no container, so the same
# directory NAME is referenced by absolute path in the env files below.
REMOTE_CONTROL_SYNC_DIR="$REMOTE_DIR/control-sync"
# GATEWAY_SYNC_NAMESPACE is the PER-AGENT namespace, generated by control at
# enrollment; it is normally UNKNOWN at deploy time (see the header). It may be
# supplied in .env.testing for a fully-live re-run, and --pin-namespace always
# wins. Empty (the default) ships the gateway DARK.
GATEWAY_SYNC_NAMESPACE="${GATEWAY_SYNC_NAMESPACE:-}"

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

# Namespace resolution: --pin-namespace overrides .env.testing; empty means the
# gateway ships dark until the operator pins it post-enrollment.
if [[ -n "$PIN_NAMESPACE" ]]; then
  GATEWAY_SYNC_NAMESPACE="$PIN_NAMESPACE"
fi

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

# §11.3 sync channel. Bind must be numeric loopback: relayctl.NewServer fails
# the control process closed on any public/unspecified address, and the
# collocated gateway is the only client, so a loopback bind is never firewalled.
[[ "$CONTROL_SYNC_BIND_ADDR" =~ ^127\.0\.0\.1:[0-9]+$ ]] || fail "CONTROL_SYNC_BIND_ADDR must be numeric loopback (127.0.0.1:<port>), got $CONTROL_SYNC_BIND_ADDR"
is_uint "$CONTROL_SYNC_PORT" || fail "CONTROL_SYNC_BIND_ADDR port must be an integer"
(( CONTROL_SYNC_PORT >= 1 && CONTROL_SYNC_PORT <= 65535 )) || fail "CONTROL_SYNC_BIND_ADDR port must be 1-65535"
[[ "$CONTROL_SYNC_SERVER_SAN" =~ ^[A-Za-z0-9]([A-Za-z0-9-]*[A-Za-z0-9])?(\.[A-Za-z0-9]([A-Za-z0-9-]*[A-Za-z0-9])?)+$ ]] || fail "CONTROL_SYNC_SERVER_SAN must be an RFC 1123 DNS name"
[[ "$GATEWAY_SYNC_CLIENT_SAN" =~ ^[A-Za-z0-9]([A-Za-z0-9-]*[A-Za-z0-9])?(\.[A-Za-z0-9]([A-Za-z0-9-]*[A-Za-z0-9])?)+$ ]] || fail "GATEWAY_SYNC_CLIENT_SAN must be an RFC 1123 DNS name"
# Empty = dark (not yet pinned). Non-empty must be exactly what control
# generates (directctl.GenerateNamespace: "sb" + 8 lowercase hex); the gateway
# applier refuses anything else (controlsync.validApplierNamespace).
if [[ "$GATEWAY_SYNC_NAMESPACE" != "" ]]; then
  [[ "$GATEWAY_SYNC_NAMESPACE" =~ ^sb[0-9a-f]{8}$ ]] || fail "GATEWAY_SYNC_NAMESPACE must be 'sb' + 8 lowercase hex (control GenerateNamespace), got $GATEWAY_SYNC_NAMESPACE"
fi

# ---- derive the gateway's verification key from the relay seed ----------------
# Control signs with RELAY_AUTH_KEY_SEED (control/internal/relayctl/
# credentials.go); the gateway holds ONLY the matching Ed25519 public key in
# SHAREBRIDGE_CONTROL_RELAY_PUBLIC_KEY (relay/cmd/gateway/main.go requires 64
# hex chars). No committed tool prints the derived key yet, so this deploy
# script derives it with the Go stdlib on the machine that already builds the
# binaries. The seed is piped to the helper over STDIN — never passed as an
# argv element, where any local user could read it via ps — and is never
# echoed; it never leaves this machine unencrypted.
PUBKEY_TMP="$(mktemp -d)"
trap 'rm -rf "$PUBKEY_TMP"' EXIT
cat > "$PUBKEY_TMP/main.go" <<'EOF'
package main

import (
	"crypto/ed25519"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"strings"
)

func main() {
	raw, err := io.ReadAll(os.Stdin)
	if err != nil {
		fmt.Fprintf(os.Stderr, "read seed from stdin: %v\n", err)
		os.Exit(1)
	}
	seed, err := hex.DecodeString(strings.TrimSpace(string(raw)))
	if err != nil || len(seed) != ed25519.SeedSize {
		fmt.Fprintln(os.Stderr, "seed must be 64 hex chars on stdin")
		os.Exit(1)
	}
	priv := ed25519.NewKeyFromSeed(seed)
	pub, _ := priv.Public().(ed25519.PublicKey)
	fmt.Println(hex.EncodeToString(pub))
}
EOF
CONTROL_PUBLIC_KEY_HEX="$(printf '%s' "$RELAY_AUTH_KEY_SEED" | go run "$PUBKEY_TMP/main.go" | tr -d '[:space:]')"
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

# ---- control↔gateway sync mTLS material (runbook §8 recipe) -------------------
# One shared sync CA + two leaves: control's server leaf (SAN =
# CONTROL_SYNC_SERVER_SAN, EKU serverAuth) and the gateway's client leaf (SAN =
# GATEWAY_SYNC_CLIENT_SAN exactly, EKU clientAuth). Generated locally under
# .env.testing.d/ (gitignored via the .env.* pattern) and REUSED across re-runs
# so a re-deploy never invalidates material already installed. The CA key never
# leaves this machine; only the CA certificate is copied to the box.
SYNC_CA_KEY="$CA_DIR/sync-ca.key"
SYNC_CA_CRT="$CA_DIR/sync-ca.crt"
CONTROL_SYNC_KEY="$CA_DIR/control-sync.key"
CONTROL_SYNC_CRT="$CA_DIR/control-sync.crt"
GATEWAY_SYNC_KEY="$CA_DIR/gateway-sync.key"
GATEWAY_SYNC_CRT="$CA_DIR/gateway-sync.crt"
if [[ ! -f "$SYNC_CA_CRT" || ! -f "$SYNC_CA_KEY" ]]; then
  openssl ecparam -genkey -name prime256v1 -noout -out "$SYNC_CA_KEY" 2>/dev/null
  openssl req -x509 -new -key "$SYNC_CA_KEY" -sha256 -days 825 \
    -subj /CN=sharebridge-sync-ca -out "$SYNC_CA_CRT" 2>/dev/null
  echo "created control↔gateway sync CA (test-only, 825d): $SYNC_CA_CRT"
fi
chmod 600 "$SYNC_CA_KEY"

# generate_sync_leaf <name> <CN> <serverAuth|clientAuth> <SAN>
# Idempotent: an existing key/cert pair is reused. SAN/EKU are pinned by the
# caller; the CA serial file is removed so no stray state is left behind.
generate_sync_leaf() {
  local name="$1" cn="$2" eku="$3" san="$4"
  local key="$CA_DIR/$name.key" crt="$CA_DIR/$name.crt"
  local csr="$CA_DIR/$name.csr" ext="$CA_DIR/$name.ext"
  if [[ -f "$crt" && -f "$key" ]]; then
    return 0
  fi
  cat > "$ext" <<EOF
basicConstraints=CA:FALSE
keyUsage=digitalSignature
extendedKeyUsage=${eku}
subjectAltName=DNS:${san}
EOF
  openssl ecparam -genkey -name prime256v1 -noout -out "$key" 2>/dev/null
  openssl req -new -key "$key" -subj "/CN=${cn}" -out "$csr" 2>/dev/null
  openssl x509 -req -in "$csr" -CA "$SYNC_CA_CRT" -CAkey "$SYNC_CA_KEY" -CAcreateserial \
    -out "$crt" -days 825 -sha256 -extfile "$ext" 2>/dev/null
  rm -f "$csr" "$ext" "$CA_DIR/sync-ca.srl"
  chmod 600 "$key"
  echo "issued sync leaf: $crt (SAN=${san}, EKU=${eku})"
}
generate_sync_leaf control-sync control-sync.internal serverAuth "$CONTROL_SYNC_SERVER_SAN"
generate_sync_leaf gateway-sync sharebridge-relay-gateway.sync.internal clientAuth "$GATEWAY_SYNC_CLIENT_SAN"

# ---- render remote config files locally (ssh stdin; every name verified
# ---- against control/internal/config/config.go Load() and relay/cmd/gateway/main.go)
# Rendering before any ssh lets --dry-run prove the exact env/mTLS wiring with
# no host contact and lets the deploy gate assert the literal file content.
CONTROL_ENV_FILE="$PUBKEY_TMP/control.env"
GATEWAY_ENV_FILE="$PUBKEY_TMP/relay-gateway.env"
FRPS_TOML_FILE="$PUBKEY_TMP/frps.toml"

cat > "$CONTROL_ENV_FILE" <<EOF
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
# §11.3 control↔gateway sync listener (task #16). The four material/identity
# variables are the switch and are written TOGETHER (a partial set refuses
# startup); the bind is numeric loopback and is never added to the firewall.
# Material is installed root:root 0600 under ${REMOTE_CONTROL_SYNC_DIR}
# (the container topology mounts ./control-sync at /run/sharebridge-sync:ro).
CONTROL_SYNC_BIND_ADDR=${CONTROL_SYNC_BIND_ADDR}
CONTROL_SYNC_CERT_FILE=${REMOTE_CONTROL_SYNC_DIR}/control-sync.crt
CONTROL_SYNC_KEY_FILE=${REMOTE_CONTROL_SYNC_DIR}/control-sync.key
CONTROL_SYNC_CLIENT_CA_FILE=${REMOTE_CONTROL_SYNC_DIR}/sync-ca.crt
CONTROL_SYNC_EXPECTED_CLIENT_SAN=${GATEWAY_SYNC_CLIENT_SAN}
EOF

{
  cat <<EOF
SHAREBRIDGE_GATEWAY_LISTEN_ADDR=:443
SHAREBRIDGE_FRP_PLUGIN_LISTEN_ADDR=${PLUGIN_LISTEN}
SHAREBRIDGE_FRP_PLUGIN_SHARED_SECRET=${SHAREBRIDGE_FRP_PLUGIN_SHARED_SECRET}
SHAREBRIDGE_CONTROL_RELAY_PUBLIC_KEY=${CONTROL_PUBLIC_KEY_HEX}
SHAREBRIDGE_RELAY_PORT_MIN=${RELAY_PORT_MIN}
SHAREBRIDGE_RELAY_PORT_MAX=${RELAY_PORT_MAX}
SHAREBRIDGE_RELAY_DATA_DIR=${REMOTE_DIR}/relay-data
EOF
  # The six sync variables are all-or-nothing (a PARTIAL set refuses gateway
  # startup, main.go configuredControlSync). They are emitted ONLY when the
  # single agent's namespace has been pinned; otherwise the gateway stays in
  # the fail-closed dark posture (route_ready=false). This is a deliberate
  # single-agent test simplification — the applier serves one namespace.
  if [[ -n "$GATEWAY_SYNC_NAMESPACE" ]]; then
    cat <<EOF
SHAREBRIDGE_CONTROL_SYNC_URL=https://${CONTROL_SYNC_BIND_ADDR}
SHAREBRIDGE_CONTROL_SYNC_SAN=${CONTROL_SYNC_SERVER_SAN}
SHAREBRIDGE_CONTROL_SYNC_CA_FILE=${REMOTE_CONTROL_SYNC_DIR}/sync-ca.crt
SHAREBRIDGE_GATEWAY_SYNC_CERT_FILE=${REMOTE_CONTROL_SYNC_DIR}/gateway-sync.crt
SHAREBRIDGE_GATEWAY_SYNC_KEY_FILE=${REMOTE_CONTROL_SYNC_DIR}/gateway-sync.key
SHAREBRIDGE_GATEWAY_NAMESPACE=${GATEWAY_SYNC_NAMESPACE}
EOF
  else
    cat <<'EOF'
# §11.3 control sync intentionally DARK: NO sync variable is set at all (a
# partial set refuses startup). Pin the enrolled agent's namespace, then
# re-run with --pin-namespace <sbXXXXXXXX>.
EOF
  fi
} > "$GATEWAY_ENV_FILE"

cat > "$FRPS_TOML_FILE" <<EOF
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

# redact_line never prints a secret value: only the three secret-bearing names
# are masked; sync paths/SANs/namespace are not secret and are shown.
redact_line() {
  case "$1" in
    CLOUDFLARE_TOKEN=*|RELAY_AUTH_KEY_SEED=*|SHAREBRIDGE_FRP_PLUGIN_SHARED_SECRET=*)
      printf '%s\n' "${1%%=*}=<redacted>" ;;
    *http://${PLUGIN_USER}:*)
      printf '%s\n' 'addr = "<redacted: embeds the frps plugin shared secret>"' ;;
    *) printf '%s\n' "$1" ;;
  esac
}

install_remote_file() {
  local local_file="$1" remote_path="$2"
  if [[ "$DRY_RUN" == "1" ]]; then
    echo "--- dry-run: would install $remote_path ---"
    while IFS= read -r line; do redact_line "$line"; done < "$local_file"
    echo "--- end $remote_path ---"
    return 0
  fi
  ssh "$HOST" "cat > $remote_path && chmod 600 $remote_path" < "$local_file"
}

if [[ "$DRY_RUN" == "1" ]]; then
  echo
  echo "=== DRY RUN: no ssh/scp performed; below is the exact rendered config ==="
  install_remote_file "$CONTROL_ENV_FILE" "$REMOTE_DIR/.env"
  install_remote_file "$GATEWAY_ENV_FILE" "$REMOTE_DIR/relay-gateway.env"
  install_remote_file "$FRPS_TOML_FILE" "$REMOTE_DIR/frps.toml"
  echo
  echo "sync material (local, gitignored): $SYNC_CA_CRT $CONTROL_SYNC_CRT $CONTROL_SYNC_KEY $GATEWAY_SYNC_CRT $GATEWAY_SYNC_KEY"
  if [[ -z "$GATEWAY_SYNC_NAMESPACE" ]]; then
    echo "sync namespace: UNPINNED — gateway would ship DARK (see --pin-namespace)"
  else
    echo "sync namespace: ${GATEWAY_SYNC_NAMESPACE}"
  fi
  echo "=== DRY RUN complete (exit 0; nothing deployed) ==="
  exit 0
fi

# ---- stop any running units BEFORE copying binaries: Linux refuses to open
# ---- a running binary for write (ETXTBSY), so an in-place upgrade must not
# ---- scp over live executables. Units are restarted below after the copy.
ssh "$HOST" "systemctl stop $SVC_FRPS $SVC_GATEWAY $SVC_CONTROL 2>/dev/null || true"

# ---- copy binaries + assets + mTLS material ------------------------------------
ssh "$HOST" "mkdir -p $REMOTE_DIR/web $REMOTE_DIR/relay-bin $REMOTE_DIR/frps $REMOTE_DIR/relay-transport $REMOTE_DIR/relay-data $REMOTE_CONTROL_SYNC_DIR && chmod 700 $REMOTE_DIR/relay-data $REMOTE_DIR/relay-transport $REMOTE_CONTROL_SYNC_DIR"
scp -q "$BIN_CONTROL" "$HOST:$REMOTE_DIR/server"
scp -q "$BIN_GATEWAY" "$HOST:$REMOTE_DIR/relay-bin/gateway"
scp -q "$FRPS_BIN" "$HOST:$REMOTE_DIR/frps/frps"
scp -q "$LEAF_CRT" "$HOST:$REMOTE_DIR/relay-transport/frps.crt"
scp -q "$LEAF_KEY" "$HOST:$REMOTE_DIR/relay-transport/frps.key"
# §11.3 sync material: the shared CA certificate and both leaves. The CA
# PRIVATE key never leaves this machine. Everything lands root:root 0600 under
# the same-named `control-sync` directory the container topology mounts
# read-only at /run/sharebridge-sync.
scp -q "$SYNC_CA_CRT" "$HOST:$REMOTE_CONTROL_SYNC_DIR/sync-ca.crt"
scp -q "$CONTROL_SYNC_CRT" "$HOST:$REMOTE_CONTROL_SYNC_DIR/control-sync.crt"
scp -q "$CONTROL_SYNC_KEY" "$HOST:$REMOTE_CONTROL_SYNC_DIR/control-sync.key"
scp -q "$GATEWAY_SYNC_CRT" "$HOST:$REMOTE_CONTROL_SYNC_DIR/gateway-sync.crt"
scp -q "$GATEWAY_SYNC_KEY" "$HOST:$REMOTE_CONTROL_SYNC_DIR/gateway-sync.key"
# Control web assets: startup fails closed without the Task 22
# route-interstitial.* assets (directctl.LoadInterstitialAssets("./web")).
scp -q "$SCRIPT_DIR/web/home.html" "$SCRIPT_DIR/web/login.html" \
       "$SCRIPT_DIR/web/register.html" "$SCRIPT_DIR/web/account.html" \
       "$SCRIPT_DIR/web/route-interstitial.html" "$SCRIPT_DIR/web/route-interstitial.js" \
       "$SCRIPT_DIR/web/route-interstitial.css" \
       "$HOST:$REMOTE_DIR/web/"
ssh "$HOST" "chmod 600 $REMOTE_DIR/relay-transport/frps.key $REMOTE_DIR/relay-transport/frps.crt $REMOTE_CONTROL_SYNC_DIR/sync-ca.crt $REMOTE_CONTROL_SYNC_DIR/control-sync.crt $REMOTE_CONTROL_SYNC_DIR/control-sync.key $REMOTE_CONTROL_SYNC_DIR/gateway-sync.crt $REMOTE_CONTROL_SYNC_DIR/gateway-sync.key"

# ---- install the locally rendered config (ssh stdin; secrets never on argv) ----
install_remote_file "$CONTROL_ENV_FILE" "$REMOTE_DIR/.env"
install_remote_file "$GATEWAY_ENV_FILE" "$REMOTE_DIR/relay-gateway.env"
install_remote_file "$FRPS_TOML_FILE" "$REMOTE_DIR/frps.toml"

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
ss -ltn | awk 'NR>1 {print \$4}' | grep -E '(:443|:${TRANSPORT_PORT}|:${PORT}|:${CONTROL_SYNC_PORT})\$' | sort -u | sed 's/^/tcp listening: /'
ss -lun | awk 'NR>1 {print \$4}' | grep -E ':3478\$' | sort -u | sed 's/^/udp listening: /'
ss -ltn | awk 'NR>1 {print \$4}' | grep -E ':${CONTROL_SYNC_PORT}\$' | sort -u | sed 's/^/sync listener: /'
if ! ss -ltn | awk 'NR>1 {print \$4}' | grep -qE '^127\\.0\\.0\\.1:${CONTROL_SYNC_PORT}\$'; then echo 'WARNING: control sync listener is not loopback-only (should be 127.0.0.1:${CONTROL_SYNC_PORT})'; fi
journalctl -u $SVC_CONTROL -n 10 --no-pager -o cat | grep -F 'control sync listener on' | tail -n 1 | sed 's/^/control sync: /'
journalctl -u $SVC_CONTROL -n 3 --no-pager -o cat | sed 's/^/control: /'
journalctl -u $SVC_GATEWAY -n 3 --no-pager -o cat | sed 's/^/gateway: /'
journalctl -u $SVC_FRPS -n 3 --no-pager -o cat | sed 's/^/frps: /'
curl -sf http://127.0.0.1:9101/healthz 2>/dev/null | sed 's/^/gateway health: /' || echo 'gateway health: unavailable (private loopback 127.0.0.1:9101)'
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
10. sync listener (private): on the box, ss -ltn | grep ${CONTROL_SYNC_PORT} must show 127.0.0.1:${CONTROL_SYNC_PORT}, never 0.0.0.0; control logs 'control sync listener on ${CONTROL_SYNC_BIND_ADDR} (private mTLS)'
11. sync mTLS material: on the box, ls -ld ${REMOTE_CONTROL_SYNC_DIR} is root 0700 and every file in it is root:root 0600; /run/sharebridge-sync is the container mount of the same directory (container topology only)
12. gateway sync channel (after --pin-namespace): on the box, curl -sf http://127.0.0.1:9101/healthz shows {"route_ready":true,...} for THIS agent's namespace (the observable; the mTLS listener is loopback-private and never curl'd directly)
13. namespace pin: SHAREBRIDGE_GATEWAY_NAMESPACE must equal the ENROLLED agent's namespace; a mismatched value makes the gateway reject every route (fail-closed)
Control logs:    ssh $HOST journalctl -u $SVC_CONTROL -f
Gateway logs:    ssh $HOST journalctl -u $SVC_GATEWAY -f
frps logs:       ssh $HOST journalctl -u $SVC_FRPS -f
EOF

if [[ -z "$GATEWAY_SYNC_NAMESPACE" ]]; then
  echo
  echo "=== control↔gateway sync channel: DARK (namespace not pinned) ==="
  echo "SHAREBRIDGE_GATEWAY_NAMESPACE is the PER-AGENT §6 namespace that control"
  echo "GENERATES at enrollment (deliberate single-agent test simplification: this"
  echo "collocated gateway serves exactly ONE agent's namespace). It is not known"
  echo "at deploy time, so the gateway ships with NO sync variables at all (the"
  echo "fail-closed dark posture; a partial set would refuse startup)."
  echo "After the agent enrolls, read the namespace from control's admin UI"
  echo "  $BOX_HTTP/_/  ->  collections -> agents -> namespace"
  echo "or from the enrolled agent's persisted config, then re-run:"
  echo "  ./deploy-testing-relay.sh $HOST --pin-namespace sbXXXXXXXX"
  echo "Until then route_ready stays false and every relay-dependent scenario"
  echo "fails closed."
fi

# ---- bootstrap: throwaway user + API key + agent/gate env file ----------------
# The env block contains live secrets (API key), so it is written to a
# chmod-600 file inside the gitignored .env.testing.d/ directory and ONLY the
# file path is printed — secrets must not land in terminal scrollback or
# shell history.
if [[ "$MODE" == "--bootstrap" ]]; then
  EMAIL="test-$(openssl rand -hex 4)@sharebridge.app"
  PASSWORD="$(openssl rand -hex 16)"
  curl -sf -X POST "$BOX_HTTP/api/collections/users/records" -H 'Content-Type: application/json' \
    -d "{\"email\":\"$EMAIL\",\"password\":\"$PASSWORD\",\"passwordConfirm\":\"$PASSWORD\"}" >/dev/null
  JWT="$(curl -sf -X POST "$BOX_HTTP/api/collections/users/auth-with-password" -H 'Content-Type: application/json' \
    -d "{\"identity\":\"$EMAIL\",\"password\":\"$PASSWORD\"}" | python3 -c 'import sys,json;print(json.load(sys.stdin)["token"])')"
  APIKEY="$(curl -sf -X POST "$BOX_HTTP/api/keys" -H "Authorization: Bearer $JWT" -H 'Content-Type: application/json' \
    -d '{"label":"testing"}' | python3 -c 'import sys,json;print(json.load(sys.stdin)["key"])')"
  BOOTSTRAP_ENV="$CA_DIR/bootstrap-${HOSTNAME_ONLY}.env"
  umask 077
  cat > "$BOOTSTRAP_ENV" <<EOF
# Generated by deploy-testing-relay.sh --bootstrap for ${HOSTNAME_ONLY}.
# chmod 600; .env.testing.d/ is gitignored (.env.*). Never commit or paste.
# --- agent env (agent/.env on the home server; runbook §5) ---
SIGNALING_SERVER=ws://${HOSTNAME_ONLY}:${PORT}
SHAREBRIDGE_API_KEY=${APIKEY}
CONTENT_BASE_DOMAIN=${BASE_DOMAIN}
ALLOWED_SHAREBRIDGE_HOST=demo.example.com   # or your WebDAV host
# Exact interstitial origin of the TEST deployment; the agent connect check
# compares byte-exactly (test-only override — production keeps the default).
CONNECT_ALLOWED_ORIGIN=http://${HOSTNAME_ONLY}:${PORT}
# --- relay transport CA (copy to the home server, then into the container; runbook §5) ---
# scp ${CA_PEM} <homeserver>:~/sharebridge/agent/relay-transport-ca.pem
# --- Task 25 STUN gate env (run from the home network; runbook §8) ---
STUN_GATE_API_KEY=${APIKEY}   # env-only, never a flag
# scripts/stun-nat-gate.sh --target remote --server ws://${HOSTNAME_ONLY}:${PORT} --stun-addr ${STUN_ADVERTISE} --expected-public-ip <your-home-egress-IPv4>
# Optional fast path: STUN_GATE_CERT_FINGERPRINT=<fingerprint of the already-enrolled agent cert> skips CSR re-issuance.
EOF
  chmod 600 "$BOOTSTRAP_ENV"
  echo
  echo "=== bootstrap complete (throwaway user + API key created) ==="
  echo "agent + gate env written to: $BOOTSTRAP_ENV (chmod 600; contents are NOT printed)"
fi
