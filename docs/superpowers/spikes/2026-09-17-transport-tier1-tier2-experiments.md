# Tier 1 + Tier 2 transport experiments — plan and results (2026-09-17)

Purpose: close the highest-value open questions from the transport investigation
(`2026-09-15-transport-investigation-results.md`) and the two Sept 16 field tests. This doc is the
single home for the plan, the exact method for each experiment, and its results as they land.

**Status:** plan written; infra bring-up started. Results are appended per experiment (each has an
empty `Result` block until it runs).

## OVERNIGHT RESULTS SUMMARY (2026-09-18) — read this before the experiment sections

This section supersedes the interpretation in **Appendix A (Experiment 9)**, which is partly wrong.
Exp 9's *measurements* stand; its *conclusions* do not. Everything below is field-measured on the v1/v2
test rig unless marked lab.

### 1. Headline: the v2 relay transport is 2.1–3.8× faster than v1, reproducibly

| path | v1 (WebRTC DataChannel) | v2 (relay: TLS/HTTP over gateway + FRP TCP) | ratio |
|---|---|---|---|
| CLIENT-EAST ~12 ms | **102–111 Mbps** (n=3) | **233.3–233.7 Mbps** (n=3, spread 0.18%) | **2.1–2.3×** |
| CLIENT-WEST ~71 ms | **60.2 Mbps** (n=2–3) | **214.9–228.4 Mbps** (n=2) | **3.6–3.8×** |

- v2 with a **real browser download sink** (Chromium, no Download API): **239.05 Mbps** — *above* the
  curl cells, so the earlier curl-vs-browser asymmetry was not flattering v2. The result is not a
  curl artefact.
- v2 sits at **87–97% of the path's measured UDP capacity**; v1 at **25–45%**.
- **v1 halves as RTT rises (111 → 60 Mbps); v2 barely moves (233.6 → 221.6).**
- Client VMs were ~100% idle throughout the v2 cells; v2's agent used ~0.18 cores vs v1's ~1.0.

### 2. v1's ceiling is software-side — not the path, the client, or the NIC

- **The path carries ~246 Mbps of UDP** (single flow: 246.6 Mbps @ 0.90% loss EAST, 245.8 @ 0.84%
  WEST; four flows do not raise it) and **240 Mbps over 4 TCP flows**. There is no ~100 Mbps policer;
  the earlier "300 offered → 243 received at 18% loss" is exactly a 246 Mbps ceiling.
- **Two independent client hosts do not add:** 1 tab on each = **89.9 Mbps**, with *both* hosts slowing
  at once (EAST 97.3→88.8, WEST 61.4→45.4) and WEST 85% idle; 2+2 across hosts = **99.1 Mbps**, 4/4.
  The cap is shared upstream of the clients.
- **Striping is counterproductive, not merely neutral:** v1 cross-host N=2 (83.5–89.9) is *below* v1
  N=1 (102–122).
- Agent CPU during v1 cells: **90–122% (2 peers), 126–157% (4 peers)** in Docker units where 100% = one
  logical core (the host has 6), spread across 8–9 threads with **no pinned thread**. (Exp 9's "1.53%"
  was invalid — see below.)

### 3. v1 collapses under loss — it goes *idle* rather than congesting (lab, E1)

Lab wire/payload is **1.08× (L4) / 1.12× (incl. IP+UDP)** — established three independent ways — with
**no spurious retransmission** (loss 0.001 adds +0.8% bytes, loss 0.01 adds +1.2%: one-for-one
recovery). Yet **goodput collapses 16–62× while the wire stays ~1.1×**: the sender stops rather than
filling the link. Stalls are 0.9–1.2 s chunks — the hard-coded 1 s RTO floor. Real-world lossy paths
should therefore be much worse for v1 than this clean test path suggests.

**E11 mapped the cliff precisely** (rtt 12, bottleneck capped at 240 Mbps, n=3–5 per cell):

| loss | 0 | 1e-4 | **2e-4** | 3e-4 | 1e-3 | 9e-3 | 1e-2 |
|---|---|---|---|---|---|---|---|
| Mbps | **225.2** | **211.0** | **96.5** | 60.1 | 28.3 | 8.6 | 8.2 |

The collapse is a **cliff between 1e-4 and 2e-4** (~0.015%), not a gradient. Wire ratio drifts only
1.089→1.140, `shim_drop` stays 0, and Chrome's CPU falls 0.70→0.04 cores as loss rises — the sender
**idles, it does not congest**. The collapsed rate scales as **1/RTT** (5.0–5.6× for a 5.9× RTT step),
which identifies it as **window-limited with W ≈ 10 datagrams**: v1's send window collapses to a tiny
constant and throughput becomes window/RTT. That single mechanism explains why v1 halves as RTT doubles
(111 → 60 Mbps in the field), why striping cannot help, and why v2's kernel-TCP path — with real
congestion control — wins.

**E11's RTO verdict (the last place a cheap fix could hide): not a fix.** Lowering the RTO floor gives
+13.8%/+14.8% at 1e-3 loss but only +2.4%/+5.6% at 9e-3 (inside spread), and the entire gain is the
removal of the 1 s stall time (4.5 s → 1.2 s). The sender idles less; it does not send more.

**Honest disagreement to carry forward.** E11's analyst argues the field's 112 Mbps cap is *not* explained
by loss: the ladder only reaches 112 Mbps at ~1.7e-4 effective loss, ~50× below the 0.90% measured at
250 Mbps offered, while 0.9% gives 8.6 Mbps — 13× *below* the field. So the field's effective loss must be
far lower than 0.9% (consistent: that figure was measured well past the 246 Mbps ceiling), and loss alone
does not set the 112 cap. Its analyst then leans back toward a client-sink explanation. **I weight E8b's
cross-host result above that**: two independent client hosts did not add throughput, which a per-client
sink cannot produce (see §2). The best-supported remaining explanation is therefore **the v1 agent-side
userspace send path** (pion DTLS/SCTP crypto in one process, ~0.9–1.6 cores spread over 8–9 threads with no
pinned thread, on a host that also runs a GPU LLM server, Jellyfin, Immich and more).

**The decisive follow-up that would separate them (unrun):** in the *field*, measure v1 with the
application download sink removed (a bare byte-counting bench page, or a Go receiver) against the real
app on the same share and hour. Fast bare-page ≈ agent-side limit; fast app-only-when-bare ≈ client sink.

### 4. The old "wire ÷ 3 = goodput" field claim was a measurement artefact

It cannot be protocol framing (1.08–1.12×) and is almost certainly not retransmission. It needs a
**per-5-tuple field capture** to attribute (most likely NIC counters including both directions or
unrelated host traffic). Do not repeat the ÷3 figure as a transport property.

### 5. Corrections to Appendix A (Experiment 9)

- **"The client-side sink is the binding constraint" — FALSIFIED.** Cross-host striping shows the cap is
  shared upstream; the client hosts are not the limit.
- **"Agent CPU 1.53%" — INVALID.** Correct live values are 90–157% in Docker units (0.9–1.6 cores of 6).
  A post-completion sample of 7.57% Docker units = 1.26% of six cores reproduces how 1.53% arose.
- **"At N=4 one of four sessions dies early" — DID NOT REPRODUCE** (4/4 clean in two independent
  cells, no mid-transfer closes, no takeover evidence). Unexplained, but now doubtful.
- Exp 9's *numbers* (windows, throughputs, CPU idle%) remain valid; the interpretation built on them does not.

### 6. Retired hypotheses

- **RTO floor has no effect on a clean path** (E2, 91 runs): ceilings identical at every RTT
  (12/25/71/100 ms → 503/510/512, 470/456/456, 328/329/331, 243/243/243 Mbps), wire datagrams/MiB
  constant, `shim_drop=0` in all runs. Remaining question under *loss* is E11's job.
- **No long mid-transfer plateaus exist** in these configurations: 128 runs, longest stall 1.2 s. The
  Sept-15 "long plateau" story does not reproduce.
- Also found: the harness's `--mode prod` never wires `--rtomax` (`rawbench.go:225` only), so prod-mode
  RTO sweeps are silent no-ops — E5 needs the wiring added before it can measure anything.

### 7. Bench and method notes (for anyone re-running this)

- **The lab Mac cannot produce trustworthy throughput *means*** while it is busy: a sanity config
  spread **12.8×** (Time Machine + `mds` + load 3–7.4). **Rate-capped (`--bandwidth`) runs are
  bit-for-bit reproducible** because the token bucket rather than the CPU is the limit — use those.
  Ceilings, wire volume, `shim_drop` and stall structure are robust; mean deltas are not.
- Field metric discipline: agent-side windows only (`lanes ready` → `download complete`); **never** call
  Playwright's Download API (it aborts the transfers it measures); client-side artefacts are unreliable.
- Path capacity is **time-varying** (4 TCP flows measured 156 Mbps one hour and 240 the next).
  Single control readings are weak evidence; re-measure in the same session as the cell you compare.
- v1 single-session rates are noisy (EAST N=1 measured 97.3, 110.97, 122.2 across the night); n≥3 and
  same-session baselines are required for any claim.

### 8. Still open after tonight

- **v2 DIRECT mode was never measured** — all v2 cells above are **relay** mode. Direct mode is the
  architecturally interesting one (no third-party relay in the data path) and is untested for throughput.
- The field per-5-tuple loss capture (item 4) is unrun.
- E11 (loss-sensitivity ladder, incl. whether the RTO floor matters under loss) was in flight when this
  summary was written.
- Everything above is single-file, single-share, n=1–3; no statistical treatment beyond spread reporting.

---

## Goal, in one line

Determine whether the clean-path ceiling (goodput ~50 Mbps at ~12 ms while the same path carries
480+ Mbps of plain UDP) is **removable** or **physical**, and whether any of the cheap exposed
knobs move it.

## Design corrections discovered while planning (these change the method)

1. **A wire capture cannot classify SCTP retransmissions.** WebRTC data channels are SCTP over
   DTLS over UDP, so a `tcpdump` sees encrypted DTLS records. Retransmit-vs-original is not
   visible. Experiment 1 therefore needs **pion-side counters** (the existing `agent/forks/`
   infrastructure), with wire counters used only for the byte/timing side. A capture alone would
   have produced nothing usable.
2. **Path loss must be measured at the same offered rate, in the same session.** The path drops
   2–6% at 500–800 Mbps offered (iperf3 UDP), so "wire >> goodput" alone does NOT prove spurious
   retransmission — genuine loss from NIC/queue drops is a competing explanation. Every
   retransmit-attribution cell must pair the transfer with a same-rate path-loss measurement.
3. **The shim's loss model is symmetric** (`shim.go`: one `loss` field on the shared bottleneck), so
   experiment 7 (ACK-direction loss) needs a small shim change before it can run.
4. **The field N=4 = 2.5× cell in the Sept 16 field test is confounded** — it was Direct×3 +
   Relay×1, so it is not a clean striping datapoint. The clean field datapoint is N=2 = 1.03×, and
   it is at **11.9 ms**, the regime where the ramp deficit that striping multiplies does not exist.
   High-RTT striping is **lab-only** so far, which is why experiment 9 exists.

## Infrastructure prerequisites

| what | role | state |
|---|---|---|
| `<home-agent-host>` | v1 agent, host networking, 6 vCPU | running; needs the v1 agent container rebuilt with the new env knobs |
| `<east-client-vm>` | 4 vCPU client, ~12 ms to agent (clean/low-RTT cells) | unshelving |
| `<west-client-vm>` | 4 vCPU client, ~71 ms to agent (high-RTT cells) | unshelving |
| `<signaling-host>` | v1 signaling server + TLS (service workers need HTTPS) | unshelving; must verify the v1 stack and cert survived |
| benchdirect harness | lab loopback shim, real Chrome, pion sender | `/Users/ali/Git/ShareBridge/.worktrees/benchdirect` @ `68c2be83` |

Client VM requirements (from the Sept 16 notes): ≥4 vCPU (a 2 vCPU client CPU-pins and poisons
concurrency results), headed Chrome under Xvfb, Node + Playwright, `netem`/`ifb` available.

## Estimate

Estimates are wall-clock including method setup, runs, analysis, and writing the result. Lab cells
are cheap (in-process shim, 10–90 s per run); field cells cost ~2–3 min per 754 MB download plus a
container restart per variant.

| # | experiment | env | estimate |
|---|---|---|---|
| — | Infra bring-up + single-tab baseline verification | field | 1.5–3 h (risk: +2–4 h if the v1 rig needs re-provisioning) |
| 1 | Retransmit diagnosis (pion counters + matched-rate path loss) | lab → field | 3–5 h |
| 2 | RTO floor retest | lab + field | 2–3 h |
| 3 | App-level pacing (cheap token bucket) | lab | 3–4 h |
| 3b | *Gated:* pion pacer + BBR-lite re-test | fork | +4–8 h (only if 3 justifies it) |
| 4 | `BufferedAmount` time series | lab | 2–3 h |
| 5 | Three never-swept pion knobs | lab | 2.5–4 h |
| 6 | Chunk size at high RTT | lab | 1–1.5 h |
| 7 | ACK-direction loss | lab | 1.5–2.5 h |
| 8 | Why N=2 does not sum (localize the per-host pool) | field | 2–4 h |
| 9 | N-concurrency at high RTT | field | 1.5–3 h |
| — | Write-up (this doc, incremental) | — | 2–4 h |

**Total ≈ 22–34 h of work (3–5 focused days)**, excluding the gated 3b. With 3b: ~26–42 h.
Roughly 2 days if the field rig is intact and nothing surprises.

## Execution order (cheap and decisive first)

- **Stage A — lab, no field dependency:** 2 → 6 → 5 → 7 → 4 → 1(lab)
- **Stage B — field:** baseline → 9 → 8 → 2(field confirm) → 1(field confirm)
- **Stage C — gated:** 3b, only if Stage A's pacing test shows the wire/goodput ratio improves

Rationale: 2, 6, 5, 7 are cheap, already-parameterised, and any of them *might* move the ceiling,
which would reduce the value of the deeper fork work. 1 is the highest-information experiment but
needs instrumentation, so it runs after the cheap sweeps.

---

## Experiment 1 — retransmit diagnosis

**Hypothesis.** The clean-path `wire ÷ ~3 = goodput` ratio (wire 160–190 Mbps, goodput ~50 Mbps) is
dominated by **spurious RTOs** (`rtoMin` hardcoded 1000 ms) firing into an unbudgeted send queue,
rather than by genuine path loss.

**Competing hypothesis.** The path genuinely drops 2–6% at the offered rate (measured with iperf3
UDP at 500–800 Mbps), so the overhead is real loss recovery, not spurious retransmission.

**Method.** Add counters in the pion fork (retransmitted chunks, RTO firings, dup-ACKs received,
cwnd/ssthresh trajectory, send-queue depth) and emit them in the harness JSONL, then pair each
transfer with a same-rate path-loss measurement. Wire bytes and datagram sizes/timing come from the
NIC counters and the in-process shim.

**Cells.** 12 ms clean (field, agent NIC counters) and the equivalent lab cell; n≥3.

**Metric.** Retransmitted bytes ÷ sent bytes, by cause (RTO vs fast-retransmit vs duplicate);
matching path loss %; wire/goodput ratio.

**Decides.** Whether the 2.5–3× headroom identified in the Sept 16 field test is real.

**Result.** _pending_

---

## Experiment 2 — RTO floor on the clean path

**Hypothesis.** A lower RTO floor reduces spurious retransmission on a clean low-RTT path.

**Why untested:** the earlier `rtoMin` test was dismissed as "irrelevant once overshoot is removed (no
drops ⇒ no RTOs)" — but that was in a *drop-induced* lab context. The clean-path `÷3` implies RTOs
fire without loss.

**Method.** No fork needed: `SB_SCTP_RTO_MAX_MS` below 1 s also lowers the effective RTO floor (per
the harness flag's own help text). Rebuild the agent with each value; completed-download metric.

**Cells.** 12 ms and 71 ms × {default, 500 ms, 200 ms} × n=2; plus the harness `--rtomax` sweep.

**Metric.** Completed-download goodput + retransmit counters (from experiment 1).

**Result.** _pending_

---

## Experiment 3 — app-level pacing

**Hypothesis.** Pacing the send loop (rate-limit to ~1.2× measured delivery) reduces the burst
overflow that the unbudgeted send queue causes, improving goodput *and* the wire/goodput ratio.

**Why it matters:** BBR-lite failed because pion does not pace; if pacing alone helps, the earlier
"do not build a controller" verdict is about the wrong layer.

**Method.** Stage 3a: token bucket in the harness send loop (`rawbench.go` / `prodbench.go`), no
fork. Stage 3b (gated on 3a): pacer in the pion fork + BBR-lite re-test.

**Cells.** pacing on/off × rtt {12, 71, 100} × N=1, n≥3.

**Metric.** Goodput, wire rate, wire/goodput ratio, retransmit counters.

**Result.** _pending_

---

## Experiment 4 — `BufferedAmount` time series

**Hypothesis.** The send queue is unbudgeted and periodically overflows, producing the burst/retransmit
behaviour. This is the direct observable and was explicitly named a "spec gap" in the earlier work
and never built.

**Method.** Sample `BufferedAmount` at 100 ms during a transfer, alongside wire counters; emit to
JSONL.

**Metric.** Send-queue depth envelope vs wire rate and retransmit events, time-aligned.

**Result.** _pending_

---

## Experiment 5 — three never-swept pion knobs

All three are already env-wired in production (`agent/internal/peer/peer.go`) but were never
measured:
`SB_SCTP_FAST_RTX_WND` (`SetSCTPFastRtxWnd`), `SB_SCTP_MAX_RX_BUF`
(`SetSCTPMaxReceiveBufferSize`), `SB_SCTP_MAX_MSG` (`SetSCTPMaxMessageSize`).

**Why:** only three of the six exposed knobs have published results (CA-step, min-cwnd, RTO-max).
FastRtxWnd acts directly on the retransmit path that experiment 1 is about.

**Method.** Add harness flags mirroring the env vars; sweep each over ~4 values.

**Cells.** 3 knobs × 4 values × {12 ms clean, 71 ms} × n=2.

**Result.** _pending_

---

## Experiment 6 — chunk size at high RTT

**Hypothesis.** Chunk size interacts with retransmit granularity and head-of-line blocking at high
RTT; the existing sweep compared 16 KiB vs 64 KiB **at rtt=0 only**, where neither matters.

**Method.** Existing `-chunk` flag; rtt {25, 71, 100} × chunk {16, 64, 256 KiB} × loss {0%, 0.1%}.

**Result.** _pending_

---

## Experiment 7 — ACK-direction loss

**Hypothesis.** SCTP is ack-clock-driven, so ACK loss may dominate the collapse currently attributed
to data-direction loss. All prior loss testing applied loss only at the client ingress (data
direction).

**Method.** Requires a shim change (the loss model is currently symmetric). Then compare loss
applied to data-only vs ack-only vs both.

**Cells.** rtt 25/71 × {data 1%, ack 1%, both 1%} × n=2.

**Result.** _pending_

---

## Experiment 8 — why N=2 does not sum

**Hypothesis.** The agent-host NIC TX plateau at 160–190 Mbps **regardless of N** indicates a
**per-host** ceiling (one socket / DTLS pipeline / NIC offload path / single-threaded send), not a
per-session one. If per-host, striping can never multiply goodput on this agent.

**Method.** Field: N=1/2/4 at 12 ms with (a) default, (b) separate client processes/ports, (c) a
second agent process. Capture per-connection wire rates, not just the aggregate.

**Result.** _pending_

---

## Experiment 9 — N-concurrency at high RTT (field)

**Hypothesis.** Striping's benefit grows with RTT (independent congestion windows multiply the ramp),
so the field result at 11.9 ms (N=2 = 1.03×) does not generalise. Lab evidence at 100 ms supports a
large effect (p10 54.9 → 362.1 Mbps); the field has never run N>1 above 12 ms.

**Method.** Reuse the existing N-tab Playwright driver on the west-region VM at ~71 ms; N ∈ {1, 2, 4},
completed-download metric, n=2.

**Decides.** Whether the field high-RTT ceiling is ramp-limited (striping helps) or pool-limited
(striping does not).

**Result — COMPLETE (with one unresolved confound, see caveats).**

Measured 2026-09-18 on the v1 field rig (home agent ↔ two OVH client VMs, one east ~12 ms, one
west ~71 ms), CA-step tuning on, all sessions confirmed **direct** (`DataChannel lanes ready`), one
754 MiB file (790,626,304 B = 6,325 Mb). "Agent-side window" = first `lanes ready` → last
`download complete` — driver-independent, and the only trustworthy metric here (see caveats).

| cell | RTT | completed | agent-side window | aggregate goodput | vs N=1 |
|---|---|---|---|---|---|
| N=1 | ~12 ms | 1/1 | 65 s | **97.3 Mbps** | 1.00× |
| N=2 | ~12 ms | 2/2 | 129 s | **98.1 Mbps** | 1.01× |
| N=4 | ~12 ms | 3/4 | 145 s | **130.9 Mbps** | 1.35× |
| N=1 | ~71 ms | 1/1 | 103 s | **61.4 Mbps** | 1.00× |
| N=2 | ~71 ms | 2/2 | 189 s | **66.9 Mbps** | 1.09× |
| N=4 | ~71 ms | 3/4 | 250 s | **75.9 Mbps** | 1.24× |

Controls (agent → client, i.e. the download direction), measured on the 71 ms path:

| control | result |
|---|---|
| iperf3 TCP, 1 flow | **66.6 Mbps** |
| iperf3 TCP, 4 flows | **156 Mbps** |
| iperf3 UDP, 100 Mbps offered | **98.7 Mbps received, 0.43% loss** |
| iperf3 UDP, 300 Mbps offered | 243 Mbps received, 18% loss |
| agent CPU during a cell | **~1.5%** of a 6-core host |
| client CPU (4 vCPU) | idle 28–49% at N=1, **5–27% at N=2** (busy 63% → 85%) |

Window definition used throughout: **first `DataChannel lanes ready` → last `download complete`** in that
cell. That is driver-independent (it never touches Playwright), which matters because every
client-side metric here proved untrustworthy (see caveats).

Client-side walls, for corroboration only — they are consistently longer than the agent-side window
because they include the browser's verify/write tail:

| cell | client wall | implied (vs agent-side) |
|---|---|---|
| east N=1 | 139.9 s | 45.2 Mbps (vs 97.3) |
| east N=2 | 252 s | 50.2 Mbps (vs 98.1) |
| west N=1 | 114 s | 55.5 Mbps (vs 61.4) |
| west N=2 (run 1) | 221 s | 57.2 Mbps (vs 66.9) |
| west N=2 (run 2) | 211 s | 59.9 Mbps |
| west N=4 | not measurable | artefacts vanished mid-run |

Client CPU profiles (sampled every 5 s through the transfer):

- **N=1:** idle 28–49% (mean 36.9%) → **63% busy**; aggregate byte rate 7.52 MB/s.
- **N=2:** idle 5–27% (mean 15.4%) → **85% busy**; aggregate byte rate 7.86 MB/s.

The client's busy fraction rises with N while the aggregate byte rate does not move — the signature of
a client-side cap rather than a transport cap.

**Findings.**

1. **Striping does not multiply aggregate goodput in the field, at either RTT.** N=2 buys 1.01×
   (12 ms) and 1.09× (71 ms); N=4 buys 1.35× and 1.24×. Far below N×.
2. **This contradicts the lab prediction** (rtt=100 lab: N=1→N=4 mean 132.7 → 392.1 Mbps). The lab
   receiver was also real Chrome, but the lab bypasses the application's download sink entirely
   (raw DataChannel receive, versus SHA-1 verification plus service-worker disk writes in the field).
3. **The bottleneck is not the path and not the agent.** The path carries 156 Mbps across 4 TCP
   flows and 98.7 Mbps of clean UDP; the agent sits at ~1.5% CPU. Both have large headroom over the
   observed 61–76 Mbps (71 ms) aggregate.
4. **The client is the leading suspect**: a 4-vCPU client is 64% busy at N=1 and 85% at N=2, and its
   busy fraction rises with N while aggregate goodput does not.
5. **Reliability finding, repeatable: at N=4 exactly one of four sessions died early in BOTH
   regimes** (agent log shows a peer `closed` before its `download complete`), so both N=4 cells are
   3/4. Any striping design must handle this.

**Caveats — and what this does NOT yet establish.**

- Aggregate idle% cannot exclude a **single critical thread** (Chrome network/decrypt/verify) being
  saturated while other cores idle. Separating "client sink" from "transport" needs a client with
  more vCPUs: the decisive follow-up is to re-run this matrix on an 8–16 vCPU client. If aggregate
  scales there, the client sink was the cap and the transport is exonerated; if it does not, the cap
  is in the transport or the agent and item 3's headroom claim needs revisiting.
- n=1 per cell, except N=2 at 71 ms which was reproduced twice (agent-side 189 s; client-side 221 s
  and 211 s).
- Direct mode only; one payload, one browser build, one agent config (CA-step on).
- **The client-side artefacts are not a usable metric**: Playwright's download files vanished
  mid-run at N=4 (`files=4` at t=15 s → `files=0` later) while the agent completed three of them, and
  calling `download.path()`/`saveAs()` **aborts transfers** (it caused the one early peer close in the
  first N=4 attempts and reports `canceled` even for transfers that succeed). All numbers above are
  therefore agent-side. Client-side wall times are reported only as corroboration (N=1: 114 s vs
  103 s agent-side; N=2: 211/221 s vs 189 s) — the gap is the client's verify/write tail.
- The first field N=4 attempt (before the click-only driver) is **excluded**: `saveAs` cancelled it.
- `pkill -f` inside an ssh command string self-matches and kills its own shell (documented in the
  Sept 16 ops notes, and reproduced here twice); use `pkill -x <name>` or `kill $(pgrep -x …)`.


---

## Cost and risks

- **VM cost:** three instances for ~3–5 days is low tens of dollars; all are shelvable again at the end.
- **Risk 1 (field rig):** if the v1 signaling server/cert did not survive or the agent container is
  gone, bring-up grows by ~2–4 h. Mitigation: verify the rig with one baseline download before any
  field experiment.
- **Risk 2 (instrumentation):** experiment 1 requires fork counters. If the fork is not currently
  cleanly buildable against pion v4.2.11, this becomes its own task.
- **Risk 3 (gated work):** 3b is open-ended Go work in a vendored fork; it is deliberately gated on
  3a showing a signal.
- **Hygiene:** no IPs, hostnames, or credentials in this document; runs are recorded as sanitised
  commands.

---

# Appendix A — Experiment 9 raw data (2026-09-18)

## A.1 Environment

- **Agent:** home host, v1 agent container (`sb-agent:pristine`, host networking, UI on 127.0.0.1:7879),
  `SB_SCTP_CA_STEP=32768` (CA-step **on** — the shipped-tuning configuration).
- **Clients:** two cloud VMs, **4 vCPU / 14 GB / 92 GB free**, one east (~12 ms to the agent), one west
  (~71 ms). Both already had Node, Playwright 1.63.0 + Chromium 1243, and Xvfb.
- **Signalling:** v1 signaling server + Caddy TLS on the east host. **To run v1 at all, the v2 relay
  stack on that host had to be stopped** (SNI gateway :443, control :8080, frps, UDP 3478) and
  `sharebridge-test.service` + `caddy` started; the v2 test agent on the home host was stopped to free
  UI port 7879. Both must be restored. This is a real bring-up cost worth remembering: the v1 field rig
  and the v2 relay stack collide on :443, :8080 and UDP 3478.
- **Session:** created fresh for the matrix via `POST /api/v1/shares` with `share_type=opencloud`,
  `relay_only=false`, 48 h expiry, `max_downloads=500`, pointing at a public OpenCloud share of one
  754 MiB file. (`relay_only` is per-share, not per-agent — the agent default is `DefaultRelayOnly: true`.)
- **Payload:** one file, 754 MiB = 790,626,304 B = 6,325.01 Mb per download.

## A.2 Per-cell agent-side timeline

| cell | first lanes-ready | last download-complete | completed | window | aggregate |
|---|---|---|---|---|---|
| east N=1 | 04:49:39 | 04:50:44 | 1/1 | 65 s | 97.3 Mbps |
| east N=2 | 04:52:03 | 04:54:12 | 2/2 | 129 s | 98.1 Mbps |
| east N=4 | 05:00:42 | 05:03:07 | 3/4 | 145 s | 130.9 Mbps |
| west N=1 | 05:16:20 | 05:18:03 | 1/1 | 103 s | 61.4 Mbps |
| west N=2 | 05:41:00 | 05:44:09 | 2/2 | 189 s | 66.9 Mbps |
| west N=4 | 05:46:07 | 05:50:17 | 3/4 | 250 s | 75.9 Mbps |

In both N=4 cells the agent logged a peer `closed` **before** the completions (east 05:02:25, west
05:49:38) — i.e. the fourth session died mid-transfer, repeatably, in both RTT regimes.

## A.3 Controls (agent → client, the download direction), west path

| control | result |
|---|---|
| iperf3 TCP, 1 flow | 66.6 Mbps receiver (69.8 sender) |
| iperf3 TCP, 4 flows | 156 Mbps SUM receiver |
| iperf3 UDP, 100 Mbps offered | 98.7 Mbps received, 0.43% loss (298/69,070) |
| iperf3 UDP, 300 Mbps offered | 243 Mbps received, 18% loss (36,714/207,186) |
| agent CPU during a cell (`docker stats`) | **1.53%** of a 6-core host |

Note the useful comparison this produces: **one WebRTC session at 71 ms (61.4 Mbps) is now within ~8%
of one kernel-TCP flow (66.6 Mbps)** — the CA-step tuning has made a single pion session
single-flow-competitive at high RTT. What it does *not* do is scale across sessions the way TCP does
(4 flows = 156 Mbps = 2.3×).

## A.4 Driver / measurement failures encountered (methods record)

1. **Playwright's Download API aborts the transfers it is measuring.** `download.path()` and
   `download.saveAs()` fail with `canceled`, and the cancellation is **real** for some tabs: in the
   first N=4 attempts the agent logged a peer closing early and only 3 of 4 transfers completed.
   Calling the API is therefore *out of the measurement path* entirely.
2. **Client-side download artefacts are not a usable metric**: at N=4 the artefacts went from
   `files=4` (t=15 s) to `files=0` while the agent completed three downloads. File-size growth is
   fine for coarse progress but cannot be trusted for completion.
3. **`pkill -f` self-matches inside an ssh command string** and kills its own shell (documented in the
   Sept 16 ops notes; reproduced here). Use `pkill -x <name>` or `kill $(pgrep -x <name>)`.
4. **`cd dir && nohup cmd &`** backgrounds the whole list, so a following `cd`-relative command runs in
   `$HOME`. Use absolute paths.
5. **A backgrounded `nohup` job holds the ssh channel open**, making the call hang for minutes. Use
   `setsid nohup cmd >/dev/null 2>&1 </dev/null &` and poll in a separate call.
6. **Nested `ssh` inside a polling loop hung** with no output at all (no samples written). Sample
   locally on the client VM and poll with short, separate calls.
7. **The Cloudflare speed endpoint returns ~10 B/s from these VMs** — it cannot be used as a capacity
   control. Use iperf3 against the peer.
8. Every cell logs `relay: pending wait window exceeded` for **speculative** relay channels. This is
   cosmetic and expected when a peer goes direct; all cells in this matrix were direct.

## A.5 Still open

- **The bigger-client confirmation** (8–16 vCPU west client): re-run the same matrix. If aggregate
  scales there, the client sink is the cap and the transport is exonerated; if it does not, the cap is
  in the transport/agent.
- **Annex B candidate (not started):** the N=4 session death — reproduce with per-peer signalling and
  agent logs at debug level, and test whether it is the known one-connection-per-API-key takeover
  (`hub.go:20`) or a resource limit.
- Experiments 1–8 of this plan are **not started**.
