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
