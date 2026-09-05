# Phase 4a — FRP Relay MVP Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Spec:** [`docs/superpowers/specs/2026-09-02-phase4a-relay-mvp-design.md`](../specs/2026-09-02-phase4a-relay-mvp-design.md)

**Base branch:** `v2`

**Status:** Draft — revised after round-2 plan review

**Revision:** Round-2 plan-review fixes applied

## Spec deviations

1. **Approval status:** the normative spec is approved after user review and two SATISFIED GLM 5.3 review rounds. This revision changes plan executability and traceability only; it does not change product or protocol behavior.
2. **No behavioral deviations.** The plan does not absorb measure-and-prefer, on-demand DNS, manual-endpoint override, password/Nextcloud/OpenCloud parity, CT monitoring, or CAA. CT/CAA remain a separate user-owned release prerequisite before Phase 4b, exactly as §3.2 says.

**Goal:** Add a reliable outbound-only FRP relay path that preserves browser-to-agent TLS, exact share routing, Phase 3 content semantics, and direct-first behavior while recovering from CGNAT, UPnP failure, non-hairpin NAT, and recipient-specific direct-path failure.

**Architecture:** A new `relay/` Go module runs a public Layer-4 SNI gateway beside a pinned `frps`. Control assigns one loopback-only FRP TCP proxy per enrolled agent, signs short-lived epoch-bound admission credentials, synchronizes exact share routes and gateway-authoritative leased presence, and serves the route-preparation interstitial. The agent supervises the pinned `frpc`, keeps its existing HTTPS server alive across control-WebSocket reconnects, binds direct and relay origins to the same Phase 3 content session, and uses STUN only to qualify direct probing.

**Tech Stack:** Go 1.26 modules (`agent/`, `control/`, new `relay/`), PocketBase v0.36.8, coder/websocket, Ed25519 JWT credentials, FRP `frps`/`frpc` child binaries, FRP HTTP server plugin, `github.com/pion/stun/v3 v3.1.7`, `net/http`, systemd, nftables, Prometheus text metrics, existing chromedp Phase 3 browser fixture plus a new Playwright cross-browser runner.

## Milestone overview

| Milestone | Spec rollout alignment | Tasks | Deliverable | Blocking evidence |
|---|---|---:|---|---|
| **M1 — Hermetic foundation** | §20 steps 1–4 groundwork | 1–10 (10) | FRP artifact pin, gateway SNI router, plugin adapter, locked `frps`, DB migration, relay WS contract, agent `TunnelManager`, relay DNS/baseline readiness | §23.1, §23.2, §23.7 |
| **M2 — Route distribution + presence** | §20 step 2 | 11–15 (5) | mTLS control↔gateway protocol, snapshot/delta reconciliation, route lease, gateway boot/revision fencing, authoritative tunnel lease | Revision-gap and stale-state tests |
| **M3 — Selection + interstitial + STUN** | §20 steps 2–3 | 16–25 (10) | Authenticated STUN, live predicates, direct-first selector, `prepare-route`, interstitial/CSP/noscript, agent `/connect`, browser and real-NAT gates | §23.4, §23.5 |
| **M4 — Agent serving integration** | §20 step 4 | 26–31 (6) | Dual-origin Binder, relayOnly restoration, long-lived listener/tunnel, route-aware accounting, lockdown v2, real-FRP content parity | §23.3; acceptance #7, #8, #11, #15 |
| **M5 — Hardening + operations** | §20 step 5 | 32–36 (5) | Limits, failure recovery, metrics/log hygiene/health, systemd/firewall/DNS audit, capacity baseline | §14–§18; §23.8 |
| **M6 — Live acceptance + rollout** | §20 steps 1–7 | 37–44 (8) | Separate Hetzner relay VM, real Immich/NAT/browser acceptance, L4 capture proof, final gates, staged enablement and rollback drill | §19 #1–#15; §23.1–§23.9 |

**Task-count summary:** 44 globally numbered tasks: M1 10, M2 5, M3 10, M4 6, M5 5, M6 8. Sizes are rough reviewer/implementation estimates: S ≤ half day, M ≈ one day, L ≈ multiple days.

## Global constraints

- The spec is normative. If implementation evidence invalidates an FRP assumption, stop at the corresponding §23 gate; do not replace the design with TLS termination, custom Noise/SCTP, or a generic public proxy.
- There is no root `go.mod` or `go.work`. Run commands inside `agent/`, `control/`, and `relay/` independently.
- Use descriptive Go identifiers; do not introduce single-letter local variables except conventional bounded indices or receivers already established by the package.
- No relay process receives an agent content certificate/key, parent-zone DNS/ACME credential, cookie, path, header, share code, or plaintext body. Do not inject PROXY protocol or any bytes into the browser TLS stream.
- Browser content TLS terminates only in `agent/internal/direct`. FRP transport TLS is a second, verified outer layer for the agent→relay leg.
- FRP artifacts are **not committed as opaque binaries**. CI and developer tests fetch one exact user-approved FRP release into a checksum-keyed cache using a committed manifest containing platform SHA-256 digests. Release packaging bundles those verified binaries into the single ShareBridge agent distribution and relay deployment artifact.
- `frps` proxy ports bind only to `127.0.0.1`, are restricted to the assigned range, and are never firewalled publicly. Public 443 belongs only to `sharebridge-relay-gateway`. FRP's shared-token auth is never distributed to agents; Login must pass the mandatory local plugin.
- All new wire payloads are versioned and bounded, ignore unknown JSON fields, reject invalid enum/range/identity values, and never log credentials.
- Route and transport eligibility are live predicates. `relay_last_seen_at`, `direct_status`, and `direct_status_reason` are diagnostic only.
- `relay_only=true` must never select, prepare, probe, mutate DNS for, emit an open signal for, or navigate/request the direct origin for that access. UI copy is “Always use relay for this share”; it must not promise anonymity.
- `open_signal`, `open_ack`, `SignalGate`, endpoint reporting, DDNS, public probe, and `OnDemandPort` remain RouteDirect-only.
- Every new WebSocket handler and HTTP endpoint includes an explicit caller/authentication/rejection review in its task and test names.
- Every schema change is a new forward-only PocketBase migration. Do not edit migrations 1–8.
- Never revive deleted v1 code or dependencies: Noise, WebRTC, SCTP, relaychannel, multilane, browser service workers, `/ws/client`, or `/ws/relay`.
- Do not read or print `.env` files. Live scripts source secrets without echoing them and write root-readable deployment environment files.
- `RELAY_SELECTION_ENABLED` remains false in the production/all-v2 deployment until blocking Tasks 8, 24, 25, 31, and 42 have passed and Task 44 records a GO decision for §23 gates 1–7.
- Tasks 38–43 execute against the isolated test-account deployment with `RELAY_SELECTION_ENABLED=true`, set explicitly at the start of Task 38 and recorded as evidence in the acceptance artifacts; the production/all-v2 deployment keeps the flag false until Task 44 records GO.
- Task 25 supplies the representative real-NAT evidence for §23.5; Task 42 supplies the live reconnect/cadence/warm/cold-budget evidence for §23.6.
- Each implementation task ends with focused tests, all touched-module tests/builds, and an independently reviewable commit.

---

## File structure map

### New relay module

| File | Responsibility |
|---|---|
| `relay/go.mod`, `relay/go.sum` | Isolated gateway module and pinned Go dependencies |
| `relay/frp/manifest.json` | Exact FRP version, upstream URL, platform artifact names, SHA-256 digests |
| `relay/scripts/fetch-frp.sh` | Checksum-verifying CI/developer artifact fetch into cache |
| `relay/internal/clienthello/parser.go` | Bounded fragmented/multi-record ClientHello SNI extraction without TLS termination |
| `relay/internal/routes/table.go` | Exact route table, tunnel-generation join, route lease and revision application |
| `relay/internal/gateway/server.go` | Public 443 accept loop, prefix replay, loopback connect, bidirectional copy, stream registry |
| `relay/internal/frpplugin/server.go` | Fail-closed FRP Login/NewProxy/CloseProxy/Ping adapter |
| `relay/internal/presence/registry.go` | Boot-ID/revisioned 45-second tunnel presence state machine |
| `relay/internal/controlsync/client.go` | mTLS snapshot fetch, presence delivery, reconciliation |
| `relay/internal/limits/limits.go` | Pre-tunnel IP/origin/agent/global accounting and time/lifetime bounds |
| `relay/internal/metrics/metrics.go` | Bounded metadata-only metrics and close reasons |
| `relay/cmd/gateway/main.go` | Config, listener lifecycle, private/plugin/health/metrics servers |
| `relay/config/frps.toml` | Root-owned pinned-release static `frps` configuration |
| `relay/deploy/*.service`, `relay/deploy/firewall.nft`, `relay/deploy/install.sh` | Separate-user systemd units, sandboxing, firewall and deploy flow |

### Control module

| File | Responsibility |
|---|---|
| `control/migrations/9_agents_relay_stun.go` | §12 forward-only fields and partial unique relay-port index |
| `control/internal/relayctl/credentials.go` | Stable assignment, generation fencing, Ed25519 relay credential issuance |
| `control/internal/relayctl/protocol.go` | Versioned bounded route/presence sync DTOs |
| `control/internal/relayctl/server.go` | mTLS snapshot and presence endpoints |
| `control/internal/relayctl/publisher.go` | Ordered route add/revoke/limit deltas and acknowledgement/retry |
| `control/internal/relayctl/presence.go` | In-memory gateway-authoritative leased availability |
| `control/internal/stun/server.go` | UDP 3478 challenge verification, observed-address receipt issuance |
| `control/internal/directctl/stun.go` | WS challenge lifecycle, freshness/cadence, match policy |
| `control/internal/directctl/directpredicate.go` | Live direct-eligibility predicate and diagnostic reason codes |
| `control/internal/directctl/routes.go` | Direct-first/relay/unavailable selection result and rollout gating |
| `control/internal/directctl/prepare.go` | Four-second direct preparation endpoint orchestration |
| `control/internal/directctl/interstitial.go` | No-store page, namespace-scoped CSP, noscript/manual relay |
| `control/web/route-interstitial.html`, `control/web/route-interstitial.js`, `control/web/route-interstitial.css` | Control-hosted recipient route-check UI |
| `control/internal/handler/agent_ws.go` | New authenticated agent WS messages and `share_registered.relay_origin` |
| `control/internal/config/config.go`, `control/cmd/server/main.go` | Relay/STUN/mTLS keys and listeners; load and wire `RELAY_SELECTION_ENABLED` into `config.Config.RelaySelectionEnabled` |

### Agent module

| File | Responsibility |
|---|---|
| `agent/internal/tunnel/config.go` | Render only the one fixed FRP proxy to loopback `:8443` |
| `agent/internal/tunnel/manager.go` | Atomic 0600 config, no-shell child lifecycle, replacement, backoff, shutdown |
| `agent/internal/stun/client.go` | One-use authenticated Binding request and receipt echo |
| `agent/internal/signaling/client.go` | Relay/STUN/lockdown wire fields and send helpers |
| `agent/internal/direct/sni.go` | Both exact origin bindings and route-kind authorization |
| `agent/internal/direct/server.go` | `/connect`, route-aware connection/hold accounting, connection close registry |
| `agent/internal/daemon/daemon.go` | Baseline lifecycle, persistent HTTPS/tunnel, dual origins, relayOnly, lockdown/unlock |
| `agent/internal/store/store.go`, `agent/internal/config/config.go` | Persist both origins/policy and fixed tunnel executable/config settings |

### Cross-module and live test assets

| File | Responsibility |
|---|---|
| `testdata/relay-sync/*.json` | Golden protocol examples consumed by both Go modules |
| `relay/internal/integration/relay_test.go` | Real pinned `frps`/`frpc` hermetic data-plane suite |
| `e2e/browser/package.json`, `e2e/browser/playwright.config.js`, `e2e/browser/relay-route.spec.js` | Chromium/Firefox/WebKit route and CSP suite |
| `e2e/browser/fixture.mjs` | Starts/stops the hermetic control/gateway/FRP/agent fixture |
| `scripts/live-phase4a.sh` | Secret-safe Hetzner/home/Immich acceptance runner and evidence manifest |
| `scripts/phase4a-release-gate.sh`, `scripts/phase4a-release-gate_test.sh` | Machine-testable final evidence/selection-flag release gate and its shell tests |
| `docs/operations/phase4a-relay.md` | Deployment, DNS/ECH audit, health, metrics, rollback, acceptance evidence |

## Shared contracts

Implementers must keep these names and meanings consistent across tasks:

- `relayctl.RelayAssignment`: agent record ID, namespace, proxy name, relay port, generation.
- `relayctl.CredentialClaims`: issuer, audience `sharebridge-relay`, API-key ID, agent record ID, namespace, proxy name, relay port, generation, issued/expiry times, JTI.
- `relayctl.Route`: exact relay hostname, agent record ID, relay port, generation, session ID, route revision, limit values, active flag.
- `relayctl.PresenceEvent`: gateway boot ID, monotonically increasing revision, agent record ID, relay port, generation, state and lease expiry.
- `relayctl.RouteDecision`: lifecycle status; selection `interstitial|relay|unavailable`; direct URL candidate; optional relay URL; namespace; `directTimeoutMs=4000`.
- `tunnel.Config`: generation, gateway address/port, proxy name/relay port, credential/expiry, fixed local HTTPS address; callers cannot add proxies or change the local target.
- Agent `signaling.Message` adds `RelayOrigin` and these exact §11.1 wire shapes without changing existing direct-message JSON tags:
  - control → agent `relay_config`: `{ version, generation, gateway_addr, gateway_port, proxy_name, relay_port, credential, expires_at }`.
  - control → agent `stun_challenge`: `{ version, challenge, server, expires_at }`.
  - agent → control `stun_result`: `{ challenge, transaction_id, receipt }`.
  - agent → control `relay_client_state`: `{ generation, status: "starting"|"running"|"stopped"|"error", reason? }`; telemetry only.
  - agent → control `lockdown_status`: `{ generation, locked: bool }`; advisory fast-path telemetry only and never an availability fact.
  - control → agent `lockdown_ack`: optional acknowledgement of control-side transport deactivation.
- `config.Config.RelaySelectionEnabled` is loaded from `RELAY_SELECTION_ENABLED`, defaults to false, and gates relay selection only. False preserves the Phase 4a interstitial/`prepare-route`/STUN/direct-connect flow but makes relay-only and other relay-dependent resolutions unavailable; it never restores the Phase 3 direct 302 path.
- Current `direct.RouteDirect`, `direct.RouteRelay`, `Binder.Allow`, `Binder.Revoke`, `SignalGate.Admit`, `directctl.Controller.EmitOpen`, three-second `ackTimeout`, `directctl.Controller.Probe`, and `isSupportedGallerySession` are extended in place, not renamed speculatively.

---

# M1 — Hermetic foundation

### Task 1: Pin and fetch the exact FRP release [S]

**Purpose / spec:** Establish the reproducible real-binary test foundation required by §§3.1, 4.5, 18.2 and 23. The exact release is the user pinning decision listed at the end; implementation cannot pass this task until it is recorded.

**Files:** Create `relay/go.mod`, `relay/frp/manifest.json`, `relay/scripts/fetch-frp.sh`, `relay/internal/frptest/artifact_test.go`; modify `.gitignore`, `.github/workflows/agent-container.yml`.

- [ ] **Step 1 — Test first:** add `TestPinnedFRPArtifactsVerifyChecksums`, asserting the manifest names one immutable release, supplies upstream URLs and SHA-256 for supported developer/CI/release platforms, downloads to a version+digest cache, rejects a modified artifact, and locates both `frps` and `frpc`.
- [ ] **Step 2 — Verify RED:** run `cd relay && go test ./internal/frptest -run TestPinnedFRPArtifactsVerifyChecksums -v`; expect failure because the manifest/fetcher does not exist.
- [ ] **Step 3 — Implement:** record the approved version/digests; make the fetch script use HTTPS, fail on checksum mismatch, avoid logging credentials, and never fall back to an unpinned latest release. Add the cache step to CI and document that release packaging bundles the verified executables rather than committing binaries.
- [ ] **Step 4 — Verify GREEN:** rerun the focused test twice (cold and cached), then `cd relay && go test ./... && go build ./...`; expect both binaries to report the exact manifest version.
- [ ] **Step 5 — Commit:** `git commit -m "build(relay): pin and verify FRP artifacts"`.

### Task 2: Parse bounded fragmented ClientHello records [L]

**Purpose / spec:** Implement only the SNI peek required by §§1, 4.4, 8, 14 and 16.1; fail closed on missing/hidden/malformed input.

**Files:** Create `relay/internal/clienthello/parser.go`, `relay/internal/clienthello/parser_test.go`, `relay/internal/clienthello/testdata/*`.

- [ ] **Step 1 — Test first:** add `TestParserFragmentedTLS12ClientHello`, `TestParserFragmentedTLS13ClientHello`, `TestParserMultiRecordHello`, `TestParserReplaysInspectedPrefixExactly`, `TestParserRejectsMissingSNI`, `TestParserRejectsECHHiddenSNI`, `TestParserRejectsMalformedHello`, `TestParserRejectsOversize`, and `TestParserTimesOut`; assert normalization and the 64-KiB/5-second bounds.
- [ ] **Step 2 — Verify RED:** run `cd relay && go test ./internal/clienthello -v`; expect undefined parser failures.
- [ ] **Step 3 — Implement:** parse TLS records incrementally without invoking `tls.Server`; return normalized exact SNI plus an owned bounded prefix; preserve every inspected byte unchanged; pool only fixed-capacity buffers and zero/reset them before reuse.
- [ ] **Step 4 — Verify GREEN:** run the focused tests plus `go test -race ./internal/clienthello`; expect all failure classes to close without panics or unbounded allocation.
- [ ] **Step 5 — Commit:** `git commit -m "feat(relay): bounded fragmented ClientHello parser"`.

### Task 3: Build exact route and active-stream registries [L]

**Purpose / spec:** Supply exact-share routing, tombstone rejection, generation joining and immediate revoke semantics from §§6, 8, 14, 15.6 and 16.2.

**Files:** Create `relay/internal/routes/table.go`, `relay/internal/routes/table_test.go`, `relay/internal/gateway/streams.go`, `relay/internal/gateway/streams_test.go`.

- [ ] **Step 1 — Test first:** add `TestRouteTableAcceptsOnlyExactActiveHostname`, `TestRouteTableRejectsBareWildcardRandomAndTombstoned`, `TestRouteTableRequiresMatchingAgentPortGeneration`, `TestRouteRevisionNeverRegresses`, and `TestRevokeClosesOnlyIndexedRouteStreams`.
- [ ] **Step 2 — Verify RED:** run `cd relay && go test ./internal/routes ./internal/gateway -run 'RouteTable|Revoke' -v`; expect missing registries.
- [ ] **Step 3 — Implement:** keep the two §8 maps in memory; normalize once; reject route/presence mismatches; index streams by exact route and agent; make route revoke and lockdown close matching connections idempotently.
- [ ] **Step 4 — Verify GREEN:** run the focused tests with `-race`, then `go test ./... && go build ./...` in `relay/`.
- [ ] **Step 5 — Commit:** `git commit -m "feat(relay): exact route and stream registries"`.

### Task 4: Implement the public L4 gateway acceptor [L]

**Purpose / spec:** Complete the gateway data plane in §§4.1, 4.4, 8 and 16.1 without TLS or HTTP termination.

**Files:** Create `relay/internal/gateway/server.go`, `relay/internal/gateway/server_test.go`, `relay/cmd/gateway/main.go`; modify `relay/internal/gateway/streams.go`.

- [ ] **Step 1 — Test first:** add `TestGatewayReplaysPrefixBeforeBidirectionalCopy`, `TestGatewayUnknownSNIClosesWithoutDial`, `TestGatewayAbsentTunnelClosesWithoutDial`, `TestGatewayLoopbackConnectTimeout`, `TestGatewayClientHelloDeadline`, and `TestGatewayInjectsNoBytes`; assert dial target is exactly `127.0.0.1:<assigned-port>` and connect budget is two seconds.
- [ ] **Step 2 — Verify RED:** run `cd relay && go test ./internal/gateway -run Gateway -v`; expect missing server.
- [ ] **Step 3 — Implement:** accept public TCP, parse SNI through Task 2, join route/presence entirely from in-memory synchronized state (never query PocketBase on a public connection), acquire limits before dialing, replay the prefix, then `io.Copy` both directions; generic close every rejection and release counters/streams on all exits.
- [ ] **Step 4 — Verify GREEN:** run focused tests with `-race`, then `go test ./... && go build ./cmd/gateway`.
- [ ] **Step 5 — Commit:** `git commit -m "feat(relay): public L4 SNI forwarding gateway"`.

### Task 5: Add the forward-only relay/STUN agent migration [M]

**Purpose / spec:** Implement §12 without persisting authoritative availability.

**Files:** Create `control/migrations/9_agents_relay_stun.go`, `control/migrations/9_agents_relay_stun_test.go`.

- [ ] **Step 1 — Test first:** add `TestAgentsRelaySTUNMigration`, asserting all seven specified fields, allowed diagnostic values, existing agent preservation, idempotent migration, and a partial unique index for nonzero `relay_port` while existing API-key/namespace indexes remain.
- [ ] **Step 2 — Verify RED:** run `cd control && go test ./migrations -run TestAgentsRelaySTUNMigration -v`; expect missing migration.
- [ ] **Step 3 — Implement:** register migration 9 only; add `relay_port`, `relay_generation`, `relay_last_seen_at`, `stun_observed_ip`, `stun_observed_at`, `direct_status`, `direct_status_reason`; never add a durable relay-online boolean or `sessions.relay_origin`.
- [ ] **Step 4 — Verify GREEN:** run `cd control && go test ./migrations -v && go test ./... && go build ./...`.
- [ ] **Step 5 — Commit:** `git commit -m "feat(control): add relay and STUN diagnostic agent fields"`.

### Task 6: Allocate assignments, sign credentials, and extend relay WebSocket messages [L]

**Purpose / spec:** Implement §§4.5, 6, 7.2, 11.1–11.2 and API-key rotation preservation.

**Files:** Create `control/internal/relayctl/credentials.go`, `control/internal/relayctl/credentials_test.go`; modify `control/internal/directctl/agentstore.go`, `control/internal/directctl/enroll.go`, `control/internal/handler/agent_ws.go`, `control/internal/handler/agent_ws_test.go`, `control/internal/handler/origin_test.go`, `control/internal/config/config.go`, `control/internal/handler/api_keys.go`.

- [ ] **Step 1 — Test first:** add `TestRelayPortAllocationIsUniqueAndStable`, `TestRelayGenerationFencesOldCredential`, `TestRelayCredentialContainsExactNormativeClaims`, `TestRelayCredentialExpiresAfterTenMinutes`, `TestAcceptedTunnelMayOutliveTokenButReconnectRequiresFreshCredential`, `TestShareRegisteredReturnsControlDerivedRelayOrigin`, and `TestAPIKeyRotationPreservesRelayAssignment`.
- [ ] **Step 2 — Verify RED:** run `cd control && go test ./internal/relayctl ./internal/handler -run 'Relay|Rotation' -v`; expect missing issuer/message fields.
- [ ] **Step 3 — Implement:** allocate from the configured narrow range transactionally; derive relay origin by inserting `.relay.` into persisted direct origin; sign Ed25519 credentials with the exact claims; send `relay_config` only on the authenticated current epoch after baseline readiness; parse `relay_client_state` and `lockdown_status` as bounded telemetry only.
- [ ] **Auth/rejection review:** callers are API-key-authenticated agents after `hello`; reject stale sockets, wrong generation/status, oversize reason, invalid expiry/port/name, and messages before enrollment. Neither telemetry message may create relay availability or mutate share lifecycle.
- [ ] **Step 4 — Verify GREEN:** run focused tests, `go test ./...`, and `go build ./...` in `control/`.
- [ ] **Step 5 — Commit:** `git commit -m "feat(control): assign relay tunnels and issue epoch credentials"`.

**Amendment (2026-09-04, post-readiness-spike):** handle the new agent→control
`relay_credential_request` message (§11.1): requires the authenticated session,
rate-limited per agent (§16.4 bounds), responds with a freshly signed `relay_config`
for the current assignment/generation. The signer is already safe
(`control/internal/relayctl/credentials.go` derives `issued_at`/`expires_at` from a
single `nowFn()` reading). Test:
`TestRelayCredentialRequestIssuesFreshConfigRateLimited`.

### Task 7: Implement the fail-closed FRP authorization plugin [L]

**Purpose / spec:** Enforce §§4.2, 7.2 and 16.2 at the FRP boundary.

**Files:** Create `relay/internal/frpplugin/server.go`, `relay/internal/frpplugin/credential.go`, `relay/internal/frpplugin/server_test.go`; modify `relay/cmd/gateway/main.go`.

- [ ] **Step 1 — Test first:** add `TestPluginAcceptsExactLoginAndSingleTCPProxy`, plus rejection tests for invalid/expired/replayed/superseded credentials, identity/name/port/generation mismatch, non-TCP proxy, second proxy, out-of-range port, compression, unapproved options and unknown operations.
- [ ] **Step 2 — Verify RED:** run `cd relay && go test ./internal/frpplugin -v`; expect missing plugin.
- [ ] **Step 3 — Implement:** verify Ed25519 with only the control public key; keep JTI replay and active-generation state bounded; return FRP’s fail-closed reject response; pass validated Login/NewProxy/CloseProxy/Ping facts to presence without ever logging token material.
- [ ] **Auth/rejection review:** caller is only local `frps` over a loopback Unix/TCP endpoint; reject non-loopback peers and missing plugin shared authentication in addition to payload validation. Public clients cannot call this endpoint.
- [ ] **Step 4 — Verify GREEN:** run focused tests with `-race`, then `go test ./... && go build ./...` in `relay/`.
- [ ] **Step 5 — Commit:** `git commit -m "feat(relay): fail-closed FRP authorization plugin"`.

**Amendment (2026-09-04, post-readiness-spike):** add a readiness-only `NewUserConn`
handler on the existing bounded fail-closed event path: never an authorization input,
always returns FRP's accept response, records the correlation tuple (proxy name,
server-assigned run ID, generation metadata, remote address) for the presence
registry, and deduplicates per user connection. Probe connections are accepted
(spike-proven accept-mode, at most one zero-byte connection at the agent) rather than
the spike's suggested reject-mode, keeping the plugin response surface uniform.
Tests:
`TestNewUserConnIsReadinessOnlyAndNeverAuthorizes`,
`TestNewUserConnCorrelationTupleRecordedBounded`.

### Task 8: Prove pinned FRP plugin, loopback, port, TLS and heartbeat behavior — BLOCKING §23.1/§23.2/§23.7 [L]

**Purpose / spec:** Turn FRP assumptions into go/no-go evidence before relying on the release.

**Files:** Create `relay/internal/frptest/plugin_gate_test.go`, `relay/internal/frptest/config_gate_test.go`, `relay/internal/frptest/heartbeat_gate_test.go`, `relay/frp/GATE-EVIDENCE.md`; create `relay/config/frps.toml`.

- [ ] **Step 1 — Test first:** write real-binary tests `TestPinnedFRPInvokesRequiredPluginOperations`, `TestPinnedFRPDisconnectsOnPluginRejection`, `TestPinnedFRPProxyBindAddrIsLoopback`, `TestPinnedFRPAllowPortsAndOneProxy`, `TestPinnedFRPVerifiesTransportTLS`, and `TestPinnedFRPPingIntervalAndLeaseMargin`.
- [ ] **Step 2 — Verify RED:** run `cd relay && SHAREBRIDGE_FRP_GATE=1 go test ./internal/frptest -run 'TestPinnedFRP' -v`; expect failure until real config and adapter assumptions match the pinned release.
- [ ] **Step 3 — Implement/prove:** lock `proxyBindAddr=127.0.0.1`, narrow `allowPorts`, `maxPortsPerClient=1`, mandatory local plugin, verified transport TLS, bounded heartbeat/user-connection timeouts, no public dashboard, compression disabled, and explicit 10-second authenticated Ping (operator bandwidth cap deferred to Phase 4b per §14).
- [ ] **Step 4 — Go/no-go:** require observed Login/NewProxy/CloseProxy/Ping/NewUserConn metadata and rejection disconnect behavior. If any required operation or enforcement is unavailable, record **NO-GO** and stop; do not weaken the spec. On success, save command output/version/config digest in `GATE-EVIDENCE.md` and rerun all relay tests.
- [ ] **Step 5 — Commit:** `git commit -m "test(relay): prove pinned FRP security and heartbeat gates"`.

**Amendment (2026-09-04, post-readiness-spike):** gate the probe-confirmed readiness
predicate of §23.1 instead of the original Login+NewProxy+Ping presence assumption,
building on the retained uncommitted harness and
`frp-readiness-spike-report.md`. Replace `TestPinnedFRPBandwidthCap` (cap deferred to
4b per §14) with `TestPinnedFRPReadinessProbeConfirmsOnlyRegisteredProxy`: healthy
registration confirms in one attempt; a pre-bound port forcing post-`NewProxy`
registration failure yields zero confirmations while authenticated `Ping` continues;
stale old-generation callbacks and `frpc`/`frps` restarts never confirm the current
generation. Add delayed-Ping tolerance and true 45-second lease-expiry transition
tests (§23.7) and runtime second-proxy/out-of-range-port rejection tests (§23.2).
Record per-gate GO/NO-GO in `GATE-EVIDENCE.md` (annotate the retained `TestPinnedFRPBandwidthCap` entry as
superseded by this deferral).

### Task 9: Supervise `frpc` with an agent `TunnelManager` [L]

**Purpose / spec:** Implement §7.4 and the agent-owned half of §4.5.

**Files:** Create `agent/internal/tunnel/config.go`, `agent/internal/tunnel/config_test.go`, `agent/internal/tunnel/manager.go`, `agent/internal/tunnel/manager_test.go`; modify `agent/internal/config/config.go`, `agent/Dockerfile`, `.github/workflows/agent-container.yml`.

- [ ] **Step 1 — Test first:** add `TestRenderConfigHasOneFixedLoopbackProxy`, `TestConfigWrittenAtomicallyMode0600`, `TestManagerStartsWithoutShell`, `TestManagerReplacesOnlyHigherGeneration`, `TestManagerRefreshesExpiredReconnectCredential`, `TestManagerBackoffCapsAt60SecondsWithJitter`, and `TestManagerStopsAndKillsChild`.
- [ ] **Step 2 — Verify RED:** run `cd agent && go test ./internal/tunnel -v`; expect missing package.
- [ ] **Step 3 — Implement:** validate `relay_config`; render only `tcp`, assigned name/port, fixed `127.0.0.1:8443`, transport server-name/certificate verification and 10-second Ping; use `exec.CommandContext` argument arrays, atomic rename and 0600 mode; emit diagnostics through a callback.
- [ ] **Step 4 — Verify GREEN:** run focused tests with `-race`, then `cd agent && go test ./... && go build ./...`; inspect the image/release layout to prove the Task 1 verified `frpc` is bundled.
- [ ] **Step 5 — Commit:** `git commit -m "feat(agent): supervise the pinned FRP client"`.

**Amendment (2026-09-04, post-readiness-spike):** the renderer has hard security
requirements proven by Task 8: `transport.trustedCaFile` + `transport.serverName`
(verification must fail closed), `transport.poolCount=1`, TCP multiplexing disabled
(mux suppresses application Pings), and the explicit 10-second Ping interval (the
first Ping arrives immediately after login). Extend
`TestManagerRefreshesExpiredReconnectCredential`: after an `frps` restart the burned
one-use `jti` is replay-rejected, and the manager must request a fresh credential
over the authenticated control WebSocket before re-login rather than looping on the
stale one. Bandwidth-limit rendering is out of scope (§14 defers the cap to Phase 4b).

### Task 10: Decouple baseline enrollment from direct DDNS and provision relay DNS [L]

**Purpose / spec:** Implement §§6, 7.1, 11.2 and 17.2 so no-mapper agents can enroll.

**Files:** Modify `control/internal/directctl/controller.go`, `control/internal/directctl/enroll.go`, `control/internal/directctl/enroll_test.go`, `control/internal/directctl/endpoint.go`, `control/internal/directctl/endpoint_test.go`, `control/internal/ddns/ddns.go`, `control/internal/config/config.go`; modify `agent/internal/daemon/daemon.go`, `agent/internal/daemon/daemon_direct_test.go`.

- [ ] **Step 1 — Test first:** add `TestBaselineReadyRequiresTLSAndRelayDNSNotDirectDDNS`, `TestNoMapperCanReachEnrollmentReady`, `TestRelayWildcardProvisionedAtEnrollment`, `TestTunnelHostnameIsNotContentWildcard`, and `TestDirectDDNSFailureDoesNotBlockShareRegistration`.
- [ ] **Step 2 — Verify RED:** run focused control and agent tests; expect current TLS+direct-DDNS readiness and mapper-dependent agent behavior to fail assertions.
- [ ] **Step 3 — Implement:** replace epoch `ddnsReady` with `relayDNSReady`; idempotently provision `*.relay.<namespace>.<base-domain>` to configured gateway IPv4 during enrollment; retain direct endpoint/DDNS as optional live capability; emit `enrollment_ready` after current-epoch TLS + relay DNS only.
- [ ] **Auth/rejection review:** enrollment remains API-key WS + `hello` fenced; DNS targets come only from the authenticated agent row and operator config. Reject absent/invalid relay IPv4 or foreign namespace; never accept DNS names/IPs from agent messages.
- [ ] **Step 4 — Verify GREEN:** run `cd control && go test ./... && go build ./...` and `cd agent && go test ./... && go build ./...`.
- [ ] **Step 5 — Commit:** `git commit -m "feat: make relay DNS part of baseline enrollment"`.

---

# M2 — Route distribution and authoritative presence

### Task 11: Define and authenticate the control↔gateway protocol [L]

**Purpose / spec:** Implement the bounded versioned mTLS HTTP contract in §11.3.

**Files:** Create `control/internal/relayctl/protocol.go`, `control/internal/relayctl/server.go`, `control/internal/relayctl/server_test.go`, `relay/internal/controlsync/client.go`, `relay/internal/controlsync/client_test.go`, `testdata/relay-sync/snapshot.json`, `testdata/relay-sync/delta.json`, `testdata/relay-sync/presence.json`.

- [ ] **Step 1 — Test first:** add `TestSyncGoldenPayloadsRoundTripBothModules`, `TestSyncRequiresMutualTLS`, `TestSyncRejectsOversizeUnknownVersionAndBadRevision`, and `TestSyncAcknowledgesLastAppliedRevision`.
- [ ] **Step 2 — Verify RED:** run the focused packages in both modules; expect missing protocol/servers.
- [ ] **Step 3 — Implement:** expose private-only versioned snapshot, route-delta, presence-snapshot/event and status/ack endpoints; include boot ID, revision and bounded arrays; configure client and server certificate pin/CA independently of content PKI.
- [ ] **Auth/rejection review:** callers are the one configured gateway/control identities on the Hetzner private network. Reject public-interface binding, missing/wrong client SAN, stale boot/revision, gaps, oversized bodies, unknown versions and extra routes beyond configured bounds.
- [ ] **Step 4 — Verify GREEN:** run both focused suites with `-race`, then all control/relay tests and builds.
- [ ] **Step 5 — Commit:** `git commit -m "feat: add mutually authenticated relay sync protocol"`.

### Task 12: Publish full route snapshots and ordered deltas from control [L]

**Purpose / spec:** Implement exact active route distribution from §§6, 8, 11.3 and 15.7.

**Files:** Create `control/internal/relayctl/publisher.go`, `control/internal/relayctl/publisher_test.go`; modify `control/internal/handler/agent_ws.go`, `control/cmd/server/main.go`.

- [ ] **Step 1 — Test first:** add `TestPublisherSnapshotContainsOnlyActiveSupportedExactRelayRoutes`, `TestPublisherOrdersAddRevokeLimitDeltas`, `TestPublisherRetriesUntilExplicitAck`, and `TestPublisherGapForcesSnapshot`.
- [ ] **Step 2 — Verify RED:** run `cd control && go test ./internal/relayctl -run Publisher -v`; expect missing publisher.
- [ ] **Step 3 — Implement:** build snapshot from active supported public Immich rows; attach deterministic relay origin, assignment and limit values; publish add after successful registration, revoke before/with local lifecycle removal, and refresh finite 120-second route leases every 30 seconds while sync is healthy.
- [ ] **Auth/rejection review:** only internal mTLS gateway endpoint receives deltas; all origin/agent/session fields derive from PocketBase, never an agent or HTTP parameter. Missing assignment makes the route absent rather than partially routable.
- [ ] **Step 4 — Verify GREEN:** run focused tests, then full control tests/build.
- [ ] **Step 5 — Commit:** `git commit -m "feat(control): publish exact relay route snapshots and deltas"`.

### Task 13: Apply snapshots/deltas and expire gateway route leases [L]

**Purpose / spec:** Implement gateway startup readiness and gap recovery from §§8, 14 and 15.1/15.4.

**Files:** Modify `relay/internal/routes/table.go`; create `relay/internal/controlsync/reconcile.go`, `relay/internal/controlsync/reconcile_test.go`.

- [ ] **Step 1 — Test first:** add `TestGatewayRejectsPublicTrafficBeforeInitialSnapshot`, `TestSnapshotAtomicallyReplacesRoutes`, `TestDeltaGapBlocksAndRefetchesSnapshot`, `TestRouteLeaseExpiryBlocksNewButPreservesEstablishedStreams`, and `TestExplicitRevokeClosesEstablishedStreams`.
- [ ] **Step 2 — Verify RED:** run `cd relay && go test ./internal/controlsync ./internal/routes -run 'Snapshot|Delta|RouteLease|Revoke' -v`.
- [ ] **Step 3 — Implement:** fetch a fresh snapshot on boot/reconnect, atomically apply it, never guess over revision gaps, mark health ready only after apply, expire new-connection eligibility at 120 seconds, and distinguish lease expiry from explicit revoke.
- [ ] **Step 4 — Verify GREEN:** run focused tests with `-race`, then full relay suite/build.
- [ ] **Step 5 — Commit:** `git commit -m "feat(relay): reconcile routes and enforce finite route leases"`.

### Task 14: Drive the gateway presence state machine from FRP events [L]

**Purpose / spec:** Implement §§4.2, 7.3 and 15.1–15.3.

**Files:** Create `relay/internal/presence/registry.go`, `relay/internal/presence/registry_test.go`; modify `relay/internal/frpplugin/server.go`, `relay/internal/routes/table.go`.

- [ ] **Step 1 — Test first:** add `TestPresenceLoginPlusExactProxyBecomesOnline`, `TestCurrentPingRenews45SecondLease`, `TestCloseLogoutAndExpiryBecomeAbsent`, `TestReplacementGenerationFencesOldTunnel`, `TestBootIDAndRevisionMonotonic`, and `TestDelayedPingDoesNotFlapLease`.
- [ ] **Step 2 — Verify RED:** run `cd relay && go test ./internal/presence -v`; expect missing registry.
- [ ] **Step 3 — Implement:** require valid Login plus authorized NewProxy and probe-confirmed current-generation `NewUserConn` before online; renew only authenticated current-generation Ping; clear on CloseProxy/logout/frps reset/expiry; produce boot-ID and monotonic revision events and join presence against route agent/port/generation.
- [ ] **Step 4 — Verify GREEN:** run focused tests with fake clock and `-race`, then relay full suite/build.
- [ ] **Step 5 — Commit:** `git commit -m "feat(relay): gateway-authoritative tunnel presence leases"`.

**Amendment (2026-09-04, post-readiness-spike):** online requires probe-confirmed
registration, not `Login`+`NewProxy` alone (§7.3): after each authorized `NewProxy`
the gateway performs the bounded loopback readiness probe (§4.4: ≤5 attempts,
~500 ms backoff, 2.5 s hard deadline, deadline re-checked before each wait) and only
a correlated current-generation `NewUserConn` fact yields online. Replace
`TestPresenceLoginPlusExactProxyBecomesOnline` with
`TestPresenceRequiresProbeConfirmedNewUserConn` and add
`TestRegistrationFailureNeverBecomesOnlineWhilePingContinues` and
`TestStaleGenerationNewUserConnNeverConfirmsCurrentGeneration`.

### Task 15: Maintain control’s ephemeral relay availability view [L]

**Purpose / spec:** Implement §§4.2, 7.1, 7.3, 12 and 15.7; never route from the database diagnostic.

**Files:** Create `control/internal/relayctl/presence.go`, `control/internal/relayctl/presence_test.go`; modify `control/internal/relayctl/server.go`, `control/internal/directctl/controller.go`.

- [ ] **Step 1 — Test first:** add `TestControlLoadsFreshPresenceSnapshotOnRestart`, `TestControlDiscardsOlderBootAndRevision`, `TestPresenceLeaseExpiryMakesRelayUnavailable`, `TestRelayLastSeenIsDiagnosticOnly`, and `TestAgentTelemetryCannotSetAvailability`.
- [ ] **Step 2 — Verify RED:** run `cd control && go test ./internal/relayctl -run Presence -v`.
- [ ] **Step 3 — Implement:** hold an in-memory leased view keyed by agent; reconcile snapshot on startup; persist `relay_last_seen_at` only for diagnostics; expose a read-only `Available(agentID, port, generation, routeRevision, now)` predicate to `directctl`.
- [ ] **Auth/rejection review:** only Task 11 mTLS gateway identity supplies facts; reject revision gaps, future/expired leases, wrong assignment and superseded boot. Agent `relay_client_state` remains log/UI telemetry.
- [ ] **Step 4 — Verify GREEN:** run focused and full control tests/build with `-race`.
- [ ] **Step 5 — Commit:** `git commit -m "feat(control): consume authoritative relay presence leases"`.

---

# M3 — Route selection, interstitial and STUN

### Task 16: Implement authenticated STUN listener and receipts [L]

**Purpose / spec:** Implement the control-observed UDP flow in §10.1 and security property §16.4.

**Files:** Create `control/internal/stun/server.go`, `control/internal/stun/server_test.go`; modify `control/internal/config/config.go`, `control/cmd/server/main.go`, `control/go.mod`, `control/go.sum`.

- [ ] **Step 1 — Test first:** add `TestSTUNRecordsUDPSourceAndReturnsIntegrityProtectedReceipt`, `TestSTUNChallengeSingleUseAnd60SecondExpiry`, `TestSTUNRejectsBadIntegrityReplaySpoofAndUnknownTransaction`, and `TestSTUNRateLimitsPerAgent` using real UDP sockets.
- [ ] **Step 2 — Verify RED:** run `cd control && go test ./internal/stun -v`; expect missing listener.
- [ ] **Step 3 — Implement:** pin `github.com/pion/stun/v3 v3.1.7` in `control/go.mod`; issue one-use short-term credential state; validate STUN MESSAGE-INTEGRITY; bind receipt to challenge, transaction ID, source IPv4 and current WS epoch; return mapped address plus unpredictable receipt; store only bounded pending/accepted state. If v3.1.7 cannot support the required RFC 8489 transaction handling, short-term integrity, custom integrity-protected receipt attribute, Go 1.26 build, or Tasks 25/42 gates, stop and pin another maintained compatible package only after it passes those same criteria; do not hand-roll or weaken the protocol.
- [ ] **Auth/rejection review:** caller must know the one-use secret delivered over its authenticated WS epoch; reject malformed attributes, wrong algorithm/integrity, reused/expired challenge, non-IPv4/private-reserved observation where policy applies, and per-agent excess. UDP 3478 is the only public control listener added.
- [ ] **Step 4 — Verify GREEN:** run focused tests with `-race`, full control tests/build, and a packet fixture proving the receipt is integrity protected.
- [ ] **Step 5 — Commit:** `git commit -m "feat(control): authenticated STUN observation listener"`.

### Task 17: Add the agent STUN client and WS wire helpers [L]

**Purpose / spec:** Implement the agent half of §§10.1 and 11.1.

**Files:** Create `agent/internal/stun/client.go`, `agent/internal/stun/client_test.go`; modify `agent/internal/signaling/client.go`, `agent/internal/signaling/client_test.go`, `agent/go.mod`, `agent/go.sum`.

- [ ] **Step 1 — Test first:** add `TestClientSendsIntegrityProtectedBindingAndEchoesReceipt`, `TestClientRejectsWrongTransactionIntegrityAndExpiredChallenge`, `TestSTUNResultWireShape`, and `TestRelayConfigAndClientStateWireShapes`.
- [ ] **Step 2 — Verify RED:** run `cd agent && go test ./internal/stun ./internal/signaling -run 'STUN|RelayConfig|ClientState' -v`.
- [ ] **Step 3 — Implement:** pin the same `github.com/pion/stun/v3 v3.1.7` dependency in `agent/go.mod`; validate version/server/expiry; send one request per challenge; require matching transaction and integrity on response; send only `{challenge, transaction_id, receipt}` through the existing authenticated WS. Add bounded `relay_client_state` and `lockdown_status` helpers with the exact shared-contract fields above.
- [ ] **Step 4 — Verify GREEN:** run focused tests, then all agent tests/build.
- [ ] **Step 5 — Commit:** `git commit -m "feat(agent): authenticated STUN client and relay wire messages"`.

### Task 18: Schedule immediate, proactive and inline STUN challenges [L]

**Purpose / spec:** Implement §10.2 and the freshness portion of §§4.4/7.1.

**Files:** Create `control/internal/directctl/stun.go`, `control/internal/directctl/stun_test.go`; modify `control/internal/directctl/controller.go`, `control/internal/directctl/enroll.go`, `control/internal/handler/agent_ws.go`.

- [ ] **Step 1 — Test first:** add `TestSTUNChallengeImmediatelyAfterEnrollmentReadyForCurrentEpoch`, `TestSTUNRechallengeAtFourMinutesWithJitter`, `TestFreshObservationReusedByWarmPrepare`, `TestMissingOrStaleObservationChallengesInlineOnce`, `TestReconnectInvalidatesOldObservation`, and `TestChallengeRetryBackoffIsBounded`.
- [ ] **Step 2 — Verify RED:** run `cd control && go test ./internal/directctl -run STUN -v`.
- [ ] **Step 3 — Implement:** add per-epoch challenge state and fake-clock seams; challenge after current-epoch enrollment handshake, re-challenge about four minutes after acceptance, expire observation at five minutes, and allow the prepare context to await one inline refresh without exceeding its parent deadline.
- [ ] **Auth/rejection review:** `stun_result` is accepted only from the current API-key socket after `hello`; reject unknown challenge/transaction/receipt, wrong epoch, expiry, replay, malformed sizes and surplus result rate. It may update observation, never relay availability.
- [ ] **Step 4 — Verify GREEN:** run focused tests with `-race`, then full control tests/build.
- [ ] **Step 5 — Commit:** `git commit -m "feat(control): schedule current-epoch STUN observations"`.

### Task 19: Gate DDNS/probing on exact STUN match and record diagnostics [L]

**Purpose / spec:** Implement §§7.1, 10.3, 11.2, 12 and 15.7, closing the Phase 2 probe gap while ensuring a relay tunnel never manufactures direct endpoint state.

**Files:** Modify `control/internal/directctl/endpoint.go`, `control/internal/directctl/probe.go`, `control/internal/directctl/controller.go`; create `control/internal/directctl/directpredicate.go`, `control/internal/directctl/directpredicate_test.go`; modify `agent/internal/daemon/daemon.go`, `agent/internal/daemon/daemon_direct_test.go`.

- [ ] **Step 1 — Test first:** add `TestDirectPredicateRequiresLiveWSDDNSEndpointAndFreshSTUNMatch`, `TestSTUNMustMatchReportEndpointAndOpenAck`, `TestMismatchStopsBeforeDDNSUpdateAndProbe`, `TestPrivateReservedCGNATObservationFallsBack`, `TestDirectStatusDiagnosticsNeverAuthorizeRoute`, `TestRelayTunnelOnlyAgentDoesNotReportOrAuthorizeFakePublicEndpoint`, and `TestDirectEligibleAgentStillReportsRealPublicEndpoint`. The relay-only test establishes only the FRP tunnel, asserts the agent sends `relay_client_state` but no `report_endpoint`, and asserts control records/authorizes no endpoint from that state.
- [ ] **Step 2 — Verify RED:** run `cd control && go test ./internal/directctl -run 'DirectPredicate|Mismatch|Diagnostics|FakePublicEndpoint' -v` and `cd agent && go test ./internal/daemon -run 'FakePublicEndpoint|ReportsRealPublicEndpoint' -v`; current probe can run without STUN and relay-only endpoint isolation is not explicit.
- [ ] **Step 3 — Implement:** compare exact IPv4 against both endpoint report and `open_ack.public_ip`; update direct DDNS only after a match; wrap `Probe` so it cannot run without a fresh current-epoch match; persist bounded diagnostic reason codes and `relay_fallback` without reading them as input. Wire tunnel lifecycle only to `relay_client_state`; keep `ReportEndpoint` calls sourced only by actual mapper/public-IP discovery and direct mapping transitions, and keep control from treating relay state as a reported endpoint. Preserve the existing direct report behavior for direct-capable agents.
- [ ] **Step 4 — Verify GREEN:** run focused/full control tests and build; use a spy to prove zero DDNS/probe calls on mismatch.
- [ ] **Step 5 — Commit:** `git commit -m "fix(control): require STUN match before direct DDNS and probe"`.

### Task 20: Implement the complete live route-selection matrix [L]

**Purpose / spec:** Implement §§6.1 and 9.1–9.2 without measure-and-prefer.

**Files:** Create `control/internal/directctl/routes.go`, `control/internal/directctl/routes_test.go`; modify `control/internal/directctl/redirect.go`, `control/internal/directctl/redirect_test.go`, `control/internal/directctl/controller.go`, `control/internal/config/config.go`, `control/internal/config/config_test.go`, `control/cmd/server/main.go`, `control/cmd/server/main_test.go`.

- [ ] **Step 1 — Test first:** add table-driven `TestRouteSelectionMatrix` over lifecycle, relayOnly, direct eligible, STUN missing/stale/mismatch, relay lease, WS state, and `RelaySelectionEnabled`; add `TestRelaySelectionEnabledDefaultsFalse`, `TestBothCanonicalRoutesPreserve404410Interstitial302And503`, `TestRelaySelectionNeverSignalsProbesOrMutatesDirectState`, `TestRelayOnlyPrepareCannotTouchDirect`, `TestOriginsAlwaysDerivedFromPersistedSession`, and `TestRollbackModeDisablesRelaySelectionButKeepsNewDirectFlow`. The rollback test must prove: a non-relayOnly direct candidate still gets the Phase 4a interstitial; a relay-ineligible, direct-ineligible case gets the 503 offline page; a present relay with the flag off still gets 503 rather than relay; and no case revives the legacy Phase 3 direct 302.
- [ ] **Step 2 — Verify RED:** run `cd control && go test ./internal/config ./internal/directctl ./cmd/server -run 'RelaySelectionEnabled|RouteSelection|RelaySelection|RollbackMode|Origins' -v`; expect the default/rollback tests to fail because no flag is loaded or wired.
- [ ] **Step 3 — Implement:** add `config.Config.RelaySelectionEnabled`, load it from operator environment variable `RELAY_SELECTION_ENABLED` with default false, pass it through `directctl.Config` in `control/cmd/server/main.go`, and gate every relay selection on it. With the flag true, keep `ResolveForRedirect` as lifecycle owner, allow active public unprotected Immich `relay_only`, return interstitial only for non-relayOnly direct candidates, immediate relay for relayOnly/non-STUN direct-ineligible with presence, and 503 otherwise; construct relay URL deterministically. With the flag false, preserve interstitial/`prepare-route`/STUN/direct-connect behavior but never select relay or restore the old direct 302.
- [ ] **Step 4 — Verify GREEN:** rerun the focused tests with the environment unset, false, and true, then run full control tests/build; assert no performance scores or measurements influence decisions.
- [ ] **Step 5 — Commit:** `git commit -m "feat(control): select direct candidate or authoritative relay"`.

### Task 21: Add `POST /api/shares/<code>/prepare-route` [L]

**Purpose / spec:** Implement the bounded preparation flow in §§4.4 and 9.3.

**Files:** Create `control/internal/directctl/prepare.go`, `control/internal/directctl/prepare_test.go`; modify `control/cmd/server/main.go`.

- [ ] **Step 1 — Test first:** add `TestPrepareRouteSuccessReturnsDerivedURLsAnd4000ms`, `TestPrepareRouteOverallBudgetNeverExceedsFourSeconds`, `TestPrepareRunsMappingAndSTUNConcurrentlyButProbeWaitsForMatch`, `TestPrepareFailureSelectsAvailableRelay` with `RelaySelectionEnabled=true` in the test controller, `TestBudgetMissDoesNotPersistStickyFailure`, and lifecycle/method/body rejection tests.
- [ ] **Step 2 — Verify RED:** run `cd control && go test ./internal/directctl -run PrepareRoute -v`.
- [ ] **Step 3 — Implement:** re-resolve lifecycle and live predicates; create one four-second context encompassing inline STUN, `EmitOpen`, ack and verified-tuple probe; allow mapping/STUN overlap but sequence probe after match; return no-store bounded JSON with direct/optional relay or unavailable.
- [ ] **Auth/rejection review:** caller is an unauthenticated recipient possessing the native share code (existing bearer capability). Accept POST only, no redirect/origin inputs, bounded empty/JSON body; return 404 revoked/unknown, 410 expired/unsupported, unavailable without topology details, and never expose internal agent IDs.
- [ ] **Step 4 — Verify GREEN:** run focused tests with fake clock and full control tests/build.
- [ ] **Step 5 — Commit:** `git commit -m "feat(control): add bounded prepare-route endpoint"`.

### Task 22: Serve the no-store interstitial, manual relay, CSP and noscript path [L]

**Purpose / spec:** Implement §§4.3–4.4 and 9.3–9.4.

**Files:** Create `control/internal/directctl/interstitial.go`, `control/internal/directctl/interstitial_test.go`, `control/web/route-interstitial.html`, `control/web/route-interstitial.js`, `control/web/route-interstitial.css`; modify `control/cmd/server/main.go`, `control/deploy-testing.sh`.

- [ ] **Step 1 — Test first:** add `TestInterstitialNoStoreAndNonceScript`, `TestInterstitialCSPAllowsOnlySameOriginAndCurrentNamespaceHTTPSAnyPort`, `TestInterstitialNeverEmbedsContentIframe`, `TestInterstitialNoscriptUsesExactDerivedRelay` with `RelaySelectionEnabled=true` in the test controller, and `TestInterstitialWithoutRelayShowsUnavailableAndRetry`.
- [ ] **Step 2 — Verify RED:** run `cd control && go test ./internal/directctl -run Interstitial -v`.
- [ ] **Step 3 — Implement:** render progress, immediate “Use relay now,” cancellation of preparation/direct fetch, exact navigation rules and canonical retry; construct CSP from authenticated session namespace, not request input; keep `default-src 'none'`, nonce-bound script, same-origin preparation and namespace-scoped HTTPS `connect-src`.
- [ ] **Auth/rejection review:** caller and lifecycle results match Task 21. HTML contains only control-derived URLs; reject invalid code before rendering. No frame/object/http destinations and no content proxying.
- [ ] **Step 4 — Verify GREEN:** run focused/full control tests/build and verify deploy script copies the three new assets without printing secrets.
- [ ] **Step 5 — Commit:** `git commit -m "feat(control): add direct-check relay-fallback interstitial"`.

### Task 23: Add the agent `GET /s/<code>/connect` CORS endpoint [M]

**Purpose / spec:** Implement the recipient-path test in §9.3.

**Files:** Modify `agent/internal/direct/server.go`, `agent/internal/direct/server_test.go`, `agent/internal/direct/handlers.go`.

- [ ] **Step 1 — Test first:** add `TestConnectReturns204NoStoreForAuthorizedDirectBinding`, `TestConnectAllowsOnlyShareBridgeAppOrigin`, `TestConnectSetsNoCookieAndTouchesNoBackend`, `TestConnectRejectsRelayBindingWrongHostWrongCodeAndNonGET`, and `TestConnectPreflightIsNotBroadened`.
- [ ] **Step 2 — Verify RED:** run `cd agent && go test ./internal/direct -run Connect -v`; expect 404.
- [ ] **Step 3 — Implement:** after Binder SNI/Host/code authorization, require `RouteDirect`, exact `Origin: https://sharebridge.app`, GET, return 204 with no-store and one exact ACAO; perform no resolver/backend/accounting work and set no cookie.
- [ ] **Auth/rejection review:** caller is the control-hosted interstitial; Binder is the primary authorization. Reject relay route, absent/foreign Origin, wrong method/Host/code/SNI and any unauthenticated path with 403/404 without reflecting arbitrary origins.
- [ ] **Step 4 — Verify GREEN:** run focused/full agent tests/build.
- [ ] **Step 5 — Commit:** `git commit -m "feat(agent): add authorized direct connect check"`.

### Task 24: Prove four-second browser/CORS/CSP/noscript behavior — BLOCKING §23.4 [L]

**Purpose / spec:** Automate §18.3, acceptance #4/#14 and gate §23.4 across supported browser engines.

**Files:** Modify `agent/internal/direct/parity_test.go`; create `e2e/browser/package.json`, `e2e/browser/package-lock.json`, `e2e/browser/playwright.config.js`, `e2e/browser/relay-route.spec.js`, `e2e/browser/fixture.mjs`; modify `README.md` test commands.

- [ ] **Step 1 — Test first:** name cases `direct success never touches relay`, `blackhole falls back within bound`, `manual relay cancels direct`, `relayOnly emits no direct request`, `non443 direct passes scoped CSP`, `unrelated destinations are blocked`, `connect has no cookies/content`, `javascript-disabled follows exact noscript relay`, and chromedp case `relay gallery lightbox video seek returns 206`.
- [ ] **Step 2 — Verify RED:** run `cd e2e/browser && npm ci && npx playwright test --project=chromium --project=firefox --project=webkit`, then `cd agent && go test ./internal/direct -run TestRelayParityBrowserGalleryLightboxVideoSeek206 -v`; expect missing UI/fixture/relay-origin behavior.
- [ ] **Step 3 — Implement fixture/assertions:** extend `agent/internal/direct/parity_test.go` in the shipped Phase 3 chromedp style: load the gallery through its `RouteRelay` hostname, open the video lightbox, seek by setting a later `currentTime`, and assert the relay-origin playback request carries Range semantics and receives HTTP `206` with the expected `Content-Range`. Use Playwright Chromium/Firefox/WebKit for the normative cross-browser routing/CSP/CORS/noscript matrix. Record navigation/fetch timing from successful preparation to fallback; assert the AbortController deadline is 4000 ms and tolerances do not permit hangs.
- [ ] **Step 4 — Go/no-go:** also execute real Safari macOS and iOS Safari smoke cases because Playwright WebKit is not a substitute for deployed Safari. Save versions and results. Any browser that cannot observe credentialless CORS 204, scoped non-443 CSP, or noscript fallback is **NO-GO** for automatic fallback.
- [ ] **Step 5 — Commit:** `git commit -m "test(browser): prove bounded cross-browser relay fallback"`.

### Task 25: Prove authenticated STUN through real NAT — BLOCKING §23.5 [M]

**Purpose / spec:** Validate §§10.1, 10.3 and 23.5 before direct probing depends on the new protocol. Task 25 is the representative real-NAT gate; Task 42 separately owns the blocking live cadence/warm/cold-budget evidence for §23.6.

**Files:** Create `scripts/stun-nat-gate.sh`, `docs/operations/evidence/phase4a-stun-nat-gate.md`; modify `control/internal/directctl/stun_test.go`. Task 35 owns creation of `docs/operations/phase4a-relay.md`; Task 25 modifies that operations doc only if Task 35 has already created it.

- [ ] **Step 1 — Test first:** script named cases for owner router NAT, phone hotspot/cellular NAT, blocked UDP, spoof, mismatched egress, receipt replay, and expired challenge.
- [ ] **Step 2 — Verify RED:** run against a test control listener before deployment; expect no valid receipt and a nonzero gate result.
- [ ] **Step 3 — Execute/prove:** deploy only the STUN listener and test agent; capture transaction/receipt IDs as hashes, never secrets; prove the exact observed source reaches control, receipt replay/spoof/timeout/mismatched-egress paths fail closed, and no public probe follows mismatch.
- [ ] **Step 4 — Go/no-go:** save NAT/browser/network versions and timestamps as the §23.5 evidence in `docs/operations/evidence/phase4a-stun-nat-gate.md`. If Task 35 has already created `docs/operations/phase4a-relay.md`, append the STUN NAT-gate results there; otherwise keep them only in Task 25's evidence file for Task 35 to reference later. Failure under representative NAT is **NO-GO** for automatic fallback; do not replace the receipt with agent-reported IP. Do not mark §23.6 complete here; Task 42 supplies that live evidence.
- [ ] **Step 5 — Commit:** `git commit -m "test(stun): prove real-NAT authenticated receipt gate"`.

---

# M4 — Agent serving integration

### Task 26: Bind, persist and revoke both origins together [L]

**Purpose / spec:** Implement §§6, 11.1 and 13.1 on the shipped `RouteDirect`/`RouteRelay` Binder.

**Files:** Modify `agent/internal/signaling/client.go`, `agent/internal/signaling/client_test.go`, `agent/internal/store/store.go`, `agent/internal/store/store_test.go`, `agent/internal/direct/sni.go`, `agent/internal/direct/sni_test.go`, `agent/internal/daemon/daemon.go`, `agent/internal/daemon/daemon_direct_test.go`; modify `control/internal/handler/agent_ws.go` tests as needed.

- [ ] **Step 1 — Test first:** add `TestShareRegisteredReturnsAndPersistsBothOrigins`, `TestBothOriginsResolveSameContentSession`, `TestRevocationRemovesBothBindings`, `TestBinderRebuildReallowsBothRouteKinds`, and `TestSignalGateStillRejectsRouteRelay`.
- [ ] **Step 2 — Verify RED:** run focused agent/control tests; current registration carries only `origin` and daemon binds direct only.
- [ ] **Step 3 — Implement:** add `relay_origin` without changing `origin`; store both per session; call `Binder.Allow` with correct route kinds; rebuild and revoke as an atomic logical pair; keep one resolver/snapshot/ledger and existing native code.
- [ ] **Step 4 — Verify GREEN:** run all agent/control tests and builds.
- [ ] **Step 5 — Commit:** `git commit -m "feat(agent): bind direct and relay origins to one session"`.

### Task 27: Restore relayOnly for supported public Immich shares [M]

**Purpose / spec:** Implement §§6.1, 13.3 and acceptance policy without restoring deferred share types.

**Files:** Modify `agent/internal/daemon/daemon.go`, `agent/internal/daemon/enforce_test.go`, `agent/internal/config/config.go`, `control/internal/handler/agent_ws.go`, `control/internal/handler/agent_ws_test.go`, `control/internal/directctl/redirect.go`, `control/internal/directctl/redirect_test.go`, `agent/internal/web/handlers.go`, `agent/internal/web/api.go`, `agent/internal/web/api_test.go`, `agent/internal/web/templates/share-form.html`, `agent/internal/web/templates/settings.html`.

- [ ] **Step 1 — Test first:** add `TestRelayOnlyPollingRegistrationAccepted`, `TestRelayOnlyManualCreationAccepted`, `TestRelayOnlyRestorationAccepted`, `TestControlAcceptsSupportedRelayOnlyImmich`, `TestCanonicalRelayOnlyResolution`, and `TestShareFormUsesAlwaysUseRelayCopy`; retain named rejection tests for password and Nextcloud/OpenCloud and assert old unsupported tombstones are not reactivated. The setting is definitively exposed on branch `v2`: `config.Config.DefaultRelayOnly` is rendered and saved by the local admin surface in `agent/internal/web/handlers.go`, `agent/internal/web/api.go`, `agent/internal/web/templates/share-form.html`, and `agent/internal/web/templates/settings.html`.
- [ ] **Step 2 — Verify RED:** run `cd agent && go test ./internal/daemon ./internal/web -run RelayOnly -v` and matching control tests; current Phase 3 guards reject it.
- [ ] **Step 3 — Implement:** remove only the temporary relayOnly rejection, preserve source verification and new-registration semantics, wait for baseline rather than direct readiness, and avoid any direct setup for the share. Keep `DefaultRelayOnly` in agent config and the existing authenticated local admin surface; replace the current “Relay (recommended)”/“hides your IP” wording with the exact non-anonymity claim “Always use relay for this share” wherever the share/default mode is presented.
- [ ] **Step 4 — Verify GREEN:** run full agent/control tests/builds.
- [ ] **Step 5 — Commit:** `git commit -m "feat: restore relay-only public Immich shares"`.

### Task 28: Keep HTTPS and tunnel alive across control-WebSocket reconnects [L]

**Purpose / spec:** Implement §§7.4, 13.1 and 15.3–15.4.

**Files:** Modify `agent/internal/daemon/daemon.go`, `agent/internal/daemon/daemon_direct_test.go`, `agent/internal/tunnel/manager.go`, and `agent/cmd/agent/main.go`; at `agent/cmd/agent/main.go:89`, wire daemon/tunnel shutdown to the existing `signal.NotifyContext` process context.

- [ ] **Step 1 — Test first:** add `TestHTTPSServerSurvivesControlWebSocketReconnect`, `TestSignalGateResetsButTunnelContinues`, `TestHigherGenerationReplacesTunnelWithoutPrematureKill`, `TestDaemonShutdownStopsHTTPSAndFRPC`, and `TestAgentRestartHydratesBeforeContentReady`.
- [ ] **Step 2 — Verify RED:** current `onSignalingDisconnect` cancels the HTTPS server.
- [ ] **Step 3 — Implement:** move the HTTPS listener out of WS epoch lifecycle once a valid cert exists; reset only direct SignalGate/readiness on disconnect; preserve a healthy tunnel until replacement succeeds; on restart load cert/namespace/session state, obtain fresh credential, hydrate snapshots and then report serving readiness.
- [ ] **Step 4 — Verify GREEN:** run daemon/tunnel race tests, then full agent suite/build.
- [ ] **Step 5 — Commit:** `git commit -m "feat(agent): keep relay serving across control reconnects"`.

**Amendment (2026-09-04, post-readiness-spike):** add
`TestFRPCRestartObtainsFreshCredentialAfterReplayRejection`: after an `frps` restart
the burned one-use `jti` is replay-rejected, and the manager must request a fresh
credential over the reconnected control WebSocket before re-login instead of
retry-looping the stale one (§§7.2, 15.2). The WebSocket may still be connected in an
`frps`-restart scenario; the request rides it, or the reconnected one after a full
agent–control disconnect.

### Task 29: Make connection accounting route-aware and closable [L]

**Purpose / spec:** Implement §§9.4 and 13.2 so relay traffic cannot hold the home mapping.

**Files:** Modify `agent/internal/direct/server.go`, `agent/internal/direct/server_test.go`, `agent/internal/direct/ondemand_hold_test.go`; create `agent/internal/direct/connections.go`, `agent/internal/direct/connections_test.go`.

- [ ] **Step 1 — Test first:** add `TestDirectConnectionUsesOnDemandSessionAndHold`, `TestRelayConnectionNeverBeginsRenewsOrHoldsPort`, `TestBothRoutesShareContentSemaphoresLedgerAndResolver`, `TestCloseByRouteAndShare`, and `TestDirectAndRelayStreamsCanCoexist` including HTTP/2 streams.
- [ ] **Step 2 — Verify RED:** current `connState` has no route and all content activity can call `SessionTracker`.
- [ ] **Step 3 — Implement:** store Binder-admitted `RouteKind`/origin/share on connection state; invoke `OnDemandPort` session/hold only for direct; keep Phase 3 stream gates/accounting common; register raw connections for close-by-route/share/all.
- [ ] **Step 4 — Verify GREEN:** run focused tests with `-race`, then full agent tests/build.
- [ ] **Step 5 — Commit:** `git commit -m "fix(agent): isolate direct port accounting from relay traffic"`.

### Task 30: Implement reversible lockdown v2 and explicit unlock [L]

**Purpose / spec:** Implement §13.4 and acceptance #10 without tombstoning source shares.

**Files:** Modify `agent/internal/daemon/daemon.go`, `agent/internal/daemon/daemon_test.go`, `agent/internal/direct/connections.go`, `agent/internal/web/api.go`, `agent/internal/web/api_test.go`, `agent/internal/signaling/client.go`; modify `control/internal/handler/agent_ws.go`, `control/internal/directctl/routes.go` and tests.

- [ ] **Step 1 — Test first:** add `TestLockdownSetsGateDeletesMappingStopsTunnelRevokesBinderAndClosesBothRoutes`, `TestLockdownStatusOnlySuppressesNeverCreatesAvailability`, `TestUnlockRestoresSourceVerifiedBindingsAndUsesFreshCredential`, and `TestLockdownDoesNotTombstoneSession`.
- [ ] **Step 2 — Verify RED:** run focused agent/control tests; current behavior does not coordinate tunnel/relay streams.
- [ ] **Step 3 — Implement:** concurrently/best-effort execute all six §13.4 actions; make local enforcement final; add explicit local unlock that rebuilds admissions, starts listener/tunnel and requests current credential/presence (via `relay_credential_request`, §11.1); optional control `lockdown_ack` acknowledges deactivation only.
- [ ] **Auth/rejection review:** lockdown/unlock caller is the existing authenticated local agent admin API/UI, not recipient/control. WS status is current-epoch advisory; reject stale generation and never let `locked=false` make a route available.
- [ ] **Step 4 — Verify GREEN:** run race tests and full agent/control builds.
- [ ] **Step 5 — Commit:** `git commit -m "feat(agent): stop direct and relay paths during reversible lockdown"`.

### Task 31: Run real-FRP hermetic TLS/content parity — BLOCKING §23.3 [L]

**Purpose / spec:** Implement §18.2, prove §16.1 byte preservation, gate §23.3, and automate acceptance #7/#8/#11/#15.

**Files:** Create `relay/internal/integration/relay_test.go`, `relay/internal/integration/fixtures_test.go`; modify `agent/internal/direct/parity_test.go`, `relay/internal/frptest/heartbeat_gate_test.go`.

- [ ] **Step 1 — Test first:** add `TestRealFRPRelayEndToEndTLS12HTTP11`, `TestRealFRPRelayEndToEndTLS13HTTP2`, `TestRealFRPFragmentedClientHelloReplay`, `TestRealFRPExactRouting`, `TestRealFRPContentParity`, `TestRelayPathNeverEmitsOpenSignal`, and heartbeat delay/expiry cases.
- [ ] **Step 2 — Verify RED:** run `cd relay && SHAREBRIDGE_FRP_INTEGRATION=1 go test ./internal/integration -v`; expect no complete stack.
- [ ] **Step 3 — Implement fixture:** start checksum-verified `frps`/`frpc`, gateway, control sync stub, agent HTTPS with test cert and fake Phase 3 backend; exercise page/items/thumb/preview/original/playback multiple `206` seeks/archive, cancellation, accounting, concurrency, revocation and tunnel restart against both base URLs.
- [ ] **Step 4 — Go/no-go:** compare the browser TLS byte stream before gateway and at agent after FRP decapsulation; require exact bytes for fragmented TLS 1.2/1.3 and HTTP/1.1/2. Save versions/digests. Any mismatch or protocol failure is **NO-GO**.
- [ ] **Step 5 — Commit:** `git commit -m "test(relay): prove real-FRP TLS replay and content parity"`.

---

# M5 — Hardening, failure handling and operations

### Task 32: Enforce all gateway resource bounds [L]

**Purpose / spec:** Implement every default in §14 before any FRP user connection opens.

**Files:** Create `relay/internal/limits/limits.go`, `relay/internal/limits/limits_test.go`; modify `relay/internal/gateway/server.go`, `relay/internal/gateway/server_test.go`, `relay/internal/routes/table.go`.

- [ ] **Step 1 — Test first:** add tests for 16/source-IP, 32/origin, 64/agent, 8192/global-or-lower-FD, one proxy, 64-KiB hello, 5-second hello, 2-second dial, 5-minute no-byte idle, 24-hour absolute close, per-agent bytes/active streams and every error/close counter-release path.
- [ ] **Step 2 — Verify RED:** run `cd relay && go test ./internal/limits ./internal/gateway -run 'Limit|Timeout|Lifetime|Counter' -v`.
- [ ] **Step 3 — Implement:** atomically acquire global→IP→agent→origin before dial, release in reverse on every exit; track activity without buffering payload; make defaults configurable/tested; per-agent byte/stream counters and saturation alerts are the operator's emergency tools while the product-tier throttle stays off (cap deferred to Phase 4b per §14).
- [ ] **Step 4 — Verify GREEN:** run focused tests with `-race` and leak checks, then full relay suite/build.
- [ ] **Step 5 — Commit:** `git commit -m "feat(relay): enforce bounded gateway resources"`.

### Task 33: Prove specified restart, partition and stale-state recovery [L]

**Purpose / spec:** Implement all §15 failure cases and §18.5 recovery tests.

**Files:** Create `relay/internal/integration/failure_test.go`; modify `relay/internal/controlsync/reconcile_test.go`, `control/internal/relayctl/presence_test.go`, `agent/internal/daemon/daemon_direct_test.go`.

- [ ] **Step 1 — Test first:** add named cases for gateway restart, frps-only restart, agent restart idle/active, control-WS loss with healthy tunnel, control-sync loss past route lease, presence expiry without CloseProxy, direct failure before/after navigation, IP/DDNS change, contradictory state and revoke during long transfer.
- [ ] **Step 2 — Verify RED:** run the integration failure suite; expect stale availability or incorrect stream closure until lifecycle code is complete.
- [ ] **Step 3 — Implement/fix owners:** gateway boot starts unready/empty; frps reset clears presence immediately; route-lease expiry blocks new but not established streams; explicit revoke/lockdown closes active streams; no DB/agent telemetry overrides gateway absence; warm replacement does not kill healthy tunnel early.
- [ ] **Step 4 — Verify GREEN:** run failure suite repeatedly with `-count=10`, then all three module suites/builds.
- [ ] **Step 5 — Commit:** `git commit -m "test: prove relay restart partition and stale-state recovery"`.

### Task 34: Add metadata-only metrics, log hygiene and truthful health [L]

**Purpose / spec:** Implement §§16.6 and 17.1/17.3.

**Files:** Create `relay/internal/metrics/metrics.go`, `relay/internal/metrics/metrics_test.go`; modify `relay/internal/gateway/server.go`, `relay/internal/frpplugin/server.go`, `relay/internal/controlsync/client.go`, `relay/cmd/gateway/main.go`; modify `control/internal/directctl/prepare.go`, `control/internal/directctl/stun.go`, `control/cmd/server/main.go`.

- [ ] **Step 1 — Test first:** add `TestMetricsCoverRequiredRelayAndDirectSignals`, `TestLogsNeverContainCredentialCodePathHeaderBodyOrClientHello`, `TestGatewayHealthRequiresSnapshotAndControlSync`, and `TestMetricsEndpointIsPrivateOnly`.
- [ ] **Step 2 — Verify RED:** run relay/control metric/log tests.
- [ ] **Step 3 — Implement:** expose the exact §17.3 counters/gauges/histograms; use bounded reason labels and agent/origin policy labels without plaintext; split gateway snapshot-ready health from frps process health; set bounded log retention/rate expectations.
- [ ] **Auth/rejection review:** health/metrics callers are private monitoring only; bind private/loopback, reject public Host/interface and do not expose route lists or source-IP detail in unauthenticated responses.
- [ ] **Step 4 — Verify GREEN:** run focused tests, secret/canary log scan, then full relay/control suites/builds.
- [ ] **Step 5 — Commit:** `git commit -m "feat(ops): add relay health metrics and log redaction"`.

### Task 35: Ship systemd, firewall, mTLS, transport DNS and ECH audit deployment [L]

**Purpose / spec:** Implement §§4.6, 6 and 17 on a separate same-region VM.

**Files:** Create `relay/deploy/sharebridge-relay-gateway.service`, `relay/deploy/sharebridge-relay-frps.service`, `relay/deploy/firewall.nft`, `relay/deploy/install.sh`, `relay/README.md`, `docs/operations/phase4a-relay.md`; modify `control/deploy-testing.sh`, `docs/RELEASING.md`.

- [ ] **Step 1 — Test first:** create `relay/deploy/deploy_test.sh` to assert distinct unprivileged users, root-owned 0600 secrets/config, read-only filesystems except explicit run/state dirs, `LimitNOFILE`, `MemoryMax`, restart/log-rate policy, only 443/tcp + transport port public, and no public proxy/plugin/metrics/admin ports.
- [ ] **Step 2 — Verify RED:** run `bash relay/deploy/deploy_test.sh`; expect missing units/config.
- [ ] **Step 3 — Implement:** package gateway and pinned frps; provision dedicated transport certificate/key only; configure private mTLS; document restart ordering; add DNS audit commands proving relay wildcard and tunnel host are DNS-only and the zone publishes no HTTPS/SVCB/ECH records.
- [ ] **Step 4 — Verify GREEN:** run deploy test and `systemd-analyze verify` in a Linux container/VM; inspect firewall and service users; run secret-safe dry run.
- [ ] **Step 5 — Commit:** `git commit -m "ops(relay): add hardened separate-VM deployment"`.

### Task 36: Establish load/capacity baseline — §23.8 [M]

**Purpose / spec:** Validate §14 defaults and §18.5 safety without implementing Phase 4b selection.

**Files:** Create `relay/internal/integration/load_test.go`, `scripts/relay-capacity-gate.sh`; modify `docs/operations/phase4a-relay.md`.

- [ ] **Step 1 — Test first:** add one-agent/multi-agent throughput, limit saturation without cross-agent starvation, active stream beyond idle, no-byte idle close, absolute lifetime, cancellation and goroutine/FD/buffer plateau cases.
- [ ] **Step 2 — Verify RED:** run bounded local load with conservative thresholds; expect missing counters/plateau evidence.
- [ ] **Step 3 — Execute:** collect throughput, CPU, RSS, FDs, buffers, NIC saturation and per-agent bytes on the target VM size; choose global limit no higher than host FD budget; keep default bandwidth throttle disabled (cap deferred to Phase 4b per §14).
- [ ] **Step 4 — Verify GREEN:** record safe defaults and graphs/tables in operations doc; explicitly state results do not alter route selection and do not substitute for Phase 4b packet-impairment work.
- [ ] **Step 5 — Commit:** `git commit -m "test(relay): establish MVP capacity and safety baseline"`.

---

# M6 — Live acceptance and staged rollout

### Task 37: Deploy the dark separate-VM topology and pass §23.9 [L]

**Purpose / spec:** Execute §20 steps 1–4 without automatic selection, and prove the real deployment assumptions.

**Files:** Create `scripts/live-phase4a.sh`, `docs/operations/evidence/phase4a-environment.md`; modify `docs/operations/phase4a-relay.md`.

- [ ] **Step 1 — Acceptance first:** script `gate_23_9_dark_topology` checks control VM, new relay VM in same Hetzner region/private network, home Mac agent and real Immich; relay public 443/transport firewall; private mTLS sync; tunnel DNS certificate validation; snapshot health and restart ordering.
- [ ] **Step 2 — Verify RED:** run before provisioning with `RELAY_SELECTION_ENABLED` unset or false and no relay DNS/routes enabled; require failure for the missing topology/DNS/mTLS conditions while separately asserting the selection flag remains false.
- [ ] **Step 3 — Deploy dark:** install gateway/frps with no public relay wildcard/routes, deploy migration/protocol support, provision test namespace DNS, deploy agent TunnelManager/dual binding, and set the Task 20 operator flag `RELAY_SELECTION_ENABLED=false` explicitly in the control deployment.
- [ ] **Step 4 — Go/no-go:** verify no content cert/key or DNS/ACME credential exists on relay, proxy ports are loopback, and gate 23.9 passes. Failure blocks production rollout.
- [ ] **Step 5 — Commit evidence only:** `git commit -m "docs(relay): record dark topology validation"`.

### Task 38: Live owner-hairpin and deterministic interstitial acceptance [M]

**Purpose / spec:** Prove §19 acceptance #1 and #4 on the confirmed non-hairpin router, using the global-constraint authorization for isolated test-account relay selection while production/all-v2 selection stays disabled.

**Files:** Modify `scripts/live-phase4a.sh`, `docs/operations/evidence/phase4a-environment.md`.

- [ ] **Step 1 — Acceptance/setup first:** at the start of Task 38, explicitly set `RELAY_SELECTION_ENABLED=true` only in the isolated test-account deployment and record the captured deployment configuration as an immutable evidence reference in the acceptance artifacts. Confirm the production/all-v2 deployment remains false. Then add `acceptance_01_owner_hairpin` and `acceptance_04_interstitial_blackhole`, recording canonical URL, successful control preparation, browser direct failure start/end, exact relay navigation and final gallery status without secrets/codes in committed output.
- [ ] **Step 2 — Verify RED:** run with the isolated test-account flag true but the blackhole absent; require the interstitial-blackhole acceptance to fail.
- [ ] **Step 3 — Execute:** open the real share from owner LAN; separately blackhole direct after successful preparation; prove fallback occurs within the documented recipient bound and same share content loads over exact relay origin.
- [ ] **Step 4 — Verify GREEN:** repeat Chrome, Firefox and Safari; attach sanitized HAR/timing summaries and zero CSP violation result.
- [ ] **Step 5 — Commit evidence:** `git commit -m "test(live): prove hairpin and interstitial relay fallback"`.

### Task 39: Live cellular relay and Phase 3 content parity acceptance [M]

**Purpose / spec:** Prove §19 acceptance #2 and #11 with real Immich.

**Files:** Modify `scripts/live-phase4a.sh`, `docs/operations/evidence/phase4a-environment.md`.

- [ ] **Step 1 — Acceptance first:** add `acceptance_02_cellular_relay_video` and `acceptance_11_content_parity`, covering gallery/items/thumb/preview/original/archive/accounting and at least two valid `206` video seeks.
- [ ] **Step 2 — Verify RED:** block/unavailable direct and deliberately disable the isolated test-account relay route while running before the production/all-v2 selection flag is enabled; require failure. Restore that relay route with the Task 38 isolated test-account flag still true before Step 3.
- [ ] **Step 3 — Execute:** use a phone on cellular against real Immich and relay hostname; complete all content operations and cancellation/retry checks.
- [ ] **Step 4 — Verify GREEN:** compare statuses, security headers, bytes, Range and ledger outcomes with direct Phase 3 baseline; record sanitized results.
- [ ] **Step 5 — Commit evidence:** `git commit -m "test(live): prove cellular relay content parity"`.

### Task 40: Live relayOnly end-to-end acceptance [M]

**Purpose / spec:** Prove §19 acceptance #3 and the precise §6.1 privacy behavior.

**Files:** Modify `scripts/live-phase4a.sh`, `docs/operations/evidence/phase4a-environment.md`.

- [ ] **Step 1 — Acceptance first:** add `acceptance_03_relay_only`, instrumenting DNS mutations/lookups, WS open messages, probe calls, mapper calls and browser requests.
- [ ] **Step 2 — Verify RED:** current Phase 3 registration rejects relayOnly.
- [ ] **Step 3 — Execute:** create a new supported public Immich relayOnly share, register it, resolve canonical URL, load content through relay.
- [ ] **Step 4 — Verify GREEN:** require zero direct DNS mutation/lookup initiated by access, zero direct probe/open signal/browser request/mapper call, exact relay URL, and successful content. Confirm UI wording does not promise anonymity.
- [ ] **Step 5 — Commit evidence:** `git commit -m "test(live): prove relay-only never activates direct"`.

### Task 41: Capture the no-plaintext L4 passthrough proof [L]

**Purpose / spec:** Execute §18.4 and §19 acceptance #5.

**Files:** Create `scripts/l4-canary-capture.sh`; modify `docs/operations/evidence/phase4a-environment.md`, `docs/operations/phase4a-relay.md`.

- [ ] **Step 1 — Acceptance first:** define unique per-run canaries in an HTTP header, path, request body and response body; scan gateway public capture, gateway↔frps/loopback capture, process memory/bounded buffers and logs; inspect agent post-TLS handler.
- [ ] **Step 2 — Verify RED/control:** first run the scanner against a deliberately plaintext local control fixture to prove it detects every canary; do not deploy that fixture.
- [ ] **Step 3 — Execute:** capture a real browser TLS request through gateway+FRP to agent; inventory relay files/process args/listeners and certificate stores.
- [ ] **Step 4 — Verify GREEN:** canaries absent from every relay capture/buffer/log and present at agent; original SNI/Host/body observed; browser TLS stream byte-identical after FRP; no content-origin key/cert and no public TLS acceptor on gateway. Any plaintext is **NO-GO**.
- [ ] **Step 5 — Commit sanitized evidence:** `git commit -m "test(security): prove relay sees no content plaintext"`.

### Task 42: Live no-mapper enrollment, STUN mismatch and cold-budget acceptance — BLOCKING §23.6 [M]

**Purpose / spec:** Prove §19 acceptance #6, #12 and #13 using the Task 25 NAT setup, and record the blocking live reconnect/cadence/warm/cold-budget evidence for §23.6.

**Files:** Modify `scripts/live-phase4a.sh`, `docs/operations/evidence/phase4a-environment.md`.

- [ ] **Step 1 — Acceptance first:** add `acceptance_06_no_mapper_enrollment`, `acceptance_12_stun_mismatch`, and `acceptance_13_stun_cadence_cold_budget`.
- [ ] **Step 2 — Verify RED:** disable mapper and direct endpoint on a pre-Phase4a agent; require enrollment/share failure.
- [ ] **Step 3 — Execute:** enroll/register/serve solely over outbound tunnel; force egress mismatch and prove diagnostic `relay_fallback` plus zero public probe; reconnect and observe immediate/four-minute/no-warm-repeat cadence; force stale cold delay.
- [ ] **Step 4 — Go/no-go:** cold preparation finishes within four seconds or selects relay, never creates sticky failure, and later warm direct succeeds when observation/mapping are fresh. Save the immediate reconnect, four-minute refresh, no-warm-repeat, stale-cold and later-warm results as the §23.6 evidence; any failure is **NO-GO** for automatic fallback.
- [ ] **Step 5 — Commit evidence:** `git commit -m "test(live): prove no-mapper and STUN fallback behavior"`.

### Task 43: Live exact routing, recovery, lockdown and heartbeat acceptance [L]

**Purpose / spec:** Prove §19 acceptance #7–#10 and #15 on the real topology; #11 remains Task 39.

**Files:** Modify `scripts/live-phase4a.sh`, `docs/operations/evidence/phase4a-environment.md`.

- [ ] **Step 1 — Acceptance first:** add `acceptance_07_no_relay_open_signal`, `acceptance_08_exact_routing`, `acceptance_09_restart_recovery`, `acceptance_10_lockdown`, and `acceptance_15_heartbeat_tunnel_dns`.
- [ ] **Step 2 — Verify RED:** execute each against one deliberately unmet precondition (unknown route, stopped tunnel, stale snapshot) and require fail-closed outcome.
- [ ] **Step 3 — Execute:** test random/bare/tombstoned SNI, gateway/frps/agent restarts, direct+relay active connections during lockdown/unlock, dedicated tunnel DNS/cert, delayed Ping and true 45-second expiry.
- [ ] **Step 4 — Verify GREEN:** require zero relay open/ack/probe/mapper activity, no cross-agent routing, availability only after fresh presence, both route connections closed on lockdown, clean unlock with fresh credential, one delayed Ping tolerated and true expiry unavailable.
- [ ] **Step 5 — Commit evidence:** `git commit -m "test(live): prove exact routing recovery lockdown and heartbeat"`.

### Task 44: Final go/no-go, staged automatic fallback and rollback drill [S]

**Purpose / spec:** Enforce §20 steps 5–7 and the explicit §23 blocking rule.

**Files:** Create `scripts/phase4a-release-gate.sh`, `scripts/phase4a-release-gate_test.sh`, `docs/operations/evidence/phase4a-release-gates.txt`; modify `docs/operations/evidence/phase4a-environment.md`, `docs/operations/phase4a-relay.md`, `docs/RELEASING.md`.

- [ ] **Step 1 — Test first:** add shell tests `test_missing_blocking_evidence_is_rejected`, `test_false_relay_selection_flag_is_rejected`, `test_stale_dns_ech_firewall_audit_is_rejected`, and `test_complete_release_evidence_is_accepted`. Fixtures use the production manifest schema: exact lines `TASK_08=GO|<immutable-evidence-ref>`, `TASK_24=GO|<immutable-evidence-ref>`, `TASK_25=GO|<immutable-evidence-ref>`, `TASK_31=GO|<immutable-evidence-ref>`, and `TASK_42=GO|<immutable-evidence-ref>`; `TASK_36=RECORDED|<immutable-evidence-ref>` for non-selection capacity; `TASK_37=GO|<immutable-evidence-ref>` for topology; `ACCEPTANCE_01_15=GO|<immutable-evidence-ref>`; `RELAY_SELECTION_ENABLED=true|<immutable-evidence-ref>` whose reference identifies captured deployment configuration; plus `AUDIT_UNIX=<epoch>` for a DNS/ECH/firewall audit no older than 24 hours. An immutable evidence reference is either `git:<40-hex-commit>:<repo-path>` or `sha256:<64-hex-digest>:<artifact-path>`. Assert exact failures including `BLOCKED: missing GO evidence for TASK_24 (§23.4)`, `BLOCKED: RELAY_SELECTION_ENABLED must be true`, and `BLOCKED: DNS/ECH/firewall audit is missing or older than 24h`; the success line is `GO: Phase 4a relay fallback release gates satisfied`.
- [ ] **Step 2 — Verify RED:** run `bash scripts/phase4a-release-gate_test.sh`; expect nonzero because `scripts/phase4a-release-gate.sh` does not exist. The false-flag fixture must contain `RELAY_SELECTION_ENABLED=false|<immutable-evidence-ref>` and produce the exact refusal above; the gate process environment is not evidence of deployment state.
- [ ] **Step 3 — Implement gate and enable staged:** implement `scripts/phase4a-release-gate.sh <manifest>` to parse the manifest as data without sourcing it, reject missing/NO-GO/non-immutable references, require Tasks 8/24/25/31/42, require the Task 36 capacity record, Task 37 topology GO, all §19 acceptance evidence, the fresh audit, and the manifest deployment-state line `RELAY_SELECTION_ENABLED=true|<immutable-evidence-ref>`; validate its immutable reference and never infer deployment state from the gate process environment. Print the specific `BLOCKED:` reason and exit nonzero for any unmet check, and print the specified `GO:` line and exit zero only when releasable. Document the command in `docs/RELEASING.md`. Populate the initial manifest flag line with Task 38's immutable captured test-account deployment configuration, run the gate, record GO, and observe metrics/alerts through a complete lease/reconnect cycle while production/all-v2 remains false. Then set the flag true for all v2 accounts, capture that deployment configuration, replace the manifest flag reference with the new immutable evidence, and run the gate again. Task 25 is §23.5 evidence; Task 42 is §23.6 evidence. Do not add performance preference.
- [ ] **Step 4 — Rollback drill:** set the concrete Task 20 flag `RELAY_SELECTION_ENABLED=false` and disable route distribution while leaving interstitial, `prepare-route`, live STUN and recipient direct check active. Capture the rolled-back deployment configuration and set the manifest line to `RELAY_SELECTION_ENABLED=false|<immutable-evidence-ref>`. Require `scripts/phase4a-release-gate.sh` to fail with `BLOCKED: RELAY_SELECTION_ENABLED must be true`; rerun `TestRollbackModeDisablesRelaySelectionButKeepsNewDirectFlow`; verify new relay connections stop, existing policy closes as documented, DB fields remain safely unused, direct candidates still use the interstitial, relay-dependent cases return 503, and the old Phase 3 direct 302 behavior is not restored.
- [ ] **Step 5 — Commit:** `git commit -m "ops: record Phase 4a go decision and staged rollout"`.

---

## Acceptance and validation traceability

| Requirement | Named task(s) |
|---|---|
| §19 #1 owner hairpin | Task 38 |
| §19 #2 cellular/no direct path | Task 39 |
| §19 #3 relayOnly end to end | Task 40 |
| §19 #4 deterministic interstitial fallback | Tasks 24, 38 |
| §19 #5 no plaintext at gateway / §18.4 canary | Task 41 |
| §19 #6 CGNAT/UPnP-off enrollment | Tasks 10, 42 |
| §19 #7 no relay open-signal | Tasks 20, 31, 43 |
| §19 #8 exact routing | Tasks 3, 31, 43 |
| §19 #9 restart recovery | Tasks 33, 43 |
| §19 #10 lockdown | Tasks 30, 43 |
| §19 #11 content parity | Tasks 31, 39 |
| §19 #12 STUN mismatch | Tasks 19, 42 |
| §19 #13 cadence and cold budget | Tasks 18, 42 |
| §19 #14 no-JS/CSP fallback | Tasks 22, 24 |
| §19 #15 heartbeat margin and tunnel DNS | Tasks 8, 31, 35, 43 |
| §23.1 plugin operations/disconnect | Task 8 — blocking |
| §23.2 bind/ports/one-proxy/TLS | Task 8 — blocking |
| §23.3 fragmented TLS 1.2/1.3 + HTTP/1.1/2 | Task 31 — blocking |
| §23.4 4-second Safari/Chrome/Firefox + CORS/CSP/noscript | Task 24 — blocking |
| §23.5 real-NAT STUN/spoof/mismatch | Task 25 — blocking |
| §23.6 live reconnect cadence/warm/cold budget | Task 42 — blocking |
| §23.7 10-second Ping/45-second lease | Tasks 8, 31 — blocking |
| §23.8 capacity baseline only | Task 36 |
| §23.9 real topology/DNS/mTLS/restart ordering | Task 37 |

## Test execution strategy

### Hermetic CI

1. `relay/scripts/fetch-frp.sh` downloads the exact manifest release and verifies SHA-256 into the CI cache; tests never use a system `frps`/`frpc` or `latest` URL.
2. Fast unit suites run in all three modules with fake clocks and loopback sockets.
3. `SHAREBRIDGE_FRP_INTEGRATION=1` starts the real verified `frps`/`frpc` with ephemeral ports, gateway, control fixture, agent HTTPS test certificate and deterministic Phase 3 backend. It runs in an isolated temp directory and kills every child on test cleanup.
4. Both direct and relay base URLs execute the same Phase 3 parity assertions for status, headers, bytes, Range, archive/accounting, cancellation and concurrency.
5. Race/leak runs target presence, route reconciliation, stream registry, TunnelManager and limit release paths.

### Browser suite

- Preserve and extend `agent/internal/direct/parity_test.go` (the shipped Phase 3 suite is chromedp-based, despite earlier plans calling it Playwright-style) for quick Chromium/CSP regression.
- Add `e2e/browser/` Playwright projects for Chromium, Firefox and WebKit. The blocking live gate also runs actual Safari macOS and iOS Safari.
- Capture requests and CSP violations, not just rendered text. Pin exact control-derived destinations, absence of cookies/content on `/connect`, no direct request for relayOnly, four-second cancellation, manual relay cancellation and no-JS behavior. The chromedp parity regression must load the relay origin, open the gallery lightbox, seek video, and observe a successful `206` Range response.

### Live topology

- Follow `control/deploy-testing.sh` patterns: cross-build, copy minimal artifacts, source gitignored secret files without echoing, install systemd, wait for truthful health, and bootstrap throwaway credentials.
- Use the existing control Hetzner box, a new relay VM in the same region/private network, the home Mac agent, and real Immich over Tailscale. Browser/recipient traffic must not use Tailscale; Tailscale remains management/backend connectivity only.
- Record sanitized versions, digests, timestamps, timings and pass/fail in `docs/operations/evidence/phase4a-environment.md`; never commit share codes, API keys, private keys, IP-bearing packet captures, or raw credentials.

## Author self-review: normative spec coverage

The completed plan was checked once against every normative section requested by the brief:

| Spec section | Plan coverage |
|---|---|
| §4 architecture decisions | Tasks 1–10, 14–15, 21–25, 35–37 |
| §5 high-level architecture | File map, shared contracts, Tasks 4, 10–15, 26–31 |
| §6 names/DNS/origin binding and relayOnly wording | Tasks 3, 6, 10, 12, 26–27, 35, 40 |
| §7 enrollment, credential, presence and child lifecycle | Tasks 6–10, 14–15, 28 |
| §8 route distribution/gateway routing | Tasks 2–4, 11–15, 31–33 |
| §9 selection/interstitial/mixed sessions | Tasks 20–24, 29, 33, 38–40 |
| §10 STUN flow/cadence/policy | Tasks 16–19, 25, 42 |
| §11 agent WS and internal protocol | Tasks 6, 11–12, 17–18, 30 |
| §12 database migration/diagnostic-only fields | Tasks 5, 15, 19 |
| §13 agent integration, relayOnly and lockdown | Tasks 26–30 |
| §14 all resource defaults and FRP lockdown | Tasks 2, 4, 8–9, 13–14, 32, 35–36 |
| §15 gateway/frps/agent/WS/direct/stale failure behavior | Tasks 13–15, 28–30, 33, 43 |
| §16 L4 confidentiality, proxy prevention and log policy | Tasks 2–8, 31–32, 34–35, 41 |
| §17 deployment, health and metrics | Tasks 10–12, 34–37 |
| §18 unit/integration/browser/L4/failure-load strategy | Tasks 1–36 and test strategy |
| §19 acceptance #1–#15 | Traceability table; Tasks 31 and 38–43 |
| §20 rollout/rollback | Milestone table; Tasks 37 and 44 |
| §23 validation gates | Tasks 8, 24–25, 31, 36–37, 42 and final Task 44 |

The placeholder scan found no `TBD`, `TODO`, “implement later,” “similar to,” or “fix whatever” instructions. All 44 tasks have a size, named RED test/acceptance step, expected GREEN verification, exact file list and commit point. Shared names were checked against current code (`RouteDirect`, `RouteRelay`, `Binder.Allow`/`Revoke`, `SignalGate.Admit`, `Controller.EmitOpen`, `Controller.Probe`, three-second `ackTimeout`, `isSupportedGallerySession`, `config.Config.DefaultRelayOnly`, and the local admin files named in Task 27) and against the new shared-contract list above. The new `config.Config.RelaySelectionEnabled`/`RELAY_SELECTION_ENABLED` contract is defined and tested in Task 20 before Tasks 37 and 44 consume it.

## Risks

1. **Pinned FRP plugin mismatch:** the release may not surface Ping/options/disconnect semantics as assumed. Task 8 is an intentional stop gate; no silent downgrade is allowed.
2. **FRP heartbeat semantics:** an apparent 10-second config may not mean authenticated plugin Ping. Real observation, not config parsing, gates release.
3. **ClientHello edge cases/ECH:** fragmentation and TLS versions are testable; ECH must fail closed. An accidental HTTPS/SVCB `ech` record would disable routing, so deployment audits DNS repeatedly.
4. **Cross-browser timing:** Safari/iOS networking and AbortController behavior can diverge from Playwright WebKit. Actual devices are required before enablement.
5. **STUN protocol/NAT diversity:** authenticated receipt behavior may fail on restrictive UDP or multi-WAN. Correct behavior is relay fallback, but spoof/mismatch must never permit probing.
6. **Stale distributed state:** route/presence revisions cross process/host restarts. Boot IDs, snapshot-before-ready, finite leases and gap reconciliation are mandatory.
7. **Resource exhaustion/DDoS:** pre-dial limits and systemd budgets bound normal abuse but cannot promise volumetric protection; provider protection and monitoring remain operational dependencies.
8. **Tunnel child packaging:** each supported OS/architecture needs a verified `frpc`; unsupported platforms must fail install clearly rather than fetch dynamically at runtime.
9. **Lockdown races:** Binder removal, stream close, mapper deletion and tunnel stop are best-effort concurrent operations. Local gates must close first and tests must tolerate partial external failures without reopening availability.
10. **Control trust boundary:** L4 relay does not solve parent DNS/ACME compromise. Separate CT monitoring, CAA and least-privilege token work remains required before Phase 4b.

## Open items for the user

1. **FRP pin:** approve the exact `frpc`/`frps` release Task 1 records. This plan decides the distribution method—checksum-verified CI/developer fetch plus release bundling, no committed opaque binaries—but does not choose a release contrary to the user-owned pinning decision.
2. **Relay VM:** provide/approve the new same-region Hetzner VM, public IPv4, private-network address, SSH deployment principal and firewall change window.
3. **DNS:** provide the relay wildcard target IPv4 and `<relay-tunnel-host>` name; ensure the existing least-privilege DNS token can create the static test wildcard and tunnel A record without exposing the token.
4. **Transport and mTLS identity:** provide/approve issuance/storage for the dedicated FRP transport certificate and control↔gateway client/server certificates or private CA. None may be valid for browser content origins.
5. **Relay auth keys:** approve secure generation/storage paths for the control Ed25519 signing key and gateway verification key.
6. **Live acceptance access:** confirm the existing control test box, home Mac agent, non-hairpin router, cellular device/browser matrix, Tailscale-managed real Immich access, and maintenance windows for restart/blackhole/capture tests.
7. **Packet-capture retention:** approve where unredacted L4 captures live temporarily and how they are destroyed after sanitized gate evidence is recorded.

## Definition of done

Phase 4a is done only when all of the following are true:

- Tasks 1–36 pass in CI/local hermetic environments, including real checksum-verified `frps`/`frpc`, full Phase 3 parity on both origins, race/leak/limit/restart suites and browser automation.
- §23 gates 1–7 each have an explicit GO result before automatic fallback; gate 9 has a GO before production rollout. Gate 8 records capacity only and does not affect selection.
- Every §19 acceptance test #1–#15 has named, sanitized evidence from the required automated or live task, including real owner hairpin, cellular, no-mapper, relayOnly, STUN, restart, lockdown and heartbeat cases.
- The §18.4 canary/capture proof shows no plaintext or content key/certificate at the relay and byte-identical browser TLS delivery to the agent.
- Relay selection never emits `open_signal`, waits for `open_ack`, probes, updates direct DNS, or calls the mapper; relayOnly causes no direct access activity.
- Baseline enrollment works without mapper/direct DDNS; relay availability comes only from exact active route + gateway-observed current-generation proxy + fresh Ping lease.
- Exact unknown/bare/random/tombstoned SNI never reaches an agent, and revoke/lockdown closes indexed active streams as specified.
- Direct and relay share the same Phase 3 Binder/resolver/snapshot/membership/semaphores/archive ledger/download accounting, while relay connections never hold the direct mapping.
- The separate relay VM passes systemd/firewall/mTLS/transport-DNS/no-ECH audits and contains no parent DNS/ACME credential or browser-content key.
- Metrics/health/logging meet §§16.6–17.3, defaults meet §14, and §15 failure behavior is repeatably demonstrated.
- Automatic fallback is enabled first for test accounts, observed through a lease/restart cycle, then enabled for all v2 accounts; the rollback drill succeeds without restoring the Phase 3 direct-only 302 path.
- All three Go modules build/test cleanly, browser suites pass, documentation/evidence is current, and no deleted v1 transport or explicitly deferred Phase 4b/user-quick-win scope has been reintroduced.
