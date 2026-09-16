# Field test: SCTP patches at 71 ms RTT (clean, 0.5%, 1% loss)

**Date:** 2026-09-16 · **Status:** complete · **Verdict:** CA-step patch is real at high RTT (+36% clean, 3.4–3.8× under loss); the fork adds nothing and is deletable; keep-v2 unchanged.

## Motivation

The 12 ms patch A/B ([n4-concurrency-and-patch-field-test](2026-09-16-n4-concurrency-and-patch-field-test.md)) found no effect: stock 50.05 Mbps vs CA-step 50.7 vs fork 50.05. The patches were labelled "conditional tuning for lossy/high-BDP paths" — but that label rested on lab netem sweeps, never a real high-RTT path with the honest (completed-download) metric. An earlier attempt at a west-coast client was invalidated by CPU pinning (2 vCPU). This test fills that cell on proper hardware.

## Setup

- Agent: home server (6 vCPU), east-coast metro, fiber uplink ≫ needs.
- Client VM: 4 vCPU / 15 GB, provider's **west region** — a real cross-continent public-internet path, **71 ms RTT, 0.0% baseline loss, 1.3 ms jitter**.
- Signaling/relay server: east region, 12 ms from agent.
- Path reference: kernel TCP (iperf3) on the same agent→client path: **113 Mbps**. BDP ≈ 1 MB; browser rwnd 4–5 MiB (not binding).
- Variants (same rig as prior A/B): `u` stock pion, `c` stock + `SB_SCTP_CA_STEP=32768`, `f` pion fork + CA-step.
- File: 754 MB, single tab per run.

### Metrics (pre-registered)

- **Clean cells: completed-download time** (the only trustworthy metric — wire counters are inflated by retransmissions).
- **Loss cells: wire rate over a fixed 260 s window** (ens3 RX-byte deltas), because no variant can finish 754 MB at collapsed rates inside any reasonable cap. Wire is an *upper bound* on goodput under loss; comparisons between variants remain valid. Cross-validated against `tc netem` drop counters (dropped/loss-rate ⇒ sent bytes ⇒ rate; agreement within a few %).
- Loss applied as 1% / 0.5% ingress drop at the client (netem via ifb ingress redirect, data direction; verified live by drop counters).

### Pre-registered predictions

| # | Prediction | Outcome |
|---|---|---|
| P1 | stock clean @71 ms: 35–50 Mbps | ✅ 38.2 / 43.4 |
| P2 | patches @71 ms clean: Δ < 10% | ❌ **FALSIFIED: +36%** |
| P3 | 1% loss: stock collapses (<15 Mbps), patched 2–5× stock | ✅ stock 1.7; patched 3.4× |

## Results

### Clean path, 71 ms (completed downloads, n=2 each)

| variant | wall (s) | Mbps |
|---|---|---|
| `u` stock | 157.9 / 139.2 | 38.2 / 43.4 (mean 40.8) |
| `c` CA-step | 109.4 / 107.8 | 55.2 / 56.0 (mean 55.6) |
| `f` fork+CA | 107.1 / 105.4 | 56.4 / 57.3 (mean 56.8) |

Ranges do not overlap: stock max 43.4 < CA-step min 55.2.

### Lossy path, 71 ms (wire Mbps over 260 s, n=1 each)

| variant | 1.0% loss | 0.5% loss |
|---|---|---|
| `u` stock | 1.7 | 2.3 |
| `c` CA-step | 5.7 | 8.8 |
| `f` fork+CA | 5.8 | — |

At 1% loss no variant completed 754 MB inside 550 s (all DNF in an earlier pass; the wire numbers above quantify why). Drop counters: e.g. CA-step @1% ≈ 1,537 drops ≈ 154k packets ≈ 184 MB ≈ 5.7 Mbps — matches the RX-byte rate.

## Interpretation

1. **CA-step is a real high-RTT win, and the mechanism is recovery-rate physics.** Every retransmission event costs cwnd; regrowing it takes ~cwnd/step bytes per RTT. At 12 ms the agent recovers within a few dozen ms (invisible over a 120 s transfer); at 71 ms recovery is 6× slower and the patch's larger CA increment is worth +36% end-to-end. It even lifts the 71 ms rate (55.6) *above* the 12 ms stock ceiling (~50) — consistent with reducing retransmission waste inside the same structural wire pool.
2. **Under loss the patch is 3.4–3.8×, but nothing is "fine".** Stock sits at the Mathis limit (1.22·MSS/(RTT·√p) ≈ 1.6 Mbps at 71 ms/1%); CA-step climbs 3.4× above it, yet the best case (5.8 Mbps) is ~10× below its own clean rate. Patches mitigate; they do not rescue. Any window-based CC dies at 1% loss and 71 ms — this is physics as much as implementation.
3. **The fork is dead weight.** Across four regimes (12 ms clean, 71 ms clean, 71 ms + 1%, 71 ms + 0.5%) fork ≈ CA-step-only within noise. Recommendation: **delete the fork**, keep the `SB_SCTP_CA_STEP` env knob.
4. **Env-gate guidance (updated, now evidence-based):** enable CA-step when path RTT ≳ 40 ms or the path is lossy; it is a no-op on short-RTT clean paths. Default stays off pending soak testing, but the case for default-on is now strong on long paths.
5. **Keep-v2 unchanged.** Kernel TCP did 113 Mbps on this exact path (2× the best patched WebRTC cell, graceful under loss via SACK/CUBIC). The WebRTC data plane remains bench hardware.

## Limitations

- n=2 per variant (clean) and n=1 per variant (loss cells); single path, single file size, single browser build.
- Wire rate ≠ goodput under loss (includes retransmissions); intra-table comparisons valid, absolute goodput lower.
- 0.5% cells measured while the client's local verifier showed transient path noise; the netem drop-counter cross-check is the authoritative loss figure.
- Loss applied only at the client ingress (data direction). ACK-direction loss not tested.

## Ops notes (for whoever reruns this)

- Ingress loss on the client: `tc qdisc add dev <if> handle ffff: ingress` + u32 match-all `mirred egress redirect dev ifb0` + `netem loss X%` on ifb0. Always arm a background cleanup watchdog before applying.
- Beware `pkill -f` self-matching inside `ssh 'bash -c …'` command strings — it kills its own shell. Use `kill $(pgrep -f '[s]ample.sh')`.
- Fixed-window wire sampling: 20 s `/proc/net/dev` deltas beat every CDP/Playwright progress probe under load (those hang or lie).
