# Experiment E34 — is the DataChannel receive path's cost per *message* or per *byte*? (N1) — 2026-10-02

**Verdict: N1 is refuted, and it was already answered before this experiment ran.** The receiver's marginal
cost per DataChannel message is real and measurable — **7.1 / 8.7 / 9.8 µs per message** of Chrome CPU at
uncapped / 64 Mbps / 240 Mbps, from an in-block least-squares fit over a 16×-downward message ladder
(1 KiB → 256 KiB) at constant delivered rate — but at the field's own message rate (48,258 × 16 KiB messages
for 754 MiB; **943.6 msg/s** at the E28 4-vCPU bare arm's 123.68 Mbps) that term is **0.007–0.009 of a core**,
i.e. **0.5–0.7 % of the 1.27 client cores** that arm spent. Cutting message count 16× (16 KiB → 256 KiB) at a
pinned rate moves receiver CPU per byte by **−6.2 % (uncapped), −0.1 % (64 Mbps), −3.0 % (240 Mbps)** — the
whole N1 lever, and it is at or below this bench's own in-block baseline spread (6.1–31.8 %). The lab's *bare*
Chrome receiver sustains **443 Mbps at 54,018 msg/s** (57× the field's message rate) and **526 Mbps at
3,812 msg/s** (4.3× the field's throughput) on the same 16 KiB framing the field uses; message dispatch cannot
be what holds the field at ~120–138 Mbps. **E23 §2 had already run this exact test at a pinned 80 Mbps and
found sender CPU-s/GB flat within 4 % and receiver cores flat at 0.26–0.30 across 16/64/256 KiB**; E6 could
not have answered it because E6 never measured receiver or sender CPU at all. This experiment adds the
*downward* ladder that turns the per-message term into a number, three pinned rates plus an uncapped ceiling,
and the rtt-71 axis — and reproduces the "already closed" answer with a tighter design.

**Setup:** lab only — no field rig, container or VM touched. `--mode raw --backpressure poll --size 64MiB
--deadline 120`, **Go/pion SENDS, headless Chrome RECEIVES** (same direction as the field), receiver =
`cmd/benchdirect/web/bench.js` bare byte counter (`note()` per `onmessage`), i.e. the **E28 "bare" arm**.
`--chunk` is the `dc.Send()` size, which is the DataChannel message size (SCTP re-fragments on the wire and
reassembles before delivery). Grid: `--chunk {1,4,16,64,256 KiB}` × `--rtt {0,71}` × `--bandwidth
{uncapped, 8MB=64 Mbps, 30MB=240 Mbps}` × **n=3** (16 KiB n=6 at rtt 0, interleaved as the in-block baseline
immediately before *and* after each sweep), plus the runbook sanity gate `--rtt 71 --mode raw --size 32MiB`
n=3. **85 cells, 85 `OK`, 0 `FAIL`, `shim_write_err = 0` in every cell, and 100 % of the payload delivered in
every cell** (`recv` = 67,108,864 / 33,554,432 exactly). `--queue` is the harness default (100 ms of the
bandwidth cap: **3 MB at 30 MB/s, 800 KB at 8 MB/s**) — E6 used an explicit `--queue 5MB`; that is the one
material config difference and it is flagged in Caveats. Runner
`raw/exp34-message-granularity/run-exp34.py`, per-cell rows appended to `cells.tsv` as each cell finished,
machine state (`uptime`, `vm.swapusage`, top-8 CPU) captured before and after every cell
(`system-snapshots.txt`).

## 0. The sanity gate failed, as the runbook says it will on this host

| label | mbps | ceil | chrome_cores |
|---|---|---|---|
| e34-sanity-rtt71-32MiB-16KiB-r1 | **240.75** | 467 | 0.524 |
| e34-sanity-rtt71-32MiB-16KiB-r2 | 9.74 | 36 | 0.048 |
| e34-sanity-rtt71-32MiB-16KiB-r3 | 9.05 | 15 | 0.041 |
| e34-sanity-rtt0-32MiB-16KiB-r1 (reference) | 535.91 | 510 | 1.070 |

Runbook §3 wants ≈100 Mbps; the arm is **bimodal, 240.7 / 9.7 / 9.1**, exactly E23's sanity shape
(10.72 / 13.72 there). This is the host, not the harness: `vm.swapusage` was pinned at **6.72 GB of 8.19 GB**
all session, `backupd` (Time Machine) and a concurrent Go compile were running, load 2.1–2.9. Nothing below
rests on an uncapped mean; the load-robust observables are **chrome_cores at a pinned rate**, **per-byte
CPU**, **wire datagram count**, `shim_drop`, and the uncapped **ceiling**.

## 1. Raw table — every cell

`ceil` = best sliding 5 × 100 ms plateau; `cs/GB` = Chrome CPU-s per GB of payload delivered; `msgs` =
DataChannel messages delivered; `fwd`/`drop` = shim datagrams forwarded / token-bucket dropped. Cells below
90 % of their rate cap are marked COLLAPSED and are **excluded from the fits but retained in the table**;
rtt-71 cells are collapsed by construction and are never used for a rate-capped comparison.

| label | rtt | cap | chunk | mbps | ceil | wall_s | cores | cs/GB | msg/s | msgs | fwd | drop | werr | note |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| e34-sanity-rtt71-32MiB-16KiB-r1 | 71 | 0 | 16KiB | 240.749 | 467.4 | 2.90 | 0.5237 | 22.650 | 1836.8 | 2048 | 43043 | 0 | 0 | sanity |
| e34-sanity-rtt71-32MiB-16KiB-r2 | 71 | 0 | 16KiB | 9.741 | 35.7 | 29.29 | 0.0477 | 39.637 | 74.3 | 2048 | 43951 | 0 | 0 | sanity |
| e34-sanity-rtt71-32MiB-16KiB-r3 | 71 | 0 | 16KiB | 9.052 | 14.7 | 31.34 | 0.0407 | 36.359 | 69.1 | 2048 | 43654 | 0 | 0 | sanity |
| e34-sanity-rtt0-32MiB-16KiB-r1 | 0 | 0 | 16KiB | 535.906 | 509.6 | 1.63 | 1.0696 | 22.054 | 4088.6 | 2048 | 43041 | 0 | 0 | sanity |
| e34-ceil-rtt0-16KiB-r1 | 0 | 0 | 16KiB | 409.950 | 454.6 | 3.04 | 0.9571 | 23.693 | 3127.7 | 4096 | 86569 | 0 | 0 |  |
| e34-ceil-rtt0-64KiB-r1 | 0 | 0 | 64KiB | 522.451 | 522.2 | 2.16 | 1.2033 | 21.458 | 996.5 | 1024 | 86051 | 0 | 0 |  |
| e34-ceil-rtt0-256KiB-r1 | 0 | 0 | 256KiB | 455.245 | 532.7 | 2.78 | 0.7880 | 21.607 | 217.1 | 256 | 87413 | 0 | 0 |  |
| e34-ceil-rtt0-16KiB-r2 | 0 | 0 | 16KiB | 517.715 | 520.6 | 2.19 | 1.2614 | 22.501 | 3949.9 | 4096 | 86051 | 0 | 0 |  |
| e34-ceil-rtt0-64KiB-r2 | 0 | 0 | 64KiB | 523.470 | 526.4 | 2.13 | 1.2122 | 21.458 | 998.4 | 1024 | 88150 | 0 | 0 |  |
| e34-ceil-rtt0-256KiB-r2 | 0 | 0 | 256KiB | 532.927 | 536.9 | 2.14 | 1.1858 | 21.011 | 254.1 | 256 | 86051 | 0 | 0 |  |
| e34-ceil-rtt0-16KiB-r3 | 0 | 0 | 16KiB | 527.482 | 556.0 | 2.14 | 1.2610 | 22.501 | 4024.4 | 4096 | 88125 | 0 | 0 |  |
| e34-ceil-rtt0-64KiB-r3 | 0 | 0 | 64KiB | 526.292 | 528.5 | 2.13 | 1.2269 | 21.607 | 1003.8 | 1024 | 86051 | 0 | 0 |  |
| e34-ceil-rtt0-256KiB-r3 | 0 | 0 | 256KiB | 532.927 | 536.9 | 2.14 | 1.1748 | 21.011 | 254.1 | 256 | 86581 | 0 | 0 |  |
| e34-ceil-rtt71-16KiB-r1 | 71 | 0 | 16KiB | 8.646 | 16.3 | 64.15 | 0.0373 | 34.720 | 66.0 | 4096 | 87253 | 0 | 0 |  |
| e34-ceil-rtt71-64KiB-r1 | 71 | 0 | 64KiB | 8.161 | 41.9 | 67.76 | 0.0376 | 37.104 | 15.6 | 1024 | 87753 | 0 | 0 |  |
| e34-ceil-rtt71-256KiB-r1 | 71 | 0 | 256KiB | 8.301 | 33.6 | 66.86 | 0.0368 | 35.763 | 4.0 | 256 | 87572 | 0 | 0 |  |
| e34-ceil-rtt71-16KiB-r2 | 71 | 0 | 16KiB | 7.884 | 55.6 | 69.90 | 0.0357 | 36.359 | 60.2 | 4096 | 89403 | 0 | 0 |  |
| e34-ceil-rtt71-64KiB-r2 | 71 | 0 | 64KiB | 7.312 | 32.5 | 75.44 | 0.0345 | 37.998 | 13.9 | 1024 | 90232 | 0 | 0 |  |
| e34-ceil-rtt71-256KiB-r2 | 71 | 0 | 256KiB | 7.604 | 50.3 | 72.74 | 0.0349 | 37.104 | 3.6 | 256 | 87916 | 0 | 0 |  |
| e34-ceil-rtt71-16KiB-r3 | 71 | 0 | 16KiB | 12.134 | 34.9 | 46.05 | 0.0446 | 29.653 | 92.6 | 4096 | 86906 | 0 | 0 |  |
| e34-ceil-rtt71-64KiB-r3 | 71 | 0 | 64KiB | 7.444 | 13.6 | 73.96 | 0.0342 | 36.955 | 14.2 | 1024 | 87336 | 0 | 0 |  |
| e34-ceil-rtt71-256KiB-r3 | 71 | 0 | 256KiB | 7.635 | 16.8 | 72.46 | 0.0340 | 35.912 | 3.6 | 256 | 87392 | 0 | 0 |  |
| e34-cap8mb-rtt0-16KiB-r1 | 0 | 8MB | 16KiB | 59.705 | 62.7 | 10.16 | 0.2240 | 30.547 | 455.5 | 4096 | 86726 | 37 | 0 |  |
| e34-cap8mb-rtt0-64KiB-r1 | 0 | 8MB | 64KiB | 59.709 | 62.9 | 10.09 | 0.2151 | 29.355 | 113.9 | 1024 | 86757 | 32 | 0 |  |
| e34-cap8mb-rtt0-256KiB-r1 | 0 | 8MB | 256KiB | 59.721 | 67.1 | 10.09 | 0.2140 | 29.206 | 28.5 | 256 | 86757 | 23 | 0 |  |
| e34-cap8mb-rtt0-16KiB-r2 | 0 | 8MB | 16KiB | 59.698 | 69.7 | 10.11 | 0.2158 | 29.504 | 455.5 | 4096 | 86761 | 28 | 0 |  |
| e34-cap8mb-rtt0-64KiB-r2 | 0 | 8MB | 64KiB | 54.482 | 91.2 | 10.93 | 0.2058 | 30.696 | 103.9 | 1024 | 87880 | 2781 | 0 | COLLAPSED<90%cap |
| e34-cap8mb-rtt0-256KiB-r2 | 0 | 8MB | 256KiB | 54.481 | 92.3 | 10.93 | 0.2122 | 31.590 | 26.0 | 256 | 87933 | 2818 | 0 | COLLAPSED<90%cap |
| e34-cap8mb-rtt0-16KiB-r3 | 0 | 8MB | 16KiB | 59.696 | 65.8 | 10.10 | 0.2184 | 29.802 | 455.4 | 4096 | 86740 | 38 | 0 |  |
| e34-cap8mb-rtt0-64KiB-r3 | 0 | 8MB | 64KiB | 59.713 | 65.0 | 10.10 | 0.2074 | 28.312 | 113.9 | 1024 | 86681 | 39 | 0 |  |
| e34-cap8mb-rtt0-256KiB-r3 | 0 | 8MB | 256KiB | 59.737 | 71.3 | 10.16 | 0.2173 | 29.653 | 28.5 | 256 | 86678 | 66 | 0 |  |
| e34-cap8mb-rtt71-16KiB-r1 | 71 | 8MB | 16KiB | 8.182 | 14.7 | 67.35 | 0.0370 | 36.359 | 62.4 | 4096 | 87152 | 0 | 0 | COLLAPSED<90%cap |
| e34-cap8mb-rtt71-64KiB-r1 | 71 | 8MB | 64KiB | 9.080 | 116.4 | 61.00 | 0.0373 | 33.081 | 17.3 | 1024 | 90544 | 1432 | 0 | COLLAPSED<90%cap |
| e34-cap8mb-rtt71-256KiB-r1 | 71 | 8MB | 256KiB | 8.530 | 16.8 | 65.07 | 0.0352 | 33.379 | 4.1 | 256 | 87148 | 0 | 0 | COLLAPSED<90%cap |
| e34-cap8mb-rtt71-16KiB-r2 | 71 | 8MB | 16KiB | 8.254 | 54.8 | 66.88 | 0.0369 | 35.912 | 63.0 | 4096 | 87638 | 0 | 0 | COLLAPSED<90%cap |
| e34-cap8mb-rtt71-64KiB-r2 | 71 | 8MB | 64KiB | 7.112 | 25.2 | 77.46 | 0.0336 | 37.998 | 13.6 | 1024 | 88438 | 859 | 0 | COLLAPSED<90%cap |
| e34-cap8mb-rtt71-256KiB-r2 | 71 | 8MB | 256KiB | 8.253 | 79.7 | 67.16 | 0.0365 | 35.763 | 3.9 | 256 | 89829 | 1444 | 0 | COLLAPSED<90%cap |
| e34-cap8mb-rtt71-16KiB-r3 | 71 | 8MB | 16KiB | 10.285 | 107.5 | 53.95 | 0.0404 | 31.590 | 78.5 | 4096 | 89880 | 1430 | 0 | COLLAPSED<90%cap |
| e34-cap8mb-rtt71-64KiB-r3 | 71 | 8MB | 64KiB | 7.791 | 14.7 | 70.83 | 0.0353 | 36.508 | 14.9 | 1024 | 87264 | 0 | 0 | COLLAPSED<90%cap |
| e34-cap8mb-rtt71-256KiB-r3 | 71 | 8MB | 256KiB | 10.472 | 75.5 | 53.34 | 0.0380 | 29.355 | 5.0 | 256 | 88962 | 49 | 0 | COLLAPSED<90%cap |
| e34-cap30mb-rtt0-16KiB-r1 | 0 | 30MB | 16KiB | 228.329 | 259.3 | 3.44 | 0.7400 | 27.418 | 1742.0 | 4096 | 88378 | 883 | 0 |  |
| e34-cap30mb-rtt0-64KiB-r1 | 0 | 30MB | 64KiB | 228.524 | 274.7 | 3.43 | 0.7352 | 27.269 | 435.9 | 1024 | 88562 | 573 | 0 |  |
| e34-cap30mb-rtt0-256KiB-r1 | 0 | 30MB | 256KiB | 170.241 | 318.8 | 4.27 | 0.5645 | 27.716 | 81.2 | 256 | 88051 | 594 | 0 | COLLAPSED<90%cap |
| e34-cap30mb-rtt0-16KiB-r2 | 0 | 30MB | 16KiB | 227.816 | 258.7 | 3.50 | 0.7531 | 27.865 | 1738.1 | 4096 | 88705 | 705 | 0 |  |
| e34-cap30mb-rtt0-64KiB-r2 | 0 | 30MB | 64KiB | 230.051 | 253.8 | 3.44 | 0.7279 | 26.822 | 438.8 | 1024 | 87702 | 177 | 0 |  |
| e34-cap30mb-rtt0-256KiB-r2 | 0 | 30MB | 256KiB | 230.634 | 260.0 | 3.45 | 0.7214 | 26.673 | 110.0 | 256 | 87264 | 686 | 0 |  |
| e34-cap30mb-rtt0-16KiB-r3 | 0 | 30MB | 16KiB | 225.728 | 242.2 | 3.51 | 0.7104 | 27.120 | 1722.2 | 4096 | 89514 | 976 | 0 |  |
| e34-cap30mb-rtt0-64KiB-r3 | 0 | 30MB | 64KiB | 226.930 | 256.9 | 3.41 | 0.7383 | 27.269 | 432.8 | 1024 | 89167 | 846 | 0 |  |
| e34-cap30mb-rtt0-256KiB-r3 | 0 | 30MB | 256KiB | 228.223 | 260.0 | 3.43 | 0.7081 | 26.226 | 108.8 | 256 | 88648 | 783 | 0 |  |
| e34-cap30mb-rtt71-16KiB-r1 | 71 | 30MB | 16KiB | 8.994 | 21.5 | 62.19 | 0.0358 | 32.037 | 68.6 | 4096 | 87010 | 0 | 0 | COLLAPSED<90%cap |
| e34-cap30mb-rtt71-64KiB-r1 | 71 | 30MB | 64KiB | 9.420 | 21.0 | 58.94 | 0.0357 | 30.547 | 18.0 | 1024 | 87104 | 0 | 0 | COLLAPSED<90%cap |
| e34-cap30mb-rtt71-256KiB-r1 | 71 | 30MB | 256KiB | 11.873 | 104.9 | 48.10 | 0.0413 | 28.163 | 5.7 | 256 | 87947 | 0 | 0 | COLLAPSED<90%cap |
| e34-cap30mb-rtt71-16KiB-r2 | 71 | 30MB | 16KiB | 8.632 | 18.6 | 64.03 | 0.0366 | 34.124 | 65.9 | 4096 | 87185 | 0 | 0 | COLLAPSED<90%cap |
| e34-cap30mb-rtt71-64KiB-r2 | 71 | 30MB | 64KiB | 8.981 | 14.7 | 61.76 | 0.0352 | 31.590 | 17.1 | 1024 | 87141 | 0 | 0 | COLLAPSED<90%cap |
| e34-cap30mb-rtt71-256KiB-r2 | 71 | 30MB | 256KiB | 18.693 | 247.5 | 30.79 | 0.0617 | 26.971 | 8.9 | 256 | 89466 | 0 | 0 | COLLAPSED<90%cap |
| e34-cap30mb-rtt71-16KiB-r3 | 71 | 30MB | 16KiB | 17.872 | 35.9 | 31.78 | 0.0658 | 29.802 | 136.4 | 4096 | 87180 | 0 | 0 | COLLAPSED<90%cap |
| e34-cap30mb-rtt71-64KiB-r3 | 71 | 30MB | 64KiB | 8.943 | 44.0 | 61.99 | 0.0348 | 31.441 | 17.1 | 1024 | 87617 | 0 | 0 | COLLAPSED<90%cap |
| e34-cap30mb-rtt71-256KiB-r3 | 71 | 30MB | 256KiB | 18.139 | 29.4 | 31.81 | 0.0677 | 30.547 | 8.6 | 256 | 86536 | 0 | 0 | COLLAPSED<90%cap |
| e34-small-ceil-1KiB-r1 | 0 | 0 | 1KiB | 440.708 | 486.2 | 2.43 | 1.4996 | 31.292 | 53797.4 | 65536 | 100385 | 0 | 0 |  |
| e34-small-ceil-4KiB-r1 | 0 | 0 | 4KiB | 459.453 | 463.9 | 2.31 | 1.2940 | 26.673 | 14021.4 | 16384 | 98339 | 0 | 0 |  |
| e34-small-ceil-16KiB-r1 | 0 | 0 | 16KiB | 526.035 | 525.1 | 2.16 | 1.2383 | 22.203 | 4013.3 | 4096 | 86054 | 0 | 0 |  |
| e34-small-ceil-1KiB-r2 | 0 | 0 | 1KiB | 443.768 | 445.9 | 2.31 | 1.5096 | 31.292 | 54170.9 | 65536 | 98339 | 0 | 0 |  |
| e34-small-ceil-4KiB-r2 | 0 | 0 | 4KiB | 458.825 | 462.7 | 2.29 | 1.3263 | 26.673 | 14002.2 | 16384 | 98339 | 0 | 0 |  |
| e34-small-ceil-16KiB-r2 | 0 | 0 | 16KiB | 515.330 | 526.4 | 2.22 | 1.1847 | 22.352 | 3931.7 | 4096 | 86051 | 0 | 0 |  |
| e34-small-ceil-1KiB-r3 | 0 | 0 | 1KiB | 443.072 | 444.4 | 2.33 | 1.5312 | 31.590 | 54086.0 | 65536 | 98339 | 0 | 0 |  |
| e34-small-ceil-4KiB-r3 | 0 | 0 | 4KiB | 466.844 | 475.2 | 2.26 | 1.3412 | 26.375 | 14247.0 | 16384 | 98342 | 0 | 0 |  |
| e34-small-ceil-16KiB-r3 | 0 | 0 | 16KiB | 501.046 | 540.8 | 2.25 | 1.1519 | 22.352 | 3822.7 | 4096 | 88035 | 0 | 0 |  |
| e34-small-cap30mb-1KiB-r1 | 0 | 30MB | 1KiB | 227.064 | 278.8 | 3.45 | 1.0019 | 36.955 | 27717.8 | 65536 | 100340 | 88 | 0 |  |
| e34-small-cap30mb-4KiB-r1 | 0 | 30MB | 4KiB | 225.264 | 271.7 | 3.50 | 0.8353 | 31.888 | 6874.5 | 16384 | 101284 | 679 | 0 |  |
| e34-small-cap30mb-16KiB-r1 | 0 | 30MB | 16KiB | 227.411 | 253.5 | 3.43 | 0.7399 | 27.418 | 1735.0 | 4096 | 88745 | 847 | 0 |  |
| e34-small-cap30mb-1KiB-r2 | 0 | 30MB | 1KiB | 227.016 | 279.7 | 3.44 | 0.9979 | 36.955 | 27712.0 | 65536 | 100333 | 235 | 0 |  |
| e34-small-cap30mb-4KiB-r2 | 0 | 30MB | 4KiB | 225.065 | 276.2 | 3.50 | 0.8338 | 31.739 | 6868.4 | 16384 | 101520 | 738 | 0 |  |
| e34-small-cap30mb-16KiB-r2 | 0 | 30MB | 16KiB | 227.257 | 263.7 | 3.40 | 0.7412 | 27.418 | 1733.8 | 4096 | 89128 | 885 | 0 |  |
| e34-small-cap30mb-1KiB-r3 | 0 | 30MB | 1KiB | 227.036 | 279.7 | 3.45 | 0.9880 | 36.508 | 27714.3 | 65536 | 100297 | 220 | 0 |  |
| e34-small-cap30mb-4KiB-r3 | 0 | 30MB | 4KiB | 227.122 | 250.7 | 3.43 | 0.8500 | 31.441 | 6931.2 | 16384 | 100202 | 158 | 0 |  |
| e34-small-cap30mb-16KiB-r3 | 0 | 30MB | 16KiB | 227.652 | 255.1 | 3.44 | 0.7866 | 29.057 | 1736.8 | 4096 | 88787 | 816 | 0 |  |
| e34-small-cap8mb-1KiB-r1 | 0 | 8MB | 1KiB | 59.026 | 69.0 | 10.20 | 0.2810 | 38.743 | 7205.3 | 65536 | 99207 | 67 | 0 |  |
| e34-small-cap8mb-4KiB-r1 | 0 | 8MB | 4KiB | 59.029 | 65.3 | 10.20 | 0.2460 | 33.975 | 1801.4 | 16384 | 99176 | 75 | 0 |  |
| e34-small-cap8mb-16KiB-r1 | 0 | 8MB | 16KiB | 59.703 | 68.7 | 10.09 | 0.2219 | 30.249 | 455.5 | 4096 | 86735 | 45 | 0 |  |
| e34-small-cap8mb-1KiB-r2 | 0 | 8MB | 1KiB | 59.028 | 70.4 | 10.20 | 0.2746 | 37.849 | 7205.6 | 65536 | 99172 | 79 | 0 |  |
| e34-small-cap8mb-4KiB-r2 | 0 | 8MB | 4KiB | 59.033 | 61.7 | 10.21 | 0.2474 | 34.124 | 1801.5 | 16384 | 99135 | 47 | 0 |  |
| e34-small-cap8mb-16KiB-r2 | 0 | 8MB | 16KiB | 56.020 | 109.8 | 10.70 | 0.2112 | 30.696 | 427.4 | 4096 | 88219 | 26 | 0 | COLLAPSED<90%cap |
| e34-small-cap8mb-1KiB-r3 | 0 | 8MB | 1KiB | 59.075 | 68.7 | 10.22 | 0.2782 | 38.445 | 7211.3 | 65536 | 98529 | 52 | 0 |  |
| e34-small-cap8mb-4KiB-r3 | 0 | 8MB | 4KiB | 59.036 | 67.0 | 10.20 | 0.2486 | 34.273 | 1801.6 | 16384 | 99128 | 75 | 0 |  |
| e34-small-cap8mb-16KiB-r3 | 0 | 8MB | 16KiB | 59.704 | 68.2 | 10.10 | 0.2216 | 30.249 | 455.5 | 4096 | 86670 | 55 | 0 |  |


## 2. The pinned-rate ladders (the load-robust result)

Chrome CPU per GB of payload delivered, at a **constant delivered rate**, as the message size is swept
16× down and 16× up. n=3 per arm (n=6 at 16 KiB, baseline split before/after the sweep). Means of `chrome_cs/GB`:

| cap | 1 KiB | 4 KiB | 16 KiB | 64 KiB | 256 KiB | msgs/s at 16 KiB vs 256 KiB |
|---|---|---|---|---|---|---|
| uncapped (~440–530 Mbps) | 31.39 | 26.60 | **22.60** | 21.53 | 21.21 | 3,812 vs 242 |
| 8 MB (64 Mbps) | 38.35 | 34.13 | **30.17** | 29.47 | 30.15 | 451 vs 28 |
| 30 MB (240 Mbps) | 36.81 | 31.67 | **27.72** | 27.13 | 26.87 | 1,735 vs 100 |

Least-squares `chrome_cores` vs messages/s over the full 1 KiB→256 KiB ladder, within block:

| cap | cells | slope | max residual | 16 KiB → 256 KiB (16× fewer msgs) | 16 KiB → 1 KiB (16× more msgs) |
|---|---|---|---|---|---|
| uncapped | 18 | **7.1 µs/msg** | 0.364 cores | **−6.2 %** | +38.9 % |
| 8 MB | 15 | **8.7 µs/msg** | 0.015 cores | **−0.1 %** | +27.1 % |
| 30 MB | 17 | **9.8 µs/msg** | 0.048 cores | **−3.0 %** | +32.8 % |

Read this as two statements, both load-robust because both are within-block at constant delivered rate:

1. **A per-message term exists and is measurable, but only in the sub-16-KiB direction.** Three independent
   rate settings agree on **7–10 µs of Chrome CPU per message** (residual ≤ 0.36 cores uncapped, ≤ 0.048
   cores at the pins). Note the asymmetries that show it is *not* pure dispatch: the 1 KiB arm moves **15 %
   more wire datagrams** (`fwd` 98,339–101,520 vs 86,051 at 16 KiB — 1,568 vs 1,345 dg/MiB) and generates
   token-bucket drops (up to 885), so part of its +27…+39 % is per-datagram work and retransmission, not
   `onmessage`. **The 7–10 µs slope is therefore an upper bound on dispatch cost.**
2. **In the direction N1 proposes, there is nothing to win.** Going 16 KiB → 256 KiB is a 16× message-count
   reduction and buys **−6.2 % / −0.1 % / −3.0 %** of receiver CPU per byte. The slope extrapolates to
   −1.0…−2.0 % at the pins; the measured value agrees. Compare the in-block baseline spread for the *same*
   16 KiB arm run before and after each sweep: **uncapped 0.957–1.261 cores (31.8 %), 8 MB 0.211–0.224
   (6.1 %), 30 MB 0.710–0.787 (10.7 %)**. The 16 KiB→256 KiB effect is *at or below* the noise at two of the
   three rates.

### What this means at the field's operating point

| | E28 4-vCPU bare (near field) | E32-W5 1-vCPU bare |
|---|---|---|
| agent throughput | 123.68 Mbps | 77.363 Mbps |
| messages (16 KiB) | 48,258 over 51.1 s → **943.6/s** | 48,258 over 81.758 s → **590/s** |
| client CPU | 1.27 cores (renderer 0.63) | 0.337 renderer, 0.597 client |
| per-byte client CPU | 82 CPU-s/GB | — |
| **per-message term (this experiment)** | 943.6 × 9.8 µs = **0.0092 core** = **0.7 %** of 1.27 | 0.0058 core |
| **saving from 16 KiB → 64 KiB messages** | **≤ 0.7 % of receiver CPU** | ≤ 0.5 % |

The field plateau is set by work that scales with **bytes**, not messages. To go from 123.68 to the near-field
ceiling's 138 Mbps needs ~12 %; message granularity offers ≤1 %.

### Is the receiver's *message rate* the limiting quantity? No — by 57×

The lab bare receiver, same 16 KiB framing, same code path:

| arm | delivered | messages/s | chrome_cores | of the 10-core Mac |
|---|---|---|---|---|
| uncapped, 1 KiB | **443 Mbps** | **54,018** | 1.51 | 15 % |
| uncapped, 4 KiB | 460 Mbps | 14,090 | 1.32 | 13 % |
| uncapped, 16 KiB | 526 Mbps | 3,812 | 1.19 | 12 % |
| 240 Mbps cap, 1 KiB | 227 Mbps | 27,715 | 1.00 | 10 % |
| 240 Mbps cap, 16 KiB | 227 Mbps | 1,735 | 0.73 | 7 % |

The field runs **590–944 msg/s**. The lab receiver does **27,715 msg/s at a pinned 240 Mbps** for 1.00 core and
**54,018 msg/s uncapped while still delivering 443 Mbps** — 9× the field throughput at 57× the field message
rate. Message rate is not the limiting quantity, and it is not close.

## 3. The uncapped ceiling (best-case plateau, the one uncapped number that is trustworthy)

| chunk | mbps (n=3, or n=6 for 16 KiB) | best 5×100 ms plateau | chrome_cores |
|---|---|---|---|
| 1 KiB | 441 / 444 / 443 | 446–486 | 1.500 / 1.510 / 1.531 |
| 4 KiB | 459 / 459 / 467 | 463–475 | 1.294 / 1.326 / 1.341 |
| 16 KiB | 410 / 518 / 527 / 526 / 515 / 501 | 455–556 | 0.957 / 1.261 / 1.261 / 1.238 / 1.185 / 1.152 |
| 64 KiB | 522 / 523 / 526 | 522–528 | 1.203 / 1.212 / 1.227 |
| 256 KiB | 455 / 533 / 533 | 302–537 | 0.788 / 1.186 / 1.175 |

Two honest non-reproductions of E6 at rtt 0:

- **E6's "64 KiB wins on a clean path" does not reproduce here.** 16 KiB (501–527, ceiling 525–556),
  64 KiB (522–526, ceiling 522–528) and 256 KiB (533, 533, one 455) are the same within the block. E6's
  64 KiB advantage was "a throughput effect" (E23 §2) and it is not visible on this host at rtt 0.
- **E6's "256 KiB is a trap" does not reproduce**, at rtt 0 and at the same 240 Mbps cap that produced it:
  228 / 231 / 228 Mbps with one 170 Mbps dip (2/3 healthy vs E6's 1/3). My queue is *shallower* than E6's
  (3 MB derived vs 5 MB explicit), so queue depth does not explain E6's trap; I did not test rtt 25/100, so
  this is "did not reappear", not "refuted". The `ceil` column is also coarse for 256 KiB (one message =
  22.5 Mbps of a 100 ms window at these rates), so its 302 reading on the 455 Mbps cell is quantisation, not
  a stall.

## 4. rtt 71: message size is completely irrelevant in the regime that matters

At rtt 71 **every** arm at **every** cap collapses — 7.1–18.7 Mbps, `shim_drop = 0` in most cells, 100 % of the
payload eventually delivered, identical `fwd` (~87,000–90,500 per 64 MiB):

| cap | 1 KiB | 4 KiB | 16 KiB | 64 KiB | 256 KiB |
|---|---|---|---|---|---|
| uncapped | — | — | 8.6 / 7.9 / 12.1 | 8.2 / 7.3 / 7.4 | 8.3 / 7.6 / 7.6 |
| 8 MB | — | — | 8.2 / 8.3 / 10.3 | 9.1 / 7.1 / 7.8 | 8.5 / 8.3 / 10.5 |
| 30 MB | — | — | 9.0 / 8.6 / 17.9 | 9.4 / 9.0 / 8.9 | 11.9 / 18.7 / 18.1 |

The rtt-71 collapse **is not rate-capped on this host** — the 8 MB and 30 MB token buckets were irrelevant
(delivered 7–19 Mbps against 64 and 240 Mbps caps), which is why the "rate caps are bit-for-bit reproducible"
rule cannot be used at rtt 71 tonight (E23 saw the same thing: 7 of 13 cells at rtt 71 / 80 Mbps collapsed).
Chunk size changes **nothing**: at every cap the three sizes overlap completely, exactly as E6 §2 found for the
loss-collapse. This independently reproduces E6/E11's conclusion — the collapse is a *window* phenomenon
(`W/RTT`), not a framing one — at zero added loss.

## 5. Interpretation

- **N1 is closed, and it was closed before tonight.** The proposal was: the ~48,258 `onmessage` events are the
  serialized limit, so send 64–256 KiB messages. Measured: per-message cost is 7–10 µs (upper bound), so the
  48,258 events cost **0.007–0.009 core** out of the 1.27 the receiver spent; and shrinking the message count
  16× moves receiver CPU per byte by ≤6 %, at or under the baseline spread. The hypothesis's own arithmetic
  does not survive contact, either: at 16 KiB, 123.68 Mbps is **943.6 msg/s**, not the 7,700 msg/s the review
  used to turn 82 µs/msg into 0.63 core — and the measured *marginal* dispatch cost is 7–10 µs, not 82 µs
  (82 µs is closer to nothing in particular; E28's total renderer time per message is 0.63 cores / 943.6
  msg/s = 667 µs, which is per-byte work plus delivery, not dispatch). Sending 64 KiB messages would have
  bought ~0.5 %, and 256 KiB would have bought the same while reintroducing E6's instability risk.
- **What E6 did and did not cover.** E6 swept exactly `{16, 64, 256 KiB} × rtt {25,71,100} × loss {0,1e-3}` at a
  240 Mbps cap and 5 MB queue, and reported throughput, ceilings, stall shape and wire volume — **no CPU
  measurement of either end**. So E6 could not distinguish per-message from per-byte cost, and its 256 KiB
  collapse is not evidence for N1: E6 observed the collapse's top-5-window ceiling *pinned at ~105 Mbps in
  four separate cells at all three RTTs including rtt 25 where RTT cannot matter* and correctly called it a
  "receiver-side granularity limit" — but a limit on buffering/reassembly of one 256 KiB message, which is a
  *capacity* claim, not a *dispatch-rate* claim, and the lab's own 256 KiB arm here delivered 533 Mbps
  uncapped and 228 Mbps pinned. E6 did not test anything below 16 KiB, did not test rtt ≤ 25 with a pinned
  rate, did not measure receiver CPU, and did not test a 256 KiB arm without the 5 MB queue.
- **E23 §2 did answer it.** At a pinned 80 Mbps E23 swept chunk {16, 64, 256 KiB} and found sender CPU-s/GB
  54.7 / 56.9 / 56.1 (spread 4 %, inside noise) with **`shim_fwd` = 86,053–86,263 datagrams for 64 MiB in
  every arm**, and receiver cores flat at 0.26–0.30. That is N1 tested directly, 15 days before this proposal,
  and its explanation is the one that holds: SCTP re-fragments every message into the same ~780-byte
  datagrams, so per-byte transport work is invariant to message size. This experiment's `fwd` reproduces
  that invariant at 16/64/256 KiB (86,051 / ~87,000 / ~87,000 per 64 MiB, and *higher* — 98,000–101,500 —
  only below 4 KiB, where SCTP chunk headers and padding cost real bytes).
- **Where the remaining lever is, unchanged:** per-datagram cost in pion's SCTP/DTLS path (E23 §5) and
  whatever the field's VM/contention regime adds per byte (the lab's same Chrome does 21–31 CPU-s/GB against
  the field's 82). Neither is addressable by message granularity.

## 6. Caveats

1. **Lab, not field.** No field rig, container or VM touched. The per-message *slope* is a Mac measurement; the
   *conclusion* only needs the slope to be small relative to the field's per-byte cost, which it is by 2–3
   orders of magnitude.
2. **The sanity gate failed 2/3** (240.7 / 9.7 / 9.1 Mbps) and the uncapped 16 KiB mean is bimodal
   (410–527). Every headline number is a within-block, constant-delivered-rate comparison; the error bars
   quoted are the in-block 16 KiB baseline spreads (31.8 % uncapped, 6.1 % at 64 Mbps, 10.7 % at 240 Mbps).
3. **`--queue` differs from E6.** I used the harness default (100 ms of the cap: 3 MB at 30 MB/s) where E6 used
   an explicit 5 MB. That is why my rate-capped cells carry `shim_drop > 0` (0.03–0.9 % of datagrams) where
   E6's were 0, and it is a genuine confound for the 256 KiB comparison in §3 — but a shallower queue should
   make a 256 KiB burst *worse*, and it did not, so it does not rescue E6's trap.
4. **The 1 KiB arm is not a clean dispatch probe.** It moves 15 % more wire datagrams (1,568 vs 1,345 dg/MiB)
   and generates token-bucket drops, so the 7–10 µs slope mixes dispatch with per-datagram and retransmission
   work. It is an upper bound, which is the direction that favours N1 — and N1 still loses.
5. **rtt 71 is unusable for rate-capped comparisons on this host** (all 27 cells collapsed below their cap);
   those cells are reported as raw evidence that chunk size does not move the collapse, nothing more.
6. **n=3 per arm (n=6 at 16 KiB), one host, one session; no statistical tests.** Cells excluded from fits are
   named in §1, never dropped from the table.
7. **`chrome_cores` sums every Chrome/Chromium process** (`cpu.go: chromeCPUSeconds`, `ps -Ao
   cputime=,comm=`) — renderer + network service + browser, i.e. E28's "client total", not renderer-only. The
   comparison in §2 uses E28's client total (1.27) for exactly that reason.

## 7. Artifacts (`docs/superpowers/spikes/results/raw/exp34-message-granularity/`)

| file | what |
|---|---|
| `run-exp34.py` | runner (blocks `sanity`, `ceil`, `cap8mb`, `cap30mb`, `small`); one `benchdirect` at a time; row appended to `cells.tsv` after every cell |
| `row.py` | JSON/log → TSV row (ceiling, stalls, cs/GB, msg/s, µs/msg) + machine `snapshot()` |
| `analyze.py` → `analysis.txt` | the §2/§4 tables: ladders, least-squares slopes, 16 KiB baselines, field projection |
| `cells.tsv` | one row per cell, 85 rows |
| `runner.log` | START/OK/FAIL per cell + block markers |
| `system-snapshots.txt` | `uptime` + `vm.swapusage` + top-8 CPU before and after every cell, plus session start/end |
| `<label>.json` / `<label>.log` | raw result + full stdout/stderr per cell |
| `rawtable.md` | the §1 table as generated |

No harness source change was needed; no `git` command was run.

### Appendix — every cell (incremental; see §1 for column meanings)
