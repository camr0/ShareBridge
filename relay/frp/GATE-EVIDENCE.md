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

Two different gate environments exist and must not be conflated:

- `SHAREBRIDGE_FRP_GATE=1` is the Task 8 `internal/frptest` gate (below); that package
  still accepts it.
- The §23.3 `internal/integration` gate **rejects** `SHAREBRIDGE_FRP_GATE=1`: its only
  supported mode is `SHAREBRIDGE_FRP_GATE=required` together with
  `SHAREBRIDGE_FRP_INTEGRATION=1` (see the §23.3 section).

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

### Non-skippable gate mode (fix round, Task 31)

The gate is invoked in **required mode**, which cannot be satisfied by
skipping:

```text
cd relay && SHAREBRIDGE_FRP_INTEGRATION=1 SHAREBRIDGE_FRP_GATE=required \
  go test -v -race -count=1 -timeout 900s -skip '^TestRelayCapacity' ./internal/integration
```

The `-skip '^TestRelayCapacity'` scope keeps the package's §23.8 capacity/safety
suite (`load_test.go`) out of this blocking gate's exit code, so an exit-1 run is
always attributable to a genuine §23.3 case or gate guard rather than a
capacity-timing flake. The capacity suite is non-blocking and has its own gate
(`scripts/relay-capacity-gate.sh`, §3.6 of `docs/REPO-MAP.md`); the scope cannot
weaken the gate because required mode asserts every `requiredGateCases` entry
started and completed, and `TestGateScheduleScopesOutOnlyTheCapacitySuite` (which
runs under the gate command) fails if the pattern ever matches a gate case or
misses a capacity case. This fixes the whole-branch review's F1 signal-integrity
finding for the §23.3 gate.

`TestMain` in required mode fails closed (non-zero exit, no case started) when
`SHAREBRIDGE_FRP_INTEGRATION=1` is unset or the pinned artifacts are missing,
unpinned, corrupt, or poisoned, and fails after the run when any of the sixteen
named cases did not start and complete — a SKIP is a failure, not a pass.
The authoritative count is `len(requiredGateCases)` in
`relay/internal/integration/gate_test.go`: **16** (7 original §23.3 cases +
9 Task-33 failure/recovery cases). Count history: 7 → 14 in Task-33 fix
round 1 (the failure harness), 14 → 15 in fix round 2
(`TestRealFRPStaleSessionReplayDoesNotClearAHealthyReplacementTunnel`, the
generation-precision case), and 15 → 16 in fix round 3
(`TestRealFRPSameGenerationRecredentialSurvivesOlderCredentialReplay`, the
credential-precision case). See `task-33-report.md` "Fix round 3".
Observed (fix round 3): `-short` (which skips the heartbeat case) exits 1 with
`FAIL: required gate mode: 1 of 16 named §23.3 gate case(s) did not execute
cleanly: - TestRealFRPRelayPresenceHeartbeatDelayAndExpiry: skipped`, and a
`-run` subset exits 1 listing the 15 cases that never started. Required mode also
re-hashes the pinned artifacts at gate time: the verified tarball against
`manifest.json`, the `frps`/`frpc` extracted from that tarball against the
staged cache executables, and `frps --version`/`frpc --version` against the
manifest version.

Default `go test ./...` remains skippable (`SHAREBRIDGE_FRP_INTEGRATION`
unset ⇒ 16 × SKIP, exit 0). Regression tests prove both directions:
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
GATE(EVIDENCE) all 16 named §23.3 gate cases executed to completion
```

The run also emitted the three §23.3 byte-parity lines (`-v` prefix
`relay_test.go:<line>: ` shown exactly as emitted):

```text
    relay_test.go:70: §23.3 evidence case=TestRealFRPRelayEndToEndTLS12HTTP11 tlsRecords=2 browserBytes=551 agentBytes=551 extraAtAgent=0 browserSHA256=06f844741ef9fe553b8205d9cfd3c42e3271db800fa9a78ea52259b259532650 agentSHA256=06f844741ef9fe553b8205d9cfd3c42e3271db800fa9a78ea52259b259532650
    relay_test.go:108: §23.3 evidence case=TestRealFRPRelayEndToEndTLS13HTTP2 tlsRecords=2 browserBytes=1819 agentBytes=1819 extraAtAgent=0 browserSHA256=d2e73b5233d1ff5f058fdeba48b1f8b2d33e6f3b4e0e426ecd2a1f0b67eab0e4 agentSHA256=d2e73b5233d1ff5f058fdeba48b1f8b2d33e6f3b4e0e426ecd2a1f0b67eab0e4
    relay_test.go:136: §23.3 evidence case=TestRealFRPFragmentedClientHelloReplay tlsRecords=5 browserBytes=1811 agentBytes=1811 extraAtAgent=0 browserSHA256=499462c9116371c1b1ff0dc68eeed0d5c709c92bc0e28e77f8ca7fa4c612c42f agentSHA256=499462c9116371c1b1ff0dc68eeed0d5c709c92bc0e28e77f8ca7fa4c612c42f
```

`ok  sharebridge/relay/internal/integration  85.188s` (exit 0).

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
- `TestRealFRPGatewayRestartServesNothingUntilSnapshotAndPresence`
- `TestRealFRPFRPSRestartClearsPresenceAndTerminatesEstablishedStreams`
- `TestRealFRPAgentHTTPSReplacementAndFRPCRestartDuringIdleAndActiveTransfers`
  (agent HTTPS replacement + frpc restart with a fresh credential; **not** a
  daemon restart — the relay-only harness has no agent daemon, and the
  daemon-level behaviour is covered in `agent/internal/daemon`)
- `TestRealFRPStaleSessionReplayDoesNotClearAHealthyReplacementTunnel` (fix
  round 2: a replayed burned credential clears only the session it names, so a
  healthy newer-generation tunnel keeps its presence, streams and public path)
- `TestRealFRPSameGenerationRecredentialSurvivesOlderCredentialReplay` (fix
  round 3: the reset is credential-precise, so a delayed replay of a
  superseded SAME-generation credential does not clear the healthy replacement
  that re-credentialed at the same agent/port/generation join key)
- `TestRealFRPDirectOriginFailureMidTransferRecoversThroughCanonicalRelayLink`
  (the direct origin becomes unreachable mid-transfer; recovery through the
  canonical relay link is a fresh, complete, byte-identical transaction)
- `TestRealFRPControlSyncLossPastRouteLeaseKeepsEstablishedStream`
- `TestRealFRPRevokeDuringLongTransferClosesEstablishedStream`
- `TestRealFRPPresenceExpiryWithoutCloseProxy`

The last nine are the Task-33 §18.5 failure/recovery cases; all sixteen are
listed in `requiredGateCases` and guarded against omission by
`TestFailureSuiteCasesAreGateRegistered` (it parses `failure_test.go` and
`relay_test.go`, requires every `Test*` to be registered and to call
`beginGateCase`, and requires the parsed count to equal `len(requiredGateCases)`).

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

### Digest comparison table (fix round 3, from the required-mode `-race -count=1` run)

| case | TLS / ALPN | ClientHello records | bytes | browser SHA-256 | agent SHA-256 | extraAtAgent |
| --- | --- | --- | --- | --- | --- | --- |
| TLS 1.2 / HTTP/1.1 | TLS1.2 / http/1.1 | 2 | 551 | `06f844741ef9fe553b8205d9cfd3c42e3271db800fa9a78ea52259b259532650` | `06f844741ef9fe553b8205d9cfd3c42e3271db800fa9a78ea52259b259532650` | **0** |
| TLS 1.3 / HTTP/2 | TLS1.3 / h2 | 2 | 1819 | `d2e73b5233d1ff5f058fdeba48b1f8b2d33e6f3b4e0e426ecd2a1f0b67eab0e4` | `d2e73b5233d1ff5f058fdeba48b1f8b2d33e6f3b4e0e426ecd2a1f0b67eab0e4` | **0** |
| fragmented replay | TLS1.3 / http/1.1 | 5 | 1811 | `499462c9116371c1b1ff0dc68eeed0d5c709c92bc0e28e77f8ca7fa4c612c42f` | `499462c9116371c1b1ff0dc68eeed0d5c709c92bc0e28e77f8ca7fa4c612c42f` | **0** |

The table is the three `§23.3 evidence case=…` lines quoted verbatim above.
Digests are per-run (TLS ClientHello randoms/key shares differ every
connection); equality/replay is the invariant, not the digest value.
`assertByteParity` requires non-empty captures, **exact post-quiescence length
equality**, and `ExtraAtAgent == 0` as a hard assertion (the old code only
logged the extra byte count and accepted any suffix);
`TestParityComparatorRequiresExactLengthAndNonEmptyCaptures` and friends prove
an injected trailing byte, a truncated capture, an empty capture, and a
same-length bit flip all FAIL.

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
# RED (fix round 2, tests first) — the reset was agent-wide and droppable:
cd relay && go test -race -count=1 -timeout 120s \
  -run TestStaleSessionResetDoesNotClearAHealthyReplacementSession ./internal/presence
# FAIL — exit 1: registry_test.go:615: generation 2 unexpectedly offline
cd relay && go test -race -count=1 -timeout 120s \
  -run TestPluginSessionResetSurvivesDispatcherSaturation ./internal/frpplugin
# FAIL — exit 1: the reset path completed while the dispatcher was full: the §15.2 reset was dropped instead of back-pressured
cd relay && SHAREBRIDGE_FRP_INTEGRATION=1 go test -race -count=1 -timeout 300s \
  -run TestRealFRPStaleSessionReplayDoesNotClearAHealthyReplacementTunnel ./internal/integration
# FAIL — exit 1: failure_test.go:654: a stale generation-1 credential replay cleared the healthy generation-2 tunnel

# RED (fix round 3, tests first) — the reset was not CREDENTIAL-precise:
cd relay && SHAREBRIDGE_FRP_INTEGRATION=1 go test -race -count=1 -timeout 300s \
  -run TestRealFRPSameGenerationRecredentialSurvivesOlderCredentialReplay ./internal/integration
# FAIL — exit 1: failure_test.go:737: a delayed replay of the superseded same-generation credential cleared the healthy replacement session
# (compile-only RED at the same revision for the new CredentialJTI/CredentialIssuedAt
#  assertions in internal/presence and internal/frpplugin: the fields did not exist)

# GREEN (§23.3 non-skippable gate, race detector, fix round 3 revision):
cd relay && SHAREBRIDGE_FRP_INTEGRATION=1 SHAREBRIDGE_FRP_GATE=required \
  go test -v -race -count=1 -timeout 900s ./internal/integration
# PASS — exit 0; "all 16 named §23.3 gate cases executed to completion"; ok 85.188s
# NOTE (F1 signal-hygiene fix, later revision): this recorded run predates the
# `-skip '^TestRelayCapacity'` scope; the current documented command adds it so
# the §23.8 capacity suite can no longer pollute the gate's package exit code.

# GREEN (mandated repetition, fix round 3 revision):
cd relay && SHAREBRIDGE_FRP_INTEGRATION=1 go test -race -count=10 -timeout 30m ./internal/integration
# PASS — exit 0; ok 818.131s

# Negative controls (proven by the subprocess regression tests, reproducible):
cd relay && SHAREBRIDGE_FRP_GATE=required go test ./internal/integration -v -count=1
# FAIL — exit 1: "required gate mode ... needs SHAREBRIDGE_FRP_INTEGRATION=1"
cd relay && SHAREBRIDGE_FRP_GATE=1 go test ./internal/integration -v -count=1
# FAIL — exit 1: SHAREBRIDGE_FRP_GATE="1" is not supported; the only gate mode is SHAREBRIDGE_FRP_GATE=required
cd relay && SHAREBRIDGE_FRP_INTEGRATION=1 SHAREBRIDGE_FRP_GATE=required go test -short ./internal/integration -count=1
# FAIL — exit 1: "1 of 16 named §23.3 gate case(s) did not execute cleanly: - TestRealFRPRelayPresenceHeartbeatDelayAndExpiry: skipped"
cd relay && go test ./internal/integration -v -count=1
# PASS — exit 0, 16 × SKIP
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
| §23.3 fragmented TLS 1.2/1.3 + HTTP/1.1/2 replay / content parity | **GO** — required-mode gate over **16** named cases (non-skippable: missing env, a non-`required` gate mode, poisoned pins, skips, and incomplete cases all fail), gate-emitted version + binary SHA-256, non-empty captures with exact length equality and `extraAtAgent=0` for TLS 1.2/HTTP/1.1, TLS 1.3/HTTP/2, and a 5-record record-level fragmented ClientHello; exact routing, content parity, revocation, restart, presence expiry, the frps-reset session-precise and credential-precise clears, and a wired relay-path dial/`/connect` audit. Verified at this revision: `-race -count=1` required gate exit 0 in 85.188s and `-race -count=10` exit 0 in 818.131s |

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
