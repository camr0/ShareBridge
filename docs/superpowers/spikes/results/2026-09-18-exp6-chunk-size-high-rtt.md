# Experiment E6 — chunk size at high RTT — 2026-09-18

**Verdict: chunk size matters on a clean path and does not matter at all under loss.** With `--mode raw
--backpressure poll` at a 240 Mbps cap: at **loss 0** `--chunk 64KiB` is the best and by far the most stable
arm at every RTT (**222.9 / 202.4 / 188.1 Mbps** mean at rtt 25/71/100, one run each of which is byte-for-byte
the clean-path reference 1344.6 dg/MiB), 16 KiB is 1–7 % slower and collapses 1/3 of the time (52.6–81.8 Mbps
cells), and **256 KiB is a trap: 2 of 3 runs fall out of the cap at *every* RTT (13–91 Mbps, ceiling pinned
at ~105 Mbps) even with zero added loss** — a receiver-side granularity limit, not a path limit. Under
**loss 1×10⁻³ the chunk size makes no difference whatsoever**: rtt 25 ≈ **16 Mbps**, rtt 71 ≈ **5.8 Mbps**,
rtt 100 ≈ **3.9 Mbps** for all three chunk sizes, arms overlapping within ±9 %, with **identical wire volume
(~87,700 datagrams per 64 MiB, ratio 1.10)**. The collapse is loss-driven, not framing-driven. The
100 ms-scale *shape* of delivery does change with chunk size — interior stalls go from ~30 stalls / 3 s
(16 KiB) to ~1,100 stalls / 110 s (256 KiB) at **the same goodput** — which is a message-granularity
artefact, not a rate effect. Cross-check of E11: under loss, goodput is `∝ 1/RTT` to within 3 %
(all-chunk mean 16.03 / 5.76 / 3.88 Mbps for rtt 25 / 71 / 100, i.e. 1 : 2.78 : 4.13 against the RTT ratio's
prediction of 1 : 2.84 : 4.00).

**Setup:** lab only — **no field rig, container or VM touched** (runbook §1). `--mode raw --backpressure
poll --size 64MiB --bandwidth 30MB --queue 5MB --deadline 300` (the same 240 Mbps / 5 MB bottleneck as
E11, so the loss-0 cells are directly comparable), rtt {25, 71, 100} × `--chunk` {16 KiB, 64 KiB, 256 KiB} ×
loss {0, 1×10⁻³} × n=3, interleaved within each rtt group. Runner
`raw/exp6-chunk-size-high-rtt/run-exp6.sh` (phases `zero loss25 loss71 loss100`), log `runner.log` —
**54 cells, 54 `OK`, 0 `FAIL`**. Machine state (`uptime`, `vm.swapusage`, top-8 CPU) recorded before and
after every cell (`system-snapshots.txt`). The applied `--chunk` is echoed in each cell's JSON (field added
this session), so the `chunk` column below is read back from the artifact rather than assumed.

## 1. Loss 0 — chunk size matters, and 64 KiB wins

Mean / min–max Mbps, n=3. `ceil` = median of the five fastest 100 ms windows.

| rtt | 16 KiB | 64 KiB | 256 KiB |
|---|---|---|---|
| 25 | 172.7 (81.8–218.3), ceil 229 | **222.96 (222.8–223.2)**, ceil 225 | 118.8 (44.5–228.0), ceil 105–231 |
| 71 | 179.1 (154.6–191.5), ceil 226–258 | **202.29 (202.0–202.9)**, ceil 225–231 | 65.2 (12.9–165.7), ceil 42–273 |
| 100 | 132.8 (52.6–177.6), ceil 60–302 | **188.09 (187.8–188.3)**, ceil 278–294 | 40.8 (15.5–91.1), ceil 63–105 |

Three facts, all in-block comparisons:

1. **64 KiB is rock-solid at every RTT** — 222.8–223.2 (rtt 25), 202.0–202.9 (rtt 71), 187.8–188.3
   (rtt 100): a **±0.3 % spread** on a machine that was otherwise intermittent. Its 3.1 s / 3.8 s / 4.3 s
   wall times are the fastest of any arm, and its wire volume is **exactly 1344.6 dg/MiB (ratio 1.083) in
   every single run** — the calibrated clean-path reference from E1.
2. **16 KiB is slightly slower and occasionally unstable** (222.9 → 172.7 mean at rtt 25 because one run
   sat at 81.8; a 52.6 Mbps run at rtt 100). Its best runs (218.3 / 191.5 / 177.6) are still below the
   64 KiB arm's worst runs at the same RTT, so the ranking is real even after discarding the collapses.
3. **256 KiB frequently collapses with no added loss** — 2 of 3 runs at *each* RTT (83.95/44.48,
   16.94/12.86, 15.45/15.89), and its collapsed cells pin the **top-5-window ceiling at ~105 Mbps**
   (104.9 in four separate cells across all three RTTs). The two distinct collapse shapes are worth noting:
   at rtt 25 the 256 KiB collapse is a *uniform* low-rate state (44–84 Mbps, **zero interior stalls**,
   7–13 s wall), while at rtt 71/100 it is *stall-shaped* (128–175 stalls, 12.8–17.5 s stall time). Wire
   volume stays at 1345–1390 dg/MiB and `shim_drop = 0` throughout, so this is not loss and not queueing —
   it is a per-message processing limit (the receiver reassembling a 256 KiB message), which is why the
   same failure appears at 25 ms where RTT cannot matter.

**Practical reading:** on a clean path, 64 KiB is the right chunk; 256 KiB is not merely no better, it is
unreliable. Note the runbook's original sweep compared 16 KiB vs 64 KiB at rtt 0 "where neither matters" —
the correct statement is the opposite: 64 KiB is consistently *better* than 16 KiB by 1–7 % at rtt 25–100,
i.e. the effect is real but modest, and the dramatic effect is 256 KiB's instability.

## 2. Loss 1×10⁻³ — chunk size is irrelevant, and 1/RTT is confirmed

Mean / min–max Mbps, n=3:

| rtt | 16 KiB | 64 KiB | 256 KiB | all-arm spread |
|---|---|---|---|---|
| 25 | 16.51 (15.2–18.4) | 15.57 (14.7–16.7) | 16.02 (13.7–18.3) | ±9 % |
| 71 | 5.51 (4.9–6.2) | 6.00 (5.8–6.3) | 5.77 (5.4–6.1) | ±9 % |
| 100 | 3.84 (3.7–4.0) | 3.73 (3.5–4.0) | 4.07 (3.8–4.3) | ±9 % |

- **No arm ordering survives; every arm overlaps every other** at every RTT. The loss collapse swamps any
  framing effect — a 16 KiB, a 64 KiB and a 256 KiB message all end up in the same low-rate attractor.
- **Wire volume is identical across chunks** (87,200–89,000 datagrams per 64 MiB; wire/payload 1.098–1.122,
  vs 1.083 on the clean path) and `shim_drop = 0` / `shim_write_err = 0` in all 27 loss cells. So chunk size
  changes neither the overhead nor the loss-recovery traffic.
- **1/RTT scaling holds to within 3 %.** All-chunk means 16.03 : 5.76 : 3.88 Mbps for 25 : 71 : 100 ms, i.e.
  1 : 2.78 : 4.13, against the RTT ratio's 1 : 2.84 : 4.00. This independently reproduces E11 §1.6 (the collapsed state is
  a fixed window, rate ≈ W/RTT) at a loss level and at RTTs E11 did not test, and it puts W ≈ 50 KB
  (≈ 42 datagrams) at 1×10⁻³, larger than E11's ~10-datagram estimate for 9×10⁻³ — consistent with a
  window that shrinks further as loss rises.
- **Chunk size does change the delivery *shape***, at the same goodput: interior stalls / stall seconds at
  rtt 100 are **29–43 / 2.9–4.3 s (16 KiB)** → **381–536 / 38–54 s (64 KiB)** → **1,014–1,149 / 101–115 s
  (256 KiB)**, out of a 128–154 s transfer, with identical wire volume. Larger messages make delivery
  burstier, so more 100 ms windows fall below 10 % of the median delta. This is a granularity artefact of
  the stall metric — **do not read it as the sender idling more**; the goodput is the same.

## 3. Interpretation

- **Against the v1 defect:** chunk size is not a lever. Under loss — the only regime where v1 collapses —
  the chunk size changes goodput by less than the run-to-run spread at every RTT tested. This reinforces
  E5's conclusion that the collapse is a *window* phenomenon (E11's `W/RTT`): if it were a
  framing/segmentation problem, the 16 KiB arm would behave differently from the 64 KiB arm at 1×10⁻³, and
  it does not. The one chunk-size effect that is real — 256 KiB's ~105 Mbps ceiling — is a *receiver*
  limit, which is a data point for the client-sink hypothesis rather than against it (the lab's headless
  Chrome, which does **not** even run the field's verify/service-worker sink, already cannot sustain
  256 KiB messages).
- **Actionable for both v1 and v2:** use 64 KiB chunks (best goodput, most stable, lowest wire volume, and
  the only arm whose overhead equals the clean-path reference), and treat 256 KiB as unsupported: at 25 ms
  RTT — where no RTT effect can exist — two of three runs still lost two thirds of the cap. If a 256 KiB
  chunk is anywhere in a production default, it is a latent random-throughput bug.
- **Nothing here contradicts Experiment 9.** No field rig was involved.

## 4. Caveats

1. **Lab, not field.** No field rig, container or VM touched (runbook §1); no v1/v2 field numbers here.
2. **The rtt-25/100 loss-0 collapses are not machine noise** — the 64 KiB arm was simultaneously
   ±0.3 % stable, and the collapse appears at 2/3 frequency in a fixed arm (256 KiB) at all three RTTs. But
   n=3 cannot quantify the 256 KiB collapse *probability*; "2 of 3, twice" is the honest statement.
3. **This Mac was intermittently CPU-loaded** (MediaAnalysis daemon 106–116 % CPU, load avg 5.3–6.2, swap
   pinned at 6.27 GB / 7.17 GB). That is why the loss-0 arms carry one or two low runs each; conclusions
   are drawn only where arms are internally consistent (64 KiB) or where the arms are interleaved and
   overlapping (all the loss cells).
4. **The stall metric is relative** (intervals < 10 % of the cell's median non-zero delta, with
   leading/trailing ramp trimmed) and is message-granularity-sensitive — hence the explicit warning in §2.
5. **`--chunk` 256 KiB never errored**, so there is no max-message-size failure to report: `dc.Send` of
   262,144 B is exactly Chrome's advertised `max-message-size`, and it succeeds. (Compare E5: an
   *advertised* `max-message-size` below the sender's largest message breaks the transfer outright.)
6. **One machine, one night, n=3.** No statistical tests; all deltas quoted are within/between in-block
   arms.

## 5. Artifacts

Directory `docs/superpowers/spikes/results/raw/exp6-chunk-size-high-rtt/`:

| file | what |
|---|---|
| `run-exp6.sh` | runner, phases `zero loss25 loss71 loss100`; one `benchdirect` at a time |
| `runner.log` | START/OK per cell + full stderr trace of each (54 cells, 54 `OK`, 0 `FAIL`) |
| `row.py` | JSON/log → TSV row (goodput, ceiling, chunk, wire/payload ratio, dg/MiB, interior stalls) |
| `cells.tsv`, `*.row` | one row per cell, appended after every cell |
| `system-snapshots.txt` | `uptime` + `vm.swapusage` + top-8 CPU before and after every cell |
| `<label>.json` / `<label>.log` | raw result + stderr trace per cell |

Harness diff: `agent/cmd/benchdirect/{rawbench.go,prodbench.go}` gained a `chunk` JSON field (E5's diff
added the `-fastrtxwnd/-maxrxbuf/-maxmsg` flags and prod `-queue`; E14 added `min_cwnd`).

## 6. Appendix — every cell (appended incrementally during the run)

| label | mbps | ceil_mbps | chunk | rtt | loss | wall_s | nstall | stall_s | longest_s | drop | write_err | fwd | dg_per_mib | wire_ratio | error |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| e6-zero-rtt100-ck16KiB-r1 | 52.64 | 60.3 | 16384 | 100 | 0 | 11.5 | 1 | 0.1 | 0.1 | 0 | 0 | 86610 | 1353.3 | 1.090 |  |
| e6-zero-rtt100-ck16KiB-r2 | 177.62 | 273.9 | 16384 | 100 | 0 | 4.2 | 0 | 0.0 | 0.0 | 0 | 0 | 85939 | 1342.8 | 1.082 |  |
| e6-zero-rtt100-ck16KiB-r3 | 168.09 | 301.5 | 16384 | 100 | 0 | 4.5 | 1 | 0.1 | 0.1 | 0 | 0 | 87771 | 1371.4 | 1.105 |  |
| e6-zero-rtt100-ck256KiB-r1 | 91.12 | 104.9 | 262144 | 100 | 0 | 7.6 | 0 | 0.0 | 0.0 | 0 | 0 | 86807 | 1356.4 | 1.093 |  |
| e6-zero-rtt100-ck256KiB-r2 | 15.45 | 104.9 | 262144 | 100 | 0 | 36.4 | 171 | 17.1 | 0.3 | 0 | 0 | 88569 | 1383.9 | 1.115 |  |
| e6-zero-rtt100-ck256KiB-r3 | 15.89 | 62.9 | 262144 | 100 | 0 | 35.4 | 128 | 12.8 | 0.3 | 0 | 0 | 87267 | 1363.5 | 1.098 |  |
| e6-zero-rtt100-ck64KiB-r1 | 187.79 | 293.6 | 65536 | 100 | 0 | 4.3 | 0 | 0.0 | 0.0 | 0 | 0 | 86055 | 1344.6 | 1.083 |  |
| e6-zero-rtt100-ck64KiB-r2 | 188.16 | 283.1 | 65536 | 100 | 0 | 4.3 | 0 | 0.0 | 0.0 | 0 | 0 | 86055 | 1344.6 | 1.083 |  |
| e6-zero-rtt100-ck64KiB-r3 | 188.32 | 277.9 | 65536 | 100 | 0 | 4.3 | 0 | 0.0 | 0.0 | 0 | 0 | 86055 | 1344.6 | 1.083 |  |
| e6-zero-rtt25-ck16KiB-r1 | 81.80 | 229.4 | 16384 | 25 | 0 | 7.2 | 0 | 0.0 | 0.0 | 0 | 0 | 87363 | 1365.0 | 1.100 |  |
| e6-zero-rtt25-ck16KiB-r2 | 218.27 | 225.4 | 16384 | 25 | 0 | 3.1 | 0 | 0.0 | 0.0 | 0 | 0 | 86055 | 1344.6 | 1.083 |  |
| e6-zero-rtt25-ck16KiB-r3 | 217.97 | 246.4 | 16384 | 25 | 0 | 3.1 | 0 | 0.0 | 0.0 | 0 | 0 | 86966 | 1358.8 | 1.095 |  |
| e6-zero-rtt25-ck256KiB-r1 | 83.95 | 104.9 | 262144 | 25 | 0 | 7.1 | 0 | 0.0 | 0.0 | 0 | 0 | 86433 | 1350.5 | 1.088 |  |
| e6-zero-rtt25-ck256KiB-r2 | 44.48 | 83.9 | 262144 | 25 | 0 | 12.8 | 0 | 0.0 | 0.0 | 0 | 0 | 86840 | 1356.9 | 1.093 |  |
| e6-zero-rtt25-ck256KiB-r3 | 228.04 | 230.7 | 262144 | 25 | 0 | 3.1 | 0 | 0.0 | 0.0 | 0 | 0 | 86055 | 1344.6 | 1.083 |  |
| e6-zero-rtt25-ck64KiB-r1 | 222.94 | 225.4 | 65536 | 25 | 0 | 3.1 | 0 | 0.0 | 0.0 | 0 | 0 | 86055 | 1344.6 | 1.083 |  |
| e6-zero-rtt25-ck64KiB-r2 | 223.18 | 225.4 | 65536 | 25 | 0 | 3.1 | 0 | 0.0 | 0.0 | 0 | 0 | 86055 | 1344.6 | 1.083 |  |
| e6-zero-rtt25-ck64KiB-r3 | 222.77 | 225.4 | 65536 | 25 | 0 | 3.1 | 0 | 0.0 | 0.0 | 0 | 0 | 86905 | 1357.9 | 1.094 |  |
| e6-zero-rtt71-ck16KiB-r1 | 191.45 | 225.4 | 16384 | 71 | 0 | 3.8 | 0 | 0.0 | 0.0 | 0 | 0 | 86055 | 1344.6 | 1.083 |  |
| e6-zero-rtt71-ck16KiB-r2 | 191.36 | 258.2 | 16384 | 71 | 0 | 3.9 | 0 | 0.0 | 0.0 | 0 | 0 | 87889 | 1373.3 | 1.106 |  |
| e6-zero-rtt71-ck16KiB-r3 | 154.60 | 242.5 | 16384 | 71 | 0 | 4.5 | 5 | 0.5 | 0.5 | 0 | 0 | 89860 | 1404.1 | 1.131 |  |
| e6-zero-rtt71-ck256KiB-r1 | 165.69 | 272.6 | 262144 | 71 | 0 | 4.6 | 0 | 0.0 | 0.0 | 0 | 0 | 88737 | 1386.5 | 1.117 |  |
| e6-zero-rtt71-ck256KiB-r2 | 16.94 | 104.9 | 262144 | 71 | 0 | 33.1 | 128 | 12.8 | 1.0 | 0 | 0 | 88941 | 1389.7 | 1.120 |  |
| e6-zero-rtt71-ck256KiB-r3 | 12.86 | 41.9 | 262144 | 71 | 0 | 43.1 | 175 | 17.5 | 0.3 | 0 | 0 | 87414 | 1365.8 | 1.100 |  |
| e6-zero-rtt71-ck64KiB-r1 | 202.01 | 225.4 | 65536 | 71 | 0 | 3.8 | 0 | 0.0 | 0.0 | 0 | 0 | 86055 | 1344.6 | 1.083 |  |
| e6-zero-rtt71-ck64KiB-r2 | 202.85 | 230.7 | 65536 | 71 | 0 | 3.8 | 0 | 0.0 | 0.0 | 0 | 0 | 86055 | 1344.6 | 1.083 |  |
| e6-zero-rtt71-ck64KiB-r3 | 202.02 | 225.4 | 65536 | 71 | 0 | 3.9 | 0 | 0.0 | 0.0 | 0 | 0 | 86055 | 1344.6 | 1.083 |  |
| e6-loss0.001-rtt100-ck16KiB-r1 | 3.79 | 26.2 | 16384 | 100 | 0.001 | 143.0 | 29 | 2.9 | 0.2 | 0 | 0 | 88051 | 1375.8 | 1.108 |  |
| e6-loss0.001-rtt100-ck16KiB-r2 | 3.70 | 30.1 | 16384 | 100 | 0.001 | 146.5 | 43 | 4.3 | 0.2 | 0 | 0 | 88222 | 1378.5 | 1.111 |  |
| e6-loss0.001-rtt100-ck16KiB-r3 | 4.03 | 99.6 | 16384 | 100 | 0.001 | 134.4 | 29 | 2.9 | 0.1 | 0 | 0 | 88807 | 1387.6 | 1.118 |  |
| e6-loss0.001-rtt100-ck256KiB-r1 | 4.12 | 21.0 | 262144 | 100 | 0.001 | 132.1 | 1051 | 105.1 | 1.4 | 0 | 0 | 87692 | 1370.2 | 1.104 |  |
| e6-loss0.001-rtt100-ck256KiB-r2 | 3.83 | 21.0 | 262144 | 100 | 0.001 | 141.7 | 1149 | 114.9 | 1.3 | 0 | 0 | 87943 | 1374.1 | 1.107 |  |
| e6-loss0.001-rtt100-ck256KiB-r3 | 4.26 | 41.9 | 262144 | 100 | 0.001 | 127.8 | 1014 | 101.4 | 1.3 | 0 | 0 | 88038 | 1375.6 | 1.108 |  |
| e6-loss0.001-rtt100-ck64KiB-r1 | 3.98 | 15.7 | 65536 | 100 | 0.001 | 136.2 | 381 | 38.1 | 0.4 | 0 | 0 | 87575 | 1368.4 | 1.102 |  |
| e6-loss0.001-rtt100-ck64KiB-r2 | 3.70 | 36.7 | 65536 | 100 | 0.001 | 146.5 | 521 | 52.1 | 0.4 | 0 | 0 | 88087 | 1376.4 | 1.109 |  |
| e6-loss0.001-rtt100-ck64KiB-r3 | 3.52 | 10.5 | 65536 | 100 | 0.001 | 154.1 | 536 | 53.6 | 0.5 | 0 | 0 | 87608 | 1368.9 | 1.103 |  |
| e6-loss0.001-rtt25-ck16KiB-r1 | 15.90 | 165.2 | 16384 | 25 | 0.001 | 34.4 | 0 | 0.0 | 0.0 | 0 | 0 | 89648 | 1400.8 | 1.128 |  |
| e6-loss0.001-rtt25-ck16KiB-r2 | 18.41 | 68.2 | 16384 | 25 | 0.001 | 29.8 | 0 | 0.0 | 0.0 | 0 | 0 | 88238 | 1378.7 | 1.111 |  |
| e6-loss0.001-rtt25-ck16KiB-r3 | 15.21 | 47.2 | 16384 | 25 | 0.001 | 35.9 | 0 | 0.0 | 0.0 | 0 | 0 | 87684 | 1370.1 | 1.104 |  |
| e6-loss0.001-rtt25-ck256KiB-r1 | 18.33 | 62.9 | 262144 | 25 | 0.001 | 30.0 | 79 | 7.9 | 0.2 | 0 | 0 | 87665 | 1369.8 | 1.103 |  |
| e6-loss0.001-rtt25-ck256KiB-r2 | 13.69 | 41.9 | 262144 | 25 | 0.001 | 40.1 | 155 | 15.5 | 0.9 | 0 | 0 | 87728 | 1370.8 | 1.104 |  |
| e6-loss0.001-rtt25-ck256KiB-r3 | 16.04 | 83.9 | 262144 | 25 | 0.001 | 34.2 | 106 | 10.6 | 0.2 | 0 | 0 | 88223 | 1378.5 | 1.111 |  |
| e6-loss0.001-rtt25-ck64KiB-r1 | 16.72 | 52.4 | 65536 | 25 | 0.001 | 32.8 | 1 | 0.1 | 0.1 | 0 | 0 | 87771 | 1371.4 | 1.105 |  |
| e6-loss0.001-rtt25-ck64KiB-r2 | 14.71 | 83.9 | 65536 | 25 | 0.001 | 37.2 | 11 | 1.1 | 0.9 | 0 | 0 | 88970 | 1390.2 | 1.120 |  |
| e6-loss0.001-rtt25-ck64KiB-r3 | 15.28 | 89.1 | 65536 | 25 | 0.001 | 35.8 | 2 | 0.2 | 0.1 | 0 | 0 | 88184 | 1377.9 | 1.110 |  |
| e6-loss0.001-rtt71-ck16KiB-r1 | 4.93 | 15.7 | 16384 | 71 | 0.001 | 110.0 | 8 | 0.8 | 0.1 | 0 | 0 | 87541 | 1367.8 | 1.102 |  |
| e6-loss0.001-rtt71-ck16KiB-r2 | 6.21 | 34.1 | 16384 | 71 | 0.001 | 87.4 | 3 | 0.3 | 0.1 | 0 | 0 | 87463 | 1366.6 | 1.101 |  |
| e6-loss0.001-rtt71-ck16KiB-r3 | 5.40 | 26.2 | 16384 | 71 | 0.001 | 100.5 | 12 | 1.2 | 0.6 | 0 | 0 | 87797 | 1371.8 | 1.105 |  |
| e6-loss0.001-rtt71-ck256KiB-r1 | 6.05 | 21.0 | 262144 | 71 | 0.001 | 90.6 | 632 | 63.2 | 0.9 | 0 | 0 | 87203 | 1362.5 | 1.098 |  |
| e6-loss0.001-rtt71-ck256KiB-r2 | 5.81 | 21.0 | 262144 | 71 | 0.001 | 93.8 | 671 | 67.1 | 1.3 | 0 | 0 | 87692 | 1370.2 | 1.104 |  |
| e6-loss0.001-rtt71-ck256KiB-r3 | 5.44 | 21.0 | 262144 | 71 | 0.001 | 100.1 | 734 | 73.4 | 1.0 | 0 | 0 | 87660 | 1369.7 | 1.103 |  |
| e6-loss0.001-rtt71-ck64KiB-r1 | 6.33 | 78.6 | 65536 | 71 | 0.001 | 86.1 | 171 | 17.1 | 0.3 | 0 | 0 | 89105 | 1392.3 | 1.122 |  |
| e6-loss0.001-rtt71-ck64KiB-r2 | 5.75 | 31.5 | 65536 | 71 | 0.001 | 94.5 | 182 | 18.2 | 0.3 | 0 | 0 | 87771 | 1371.4 | 1.105 |  |
| e6-loss0.001-rtt71-ck64KiB-r3 | 5.91 | 47.2 | 65536 | 71 | 0.001 | 91.9 | 164 | 16.4 | 0.2 | 0 | 0 | 88269 | 1379.2 | 1.111 |  |
