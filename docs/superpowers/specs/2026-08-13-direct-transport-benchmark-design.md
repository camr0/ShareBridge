# Direct Transport Benchmark Harness — Design

**Date:** 2026-08-13
**Status:** Approved for planning
**Scope:** `agent/cmd/benchdirect` (new), zero production-code changes

## Purpose

Measure the throughput of ShareBridge's **direct** WebRTC data path and answer one question:

> Is direct-mode throughput limited by the browser's SCTP association (a fundamental
> transport ceiling), or by ShareBridge's own chunking, scheduling, buffering, copying,
> source reads, or browser-side reconstruction?

This benchmark is the empirical gate that decides whether direct mode needs ordinary
tuning, multiple independent `PeerConnection`s, or deliberately lowered performance
expectations. It does **not** change the transport; it measures it.

## Non-Goals

- Not a product feature. The harness is deliberately **disposable** and must never
  leak abstractions into the production transport.
- Not a NAT-traversal or signaling test. Everything runs on loopback.
- Not a substitute for product end-to-end tests (those are a future Playwright
  concern on the `signaling-server/web` side).
- Not yet a multiple-`PeerConnection` striping implementation. Multi-connection
  support is a *measurement* parameter (1/2/4), not a production feature.

## Hard Constraints

1. **Zero production-code changes.** The bench imports `agent/internal/*` and drives
   it as a black box through existing interfaces. Instrumentation comes from Go
   `pprof`, the already-exposed `BufferedAmount()`, and end-to-end bytes/time.
2. **No API keys, no signaling server, no TURN, no PocketBase.** Self-contained.
3. **CLI-runnable.** `go run ./cmd/benchdirect ...` launches headless Chrome itself.
4. **Single Go binary + installed Chrome.** No Node toolchain on the bench side.
5. **Controlled latency and loss.** Simulated in-process, no kernel tools, no root.

## Core Design: Isolate the Ceiling from the Implementation

The harness has two modes sharing one binary, so a single number is always
interpretable against another:

| Mode | Go sender | Browser receiver | What it isolates |
|---|---|---|---|
| **A — raw** | bare pion `DataChannel` pump, tunable chunk/watermarks | minimal JS byte counter | the **transport ceiling** (browser SCTP + pion + chunking) |
| **B — go-sender** | real `peer` + `transfer.Manager` `sendWithBackpressure` loop (fake in-memory backend) | minimal JS receiver: sends a protocol request, decodes the 14-byte chunk frame, counts bytes | **Go sender overhead** vs A |
| **B-full — (later, optional)** | same as B | real `downloadPipeline.js` / `sw.js` receiver | **browser reconstruction overhead** vs B |

Decision logic:

- **A ≈ B** → sender is already at the ceiling; to go faster, raise the ceiling
  (multiple `PeerConnection`s, transport change).
- **A ≫ B** → the Manager's polling/watermark/chunking/framing copies are leaking
  throughput; optimize the implementation.
- **A slow even at RTT 0** → fundamental browser-SCTP limit, not RTT or our code.

`B-full` is deliberately deferred: it requires running the full app's JS, which is
tangled with signaling and the Service Worker. Build A and B first.

### Direct-path send loop (what mode B actually measures)

The direct path does **not** use the `multilane.Scheduler` — that is wired only into
the encrypted relay transport (`relaychannel`). In direct mode each lane is its own
`DataChannel`, and the sender loop is `Manager.sendWithBackpressure`:

- `chunkSize = 64 KiB`
- `maxBuffer = 5 MiB` (high-water mark)
- `sleepInterval = 10 ms` — the sender **polls** `time.After(10ms)` while
  `BufferedAmount() > maxBuffer`, rather than waiting on an event-driven
  `bufferedamountlow` callback.

That 10 ms polling granularity is a prime throughput suspect: at high bandwidth the
sender can sleep up to 10 ms per iteration while the buffer drains. Mode B exercises
this loop for real. Mode A defaults to an event-driven watermark (the ideal), so
`A(event)` vs `B` includes the poll in the delta, while a separate `A(event)` vs
`A(poll)` sweep isolates the poll's cost directly.

Because the streaming methods are unexported, mode B drives the Manager through its
public surface: a real protocol request (e.g. a file request) via `HandleMessage`
triggers `streamFile` → `sendWithBackpressure` against a fake in-memory
`StorageBackend`. The page decodes the 14-byte chunk envelope
(`type + version + uint64 operation + uint32 generation`) and counts payload bytes.

## Latency Injection: UDP Shim

A tiny in-process UDP forwarder sits between Chrome and the Go peer. It is **dumb**:
it never decrypts or terminates DTLS/SCTP; it holds each datagram in a delay queue,
then forwards. WebRTC's own stacks cannot distinguish shim queue time from real
network RTT.

```
Chrome ──► shim listener PA ──[hold RTT/2]──► Go peer real socket R
Chrome ◄── shim listener PB ◄──[hold RTT/2]── Go peer real socket R
```

### Forcing traffic through the shim (candidate rewrite)

WebRTC chooses its UDP path via ICE, so a sidecar shim would be ignored. Instead the
shim *is* the path: rewrite the `a=candidate` addresses in the SDP so each peer
believes the other lives at a shim port.

1. Go creates the offer; read its real host candidate (`127.0.0.1:R`).
2. Rewrite `R → PA` in the offer; configure shim listener PA to forward to `R`.
3. Chrome creates the answer; read its real host candidate (`127.0.0.1:C`).
4. Rewrite `C → PB` in the answer; configure shim listener PB to forward to `C`.
5. Apply the rewritten answer to the Go peer.

Because both peers only ever learn shim addresses for the other, the shim is the
**only** viable path — ICE cannot bypass it. Both directions are delayed `RTT/2`,
so total added RTT equals the dialed value.

### mDNS

- **Go/pion:** default is `MulticastDNSModeQueryOnly` (verified in `pion/ice`
  `agent.go:442`), so the offer contains plain IP host candidates. No change needed.
- **Chrome:** launch with `--disable-features=WebRtcHideLocalIpsWithMdns` so its
  answer also carries a plain `127.0.0.1` candidate. This flag is set by chromedp
  in bench code.

### Loss / jitter (bonus, same shim)

The shim's forward path can also drop / duplicate / reorder datagrams by policy,
giving packet-loss simulation with no additional architecture. Loss is a phase-2
parameter; latency is phase 1.

## Components

```
agent/cmd/benchdirect/
  main.go        CLI + orchestration; launches Chrome via chromedp
  shim.go        UDP latency/loss shim (listeners PA/PB, delay queue)
  sdp.go         read + rewrite candidate address/port in offer/answer
  rawbench.go    mode A: bare pion DataChannel pump
  prodbench.go   mode B: peer + Manager sendWithBackpressure loop, fake backend
  fakebackend.go in-memory StorageBackend / GalleryBackend
  server.go      local HTTP server: serves web/, /start, /answer
  report.go      JSON + human output
  web/           go:embed static page
    bench.js     receiver: create peer, count bytes, expose window.__benchResult
  *_test.go      unit tests (shim, sdp, fakebackend) + smoke test
```

### Key implementation notes

- **`peer.New(nil, false)`** returns a `*Peer` that already satisfies
  `multilane.ChannelSet`. The bench passes it to `transfer.NewManager(...)`.
- **`transfer.NewManager(channels, StorageBackend, maxDownloads)`** and
  `NewGalleryManager` take injected interfaces, so the bench supplies an in-memory
  backend that streams pseudo-random bytes on demand. No production edit.
- **Mode A** does not use `peer` at all — it builds its own `webrtc.PeerConnection`
  with a custom `SettingEngine` in bench code, so it is fully self-contained and
  immune to any production construction quirks.
- **Reusable payload buffer** by default (send the same `--chunk`-sized buffer
  repeatedly) to keep allocation cost out of the measurement; optional
  `--unique-data` flag to regenerate per chunk if allocation cost is itself the
  subject of study.
- **SDP exchange over local HTTP**, not CDP evaluate gymnastics: the page fetches
  `/start` (returns offer + config), does `setRemoteDescription`, creates the
  answer, and POSTs it to `/answer`. chromedp is used to launch/navigate and to
  read the final `window.__benchResult`. Loopback `http://127.0.0.1` is a secure
  context in Chrome, so `RTCPeerConnection` works without TLS.

## CLI Contract

```
go run ./cmd/benchdirect \
  --mode raw|prod            # A or B
  --rtt 0|25|50|100          # added RTT in ms
  --loss 0.0                 # packet loss fraction (phase 2)
  --size 512MiB              # total bytes to send
  --chunk 16KiB              # bytes per send
  --backpressure event|poll # mode A watermark mechanism (default event)
  --peer-conns 1             # 1|2|4 (phase 2)
  --duration 30s             # alternative to --size
  --unique-data              # regenerate payload per chunk
  --out results.json         # JSON output (default: - for stdout)
  --cpuprofile cpu.pprof     # optional pprof CPU profile path
```

Defaults target a quick, repeatable run on a typical dev machine.

## Data Flow (mode A, illustrative)

1. `main` parses flags; starts the shim (PA/PB) and the local HTTP server.
2. `main` launches headless Chrome via chromedp with the mDNS flag; navigates to
   `http://127.0.0.1:<port>/`.
3. Go side builds the `PeerConnection`, creates the offer, rewrites its candidate to
   PA, and publishes it at `/start`.
4. Page fetches `/start`, sets remote description, answers, POSTs to `/answer`.
5. Go side reads the answer, rewrites its candidate to PB, applies it. ICE connects
   through the shim; both directions now carry the configured delay.
6. Sender pumps `--size` bytes in `--chunk` chunks through the `DataChannel`,
   respecting `bufferedAmount` watermarks. Page counts bytes on `onmessage`.
7. Sender signals completion (sentinel or channel close); page records
   `{receivedBytes, elapsedMs}` and sets `window.__benchResult`.
8. chromedp reads the result; `report.go` merges sender-side stats (bytes sent,
   `BufferedAmount` samples, pprof) and writes JSON.

Mode B differs only in steps 5–6: the page sends a real protocol request, and bytes
flow through `Manager.sendWithBackpressure` (the 64 KiB / 5 MiB / 10 ms poll loop) →
the `Peer` bulk/media DataChannel → the page, which strips the 14-byte chunk envelope
before counting. No `Scheduler` and no lane-envelope byte — those are relay-path only.

## Metrics & Output

Captured per run:

- Sustained MB/s (not just peak), peak MB/s.
- Bytes sent / received / lost (via `BufferedAmount` deltas and sender accounting).
- RTT actually observed (baseline probe before transfer).
- `BufferedAmount()` time series summary: min / max / mean / p95.
- pprof CPU profile path and peak heap allocs (sender side).
- Loss/retransmission stats if pion exposes them at the `DataChannel`/SCTP layer.

Output is a single JSON object, plus a one-line human summary on stderr. JSON lets
matrix runs be diffed and plotted without a bespoke report format.

## Test Matrix

Phase 1 (decide tuning vs ceiling):

```
mode ∈ {raw, prod}
rtt  ∈ {0, 25, 50, 100}
chunk ∈ {8KiB, 16KiB, 32KiB, 64KiB}   # sweep, one variable at a time
```

Phase 2 (only if phase 1 implicates the association):

```
peer-conns ∈ {1, 2, 4}
loss ∈ {0, 0.5%, 1%}
```

Compare against a raw HTTPS baseline measured separately under the same loopback
conditions (a trivial Go TLS server + Chrome fetch), to distinguish "WebRTC/SCTP
is the ceiling" from "the machine/loopback itself is the ceiling."

## Decision Criteria

- **Multi-connection scales** → pursue a bounded connection pool + bulk striping
  (as a separate, designed feature — not in this harness).
- **Multi-connection does not scale but HTTPS does** → browser WebRTC or shared
  implementation is the bottleneck; reassess direct-mode expectations.
- **Neither scales** → investigate home upload, source storage, or network — out
  of this harness's scope.
- **WebRTC far below its expected RTT/window curve** → fix the implementation
  (the 10 ms polling `sleepInterval`, `chunkSize`, `maxBuffer` watermark, chunk-frame
  copies) before changing architecture.

## Harness Testing

- **Unit:** shim delay/loss correctness (send datagram, assert arrival time/absence),
  SDP candidate rewrite (port swap, no corruption of other fields), fake backend
  streams exactly N bytes.
- **Smoke:** a mode-A run with `--rtt 0 --size 8MiB` that completes and reports a
  positive MB/s. Skip when Chrome is unavailable (`testing.Short()` or a runtime
  probe), so the unit suite stays runnable in CI without a browser.

## Risks & Mitigations

| Risk | Mitigation |
|---|---|
| ICE selects a direct path, bypassing the shim | Both candidates rewritten, so only shim addresses are known; verify by logging the selected candidate pair (pion `OnSelectedCandidatePairChange`, Chrome via `getStats`) |
| Chrome mDNS obscures its host candidate | `--disable-features=WebRtcHideLocalIpsWithMdns` launch flag |
| chromedp flakiness / version drift | Pin chromedp; deterministic `/start`→`/answer` protocol; timeouts + retry; smoke test |
| Port collisions | Bind shim/HTTP on `127.0.0.1:0` and read assigned ports |
| Loopback timing noise at RTT 0 | Probe actual RTT first; report it with results |
| `agent/go.mod` gains chromedp | Go links only imported packages, so `cmd/agent` is unaffected; chromedp is imported only by `cmd/benchdirect` |

## Dependencies

- `github.com/chromedp/chromedp` (bench-only; not linked into `cmd/agent`).
- Existing `github.com/pion/webrtc/v4` and `github.com/pion/datachannel` (already
  present for mode A's direct `PeerConnection`).
- No new production dependencies.

## Out of Scope / Future

- **Playwright product e2e** (cross-browser, richer assertions) — separate effort on
  `signaling-server/web`, not this harness.
- **StreamSaver.js stress test** — a JS-side download stress test fits the future
  Playwright e2e suite (cross-browser, already-JS), not the Go bench. Noted as a
  follow-up idea, not part of this spec.
- **Multiple-`PeerConnection` striping as a feature** — measurement parameter here,
  designed separately if the data justifies it.

## Resume Point

Implement in this order:

1. `shim.go` + `sdp.go` + unit tests (no browser needed).
2. `server.go` + `web/bench.js` + chromedp launch/smoke test (mode A, RTT 0).
3. `rawbench.go` (mode A) end to end.
4. Latency matrix on mode A; record the ceiling curve.
5. `fakebackend.go` + `prodbench.go` (mode B) end to end.
6. Latency matrix on mode B; compare against A; apply decision criteria.
