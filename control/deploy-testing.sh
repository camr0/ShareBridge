#!/usr/bin/env bash
#
# deploy-testing.sh — deploy the ShareBridge signaling server to a throwaway test box.
#
# The signaling server is a single static Go binary (pure-Go sqlite, no cgo),
# so deploying is just: cross-compile → scp binary + web assets → write a
# systemd unit → start. No Docker or runtime dependencies on the box.
#
# Usage:
#   ./deploy-testing.sh <user@host>               # build, copy, systemd, start (idempotent)
#   ./deploy-testing.sh <user@host> --bootstrap   # also create a test user + API key, print agent env
#   ./deploy-testing.sh <user@host> --teardown    # stop + disable the service
#
# This script deploys the CONTROL plane only. The phase-4a production relay
# runs on its own VM (§4.6) and is provisioned with `relay/deploy/install.sh`;
# see docs/operations/phase4a-relay.md. Relay-only material (the FRP transport
# certificate, the gateway control-sync client certificate) never belongs on
# the control box, and no content certificate/ACME credential belongs on the
# relay VM.
#
# Secrets: reads CLOUDFLARE_TOKEN from .env.testing (gitignored). The token is
# forwarded to the box but NEVER echoed to stdout.
#
# Setup (one-time, on your machine):
#   cat > control/.env.testing <<'EOF'
#   CLOUDFLARE_TOKEN=<your-dns-edit-token>
#   EOF
#   ./deploy-testing.sh root@178.x.x.x --bootstrap
set -euo pipefail

HOST="${1:?usage: deploy-testing.sh <user@host> [--bootstrap|--teardown]}"
MODE="${2:-deploy}"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REMOTE_DIR="/opt/sharebridge"
SERVICE="sharebridge"
ENV_FILE="$SCRIPT_DIR/.env.testing"
HOSTNAME_ONLY="${HOST#*@}"

# ---- teardown ----
if [[ "$MODE" == "--teardown" ]]; then
  ssh "$HOST" "systemctl disable --now $SERVICE 2>/dev/null || true; echo 'stopped — delete the box in the Hetzner console to stop billing'"
  exit 0
fi

# ---- secrets + config (token sourced, never echoed) ----
[[ -f "$ENV_FILE" ]] || { echo "error: $ENV_FILE not found (needs: CLOUDFLARE_TOKEN=...)" >&2; exit 1; }
# shellcheck disable=SC1090
source "$ENV_FILE"
: "${CLOUDFLARE_TOKEN:?CLOUDFLARE_TOKEN not set in $ENV_FILE}"

BASE_DOMAIN="${CONTENT_BASE_DOMAIN:-sharebridgeusercontent.com}"
ACME_EMAIL="${ACME_EMAIL:-ali@sharebridge.app}"
# Production CA by default: the agent validates against the system trust store,
# so Let's Encrypt staging certs (untrusted staging root) are rejected.
ACME_CA="${ACME_CA_DIR:-https://acme-v02.api.letsencrypt.org/directory}"
PORT="${PORT:-8080}"
RELAY_SECRET="$(openssl rand -base64 32)"

# ---- build (pure-Go, no cgo needed) ----
BIN="/tmp/sharebridge-server-linux"
( cd "$SCRIPT_DIR" && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o "$BIN" ./cmd/server )
echo "built $BIN"

# ---- copy binary + web assets (four retained control-hosted pages + the
# ---- Task 22 route-interstitial assets main.go loads from ./web at startup;
# ---- startup fails closed without them) ----
ssh "$HOST" "mkdir -p $REMOTE_DIR/web"
scp -q "$BIN" "$HOST:$REMOTE_DIR/server"
scp -q "$SCRIPT_DIR/web/home.html" "$SCRIPT_DIR/web/login.html" \
       "$SCRIPT_DIR/web/register.html" "$SCRIPT_DIR/web/account.html" \
       "$SCRIPT_DIR/web/route-interstitial.html" "$SCRIPT_DIR/web/route-interstitial.js" \
       "$SCRIPT_DIR/web/route-interstitial.css" \
       "$HOST:$REMOTE_DIR/web/"

# ---- remote .env (token forwarded via ssh stdin, never echoed) ----
ssh "$HOST" "cat > $REMOTE_DIR/.env" <<EOF
CLOUDFLARE_TOKEN=${CLOUDFLARE_TOKEN}
CONTENT_BASE_DOMAIN=${BASE_DOMAIN}
ACME_EMAIL=${ACME_EMAIL}
ACME_CA_DIR=${ACME_CA}
RELAY_JWT_SECRET=${RELAY_SECRET}
PORT=${PORT}
DATA_DIR=${REMOTE_DIR}/pb_data
# Private monitoring listener (Task 34/35): loopback-only by design, never
# firewalled, never scraped over the public interface.
CONTROL_METRICS_ADDR=127.0.0.1:9102
EOF

# ---- systemd unit (idempotent) ----
UNIT=$(cat <<EOF
[Unit]
Description=ShareBridge Signaling Server (testing)
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
UNIT_B64="$(printf '%s' "$UNIT" | base64)"
ssh "$HOST" "printf '%s' '$UNIT_B64' | base64 -d > /etc/systemd/system/$SERVICE.service && systemctl daemon-reload && systemctl enable --now $SERVICE"

# ---- wait for health ----
BOX_HTTP="http://${HOSTNAME_ONLY}:${PORT}"
for _ in {1..20}; do
  if curl -sf -o /dev/null "$BOX_HTTP/_/" 2>/dev/null; then break; fi
  sleep 1
done
echo "deployed + listening at $BOX_HTTP (service: $SERVICE)"

# ---- bootstrap a throwaway user + API key ----
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
  echo "=== agent env (run on the agent host) ==="
  echo "export SIGNALING_SERVER=ws://${HOSTNAME_ONLY}:${PORT}"
  echo "export SHAREBRIDGE_API_KEY=${APIKEY}"
  echo "export CONTENT_BASE_DOMAIN=${BASE_DOMAIN}"
  echo "export ALLOWED_SHAREBRIDGE_HOST=demo.example.com   # or your WebDAV host"
fi
