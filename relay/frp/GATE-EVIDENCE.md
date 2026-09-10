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
(`relay/internal/integration`) runs the real pinned `frps`/`frpc`, the real
`frpplugin.Server`, the real `presence.Registry`, the gateway, and an
agent-side HTTPS listener with a test CA in front of a deterministic fake
Phase 3 backend.

### Non-skippable gate mode (fix round, Task 31 review)

The gate is invoked in **required mode**, which cannot be satisfied by
skipping:

```text
cd relay && SHAREBRIDGE_FRP_INTEGRATION=1 SHAREBRIDGE_FRP_GATE=required \
  go test ./internal/integration -v -race -count=2 -timeout 900s
```

`TestMain` in required mode fails closed (non-zero exit, no case started) when
`SHAREBRIDGE_FRP_INTEGRATION=1` is unset or the pinned artifacts are missing,
unpinned, corrupt, or poisoned, and fails after the run when any of the seven
named cases did not start and complete — a SKIP is a failure, not a pass.
Observed: `-short` (which skips the heartbeat case) exits 1 with
`TestRealFRPRelayPresenceHeartbeatDelayAndExpiry: skipped`, and a `-run`
subset exits 1 listing the six cases that never started. Required mode also
re-hashes the pinned artifacts at gate time: the verified tarball against
`manifest.json`, the `frps`/`frpc` extracted from that tarball against the
staged cache executables, and `frps --version`/`frpc --version` against the
manifest version.

Default `go test ./...` remains skippable (`SHAREBRIDGE_FRP_INTEGRATION`
unset ⇒ 7 × SKIP, exit 0). Regression tests prove both directions:
`TestGateRequiredModeFailsClosedWithoutIntegrationEnv` (required-without-env
exits non-zero with a clear message; default-without-env exits 0 with SKIP),
`TestGateRequiredModeWithoutPinnedCacheFailsClosed` (poisoned cache fails
before `m.Run()`), and `TestGateIntegrityRejectsPoisonedCache` (tampered
cached binary, tampered tarball, missing binary, and missing tarball all
rejected).

### Gate-emitted pin evidence (verbatim from the required-mode run)

```text
GATE(EVIDENCE) §23.3 required gate — pinned FRP preconditions verified
GATE(EVIDENCE) frp.version=0.71.0 platform=darwin_arm64
GATE(EVIDENCE) manifest.sha256=d7903d1db66c714d817ebef82c43cbf061eb00d5e8c07118fea9ba490e8a3a93
GATE(EVIDENCE) config_frps_toml.sha256=f1337711b6e383d3d3e7e31745407e5384da2d15880a22cfb82c8152c291ac10
GATE(EVIDENCE) tarball.path=…/relay/.cache/frp/downloads/frp_0.71.0_darwin_arm64.tar.gz tarball.sha256=45be02b186860d375ed49a8941ae9569628a54bf14e67fc36b29c98c99dabcc6 tarball_matches_manifest=yes
GATE(EVIDENCE) frps.path=…/relay/.cache/frp/v0.71.0-sha256-45be02b1…/frps frps.sha256=71a4896060db4a9290bd830f48561334a3660545a0907c29dfade42f91f57037 frps.version=0.71.0
GATE(EVIDENCE) frpc.path=…/relay/.cache/frp/v0.71.0-sha256-45be02b1…/frpc frpc.sha256=3ce4ba70ffce7da4026940586c5f3454df50814f4c050d6560efc556b3adef48 frpc.version=0.71.0
GATE(EVIDENCE) cache_artifact_digest_sum=9cb1c154ff8e3e44d7add45e9d06282e460ac04435811f6dc548ba8123266a42
…
GATE(EVIDENCE) all 7 named §23.3 gate cases executed to completion
```

(Pin: FRP `v0.71.0`, darwin_arm64; long cache paths shortened to the module
form for readability — the run prints absolute paths. Every value above is
emitted by the gate itself; `frps`/`frpc` are staged only by
`relay/scripts/fetch-frp.sh` and no binary is committed.)

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
handshake record is deliberately split into N TLS records by an exact
record-layer fragmenter, so the gateway's fragmented/multi-record ClientHello
parser and its byte-for-byte replay run against a real handshake. The agent
side is recorded by a raw listener tap in front of TLS termination.

Fragmentation scope (corrected in the fix round): the gate asserts an
**exact** record count (2, 2, and 5 respectively — never `>=`) and the
fragmenter is proven to emit exactly the requested number of records for a
range of body lengths
(`TestRecordFragmenterEmitsExactlyRequestedRecordCount`). This is
**record-level fragmentation only**. Separate TCP `write()` calls do not prove
distinct kernel TCP segments, and this cross-platform stdlib-only harness has
no packet capture or `TCP_INFO` seam, so **no TCP-segmentation claim is made
anywhere**. The substantive claim is that the ClientHello spans five real TLS
records and the gateway replays those records byte-for-byte into a completing
handshake.

### Digest comparison table (from the fix-round required-mode `-race -count=2` run)

| case | TLS / ALPN | ClientHello records | bytes | browser SHA-256 | agent SHA-256 | extraAtAgent |
| --- | --- | --- | --- | --- | --- | --- |
| TLS 1.2 / HTTP/1.1 | TLS1.2 / http/1.1 | 2 | 551 | `ce4f7043345bd12ef8873e084b5863caa5bebec09d26f1d2e2b2eac2ccea8b9c` | `ce4f7043345bd12ef8873e084b5863caa5bebec09d26f1d2e2b2eac2ccea8b9c` | **0** |
| TLS 1.3 / HTTP/2 | TLS1.3 / h2 | 2 | 1819 | `412d98ffa776cce31400d2e0c93bc2fac0d9d96230e24ee0456a58efd18c933a` | `412d98ffa776cce31400d2e0c93bc2fac0d9d96230e24ee0456a58efd18c933a` | **0** |
| fragmented replay | TLS1.3 / http/1.1 | 5 | 1811 | `fee05659cbe33e678bb8dd5e298fd56aadda9366323f698ed1a18e0faada695c` | `fee05659cbe33e678bb8dd5e298fd56aadda9366323f698ed1a18e0faada695c` | **0** |

These three lines are copy-pasted from the `§23.3 evidence case=…` lines the
gate emitted in the run reported as `ok 134.267s` (the second `-count=2`
iteration produced different per-run digests — see below). Digests are per-run
(TLS ClientHello randoms/key shares differ every connection); equality/replay
is the invariant, not the digest value. `assertByteParity` now requires
non-empty captures, **exact post-quiescence length equality**, and
`ExtraAtAgent == 0` as a hard assertion (the old code only logged the extra
byte count and accepted any suffix).
`TestParityComparatorRequiresExactLengthAndNonEmptyCaptures`
and friends prove an injected trailing byte, a truncated capture, an empty
capture, and a same-length bit flip all FAIL.

Second-iteration emitted lines (same run, `-count=2`):

```text
§23.3 evidence case=TestRealFRPRelayEndToEndTLS12HTTP11 tlsRecords=2 browserBytes=551 agentBytes=551 extraAtAgent=0 browserSHA256=835fe008bb58d9bc949f09d11141508030f6a4d14af4d2c8a86f5637bfce621e agentSHA256=835fe008bb58d9bc949f09d11141508030f6a4d14af4d2c8a86f5637bfce621e
§23.3 evidence case=TestRealFRPRelayEndToEndTLS13HTTP2 tlsRecords=2 browserBytes=1819 agentBytes=1819 extraAtAgent=0 browserSHA256=95f11087054105479d239a32165087c32d4fbd7c620238af9fc1af1e99de6007 agentSHA256=95f11087054105479d239a32165087c32d4fbd7c620238af9fc1af1e99de6007
§23.3 evidence case=TestRealFRPFragmentedClientHelloReplay tlsRecords=5 browserBytes=1811 agentBytes=1811 extraAtAgent=0 browserSHA256=77cc5b0639dc94d018e6680833595870ec6c5c3a29769a5b9b61c56414cb4a94 agentSHA256=77cc5b0639dc94d018e6680833595870ec6c5c3a29769a5b9b61c56414cb4a94
```

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
requests; **every gateway dial, recorded through the real
`gateway.WithDialer` seam, targeted exactly the resolved route's loopback FRP
port** (with a non-empty-dial requirement so the negative is not vacuous); the
route table resolved the relay origin (positive control) but exposed no
route for the direct origin; and a real TLS hello carrying the direct
origin's SNI reached no agent. Scope note: the agent-module `SignalGate` and
port mapper cannot be instantiated from this stdlib-only relay test package;
their semantics are proven by `agent/internal/direct/opensignal_test.go`. The
test no longer reports dead `mapperCalls`/`signalAdmits` fixture counters.
- The parity assertion is a hard comparison: non-empty captures, exact
post-quiescence length equality, and `extraAtAgent == 0`.
- `TestRealFRPRelayPresenceHeartbeatDelayAndExpiry`: a 14 s freeze of the real
frpc does not flap the 45 s presence lease; sustained absence transitions
offline only at true lease expiry (~45 s after the last authenticated Ping).
- Agent-side `TestParityContentParityAcrossRouteKinds` in
`agent/internal/direct/parity_test.go` runs the Phase 3 content assertions
unchanged against both the direct and relay base URLs.

### Reproduction

```text
# RED (fix round, tests first) — required mode did not exist:
cd relay && go test ./internal/integration -v
# PASS — exit 0, 7 × SKIP in 0.261s (the reviewed forge-by-omission defect)
cd relay && go test ./internal/integration \
  -run 'TestGateRequiredMode|TestRecordFragmenterEmitsExactly' -v -count=1
# FAIL — exit 1: required mode exited 0; fragmenter emitted 4 records for parts=5, bodyLen=11
cd relay && go test ./internal/integration -run '^TestParityComparator' -count=1
# FAIL [build failed] — undefined: compareParity, checkFragmentation, checkDialTargets, hashCacheArtifacts

# GREEN (§23.3 non-skippable gate, race detector, twice):
cd relay && SHAREBRIDGE_FRP_INTEGRATION=1 SHAREBRIDGE_FRP_GATE=required \
  go test ./internal/integration -v -race -count=2 -timeout 900s
# PASS — exit 0; "all 7 named §23.3 gate cases executed to completion"; ok 134.267s

# Negative controls (proven by the subprocess regression tests, reproducible):
cd relay && SHAREBRIDGE_FRP_GATE=required go test ./internal/integration -v -count=1
# FAIL — exit 1: "required gate mode ... needs SHAREBRIDGE_FRP_INTEGRATION=1"
cd relay && go test ./internal/integration -v -count=1
# PASS — exit 0, 7 × SKIP
```

### Verdict

§23.3: **GO**. Fragmented ClientHello replay through gateway → FRP → agent is
byte-for-byte exact for TLS 1.2/1.3 with HTTP/1.1 and HTTP/2 — non-empty
captures, exact post-quiescence length equality, and `extraAtAgent=0` asserted
in every case (not merely logged). The gate is now non-skippable in required
mode: missing integration env, missing/poisoned pins, and any skipped or
incomplete named case all fail the run. Fragmentation is record-level (exact
counts 2/2/5); no TCP-segmentation claim is made.

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
| §23.3 fragmented TLS 1.2/1.3 + HTTP/1.1/2 replay / content parity | **GO** — required-mode gate (non-skippable: missing env, poisoned pins, skips, and incomplete cases all fail), gate-emitted version + binary SHA-256, non-empty captures with exact length equality and `extraAtAgent=0` for TLS 1.2/HTTP/1.1, TLS 1.3/HTTP/2, and a 5-record record-level fragmented ClientHello; exact routing, content parity, revocation, restart, and a wired relay-path dial/`/connect` audit |

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
