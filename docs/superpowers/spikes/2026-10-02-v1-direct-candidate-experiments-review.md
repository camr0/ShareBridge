# Candidate experiments for v1 direct — adversarial review (2026-10-02)

**Status:** review complete; **nothing run**. No code changed, no rig touched, no instance state altered.

**Purpose.** E1–E32 concluded that the v1-direct ceiling is the browser's WebRTC DataChannel receive path
(`2026-09-18-v1-direct-optimization-verdict.md` + F19–F24). This document reviews **15 candidate follow-up
experiments** proposed after that campaign, decides which are worth running, and adds 5 the review produced.
It is a **plan, not a result**: every item below is unrun.

**Provenance / method.** Two independent model passes over the primary record:

- **Pass 1 — idea generation** (`opencode-go/deepseek-v4.1-flash`): produced the 15 candidates, from the
  verdict doc, E32, F19–F24 and the memory files.
- **Pass 2 — adversarial review** (`opencode-go/kimi-k3`): re-read the verdict doc, E31, E32, **F1–F24**
  including the "Do NOT land" section, the tier-1 pending list, and the source
  (`transfer/manager.go`, `peer/peer.go`, `multilane/lane.go`, `downloadSinks.js`); verified every
  load-bearing factual claim against the results file it came from. It **rejected 4 candidates, demoted 1,
  and confirmed 3 corrections to existing docs** (banners applied — §2).

Neither pass measured anything. **Treat this as hypotheses, not evidence.**

---

## 1. Where the ceiling is — the two shapes

The campaign's single most important late correction (E32/F24) is that "the ceiling" is **two different
regimes**, and earlier work conflated them:

| regime | shape | evidence |
|---|---|---|
| **near-field ~11 ms** | **serialized** ~120–138 Mbps | client uses 0.98 of 4 cores at the ceiling; hurt by fewer cores, **not helped by more** (E30/F21). Bare counting handler 123.68 (E28). |
| **far-field ~70 ms** | **window/RTT-shaped** ~75 Mbps | client only at **0.62** renderer cores; bare handler ties the fixed client (77.36 vs 74.71, 1.04×) on 3.1× less CPU (E32). Sender parked in CC/retransmit at ~2.3× CPU per delivered Mbps (E32-W6). |

Exonerated: path (245–249 Mbps, E32 controls), sender (0.65 core at the plateau, E21; 521–533 Mbps uncapped
vs a Go receiver on **loopback**, E22), host (0.000% steal, E21). Closed: the pion fork (≤1.19–1.32×) and
every exposed SCTP knob. **v1 relay with the fixed client is ~225 Mbps (line speed)** and loses ~3% from
12→71 ms, so relay > direct on v1 (E31/F23).

**Consequence for candidate design:** the only live mechanisms are those aimed at a *non-exonerated*
component — **association-level window multiplication** and **queue / head-of-line shape** — and only in the
**far-field** regime, where the client is not the binding constraint.

---

## 2. Corrections to existing docs (banners applied 2026-10-02)

Three claims in the standing docs do not survive their own later data. Each was verified against the
originating results file before the banner was added.

### 2.1 "Replacing StreamSaver is the most promising remaining lever" — **STALE**
*(verdict doc §5; F22 Consequence 2)*

E29 kept StreamSaver and moved the direct rate only ~1.10×, which the verdict reads as "StreamSaver is the
residual". But the campaign's own later ablations bound the **entire** app receive path — of which the
StreamSaver hop is a subset — at ≤4% **for 4-vCPU clients**:

- **E32-W5:** bare counting handler **77.36** vs fixed client **74.71** = **1.04×** (direct, 70 ms, same session).
- **E31 arm C:** discard sink **229.93** vs fix **224.82** = **1.02×** (relay, JS Noise decryption intact).

**Caveat that keeps a sliver open:** the closure is **rate-at-4-vCPU only**. At **1 vCPU** the app path still
matters — E28-bare pinned **82.99** vs E29-fixed pinned **37.53** ≈ **2.2×** — so for low-end/mobile clients
the sink write path remains a live **CPU/tail** lever (not a rate lever for the rig's 4-vCPU client).

### 2.2 `SB_SCTP_MIN_CWND` "CLOSED. No field value at any size." — **OVERREACHES BY ONE RTT REGIME**
*(F8)*

F8's closure rests on **E17/E18**. E18 swept the small floors (32/64/128/256 KiB) on **CLIENT-EAST only** —
its own results file says so explicitly:

> "CLIENT-WEST (71 ms, BDP ≈ 1 MB) **not swept**: EAST answered the question … and the timebox favored the
> restore." — `results/2026-09-18-exp18-small-min-cwnd-floors.md:45`

E17's 2 MiB catastrophe did hit both clients, but 2 MiB is only ≈2× the **WEST** BDP (≈1 MB). So the closure
is valid **at ~12 ms** (BDP ≈165 KiB, where the 0.8×→1.6× BDP harm threshold was bracketed at 128–256 KiB)
and **the 70 ms regime has never had a sub-BDP sweep in its own units**. E32 later showed 70 ms is a
*distinct, window-shaped* regime — exactly where a floor could plausibly do something. This is the one
**cheap, zero-code** candidate (§3, A5).

### 2.3 "Experiment 7 — ACK-direction loss" is still marked `_pending_` — **CLOSED BY MEASUREMENT**
*(tier-1 doc)*

E19 measured the field's SACK direction directly and found it **lossless**: **0 missing** of 411,209 records
(EAST) and 0 of 372,552 (WEST), bound < 2.7×10⁻⁶ (`results/2026-09-18-exp19-in-flow-loss.md:107–125`). The
data direction carries 0.27–0.68% loss; the ACK direction does not. A lab ACK-loss cell would test a regime
the field never exhibits — the same error F10 warns about for the loss levers. The tier-1 doc was never
updated after E19; the banner now records it.

*(Also still `_pending_` there and **subsumed** by candidate D1/D2 below: Exp 1 — retransmit diagnosis with
pion counters; Exp 4 — `BufferedAmount` time series.)*

---

## 3. The 15 candidates — verdicts

Verdict key: **RUN** (worth it) · **GATED** (worth it behind a prerequisite) · **PROBE** (needs a cheaper
prerequisite first) · **CLOSED** (already answered by E1–E32) · **NO** (not worth the cell).

| # | candidate | verdict | reason |
|---|---|---|---|
| **A1** | N independent PeerConnections per single transfer (one tab, agent-fanned, N∈{1,2,4}, global chunk index, client reorder) | **PROBE** | Real mechanism — per-association cwnd — and **not** forbidden by E30 (the serial cap is a 12 ms finding; at 70 ms the client is at 0.62 cores). But **not novel**: `future_parallel_webrtc_channels.md` (Stage 2) already documents it, and **Chrome's rwnd is ~4–5 MiB**, which rules out receive-window as the 70 ms limiter (4.5 MiB / 70 ms ≈ 514 Mbps) and redirects the question to **sender cwnd**. Probe first with a fixed-client **2-tab striping at 70 ms** (E20's driver, ~$0). Honest prior: ≤1.2× at 12 ms, **1.0–1.8×** at 70 ms — not the 1.5–3× originally claimed. |
| **A2** | Unordered bulk lane (`Ordered:false`) + app reorder buffer | **GATED** | HOL-stall is a plausible 1.84× mechanism (0.27–0.68% in-flow loss ⇒ tens of losses/s at 75 Mbps, each ~1 RTT of ordered-delivery stall) and was never isolated. Gate on D1/D2 showing rtx-correlated stalls **before** building the reorder buffer. |
| **A3** | App-level pacing (token bucket at ~1.2× delivered) | **GATED** | Tier-1 Exp 3 is still `_pending_`; never falsified. Real mechanism: the path polices at ~249 Mbps (E32 UDP control: 300 offered → 249, 17% loss), so an unpaced bursting sender manufactures its own loss. But it acts on the end of the wire with headroom ⇒ 0–20%. **Fold into the D1/A5 session**; do not run standalone. |
| **A4** | Event-driven backpressure (`onBufferedAmountLow`) replacing the hardcoded 10 ms poll | **NO** | Sender is exonerated (0.65 core at the plateau, E30). The 2.3× CPU/Mbps at 70 ms is a **symptom** (E32-W6), not a cause. With a 5 MiB `maxBuffer` the poll rarely gates. This is a CPU-hygiene PR, not an experiment. Revisit only if something lifts the ceiling past ~200 Mbps. |
| **A5** | `SB_SCTP_MIN_CWND` sized to the **70 ms** BDP (256–640 KiB) | **RUN** (after D1) | **Premise verified** (§2.2): the closure was derived at 12 ms and never swept at 70 ms in its own units. Chrome's rwnd is not binding, so the live question is loss-suppressed **sender cwnd** at 70 ms. F8's own rule predicts 256–640 KiB sits in the "harmless" zone at a ~0.66 MB delivered-BDP; the test is whether *harmless* flips to *helpful* at high RTT. 3 floors × n=2 on WEST, one container recreate each. |
| **B1** | Batched-write ablation of the sink (≥1 MiB writes vs 16 KiB appends) | **CLOSED** (mostly) | Isolates a **subset** of what E28/E30/E32 already bounded at ≤1.04× at 4 vCPU and both RTTs. Open sliver is renderer **CPU for low-end clients**, not rate. Don't spend a cell. |
| **B2** | Move the entire receive pipeline into a Worker | **CLOSED** | **Premise wrong.** E28's bare counter **is** the main-thread-idle test: 123.68 Mbps on 0.63 cores (12 ms) and 77.36 on 0.34 (70 ms) proves the browser receives slowly with the main thread idle. Also `RTCPeerConnection` is **not exposed in Chrome workers**, so bytes would `postMessage` back across — adding the hop the candidate tries to remove. |
| **C1** | Bigger client VM (8–16 vCPU) | **RUN** (cheap) | The verdict's §5 and F21 both label "a bigger VM won't help" as **inference, not measurement** — the last unverified assumption behind the whole verdict. One 8-vCPU cell at 12 ms + one at 70 ms. Expected null; a positive reopens everything. |
| **C2** | Browser-identity / flag sweep (Firefox, Chrome stable/Canary) | **RUN** (cheap) | All 32 experiments ran **one Chromium**. Firefox is usrsctp with a ~1 MiB rwnd (memory); Chrome is dcSCTP. Two cells test whether ~120/75 Mbps is an implementation artifact. Low probability, real information. |
| **D1** | Instrument the 70 ms regime (pion `snd_cwnd`/`snd_rwnd`/outstanding/RTO-vs-fast-rtx + client `getStats()`) | **RUN — early** | Subsumes two still-`pending` tier-1 experiments (Exp 1 via the existing `agent/apply_sctp_patch.sh` tooling; Exp 4 `BufferedAmount` series). It is **the discriminator for A1 vs A2 vs A5**: cwnd → A5, rwnd/association → A1, HOL → A2. No in-flight trace exists anywhere in the campaign. |
| **D2** | Instantaneous throughput time series (250 ms) — ramp vs plateau vs sawtooth | **RUN — nearly free** | Every campaign number is a window average. A stall-sawtooth is a cheap pre-test of A2's hypothesis; a ramp means slow-start/window. **Merge into D1.** |
| **D3** | Real mobile/cellular path + 30/100/150 ms RTT bands | **SPLIT** | netem RTT bands on the existing rig: cheap, **yes** — they map the direct-vs-relay curve. Real cellular devices: expensive and confirmatory (relay ≥ direct at every measured point already settles the product guidance). |
| **E1** | Measure v2 direct (native HTTPS) nose-to-nose with v1 direct | **RUN** (one strategic cell) | F22's matrix records "v2 direct **never measured** (explicitly skipped)". E32's own control predicts ~111–163 Mbps single-flow TCP at 70 ms. Completes the 2×2 and directly prices the v2 migration that deletes the DataChannel stack after parity. |
| **E2** | Non-browser (Go/pion) receiver on the **field** path | **RUN — FIRST** | The 521–533 Mbps figure is **loopback** (RTT≈0, no window physics). A Go receiver on the 70 ms field path is the **only** experiment that splits "browser receive path" from "pion sender CC" as the cause of the 1.84× — E32's bare counter cannot see this (it still runs in the browser). Tooling exists (`agent/cmd/benchdirect` prodbench receiver). |
| **E3** | Per-recipient direct-vs-relay measure-and-prefer (locate the crossover RTT) | **CLOSED** | With the fixed client, relay beats direct at **both** measured RTTs (228.6 vs 137.7 @12 ms; 221.0 vs 74.7 @71 ms). **There is no rate crossover to locate.** The only crossover lives in the *pre-fix* client (direct 119 vs relay 51.6), which the fix retires. Route choice is ops/egress-cost policy, not measurement. |

---

## 4. New candidates from the review (N1–N5)

**N1 — DataChannel message granularity vs the serial cap. (nearly free; could falsify the verdict's framing)**
The plateau carries **48,258 × 16 KiB `onmessage` events** (E32-W5 byte accounting); the bare counter spends
~82 µs/message on browser-internal dispatch (123.68 Mbps on 0.63 core ≈ 7,700 msg/s). Hypothesis: **the
serialization is per-message, not per-byte.** Send 64–256 KiB DataChannel *messages* — SCTP re-fragments on
the wire and reassembles **before** delivery, so this is **not** the closed chunk-size lever (E6/E23 measured
wire datagrams and sender CPU). One send-side constant + the E28 bare client, 12 ms, n=2. Expected 1.0–2×.
**If positive, "the browser's receive path is a hard cap" becomes "dispatch is per-message serialized" — a
fixable app/agent property.** E5 closed `MAX_MSG` as a transport knob at capped rates, not as a
dispatch-granularity lever.

**N2 — Loss-correlated time series. (nearly free; orders A2 vs A5/A1)**
Combine E19's DTLS-record-seq-gap capture with 250 ms goodput sampling on one WEST cell. If dips align 1:1
with loss bursts ~1 RTT deep, HOL/reorder stalls are confirmed **without building A2**; if throughput is flat
through loss, cwnd suppression is confirmed and A5/A1 move up. This is the **loss axis D1/D2 lack**.

**N3 — Same-browser native-HTTPS reference at 70 ms. (the browser-side falsifier)**
Serve the identical 754 MiB payload over plain HTTPS with `Content-Disposition` (the v2-relay arm's
zero-page-JS technique, F18) from VERSA to the **same** Chrome on CLIENT-WEST, same hour. If native TCP hits
the ~111–163 Mbps control band while the DataChannel does 75, the "browser DataChannel receive" attribution is
**airtight** *and* prices a middle path (v1-direct fallback over HTTPS byte-range). If the native download also
lands ~75, **E32's attribution collapses** and the path/policer story reopens. One nginx + one drive cell.

**N4 — Kernel pacing instead of app pacing. (the cheap A3)**
`tc fq` egress pacing on VERSA at ~1.1× delivered rate is **ops-only** — tests the burst→policer-loss
hypothesis without touching agent code or the harness's contaminated token bucket (F12 artifact 1). Null ⇒ A3
is closed for real; positive ⇒ a deployment line, not a code change.

**N5 — K ordered streams on the one association. (cheaper A2)**
SCTP HOL blocking is **per-stream**. Spreading chunks round-robin over K=4–8 *ordered* DataChannels on the
existing single association confines each loss stall to 1/K of the flow — no unordered semantics, no app
reorder buffer (the client writes per-stream in global sequence order). Captures most of A2's hypothesized win
at a fraction of the build cost. Untested: v1's bulk rides **exactly one lane** today (`lane.go:103`).

---

## 5. Ranking — information per dollar

1. **E2** — Go/pion receiver at 70 ms. Decisive browser-vs-pion split; existing tooling; can falsify the
   verdict's high-RTT mechanism. **Run this first.**
2. **D1 + D2 (merged)** — cwnd/rtx/`BufferedAmount` instrumentation + 250 ms series; tells you which of
   A1/A2/A5 can possibly work.
3. **C1** — one 8-vCPU cell; cheapest falsification of the serial-cap inference the whole verdict rests on.
4. **C2** — two Firefox cells; implementation-specificity probe.
5. **A5** — 70 ms-BDP min-cwnd sweep; cheap closure once D1 shows the cwnd trajectory.
6. **A1-probe** — fixed-client 2-tab striping at 70 ms (E20's driver). Build A1 only if this scales toward 2×.
7. **A2** — only if D1/D2 show rtx-correlated stalls (or N5 as the cheap variant).
8. **E1** — one strategic cell completing the v1/v2 matrix.
9. **A3** — bundled into the D1/A5 rig session, or via N4.

**Near-free additions worth bolting on:** **N1** (one constant), **N2** (reuses E19 tooling), **N3** (one
nginx), **N4** (one `tc` line), **N5** (a second lane + round-robin).

---

## 6. Recommendation

**Do one cheap falsification pass, then stop.** Run **E2, D1+D2, C1, C2** — roughly a day or two, mostly
existing tooling — plus **N1** (one constant). Between them they can still overturn the verdict: browser-vs-pion
at 70 ms (E2), the serial-cap inference (C1), the single-browser monoculture (C2), and the per-message framing
(N1). If they come back as expected, **stop**: the relay is at line speed at every measured RTT (F23/F24), the
v2 design deletes the DataChannel stack after parity anyway (`future_direct_tcp_mode.md`), and A1/A2 become
insurance policies to be opened only if E2 or D1 say the 70 ms penalty is sender-side after all.

**The largest confirmed v1 win is not an experiment.** Two measured changes are sitting unlanded:

- **F1 — `SB_SCTP_CA_STEP=32768`, ~1.19–1.30×.** Committed on `main` (`agent/internal/peer/peer.go:125-127`)
  but the guard is `if v > 0` and **production sets nothing** — one ops line, needs operator approval.
- **The E29 client fix (1 MiB tail buffer + SHA-1 module worker) — 3.23× user-visible, −48% CPU, 4.36× on
  relay.** `hashWorker.js` is **absent from both `main` and `benchdirect`**, and `downloadSinks.js` on `main`
  still does `concatBytes(tail, bytes)` per 16 KiB append. It exists only as a diff in
  `results/2026-09-18-exp29-verification-preserving-client-fix.md` (200/200 randomized equivalence trials
  byte-identical).

Spend the build budget on v2 direct-TCP + relay; treat A1/A2 as insurance.

---

## 7. What was verified, and how

| claim | verdict | source checked |
|---|---|---|
| E18 never swept the 70 ms client | **TRUE** | `results/2026-09-18-exp18-small-min-cwnd-floors.md:45` — "CLIENT-WEST (71 ms, BDP ≈ 1 MB) not swept" |
| The field ACK direction is lossless (so Exp 7 is moot) | **TRUE** | `results/2026-09-18-exp19-in-flow-loss.md:107–125` — SACK dir 0 missing of 411,209 (EAST) / 372,552 (WEST), bound <2.7×10⁻⁶ |
| E31/E32 close "replace StreamSaver" at ≤4% (4-vCPU rate) | **TRUE, with a 1-vCPU caveat** | E32-W5 77.36 vs 74.71 = 1.04×; E31 arm C 229.93 vs 224.82 = 1.02×; but E28-bare 1 vCPU 82.99 vs E29-fix 1 vCPU 37.53 ≈ 2.2× |
| Chrome's SCTP rwnd is ~4–5 MiB (so receive-window is not the 70 ms limiter) | **TRUE (prior measurement)** | `memory/project_benchdirect_findings.md` (2026-08-13) |
| `sendWithBackpressure` = 64 KiB chunk, 5 MiB `maxBuffer`, 10 ms poll, no `OnBufferedAmountLow` | **TRUE** | `agent/internal/transfer/manager.go` |
| v1's bulk rides exactly one lane over one association | **TRUE** | `agent/internal/peer/peer.go:186`, `multilane/lane.go:103` |

**Corrections applied to standing docs (2026-10-02):** verdict doc §5 (StreamSaver bullet — stale),
`ACTIONABLE-FINDINGS` F8 (min-cwnd closure overreaches by one RTT regime) and F22 Consequence 2 (E32 extends
the E31 correction), tier-1 doc Experiment 7 (closed by E19).

**Not done here:** no experiment run; no code changed; no rig or instance touched. The three test VMs remain
**shelved** as of 2026-09-19.
