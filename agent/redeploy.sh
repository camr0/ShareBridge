#!/usr/bin/env bash

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SOURCE_DIR="${SOURCE_DIR:-$SCRIPT_DIR}"
REMOTE_HOST="${REMOTE_HOST:-ali@homeserver}"
REMOTE_DIR="${REMOTE_DIR:-/home/ali/sharebridge/agent}"

require_command() {
  if ! command -v "$1" >/dev/null 2>&1; then
    echo "missing required command: $1" >&2
    exit 1
  fi
}

validate_source_dir() {
  if [[ ! -f "$SOURCE_DIR/Dockerfile" || ! -f "$SOURCE_DIR/docker-compose.yml" ]]; then
    echo "source dir does not look like agent root: $SOURCE_DIR" >&2
    exit 1
  fi
}

run_redeploy() {
  require_command rsync
  require_command ssh
  validate_source_dir

  echo "Redeploying agent"
  echo "  source: $SOURCE_DIR/"
  echo "  target: $REMOTE_HOST:$REMOTE_DIR/"

  rsync -avz --delete \
    --exclude=.env \
    --exclude=.env.* \
    --exclude=.git/ \
    --exclude=.DS_Store \
    --exclude=agent.log \
    "$SOURCE_DIR/" "$REMOTE_HOST:$REMOTE_DIR/"

  ssh "$REMOTE_HOST" \
    "cd '$REMOTE_DIR' && docker compose build --no-cache && docker compose up -d"

  ssh "$REMOTE_HOST" \
    "cd '$REMOTE_DIR' && docker compose logs --tail=20"
}

main() {
  run_redeploy
}

if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then
  main "$@"
fi
