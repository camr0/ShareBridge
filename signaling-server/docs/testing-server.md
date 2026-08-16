# Testing server

Spin up a throwaway signaling-server box for live end-to-end testing (agent on the home Mac, server on a VPS).

## One-time setup

```bash
cat > signaling-server/.env.testing <<'EOF'
CLOUDFLARE_TOKEN=<your-dns-edit-token>
EOF
```

`.env.testing` is gitignored (`.env.*`). It holds **only** the token; everything else is generated with sane defaults by the script.

## Deploy

```bash
cd signaling-server
./deploy-testing.sh root@<box-ip> --bootstrap
```

This cross-compiles the server (pure-Go, no cgo), `scp`s the binary + `web/` + a generated `.env`, installs the `sharebridge` systemd service (idempotent), creates a throwaway test user + API key, and prints the agent env:

```bash
export SIGNALING_SERVER=ws://<box-ip>:8080
export SHAREBRIDGE_API_KEY=<printed key>
export CONTENT_BASE_DOMAIN=sharebridgeusercontent.com
export ALLOWED_SHAREBRIDGE_HOST=demo.example.com   # lets /api/v1/shares pass CORS
```

Run the agent with that env (fresh `$HOME` to avoid stale namespace/key), register a share, then hit `http://<box-ip>:8080/s/<code>`.

## Teardown

```bash
./deploy-testing.sh root@<box-ip> --teardown
```

…then **delete the box in the Hetzner console** — `poweroff` does *not* stop billing (allocation-based). Snapshot the clean base first so the next box restores in ~30s.

## Notes

- ACME defaults to the **production** CA — the agent validates against the system trust store, so Let's Encrypt *staging* certs are rejected.
- The server is a single static binary; deployment is `scp` + systemd, no Docker.
