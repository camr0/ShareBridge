# ShareBridge relay

The `relay/` module is the public layer-4 SNI gateway and FRP server for the
Phase 4a relay path. It is a separate Go module (no root `go.mod`/`go.work`);
run every command from inside `relay/`.

## What runs here

- `sharebridge-relay-gateway` (`cmd/gateway`) — the public `:443/tcp` L4
  gateway. It peeks the ClientHello to read the SNI, routes by exact
  control-distributed hostname, and splices the byte stream onto the agent's
  loopback FRP proxy port. Browser TLS is **never** terminated here. The same
  process exposes a loopback-only FRP authorization plugin (`:9001`) and a
  loopback-only metrics/health endpoint (`:9101`).
- `frps` — the FRP server (pinned release, not committed). It owns the public
  FRP transport port and the loopback-only agent proxy range, and it calls the
  gateway's authorization plugin on every login/proxy/heartbeat.

Layout:

| Path | Contents |
|---|---|
| `cmd/gateway` | gateway binary (flags via environment, §14 bounds) |
| `internal/clienthello` | fragmented/oversized ClientHello SNI parser |
| `internal/gateway` | acceptor, router, splice, health truths |
| `internal/routes` | control-distributed exact route table |
| `internal/presence` | gateway-authoritative tunnel presence/leases |
| `internal/frpplugin` | fail-closed FRP authorization plugin |
| `internal/controlsync` | mTLS control↔gateway snapshot/delta sync |
| `internal/limits` | §14 resource bounds and accounting |
| `internal/metrics` | §17.3 Prometheus signals, private-only handler |
| `config/frps.toml` | committed root-owned frps config template |
| `frp/manifest.json` | pinned FRP release + SHA-256 digests |
| `scripts/fetch-frp.sh` | checksum-verified FRP fetch/stage |
| `deploy/` | systemd units, nftables ruleset, installer, deploy test |

## Build, test, verify

```bash
cd relay
go test ./... -count=1
go vet ./...
go build ./cmd/gateway

# Deployment hardening gate (no root/systemd needed):
bash deploy/deploy_test.sh

# Real-FRP hermetic §23.3 gate (downloads the pinned FRP release):
SHAREBRIDGE_FRP_INTEGRATION=1 go test ./internal/integration -count=1
```

FRP binaries are never committed. `scripts/fetch-frp.sh` downloads exactly the
release recorded in `frp/manifest.json`, verifies the SHA-256, and stages
`frps`/`frpc` into a digest-keyed cache.

## Deploy the separate relay VM

The production MVP runs on its own small VM in the same region/private network
as control (spec §4.6). Install and harden it with:

```bash
relay/deploy/install.sh --tunnel-host <relay-tunnel-host> \
  --sync-url <https://control-sync-endpoint> --sync-san <control-sync-san> \
  --namespace sb0123abcd --sync-ca control-ca.crt \
  --sync-cert gateway-sync.crt --sync-key gateway-sync.key \
  --transport-cert tunnel-server.crt --transport-key tunnel-server.key
```

Secrets are supplied through the environment or root-only files and are
installed `root:root 0600`; the units read them through systemd credentials.
Only `install.sh --dry-run` prints the plan (never a secret).

The full operator runbook — ports, hardening inventory, restart ordering, DNS
audit commands, §14/§17 environment reference, and known open items — is
[`docs/operations/phase4a-relay.md`](../docs/operations/phase4a-relay.md).

## Security invariants

- Public TCP is exactly `443` (gateway) and the FRP transport port; proxy,
  plugin, metrics and admin surfaces are loopback/private only.
- The relay VM holds a dedicated FRP transport certificate and the gateway
  control-sync client certificate. It never holds an agent content
  certificate/key, a DNS/ACME credential, or any share secret.
- Both services run as distinct unprivileged users under `ProtectSystem=strict`
  with explicit state/run directories, bounded `LimitNOFILE`/`MemoryMax`, and
  a bounded journal rate.
- The `sharebridgeusercontent.com` zone must never publish HTTPS/SVCB (ECH)
  records: hidden SNI would silently break exact routing. `install.sh
  --audit-dns` proves the invariant.
