# FRP v0.71.0 gate evidence — GO (§23.1, §23.2, §23.3, §23.7)

Retried Task 8 per the 2026-09-04 amendment: the probe-confirmed readiness
predicate (§4.2/§7.3, `frp-readiness-spike-report.md`) replaces the falsified
Login+NewProxy+Ping presence assumption of the first attempt. The prior
`TestPinnedFRPBandwidthCap` entry at the bottom is superseded (server-side cap
deferred to Phase 4b per §14).

## Pin

- Manifest status: `approved` (user-approved exact release)
- `manifest.json` SHA-256: `d7903d1db66c714d817ebef82c43cbf061eb00d5e8c07118fea9ba490e8a3a93`
- `config/frps.toml` SHA-256: `f1337711b6e383d3d3e7e31745407e5384da2d15880a22cfb82c8152c291ac10`
- Verified fetched artifact: darwin_arm64 SHA-256
  `45be02b186860d375ed49a8941ae9569628a54bf14e67fc36b29c98c99dabcc6`
- Staged binary digests (darwin_arm64 cache entry):
  `frps` `71a4896060db4a9290bd830f48561334a3660545a0907c29dfade42f91f57037`,
  `frpc` `3ce4ba70ffce7da4026940586c5f3454df50814f4c050d6560efc556b3adef48`
- `frps --version`: `0.71.0`; `frpc --version`: `0.71.0`
- Binaries were obtained only through `relay/scripts/fetch-frp.sh` into its
  digest-keyed cache and re-verified by `TestPinnedFRPArtifactsVerifyChecksums`.
  No binary is committed.

## Commands

```text
# RED (amended gate tests first; compile failure: readiness/runtime/heartbeat
# fixture capabilities did not exist)
cd relay && SHAREBRIDGE_FRP_GATE=1 go test ./internal/frptest -run 'TestPinnedFRP' -v

# GREEN (same command, after the harness was implemented)
cd relay && SHAREBRIDGE_FRP_GATE=1 go test ./internal/frptest -run 'TestPinnedFRP' -v -count=1 -timeout 900s
# PASS — ok  sharebridge/relay/internal/frptest  154.5s (all TestPinnedFRP tests)

# Stability: readiness gate, 10 consecutive runs
cd relay && SHAREBRIDGE_FRP_GATE=1 go test ./internal/frptest \
  -run 'TestPinnedFRPReadinessProbeConfirmsOnlyRegisteredProxy' -v -count=10 -timeout 1800s
# PASS 10/10 (34.66s–34.87s per run; ok ... 347.9s)

# Baseline verification
cd relay && go test ./... && go build ./... && go vet ./...
cd relay && go test ./internal/frpplugin -race -count=1
# all ok
```

## §23.1 — plugin operations, ordering, correlation, readiness

`TestPinnedFRPInvokesRequiredPluginOperations` observed the real pinned frps
invoke `Login`, `NewProxy`, `NewUserConn`, `Ping`, and `CloseProxy` on the
mandatory plugin. First-observation order: `Login` first, `CloseProxy` last;
`NewUserConn` fired 18–20 ms after its `NewProxy` authorization and never
before it. Login metadata carried the credential + generation metadata (values
redacted); the real plugin's own event stream emitted the matching
Login/NewProxy authorization facts.

`NewUserConn` payload shape (correlation tuple, values redacted where
sensitive): `proxy_name` (exact control-assigned name), `proxy_type=tcp`,
empty FRP `user`, server-assigned `user.run_id`, Login-copied
`user.metas.sharebridge_generation`, and `remote_addr` — the connecting
socket's source address as seen by frps.

`TestPinnedFRPReadinessProbeConfirmsOnlyRegisteredProxy` (all five subtests
PASS, 10/10 consecutive full runs):

| subtest | observed result |
| --- | --- |
| healthy registration | probe confirmed in **1 attempt**; correlated callback 19 ms after NewProxy authorization, 0 ms after connect, 11 ms total (≤2.5 s budget); `remote_addr` matched the probe socket bit-for-bit; a browser-like stream on the same port kept passing payloads through the probe, and the agent target saw exactly the browser bytes plus the one zero-byte accept-mode probe connection |
| forced registration failure | with the assigned port pre-bound, NewProxy was authorized but downstream registration failed: 3 probe attempts TCP-connected (to the blocker) and produced **0 confirmations and 0 NewUserConn callbacks**; authenticated Pings continued (next Ping 7.47 s after the probe gave up); NewProxy was never retried (no-retry window ≥15 s) |
| stale old generation | generation-2 NewProxy authorized but "already exists" at registration; probes with the generation-2 expectation never confirmed; the stale generation-1 listener answered the probe socket with the OLD run id and generation — name-only correlation would have falsely confirmed; generation-2 NewProxy count stayed exactly 1 |
| frpc restart | graceful stop delivered CloseProxy and left the port refused with zero callbacks; the restarted frpc registered under a fresh server-assigned run id; probing with the OLD run id never confirmed (the new listener answered with the new run id); the new expectation confirmed exactly |
| frps restart | during the outage the probe was refused with zero callbacks; after restart the stock frpc re-Login reused its previous run id and the SAME one-use credential and was replay-rejected — no NewProxy re-registration (count stayed 1), no confirmation; a fresh-credential replacement client re-registered under a fresh run id and confirmed exactly |

Enforced-vs-notification-only semantics observed on the pinned release:

| operation | rejection semantics |
| --- | --- |
| `Login` | **enforced**: reject response denies the session; `TestPinnedFRPDisconnectsOnPluginRejection` saw only rejected Login attempts, no NewProxy/NewUserConn, and no bound listener across the client's reconnect loop |
| `NewProxy` | **enforced**: the runtime second-proxy rejection left the real frpc proxy in `start error` with the plugin's reject reason; no listener |
| `NewUserConn` | **enforced at the user connection**: frps closes the user connection before any work-conn request on a plugin reject (v0.71.0 `server/proxy/proxy.go`); this harness runs the Task 7 amendment's accept-mode probe shape |
| `Ping` | client-enforcing: a rejected Ping surfaces as a Pong error and the client closes its own session (observed during old-generation fencing) |
| `CloseProxy` | notification-only: its reject response is not claimed to disconnect; the enforced-rejection proof uses Login/NewProxy only |

NewProxy is confirmed to be pre-registration authorization: the forced-failure
subtest observed the authorization fact while the port was still held, with no
retry and no usable proxy — closing the first attempt's blocking finding via
the correlated-callback readiness requirement.

## §23.2 — loopback, ports, runtime rejection, transport TLS

- `TestPinnedFRPProxyBindAddrIsLoopback`: the registered proxy was reachable
  on `127.0.0.1` and forwarded to the loopback target; dials to every
  non-loopback local IPv4 interface address at the proxy port failed.
- `TestPinnedFRPAllowPortsAndOneProxy`: the plugin authorized exactly one
  proxy; the real frpc admin `/api/status` showed exactly one `running` TCP
  proxy; a quiet-range unassigned port stayed bindable by the test process
  (proving frps never bound it). Neighbor-of-ephemeral-port dialing was
  removed as unreliable: the fixture's own sockets occupy adjacent
  kernel-assigned ports.
- `TestPinnedFRPRuntimeRejectsSecondProxyAndOutOfRangePort` (runtime, not
  config-text):
  - second proxy: the real frpc delivered both NewProxy calls; the plugin
    recorded exactly one runtime rejection; admin status showed the extra
    proxy `start error` with err `request rejected`, the primary kept serving,
    and the extra port was never bound.
  - out-of-range port: the credential matched the requested port, so the
    plugin accepted the NewProxy; frps's own allowPorts enforcement then
    rejected registration at runtime — frps logged `new proxy [sb-sbdeadbeef]
    type [tcp] error: port not allowed`, admin status showed `start error`
    with err `port not allowed`, zero NewUserConn callbacks, and the port was
    never bound. Belt-and-braces: even a plugin-trusted client cannot escape
    `allowPorts`.
- `TestPinnedFRPVerifiesTransportTLS`: a real client pinning a distinct
  untrusted CA (`transport.tls.trustedCaFile` + `transport.tls.serverName`)
  produced **zero plugin calls over a 2.5 s reconnect window** and no listener
  ever bound — TLS verification fails closed before Login. v0.71.0 caveat
  (carried from the first attempt): an ABSENT `trustedCaFile` degrades to
  insecure skip-verify, so the Task 9 renderer must set both fields
  explicitly.

## §23.7 — Ping cadence, delayed-Ping tolerance, true lease expiry

- `TestPinnedFRPPingIntervalAndLeaseMargin`: first authenticated Ping arrived
  **1 ms after Login** (immediate, as §7.3 now records); subsequent intervals
  10.001 s / 10.001 s (bound 8–12 s); four nominal opportunities leave ≥4.99 s
  of margin inside the 45 s lease. TCP mux is disabled in the fixture and in
  `relay/config/frps.toml` because v0.71.0 suppresses application Pings when
  mux is enabled.
- `TestPinnedFRPDelayedPingToleratedUnderLease`: freezing the real frpc past a
  full heartbeat interval produced a 14.01 s Ping gap (≥1 missed 10 s beat);
  the session survived, cadence resumed at ~10 s, and the proxy kept serving —
  lease margin after the gap 30.99 s.
- `TestPinnedFRPTrueLeaseExpiryTransitionsOffline`: with the client frozen and
  no Ping possible, the proxy listener was still present at +20 s (two missed
  beats) and transitioned offline **45.12 s after the last authenticated
  Ping** (heartbeatTimeout=45 s), after which zero Pings were observed. True
  expiry — not early flapping — is the only offline transition.

## §23.3 — fragmented ClientHello replay and content parity (Task 31)

Gate §23.3 is the blocking byte-preservation proof: the browser TLS record
stream captured **before the gateway** must be byte-identical to the stream
captured **at the agent after FRP decapsulation** (spec §16.1), for
fragmented TLS 1.2/1.3 with HTTP/1.1 and HTTP/2. The Task 31 hermetic suite
(`relay/internal/integration`, env-gated `SHAREBRIDGE_FRP_INTEGRATION=1`)
runs the real pinned `frps`/`frpc`, the real `frpplugin.Server`, the real
`presence.Registry`, the gateway, and an agent-side HTTPS listener with a
test CA in front of a deterministic fake Phase 3 backend.

Pin (unchanged from the Task 8 entry): FRP `v0.71.0`, darwin_arm64 cache
SHA-256 `45be02b186860d375ed49a8941ae9569628a54bf14e67fc36b29c98c99dabcc6`;
staged `frps` SHA-256 `71a4896060db4a9290bd830f48561334a3660545a0907c29dfade42f91f57037`,
`frpc` SHA-256 `3ce4ba70ffce7da4026940586c5f3454df50814f4c050d6560efc556b3adef48`.

### Named tests

- `TestRealFRPRelayEndToEndTLS12HTTP11`
- `TestRealFRPRelayEndToEndTLS13HTTP2`
- `TestRealFRPFragmentedClientHelloReplay`
- `TestRealFRPExactRouting`
- `TestRealFRPContentParity` (content / range-seeks / cancellation /
  accounting / concurrency / revocation / tunnel-restart)
- `TestRelayPathNeverEmitsOpenSignal`
- `TestRealFRPRelayPresenceHeartbeatDelayAndExpiry` (§18.2 heartbeat at the
  presence layer; the FRP-level delay/expiry cases remain in
  `relay/internal/frptest/heartbeat_gate_test.go`)

The browser-side bytes are recorded under a `crypto/tls` client whose first
handshake record is deliberately split into N TLS records by a record-layer
fragmenter, so the gateway's fragmented/multi-record ClientHello parser and
its byte-for-byte replay run against a real handshake. The agent side is
recorded by a raw listener tap in front of TLS termination.

### Digest comparison table (representative `-race -count=2` run)

| case | TLS / ALPN | ClientHello records | bytes | browser SHA-256 | agent SHA-256 | exact |
| --- | --- | --- | --- | --- | --- | --- |
| TLS 1.2 / HTTP/1.1 | TLS1.2 / http/1.1 | 2 | 551 | `82c4a298a2eeb1e979eacc41727a2d78fe4b69920e080cda6f6e0802faf7dee8` | `82c4a298a2eeb1e979eacc41727a2d78fe4b69920e080cda6f6e0802faf7dee8` | **yes** |
| TLS 1.3 / HTTP/2 | TLS1.3 / h2 | 2 | 1819 | `0e95d6218d74d0678f9b908846a13fa63e6002df3a6b8830e51628e7456e365d` | `0e95d6218d74d0678f9b908846a13fa63e6002df3a6b8830e51628e7456e365d` | **yes** |
| fragmented replay | TLS1.3 / http/1.1 | 5 | 1811 | `a8103fe945882136420b2475289d800b0360828858d0fe935472d040522f1359` | `a8103fe945882136420b2475289d800b0360828858d0fe935472d040522f1359` | **yes** |

Digests are per-run (TLS ClientHello randoms/key shares differ every
connection); equality/replay is the invariant, not the digest value. In every
run `agentExtraBytes=0`.

### §23.3 supporting results

- `TestRealFRPExactRouting`: a valid exact origin reaches only its owning
tunnel; random, bare, unknown, and tombstoned SNI reach no agent (gateway
logs `no such route` / `route is inactive`).
- `TestRealFRPContentParity`: page, items, thumb, preview, original,
archive manifest+parts, and five `206` byte-range seeks are identical in
status, content headers, and body between the direct origin and the relay
origin; cancellation propagates to the agent handler; per-endpoint accounting
is non-zero; eight concurrent relay requests succeed; revoke closes the
established stream and blocks new connections; a graceful frpc stop
transitions presence offline and a restart restores availability.
- `TestRelayPathNeverEmitsOpenSignal`: the relay path issued zero `/connect`
requests and zero port-mapper/open-signal operations, and the gateway holds
no direct origin.
- `TestRealFRPRelayPresenceHeartbeatDelayAndExpiry`: a 14 s freeze of the real
frpc does not flap the 45 s presence lease; sustained absence transitions
offline only at true lease expiry (~45 s after the last authenticated Ping).
- Agent-side `TestParityContentParityAcrossRouteKinds` in
`agent/internal/direct/parity_test.go` runs the Phase 3 content assertions
unchanged against both the direct and relay base URLs.

### Reproduction

```text
# RED (package did not exist):
cd relay && SHAREBRIDGE_FRP_INTEGRATION=1 go test ./internal/integration -v
# FAIL — directory not found

# GREEN (§23.3 byte gate, with the race detector, twice):
cd relay && SHAREBRIDGE_FRP_INTEGRATION=1 go test ./internal/integration \
  -v -race -count=2 -timeout 600s
# PASS — ok sharebridge/relay/internal/integration 121.7s
```

### Verdict

§23.3: **GO**. Fragmented ClientHello replay through gateway → FRP → agent is
byte-for-byte exact for TLS 1.2/1.3 with HTTP/1.1 and HTTP/2; no mismatch,
truncation, or injected byte was observed in any run (any mismatch would have
been a NO-GO).

## Superseded entry

`TestPinnedFRPBandwidthCap` (first attempt): **SUPERSEDED**. Its underlying
policy proof (the Task 7 plugin rejects proxy-declared bandwidth options) is
retained by `go test ./internal/frpplugin` and remains correct fail-closed
behavior, but the operator server-side bandwidth cap it argued about is
deferred to Phase 4b per §14, so the gate test was removed rather than
retained under a misleading name.

## Result

| validation gate | verdict |
| --- | --- |
| §23.1 plugin operations / ordering / correlated readiness | **GO** — all five operations invoked with correlation-grade metadata; probe confirms only registered proxies in one attempt; failure/stale/restart cases never confirm; Pings continuing without readiness can no longer present as online |
| §23.2 loopback / runtime port+proxy enforcement / transport TLS | **GO** — loopback-only binding proven; second-proxy and out-of-range ports rejected at runtime by the plugin and by frps allowPorts respectively; transport TLS fails closed with a pinned untrusted CA |
| §23.7 Ping cadence / delayed-Ping tolerance / true expiry | **GO** — immediate first Ping, ~10 s cadence, one missed beat tolerated with ≥31 s lease margin, offline only at the true 45 s expiry |
| §23.3 fragmented TLS 1.2/1.3 + HTTP/1.1/2 replay / content parity | **GO** — browser-before-gateway and agent-after-FRP streams byte-identical (SHA-256 equal, 0 extra agent bytes) for TLS 1.2/HTTP/1.1, TLS 1.3/HTTP/2, and a 5-record fragmented ClientHello; exact routing, content parity, revocation, restart, and no-open-signal all proven |

Blocking Task 8 does not by itself enable `RELAY_SELECTION_ENABLED`; Task 31
is now also GO (§23.3 above), and Tasks 24, 25, and 42 plus the Task 44 GO
decision remain outstanding per the global constraints. Carry-forwards for
Tasks 9/14 (unchanged from the spike report):
credentials must be computed from one clock reading; the frps-restart recovery
path needs `relay_credential_request` re-issue because replayed jti values are
burned; TCP mux must stay disabled; the renderer must set `trustedCaFile` +
`serverName` + `poolCount=1` explicitly.

No credential, Basic secret, token, private key, or certificate content is
recorded here. Run identifiers are server-assigned random hex values with no
replay value.
