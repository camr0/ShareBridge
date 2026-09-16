# Spike: N-concurrent-session field test + SCTP patch A/B (completed-download metric)

Date: 2026-09-16
Branch context: `main` (v1 agent), test rig on the home agent host + disposable cloud client VM
Follows: `2026-09-15-e2e-sctp-patch-ab-field-test.md`, `2026-09-15-transport-investigation-results.md`, `2026-09-15-v1-transport-bugs.md`

## Questions (decision rule pre-registered)

1. **Striping**: do N concurrent sessions sum their goodput? If N=4 aggregate ≈ 4× single-session → build the striping layer. If ≈ 1–2× → stop.
2. **Patches**: do the SCTP tunings (CA-step `SettingEngine`; the forked `ssthresh` cap) improve *real completed-download* throughput on a clean low-RTT path?

## Method

- **Metric**: completed, SHA-1-verified 754 MB download, timed click → completion (agent logs `download complete` as the cross-check). This is the only metric immune to the wire/sink gap documented below; earlier windowed-progress measurements over-stated throughput.
- **Agent**: v1 rig on `<home-agent-host>` (residential ISP; measured 766 Mbps TCP upload to the test VPS; 6 cores).
- **Client**: disposable cloud VM, 4 vCPU / 15 GB, Ubuntu 24.04, headed Chrome 153 under Xvfb driven by a small Playwright script (join → click file → await completion). **11.9 ms RTT** to the agent — same class as the earlier campus tests.
- **Signaling**: `<test-signaling-host>` with `RELAY_PENDING_WAIT_WINDOW=45s` (bug 1 mitigation from `2026-09-15-v1-transport-bugs.md`).
- **Variants** (rig swaps the agent container only):
  - `u` = stock pion binary, no tuning env
  - `c` = stock binary + `SB_SCTP_CA_STEP=32768`
  - `f` = forked pion (`ssthresh` cap) + CA-step
- Instrumentation per run: client `vmstat` (1 Hz), agent-host NIC TX counters (1 Hz), agent-container CPU% (docker stats), plus iperf3 TCP/UDP probes for path capacity.

## Result 1 — concurrency (variant `c`)

| Cells | Wall (4-tab: first click → last completion) | Aggregate goodput | Per session | Modes |
|---|---|---|---|---|
| N=1 | 119.1 s | **50.7 Mbps** | 50.7 | Direct |
| N=2 | 231.0 s / 230.2 s (reproduced) | **52.2 / 52.5 Mbps** | ~26 | Direct×2 |
| N=4 | 189 s | 127.7 Mbps | ~32 | Direct×3 + Relay×1 (one direct attempt failed; fallback worked) |

A second N=4 run with all four Direct was attempted earlier in the evening on the same rig (details in session notes); sharing was fair (completions within ~3 s), no session starvation.

**Verdict: STOP.** A second session buys ~1.03×, not 2×. Four sessions reached 2.5× and even that was throttled by the client VM's ~240 Mbps ingress shaping (wire peaked at 248 Mbps). Striping N PeerConnections against this agent pipeline does not multiply goodput.

## Elimination chain (what it is NOT)

- **Client CPU**: min idle 15% single-tab on 4 vCPU; zero swap. (An earlier 2 vCPU client *was* CPU-bound — min idle 0% at one tab — and its numbers were discarded. Hardware class matters; this run is clean.)
- **Agent CPU**: 115% p90 / 152% max of one core, on a 6-core host. Not CPU-bound, not single-core-serialized.
- **UDP path policing**: iperf3 UDP agent-host → test VPS received **480–755 Mbps** (2–6% loss at offered 500–800 M). No ISP UDP ceiling.
- **Home upload**: 766 Mbps TCP. (Corrects an earlier belief that upload ≈ 262 Mbps was the ceiling — it never was.)
- **Client ingress cap** (~240 Mbps): binds only the N=4 cell; N=2 failed well under it.

**What remains**: agent-host NIC TX plateaus at ~160–190 Mbps of SCTP/UDP wire in *every* run regardless of N (p90: 186 @N=1, 160 @N=2, 179 @N=4), while the same path carries 480+ Mbps of plain UDP. Goodput ≈ wire ÷ ~3. The ÷3 is pion's spurious-retransmission overhead (rtoMin=1000 ms firing into its own unbudgeted send queue). The pool is **structural** — it does not respond to congestion-control parameters (see Result 2) and is not CPU-waiting.

## Result 2 — patch A/B (N=1, completed downloads)

| Variant | pion | Tuning | Completed 754 MB | Goodput |
|---|---|---|---|---|
| `u` | stock | none | 120.5 s | **50.05 Mbps** |
| `c` | stock | CA-step 32768 | 119.1 s / 119.9 s (n=2) | **50.7 / 50.35 Mbps** |
| `f` | fork | ssthresh + CA-step | 120.5 s | **50.05 Mbps** |

**The patches do nothing on this path.** All cells within ±0.7%. 

Reconciliation with the earlier campus A/B (CA-step 1.88× there):
- Campus was lossy Wi-Fi; this is clean fiber at 11.9 ms. CA-step accelerates the congestion-avoidance ramp; at 12 ms RTT the BDP is ~285 KB and even unpatched +1 MTU/RTT growth covers it in ~2.4 s — nothing to accelerate.
- The `ssthresh` fork targets the high-BDP overshoot collapse seen at rtt≈100 ms in the lab; at 12 ms RTT even an oversized initial window drains in ~200 ms — no collapse dynamics to prevent.
- Conclusion: the patches are **conditional** tuning for lossy/high-BDP paths, not unconditional wins. They should stay env-gated and default-off (which is already the case in `agent/internal/peer/peer.go`).

## Conclusions

1. **Do not build the striping layer.** The pre-registered decision rule fired: ~1× at N=2. (This also retires the wire-protocol offset proposal for scaling purposes — it would add protocol surface for no goodput.)
2. **Per-session ceiling ≈ 50 Mbps** (6.3 MB/s) on this path — consistent with the long-standing ~64 Mbps DataChannel observation — and it is *not* movable by the current patches on clean paths.
3. **The relay remains the throughput path** (it carries kernel TCP; the relay fallback engaged at full speed whenever direct failed tonight). `DefaultRelayOnly: true` is the right default for both throughput and privacy (direct exposes the agent's public IP to peers).
4. **Highest-leverage open thread**: identify the structural ~160–190 Mbps wire pool in the pion send path (send-queue pacing / DTLS record pipeline / retransmit queue interplay). If the ~3× retransmission overhead could be cut to ~1.2×, single-session goodput could reach ~130–155 Mbps — ~2.5–3×, with zero wire-protocol changes. That is where the next spike should aim.
5. **Bug confirmations in the wild**: bug 1 fix validated (relay fallback engaged within its 45 s window every time); speculative relay channels for peers that go direct expire with a noisy `StatusPolicyViolation` close after exactly the pending window (cosmetic, worth a clean cancel); bug 2 not reproduced — 4 concurrent peers on one API key shared the session fairly.

## Test-infrastructure notes (for re-running)

- Driver: Playwright joins N tabs at `https://<test-signaling-host>/s/<session-code>`, clicks the file, awaits `download` completion per tab; completion-mode driver avoids all in-browser throughput instrumentation (CDP progress polls hang under load and over-state via retransmission-inflated wire metrics).
- Client stack: Node 22 + Playwright Chromium + Xvfb (headed mode; headless shell was not exercised). Playwright's `downloadsPath` does **not** capture StreamSaver service-worker downloads, and same-name concurrent downloads make `download.path()` reject spuriously — rely on agent-side `download complete` logs as ground truth.
- Client measurement rule of thumb: any box with <4 vCPU will CPU-pin at one tab and poison concurrency results (verified: 2 vCPU client gave min-idle 0% at N=1).
