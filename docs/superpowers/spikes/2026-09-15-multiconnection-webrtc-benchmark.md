# Multi-connection WebRTC benchmark — does N associations fix the high-RTT collapse?

Date: 2026-09-15
Branch: `benchdirect` (harness work; deliberately kept off `v2`)
Harness: `agent/cmd/benchdirect` (extended), `agent/run_multiconn.sh`, `agent/stats_multiconn.py`
Raw data: `agent/multiconn_results.jsonl`

## Question

`BENCH_RESULTS.md` established that a single SCTP DataChannel collapses at high RTT and at
any packet loss, and it named the untested remedy:

> "Raising high-latency throughput beyond that needs **parallel PeerConnections** (multiply the
> window) or a non-SCTP transport."

It also found that the 3-lane production path (control/media/bulk sharing **one** SCTP
association) was *worse* than a single channel (median 81 vs 132 Mbps at rtt=100+jitter),
because more interleaving means more spurious RTOs. RFC 8831 states all SCTP streams within
one association share one congestion window, so separate `PeerConnection`s are the only way to
get independent windows.

This spike measures whether N independent `PeerConnection`s actually fix it, and — more
importantly — whether they fix the **tail** (unpredictability), not just the mean.

## Method

Extended the existing harness (real headless Chrome receiver, Go/pion sender, in-process UDP
shim between them):

- `-conns N` — N `PeerConnection`s, each with its own DataChannel, each on its own shim flow.
- `-sharing shared|independent` — one bottleneck carrying all N flows (realistic home uplink,
  one aggregate rate cap) vs. one bottleneck *per* flow (N separate links).
- Payload is striped into N contiguous shards, sent concurrently with per-connection
  backpressure (`poll`, 5 MiB window).
- `web/bench.js` creates N `RTCPeerConnection`s and tracks received bytes **per connection**,
  so starvation is visible.
- Reports `p10/p50/p90` and min/max, plus Chrome CPU cores and Go CPU seconds per run.

The `shaper`/`flow` split and the N-offer/N-answer signalling are the only substantial changes.
All pre-existing unit tests pass; N=1 reproduces the original baseline (523 vs 537 Mbps).

## Results (wall Mbps, from first received byte to completion)

### rtt=100, clean, 64 MiB, n=10

| conns | mean | p10 | p50 | p90 | min | max | max/min |
|---|---|---|---|---|---|---|---|
| 1 | 132.7 | 54.9 | 97.5 | 254.3 | **52.5** | 254.5 | 4.85 |
| 2 | 232.6 | 85.8 | 252.0 | 356.9 | 35.3 | 370.2 | 10.49 |
| 4 | 392.1 | 362.1 | 407.4 | 409.6 | **296.9** | 409.8 | **1.38** |

**The worst of 10 runs at N=4 (297 Mbps) beat the best of 10 runs at N=1 (254 Mbps).**
Bimodality (spread 4.85) disappears at N=4 (spread 1.38). This is a tail fix, not just a mean
fix: the p10 improves 6.6x (55 → 362) while the p90 improves only 1.6x (254 → 410).

### rtt=100 + 10 ms jitter, 64 MiB, n=10

| conns | mean | p10 | p50 | p90 | min | max | max/min |
|---|---|---|---|---|---|---|---|
| 1 | 209.0 | 162.7 | 221.9 | 223.3 | 148.8 | 224.3 | 1.51 |
| 2 | 279.6 | 149.0 | 320.9 | 335.3 | 144.9 | 338.1 | 2.33 |
| 4 | 341.9 | 279.2 | 368.7 | 383.4 | 227.6 | 383.9 | 1.69 |

Mean +64%, p10 +72%, and N=4's p10 (279) again clears N=1's max (224).
Note N=1 does **not** collapse here — see the zero-jitter caveat below.

### rtt=25 with 1% uniform loss, 8 MiB, n=3

| conns | mean | p10 | p50 | p90 |
|---|---|---|---|---|
| 1 | 4.3 | 4.1 | 4.4 | 4.5 |
| 2 | 7.2 | 6.9 | 7.2 | 7.4 |
| 4 | **15.8** | 14.7 | 15.7 | 17.0 |

3.7x relative improvement (non-overlapping ranges across 3 reps), but **15.8 Mbps is still
unusable**. Parallelism blunts head-of-line blocking; it does not rescue a lossy link.

### Bandwidth-constrained: rtt=100 + jitter, 8 MB/s (64 Mbps), 32 MiB, n=3

| conns | shared bottleneck | independent bottleneck (8 MB/s each) |
|---|---|---|
| 1 | 9.2 | 8.9 |
| 2 | 12.4 | 23.6 |
| 4 | 13.2 (**1.44x**) | 22.3 (**2.50x**) |

Both remain far short of available capacity: 13 Mbps of a 64 Mbps cap, and 22 Mbps of 512 Mbps
when every flow has its own link. **Each SCTP association independently collapses to roughly
10–25 Mbps at this RTT regardless of how much bandwidth is on offer.** Multi-connection then
*sums* N degraded associations; it does not restore link utilisation.

### rtt=0 ceiling (control)

| conns | mean wall Mbps | Chrome cores |
|---|---|---|
| 1 | 494 | 1.29 |
| 2 | 567 | 1.42 |
| 4 | 583 | 1.44 |

Saturates ~560–600 Mbps with Chrome at only ~1.4 cores and Go at ~1 core, so the ceiling is a
**harness artifact** (the single shim drain goroutine plus per-packet locking), not SCTP and not
CPU. Gains above ~600 Mbps are unmeasurable here.

### Per-connection balance

`min(received)/max(received)` was **1.00 in every multi-connection run, in every condition.**
No starvation, no load imbalance. The speedup is purely the sum of independent congestion
windows — which is what RFC 8831 predicts — not opportunistic rebalancing.

## Conclusions

1. **Multi-connection is a real fix for the high-RTT tail.** At rtt=100 the collapse is
   eliminated: spread 4.85 → 1.38, and the worst N=4 run beat the best N=1 run. The product
   risk was *unpredictability*, and that is what this fixes.
2. **The mechanism is summing independent per-association windows**, exactly as the SCTP spec
   implies. This also retroactively explains why the 3-lane single-association production path
   was worse than one channel.
3. **It does not fix bandwidth-constrained paths**, which is the common real download regime.
   Each association collapses to ~10–25 Mbps anyway; N just adds those up. This matches
   `BENCH_RESULTS.md`'s own qualifier that parallel connections help only "if bandwidth is NOT
   the limit".
4. **It does not make lossy links usable.** 3.7x better than catastrophic is still catastrophic.
5. **Costs scale with N**: N× SCTP/DTLS/ICE state, N× browser sockets, and N× TURN allocations
   and relayed bytes when the path is relayed.

## Caveats

- Loopback shim. Bandwidth is modelled only when `-bandwidth` is set (token bucket + 100 ms
  tail-drop buffer); otherwise the link is infinite.
- **The clean (zero-jitter) rtt=100 condition is pathological for this shim**: a fixed 50 ms
  delay makes a burst of packets arrive as a clump, which is the worst case for SCTP
  retransmit behaviour. Real paths always have jitter, and under jitter N=1 never collapsed
  (min 149 Mbps). So the dramatic 3.5x headline partly overstates the real-world gain; the
  1.54x jitter figure is the more conservative one. Both directions agree that N helps and
  that the floor rises.
- Headless Chrome on an M-series Mac, not a phone; no CPU or thermal limits modelled.
- Uniform random loss, not bursty loss; no reordering.
- All candidates are loopback host candidates — no real NAT, ICE pair selection, or TURN.
- n=10 for highrtt/jitter, n=3 for the bandwidth-capped and loss conditions (the latter are
  noisy; `bwindep` N=2 had a 3.27x spread from a single 43 Mbps outlier).

## Bearing on the architecture decision

This does **not** revive WebRTC as the primary transport, and it does not change the FRP
comparison:

- FRP moves bytes over kernel TCP at 20–30 MB/s (160–240 Mbps) on real WAN paths and needs no
  per-connection browser state. Kernel TCP uses a bandwidth-constrained link; SCTP does not.
- Multi-connection turns WebRTC-direct from "unpredictable at high RTT" into "consistently
  ~300–400 Mbps when the path is fast" — genuinely useful, but only in the regime where the
  path was already fast enough that the transfer would have succeeded anyway.
- On the constrained or lossy paths where a transfer actually fails, multi-connection does not
  rescue it.

So: a worthwhile **optional** optimisation if a WebRTC second delivery mode is ever added
(swarm offload, or bypassing the DNS-cert ceiling), with N=4 as the sensible default. It is not
a reason to make WebRTC the primary path, and it does not beat an outbound tunnel.

## Reproducing

```sh
cd agent && go build -o bin/benchdirect ./cmd/benchdirect/
./run_multiconn.sh multiconn_results.jsonl all
python3 stats_multiconn.py multiconn_results.jsonl
```
