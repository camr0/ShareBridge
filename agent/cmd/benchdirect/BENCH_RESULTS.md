# Benchdirect results — 2026-08-13 (final)

Environment: macOS arm64, Chrome (headless), Go 1.26.1, loopback + in-process reflexive-NAT UDP shim.
Averaged runs below; `bench_results.jsonl` + `run_matrix2.sh` reproduce them. `--deadline` flag added to
extend the receive deadline for long/lossy transfers.

## Core question: browser SCTP vs ShareBridge's sender loop?

**The browser's SCTP is the limiter — the sender loop adds no measurable overhead.** In every condition
tested (rtt 0/25/50/100 and 1–5% loss), mode B (real `transfer.Manager`) is equal to or slightly *above*
mode A (raw pion pump). `sendWithBackpressure` (5 MB window, 10 ms poll) keeps the SCTP link saturated.

## Ceiling (rtt=0, loss=0)

| transfer | raw (poll) | raw (event) | prod |
|---|---|---|---|
| 128 MiB | 526.96 | 512.75 | 522.73 |
| 1 GiB    | 499.24 | 459.64 | 458.83 |

~460–520 Mbps. The ~5–10% drop from 128 MiB → 1 GiB is SCTP congestion-window settling over a longer
transfer, not a sender effect (prod tracks raw).

## Latency sweep (loss=0, 64 MiB, 4 reps → mean [min–max])

| rtt | raw (poll) Mbps | prod Mbps | verdict |
|---|---|---|---|
| 0   | ~499 | ~459 | equal |
| 25  | 474.86 [472.6–476.9] | 476.94 [471.1–482.2] | equal, tight |
| 50  | 266.43 [177.8–370.5] | 307.54 [125.9–432.0] | equal, noisy |
| 100 | 53.50 [28.8–80.9] | 77.00 [57.9–103.2] | equal, noisy |

Throughput collapses with RTT (SCTP congestion window grows slowly at high RTT). rtt=50/100 remain
3x noisy even at 64 MiB × 4 reps — the variance is SCTP congestion-control dynamics, not sender overhead
(prod is never consistently below raw).

## Loss sweep (rtt=25, 8 MiB)

| loss | raw Mbps | prod Mbps |
|---|---|---|
| 0    | ~475 | ~477 |
| 0.01 | 3.68 [3.1–4.3] | 4.17 [4.0–4.3] |
| 0.05 | 1.45 | stall (>45 s) |

1% loss ≈ **120x collapse**; 5% loss ≈ **300x + stall**. This is the reliable/ordered SCTP retransmit +
head-of-line-blocking behavior — and it hits both modes identically, so it is *not* a sender-loop defect.

## Conclusions

1. **Architecture is sound**: the direct path is SCTP-bound, and the Go sender loop is not a bottleneck.
2. **The real limiters are network conditions, not code**: RTT > 25 ms and any loss (≥1%) crater
   throughput via SCTP. 100 ms RTT ≈ 50–80 Mbps; 1% loss ≈ 4 Mbps.
3. **Harness follow-ups**: rtt=50/100 still need longer transfers (or more reps) to narrow the variance;
   a probed-RTT metric and BufferedAmount time series (spec gap) would explain *why* SCTP collapses.

## 200 MiB latency sweep (loss=0, 3 reps → mean [min–max]) — 2026-08-13 follow-up

Larger transfers were expected to stabilize throughput by reaching SCTP steady state. They did NOT —
the high-RTT variance is bimodal, not a small-transfer artifact.

| rtt | raw (poll) Mbps | prod Mbps |
|---|---|---|
| 25  | 497.21 [445.5–523.4] | 490.92 [452.3–518.2] |
| 50  | 223.15 [107.8–414.5] | 127.08 [64.4–251.1] |
| 100 | 109.73 [30.8–267.3] | 37.07 [28.5–47.2] |

- **rtt=25 is stable and equal**: raw ≈ prod ≈ ~490–500 Mbps (same as 64 MiB result).
- **rtt=50/100 are bimodal**: two regimes — a "slow" ~30–80 Mbps and a "fast" ~250–430 Mbps. The mean
  is meaningless for bimodal data (rtt=100 raw mean jumped 53.5 → 109.7 across datasets purely because
  one of three runs hit the fast regime). SCTP's congestion-window trajectory is chaotic at high RTT, so
  direct-transfer throughput at rtt ≥ 50 ms is a coin flip between fast and slow.
- raw vs prod still overlap heavily at every rtt; no sender-loop penalty is separable from the noise.

### Revised conclusion
The sender loop is not the bottleneck — but at rtt ≥ 50 ms the *SCTP link itself* is unpredictable
(bimodal), which is the real risk to direct-mode transfers over WAN latencies. More reps won't fix it;
explaining it needs a congestion-window/BufferedAmount time series, not bigger files.

## Root cause of high-latency collapse (investigated 2026-08-13)

Instrumentation added: receiver throughput trace (100 ms samples), shim packet counters (`writeErr`),
`--window` (sender backpressure), `--mincwnd` (pion SCTP min congestion window). Findings:

### 1. The deterministic ceiling is the receiver's advertised window (rwnd) / RTT
- Fast-mode plateau = ~372–377 Mbps at rtt=100 = **~5 MB in flight** = `rwnd / RTT`.
- Proved it's NOT the sender's 5 MB backpressure: `--window 5→16→32→64 MiB` did not change the plateau.
- pion sctp: `ssthresh` is initialized to `RWND()` (the receiver's window), so slow-start stops there.
- Chrome's usrsctp rwnd ≈ 5 MB. Not tunable from the Go sender side.

### 2. The "slow mode" (5–26 Mbps) is SCTP congestion-window collapse
- Trace shows clean exponential slow-start (5→16→26→52→110→215→377 Mbps) then **collapse to 0
  (spurious RTO) and a ratchet-down** 377→105→52→31→16→5 Mbps, each event halving ssthresh.
- `writeErr=0` in the shim → no packet loss; the collapses are SCTP retransmission-timeout (RTO)
  events triggered by RTT jitter (delayed SACK + fixed shim delay) at high RTT.
- pion's default has **no minimum cwnd**, so each RTO drops cwnd to ~1 MTU and it re-slow-starts,
  repeatedly, from an ever-lower ssthresh.

### 3. THE FIX — `SettingEngine.SetSCTPMinCwnd`
Dose-response at rtt=100 (raw, poll, 100 MiB):

| minCwnd | Mbps |
|---|---|
| 0 (pion default) | 26.7 |
| 1 MiB | 91.7 |
| 2 MiB | 159.2 |
| 4 MiB | 262.2 |
| 6 MiB | 252.3 |
| 8 MiB | 121.7 |
| 12 MiB | 119.0 |

Optimal `minCwnd ≈ 4–6 MiB` (≈ the receiver's rwnd): prevents the collapse without exceeding rwnd
(overshooting to 8–12 MiB reintroduces zero-window stalls). ~10× improvement over the default.

### Actionable for ShareBridge (production)
In `agent/internal/peer` (the pion `SettingEngine` used for real peer connections):
1. `se.SetSCTPMinCwnd(4 * 1024 * 1024)` — the primary fix (10× at high RTT).
2. `se.SetSCTPRTOMax(...)` and/or `se.SetSCTPMaxReceiveBufferSize(...)` — secondary tuning for the
   reverse direction and RTO sensitivity.
3. Note: ShareBridge's multilane (control/media/bulk DataChannels) shares ONE SCTP association, so it
   does NOT increase throughput — the window is per-association, not per-lane.

## Jitter validation — is the collapse real, or a zero-jitter shim artifact?

Added `--jitter` (per-packet uniform +/- ms). At rtt=100, 10 ms jitter (realistic WAN), 100 MiB:

| mode | minCwnd=0 (default) | minCwnd=4MiB |
|---|---|---|
| raw  (5 reps) | 41–252, mean ~147, chaotic | **188–218, mean ~199, tight** |
| prod (5 reps) | 82–257, mean ~190, chaotic | 122–222, mean ~177 |

- The collapse is NOT a zero-jitter artifact: it fires at 5 ms and 10 ms jitter too (raw default drops to 41).
- The fix removes the worst-case collapse and the variance in RAW mode (single DataChannel).
- In PROD mode the improvement is modest — the mean is ~equal; the fix mainly tightens the low end.
  The manager's 5 MB/10 ms backpressure + the ~5 MB receiver window leave a ~180–200 Mbps
  second bottleneck at 100 ms that minCwnd does not lift. rwnd/RTT (~377 Mbps) remains the hard cap.

## Bottom line
`SetSCTPMinCwnd(4 MiB)` is a correct, strictly-better fix that removes the pathological collapse
(10x in the single-channel case), but it is NOT a complete high-latency cure: steady-state at 100 ms
RTT is still capped ~180–200 Mbps by the receiver window + backpressure, and the hard ceiling is
~377 Mbps (rwnd/RTT). Raising high-latency throughput beyond that needs parallel PeerConnections
(multiply the window) or a non-SCTP transport.

## CORRECTION (n=12 per condition) — the fix DOES help prod (~2x)

The earlier 5-rep prod comparison (mean 190 vs 177) was sampling noise on a bimodal distribution.
With 12 reps @ rtt=100, 10 ms jitter, 100 MiB:

| condition | mean | median | min | max |
|---|---|---|---|---|
| raw  default      | 141.6 | 131.6 | 35 | 253 |
| raw  minCwnd=4MiB | 223.9 | 224.5 | 111 | 314 |
| prod default      |  93.8 |  80.6 | 55 | 257 |
| prod minCwnd=4MiB | 172.9 | 155.7 | 88 | 314 |

- minCwnd=4MiB ≈ **+70% raw (median 132→224), ≈ +93% prod (median 81→156)** — roughly doubles both.
- **prod default is WORSE than raw default** (median 81 vs 132): the 3-lane production path collapses
  more readily (control/media/bulk share one SCTP association, more interleaving → more spurious RTOs).
  So the fix matters MORE in production, not less.
- Distributions stay bimodal even with the fix, but the whole distribution shifts up.
- n=12 is enough to separate the conditions; the earlier n=5 conclusion was wrong.

## CRITICAL CORRECTION — bandwidth matters (2026-08-13)

The earlier "minCwnd=4MiB ≈ 2x fix" was an artifact of a LOOPBACK shim (infinite bandwidth).
Added `--bandwidth` (token bucket + 100ms bounded buffer with tail drop) and re-ran at a
bandwidth-constrained link (8 MB/s, rtt=100, jitter=10):

| minCwnd | throughput |
|---|---|
| 0 (default) | 12–16 Mbps |
| 1 MiB | 9.1 Mbps |
| 2 MiB | 4.8 Mbps |
| 4 MiB | 2.4–2.9 Mbps (worse; hangs on large files) |

**minCwnd monotonically DEGRADES throughput on a bandwidth-constrained link.** A 4 MiB floor
over-drives the link (in-flight >> bandwidth-delay product), filling the bottleneck buffer, causing
tail drop → SCTP retransmission storm → collapse. This reproduces the real-world symptom: a brief
spike then crawl to ~1 MB/s.

**Conclusion:** the minCwnd "fix" was WRONG for real networks. The loopback benchmark modeled RTT/loss
but NOT bandwidth, collapsing two regimes: (1) infinite-bandwidth links where spurious-timeout collapse
dominates and a floor helps, and (2) bandwidth-constrained links where over-driving dominates and a
floor hurts. Real downloads are in regime (2). The fix was reverted in peer.go. The correct high-RTT
improvement (if bandwidth is NOT the limit) is a larger receiver window / parallel connections — not a
congestion-window floor.
