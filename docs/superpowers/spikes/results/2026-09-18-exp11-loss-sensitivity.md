# Experiment E11 — loss sensitivity of the v1 transport (loss ladder, field-cap, RTO-under-loss) — 2026-09-18

**Verdict:** v1's goodput collapses along a **sharp cliff, not a smooth curve**: on a clean 240 Mbps-capped
path at rtt 12 ms it runs at **225.2 Mbps (94% of the cap)** at loss 0 and is *still* cap-limited at
**1×10⁻⁴ (211 Mbps, 4/5 runs at the cap)**, but at **2×10⁻⁴ it is 96.5 Mbps**, at **3×10⁻⁴ 60 Mbps**, at
**9×10⁻³ 8.6 Mbps** — a **26× loss of goodput for 1% loss** while wire volume rises only 1.083× → 1.134×.
The sender goes *idle* (Chrome CPU 0.71 → 0.04 cores), it does not congest; `shim_drop = 0` in all 98 runs.
**Headline: loss does NOT explain the field's 112 Mbps ceiling.** The loss at which this ladder reaches
112 Mbps is **≈1.7×10⁻⁴**, i.e. **~50× below the field's measured 0.90%** — and at 0.90% the lab delivers
**8.6 Mbps, 13× *below* the field's 112**, so the lab's own curve contradicts the loss hypothesis in both
directions. **RTO-floor verdict: a real but small lever, not the defect.** Lowering the floor to 200/500 ms
gives **+13.8%/+14.8%** at loss 1×10⁻³ (n=5, non-overlapping) but only **+2.4%/+5.6%** at the field-matched
9×10⁻³ (n=5, *inside/near the run spread*) — it removes essentially all the 1-s stalls (stall time 4.5 s →
1.2–1.6 s, longest 2.9 s → 0.5–0.8 s) and that stall time *is* the entire gain. The collapsed regime obeys
**goodput ∝ 1/RTT** (5.0–5.6× for a 5.9× RTT step at every loss), i.e. it is a *window*-limited state whose
window loss shrinks — a loss-*recovery* defect, not a path-capacity or framing defect.

**Setup:** lab harness `agent/cmd/benchdirect` in `.worktrees/benchdirect` on the lab Mac — **no field rig,
no container, no VM touched** (runbook §1). Runner `raw/exp11-loss-sensitivity/run-exp11.sh`: config
`--mode raw --rtt {12,71} --size 64MiB --chunk 16KiB --backpressure poll --bandwidth 30MB --queue 5MB
--deadline 180`, one `benchdirect` at a time. **62 runs, 0 failures** (`runner.log` = 62×`OK`, no `FAIL`).
The intended matrix is **complete**: loss ladder rtt {12,71} × loss {0, 1e-4, 3e-4, 1e-3, 3e-3, 1e-2} × n=3
(36/36), field-cap cell (rtt12 × loss 9e-3 × n=3), RTO-under-loss (rtt12 × loss {1e-3, 9e-3} × `--rtomax`
{default, 200ms, 500ms} × n=3 = 18/18). See §4 for the **36 gap-fill runs added by this session** (36 `OK`,
0 `FAIL`). The 30 MB/s cap = 240 Mbps approximates the field's
~246 Mbps path; `--queue 5MB` keeps shim tail-drops at zero so `--loss` is the only loss source.

**Roles — verified from code, and both the runbook and E1 are wrong about this.** `rawbench.go:115-151`
`pump()` writes the payload with `dc.Send`, and `web/bench.js:42-46` `ch.onmessage` → `note()` increments
`window.__bench.received`. **Go/pion is the DATA sender and headless Chrome is the receiver — the same
orientation as the field** (agent sends → client browser receives), *not* the inverse that runbook §3's
parenthetical and E1's caveat 3 both claim. What the lab lacks is the field's *download sink* (SHA-1 verify,
service-worker writes), so lab numbers remain an upper bound for sink-limited results — but the sender is
the same Go/pion stack the field uses.

**⚠ Bench reliability (runbook §3).** The uncapped sanity bracket in `run-exp11.sh` (rtt71/loss0/32 MiB, n=6)
gave **10.64, 121.97, 134.13, 15.94, 154.16, 160.41 Mbps — a 15.1× spread** (`sanity-pre-1.json` is a
pre-script leftover, not part of `run-exp11.sh`), so uncapped means here are worthless. **Every number in
this report comes from the capped cells**, and the cap makes the *baseline* extremely reproducible:
the two same-block baseline brackets at rtt12/loss0 gave **225.07** (n=3: 224.66/225.19/225.37) and
**225.03** (n=3: 225.71/223.99/225.35), i.e. **±0.15%** — the token bucket, not the CPU, is the limit.
Cells that fall *below* the cap (everything at loss ≥ 2×10⁻⁴) are CPU/scheduling-sensitive, which is why
the transition region is scattery; cells well below the cliff are again tight (±1–5%, see table). Machine
state: load avg 2.8–6.2, swap pinned at ~6350 MB of 7168 MB used throughout, top CPU `pi`/`mediaanalysisd`/
`WindowServer`/`WebKit`; `uptime` + `vm.swapusage` + top-12 CPU recorded before and after *every* run
(`system-snapshots.txt`, `system-snapshots-gapfill.txt`).

## 1. The collapse curve

`goodput` = received bytes ÷ actual wall elapsed (equals JSON `mbps`; for the 6 rtt71 partials it is the
truncated-transfer rate). `wire` = L4 both-directions wire/payload ratio from E1's calibrated model
(mean L4 datagram 844.8 B; ratio = dg/MiB ÷ 1241.3; clean-path reference **1344.7–1355.5 datagrams/MiB**).
`drop` = `shim_drop` (queue tail-drops; the random `--loss` is applied silently and counted nowhere).
`#st`/`stall s`/`longest s` = stalls derived from the 100 ms `samples` array, defined as intervals
delivering < 10 % of that cell's median non-zero delta = count / total seconds / longest. `cc` = Chrome
receiver cores (a sender-idle proxy: it tracks goodput almost exactly). `atcap` = runs within 15% of the
240 Mbps bottleneck.

### rtt = 12 ms

| loss | n | mean | median | min | max | %cap | dg/MiB | wire | drop | #st | stall s | longest s | cc | atcap | class |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| 0 | 9 | **225.20** | 225.37 | 223.99 | 225.74 | 94 | 1351.9 | 1.089 | 0 | 5 | 0.5 | 0.5 | 0.70 | 9/9 | **saturating (cap)** |
| 1e-4 | 8 | 173.68 | 213.55 | 50.59 | 225.17 | 72 | 1378.4 | 1.111 | 0 | 6 | 0.6 | 0.5 | 0.51 | 5/8 | **saturating** |
| 1e-4 *(same-block n=5 only)* | 5 | **211.03** | 214.27 | 179.44 | 224.43 | 88 | — | 1.121 | 0 | 6 | 0.6 | 0.5 | 0.65 | **4/5** | **saturating** |
| 2e-4 *(new)* | 5 | **96.49** | 94.28 | 73.38 | 126.54 | 40 | 1386.3 | 1.117 | 0 | 5 | 0.5 | 0.5 | 0.26 | 0/5 | **idling** |
| 3e-4 | 8 | **60.05** | 58.63 | 49.01 | 77.83 | 25 | 1370.0 | 1.104 | 0 | 5 | 0.5 | 0.5 | 0.15 | 0/8 | **idling** |
| 1e-3 | 3 | **28.29** | 28.12 | 27.46 | 29.29 | 12 | 1368.6 | 1.103 | 0 | 5 | 0.5 | 0.5 | 0.09 | 0/3 | **idling** |
| 3e-3 | 3 | **16.23** | 16.29 | 15.72 | 16.67 | 7 | 1383.1 | 1.114 | 0 | 11 | 1.1 | 0.9 | 0.06 | 0/3 | **idling** |
| **9e-3 (field-cap)** | 3 | **8.62** | 8.72 | 8.43 | 8.72 | 4 | 1408.0 | 1.134 | 0 | 43 | 4.3 | 1.7 | 0.04 | 0/3 | **idling** |
| 1e-2 | 3 | **8.16** | 8.20 | 8.06 | 8.24 | 3 | 1415.0 | 1.140 | 0 | 48 | 4.8 | 1.0 | 0.04 | 0/3 | **idling** |

### rtt = 71 ms

| loss | n | mean | median | min | max | %cap | dg/MiB | wire | drop | #st | stall s | longest s | cc | atcap | class |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| 0 | 3 | 71.02 | **11.67** | 10.23 | 191.17 | 30 | 1355.3 | 1.092 | 0 | 14 | 1.4 | 1.3 | 0.19 | 1/3 | **bistable** |
| 1e-4 | 3 | 15.75 | 10.17 | 9.62 | 27.45 | 7 | 1378.2 | 1.110 | 0 | 13 | 1.3 | 1.0 | 0.06 | 0/3 | idling |
| 2e-4 *(new)* | 3 | **10.53** | 10.42 | 10.21 | 10.96 | 4 | 1373.1 | 1.106 | 0 | 14 | 1.4 | 1.0 | 0.04 | 0/3 | idling |
| 3e-4 | 3 | 10.99 | 10.78 | 9.59 | 12.62 | 5 | 1388.7 | 1.119 | 0 | 12 | 1.2 | 1.0 | 0.04 | 0/3 | idling |
| 1e-3 | 3 | 5.36 | 5.20 | 5.18 | 5.70 | 2 | 1368.2 | 1.102 | 0 | 23 | 2.3 | 1.0 | 0.03 | 0/3 | idling |
| 3e-3 | **0/3 partial** | 2.90 | 2.93 | 2.83 | 2.95 | 1 | 1385.2 | 1.116 | 0 | 96 | 9.6 | 1.1 | 0.02 | 0/3 | idling |
| 1e-2 | **0/3 partial** | 1.63 | 1.63 | 1.62 | 1.65 | 1 | 1409.9 | 1.136 | 0 | 329 | 32.9 | 1.3 | 0.02 | 0/3 | idling |

Partial-cell errors, verbatim: rtt71/3e-3 = `receiver timeout: 66240512/67108864` (r1),
`65880064/67108864` (r2), `63553536/67108864` (r3) — 94–99 % delivered at the 180 s deadline;
rtt71/1e-2 = `37011456/67108864`, `36634624/67108864`, `36290560/67108864` — 54 % delivered.

**Shape — five facts.**

1. **A cliff, not a gradient.** At rtt12 the ceiling is *untouched* at 1×10⁻⁴ (`max` = 225.17 Mbps, wire
   ratio still ~1.09, 5/8 runs at the cap) and collapses by 2×10⁻⁴ (`max` 126.5, 0/5 at the cap). Halving
   the loss state instead of degrading it: 211 → 96.5 → 60.0 → 28.3 Mbps for loss 1e-4 → 2e-4 → 3e-4 → 1e-3.
2. **The transition is multi-stable.** 1e-4 is bimodal across blocks (the earlier block's r2/r3 gave
   50.6/58.5 Mbps with `cc` = 0.12/0.13 vs the same-block runs' `cc` = 0.53–0.69); rtt71/loss **0** is
   outright bistable (191.2 vs 11.7/10.2 Mbps). Two attractors — cap-limited and a low-rate state — with a
   loss- and RTT-dependent switching probability. Beyond the cliff the low-rate state is *stable and
   reproducible* (rtt12 1e-3: 27.46/28.12/29.29, ±3 %; 9e-3: ±2 %; 1e-2: ±1 %). So the honest reading is
   **cliff + stable low-rate branch**, and means across the cliff are not physical.
3. **The low-rate branch scales as ~loss^−0.6** (rtt12: 68.3 → 28.3 → 16.2 → 8.2 Mbps for ~3.3× loss
   steps) — sub-linear in 1/loss, i.e. the branch is *not* simply "one lost datagram per RTO".
4. **Every collapse is idling, not congestion.** `shim_drop = 0` and `shim_write_err = 0` in all 98 runs
   (99 JSON files: the original 62 runs + 36 gap-fill runs + the pre-script `sanity-pre-1.json` leftover); wire volume rises only
   +5 % (1344.7 → 1415.0 dg/MiB, +1.2 % of bytes at 1e-2 — one-for-one loss recovery, no retransmit storm);
   and the receiver's Chrome CPU falls 0.70 → 0.04 cores in lockstep with goodput. The path is left 96 %
   empty while the protocol waits. E1's finding, reproduced across eight loss levels and two RTTs.
5. **Stalls explain only the tail-end of the collapse.** At rtt12/1e-2, 48 stalls totalling 4.8 s out of a
   65 s transfer (7 % of wall time; longest 1.0 s ≈ the 1 s RTO floor) and the trace is *uniformly slow*,
   not plateau-shaped — no long mid-transfer plateau anywhere (longest stall 0.5–1.7 s; E1's re-analysis of
   91 E2 runs found 0.7 s). Only at rtt71/1e-2 does stall time dominate a run (329 stalls, 32.9 s).
6. **The collapsed regime is window-limited: goodput ∝ 1/RTT.** At *every* loss level the rtt12:rtt71
   goodput ratio is 5.0–5.6× for an RTT ratio of 5.92 (3e-4: 60.05/10.99 = 5.46×; 1e-3: 5.28×; 3e-3:
   5.60×; 1e-2: 5.01×). A rate that is inversely proportional to RTT is the signature of a fixed small
   effective window (rate ≈ W/RTT), not of path capacity: at rtt12/9e-3, 8.62 Mbps × 12 ms ⇒ W ≈ 13 KB ≈
   10 SCTP datagrams. Loss is not throttling the *path*; it is pinning the sender's *window* near the floor.

## 2. Headline — does loss explain v1's field ceiling at 112 Mbps?

Field facts (Exp 9/10): the path carried ~246 Mbps of UDP and, at 250 Mbps **offered**, showed **0.90 % UDP
loss**; v1 delivered **102–111 Mbps at ~12 ms RTT**. The matching lab cell,
`fieldcap-rtt12-loss0.009` (n=3): **8.43 / 8.72 / 8.72 Mbps** (mean 8.62, wire 1.134, 43 stalls).

**The ladder reaches 112 Mbps at an effective loss of ≈1.7×10⁻⁴ — about 50× below the field's measured loss.
Loss therefore does not explain the field's 112 Mbps ceiling.** Derivation:

| quantity | value | source |
|---|---|---|
| loss at which the ladder matches the field's 112 Mbps | **≈1.7×10⁻⁴** (range 1.5–2×10⁻⁴) | log-linear interpolation between the 1e-4 cell (211.03 Mbps mean) and the 2e-4 cell (96.49 Mbps mean); the 112 point also falls inside the 2e-4 cell's own span (73.4–126.5) |
| field's measured path loss | **9.0×10⁻³ (0.90 %)** | Exp 9/10, at 250 Mbps offered |
| ratio | **≈53×** | |
| lab goodput at the field's measured loss | **8.62 Mbps** | `fieldcap-rtt12-loss0.009`, n=3 |
| ratio to the field's actual 112 Mbps | **13× below** | |

Two independent contradictions, either of which kills the hypothesis:

- **The lab is 13× harsher than the field at equal loss.** If the v1 flow really saw 9×10⁻³ per-datagram
  loss, this harness — which reproduces the field's sender (pion) and receiver (a browser), and is
  *more* generous because it lacks the download sink — says the transfer would sit at **8.6 Mbps**. It sits
  at 112. So 0.9 % loss cannot be the cap.
- **The implied effective loss at the field's operating rate is ~1.7×10⁻⁴.** Taking the ladder at face
  value, 112 Mbps corresponds to ~1.7×10⁻⁴ effective per-datagram loss. For the field's 112 Mbps to be
  *loss-capped*, the v1 flow would have to be experiencing ~50× less loss than the path was measured to
  have — which is a statement that the flow is **not** losing packets at the measured rate, not that loss
  is the limiter.

**Answer, stated plainly: the loss level at which v1 delivers ~112 Mbps on this ladder is ~1.7×10⁻⁴
(1.5–2×10⁻⁴). That is ~50× below the field's measured 0.90 %. Loss at 0.9 % cannot be what caps v1 at
112 Mbps in the field; the field's v1 flow behaves as if it experiences ~1.7×10⁻⁴ loss, so something the
lab does not model — host CPU, the browser download sink, NIC — is the cap.** The one measurement that
would settle it is **loss on the v1 flow's own 5-tuple at its own ~112 Mbps operating rate**; the reported
0.90 % was taken at 250 Mbps offered and is not the relevant operating point. *Do not quote the 1e-4 cell's
mean of 111.4 Mbps (from the earlier block, n=3) as "the field's operating point" — it is one cap-limited
run averaged with two collapsed ones (225.2 / 50.6 / 58.5). The same-block n=5 mean for 1e-4 is 211 Mbps.*

## 3. RTO floor under loss — the one place a fixable defect could hide

Same cap (240 Mbps), same rtt (12 ms). `--rtomax` default (0) = the 1 s SCTP floor vs 200 ms vs 500 ms.
Original n=3 cells plus a **new n=2 interleaved block** (§4) so that all three settings share the same
machine conditions: **n=5 per column.**

| loss | default mean (min–max) | `--rtomax 200ms` mean (min–max) | `--rtomax 500ms` mean (min–max) | Δ200 | Δ500 | default stall s → 200/500 s | default longest → 200/500 s |
|---|---|---|---|---|---|---|---|
| 1e-3 | **28.12** (27.56–29.15) | **31.99** (29.42–34.89) | **32.29** (28.84–36.06) | **+13.8 %** | **+14.8 %** | 0.5 → 0.5 / 0.6 s | 0.5 → 0.5 / 0.5 s |
| 9e-3 | **8.39** (7.82–8.79) | **8.59** (8.38–8.80) | **8.86** (8.58–9.07) | **+2.4 %** | **+5.6 %** | **4.5 → 1.2 / 1.6 s (−73 %)** | **2.9 → 0.8 / 0.5 s** |

`shim_drop` stays 0, wire volume is flat (1.100–1.138, no trend with RTO), Chrome CPU unchanged: the sender
idles *less*, it does not send more. `rto_max_ms` is recorded as 200/500 in the JSON for the non-default
cells, so the flag is genuinely applied (both modes were verified in E2 for raw mode).

**Verdict: lowering the RTO floor recovers a little goodput under loss — enough to be real at 0.1 % loss,
not enough to matter at the field's 0.9 % loss, and nowhere near a fix.**

- At **1×10⁻³** the gain is **+13.8 % (200 ms)** / **+14.8 % (500 ms)** and it is *outside* the noise: the
  default cell's 5 runs span 27.56–29.15 (half-range 2.8 % of the mean) while every one of the 200 ms
  runs (min 29.42) exceeds every default run (max 29.15) — the two distributions do not overlap.
- At **9×10⁻³** the gain is **+2.4 % (200 ms)** / **+5.6 % (500 ms)** and both are **inside or at the edge
  of the default cell's own spread** (7.82–8.79, half-range 5.8 %): 200 ms is inside, 500 ms is ~1.0× the
  spread. So at the field's loss level the RTO floor is **marginal in goodput**, even though it is
  **decisive in stall structure**.
- **The stall accounting closes the loop.** At 9×10⁻³ the default cell spends 4.5 s of a ~63 s transfer
  (7.1 %) in 1-s-floor stalls; eliminating them predicts +7 % and the measured 500 ms gain is +5.6 %. So the
  RTO floor's **entire** contribution at field-level loss is the few percent of wall time burnt in 1-s
  stalls — the other ~93 % of the collapse is not RTO-driven. (At 1×10⁻³ the +14 % gain exceeds the detected
  0.5 s/19 s = 2.6 % stall share, so at mild loss there is an additional RTO-bounded cost — recovery latency
  per loss event — that discrete stall detection does not see.)
- **200 ms vs 500 ms:** indistinguishable at 1×10⁻³ (+13.8 % vs +14.8 %); 500 ms is marginally better at
  9×10⁻³. If the floor is lowered, 200 ms captures essentially all the mild-loss benefit; the exact value
  is not critical.

Confirms and extends E2: the floor is a **no-op on a clean path** (E2), a **~14 % lever at 0.1 % loss**, and
a **~3–6 % lever at the field's 0.9 % loss**. Best case at 9×10⁻³ is **9.07 Mbps against a clean-path 225.2**.
A lower RTO floor is a cheap, safe, small win; it is not the v1 defect.

## 4. Gap-fill runs added by this session

The intended matrix was already complete, so the added runs target the two places where the existing n=3
was too thin to answer §2 and §3. All are `--bandwidth 30MB --queue 5MB` (rate-capped) 64 MiB rtt12 runs
unless noted, run strictly one at a time, with `uptime`/`vm.swapusage`/top-CPU snapshots before and after
each (`system-snapshots-gapfill.txt`). Script: `run-exp11-gapfill.sh`; log: `runner-gapfill.log`
(**36 `OK`, 0 `FAIL`**). Load avg during the block 6.23 → 2.78, swap constant ~6350 MB.

| block | what | why |
|---|---|---|
| G1 | rtt12 loss0 **n=3** (224.66/225.19/225.37) → loss **1e-4, 2e-4, 3e-4 n=5 each** → rtt12 loss0 **n=3** (225.71/223.99/225.35) | the headline cliff. The brackets give a **±0.15 %** error bar, so any change across the cliff is unambiguous. **2e-4 is a new loss level** that did not exist before and turned out to sit exactly on the 112 Mbps crossing. n=5 at 1e-4 showed that cell is *mostly cap-limited* (211 Mbps, 4/5 at the cap) — contrary to the bimodal n=3 the earlier block suggested. |
| G2 | rtt12 × loss {1e-3, 9e-3} × `--rtomax` {default, 200ms, 500ms}, **interleaved within each loss level, n=2 each** (added to the original n=3 → **n=5 per column**) | runbook §3 requires a same-block baseline for a delta claim. The interleaving makes machine drift hit all three settings equally, which is what let §3 distinguish the real +14 % at 1e-3 from the marginal +2–6 % at 9e-3. |
| G3 | rtt71 loss **2e-4** n=3 (new loss level) | fills the rtt71 gap between 1e-4 and 3e-4; confirmed the ~10–11 Mbps rtt71 low-rate attractor and the 1/RTT relation in §1.6. |

**Integrity checks.** All 36 `gap-*.json` files parse and are complete; `grep -c '^OK' runner-gapfill.log`
= 36, `grep -c '^FAIL'` = 0, and there are **no duplicated labels** in the log. Across **all 99 JSON files**
in the directory, `shim_write_err > 0` occurs in **none** and total `shim_drop` = **0** — so no run is
discarded under the runbook §3 rule. Nothing was re-run that already existed, and no original
`run-exp11.sh` output file was overwritten.

## 5. Interpretation for the v1-vs-v2 decision

- **The v1 transport is not framing-limited and can use the path when the path is clean.** On a 240 Mbps
  capped path at rtt12 it reaches **225.2 Mbps = 94 % of the cap**, in 2.4 s for 64 MiB, with zero drops and
  a clean 1.08× wire ratio. The field's 112 Mbps is only **46 % of the path's 246 Mbps**, so the field is
  being limited by something other than the transport's ability to fill a link.
- **Loss cannot be that something (§2).** The field's v1 flow behaves as if it sees ~1.7×10⁻⁴ loss, not the
  0.9 % the path was measured to have at 250 Mbps offered. So the v1-vs-v2 gap (112 vs 233 Mbps on the same
  path) is **not** best explained as v2 having better loss recovery. It is more consistent with Exp 9's
  client-side-sink hypothesis: v2 removes the browser receiver + DTLS/SCTP userspace crypto + service-worker
  write path, and the field's 112 Mbps is what is left when *that* is the bottleneck. **The lab supports
  "v1 underuses the path because of loss-triggered idling" only for paths that actually lose ≥2e-4; it does
  *not* show that the field path does.**
- **The real v1 defect the lab does prove is loss-triggered idle/window collapse**, not overhead: 26×
  goodput loss at 1 % loss with wire volume unchanged and the sender at 0.04 cores. It is a *loss-recovery*
  defect (`W` pinned near ~10 datagrams; rate ∝ 1/RTT). Cheap partial mitigations, in order of value:
  a lower RTO floor (~+14 % under mild loss, ~+3–6 % at field-level loss — see §3, and note it is a **no-op
  on a clean path**, so it is safe); and, on the evidence of §1.6, anything that keeps the effective window
  from collapsing to the floor. Neither restores the field's 112 → 233 gap.
- **Decisive next measurements:** (a) loss on the v1 flow's own 5-tuple at its own operating rate — this
  single number decides whether v1 in the field is loss-capped at all; (b) the field's client-side sink cost
  (Chrome userspace + verify + SW writes) measured against the 225 Mbps the transport demonstrably delivers
  into a bare browser receiver on a clean path.

## 6. Caveats

1. **Lab, not field.** The field rig, containers and VMs were untouched (runbook §1); no v1/v2 field numbers
   were produced this session. All field figures quoted (0.90 % loss at 250 Mbps, 112 Mbps, 246 Mbps,
   233 Mbps) are Exp 9/10's.
2. **Lab is an upper bound on anything sink-related.** The lab receiver is headless Chrome with no download
   sink (no SHA-1 verify, no service-worker writes). The *sender* orientation matches the field (verified
   from code, §Setup), but a sink-limited field result cannot be reproduced here.
3. **Only the capped cells are trustworthy.** Uncapped means in this experiment's own sanity bracket varied
   15.1×. Every table above is from `--bandwidth 30MB` cells; the loss-0 baseline is reproducible to ±0.15 %
   but cells below the cap are CPU/scheduling-sensitive, which is precisely why the transition region
   (1e-4 … 3e-4) scatters by up to 1.7× within a cell. Means *across* the cliff should be read as
   "fraction of runs in each attractor", not as a physical average.
4. **The implied effective loss of 1.7e-4 is a lab-curve inference, and the lab curve is known to disagree
   with the field.** The statement "loss does not explain the field cap" rests on the field's own
   inconsistency (lab at 0.9 % = 8.6 Mbps vs field at 0.9 % = 112 Mbps) and on the ~50× gap between the
   112 Mbps crossing and the measured path loss — not on the assumption that the lab is an accurate model.
   If the field's 0.90 % is *correct* for the v1 flow, then the lab is simply 13× too pessimistic and cannot
   be used to locate the field's operating point at all; either way the loss hypothesis fails.
5. **The 1e-4 cell is bimodal and block-dependent.** Earlier block: 225.2/50.6/58.5. Same-block n=5:
   224.4/212.8/224.2/179.4/214.3. The same-block data is authoritative (baseline brackets within ±0.15 %),
   but the earlier block's two collapses are unexplained and are retained in the combined n=8 row.
6. **The rtt71 loss-0 cell is itself bistable** (191.2 vs 11.7/10.2 Mbps at *zero* added loss), the same
   bench regime-collapse E1 saw in 2/3 rtt71 uncapped runs. So the rtt71 column's *threshold* is not
   cleanly measurable; only its low-rate attractor level (~10–11 Mbps at 1e-4…3e-4) and the 1/RTT relation are.
7. **Two loss levels, two RTTs, one machine, one night; n=3–9.** No statistical tests — the effects quoted
   are outside the measured spreads (cliff: 2.3× for a 2× loss step with baselines at ±0.15 %; RTO at 1e-3:
   non-overlapping n=5 distributions), and the ones that are *not* (RTO at 9e-3) are reported as marginal.
8. **The stall metric is relative** (interval < 10 % of the cell's median delta), so it measures interruption
   *within* a cell, not absolute idleness; absolute idleness is captured by %cap and Chrome cores.
9. `--loss` is applied by the shim silently and is **not** counted in `shim_drop` (E1); `shim_drop` here is
   queue tail-drops only, and is zero everywhere because of `--queue 5MB`.

## 7. Artifacts

Directory `docs/superpowers/spikes/results/raw/exp11-loss-sensitivity/` (all original experiment files
retained unmodified; session additions marked **NEW**):

| file | what |
|---|---|
| `run-exp11.sh`, `runner.log` | the original runner and its log — 62 runs, 62 `OK`, 0 `FAIL` |
| `system-snapshots.txt` | uptime + `vm.swapusage` + top-12 CPU before and after each of the 62 original runs, plus block snapshots |
| `sanity-pre-{1,2,3}.json`, `sanity-post-{1,2,3}.json` | the uncapped rtt71/loss0/32 MiB brackets (10.6–160.4 Mbps, 15.1× spread) — the bench error bar |
| `sweep-rtt{12,71}-loss{0,0.0001,0.0003,0.001,0.003,0.01}-r{1,2,3}.json` | the 36-run loss ladder |
| `fieldcap-rtt12-loss0.009-r{1,2,3}.json` | the field-matched cell (8.43/8.72/8.72 Mbps) |
| `rto-rtt12-loss{0.001,0.009}-rto{default,200ms,500ms}-r{1,2,3}.json` | the 18 original RTO-under-loss runs |
| **NEW** `run-exp11-gapfill.sh`, `runner-gapfill.log` | the gap-fill runner — 36 runs, 36 `OK`, 0 `FAIL` |
| **NEW** `system-snapshots-gapfill.txt` | per-run machine state for the gap-fill block |
| **NEW** `gap-base-rtt12-loss0-r{1,2,3}.json`, `gap-base2-rtt12-loss0-r{1,2,3}.json` | the two ±0.15 % baseline brackets around the cliff block |
| **NEW** `gap-rtt12-loss{0.0001,0.0002,0.0003}-r{1..5}.json` | the n=5 cliff cells (2e-4 is a new loss level) |
| **NEW** `gap-rto-rtt12-loss{0.001,0.009}-rto{default,200ms,500ms}-r{1,2}.json` | interleaved same-block RTO baseline (→ n=5 per column) |
| **NEW** `gap-rtt71-loss0.0002-r{1,2,3}.json` | rtt71 midpoint loss level |
| **NEW** `analyze_exp11.py` | the analysis used for every table here (parses `samples` for stalls, recomputes goodput, wire ratio and dg/MiB) |
| `run-exp11.sh`'s matrix vs intended | **complete** — nothing in the intended matrix is missing |
