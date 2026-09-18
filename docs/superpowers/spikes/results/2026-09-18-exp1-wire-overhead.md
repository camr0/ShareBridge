# Experiment E1 — wire-overhead / retransmit diagnosis — 2026-09-18

**Verdict:** In the lab the v1 WebRTC/SCTP path carries **1.08× the payload bytes on the wire
(1.12× counting IP+UDP headers, both directions incl. SACKs)** — measured three independent ways
(nettop per-process byte counters, the shim token bucket as a byte meter, and deterministic
cap-limited arithmetic) and reproduced exactly by a code-level packet model. There is **no ÷3
anywhere**: on a clean, rate-limited path the wire datagram count is *bit-for-bit deterministic*
(86,065 datagrams per 64 MiB = 1344.8/MiB = 896 data + 448.8 SACK) and random loss up to 1% —
which collapses goodput 16–62× — adds at most **+1.2% data-direction wire bytes** (≈ one-for-one
loss recovery; datagram *count* rises ≤ +6.7%, the rest being extra SACKs). Spurious retransmission
on the clean path is exactly zero when the bench is rate-limited; under load-noise it is bounded by
+4.1% worst single run. Stalls under loss are many short halts (100–200 ms) plus 0.9–1.2 s stalls
matching the 1 s RTO floor — **no long mid-transfer plateaus exist in this harness** (longest flat
stretch in all 91 E2 runs + all 37 E1 runs: 0.7 s / 1.2 s). The field's ~3× NIC-counter gap is
therefore **not protocol framing and almost certainly not SCTP retransmission volume**; it points at
counter aggregation (other traffic/directions on the agent host) or a measurement-window artefact —
a per-5-tuple field capture would settle it.

**Setup:** lab harness `agent/cmd/benchdirect` in `.worktrees/benchdirect` on the lab Mac (this Mac;
no field rig, no production container touched). Built 02:42 from go.mod resolving
`pion/webrtc/v4 v4.2.11` → `pion/sctp v1.9.4`, `pion/dtls/v3 v3.1.2`. Roles (code read): **Go/pion
is the DATA sender**, headless Chrome is the receiver (`rawbench.go` `pump()`→`dc.Send`;
`web/bench.js` `note()`), the shim is a UDP-loopback bottleneck (delay/loss/jitter/bandwidth/queue;
`shim_drop` counts **queue tail-drops only** — random `--loss` is applied silently and is *not*
counted anywhere).

Matrix (all `--mode raw --size 64MiB --chunk 16KiB --backpressure poll --deadline 120`+,
strictly one `benchdirect` at a time, n=3 unless noted):
rtt {12, 71} × loss {0, 0.001, 0.01} uncapped; rtt {12, 71} × {bw 8MB/queue 5MB, bw 8MB/queue 100KB,
bw 2MB/queue 64KB} at loss 0; plus 3 dedicated nettop-instrumented runs (bw 2MB: clean/loss
0.001/loss 0.01, queue 5MB for the lossy ones so tail-drops = 0). 37 sweep runs + 1 nettop run + 10
sanity runs, all `shim_write_err = 0`, no cell discarded.

⚠️ **Bench reliability (per runbook):** the §3 sanity config (`--rtt 71 --mode raw --size 32MiB`,
poll) bracketing the sweep gave **10.6–245.7 Mbps over 10 repeats (23×)** — pre-sweep 5 runs
{17.0, 10.6, 213.5, 114.5, 140.3}, post-sweep 5 runs {89.5, 214.1, 157.0, 245.7, 126.7}; load avg
2.3–6.5, swap constant (6.36 G used of 7 G), top CPU `WindowServer`/`IPNExtension`/`backupd`.
**All mean-throughput numbers below are load-contaminated and quoted only for context.** The
results above rest on load-robust metrics: datagram counts, byte volumes (limiter/nettop), `shim_drop`,
and the stall structure. Rate-*capped* cells are fully deterministic (the token bucket, not the CPU,
is the bottleneck) — e.g. the three bw8MB/q5MB rtt12 runs forwarded **exactly 86,065 datagrams each**
at 59.5 Mbps.

## Q1 — The true wire/payload ratio and its composition

**Datagram construction (code facts, pion/sctp v1.9.4):** `initialMTU = 1228`; a DATA chunk is
`12 B SCTP common + 16 B chunk header + ≤1200 B payload`; bundling (`bundleDataChunksIntoPackets`)
fits exactly one full chunk per packet (1216+12 = 1228). A 16 KiB message fragments into 14 chunks
(13×1200 + 784) → **896 data datagrams per MiB, mean SCTP packet 1198.3 B**. The peer SACKs every
second packet (delayed ACK, `ackModeNormal`) → **448.7 SACK/MiB × 28 B SCTP**. DTLS 1.2 with
AES-128-GCM adds 37 B per record (13 hdr + 8 explicit nonce + 16 tag — the size *solved* from the
measurements below; DTLS 1.3 would give 17 B and is excluded by them).

**Measured (three independent instruments, 64 MiB, bw 2MB cap so the limiter meter is exact):**

| instrument | data-dir wire / payload | SACK dir | both dirs L4 | both dirs + IP/UDP (28 B/dgram) |
|---|---|---|---|---|
| token bucket (`R×t` = forwarded L4) + fwd count | — | — | **1.085** (clean), **1.084** (loss 0.001) | 1.119–1.120 |
| nettop `in`/`out` algebra (in = d+2s, out = 2d+s) | 1.058 (clean) / 1.055 (0.001) | +2.4–2.9% | 1.082 / 1.084 | 1.119 / 1.120 |
| model: 896×1235.3 + 448.7×65 per MiB | 1.055 | +2.8% | **1.0837** | **1.1196** |
| deterministic cap check: 8 MB/s cap → 59.52 Mbps goodput | — | — | 1.076–1.08 | — |

At **loss 0**: rtt12 = 1.082 (L4 both-dirs), rtt71 = 1.084 — identical, RTT does not change the
ratio (the rtt71 2MB cells completed at the cap: 4.585 Mbps×117 s). **Composition of the +8.4% L4:
SCTP DATA headers +2.4%, DTLS record +3.2%, SACK traffic +2.8%** (adding IP+UDP: +3.6% more).
E2's "2^20/780 B fixed fragmentation" reading was an artefact of dividing payload by *all* datagrams:
only 66.6% of forwarded datagrams carry payload; the rest are 65 B SACKs.

## Q2 — Ratio under loss (and real shim drops)

| cell (64 MiB, n=3) | mbps mean (context only) | dg/MiB delivered | excess vs 1344.5 | `shim_drop` (sum) | implied retransmit (data-dir bytes) |
|---|---|---|---|---|---|
| rtt12 l0 uncapped | 494.7 | 1344.5–1386.5 (m 1369.0) | +0…+42 | 0 | ~0 (best runs exactly 1344.5) |
| rtt12 l0 bw8MB q5MB | 59.5 (det.) | **1344.8 (×3 identical)** | +0.3 | 0 | **0** |
| rtt12 l0.001 | 26.1 | 1366.3–1395.0 (m 1381.1) | +22…+50 | 0 | +0.8% ±0.5 (nettop run: 1379.9/MiB) |
| rtt12 l0.01 | 8.0 | 1415.3–1420.9 (m 1417.9) | +71…+76 | 0 | **+1.2% ±0.5** (nettop run 1434.0) |
| rtt12 l0 bw8MB q100KB | 42.3 | 1359.4–1388.0 (m 1369.1) | +15…+43 | 551 (0.21%/run) | ≤ +3% dg only |
| rtt12 l0 bw2MB q64KB (n=2+instr) | 14.8 | 1366.3–1366.8 | +22 | 399 (0.15%/run) | — |
| rtt71 l0 uncapped | 87.2 (ceiling 209.8; 2/3 collapsed) | 1371.6–1399.9 (m 1389.7) | +27…+55 | 0 | ~0…+4% (worst single run +4.1%) |
| rtt71 l0 bw8MB q5MB | 42.3 (2/3 at 57.5; r3 regime-collapsed to 11.8) | 1344.8, 1344.8, 1357.9 | +0.3 | 0 | 0 (deterministic runs) |
| rtt71 l0.001 | 5.6 | 1365.5–1380.3 (m 1371.8) | +21…+36 | 0 | — |
| rtt71 l0.01 | **partial: timeout at 120 s, 21.9–22.1 MiB delivered** | 1408.2–1415.0 (m 1412.3) | +64…+70 | 0 | — |
| rtt71 l0 bw8MB q100KB | 7.8 | 1361.9–1362.2 | +17 | 641 (0.25%/run) | — |
| rtt71 l0 bw2MB q64KB (n=2) | 4.6 | 1376.0–1376.2 | +32 | 315 (0.18%/run) | — |

- **One-for-one expectation:** at loss 0.001, ~0.9 lost data datagrams/MiB → +0.9/MiB, +0.1% bytes;
  at loss 0.01, ~9.0/MiB → +9/MiB, **+1.06% bytes**. Measured byte-level excess (nettop-instrumented
  runs): **+0.8% at 0.001, +1.2% at 0.01** — i.e. **consistent with real loss recovery, no gross
  spurious retransmission in bytes**. The datagram-*count* excess at 0.01 (+64…+90/MiB, ~8× the lost
  count) is dominated by small packets: immediate SACKs with gap-ack blocks and retransmissions; even
  taken entirely as retransmissions it is bounded by +6.7% of datagrams.
- **Clean path spurious retransmission = 0** whenever the run is not scheduling-noisy: the
  deterministic cap-limited cells (all six rtt12/rtt71 bw8MB/q5MB non-collapsed runs) forwarded
  86,065–86,067 datagrams (1344.8/MiB), reproducible to the datagram. Uncapped loss-0 runs scatter
  +0–4.1% (1344.5–1399.9), correlated with *slower* runs — scheduling-induced reorder makes Chrome
  send extra immediate SACKs; it disappears at high speed when the machine is quiet (nettop run:
  1344.5 exactly at 504 Mbps).
- Goodput collapse under loss (rtt12: 494.7→26.1→8.0 Mbps; rtt71: 209.8→5.6→timeout at 1.5 Mbps) is
  **not** wire-volume-driven: the link goes *idle* (congestion-window collapse + 1 s RTO waits), the
  wire ratio stays ≈1.1×. The same holds for real tail-drops (q100KB/q64KB cells).

## Q3 — Ramp and stall structure (`samples`, 100 ms cumulative)

- **TTFB** (page-load→first byte, incl. ICE+DTLS): 0.5–0.6 s (rtt12), 1.0–1.2 s (rtt71) — essentially
  constant across cells.
- **Ramp on clean fast runs:** t50 = 0.5–0.7 s, t90 = 0.9–1.1 s at rtt12 (500 Mbps, whole 64 MiB in
  ~1.05 s); t50 = 1.3 s, t90 = 2.2 s at rtt71 (210 Mbps). No stalls (0–1 per run, ≤300 ms).
- **Rate-capped runs:** perfectly linear (t50 ≈ 4.5 s, t90 ≈ 8.3 s at 59.5 Mbps), **zero stalls** —
  the samples trace is a straight line.
- **Lossy runs:** t50/t90 stretch (rtt71 l0.001: t50 ≈ 44–52 s, t90 ≈ 79–94 s for 64 MiB; rtt71
  l0.01 partials: t50 ≈ 59–64 s of the 120 s deadline for 22 MiB). Stall *structure*: **many short
  halts** (100–200 ms, distributed early/mid/tail with no periodicity) plus occasional **0.9–1.2 s
  stalls** (≈ the hard-coded 1 s RTO floor — E2's Finding 2), which cluster mid/tail at loss 0.01
  (e.g. rtt12 l0.01: 5–6 stalls/run, 4.6–5.4 s total, longest 0.9–1.0 s; rtt71 l0.01: ~195 stalls
  totalling ~21–23 s per 120 s, longest 1.2 s).
- **No long mid-transfer plateaus:** the longest flat stretch (<10% of median delta) anywhere in this
  experiment is **1.2 s**, and re-analysing all 91 E2 runs (raw+prod) the longest is **0.7 s**. The
  Sept-15-style "flat 12.9 MB" plateau **does not reproduce** in this harness at loss 0, under random
  loss, or under shim tail-drops — collapsed runs are *uniformly slow* (t50/t90 stretch out), not
  plateau-shaped. Whatever produced those traces, it is not the v1 transport on a lossy path.

## Q4 — Reconciling with the field (~3× NIC counters at 61 Mbps goodput, 754 MiB)

- **Protocol framing: ruled out.** Lab L3 ratio ≈ 1.12× both directions. To reach 3× via framing you
  would need ~190% overhead; there is 8–12%.
- **Ramp/tail in the goodput window: ruled out as primary cause.** TTFB+ramp ≈ 1 s on a ~98 s
  transfer is ~1–2%.
- **Real path loss → retransmission: lab data argues strongly against.** This stack reacts to loss
  (even 1%) by *going idle*, not by flooding retransmissions: wire stays ≈1.1× while goodput falls
  16–62×. A genuine 3× wire measurement would require ~120 Mbps of extra traffic — more than 10×
  anything observed at equivalent loss in the lab.
- **Counter double-counting / aggregation: not ruleable out from the lab — the leading candidate.**
  The agent host's NIC counters sum *all* traffic of *all* containers and flows in both directions;
  the session itself contributes only ~1.12× incl. SACKs. (In the lab, both-directions counting adds
  just 3% because SACKs are 65 B.)
- **Settling measurement (field):** (a) `tcpdump` on the agent host filtered to the session's
  5-tuple (or nettop/flow-level accounting) for one 754 MiB download — compare per-flow bytes to NIC
  deltas; (b) the same NIC delta with every other container quiesced; (c) SCTP-level counters
  (Chrome `webrtc-internals` / pion stats retransmit counts) for the session; (d) goodput window
  strictly first-byte→last-byte from agent logs.

## Interpretation (v1 vs v2)

The wire cost of the v1 transport itself is **8.4% L4 / 12% L3** — indistinguishable from a native
TLS/TCP relay's framing. The v1-v2 decision therefore cannot rest on wire overhead. What the lab
*does* show is the known fragility: 0.1% loss collapses goodput 16–35× (and 1% loss breaks it
outright at high RTT) with the link idle, not congested — a recovery-latency problem (1 s RTO floor,
cwnd collapse), consistent with E2, and the lever for v1 improvement is loss recovery, not framing.

## Caveats

1. Means are load-contaminated (sanity spread 10.6–245.7 Mbps); only deterministic/rate-limited cells,
   counters, byte volumes and stall structure are load-robust. 2 of 3 rtt71 bw8MB/q5MB runs and 2 of 3
   rtt71 loss-0 uncapped runs regime-collapsed (a bench artefact, `shim_drop = 0`, excess ≤ +4%).
2. rtt71 l0.01 cells are **partials** (receiver timeout at the 120 s deadline, ~22 MiB delivered);
   their dg/MiB is per *delivered* MiB and their mbps is wall-goodput of a truncated transfer.
3. Raw mode has **Chrome as receiver and pion as sender** (inverse of the field for the data path);
   wire volumes are direction-agnostic, but any client-sink conclusion needs the prod-mode caveat.
4. The data/SACK datagram split is derived (delayed-ACK cadence 0.501 + solved DTLS size 37 B);
   per-direction datagram *counts* are not directly instrumented (±10% on the split, ±1% on totals).
5. The loss-0.01 instrumented run sat below the cap (goodput 0.94 MB/s < 2 MB/s), so its bucket meter
   is invalid; its ratios come from the nettop algebra + window-aligned payload only (±0.5–1%).
6. nettop byte counters are socket-level (no IP/UDP headers); the +28 B/dgram L3 correction is
   modelled, and lo0 interface counters do not count loopback UDP on this macOS (calibration probe
   in `raw/calibrate_lo0.py` returned zero deltas — hence the nettop/bucket instruments).
7. Single-Mac, single-night, n=3 (n=2 for 2 MB meter cells); no statistical tests — the effect sizes
   quoted (12% vs 200%) are far outside the observed scatter of the robust metrics.

## Artifacts

Copied to `docs/superpowers/spikes/results/raw/` (all paths relative to that dir):

| file | what |
|---|---|
| `exp1-results.jsonl` | all 41 runs (37 sweep + 4 instrumented), full JSON incl. 100 ms `samples`, per-conn shim counters, load at run start |
| `sanity_pre.jsonl`, `sanity_post.jsonl` | the 10 sanity-bracket runs |
| `nettop_bw2MB_rtt12_r1.{nettop,json}` | clean instrumented run (queue 64 KB, 134 tail-drops) |
| `nettop_bw2MB_q5MB_rtt12_loss0.001.{nettop,json}` | loss-0.001 instrumented run (no tail-drops) |
| `nettop_bw2MB_q5MB_rtt12_loss0.01.{nettop,json}` | loss-0.01 instrumented run (below-cap regime) |
| `nettop_rtt12_loss0_r1.json` | 504 Mbps clean run, fwd = exactly 1344.5/MiB |
| `e1-runner.sh`, `e1-runner.log`, `sanity*.sh`, `nettop_bw.sh`, `nettop_lossy.sh` | exact runner scripts + chronological log |
| `analyze_e1.py`, `analyze_nettop.py`, `analyze_nettop_aligned.py`, `analyze_e2_pool.py`, `calibrate_lo0.py` | analysis code |
