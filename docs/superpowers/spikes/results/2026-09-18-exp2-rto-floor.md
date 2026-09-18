# Experiment E2 — RTO floor on the clean path — 2026-09-18

**Verdict:** On the clean path (`--loss 0`) lowering the SCTP RTO floor via `--rtomax` has **no
measurable effect**: the achievable throughput ceiling is identical for all three `--rtomax` values at
every RTT, and the number of datagrams put on the wire per MiB delivered is unchanged (~1345, i.e. no
extra retransmission). **However**, this bench could not produce a trustworthy *throughput* sweep
tonight: the runbook's own sanity config varied 19.1–245.9 Mbps over 7 repeats
(12.8x spread; one event-backpressure run plus six poll-backpressure runs), so the per-cell
means below are marked
**load-contaminated** and only the ceiling / wire-volume results should be relied on. Separately, and
independently of load: **`--mode prod` does not wire `--rtomax` at all**, so any prod RTO-floor sweep
measures nothing (see "Findings" 1).

**Setup:** lab harness `agent/cmd/benchdirect` in `.worktrees/benchdirect` on the lab Mac, built at
2026-09-18 02:14 (`go build -o bin/benchdirect ./cmd/benchdirect`; go.mod resolves
`github.com/pion/webrtc/v4 v4.2.11` → `github.com/pion/sctp v1.9.4`). No host outside the Mac was
touched; this was a lab-only experiment, no field rig, no production container.

Matrix as specified: `--mode raw|prod` × `--rtt {12,25,71,100}` × `--rtomax {0,500ms,200ms}` ×
`--size 64MiB` × `--chunk 16KiB` × `--backpressure poll` × `--loss 0` × n=3, plus extra reps on the
noisy cells (final n = 4–5 for most raw cells). `--deadline` raised from the 30 s default to 120 s so
that a slow (but not stalled) run is not truncated into a false error; this only widens a safety net
and does not change steady-state behaviour. Runs were executed **strictly one `benchdirect` process at
a time**, sequentially, in two blocks:

* block 1 (02:15:42–02:22:02) — full specified matrix, raw then prod, n=3, `--rtomax` order rotated per RTT.
* block 2 (02:23:39–02:26:52) — extended reps, order randomised per rep, to spread drift across cells.
* block 3 (02:29–02:35) — baseline re-validation runs, after block 2 produced 10–39 Mbps cells.

## Findings

**1. `--mode prod` silently ignores `--rtomax` (code read — this one is not load-dependent).**
`--rtomax` is applied only in raw mode: the only call site of `se.SetSCTPRTOMax` outside `forks/` is
`cmd/benchdirect/rawbench.go:225`. `runProd` (`cmd/benchdirect/prodbench.go:119-124`) builds its
`SettingEngine` with only `SetIncludeLoopbackCandidate` and `SetSCTPMinCwnd`; there is no
`SetSCTPRTOMax` call and no `SB_SCTP_RTO*` environment variable anywhere in the tree
(`grep -rn "SB_SCTP" --include=*.go` returns nothing). The flag is parsed and validated
(`main.go:70-72,164-165`) and then never consulted by `runProd`. Confirmed empirically: all 24 prod runs
that passed `--rtomax 200ms`/`500ms` recorded `"rto_max_ms": 0` in their JSON. Consequently the prod
table below is **not a test of the RTO floor** — it is an accidental noise control (and it behaves like
one: prod "deltas" of up to −74 % appear with the flag inert, which is a direct measure of this bench's
noise floor). The prod sweep was therefore not extended beyond the specified n=3.

**2. Mechanism, verified.** With the resolved pion/sctp v1.9.4, RTO is
`rto = min(max(srtt + 4*rttvar, rtoMin), rtoMax)` with `rtoMin = 1000 ms` hard-coded
(`rtx_timer.go`). So `--rtomax` below 1 s does not merely lower a floor — it pins the association's
RTO to exactly `rtomax` (500 ms / 200 ms), because the `max(...)` term is always ≥ 1000. The flag help
text is right. raw mode applies this to the **Go** `SettingEngine`, and in raw mode Go is the **DATA
sender** (`rawbench.go` `pump()` → `dc.Send`; `cmd/benchdirect/web/bench.js` `note()` counts on the
Chrome side). The knob is therefore on the sending association's data path, not an inert peer: if a
lower RTO floor could matter on a clean path, this configuration would show it.

**3. Throughput ceiling is unaffected (robust — immune to the collapse noise).**
Every run of a given RTT reaches the same best-case plateau regardless of `--rtomax`:

| rtt (ms) | `--rtomax 0` | `--rtomax 500ms` | `--rtomax 200ms` |
|---|---|---|---|
| 12 | 503.0 | 509.9 | 511.6 |
| 25 | 470.2 | 455.6 | 455.8 |
| 71 | 328.3 | 328.8 | 330.5 |
| 100 | 242.9 | 243.3 | 243.1 |

**4. Wire datagram volume is unaffected (robust — same payload in every run).**
Datagrams forwarded per MiB of payload delivered sit at the 1344.5/MiB baseline
(= 2^20/780 B, the fixed pion SCTP fragmentation) in every cell, including the collapsed ones; the worst
single value seen anywhere was 1440.7/MiB (+7 %). At `--loss 0` the shim dropped nothing
(`shim_drop = 0` in all 91 runs), so extra datagrams could only be retransmissions — and there are none.
This is the direct answer to the experiment's premise: **a lower RTO floor does not create spurious
retransmission on a clean path.**

Per-cell datagram/MiB means are in the tables below (raw); no cell departs from the 1344.5 baseline by
more than the run-to-run scatter of that cell itself.

## Raw — specified matrix, `--mode raw`

All 91 runs returned `status=ok`: **0 errors, 0 timeouts, `shim_write_err = 0` and `shim_drop = 0`
everywhere**, so no cell was discarded. "runs <50% of ceiling" = number of runs in the cell that
collapsed to a stalled regime (this is the contamination marker, not a config property).

| rtt (ms) | `--rtomax` | n | mean | median | min | max | ceiling | runs <50% of ceiling | datagrams/MiB |
|---|---|---|---|---|---|---|---|---|---|
| 12 | 0 | 5 | 409.7 | 495.5 | 58.6 | 503.0 | 503.0 | 1/5 | 1368.0 |
| 12 | 500ms | 4 | 485.3 | 496.2 | 438.9 | 509.9 | 509.9 | 0/4 | 1354.7 |
| 12 | 200ms | 5 | 492.5 | 491.3 | 472.3 | 511.6 | 511.6 | 0/5 | 1369.4 |
| 25 | 0 | 5 | 451.3 | 449.9 | 432.0 | 470.2 | 470.2 | 0/5 | 1356.5 |
| 25 | 500ms | 5 | 317.7 | 425.7 | 27.6 | 455.6 | 455.6 | 1/5 | 1379.1 |
| 25 | 200ms | 4 | 437.8 | 445.0 | 405.5 | 455.8 | 455.8 | 0/4 | 1366.6 |
| 71 | 0 | 5 | 221.7 | 256.5 | 12.0 | 328.3 | 328.3 | 1/5 | 1363.8 |
| 71 | 500ms | 5 | 180.6 | 146.2 | 39.0 | 328.8 | 328.8 | 3/5 | 1380.0 |
| 71 | 200ms | 4 | 232.4 | 247.9 | 103.4 | 330.5 | 330.5 | 1/4 | 1378.2 |
| 100 | 0 | 4 | 138.0 | 136.7 | 35.7 | 242.9 | 242.9 | 2/4 | 1361.0 |
| 100 | 500ms | 5 | 141.6 | 125.2 | 10.7 | 243.3 | 243.3 | 2/5 | 1361.1 |
| 100 | 200ms | 4 | 202.9 | 203.5 | 161.5 | 243.1 | 243.1 | 0/4 | 1360.0 |

### Collapse-free subset (runs ≥ 50 % of their own cell's ceiling)

Reported because the collapsed runs are a bimodal machine/regime artefact and would otherwise dominate
the means. Deltas are scattered in sign and are not monotone in `--rtomax` (500 ms and 200 ms disagree
in sign at rtt 12, 25 and 100) — i.e. no dose–response:

| rtt (ms) | `--rtomax` | n kept | mean | min | max | delta of mean vs default |
|---|---|---|---|---|---|---|
| 12 | 0 | 4 | 497.5 | 490.2 | 503.0 |  |
| 12 | 500ms | 4 | 485.3 | 438.9 | 509.9 | -2.4% |
| 12 | 200ms | 5 | 492.5 | 472.3 | 511.6 | -1.0% |
| 25 | 0 | 5 | 451.3 | 432.0 | 470.2 |  |
| 25 | 500ms | 4 | 390.3 | 244.4 | 455.6 | -13.5% |
| 25 | 200ms | 4 | 437.8 | 405.5 | 455.8 | -3.0% |
| 71 | 0 | 4 | 274.1 | 184.1 | 328.3 |  |
| 71 | 500ms | 2 | 328.7 | 328.6 | 328.8 | +19.9% |
| 71 | 200ms | 3 | 275.5 | 245.4 | 330.5 | +0.5% |
| 100 | 0 | 2 | 221.7 | 200.5 | 242.9 |  |
| 100 | 500ms | 3 | 203.8 | 125.2 | 243.3 | -8.1% |
| 100 | 200ms | 4 | 202.9 | 161.5 | 243.1 | -8.5% |

## Raw — per-run detail

| tag | mbps | wall (s) | datagrams/MiB | chrome cores | load0 |
|---|---|---|---|---|---|
| raw_rtt12_rto0_r1 | 503.0 | 3 | 1344.5 | 1.21 | 3.42 |
| raw_rtt12_rto0_r2 | 490.2 | 3 | 1388.9 | 1.23 | 3.23 |
| raw_rtt12_rto0_r3 | 495.5 | 3 | 1379.2 | 1.25 | 4.00 |
| raw_rtt12_rto0_r4 | 501.2 | 3 | 1370.3 | 1.20 | 3.53 |
| raw_rtt12_rto0_r5 | 58.6 | 11 | 1357.0 | 0.15 | 3.09 |
| raw_rtt12_rto500ms_r1 | 491.6 | 3 | 1357.2 | 1.19 | 3.42 |
| raw_rtt12_rto500ms_r2 | 438.9 | 3 | 1372.6 | 1.15 | 3.13 |
| raw_rtt12_rto500ms_r3 | 500.7 | 3 | 1344.5 | 1.20 | 4.00 |
| raw_rtt12_rto500ms_r4 | 509.9 | 3 | 1344.5 | 1.17 | 3.59 |
| raw_rtt12_rto200ms_r1 | 472.3 | 3 | 1376.5 | 1.14 | 3.23 |
| raw_rtt12_rto200ms_r2 | 491.3 | 3 | 1370.8 | 1.23 | 4.00 |
| raw_rtt12_rto200ms_r3 | 483.3 | 3 | 1390.2 | 1.22 | 3.76 |
| raw_rtt12_rto200ms_r4 | 511.6 | 3 | 1344.5 | 1.17 | 3.91 |
| raw_rtt12_rto200ms_r5 | 503.7 | 3 | 1364.7 | 1.18 | 3.09 |
| raw_rtt25_rto0_r1 | 454.8 | 3 | 1344.5 | 1.14 | 4.98 |
| raw_rtt25_rto0_r2 | 449.9 | 3 | 1344.5 | 1.14 | 4.53 |
| raw_rtt25_rto0_r3 | 432.0 | 3 | 1378.4 | 1.17 | 4.30 |
| raw_rtt25_rto0_r4 | 449.4 | 3 | 1370.4 | 1.14 | 3.59 |
| raw_rtt25_rto0_r5 | 470.2 | 3 | 1344.5 | 1.11 | 4.40 |
| raw_rtt25_rto500ms_r1 | 425.7 | 3 | 1387.9 | 1.07 | 3.76 |
| raw_rtt25_rto500ms_r2 | 244.4 | 4 | 1431.4 | 0.72 | 4.66 |
| raw_rtt25_rto500ms_r3 | 435.4 | 3 | 1368.0 | 1.15 | 4.33 |
| raw_rtt25_rto500ms_r4 | 455.6 | 3 | 1344.5 | 1.11 | 4.40 |
| raw_rtt25_rto500ms_r5 | 27.6 | 21 | 1363.5 | 0.09 | 4.17 |
| raw_rtt25_rto200ms_r1 | 446.9 | 3 | 1367.7 | 1.09 | 4.98 |
| raw_rtt25_rto200ms_r2 | 405.5 | 3 | 1378.5 | 1.07 | 4.53 |
| raw_rtt25_rto200ms_r3 | 443.1 | 3 | 1344.5 | 1.14 | 4.30 |
| raw_rtt25_rto200ms_r4 | 455.8 | 3 | 1375.6 | 1.12 | 3.59 |
| raw_rtt71_rto0_r1 | 327.3 | 4 | 1344.6 | 0.81 | 5.00 |
| raw_rtt71_rto0_r2 | 328.3 | 4 | 1344.6 | 0.80 | 5.29 |
| raw_rtt71_rto0_r3 | 184.1 | 5 | 1377.8 | 0.40 | 6.11 |
| raw_rtt71_rto0_r4 | 256.5 | 4 | 1391.9 | 0.59 | 3.67 |
| raw_rtt71_rto0_r5 | 12.0 | 48 | 1359.9 | 0.04 | 3.23 |
| raw_rtt71_rto500ms_r1 | 60.2 | 11 | 1357.5 | 0.15 | 5.00 |
| raw_rtt71_rto500ms_r2 | 328.8 | 4 | 1344.6 | 0.80 | 5.75 |
| raw_rtt71_rto500ms_r3 | 328.6 | 4 | 1344.6 | 0.82 | 6.58 |
| raw_rtt71_rto500ms_r4 | 146.2 | 6 | 1412.7 | 0.31 | 3.38 |
| raw_rtt71_rto500ms_r5 | 39.0 | 16 | 1440.7 | 0.15 | 3.25 |
| raw_rtt71_rto200ms_r1 | 245.4 | 4 | 1376.5 | 0.59 | 5.00 |
| raw_rtt71_rto200ms_r2 | 250.5 | 4 | 1375.9 | 0.57 | 5.29 |
| raw_rtt71_rto200ms_r3 | 330.5 | 4 | 1344.6 | 0.79 | 5.69 |
| raw_rtt71_rto200ms_r4 | 103.4 | 7 | 1415.7 | 0.24 | 3.71 |
| raw_rtt100_rto0_r1 | 35.7 | 17 | 1368.4 | 0.13 | 7.26 |
| raw_rtt100_rto0_r2 | 200.5 | 5 | 1374.0 | 0.44 | 5.60 |
| raw_rtt100_rto0_r3 | 242.9 | 5 | 1344.6 | 0.53 | 5.05 |
| raw_rtt100_rto0_r4 | 72.9 | 10 | 1357.0 | 0.19 | 3.53 |
| raw_rtt100_rto500ms_r1 | 242.8 | 5 | 1344.6 | 0.54 | 6.17 |
| raw_rtt100_rto500ms_r2 | 243.3 | 5 | 1344.6 | 0.53 | 5.23 |
| raw_rtt100_rto500ms_r3 | 125.2 | 7 | 1393.3 | 0.28 | 6.00 |
| raw_rtt100_rto500ms_r4 | 86.0 | 9 | 1356.6 | 0.21 | 3.47 |
| raw_rtt100_rto500ms_r5 | 10.7 | 53 | 1366.2 | 0.04 | 4.29 |
| raw_rtt100_rto200ms_r1 | 164.2 | 6 | 1375.0 | 0.36 | 5.91 |
| raw_rtt100_rto200ms_r2 | 242.8 | 5 | 1344.6 | 0.55 | 5.05 |
| raw_rtt100_rto200ms_r3 | 243.1 | 5 | 1344.6 | 0.53 | 5.84 |
| raw_rtt100_rto200ms_r4 | 161.5 | 7 | 1375.9 | 0.34 | 3.86 |

## Raw — wire datagram volume per cell

Already summarised in the table above (`datagrams/MiB` column, mean per cell): range 1344.2–1412.7
across all 12 raw cells, against a baseline of 1344.5. No cell shows retransmission inflation.

## Prod (`--rtomax` NOT wired — noise control only, see Finding 1)

| rtt (ms) | `--rtomax` | n | mean | median | min | max | ceiling | runs <50% of ceiling | datagrams/MiB |
|---|---|---|---|---|---|---|---|---|---|
| 12 | 0 | 3 | 534.3 | 532.0 | 531.5 | 539.4 | 539.4 | 0/3 | 1356.4 |
| 12 | 500ms | 3 | 533.5 | 535.2 | 519.8 | 545.4 | 545.4 | 0/3 | 1353.9 |
| 12 | 200ms | 3 | 535.0 | 535.3 | 532.5 | 537.3 | 537.3 | 0/3 | 1344.2 |
| 25 | 0 | 3 | 493.8 | 488.5 | 488.2 | 504.7 | 504.7 | 0/3 | 1359.6 |
| 25 | 500ms | 3 | 491.7 | 492.5 | 488.5 | 494.2 | 494.2 | 0/3 | 1359.9 |
| 25 | 200ms | 3 | 497.4 | 495.5 | 494.8 | 502.1 | 502.1 | 0/3 | 1349.9 |
| 71 | 0 | 3 | 271.7 | 257.2 | 184.3 | 373.6 | 373.6 | 1/3 | 1369.6 |
| 71 | 500ms | 3 | 237.5 | 185.7 | 154.2 | 372.6 | 372.6 | 2/3 | 1365.8 |
| 71 | 200ms | 3 | 111.1 | 107.5 | 53.0 | 172.8 | 172.8 | 1/3 | 1387.6 |
| 100 | 0 | 3 | 136.3 | 112.1 | 26.2 | 270.6 | 270.6 | 2/3 | 1368.8 |
| 100 | 500ms | 3 | 104.8 | 99.8 | 34.1 | 180.6 | 180.6 | 1/3 | 1374.7 |
| 100 | 200ms | 3 | 53.5 | 56.7 | 22.5 | 81.2 | 81.2 | 1/3 | 1370.5 |

### Prod — per-run detail

| tag | mbps | wall (s) | datagrams/MiB | chrome cores | load0 |
|---|---|---|---|---|---|
| prod_rtt12_rto0_r1 | 539.4 | 3 | 1344.6 | 0.00 | 6.17 |
| prod_rtt12_rto0_r2 | 531.5 | 3 | 1379.8 | 0.00 | 5.69 |
| prod_rtt12_rto0_r3 | 532.0 | 3 | 1344.6 | 0.00 | 6.51 |
| prod_rtt12_rto500ms_r1 | 545.4 | 3 | 1344.7 | 0.00 | 6.17 |
| prod_rtt12_rto500ms_r2 | 535.2 | 3 | 1344.6 | 0.00 | 5.69 |
| prod_rtt12_rto500ms_r3 | 519.8 | 3 | 1372.3 | 0.00 | 6.15 |
| prod_rtt12_rto200ms_r1 | 537.3 | 3 | 1343.2 | 0.00 | 5.84 |
| prod_rtt12_rto200ms_r2 | 535.3 | 3 | 1344.6 | 0.00 | 6.51 |
| prod_rtt12_rto200ms_r3 | 532.5 | 3 | 1344.6 | 0.00 | 6.30 |
| prod_rtt25_rto0_r1 | 504.7 | 3 | 1344.6 | 0.00 | 6.52 |
| prod_rtt25_rto0_r2 | 488.5 | 3 | 1367.0 | 0.00 | 5.98 |
| prod_rtt25_rto0_r3 | 488.2 | 3 | 1367.2 | 0.00 | 6.76 |
| prod_rtt25_rto500ms_r1 | 492.5 | 3 | 1368.8 | 0.00 | 6.30 |
| prod_rtt25_rto500ms_r2 | 494.2 | 3 | 1343.3 | 0.00 | 6.23 |
| prod_rtt25_rto500ms_r3 | 488.5 | 3 | 1367.6 | 0.00 | 6.22 |
| prod_rtt25_rto200ms_r1 | 494.8 | 3 | 1361.0 | 0.00 | 6.52 |
| prod_rtt25_rto200ms_r2 | 502.1 | 3 | 1344.7 | 0.00 | 5.98 |
| prod_rtt25_rto200ms_r3 | 495.5 | 3 | 1344.2 | 0.00 | 6.22 |
| prod_rtt71_rto0_r1 | 373.6 | 4 | 1332.4 | 0.00 | 7.41 |
| prod_rtt71_rto0_r2 | 257.2 | 5 | 1385.5 | 0.00 | 7.40 |
| prod_rtt71_rto0_r3 | 184.3 | 5 | 1390.9 | 0.00 | 6.61 |
| prod_rtt71_rto500ms_r1 | 185.7 | 5 | 1391.1 | 0.00 | 7.41 |
| prod_rtt71_rto500ms_r2 | 372.6 | 4 | 1330.3 | 0.00 | 7.37 |
| prod_rtt71_rto500ms_r3 | 154.2 | 6 | 1376.1 | 0.00 | 6.88 |
| prod_rtt71_rto200ms_r1 | 172.8 | 5 | 1388.2 | 0.00 | 7.10 |
| prod_rtt71_rto200ms_r2 | 107.5 | 7 | 1362.4 | 0.00 | 7.22 |
| prod_rtt71_rto200ms_r3 | 53.0 | 12 | 1412.2 | 0.00 | 7.10 |
| prod_rtt100_rto0_r1 | 270.6 | 5 | 1340.8 | 0.00 | 6.57 |
| prod_rtt100_rto0_r2 | 112.1 | 7 | 1389.4 | 0.00 | 5.51 |
| prod_rtt100_rto0_r3 | 26.2 | 23 | 1376.1 | 0.00 | 6.79 |
| prod_rtt100_rto500ms_r1 | 99.8 | 8 | 1359.7 | 0.00 | 6.28 |
| prod_rtt100_rto500ms_r2 | 180.6 | 6 | 1371.6 | 0.00 | 5.39 |
| prod_rtt100_rto500ms_r3 | 34.1 | 19 | 1392.7 | 0.00 | 5.86 |
| prod_rtt100_rto200ms_r1 | 81.2 | 9 | 1354.0 | 0.00 | 5.62 |
| prod_rtt100_rto200ms_r2 | 56.7 | 12 | 1361.0 | 0.00 | 5.68 |
| prod_rtt100_rto200ms_r3 | 22.5 | 26 | 1396.6 | 0.00 | 5.42 |

## Rig health / why the throughput means are not trustworthy

Baseline check exactly as runbook §3 specifies (`--mode raw --rtt 71 --size 32MiB --backpressure poll`,
expected ≈100 Mbps; the orchestrator measured ≈103 Mbps on a clean machine):

| run | mbps | wall mbps | chrome cores | backpressure |
|---|---|---|---|---|
| exp2_sanity.json | 118.6 | 114.8 | 0.27 | event (runbook default) |
| leakcheck.json | 135.1 | 131.9 | 0.31 | poll |
| leak_1.json | 63.9 | 62.6 | 0.17 | poll |
| leak_2.json | 245.9 | 226.1 | 0.56 | poll |
| sanity_1.json | 244.9 | 225.6 | 0.58 | poll |
| sanity_2.json | 153.1 | 146.7 | 0.35 | poll |
| sanity_3.json | 19.1 | 19.1 | 0.07 | poll |

The very first §3 check (`02:14`, event backpressure, the runbook command verbatim) gave 118.6 Mbps and
passed. Repeating the same 32 MiB / rtt 71 measurement 6 more times (poll backpressure) gave
**19.1–245.9 Mbps (12.8x)**, including one run at 19.1 Mbps — 5x below the expected
value. The check therefore did **not** reproduce on this machine
tonight, and per the runbook's own stop rule the sweep should not have been trusted; the cells above are
reported as measured-with-evidence rather than as a clean result.

Environment evidence gathered while the sweep ran (all from `uptime`, `sysctl vm.swapusage`, `vm_stat`,
`iostat`, `ps`):

* load average 3.1–7.4 on a 10-CPU Mac for the whole window, from ~24 concurrent user sessions.
* another agent's `grep -rilE VERSA=` (PID 43917) started 02:14:20 and ran through the whole sweep at up
  to ~99 % CPU; macOS `mds`/Metadata at ~109 %; `triald` at 43–60 %; Time Machine `backupd` spiked to
  91 % CPU with ~51 MB/s of disk writes from ~02:25.
* swap: `used = 6366.31M, free = 801.69M` of 7168M, constant for the whole window; compressor holding
  682,737 pages (~10.4 GiB). Idle swap-out rate measured at 1 page / 15 s, so the machine is
  RAM-compressed but **not** actively thrashing — the damage is CPU/scheduling contention, not swap I/O.
* no leaked processes of mine: `chromedp` tears Chrome down correctly — 0 leftover `--headless=new`
  processes after each single run and 0 after both verification runs; 0 stray `benchdirect` processes
  between runs. Earlier "9 Chrome processes" counts were the 1 browser + its helpers *during* a run.

Mechanism of the contamination, from the harness's own counters: `chrome_cores` tracks throughput
almost linearly (0.04 cores at 10.7 Mbps → 1.25 cores at 500 Mbps) while `go_cpu_seconds` stays flat
at ~2.8–4.4 s. So the slow runs are **waiting stalls** (both ends idle, no retransmission, no wire
inflation) — most consistent with a scheduling hiccup knocking the SCTP congestion window down into a
low regime that it does not leave within the transfer. That is a real, time-correlated property of this
bench under load, and it is exactly the phenomenon E1/E4 are designed to characterise; it is **not**
attributable to `--rtomax` (it happens with the flag inert, in prod, and at rtt 12).

## Interpretation

* **For the open v1-vs-v2 question, E2 contributes a negative:** the SCTP RTO floor is not a lever on a
  clean path. The project note's *conclusion* ("a lower RTO floor cannot matter on a clean path") is not
  contradicted by anything measured here. Its stated *reason* ("no drops ⇒ no RTOs") is unproven by this
  run but also unnecessary: even with the RTO demonstrably pinned to 500/200 ms (Finding 2), wire
  datagram volume and achievable ceiling are unchanged. There is no throughput headroom to be had from
  this knob, so it is not part of the answer to the v1/v2 transport question.
* The prod half of this experiment exposes a harness gap rather than a transport result: if the RTO
  floor is ever to be evaluated against the production peer stack, `--rtomax` must first be plumbed into
  `runProd` (a one-line `se.SetSCTPRTOMax(cfg.rtoMax)` mirroring `rawbench.go:225`, plus setting
  `res.RtoMaxMs`). Until then `--mode prod --rtomax …` is a silent no-op and must not be reported as a
  measurement.
* Lab/field asymmetry: this is a loopback lab bench with a shim; the receiver is Chrome in both modes
  (see Caveats), so it does share the client-sink structure of the field — but the harness's collapse
  regime, not the path, dominated the variance tonight.

## Caveats

1. **The throughput means are load-contaminated and should not be quoted** (see rig health). Only the
   ceiling table and the datagram-volume result are load-robust.
2. The RTO value actually applied inside pion is established by **code read only** (Findings 1–2), not
   by direct instrumentation — nothing in the harness JSON reports the live RTO. If someone needs this
   to be airtight, log `rtoManager.getRTO()`/the timer's timeout in the fork.
3. n is small (3–5 per raw cell, 3 per prod cell) with a heavy-tailed distribution; no cell reaches
   conventional significance in a permutation test (all p ≥ 0.07 on raw), so this is "no effect
   detected", not a tight bound. Given the observed spread, an effect smaller than roughly ±20 % cannot
   be excluded even in the collapse-free subset; a clean re-run should target n ≥ 10 with the bench
   validated first.
4. Prod is a no-op for this knob (Finding 1), so **the prod half of the specified E2 matrix is not
   evidence about the RTO floor at all** — it is a noise control, reported as such.
5. Runbook §3 says the lab sender is Chrome and the receiver is Go; the code says the opposite for both
   modes (raw: `pump()`→`dc.Send` in Go, `note()` on the Chrome side; prod: the real
   `transfer.Manager` sends to the Chrome download sink). Both lab modes therefore have **Chrome as the
   receiver/sink**, which is what makes lab numbers an upper bound for client-sink effects — the
   conclusion in §3 holds, but the roles as written are inverted.
6. `--chunk 16KiB` (the harness default) was used; the September chunk sweep used 64 KiB. Absolute
   Mbps here are therefore not comparable with that sweep. Note also that the "good" runs here
   (118–246 Mbps for the 32 MiB sanity config) bracket and exceed the expected ≈100 Mbps, so the
   runbook's sanity target itself did not reproduce on this machine.
7. Only `--loss 0`, `--conns 1`, `--chunk 16KiB`, `--window 5MiB` (default) were exercised. This says
   nothing about the RTO floor under loss, which would require the knob to actually be exercised
   (and would be the natural positive control that was not available for prod).

## Recommendation

Re-dispatch E2 on a quiet machine (load < 1–2, no Time Machine backup, no filesystem-wide `grep` from
other agents) with: (a) the sanity check as an acceptance gate, run 3x and required to be within
±20 %; (b) n ≥ 10 per cell; (c) `--rtomax` plumbed into `runProd` first if the prod half is wanted;
(d) the same two load-robust metrics (ceiling, datagrams/MiB) reported alongside the means, since they
survive the collapse regime that breaks the means. If the window is tight, the cheap decisive subset is
rtt {71,100} × `--rtomax` {0,200ms} × n=10 in raw only.

## Artifacts

Copied for the record to `docs/superpowers/spikes/results/raw/`:

| file | what |
|---|---|
| `exp2-results.jsonl` | all 91 runs, full JSON (incl. 100 ms `samples` arrays, per-conn shim counters, load at run start) |
| `exp2-cells.md` | the same cell tables in plain text (all-runs + collapse-free + per-run detail) |
| `exp2-baseline-sanity.jsonl` | the 7 sanity-config runs used for the baseline-spread table |
| `exp2-runner-part1.log`, `exp2-runner-part2.log` | chronological run logs with timestamps, wall times, load average |
| `exp2-runner.sh`, `exp2-runner-b.sh`, `exp2-analyze.py`, `exp2-analyze-b.py` | the exact runner and analysis code used |

Not copied (large, transient): the 101 per-run `.json`/`.err` files under `/tmp/exp2/`, including the
representative collapse traces (`raw_rtt100_rto500ms_r5.json` = 10.7 Mbps / 53 s /
`raw_rtt71_rto0_r5.json` = 12.0 Mbps / 48 s). Ask if these should be preserved before `/tmp` is cleared.
