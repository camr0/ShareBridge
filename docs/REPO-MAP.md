# ShareBridge repository map

Where things live, what each seam is, and how to run the gates.

Scope: the `phase-4a` branch of the ShareBridge monorepo (relay MVP).
Every path, command and package named here was checked to exist at the
branch HEAD when this file was written; line numbers are deliberately not
quoted because they drift. When in doubt, run the gate — do not trust prose.

The repo has **four Go modules** plus a browser test tree:

| Module | Path | `go.mod` module | Purpose |
|---|---|---|---|
| agent | `agent/` | `sharebridge/agent` | Private-network agent: talks to OpenCloud/Immich, exposes shares directly (PnP/DDNS) or through the relay tunnel |
| control | `control/` | `sharebridge/control` | Signaling, accounts, browser app, direct-route decisioning, and the private sync server for the gateway |
| relay | `relay/` | `sharebridge/relay` | Public L4 relay gateway + FRP plugin + the separate-VM deployment artifacts |
| integration (test-only) | `integration/controlsync/` | `sharebridge/control/integration` | Cross-module proof that the *real* gateway sync loop drives the *real* control listener |
| browser e2e | `e2e/browser/` | npm | Playwright route-flow gate against a hermetic fixture |

## 1. Module map

### 1.1 `agent` — private-network agent

Entry points (`agent/cmd/`):

- `cmd/agent` — the shipped agent binary (cobra; loads config, store, web UI, daemon).
- `cmd/spike-*` — throwaway spike commands (`spike-certagent`, `spike-e2e`, `spike-map`, `spike-upnp`); not production.
- `frpc/` — gitignored staging directory for the checksum-pinned frpc binary; no binary is committed.

Key `internal/` packages:

| Package | Purpose (one line) |
|---|---|
| `internal/config` | Config struct + env parsing, including the admin-UI bind/password rules |
| `internal/store` | Durable agent state (sessions/shares) behind the daemon |
| `internal/daemon` | The orchestrator: signaling loop, share lifecycle, direct/relay selection, lockdown and revocation |
| `internal/signaling` | WebSocket client and message shapes for agent↔control, backoff, and the OK-ack transport seal |
| `internal/tunnel` | Supervises the pinned frpc child process and its presence lifecycle |
| `internal/direct` | Direct data plane: SNI binder, HTTPS server, PnP/NAT-PMP port mapping, on-demand mapping state machine, endpoint reporter, connection registry |
| `internal/cert` | Agent-side CSR generation, certificate manager and validation for tunnel/content origins |
| `internal/stun` | Agent half of the control-observed STUN check (authenticated Binding exchange) |
| `internal/immich` | Immich API client |
| `internal/cloudwebdav` | OpenCloud WebDAV client |
| `internal/web` | Agent admin UI + `/api/v1` (API-key) surface, templates and static assets |

### 1.2 `control` — signaling, direct decisioning, sync server

Entry points (`control/cmd/`):

- `cmd/server` — the control server: PocketBase-backed API/browser app, agent WebSocket, direct-route decisioning, STUN listener, the private relayctl sync listener, metrics.
- `cmd/admin` — control admin CLI.
- `cmd/spike-*` — spike commands (`spike-cert`, `spike-ddns`, `spike-dnsset`); not production.

Key `internal/` packages:

| Package | Purpose (one line) |
|---|---|
| `internal/config` | Control config incl. the five `CONTROL_SYNC_*` variables (all-or-nothing) |
| `internal/handler` | HTTP/WS handlers, including the agent WebSocket (`agent_ws.go`) |
| `internal/hub` | In-memory agent connection hub used by signaling |
| `internal/middleware` | API-key and related request middleware |
| `internal/directctl` | Direct-route logic: eligibility predicate, route selection, prepare-route, open-signal, probe, endpoint facts, interstitial |
| `internal/stun` | Control's authenticated STUN observation listener (one-use challenge/credential) |
| `internal/ddns` | Cloudflare DDNS updates for content/hostname records |
| `internal/certcoordinator` | ACME coordination for per-agent wildcard certificates |
| `internal/relayctl` | The `§11.3` control-side sync surface: protocol, mTLS server, route publisher/deltas, presence view, relay credentials |

### 1.3 `relay` — public L4 gateway and deployment

Entry point: `relay/cmd/gateway` — accepts browser TLS without terminating it, routes by exact ClientHello SNI, splices to agent loopback FRP ports, and serves the loopback-only FRP authorization plugin, health, and metrics.

Key `internal/` packages:

| Package | Purpose (one line) |
|---|---|
| `internal/gateway` | Public L4 acceptor + stream pump (limits enforced before/at admission) |
| `internal/clienthello` | Bounded TLS ClientHello parser that extracts the exact SNI server name |
| `internal/routes` | Gateway's authoritative in-memory route table (epochs, revisions, tombstones, capacity) |
| `internal/controlsync` | Gateway's client for the control↔gateway sync: fetch/apply snapshot+deltas, status ack, presence publisher, reconcile loop |
| `internal/presence` | Authoritative tunnel presence registry (credential-precise lease state, boot/revision fencing) |
| `internal/frpplugin` | Local fail-closed HTTP authorization plugin for frps (Login/NewProxy/CloseProxy/Ping/NewUserConn) |
| `internal/limits` | `§14` resource bounds: config, acquire/release around the listener |
| `internal/metrics` | `§17.3` metadata-only metric registry, health split, private-only handler, NIC sampler |
| `internal/frptest` | Pinned FRP artifact verification (`§23.1`/`§23.2`/`§23.7`) |
| `internal/integration` | Real-FRP `§23.3` parity/recovery gate (`*_test.go` except `load_test.go`) + `§23.8` capacity baseline (`load_test.go`, its own gate) |

Deployment artifacts: `relay/deploy/` (`sharebridge-relay-gateway.service`, `sharebridge-relay-frps.service`, `firewall.nft`, `install.sh`, `deploy_test.sh`), `relay/config/frps.toml`, `relay/frp/manifest.json` + `GATE-EVIDENCE.md`, `relay/scripts/fetch-frp.sh`.

### 1.4 `integration/controlsync` — cross-module ack test (test-only module)

Not shipped; exists to run both halves for real.

- Control half runs in-process (real `relayctl.Server`/`Publisher`/`PresenceView`) over a real mTLS listener.
- Gateway half is a throwaway subprocess module built from `gatewayharness/main.go.txt`, importing the real `controlsync` client/applier/loop/presence publisher.
- See `integration/controlsync/README.md` for the Go internal-package constraint that forces the split.

## 2. Protocol surfaces

### 2.1 agent ↔ control WebSocket

- Agent side: `agent/internal/signaling/client.go` (transport + message types), `agent/internal/daemon/daemon.go` (handlers).
- Control side: `control/internal/handler/agent_ws.go` (endpoint), `control/internal/hub/hub.go`.
- Message types in use: `hello`, `enrolled`, `enrollment_ready`, `tls_ready`, `relay_config`, `relay_credential_request`, `stun_challenge`, `stun_result`, `open_signal`, `open_ack`, `report_endpoint`, `relay_client_state`, `lockdown_status`.
- Invariants worth knowing: `status:"ok"` `open_ack`s are sealed at the transport (`agent/internal/signaling`) — no OK ack reaches the wire without the daemon's generation/port guard approving; unknown JSON fields are tolerated by design.
- Tests: `control/internal/handler/agent_ws_test.go`, `control/internal/handler/e2e_test.go`; `agent/internal/signaling/client_test.go`, `agent/internal/signaling/client_open_ack_seal_test.go`; `agent/internal/daemon/daemon_*_test.go`.

### 2.2 control ↔ gateway private mTLS sync (`§11.3`)

- Control side: `control/internal/relayctl/` — `protocol.go` (wire), `server.go` (mTLS endpoints `/internal/relay/v1/{snapshot,deltas,status,presence/snapshot,presence/events}`), `publisher.go` (epoch/revision, tighten-only limits, `published_at`), `presence.go`, `credentials.go`.
- Gateway side: `relay/internal/controlsync/` — `client.go`, `reconcile.go` (applier; `Reconcile`, never raw deltas), `loop.go` (sync loop + ack watermark health), `presence.go` (≤60 s presence republish).
- Contracts:
  - snapshot / delta / status-ack; ack is `acknowledged:true|false` with a bounded `reason` (e.g. `foreign_epoch`); the gateway records a watermark only on genuine acceptance.
  - presence envelope carries top-level `gateway_boot_id` and `revision` so a fresh (or empty) snapshot is adoptable.
- Tests: `control/internal/relayctl/*_test.go` (incl. `sync_e2e_test.go`), `relay/internal/controlsync/*_test.go`, and the cross-module module `integration/controlsync/crossmodule_test.go`.

### 2.3 Relay data path (via frps)

- Gateway: `relay/internal/gateway` (accept/splice), `relay/internal/clienthello` (SNI), `relay/internal/routes` (table), `relay/internal/frpplugin` (authorization), `relay/internal/presence` (availability).
- Agent: `agent/internal/tunnel` (frpc supervisor + fresh-credential recovery), pinned binary via `relay/frp/manifest.json`.
- frps config: `relay/config/frps.toml`.
- Tests: `relay/internal/integration/*` (real frps/frpc; `§23.3` gate cases + gate guards, and `§23.8` capacity suite in `load_test.go` with its own gate), `relay/internal/frptest/*` (artifact/config gate), `relay/internal/frpplugin/*_test.go`, `agent/internal/tunnel/*_test.go`.

### 2.4 Direct data path (PnP + DDNS)

- Agent: `agent/internal/direct` — `portmap.go` (UPnP/NAT-PMP with bounded router I/O), `ondemand.go` (mapping state machine + generation fence), `server.go` (HTTPS content gate), `sni.go` (binder), `reporter.go` (endpoint reports), `connections.go`; plus `agent/internal/cert` and `agent/internal/stun`.
- Control: `control/internal/directctl` — `directpredicate.go`, `routes.go`, `prepare.go`, `opensignal.go`, `probe.go`, `endpoint.go`, `stun.go`; `control/internal/ddns/ddns.go`; STUN listener `control/internal/stun/server.go`.
- Tests: `agent/internal/direct/*_test.go`, `agent/internal/daemon/daemon_direct_test.go`, `control/internal/directctl/*_test.go`, `control/internal/stun/server_test.go`.

## 3. Gates and how to run them

### 3.1 `§23.3` FRP required-mode gate (BLOCKING)

```bash
cd relay && SHAREBRIDGE_FRP_INTEGRATION=1 SHAREBRIDGE_FRP_GATE=required \
  go test -v -race -count=1 -timeout 900s -skip '^TestRelayCapacity' ./internal/integration
```

Proves: byte-exact TLS parity through the *real pinned* frps/frpc for TLS 1.2/HTTP1.1,
TLS 1.3/HTTP2, and a fragmented ClientHello (`extraAtAgent=0`), plus routing, presence and
restart/recovery cases — currently **16 named cases**. Required mode fails closed (non-zero
exit, no case started) when the integration env is unset, the run is `-short`, a case is
skipped or never started, or the pinned artifacts are missing/poisoned. Evidence and digest
table: `relay/frp/GATE-EVIDENCE.md`.

The `-skip '^TestRelayCapacity'` scope keeps the package's `§23.8` capacity/safety suite
(`load_test.go`) out of this BLOCKING gate's exit code. Those cases are timing-shaped and
non-blocking, and they are already covered by their own gate (`§3.6`,
`scripts/relay-capacity-gate.sh`); before the scope was added, a capacity flake made the
gate exit non-zero while its own verdict printed all 16 gate cases green, so an exit-1 run
could not be attributed to a genuine blocking failure. The scope cannot weaken the gate:
required mode asserts every `requiredGateCases` entry started and completed, and the
pattern is pinned by `gateCapacitySkipPattern` in `relay/internal/integration/gate_test.go`
with `TestGateScheduleScopesOutOnlyTheCapacitySuite` (itself not a capacity case, so it runs
under the gate command) failing on any drift.

### 3.2 Deployment gate

```bash
bash relay/deploy/deploy_test.sh
```

Proves: the separate-VM relay artifacts are hardened and exact — distinct unprivileged
service users, root-owned `0600` secrets, systemd sandbox directives with expected values,
the public TCP allowlist being exactly `{443, transport}`, integrity-checked pinned frps,
pinned `ExecStart`/`User`/`Group`, and the collocated test-VPS `CONTROL_SYNC_*` wiring.
Prints `== N checks, M failure(s) ==` and `RESULT: GREEN|RED`; exit 0 only on GREEN.
Static only: it does not need systemd, root, or a relay host.

### 3.3 M6 acceptance harness (Task 37 skeleton)

```bash
scripts/live-phase4a.sh                 # all registered gates
scripts/live-phase4a.sh --case <name>   # one gate
scripts/live-phase4a.sh --list          # print the gate table
scripts/live-phase4a.sh --dry-run       # print config + plan; runs nothing
scripts/live-phase4a.sh --selftest      # verify the pass/fail plumbing
```

16 registered gates: `gate_23_9_dark_topology`, `acceptance_01_owner_hairpin`,
`acceptance_04_interstitial_blackhole`, `acceptance_02_cellular_relay_video`,
`acceptance_11_content_parity`, `acceptance_03_relay_only`, `l4_no_plaintext_capture`,
`acceptance_06_no_mapper_enrollment`, `acceptance_12_stun_mismatch`,
`acceptance_13_stun_cadence_cold_budget`, `acceptance_07_no_relay_open_signal`,
`acceptance_08_exact_routing`, `acceptance_09_restart_recovery`, `acceptance_10_lockdown`,
`acceptance_15_heartbeat_tunnel_dns`, `release_go_no_go_rollback`.

Exit codes: `0` GREEN, `1` RED (FAIL / MISSING / NOT_IMPLEMENTED), `2` usage error,
`3` PARTIAL (at least one SKIP, or a dry run — nothing executed is never a pass).

Honesty rules (enforced by `--selftest`): a gate with only `note()` lines is `MISSING`,
not PASS; a registered-but-unimplemented gate is `NOT_IMPLEMENTED`, never PASS; check
details are sanitised before console and evidence; the dark-posture selection flag must be
*observed*, never assumed from a code default.

`acceptance_09_restart_recovery` encodes the release-blocking dropped-session defect (an
frps restart leaves the frpc child retrying a burned single-use credential; the fix keys
recovery on the real frpc client rejection line and replaces the child with a fresh
credential). It needs `LIVE_PHASE4A_CONTROL_BASE_URL` and `LIVE_PHASE4A_SHARE_CODE` for the
baseline/serving check, gateway metrics (`LIVE_PHASE4A_GATEWAY_METRICS_URL`, or
`LIVE_PHASE4A_RELAY_HOST` + `LIVE_PHASE4A_GATEWAY_METRICS_ADDR`), and the restart targets
(`LIVE_PHASE4A_FRPS_SSH_HOST`, default `LIVE_PHASE4A_RELAY_HOST`; `LIVE_PHASE4A_AGENT_SSH_HOST`
optional; `LIVE_PHASE4A_FRPC_PID_MATCH`, default `frpc`). Because the harness reads the
gateway's loopback `/metrics` **over SSH to `LIVE_PHASE4A_RELAY_HOST` itself** whenever
`LIVE_PHASE4A_GATEWAY_METRICS_URL` is unset, that variable must be **user-qualified** when the
harness host has no default user for the machine (e.g. `root@10.0.0.5`, not a bare IP) — a bare
IP makes the metrics read unobservable and fails the gate. It restarts frps **only** under
`LIVE_PHASE4A_ALLOW_RESTART=1` (it never restarts the agent's child) and bounds the run with
`LIVE_PHASE4A_RECOVERY_BOUND_S` (default 120) and `LIVE_PHASE4A_TUNNEL_OFFLINE_BOUND_S`
(default 30). The full surface is `§13.2` of `docs/operations/phase4a-relay.md`.

### 3.4 STUN real-NAT gate (`§23.5`, BLOCKING)

```bash
scripts/stun-nat-gate.sh --target local     # normative in-process protocol proof (7 cases)
scripts/stun-nat-gate.sh --target remote    # against a deployed control, from behind a real NAT
```

Local mode spins the real listener + controller on loopback and proves all seven cases
including fail-closed negatives (7/7 PASS today). Remote mode needs `STUN_GATE_SERVER` and
`STUN_GATE_API_KEY` (`STUN_GATE_EXPECTED_PUBLIC_IP` for the full matrix — without it the
mismatched-egress case SKIPs rather than passing); see `--help`. Exit 0 iff no case FAILed —
SKIPs are reported as partial, not pass. Evidence:
`docs/operations/evidence/phase4a-stun-nat-gate.md`.

### 3.5 Cross-module ack test module

```bash
cd integration/controlsync && go test ./...
cd integration/controlsync && go test -race -count=5 -timeout 900s .
```

Proves: the real control listener and the real gateway sync loop agree over a real mTLS
socket — watermark advance, presence renewal past the lease, foreign-epoch ack refusal with
health withdrawal, mTLS identity rejection, and empty-snapshot boot adoption.

### 3.6 Capacity gate (`§23.8`, supplementary)

```bash
bash scripts/relay-capacity-gate.sh
```

Runs the local capacity/plateau suite (`relay/internal/integration/load_test.go`) and prints target-VM numbers as `PENDING HARDWARE`
(later filled in by the M6 environment record). Results do not alter route selection. This is the
only gate that exercises the `§23.8` capacity cases; the `§23.3` gate command scopes them out (`§3.1`).

### 3.7 M4-exit live relay e2e harness (task #15)

```bash
scripts/live-m4exit-e2e.sh                    # all six cases (live)
scripts/live-m4exit-e2e.sh --case relay_content_integrity
scripts/live-m4exit-e2e.sh --list             # print the case table
scripts/live-m4exit-e2e.sh --dry-run          # validate config + plan; nothing executed
scripts/live-m4exit-e2e.sh --selftest         # prove the pass/fail plumbing
```

Sibling of `§3.3` (not a replacement). It encodes the verified 2026-09-17 live-run sequence
for the M4-exit relay scenarios so they are repeatable: `enrollment_hydration_restart`
(registered share count + `loaded N sessions from store` + per-share relay content),
`relay_content_integrity` (gallery/`/items` count/`/thumb` type/full `/asset` sha1 + exact
byte count/playback HEAD+200 full+≥2 byte-exact in-range `206` seeks + the three documented
`416` cases), `stun_observe_and_rechallenge` (`match>0`, `mismatch=timeout=0`, 4m +
jitter\[0,15s) cadence), `direct_path_or_failclosed` (direct serves, or fail-closed relay
fallback with `direct_status=relay_fallback` + reason; never PASS when neither is
observable), `lockdown_withdrawal_and_recovery` (**opt-in**), `revocation_midstream`
(**opt-in**), and `tunnel_recovery_frps_restart` (**opt-in**: baseline tunnel online + serving
relay -> restart the pinned frps unit with the frpc child ALIVE -> observe offline -> require
a NEW child + NEW tunnel session + `online=1` + serving content within the bound, recording
the measured seconds). When the restart-hydration line cannot be observed, case 1 says exactly what to
do; an explicit agent restart can be opted into with `LIVE_M4EXIT_ALLOW_AGENT_RESTART=1` +
`LIVE_M4EXIT_AGENT_RESTART_COMMAND` (or supply a startup log via `LIVE_M4EXIT_AGENT_LOG_FILE`).

Config comes only from `LIVE_M4EXIT_*` env vars — `--help` documents every one and a missing
required value is named in the diagnostic (never guessed). Exit codes: `0` GREEN, `1` RED
(FAIL / MISSING), `2` usage or an incomplete `--dry-run` configuration, `3` PARTIAL (a SKIP,
or a dry run — nothing executed is never a pass). Deliberate divergence from `§3.3`: that
harness always exits `3` for `--dry-run`, while this one exits `2` when configuration is
incomplete, because an unset required input is an operator-usage error, not "a run that
executed nothing". Same honesty contract: a case PASSes only with at least one measured PASS
check; note-only, skipped and otherwise check-less cases never pass; check details and the
evidence metadata URLs/identifiers are sanitised before console and evidence (configured
literals, key/token/password/secret/cookie/authorization/`jti` text **and JSON `"key":
"value"` pairs**); the four state-changing actions are opt-in
(`LIVE_M4EXIT_ALLOW_LOCKDOWN=1`, `LIVE_M4EXIT_ALLOW_REVOKE=1`, case-1
`LIVE_M4EXIT_ALLOW_AGENT_RESTART=1`, and `LIVE_M4EXIT_ALLOW_FRPS_RESTART=1` for
`tunnel_recovery_frps_restart`) and flagged in the run metadata. A false-positive
hardening round added: `LIVE_M4EXIT_TARGET_CONFIRM` (must equal
`LIVE_M4EXIT_CONTROL_BASE_URL`), required before ANY state change; the revocation case
refuses to `DELETE` unless an in-flight transfer is proven (`0 < bytes < full size` and the
download process still alive) and unless the gateway drain line is NEWLY observed for the
exact relay host with a post-revoke timestamp and `streams>=1`; the lockdown case requires
the relay URL to SERVE before locking and marks the conservative locked state *before* the
lockdown request; the direct-path case requires the diagnostics to be updated after the
request and tied to `LIVE_M4EXIT_AGENT_RECORD_AGENT_ID`; the STUN case analyses only
`LIVE_M4EXIT_STUN_AGENT_ID` and requires the newest acceptance to be fresh. The lockdown
case's unlock safety net is installed in the main process (EXIT/INT/TERM) and proved by
`--selftest` with a stubbed admin API. Raw HTTP captures live in one 0700 scratch directory
under `umask 077` and are deleted by the EXIT/INT/TERM cleanup hook; evidence defaults to
`${TMPDIR:-/tmp}/sharebridge-m4exit-e2e` (a run never dirties the worktree).
The harness was **run live against the OVH test stack and passed 6/6 cases** (ledger: "TASK
#15 LIVE E2E — ALL SIX HARNESS CASES PASS AGAINST THE REAL DEPLOYMENT").

## 4. Operations docs index

- `docs/operations/phase4a-relay.md` — relay VM runbook: topology/ports, provisioning, hardening inventory, restart ordering, the `§17.3` metrics + listeners, the operator environment surface, and how to run/renew the gates.
- `docs/operations/phase4a-test-vps-deploy.md` — collocated test-VPS deploy (control + gateway + frps on one box): `.env.testing` keys, upgrade-in-place, frps.toml contents, and the `§11.3` sync-channel wiring.
- `docs/operations/agent-admin-ui-security.md` — agent admin UI bind defaults, fail-closed rule for non-loopback binds, auth, and secret handling.
- `docs/operations/evidence/phase4a-environment.md` — M6 environment record template (every value `PENDING`, filled by `scripts/live-phase4a.sh` runs).
- `docs/operations/evidence/phase4a-stun-nat-gate.md` — `§23.5` STUN gate run evidence.
- `relay/frp/GATE-EVIDENCE.md` — `§23.1`/`§23.2`/`§23.3`/`§23.7` FRP evidence: pin, digests, verbatim gate output.
- `docs/RELEASING.md` — release/verification/publish/deploy/rollback checklist.
- `relay/README.md`, `control/README.md`, `integration/controlsync/README.md` — per-component notes.

## 5. Configuration surfaces

Pointers only — the tables are the source of truth, do not duplicate them here.

| Family | Defined in | Documented in |
|---|---|---|
| Agent admin/security (`UI_ADDR`, `UI_PASSWORD`, `UI_PORT`, `CONNECT_ALLOWED_ORIGIN`, agent API keys) | `agent/internal/config/config.go` | `docs/operations/agent-admin-ui-security.md` |
| Ten `SHAREBRIDGE_GATEWAY_*` limits (`MAX_STREAMS_PER_SOURCE_IP/PER_ORIGIN/PER_AGENT/GLOBAL`, `MAX_HELLO_BYTES`, `HELLO_TIMEOUT`, `DIAL_TIMEOUT`, `IDLE_TIMEOUT`, `ABSOLUTE_LIFETIME`, `MAX_TRACKED_AGENTS`) | `relay/internal/limits/limits.go` (`ConfigFromEnvironment`) | `docs/operations/phase4a-relay.md` (limits + env-surface sections) |
| Metrics listeners (`SHAREBRIDGE_GATEWAY_METRICS_ADDR` 127.0.0.1:9101; `CONTROL_METRICS_ADDR` 127.0.0.1:9102) | `relay/cmd/gateway/main.go`, `control/cmd/server/main.go` | `docs/operations/phase4a-relay.md` |
| Six gateway sync vars (`SHAREBRIDGE_CONTROL_SYNC_URL/_SAN/_CA_FILE`, `SHAREBRIDGE_GATEWAY_SYNC_CERT_FILE/_KEY_FILE`, `SHAREBRIDGE_GATEWAY_NAMESPACE`; also `SHAREBRIDGE_GATEWAY_NIC_INTERFACE`, `_NIC_CAPACITY_BYTES_PER_SEC`) | `relay/cmd/gateway/main.go` | `docs/operations/phase4a-relay.md`, `docs/operations/phase4a-test-vps-deploy.md` |
| Five control sync vars (`CONTROL_SYNC_BIND_ADDR`, `_CERT_FILE`, `_KEY_FILE`, `_CLIENT_CA_FILE`, `_EXPECTED_CLIENT_SAN`) | `control/internal/config/config.go` | `docs/operations/phase4a-relay.md`, `docs/operations/phase4a-test-vps-deploy.md` |
| Test-deploy vars | `control/deploy-testing-relay.sh`, `control/.env.testing.example` | `docs/operations/phase4a-test-vps-deploy.md` |

Rules that apply across families: gateway limits fail closed on invalid values and the
global/listener-relevant ceilings are hard upper bounds; the control sync listener is
all-or-nothing (the material vars switch it on, bind address alone never does); the gateway
sync client requires all six together (partial config is a startup error).

## 6. Deferred-minor triage

Compiled from the project ledger carry-forward lists, the post-M4 and post-M5 milestone
audits, and the per-task reviews. Dispositions: **must-fix-before-M6**, **should-fix**,
**note-only**, **done** (with commit).

| Item | Origin | Disposition | Rationale |
|---|---|---|---|
| h2 multi-stream hardening test (revoke / re-registration on a shared HTTP/2 connection) | Post-M4 B2b review; post-M5 audit | should-fix | Code path is protocol-independent and covered over HTTP/1.1; a real multiplexed case would harden coverage only. |
| `TestOnDemandPort_CloseEscalatesAfterMaxAttempts` flake | Post-M4 B2b verify | note-only | Root cause is test-design fake-clock re-arm; stabilized and passes stress runs; no production race found. |
| `TestOnDemandPort_CloseRetriesThenSucceeds` flake | M4 closeout batch 1 | note-only | Same fake-clock family as above; passes standalone, flakes only under cross-package `-race` contention. |
| Readiness-correlation flake (`TestRealFRPContentParity`/revocation probe dials) | Task 33 fix round 2 | note-only | Observed once under load; 220+ isolated iterations and the full `-count=10` rerun passed; does not weaken a gate. |
| Capacity-timing tests flaking under `-race` contention | M5 round 2 | done (F1 fix, `fix(relay): keep the §23.3 gate exit code free of the capacity suite`) | The old rationale — "pass in isolation and in the gate runs" — was false: `TestRelayCapacityIdleActiveStreamSurvivesIdleWindow` flaked in the primary `§23.3` gate command and its failure polluted the gate's exit code. Removed the coupling (the `§23.3` gate command now `-skip`s `^TestRelayCapacity`, so its exit code reflects only the gate) and made the racy post-close `ActiveStreams()` assertion deterministic (`waitForLimiterDrain` polls the bounded, self-converging lease release instead of a one-shot check). The capacity suite still runs standalone and via `scripts/relay-capacity-gate.sh` with unchanged assertions. |
| chromedp seek-forced-fresh-request coverage note | Task 24 review | note-only | Ranged fetch + seek effect is proven; Chromium prefetch means "seek forced a fresh request" is not claimed. |
| Control direct-report freshness (per-open nonce/sequence) | Task 34 / Round C review | should-fix | Persisted IP+port must equal the ack and a live probe re-verifies reachability; provenance is weak but not unsafe. |
| Timer-renewal mapped-port changes are not proactively reported | Task 34 verify | should-fix | A renewed mapping's port change is only corrected by a later open signal; worst case an avoidable relay fallback. |
| Pre-report refusal arm lacks a behavioural RED | M4 closeout batch 1 | note-only | No deterministic pre-existing seam existed; kept as defence in depth, converges on the tested end state. |
| TCP-segmentation deliberately unclaimed for `§23.3` | Task 31 fix round | note-only | The gate proves exact TLS-record fragmentation (2/2/5); TCP segment boundaries are neither preserved nor relied upon. |
| NAT-PMP retry depth ≈ 4 attempts | Task 30 fix round 2 | should-fix | Bounded router I/O trade-off; validate on NAT-PMP hardware and consider a separate bounded budget if flaky. |
| `ActiveStreams` briefly lags after close | Task 33 / T36 verify | done (F1 fix, `fix(relay): keep the §23.3 gate exit code free of the capacity suite`) | Bounded, self-converging; the gateway removes a stream from the registry before its deferred lease release, so the old one-shot post-close check was racy under `-race` contention. `waitForLimiterDrain` now polls the drain to zero (a lease that never releases still fails). |
| `--case ""` semantics in the M6 harness | M6 harness verify | note-only | An empty string means "no scope" (runs all gates); an empty/unknown element exits 2. A one-line usage-error change if desired. |
| Single-namespace vs per-agent namespace design question | Remediation verification; whole-branch final review | must-fix-before-M6 | One gateway applier serves exactly one agent namespace; a second agent or a re-enroll makes relay unreachable. The whole-branch review sharpened this into a **foreign-namespace false-selection** finding: control publishes routes for every namespace while the gateway rejects foreign routes yet still acknowledges the global revision, so a foreign agent can look relay-selectable while its route was discarded. Resolve or explicitly accept before M6; multi-agent release is NO-GO until the gateway is multi-namespace or one isolated gateway per namespace is provisioned. |
| `integration/controlsync` module not enumerated in CI | Cross-module ack verify | should-fix | No repo-wide workflow enumerates modules yet; the module runs via `cd integration/controlsync && go test ./...`. |
| Misconfigured control sync listener `log.Fatalf`s at startup | Remediation verification | note-only | Fail-closed deploy-time trade-off (never silently serves); documented in the relay runbook. |
| STUN empty / leading-trailing-hyphen host validation | Post-M5 audit | done (`4963db34`) | Validator now rejects empty, hyphen-edged, over-length, underscore, IPv6 and space hosts; `ErrMalformedChallenge` fail-closed. |
| Cross-module real-gateway ↔ real-control ack integration test | Post-M5 audits | done (`0d872dfc`) | New test-only module drives both real halves over mTLS and pins watermark/presence/epoch behaviour. |
| Daemon exits on initial signaling connect failure | Live infra recon | done (`36cc7681`) | Initial connect/listen failures now retry with the existing backoff; only the web-server start failure stays fatal. |
| Presence-transport renewal + empty-snapshot boot adoption | Ledger I4-partial | done (`fc0359ba`) | 15 s presence republish (< 45 s lease) and top-level `gateway_boot_id`/`revision` adoption; control listener wired in production. |
| Lingering mapping usable after Unlock | Post-M4 Sol audit composition gap | done (`06a97708`) | Direct content now requires a logically open port and Unlock refuses while a close-failed mapping is unresolved. |
| Route `MaxStreamsGlobal` transported but unenforced; zero-limit semantics disagreed | Post-M5 audit | done (`f58b22c2`) | Route global now enforced as `min(process, route)`; zero is invalid on active routes on both sides; `MAX_TRACKED_AGENTS` hard-bounded. |
| Control-sync health ignored failed status acks; false lag on no-op polls; restoration anchored on emission | Post-M5 audit | done (`e15da308`, `5f0fb3e9`, `2799e882`) | Ack failure withdraws health; lag only sampled on a new revision; restoration anchors only on an accepted reset. |
| Deployment gate could approve a nonfunctional service or a non-dedicated account | Post-M5 audit | done (`150030b8`) | `ExecStart` and `User`/`Group` are exact-value pinned, cross-checked against the installer. |
| A later failed open tore down a healthy shared mapping | Post-M5 audit (remediation-introduced) | done (`794e2390`, `06115eec`) | Rollback now requires sole ownership (created + instance + join count) decided in the port loop. |
| Sol-model milestone audit over the remediation range | Quota escalation | done (satisfied by the four cross-model reviews, incl. `sol-session-audit`, `sol-harness-review`, `sol-fence-final`, `sol-whole-branch`) | The earlier "Sol review owed" note is satisfied: the whole-branch final review and the three targeted Sol reviews ran once the quota reset. |
| Start-permission fence races (publication race + Stop/start) | Live-session Sol audit | done (`71f81c2c`, `2aa3d12e`) | Managers now construct denied and publish under `lockdownMu`; `Stop` publishes the stopped state under `startMu`; the fence tests are mutation-proven load-bearing. |
| Fresh-boot relay credential could not start the tunnel without operator action | Live-session recon | done (`8c0cb07e`) | Cold boot now starts frpc with no operator action; live-verified on the fenced binary. |
| Bootstrap relay credential request was one-shot and silently dropped under rate limiting | Sol session audit | done (`b53f7e53`) | Bootstrap and unlock requests route through the manager's `EnsureCredential` bounded-retry event instead of a one-shot send. |
| Relay tunnel could not recover from a dropped frpc session (burned short-lived credential, no retry) | Live blocking defect | done (`b53f7e53`, `89dbed82`) | Detect the real pinned v0.71 reconnect-rejection line (plus a consecutive-error safety net) and replace the looping child with a fresh credential; proven live on two frps restarts, self-healing in ~5 s with no operator action. |
| Immich-mirrored share revoke is not durable (the poll re-registers the share in ~40-60 s) | Live session; whole-branch final review | must-fix-before-release | The admin UI offers "Revoke" while the Immich poll recreates the share, making it a ~1-minute outage rather than a durable revocation. This is an authorization-semantics decision that must be explicitly resolved (UI restriction or agent-side tombstone) before release. |
| §23.3 same-generation recredential gate flake | Live session | should-fix | `TestRealFRPSameGenerationRecredentialSurvivesOlderCredentialReplay` failed once on a full required-mode run, passed alone, and was green on re-run — the false-RED-pollutes-the-gate class. Needs a deterministic tightening or an explicit triage decision. |
| Direct-path availability silently depends on the agent host's inbound firewall | Live root cause | should-fix (ops doc required) | The host firewall rejects the PnP-mapped on-demand TCP range; the product fails closed to relay with `direct_status_reason=probe_failed`. Now documented as an agent-host prerequisite in `docs/operations/phase4a-relay.md` §1. |
| `direct_path_or_failclosed` expectation depends on a transient agent-record field | Live session | should-fix | The record reflects the LAST evaluation (`eligible` at rest, `relay_fallback`/`probe_failed` right after a probe); the harness reads it after its own prepare-route so it is correct in practice, but the transience deserves an ops note. |
| M4-exit harness correlation false positives (direct-record staleness, un-scoped STUN cadence, un-correlated revocation drain line) | Sol harness review | done (`ab25cf36`) | The hardening round requires provably current, agent/share-correlated diagnostics, an in-flight transfer before revoke, a newly observed hostmatched drain line, and a pre-lockdown serving baseline. A 6/6 live re-run of the hardened harness is not yet recorded. |
| Production `relay/config/frps.toml` plugin-shape assertion gap | Sol session audit | done (this round) | Deploy gate A20 asserts the production template's `[[httpPlugins]]` block, name, host-only `addr` and exact `path`, so the A19 path-doubling defect class can no longer recur on the production relay unnoticed. |
| Stale `REPO-MAP` state (test VPS unavailable / harness never run live) | Sol session audit | done (`ab25cf36`, this round) | §7 and §3.7 now record the live 6/6 run and the later harness hardening. |
| M4-exit harness `--help` says `LIVE_M4EXIT_CONTROL_BASE_URL` is required only for cases 2/4/5 | Sol session audit | done (this round) | Help now lists cases 1,2,4,5,6, matching the code and ops doc §14. |
| `control/deploy-testing-relay.sh` committed non-executable (`100644`) while the runbook invokes `./deploy-testing-relay.sh` | Sol session audit | done (this round) | Index mode set to `100755` to match `docs/operations/phase4a-test-vps-deploy.md:93`. |
| Dockerized agent needs `LIVE_M4EXIT_AGENT_LOG_FILE`; `agent_log_source()` description reads as a templated unit | Sol harness review | note-only | Operability/UX only; the harness diagnostic already names both remedies (supplied log or opt-in restart). |
| `api_key_id` (non-secret, logged by production control) appears in captured journal evidence | Live session | note-only | The sanitiser redacts share codes/passwords/keys but not agent identifiers; extend only if operator-shareable evidence must be identifier-free. |
| Two of fifteen registered Immich shares have dead share keys | Live session | note-only | Test-data issue (`Invalid share key`), not a product defect. |

### 6.1 Known product limitations (not code defects)

These are product/deployment constraints, deliberately recorded so they are not rediscovered as
bugs. They are not fixable by hardening the current code alone.

| Limitation | Recorded in | Effect / disposition |
|---|---|---|
| **Direct mode requires host-level cooperation.** The agent container must use `network_mode: host` (bridge networking breaks UPnP discovery and the inbound path); the **host firewall** must allow the mapped port — a container cannot manage the host firewall and neither can Docker or compose; and the router must support UPnP/NAT-PMP or be manually forwarded. The agent's external port is chosen from the hard-coded IANA range 49152–65535, so "open the firewall for the agent" currently means opening 16,384 ports. | `docs/superpowers/specs/2026-08-14-direct-tcp-mode-design.md` §14.1 | **Trade-off recorded; direction OPEN (not decided).** Relay needs nothing inbound (no port, no UPnP, no firewall rule) and works behind CGNAT and double NAT — live-measured ≈ 20 MB/s with zero configuration — so it is the robust zero-config floor, while direct is a conditional fast path whose speed advantage is peering-dependent (and can be negative vs a well-peered relay). Whether to commit to relay-first or keep investing in direct should be decided on evidence; if direct is pursued it must be opt-in, documented per platform, and diagnosed explicitly (a distinct "mapped port unreachable — check the host firewall" reason, not a generic `probe_failed`). |

## 7. Current state at HEAD

**Landed:** M1–M5 complete, plus the M4 closeout batches, the M5 remediation rounds
(R1–R4), and task #16 (control sync listener + presence renewal + empty-snapshot boot
adoption), the carry-forward robustness round (signaling retry, STUN host validation), the
cross-module ack test module, the M6 acceptance-harness skeleton, and the M4-exit live
relay e2e harness (`scripts/live-m4exit-e2e.sh`, task #15).

**Gate status:** `§23.3` required-mode gate GO (16 cases); deployment gate GREEN; `§23.5`
STUN gate GO (7/7 local + remote against the deployed test control); M6 harness RED by
design (no M6 infrastructure exists yet; all placeholders are `NOT_IMPLEMENTED`, never PASS).

**Known-open / remaining:**

- M6 acceptance (Tasks 37–44) is pending **infrastructure and user presence**: the separate
  relay VM, a non-hairpin/router surface, a cellular device, a second VM for the L4 capture,
  real NAT surfaces, and browser/device runs.
- The M4-exit live relay e2e **ran against the live OVH test stack and passed all six
  cases in one unscoped run** on 2026-09-17 against the harness commits
  `da47364d`+`46773cc3`+`90b60bf3` (ledger: "TASK #15 LIVE E2E — ALL SIX HARNESS CASES PASS
  AGAINST THE REAL DEPLOYMENT"); the harness for it is `scripts/live-m4exit-e2e.sh` (`§3.7`).
  The harness was hardened afterwards in `ab25cf36` (the three correlation false-positive
  holes) and no 6/6 live re-run of the hardened harness is recorded yet, so the GREEN above
  predates that round.
- The single-namespace-per-gateway design question above must be resolved or explicitly
  accepted before a multi-agent M6 topology.
- The **Sol-model milestone audit over the remediation range remains OWED**: both Codex
  models (`gpt-5.6-sol`, `gpt-5.6-terra`) were quota-exhausted at the end of this stretch and
  `zai/glm-5.3` hit its limit; the verification was completed by glm-5.3 (partial) and
  deepseek-v4-pro (remainder), which is weaker cross-model independence. Re-run on Sol when
  quota returns if that sign-off is required.
- The deferred-minor table in §6 lists the should-fix and note-only items still open.

**Before M6 acceptance:** provision the relay/test infrastructure, wire and verify the
`§11.3` sync channel live on the test VPS, run the full `scripts/live-phase4a.sh` gate set
with the M6 environment record filled from measured values, complete the user/browser
scenarios, and (per policy) obtain the final whole-branch review.
