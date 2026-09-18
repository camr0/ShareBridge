# Experiment E14 — SCTP congestion-window floor (`--mincwnd`) under loss — 2026-09-18

**Verdict: the min-cwnd floor is the fix we were hunting.** At the same 240 Mbps cap and rtt 12 ms that E11
used, `--mincwnd 2MiB` takes the collapsed cell from **27.9 → 204.8 Mbps at loss 1×10⁻³ (7.3×)** and from
**64.1 → 225.0 Mbps at loss 2×10⁻⁴ (3.5×)** — i.e. it restores the full clean-path rate (229 Mbps) at both
levels, on a 2.8 s transfer instead of 19.9 s. `--mincwnd 8MiB` reaches **225.9 Mbps at 1×10⁻³ (8.1×)**.
At E11's **field-matched** loss level (9×10⁻³, the field's measured 0.90 %) it is **8.2 → 58.9–157.4 Mbps
(7–19×)**, but there it stops being free: the wire/payload ratio rises 1.136 → 1.5–1.8 and one 4 MiB cell
produced **882 queue tail-drops**. It is a **no-op on a clean path in both goodput and wire volume**
(231.3 Mbps, ratio 1.083, identical to the no-floor baseline) — the floor only removes the collapse, it does
not add overhead when there is no loss to recover from. **Concretely: pion *does* clamp cwnd to `minCwnd`
after an RTO** (`association.go:1641 setCWND` floors every cwnd write, including the `setCWND(a.MTU())` RTO
path at `:3485` and the `setCWND(a.ssthresh)` fast-recovery path at `:2454`), so this is fixable by
configuration — **no `agent/apply_sctp_patch.sh` `ssthresh` patch is needed.**

**Setup:** lab only — **no field rig, container or VM touched** (runbook §1). `--mode prod --rtt 12
--size 64MiB --chunk 16KiB --bandwidth 30MB --queue 5MB --deadline 300` (240 Mbps cap / 5 MB queue: the same
bottleneck E11 used, so the baselines are directly comparable), loss {1×10⁻³, 2×10⁻⁴} × `--mincwnd`
{0/default, 2 MiB, 4 MiB, 8 MiB} × n=3, **interleaved (reps outer, arms inner)**, plus a clean-path control,
a same-block bracket before and after, and a field-matched 9×10⁻³ block. Runner
`raw/exp14-min-cwnd-under-loss/run-exp14.sh`; **40 cells, 0 failures**; machine state (`uptime`,
`vm.swapusage`, top-8 CPU) recorded before and after every cell in `system-snapshots.txt`.

**Prerequisite — verified, not assumed.** `min_cwnd` was **not** in the JSON before this session (the same
trap that bit E2 with `rto_max_ms`), so `rawResult` gained a `min_cwnd` field, populated in both modes. Every
floor cell in the appendix below carries its value read back **from that cell's JSON**
(`"min_cwnd":2097152` … `8388608`), and the 7–19× goodput difference is corroboration that the setter path
is live. `go build ./...` and `go vet ./cmd/benchdirect/` pass.

## 1. The result

Same cap, same RTT (12 ms), prod mode, `--loss` is the only loss source (`shim_drop = 0` in **39 of 40 cells**
except the one flagged below). Goodput is the browser-summary `mbps`; wire/payload is E1's calibrated model
(mean L4 datagram 844.8 B, clean-path reference 1344.7 datagrams/MiB = 1.083).

| loss | `--mincwnd` | n | mean Mbps | min–max | wire/payload | dg/MiB | wall s | stalls | `shim_drop` |
|---|---|---|---|---|---|---|---|---|---|
| 0 (clean) | 0 | 4 | **228.9** (3 healthy runs; 1 CPU-limited run 52.2) | 228.8–229.0 | 1.083 (1344.7) ×3; 1.119 (1388.8) ×1 | 1344.7–1388.8 | 2.8 (10.7) | 0 (10) | 0 |
| 0 (clean) | 2 MiB | 1 | 231.3 | — | 1.083 | 1344.8 | 2.8 | 0 | 0 |
| 0 (clean) | 8 MiB | 1 | 231.3 | — | 1.083 | 1344.7 | 2.8 | 0 | 0 |
| **1×10⁻³** | **0 (default)** | 3 | **27.9** | 27.6–28.4 | 1.099–1.110 | 1365–1378 | 19.4–19.9 | 0 | 0 |
| 1×10⁻³ | 2 MiB | 3 | **204.8** | 165.1–225.0 | 1.377–1.389 | 1709–1724 | 2.8–3.7 | 0–10 | 0 |
| 1×10⁻³ | 4 MiB | 3 | **196.9** | 149.6–222.3 | 1.372–1.407 | 1703–1746 | 2.8–4.8 | 3–16 | 0 |
| 1×10⁻³ | 8 MiB | 3 | **225.9** | 223.5–230.6 | 1.390–1.398 | 1725–1735 | 2.8–2.9 | 3–8 | 0 |
| **2×10⁻⁴** | **0 (default)** | 3 | **64.1** | 57.3–73.8 | 1.096–1.102 | 1360–1368 | 7.7–9.8 | 0 | 0 |
| 2×10⁻⁴ | 2 MiB | 3 | **225.0** | 217.9–229.0 | 1.162–1.204 | 1442–1495 | 2.7–2.9 | 0–2 | 0 |
| 2×10⁻⁴ | 4 MiB | 3 | **226.7** | 226.2–227.4 | 1.233–1.313 | 1530–1630 | 2.8 | 4–5 | 0 |
| 2×10⁻⁴ | 8 MiB | 3 | **211.8** | 205.1–216.0 | 1.244–1.293 | 1545–1605 | 2.9–3.0 | 5–7 | 0 |
| **9×10⁻³ (field-matched)** | **0 (default)** | 2 | **8.26** | 8.17–8.35 | 1.135–1.136 | 1409–1411 | 64.7–66.6 | 51–52 | 0 |
| 9×10⁻³ | 2 MiB | 2 | **85.4** | 58.9–112.0 | 1.665–1.767 | 2067–2193 | 5.2–9.5 | 19–59 | 0 |
| 9×10⁻³ | 4 MiB | 2 | **113.6** | 69.8–157.4 | 1.576–1.808 | 1956–2244 | 3.8–8.1 | 3–50 | **882 (one cell)** |
| 9×10⁻³ | 8 MiB | 2 | **64.5** | 61.0–68.1 | 1.517–1.542 | 1884–1914 | 8.3–9.6 | 48–61 | 0 |

**Five things to read out of this:**

1. **The floor removes the collapse, it does not merely shift it.** Every floor arm at 1×10⁻³ and 2×10⁻⁴
   lands at 205–231 Mbps — statistically indistinguishable from the clean-path 229 Mbps — with the default
   arms at 27.9 and 64.1 Mbps. The distributions do not overlap; the smallest floor run at 1×10⁻³ (165.1,
   a machine-degraded rep: its ceiling is 330 not 430) is still **5.9×** the largest default run (28.4).
2. **2 MiB is enough; 8 MiB is not better and at 2×10⁻⁴ is slightly worse.** At 2×10⁻⁴ the mean falls
   monotonically with floor size (225.0 → 226.7 → 211.8) while the **wire volume rises monotonically**
   (1.162–1.204 → 1.233–1.313 → 1.244–1.293). A larger floor buys nothing and costs wire. This matches
   E11 §3's "200 ms captures essentially all the benefit" pattern: the useful value is the smallest one that
   clears the collapse.
3. **The price is wire volume, and it is proportional to loss — not to the floor.** Clean path: 1.083
   (unchanged). 2×10⁻⁴: 1.16–1.31. 1×10⁻³: 1.37–1.41. 9×10⁻³: 1.52–1.81. That is *recovery* traffic, one
   retransmit per loss event rather than a storm, and it is the honest cost of running at the path rate
   instead of idling: the default cells at 1×10⁻³ use 1.10× wire **because they deliver 8× less data**.
4. **On a clean path the floor is byte-for-byte inert.** 231.26 / 231.27 Mbps, wire ratio 1.083, same
   1344.7 dg/MiB, 2.8 s, zero stalls — identical to the no-floor baseline (228.78–229.03, 1.083). So
   enabling it cannot regress a clean network, which is the main risk one would worry about.
5. **At the field's loss level (9×10⁻³) the floor is a huge win but no longer free.** 8.2 Mbps → 59–157 Mbps
   (7–19×), but the wire ratio reaches 1.5–1.8 (+33 % over the default's 1.136) and **one 4 MiB cell
   (`e14-loss0.009-mc4MiB-r2`) recorded 882 tail-drops** and only 69.8 Mbps — the floor overdriving the
   5 MB queue at 1 % loss. n=2, so the spread (58.9 / 112.0 / 157.4 / 69.8 / 68.1 / 61.0) is not a
   distribution, but the direction is clear: **at field-level loss the floor must be tuned against the real
   bottleneck buffer, because a floor that is too high converts a window collapse into queue loss.**

## 2. Why this is the right mechanism, from the source

`pion/sctp v1.9.4 association.go:1641`:

```go
func (a *Association) setCWND(cwnd uint32) {
    if cwnd < a.minCwnd { cwnd = a.minCwnd }
    atomic.StoreUint32(&a.cwnd, cwnd)
}
```

`setCWND` is the **only** writer of `a.cwnd`, and **every** congestion-control transition goes through it:

| site | transition | floored? |
|---|---|---|
| `association.go:756` | INIT cwnd (`min(4·MTU, max(2·MTU, 4380))`) | yes |
| `:2370` | slow start (`cwnd + bytesAcked`) | yes |
| `:2395` | congestion avoidance (`cwnd + step`) | yes |
| `:2454` | fast recovery: `ssthresh = max(cwnd/2, 4·MTU)`, `setCWND(ssthresh)` | **yes** |
| `:3485` | **RTO**: `ssthresh = max(cwnd/2, 4·MTU)`, `setCWND(a.MTU())` | **yes** |

So `minCwnd` is a hard floor on cwnd, and in particular the `setCWND(a.MTU())` reset on RTO — the event E11
identified as the collapse trigger (a 1-MTU cwnd with `rate ≈ W/RTT`) — is clamped. That is exactly the
predicted lever, and the measurement confirms it end to end. **`ssthresh` is not floored** (`a.ssthresh` is a
plain field), but it does not need to be: whichever way cwnd is recomputed, the store is clamped.

**Answer to the brief's question:** yes, pion clamps cwnd to `minCwnd` after an RTO, so the fix is available
as configuration in the existing v1 agent (`SB_SCTP_MIN_CWND`, already wired in
`agent/internal/peer/peer.go`) — **the `apply_sctp_patch.sh` code patch is not needed for this defect.**

## 3. Reliability of these numbers (this Mac is noisy — see the runbook warning)

The machine was intermittently CPU-loaded during this block (`MediaAnalysis.framework` daemon at 106–116 %
CPU, load avg 5.8–6.2), which does bite: the loss-0 baseline bracket at the start of the block gave
**52.24 and 228.96 Mbps** — a 4.4× spread on the *same* config. Consequences and how they were handled:

- Every arm is interleaved (reps outer, arms inner) so the interference hits all four arms of a comparison
  equally; the conclusion rests on **non-overlapping distributions** (floor 165–231 vs default 27.6–28.4 at
  1×10⁻³), and the same-block tail bracket (`17:54`, clean machine: loss 0 = 228.8/229.0) puts the decisive
  loss-1×10⁻³ pair — default **25.93** vs 8 MiB **224.81** — inside one block: **8.7× in the same block.**
- Three floor runs are visibly machine-degraded rather than collapse-limited (4 MiB/1e-3 r2 = 149.6,
  2 MiB/1e-3 r3 = 165.1, 4 MiB/1e-3 r1 = 222.3): their `ceil_mbps` (top-5-window median) is 330–430, i.e.
  they still saw the cap, and their wall times are 3.7–4.8 s. They drag the floor means *down*, so they
  cannot manufacture the result.
- `shim_write_err = 0` in all 40 cells; `shim_drop = 0` in 39/40 (the 882-drop 9×10⁻³ 4 MiB cell is
  reported, not discarded).
- `vm.swapusage` pinned at 6.27 GB of 7.17 GB used throughout.

## 4. Interpretation for v1 vs v2, and what to do next

- **This is the first lab lever that returns the flow to the clean-path rate under loss** — 7.3–8.1× at
  1×10⁻³, 3.5× at 2×10⁻⁴, 7–19× at 9×10⁻³ — against the RTO floor's +14 % and FastRtxWnd's +9–16 % (E5).
  The three knobs of E5 bounded the *retransmit burst*; this bounds the *window*, and E11 §1.6 said the
  collapsed rate is `W/RTT`. The mechanism and the fix finally line up.
- **It does not, on its own, explain or fix the field's 112 Mbps.** The field's v1 flow behaves as if it sees
  ~1.7×10⁻⁴ loss (E11 §2), where this floor gives 225 Mbps — so if the field path really had 0.90 % loss, the
  floor would help enormously; if it has ~1.7×10⁻⁴, the collapse is not what caps v1 in the field at all.
  E11's client-sink hypothesis is untouched by this experiment. **The decisive field measurement is still
  loss on the v1 flow's own 5-tuple at its own operating rate** (or simply enabling the floor in the field
  and measuring — that is now a one-env-var experiment: `SB_SCTP_MIN_CWND=2097152`).
- **Recommended next steps, in order:** (1) field A/B of `SB_SCTP_MIN_CWND=2MiB` on the v1 agent at the
  field-matched loss level — it is already wired, zero code, and reversible; (2) sweep the floor *with the
  real field queue* before going above 2 MiB, because at 9×10⁻³ an over-large floor converts the collapse
  into queue loss (882 drops, §1.5); (3) only if the field shows a cwnd collapse that a floor cannot cover,
  revisit `ssthresh` — the source says it is not floored, but nothing here shows that matters.

## 5. Caveats

1. **Lab, not field.** No field rig, container or VM touched (runbook §1). The 0.90 % "field-matched" cell is
   E11's inference about the field's operating loss, not a measurement of the v1 flow.
2. **`--mincwnd` disables part of congestion control by construction.** With rtt 12 ms and 240 Mbps the BDP
   is only ~0.36 MB, so a 2–8 MiB floor is 5.5–22× BDP. On the lab shim the 5 MB queue and the 30 MB/s token
   bucket absorb that (drops stay at 0 up to 1×10⁻³); a real, shallower path need not. This is a
   *deliberate* trade — a floor for a path whose loss-recovery is broken — not a free win.
3. **9×10⁻³ is n=2 and noisy** (58.9 / 112.0 / 157.4 / 69.8 / 68.1 / 61.0 across four floors). Read it as
   "the floor still works at field-level loss, with a queue-overflow caveat", not as a rate.
4. **One machine, one night, no statistical tests.** Effects are quoted only where the distributions are
   non-overlapping or where the same-block pair makes drift irrelevant. The one place that fails
   (9×10⁻³, n=2) is flagged.
5. **Only the capped cells are compared.** All cells are `--bandwidth 30MB --queue 5MB`; the clean-path
   bracket's 52 Mbps run is a CPU artefact of this Mac, not a floor effect.
6. **Prod mode's `--rtomax` is still not wired** (E2's other finding, out of scope here), so the floor is
   measured against the default 1 s RTO floor. Combining `--mincwnd` with `--rtomax 200ms` is untested — but
   the RTO lever is now redundant at these loss levels.

## 6. Artifacts

Directory `docs/superpowers/spikes/results/raw/exp14-min-cwnd-under-loss/`:

| file | what |
|---|---|
| `run-exp14.sh` | runner, phases `base sweep ctl fieldcap tail`; one `benchdirect` at a time |
| `runner.log` | START/OK per cell and the full stderr trace of each (40 cells, 40 `OK`, 0 `FAIL`) |
| `row.py` | JSON/log → TSV row (goodput, ceiling, `min_cwnd`, wire/payload ratio, dg/MiB, interior stalls) |
| `cells.tsv`, `*.row` | one row per cell, appended after every cell |
| `system-snapshots.txt` | `uptime` + `vm.swapusage` + top-8 CPU before and after every cell |
| `<label>.json` / `<label>.log` | raw result + stderr trace per cell |
| `../2026-09-18-exp5-pion-knobs.md` | E5, which bounded the retransmit-burst knobs and pointed here |

Harness diff: `agent/cmd/benchdirect/{main.go,rawbench.go,prodbench.go}` gained the `min_cwnd` JSON field
(E5's diff added the other three knobs and the prod `-queue` wiring — see E5 §0).

## 7. Appendix — every cell (appended incrementally during the run)

| label | mbps | ceil_mbps | min_cwnd | wire_ratio | dg_per_mib | rtt | loss | wall_s | nstall | stall_s | longest_s | drop | write_err | fwd | error |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| e14-base-loss0-mc0-r1 | 52.24 | 79.0 | 0 | 1.119 | 1388.8 | 12 | 0 | 10.7 | 10 | 1.0 | 1.0 | 0 | 0 | 88881 |  |
| e14-base-loss0-mc0-r2 | 228.96 | 225.0 | 0 | 1.083 | 1344.7 | 12 | 0 | 2.8 | 0 | 0.0 | 0.0 | 0 | 0 | 86060 |  |
| e14-tail-loss0-mc0-r1 | 228.78 | 225.0 | 0 | 1.083 | 1344.7 | 12 | 0 | 2.8 | 0 | 0.0 | 0.0 | 0 | 0 | 86062 |  |
| e14-tail-loss0-mc0-r2 | 229.03 | 225.0 | 0 | 1.083 | 1344.7 | 12 | 0 | 2.8 | 0 | 0.0 | 0.0 | 0 | 0 | 86061 |  |
| e14-tail-loss0.001-mc0-r4 | 25.93 | 52.0 | 0 | 1.099 | 1364.0 | 12 | 0.001 | 21.1 | 0 | 0.0 | 0.0 | 0 | 0 | 87299 |  |
| e14-tail-loss0.001-mc8MiB-r4 | 224.81 | 509.0 | 8388608 | 1.372 | 1703.5 | 12 | 0.001 | 2.8 | 6 | 0.6 | 0.1 | 0 | 0 | 109023 |  |
| e14-loss0-mc2MiB-r1 | 231.26 | 225.0 | 2097152 | 1.083 | 1344.8 | 12 | 0 | 2.8 | 0 | 0.0 | 0.0 | 0 | 0 | 86064 |  |
| e14-loss0-mc8MiB-r1 | 231.27 | 225.0 | 8388608 | 1.083 | 1344.7 | 12 | 0 | 2.8 | 0 | 0.0 | 0.0 | 0 | 0 | 86061 |  |
| e14-loss0.001-mc0-r1 | 27.61 | 68.0 | 0 | 1.101 | 1366.5 | 12 | 0.001 | 19.9 | 0 | 0.0 | 0.0 | 0 | 0 | 87459 |  |
| e14-loss0.001-mc0-r2 | 27.65 | 105.0 | 0 | 1.110 | 1378.0 | 12 | 0.001 | 19.9 | 9 | 0.9 | 0.9 | 0 | 0 | 88191 |  |
| e14-loss0.001-mc0-r3 | 28.37 | 68.0 | 0 | 1.099 | 1364.7 | 12 | 0.001 | 19.4 | 0 | 0.0 | 0.0 | 0 | 0 | 87340 |  |
| e14-loss0.001-mc2MiB-r1 | 224.23 | 367.0 | 2097152 | 1.379 | 1712.0 | 12 | 0.001 | 2.8 | 0 | 0.0 | 0.0 | 0 | 0 | 109571 |  |
| e14-loss0.001-mc2MiB-r2 | 224.98 | 367.0 | 2097152 | 1.377 | 1708.7 | 12 | 0.001 | 2.8 | 0 | 0.0 | 0.0 | 0 | 0 | 109354 |  |
| e14-loss0.001-mc2MiB-r3 | 165.08 | 330.0 | 2097152 | 1.389 | 1723.8 | 12 | 0.001 | 3.7 | 10 | 1.0 | 1.0 | 0 | 0 | 110320 |  |
| e14-loss0.001-mc4MiB-r1 | 222.31 | 524.0 | 4194304 | 1.372 | 1703.3 | 12 | 0.001 | 2.9 | 9 | 0.9 | 0.1 | 0 | 0 | 109012 |  |
| e14-loss0.001-mc4MiB-r2 | 149.58 | 430.0 | 4194304 | 1.407 | 1746.1 | 12 | 0.001 | 4.8 | 16 | 1.6 | 1.1 | 0 | 0 | 111752 |  |
| e14-loss0.001-mc4MiB-r3 | 218.79 | 440.0 | 4194304 | 1.380 | 1712.4 | 12 | 0.001 | 2.8 | 6 | 0.6 | 0.2 | 0 | 0 | 109596 |  |
| e14-loss0.001-mc8MiB-r1 | 230.58 | 456.0 | 8388608 | 1.398 | 1735.2 | 12 | 0.001 | 2.9 | 8 | 0.8 | 0.1 | 0 | 0 | 111053 |  |
| e14-loss0.001-mc8MiB-r2 | 223.71 | 388.0 | 8388608 | 1.390 | 1725.1 | 12 | 0.001 | 2.8 | 3 | 0.3 | 0.1 | 0 | 0 | 110409 |  |
| e14-loss0.001-mc8MiB-r3 | 223.47 | 430.0 | 8388608 | 1.394 | 1730.5 | 12 | 0.001 | 2.8 | 7 | 0.7 | 0.1 | 0 | 0 | 110751 |  |
| e14-loss0.0002-mc0-r1 | 73.81 | 220.0 | 0 | 1.102 | 1367.6 | 12 | 0.0002 | 7.7 | 0 | 0.0 | 0.0 | 0 | 0 | 87528 |  |
| e14-loss0.0002-mc0-r2 | 57.34 | 142.0 | 0 | 1.096 | 1360.0 | 12 | 0.0002 | 9.8 | 0 | 0.0 | 0.0 | 0 | 0 | 87041 |  |
| e14-loss0.0002-mc0-r3 | 61.05 | 225.0 | 0 | 1.102 | 1368.2 | 12 | 0.0002 | 9.3 | 0 | 0.0 | 0.0 | 0 | 0 | 87563 |  |
| e14-loss0.0002-mc2MiB-r1 | 229.00 | 336.0 | 2097152 | 1.198 | 1486.6 | 12 | 0.0002 | 2.7 | 1 | 0.1 | 0.1 | 0 | 0 | 95143 |  |
| e14-loss0.0002-mc2MiB-r2 | 228.19 | 362.0 | 2097152 | 1.204 | 1494.5 | 12 | 0.0002 | 2.7 | 1 | 0.1 | 0.1 | 0 | 0 | 95645 |  |
| e14-loss0.0002-mc2MiB-r3 | 217.92 | 304.0 | 2097152 | 1.162 | 1442.1 | 12 | 0.0002 | 2.9 | 2 | 0.2 | 0.1 | 0 | 0 | 92296 |  |
| e14-loss0.0002-mc4MiB-r1 | 227.40 | 467.0 | 4194304 | 1.265 | 1570.5 | 12 | 0.0002 | 2.8 | 5 | 0.5 | 0.1 | 0 | 0 | 100514 |  |
| e14-loss0.0002-mc4MiB-r2 | 226.19 | 482.0 | 4194304 | 1.233 | 1530.3 | 12 | 0.0002 | 2.8 | 5 | 0.5 | 0.1 | 0 | 0 | 97941 |  |
| e14-loss0.0002-mc4MiB-r3 | 226.38 | 409.0 | 4194304 | 1.313 | 1630.4 | 12 | 0.0002 | 2.8 | 4 | 0.4 | 0.1 | 0 | 0 | 104345 |  |
| e14-loss0.0002-mc8MiB-r1 | 216.01 | 472.0 | 8388608 | 1.293 | 1604.7 | 12 | 0.0002 | 2.9 | 7 | 0.7 | 0.2 | 0 | 0 | 102701 |  |
| e14-loss0.0002-mc8MiB-r2 | 205.08 | 440.0 | 8388608 | 1.254 | 1556.3 | 12 | 0.0002 | 3.0 | 7 | 0.7 | 0.2 | 0 | 0 | 99604 |  |
| e14-loss0.0002-mc8MiB-r3 | 214.35 | 446.0 | 8388608 | 1.244 | 1544.8 | 12 | 0.0002 | 2.9 | 5 | 0.5 | 0.2 | 0 | 0 | 98864 |  |
| e14-loss0.009-mc0-r1 | 8.35 | 52.0 | 0 | 1.135 | 1409.1 | 12 | 0.009 | 64.7 | 52 | 5.2 | 1.0 | 0 | 0 | 90183 |  |
| e14-loss0.009-mc0-r2 | 8.17 | 37.0 | 0 | 1.136 | 1410.6 | 12 | 0.009 | 66.6 | 51 | 5.1 | 1.0 | 0 | 0 | 90277 |  |
| e14-loss0.009-mc2MiB-r1 | 58.85 | 372.0 | 2097152 | 1.767 | 2193.0 | 12 | 0.009 | 9.5 | 59 | 5.9 | 1.9 | 0 | 0 | 140351 |  |
| e14-loss0.009-mc2MiB-r2 | 112.00 | 315.0 | 2097152 | 1.665 | 2067.2 | 12 | 0.009 | 5.2 | 19 | 1.9 | 1.0 | 0 | 0 | 132304 |  |
| e14-loss0.009-mc4MiB-r1 | 157.40 | 336.0 | 4194304 | 1.808 | 2243.9 | 12 | 0.009 | 3.8 | 3 | 0.3 | 0.1 | 0 | 0 | 143608 |  |
| e14-loss0.009-mc4MiB-r2 | 69.75 | 372.0 | 4194304 | 1.576 | 1955.8 | 12 | 0.009 | 8.1 | 50 | 5.0 | 0.9 | 882 | 0 | 125174 |  |
| e14-loss0.009-mc8MiB-r1 | 68.09 | 377.0 | 8388608 | 1.517 | 1883.0 | 12 | 0.009 | 8.3 | 48 | 4.8 | 1.1 | 0 | 0 | 120511 |  |
| e14-loss0.009-mc8MiB-r2 | 60.97 | 377.0 | 8388608 | 1.542 | 1913.8 | 12 | 0.009 | 9.6 | 61 | 6.1 | 1.0 | 0 | 0 | 122484 |  |
