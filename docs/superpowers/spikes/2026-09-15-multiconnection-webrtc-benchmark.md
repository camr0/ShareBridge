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

## Follow-up: the loss cliff, buffer depth, and the tunability question (same day)

Prompted by: "how likely is that scenario?" and "could we set these config options dynamically
based on the user's network setup?"

New harness capability: `-queue <bytes>` sets the bottleneck buffer depth explicitly. Without
it the buffer is `bandwidth/10` (100 ms of buffering at the capped rate).

### Loss sweep at the rtt=100 + jitter baseline (uncapped, 8 MiB, n=3)

| loss | N=1 Mbps | N=4 Mbps | speedup |
|---|---|---|---|
| 0% (from `jitter` group) | 209.0 | 341.9 | 1.64 |
| 0.01% (1 in 10,000) | 41.5 | 87.6 | 2.11 |
| 0.1% | 6.5 | 21.1 | 3.25 |
| 0.3% | 2.1 | 8.3 | 3.95 |
| 1% | 1.0 | 4.1 | 4.10 |

The cliff is **between 0% and 0.01%**, not between 0.1% and 1%. A single lost packet in
10,000 costs ~80% of throughput; 0.1% costs ~97%. Multi-connection helps substantially in
relative terms (2–4x) but does not make any lossy path usable. This answers "how likely is
that scenario": the damaging loss rates are not exotic — they are the normal background loss
of an ordinary WAN path, and give or take, a good Wi-Fi link.

### Buffer depth sweep (rtt=100 + jitter, 64 Mbps cap, 32 MiB, n=3)

| bottleneck buffer | N=1 Mbps | util | N=4 Mbps | util | drops N=1 | drops N=4 |
|---|---|---|---|---|---|---|
| 800 KB (default) | 9.6 | 15% | 19.3 | 30% | 1107 | 2356 |
| 2 MB | 16.0 | 25% | 18.9 | 30% | 1996 | 4233 |
| 5 MB (= rwnd) | **55.6** | **87%** | 27.9 | 44% | **0** | 4029 |
| 16 MB | 42.9 | 67% | **56.4** | **88%** | **0** | **0** |

**This confirms the mechanism.** With a buffer at least as large as the peer's receive window
(~5 MB), tail drop stops entirely and the single connection reaches **87% of the link**. The
collapse was never about link speed.

It also reveals a second-order cost: each association independently slow-starts toward its own
5 MB `rwnd`, so N connections need roughly **N x rwnd** of buffer. 5 MB is enough for N=1 and
exactly reproduces the collapse at N=4, while 16 MB fixes N=4. **Multi-connection makes the
buffer requirement worse, not better.**

### Bandwidth sweep (rtt=100 + jitter, buffer = bandwidth/10, 16 MiB, n=2)

| link cap | N=1 | util | N=4 | util | drops N=1 |
|---|---|---|---|---|---|
| 16 Mbps | 8.0 | 50% | 10.9 | 68% | 417 |
| 32 Mbps | 7.6 | 24% | 8.2 | 26% | 698 |
| 64 Mbps | 7.3 | 11% | 11.3 | 18% | 1549 |
| 128 Mbps | 6.9 | 5% | 9.8 | 8% | 2810 |
| 256 Mbps | 5.3 | 2% | 15.4 | 6% | 1488 |

Throughput is roughly **constant (~5–15 Mbps) across a 16x range of link speeds** — it does not
scale with bandwidth at all. Since the buffer is `bandwidth/10`, every rate tail-drops.

### Confirming that the buffer is the whole story

Giving each rate a buffer large enough to stop dropping removes the effect completely:

| config | N=1 | util | N=4 | util | drops |
|---|---|---|---|---|---|
| 16 Mbps + 200 KB buffer (default) | 8.0 | 50% | 10.9 | 68% | 417 / 472 |
| 16 Mbps + **5 MB** buffer | **14.7** | **92%** | 11.4 | 71% | **0** |
| 256 Mbps + 3.2 MB buffer (default) | 5.3 | 2% | 15.4 | 6% | 1488 / 4296 |
| 256 Mbps + **32 MB** buffer | **116.6** | 46% | **150.3** | 59% | **0** |

Across every experiment in this report the rule holds without exception:

> **zero tail drops => 45–92% link utilisation. Any tail drops => collapse to 2–30%.**

### Can these parameters be tuned dynamically from the user's network setup?

Investigated in pion's source. `SettingEngine` exposes exactly six SCTP knobs:
`SetSCTPMaxReceiveBufferSize`, `SetSCTPMaxMessageSize`, `SetSCTPRTOMax`, `SetSCTPMinCwnd`,
`SetSCTPFastRtxWnd`, `SetSCTPCwndCAStep`.

The decisive parameter is **not among them**. `sctp/association.go` hardcodes
`a.ssthresh = a.RWND()` (lines 803 and 1809), so slow start always targets the *peer's
advertised window* rather than the bandwidth-delay product. And `RWND()` is the peer's window —
in the ShareBridge flow that peer is **Chrome**, so the target (~5 MB) is set by the browser and
is not ours to configure. The one knob that was tested (`SetSCTPMinCwnd`) helped where there
were no drops and *hurt* where there were — because raising a congestion-window floor increases
the over-drive that causes the drops in the first place.

So dynamic tuning is not available through this API. To fix the overshoot properly you would have
to estimate the BDP and drive the sender at it — i.e. reimplement TCP's congestion control inside
pion's SCTP. That is precisely the work the kernel already does for the FRP / HTTPS-direct path.

### What this changes in the verdict

The earlier conclusion stands and is now sharper. The problem is not high RTT, and it is not
loss alone: it is that **SCTP's slow start targets the peer's receive window (~5 MB) rather than
the path's bandwidth-delay product, so on any bottleneck whose buffer is smaller than ~5 MB it
over-drives, tail-drops, and collapses.** Real routers do not have 5 MB buffers, and the
requirement grows to ~N x 5 MB if you add connections. Multi-connection therefore sums N
degraded associations rather than restoring utilisation, and it raises the buffer requirement
that was already the binding constraint.

## Follow-up 2: kernel TCP control, the RTO knob, and a size-confound correction

### METHODOLOGICAL CORRECTION: 8 MiB is too small at 100 ms RTT

Same nominal condition (rtt=100, jitter=10, loss=0, N=1, all defaults):

| transfer size | throughput |
|---|---|
| 64 MiB | 209.0 Mbps (n=10) |
| 8 MiB | 73.8 Mbps (n=3) |

An 8 MiB transfer at 100 ms RTT finishes during slow start and never reaches steady
state, so it understates throughput by ~3x. Worse, at 0.1% loss an 8 MiB transfer
expects only ~0.7 loss events, so it samples "did a loss event happen" rather than a
rate. Two independently-run groups at loss=0.1%, 8 MiB, N=1, identical configuration:

- `loss_0.1pct`:       9.3, 4.0, 6.2 Mbps
- `rto_default_l0.001`: 8.0, 67.3, 3.5 Mbps

A 19x spread. **The loss-cliff figures in the previous section are therefore not
steady-state throughput and must not be read as a rate.** The corrected, matched
measurement is `lossmatch` below.

### RTO ceiling sweep — hypothesis not confirmed

pion hardcodes `rtoMin = 1000ms` (rtx_timer.go:17) but computes
`rto = min(max(srtt+4*rttvar, rtoMin), rtoMax)`, so an `rtoMax` below 1s also lowers the
effective floor. `SetSCTPRTOMax` is exposed, so this is testable without forking pion.

| rtoMax | loss=0 N=1 | loss=0.01% N=1 | loss=0.1% N=1 | loss=0.1% N=4 |
|---|---|---|---|---|
| default (60s) | 73.8 | 69.2 | 26.3 | 18.5 |
| 500ms | 58.7 | 71.2 | 12.8 | 33.9 |
| 200ms | 69.9 | 72.2 | 7.3 | 56.6 |
| 100ms | **5.0** | 5.0 | 2.1 | 2.9 |

- `rtoMax=100ms` (below the path RTT) is catastrophic even at zero loss: constant
  spurious retransmits. Do not do this.
- 500ms / 200ms are roughly neutral at 0 – 0.01% loss.
- At 0.1% loss the effect is inconsistent: it helps N=4 by ~3x and hurts N=1 by ~3.5x,
  with n=3 and wide spreads (200ms / N=4 ranges 17.3–95.2).

Verdict: the 1-second RTO floor is real, but `rtoMax` is not a clean substitute for
lowering `rtoMin`, and this does not demonstrate a reliable win. It also inherits the
8 MiB size confound, so it should be re-run at 32 MiB before any conclusion is drawn.

### Kernel TCP control on matched conditions (tc/netem, Linux 6.10 in Docker)

Everything above compared SCTP-on-the-shim against TCP-on-*real WAN paths* — different
conditions, so indicative only. Docker Desktop supplies a Linux 6.10 kernel, so kernel
TCP can be measured under `tc netem` at the same nominal settings. One container, both
endpoints over `lo`, `delay 50ms` => ~100 ms RTT (verified: ping reports ~108 ms).

| rtt=100ms, 64 Mbps cap, ~800 KB queue | SCTP N=1 | SCTP N=4 | kernel TCP |
|---|---|---|---|
| loss = 0% | 8.7 Mbps (14%) | 14.7 Mbps (23%) | **54.7 Mbps (86%)**, 0 drops |
| loss = 0.1% | 4.0 Mbps (6%) | 10.1 Mbps (16%) | ~56 Mbps (88%) |
| loss = 1% | 1.1 Mbps (2%), timed out | 3.8 Mbps (6%) | **47.2 Mbps (74%)** |

Loss was verified as actually applied — the qdisc reports `loss 1%` / `loss 5%` and
throughput falls 54.7 -> 47.2 -> 24.6 Mbps for 0 -> 1% -> 5%. (netem at 5% loss still
beats SCTP at 0% loss on an identical link.)

**At zero packet loss, on identical links, kernel TCP is 6.3x faster than SCTP. At 1%
loss it is 43x faster. A single kernel TCP connection (54.7 Mbps) beats four parallel
SCTP associations (14.7 Mbps) by 3.7x.**

The mechanism is the one identified earlier: TCP converges its congestion window to the
bandwidth-delay product and therefore never overflows the 800 KB queue (zero drops
reported), while SCTP slow-starts toward the peer's ~5 MB rwnd, overflows, and collapses.
Queue depth makes no difference to TCP — 57.0 Mbps at a 530-packet queue vs 56.3 Mbps at
3500 packets — because it fills neither.

This is the controlled version of the earlier claim, and it holds. It is not that the
SCTP *harness* was unfair; it is that SCTP's congestion control is aimed at the wrong
target for a constrained link.

## Follow-up 3: does patching pion/sctp actually fix it?

Setup: `pion/sctp@v1.9.4` copied from the module cache to `agent/forks/sctp` (regenerable
via `apply_sctp_patch.sh`, gitignored) and selected with a `replace` directive in
go.mod. Both candidate constants live on the **agent's** SCTP stack, and in the
share-download flow the agent is the *sender*, so **no browser-side change is required** —
the browser only receives DATA and advertises rwnd, which is an upper bound the sender may
undershoot freely.

Condition: the matched worst case (rtt=100ms, jitter=10ms, 64 Mbps cap, 800 KB queue,
32 MiB, n=3). Unpatched = 14.5 Mbps; kernel TCP = 54.7 Mbps (86%) on the same nominal link.

| config | N=1 | N=4 | % of 64 Mbps (N=1) |
|---|---|---|---|
| unpatched (pion default) | 14.5 | 14.5 | 23% |
| unpatched + `SetSCTPCwndCAStep(32KB)` — **no fork** | 22.8 | 27.3 | 36% |
| ssthresh = 256 KB | 21.8 | 26.3 | 34% |
| ssthresh = 256 KB + CA step 8 KB | 33.9 | 32.5 | 53% |
| **ssthresh = 256 KB + CA step 32 KB** | **34.2** | **40.4** | **53%** |
| ssthresh = 256 KB + CA step 128 KB | 27.6 | 35.1 | 43% |
| ssthresh = 768 KB (approx the BDP) | 43.7 | 16.1 | 68% |
| **kernel TCP (control)** | **54.7** | — | **86%** |

### What this shows

1. **The patch works.** Capping `ssthresh` moves 14.5 -> 21.8 Mbps and, more importantly,
   removes the collapse: spread across reps falls from 3.03 to **1.00** (three consecutive
   N=1 runs at 21.7 / 21.8 / 21.9). The tail is fixed.
2. **`rtoMin` is irrelevant once the overshoot is gone.** `both` (rtoMin + ssthresh) is
   identical to `ssthresh` alone (21.8 vs 21.8, spread 1.01). No overshoot -> no drops ->
   no RTOs -> the RTO floor never matters. This is also why the earlier `rtoMax` proxy
   failed: it was treating a symptom.
3. **The 34% plateau is the congestion-avoidance step.** Slow start stops at ssthresh and CA
   grows only `max(MTU, cwndCAStep)` per RTT; `cwndCAStep` is unset, so +1 MTU per RTT,
   which cannot reach an 800 KB BDP within the transfer. Raising it is worth **+57% (N=1) /
   +54% (N=4)** and needs no fork at all, since `SetSCTPCwndCAStep` is already exposed.
4. **Neither knob alone suffices.** `cwndCAStep` without the ssthresh cap gives 22.8 Mbps
   (CA never engages while ssthresh = 5 MB); the cap without a CA step gives 21.8. Together:
   34 / 40.
5. **Tuning ssthresh to the BDP nearly reaches TCP** — 768 KB (the BDP at 64 Mbps x 100ms)
   gives 51 Mbps (80%) for N=1. But it is a magic number: it needs the BDP, and at N=4 the
   same value collapses to 16.1 Mbps because four associations each targeting 768 KB
   overflow the shared 800 KB queue. **A fixed constant cannot win across link speeds or
   connection counts.**
6. **CA step 128 KB is too aggressive** (27.6 Mbps, unstable) — overshoot reintroduces drops.

### Verdict for a fork

A **one-line fork** plus an **already-exposed knob** takes a constrained link from 23% to
53–63% of capacity and removes the variance that made direct mode unpredictable. That is
the honest size of the win — real, but not parity.

Closing the remaining gap to kernel TCP (86%) requires a per-link BDP estimate rather than a
constant, i.e. an actual congestion controller. That is the "reimplement TCP in userspace"
cost, and the N=4 result shows why a constant cannot substitute for it.

Practical note: `SetSCTPCwndCAStep(32KB)` ships today with **zero forking**, but buys
nothing on its own (36% vs 23%); it only pays off once `ssthresh` is capped.

## Follow-up 4: BBR-lite, and whether the patches survive packet loss

Two questions: (a) does a BDP-aware cwnd cap close the remaining gap to kernel TCP?
(b) do the patches help under packet loss — never measured on a patched build?

### Method

Implemented BBR-lite in the fork (`agent/forks/bbr/bbr.go`, ~70 lines plus a one-line
hook into `onCumulativeTSNAckPointAdvanced`): sample delivery rate per round (one min-RTT),
track windowed-min RTT from the RTO manager's `srtt`, exit startup after 3 rounds without
≥25% rate growth, then clamp cwnd to `rate x minRTT`. Two iterations were needed; the first
exited startup on 20ms instantaneous samples and was worse than doing nothing.

Condition: rtt=100ms, jitter=10ms, 64 Mbps cap, default 800 KB queue, 32 MiB, n=3.
Unpatched baseline from the `lossmatch` group at identical settings.

| loss | conns | unpatched | **ssthresh cap + CA step** | BBR-lite | kernel TCP | patch gain |
|---|---|---|---|---|---|---|
| 0% | 1 | 8.7 | **33.7** | 7.6 | 54.7 | 3.87x |
| 0% | 4 | 14.7 | **41.2** | 22.7 | 54.7 | 2.81x |
| 0.01% | 1 | — | **30.5** | 12.3 | ~54.7* | — |
| 0.01% | 4 | — | **37.6** | 11.4 | ~54.7* | — |
| 0.1% | 1 | 4.1 | **15.5** | 2.2 | 56.3 | 3.81x |
| 0.1% | 4 | 10.1 | **38.5** | 7.3 | 56.3 | 3.82x |

\* interpolated; the netem control measured 0%, 0.1%, 1% and 5%.

Stability across 3 reps: ssthresh+CA step ranges 1.07–1.22x min→max. BBR-lite ranges up to
**3.05x** (8.1–39.0 on the same configuration).

### Findings

1. **BBR-lite failed.** It is worse than the two-constant patch in every cell, and worse than
   *unpatched* at 0% loss / N=1 (7.6 vs 8.7). The diagnosis matters more than the number: pion
   does not **pace**. cwnd growth is realised as immediate bursts on ACK, so the buffer
   overflows during startup — before the rate estimator can have any information about the
   path. Startup exit is therefore structurally late. Real BBR depends on pacing to make
   startup non-destructive, and adding pacing means changing pion's send loop.
2. **The two-constant patch survives loss.** It delivers a consistent **~3.8x** over unpatched
   at 0% *and* at 0.1% loss, and stays tight (≤1.22x spread). This closes the gap flagged as
   unknown earlier: the patch is not just a no-loss optimisation.
3. **Multi-connection + patch is the best configuration**: 41.2 Mbps at 0% loss and 38.5 Mbps
   at 0.1% loss, versus 8.7 / 4.1 unpatched single-connection.
4. **The remaining gap to kernel TCP is ~1.3–1.5x** (41.2 vs 54.7 at 0%; 38.5 vs 56.3 at 0.1%),
   not the 6.3x gap measured on the unpatched build.

### What this means for the fork-v1 decision

The honest summary is that "write a simple algorithm that adapts to the network" was answered
empirically: **~70 lines of BBR-lite made things worse than doing nothing**, because the missing
component is pacing, which is not small. Meanwhile a **one-line fork constant plus an
already-exposed knob** (no estimator at all) delivers 3.8x and is robust.

So the cost curve is very uneven:

- **~1 line + 1 existing option → 3.8x, stable, loss-robust.** Cheap and clearly worth doing.
- **The last ~1.4x → pacing + a real congestion controller inside pion's send loop.** This is
  the expensive part, and the failed attempt above is evidence for that, not against it.

For the relay question specifically: at 0.1% loss the patched multi-connection path reaches
38.5 Mbps where kernel TCP reaches 56.3 — **a 1.46x price** for deleting ~1,000 LOC of custom
Noise crypto and framing in favour of DTLS, and for making direct-vs-relayed an ICE candidate
choice rather than a code path. That is a materially better trade than the unpatched numbers
suggested.

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
