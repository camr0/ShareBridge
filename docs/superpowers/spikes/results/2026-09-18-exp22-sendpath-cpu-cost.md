# Experiment E22 — v1 send-path CPU cost per byte (and the 1-core saturation rate) — 2026-09-18

**Verdict: the prod send path costs ~46 CPU-seconds per GB delivered, and it is *per-byte* with
essentially zero fixed overhead — so it is a genuinely batchable / replicable cost, and the fork's
premise is sound. One core saturates at ~180 Mbps in this lab.** Three independent fits agree:
**45.8 / 46.6 / 47.5 CPU-s per GB** (rtt 12 only; all prod cells; size-sweep only). The fixed startup
term is **+0.02 s** — negligible against the ~3 s of per-byte cost for 64 MiB. Measured sender cost is
**0.54–0.58 cores per 100 Mbps** above 150 Mbps (rising to 0.70–0.72 at 37 Mbps, which the regression
shows is the *bench's* poll loop, not the send path). Directly measured: **169–202 Mbps per core**.

**A second, unasked-for finding that changes the brief's premise: the lab prod stack is not a ~225 Mbps
transport.** 225 Mbps is E14's *rate-capped* number (30 MB = 240 Mbps cap). **Uncapped at rtt 12 the same
production stack moves 64 MiB at 521–533 Mbps** — but it spends **3.35 cores** to do it. The field's
122 Mbps is 4.3× below the lab's uncapped ceiling; the reason is CPU, not the transport.

## 0. Method and what is being measured

- **Lab only. No field rig, container or VM was touched** (runbook §1). **No `git` command was run.**
- Headline cells are **`--mode prod`** — the production peer/transfer stack the v1 agent runs.
- **Go/pion is the DATA SENDER** (`transfer.Manager` → `dc.Send`) and Chrome is the receiver — the same
  direction as the field. `go_cpu_seconds` is therefore **the sender's CPU**, from
  `getrusage(RUSAGE_SELF)` around the transfer window only (browser launch, WebRTC handshake and
  candidate gathering are excluded).
- Caps `--bandwidth` {5, 10, 20, 30 MB} (≈40/80/160/240 Mbps) × rtt {12, 71} ms, plus an **uncapped** cell
  at each RTT; a **size sweep** {16, 32, 64, 128 MiB}; 64 MiB default, `--queue 5MB` (the same bottleneck
  E11/E14 used, so the 30 MB cells are directly comparable to E14's 228.9 Mbps clean-path baseline).
  n=3 for the rate/cap matrix, n=2 for the size sweep, **interleaved (rep outer, arms inner)** so machine
  drift hits every arm of a comparison equally. **One `benchdirect` process at a time.**
- **All 39 cells delivered 100 % of the payload with `shim_drop=0` and `shim_write_err=0`** — no cell was
  discarded on those criteria.
- Sanity gate (runbook §3): `--mode raw --rtt 71 --size 32MiB` = **126.68 Mbps** at the start and
  **38.45 Mbps** at the end of the block. That 3.3× spread on a *single unchanged config* is exactly the
  runbook's reliability warning; it is why every comparison below rests on **prod-mode regressions over
  many cells** and on the **interleaved** structure, never on a single raw sanity cell.

### Harness change (minimal, and it was required)

`--mode prod` reported **no CPU data at all**: `runProd` never assigned `GoCPUSec` / `ChromeCPUSec` /
`ChromeCores` / `ElapsedMs`, so **every prior prod-mode JSON in this repo has `"go_cpu_seconds":0`**
(verified on E14's `e14-loss0.0002-mc2MiB-r1.json`). The experiment is impossible without this, so
`prodbench.go` gained `startCPUSampler()` + `selfCPUSeconds()` around the transfer window, a `finishCPU()`
closure on the success and both failure paths, and `ElapsedMs` / `WallMbps` / `Samples` passthrough.
`go build ./...` and `go vet ./cmd/benchdirect/` pass.

**Two measured instrumentation artefacts, both quantified rather than assumed:**

1. `chromeCPUSeconds()` shells out to `/bin/ps` every 100 ms. Measured directly (200 calls): a
   `ps -Ao cputime=,comm=` costs ~34 ms of *child* CPU on this host, of which **0.840 ms is charged to the
   sender process itself**. That is a constant **+0.008 cores** on `go_cpu_seconds`, independent of rate.
   The child CPU is contention noise shared by every cell, not a sender-path cost.
2. The 50 ms `chromedp` `b.eval` polling loop sits inside the measurement window. Regressing
   `go_cpu` on *(bytes, wall-seconds)* recovers it as the per-second coefficient **c = 0.031–0.053 cores**
   (1.6–2.6 ms of CPU per 50 ms poll) — it is the entire reason the *ratio* looks worse at low rates, and
   it is a bench artefact, not a pion cost.

### ⚠️ One confounder I could not remove

**The shim runs inside the same `benchdirect` process.** So `go_cpu_seconds` = pion send path **+** the
in-process shim relay (`ReadFromUDP` + a `time.NewTimer` *per datagram* + `WriteToUDP` + token bucket,
`shim.go:219/302/320`). At ~780 B payload per datagram, 64 MiB is **~86,000 datagrams**, so the shim
contributes ~2–5 µs × 86,000 ≈ **0.17–0.43 s of the ~3.0 s total (~6–14 %)**. That is an *estimate from a
code read, not a measurement*, and it inflates the absolute per-GB constant. It does **not** affect the
results that matter here: the shim's datagram count is proportional to bytes and identical across every
cell, so it cannot create or hide the linearity, and it is the same in all arms.

## 1. The result

### 1.1 Cost is per-byte with negligible fixed overhead (the decisive measurement)

A rate sweep alone cannot separate "per byte" from "fixed per transfer", because every cell sends the same
64 MiB. The **size sweep** at a pinned rate can, and it is unambiguous:

| size | `go_cpu` (n=2) | Mbps |
|---|---|---|
| 16 MiB | 0.6644 / 0.7258 | 251.1 / 254.4 |
| 32 MiB | 1.4627 / 1.5397 | 236.9 / 237.1 |
| 64 MiB | 2.9944 / 3.0681 / 3.1751 | 229.1 / 229.1 / 229.2 |
| 128 MiB | 5.9749 / 5.8400 | 225.1 / 225.1 |

Fit `go_cpu = a + b·MiB` (rtt 12, cap 30 MB, n=9): **a = +0.022 s, b = 0.04641 s/MiB, R² = 0.9978,
max |resid| = 0.183 s.** The straight line passes through the origin: **a 128 MiB transfer costs 8× a
16 MiB transfer, not 8× plus a lump.** There is no meaningful fixed cost to amortise.

The general 3-parameter fit `go_cpu = a + b·MiB + c·seconds` over all 37 prod cells (13–533 Mbps,
16–128 MiB, both RTTs):

| fit | n | a (fixed) | **b (per-byte)** | c (per-second) | R² | 1 core saturates at |
|---|---|---|---|---|---|---|
| ALL prod | 37 | +0.127 s | **46.58 CPU-s/GB** | 0.031 cores | 0.941 | 184 Mbps |
| prod rtt 12 | 22 | +0.157 s | **45.81 CPU-s/GB** | 0.053 cores | 0.975 | 187 Mbps |
| rtt 12, cap 30 MB (size-only) | 9 | +0.022 s | **47.52 CPU-s/GB** | — | 0.998 | 181 Mbps |

The `c` coefficient is small and positive, i.e. wall-time is *not* what costs CPU. Corroboration from the
path-latency axis at the same payload and the same cap: rtt 12 cap 30 MB = 45.9 CPU-s/GB vs
**rtt 71 cap 30 MB = 41.6 CPU-s/GB** — a 10 % difference, so the cost is insensitive to round-trip count.
The cost tracks **data volume**, not time and not round trips.

### 1.2 CPU per GB and cores per 100 Mbps

`cs/GB = go_cpu / (received/1e9)`; means over each capped arm (n=3, except rtt 12 cap 30 MB which pools the
size sweep, n=9). Uncapped arms include their collapsed reps and are flagged:

| rtt | cap | n | Mbps | go_cores | **CPU-s/GB** | **cores/100 Mbps** | Mbps/core |
|---|---|---|---|---|---|---|---|
| 12 | 5 MB | 3 | 37.25 | 0.2646 | 56.8 | 0.710 | 141 |
| 12 | 10 MB | 3 | 74.93 | 0.4840 | 51.7 | 0.646 | 155 |
| 12 | 20 MB | 3 | 151.44 | 0.8722 | 46.1 | 0.576 | 174 |
| 12 | 30 MB | 9 | 235.23 | 1.2987 | 44.6 | 0.552 | 181 |
| 12 | uncap | 4 | 528.45 | 3.3657 | 51.0 | 0.637 | 157 |
| 71 | 5 MB | 3 | 35.51 | 0.2438 | 54.8 | 0.687 | 146 |
| 71 | 10 MB | 3 | 72.85 | 0.4511 | 49.5 | 0.619 | 162 |
| 71 | 20 MB | 3 | 104.20 | 0.5835 | 47.0 | 0.560 | 179 |
| 71 | 30 MB | 3 | 204.46 | 1.0639 | 41.6 | 0.520 | 192 |
| 71 | uncap | 3 | 73.47 | 0.4019 | 49.0 | 0.547 | 183 |

(The rtt 71 20 MB and uncapped arm means are dragged down by collapsed reps — 29.84 and 13.47/36.54 Mbps
— against clean siblings at 141.4 and 170.4 Mbps; those arms have a `Mbps/core` that is *higher* than the
surviving reps, i.e. the collapse cost wall time, not CPU. This is the per-byte result restated.)

Read the two rightmost columns against the regression: **cores/100 Mbps falls from 0.71 to 0.52 as rate
rises**, and the entire fall is the *constant* `c ≈ 0.03–0.05` core poll overhead being divided by a
larger rate. The per-byte cost — the thing the fork would attack — is flat at ~46 s/GB across a 7× rate
range. The uncapped row is the one genuine divergence (50.6 s/GB, 158 Mbps/core instead of 181): at
3.35 busy cores the send path oversubscribes this Mac's cache, and the transfer window is only 1.03 s, so
startup transient weighs more.

### 1.3 Where the CPU actually goes (code read, `internal/transfer/manager.go`)

**Note: `--chunk` is ignored in prod mode** — the real manager hardcodes `chunkSize = 64KB`
(`manager.go:24`), so `--chunk 16KiB` never took effect in these cells.

Per 64 KB chunk (64 MiB = 1024 chunks), the production send path does:

1. `benchStorage.GetFile` writes a 64 KB buffer into an `io.Pipe` — **copy 1**, plus a goroutine handoff;
2. `streamFile` reads it back via `pr.Read(buf)` — **copy 2**;
3. `encodeChunkFrame` does `make([]byte, 65552)` **and copies the whole chunk** — **copy 3**, i.e. **1024
   fresh 64 KB heap allocations per 64 MiB** (`manager.go:1370`);
4. `sendWithBackpressure` polls `BufferedAmount()` and, when over `maxBuffer = 5MB`, sleeps
   `sleepInterval = 10ms` — **a sleep-poll, allocating a timer each iteration** (`manager.go:1526`);
5. `dc.Send` → SCTP chunking + **DTLS encryption of every byte** + one `WriteToUDP` **per ~1200 B
   datagram → ~86,000 syscalls per 64 MiB**.

So ~3 full copies of every byte, one 64 KB heap allocation per 64 KB, and ~86,000 datagram syscalls per
64 MiB. That is exactly the surface a fork would attack: multi-byte syscalls / `writev` / GSO coalescing
and eliminating the pipe + envelope copies. Nothing in the measured curve suggests a large fixed cost that
batching would fail to help — the whole cost is volume-proportional.

## 2. The field bridge — what transfers and what does not

**Shape-only (transfers).** The *form* of the curve is a property of the code, not the host: cost is
proportional to bytes, ~zero fixed overhead, and insensitive to RTT. So the conclusion "**the sender needs
roughly one core per ~180 Mbps of payload, and halving the per-byte cost would roughly double that**" is a
code-level statement, and it is the hypothesis the brief asked to test.

**Order-of-magnitude (consistent).** The field agent burns **0.9–1.6 cores at 122 Mbps**, which inverts to
**63–113 CPU-s/GB on VERSA** vs the Mac's **46 CPU-s/GB** — i.e. **VERSA costs 1.4–2.4× more CPU per byte**.
Rescaling the lab's 180–187 Mbps/core by that factor predicts **1 core saturates at 75–129 Mbps on VERSA**,
which **brackets the field's measured 122 Mbps almost exactly**. So the brief's hypothesis — "~1 core per
130 Mbps" — is **supported in order of magnitude**, and the field's own CPU numbers land where the lab's
per-byte cost says they should.

**Absolute cores-per-Mbps (does NOT transfer).** This lab runs on a Mac that is *also* loaded
(load averages 2.0–3.9; `WindowServer` at 43–50 % CPU, `corespotlightd` at 102 %, `GoogleUpdater` at 98 %,
Time Machine `backupd`; `vm.swapusage` pinned at 6.20 GB of 7.17 GB throughout). VERSA additionally runs an
LLM server, Jellyfin and Immich. The 46 s/GB constant is a **lower bound**: it is what this Mac's cores do,
not what VERSA's do. Any *absolute* claim about the field (e.g. "the fork saves 0.4 cores at 122 Mbps")
needs a field replay.

**The honest bottom line for the fork business case.** The send path is a per-byte cost, it is ~46 CPU-s/GB
here and 1.4–2.4× that on VERSA, and at the field rate the sender is plausibly sitting near its 1-core
ceiling. That is real justification for batching/coalescing work — the measurable factor to move is a
per-byte constant, and there is no fixed overhead for such a change to be lost in. **But this experiment
does not show that the field's 122 Mbps is sender-CPU-bound.** Experiment 9's evidence pointed at the
*client sink* (browser userspace + SHA-1 verify + service-worker writes, 63 → 85 % busy). If the field
ceiling is the client, a cheaper agent send path raises 180 Mbps to ~360 Mbps and changes nothing. The
decisive test is unchanged and cheap: **pin the field agent's CPU or measure its per-GB cost directly at a
known operating rate, and see whether throughput moves.** This experiment supplies the lab-side constant
that test needs for comparison.

## 3. Caveats

1. **Lab, not field.** No field rig, container or VM touched (runbook §1). "VERSA's 63–113 s/GB" is
   *inferred* by inverting the field's `docker stats` cores over a window — it is not a measurement of the
   v1 send path.
2. **The shim is in-process and is inside `go_cpu_seconds`** (§0). Estimated **+6–14 %** on the absolute
   constant, by code read only. The linearity result is immune (shim work is per-datagram, and every cell
   sends the same ~86,000 datagrams).
3. **`go_cpu_seconds` is the whole in-window Go process**, so it includes the bench's own 50 ms `chromedp`
   polling loop (`c = 0.03–0.05 cores`, quantified above) and 0.008 cores of sampler overhead. The *pure*
   per-byte term is therefore, if anything, slightly **below** 46 s/GB — the fits already separate it.
4. **This Mac is noisy and this is one night with no statistical tests.** Two cells collapsed without any
   knob changing: rtt 71 uncapped r2 (13.47 Mbps) and rtt 71 cap 20 MB r1 (29.84 Mbps), against sibling
   reps at 170.4 Mbps and 141.4 Mbps. Both are **retained, not tidied away**, and they are consistent with
   the runbook's warning and with E2's documentation of collapse regimes on this host. They do not change
   the per-byte conclusion (a collapsed cell still sends the same bytes for the same ~3 s of CPU).
5. **Prod uncapped 521–533 Mbps at rtt 12 is reproducible within a block** (521.5 / 530.7 / 533.4 plus the
   tail 528.3) but is *not* a trustworthy cross-day number on this host — and it used 3.35 cores, so it says
   nothing about what the field agent can reach on ~1 core.
6. **Cost is linear in bytes; it is NOT decomposed** between per-byte copies/encryption and per-64 KB-chunk
   overhead (timer allocation, pipe handoff). Both scale with size at the production's fixed 64 KB chunk, so
   the size sweep cannot separate them. Distinguishing them would need a chunk-size sweep *inside*
   `transfer.Manager` (a code change), which is the natural next experiment if the fork is pursued.

## 4. Artifacts

Directory `docs/superpowers/spikes/results/raw/exp22-sendpath-cpu-cost/`:

| file | what |
|---|---|
| `run-exp22.sh` | runner, phases `sanity sweep uncap size tail`; one `benchdirect` at a time |
| `runner.log` | START/OK/FAIL per cell |
| `row.py`, `analyze.py` | JSON/log → TSV row; the regression fits (Gaussian elimination, no numpy) |
| `cells.tsv` | one row per cell, appended after every cell |
| `analysis.txt` | full fit output (all four fits + per-cell ratios) |
| `system-snapshots.txt` | `uptime` + `vm.swapusage` + top-8 CPU before and after every cell (78 snapshots) |
| `<label>.json` / `<label>.log` | raw result + stderr trace per cell (39 cells, 0 failures) |
| `appendix.md` | the §5 table, generated from `cells.tsv` |

Harness diff: `agent/cmd/benchdirect/prodbench.go` — prod-mode CPU/elapsed instrumentation (§0).

## 5. Appendix — every cell (appended incrementally during the run)

`ceil` = exp14's ceiling (median of the best sliding 5-tuple of 100 ms samples). `cs/GB` and
`cores/100Mbps` are `go_cpu/(recv/1e9)` and `go_cores/mbps*100`. No cell was discarded: `shim_drop = 0` and
`shim_write_err = 0` in **all 39**.

| label | mode | rtt | cap | mbps | ceil | elapsed ms | go_cpu s | go cores | chrome cores | recv bytes | shim fwd | drop | cerr | cs/GB | cores/100Mbps |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| e22-sanity-raw-rtt71 | raw | 71 | uncap | 126.68 | 289.2 | 2119 | 1.4412 | 0.6801 | 0.2958 | 33554432 | 45676 | 0 | 0 | 42.9501 | 0.53688 |
| e22-prod-rtt12-cap5MB-r1 | prod | 12 | 5MB | 37.25 | 45.2 | 14414 | 3.8215 | 0.2651 | 0.1532 | 67108864 | 86085 | 0 | 0 | 56.9452 | 0.71182 |
| e22-prod-rtt12-cap10MB-r1 | prod | 12 | 10MB | 74.94 | 88.0 | 7164 | 3.3627 | 0.4694 | 0.2639 | 67108864 | 86068 | 0 | 0 | 50.1078 | 0.62635 |
| e22-prod-rtt12-cap20MB-r1 | prod | 12 | 20MB | 151.42 | 174.2 | 3546 | 3.1014 | 0.8747 | 0.4887 | 67108864 | 86062 | 0 | 0 | 46.2142 | 0.57768 |
| e22-prod-rtt12-cap30MB-r1 | prod | 12 | 30MB | 229.10 | 254.6 | 2343 | 3.1751 | 1.3549 | 0.7001 | 67108864 | 86061 | 0 | 0 | 47.3129 | 0.59141 |
| e22-prod-rtt71-cap5MB-r1 | prod | 71 | 5MB | 36.91 | 42.0 | 14547 | 3.6934 | 0.2539 | 0.1443 | 67108864 | 86082 | 0 | 0 | 55.0355 | 0.68794 |
| e22-prod-rtt71-cap10MB-r1 | prod | 71 | 10MB | 72.99 | 85.8 | 7356 | 3.2554 | 0.4426 | 0.2344 | 67108864 | 86070 | 0 | 0 | 48.5098 | 0.60637 |
| e22-prod-rtt71-cap20MB-r1 | prod | 71 | 20MB | 29.84 | 47.2 | 17991 | 3.5586 | 0.1978 | 0.0934 | 67108864 | 86660 | 0 | 0 | 53.0266 | 0.66283 |
| e22-prod-rtt71-cap30MB-r1 | prod | 71 | 30MB | 204.58 | 267.6 | 2624 | 2.6616 | 1.0143 | 0.4973 | 67108864 | 86062 | 0 | 0 | 39.6612 | 0.49577 |
| e22-prod-rtt12-cap5MB-r2 | prod | 12 | 5MB | 37.25 | 45.2 | 14414 | 3.8668 | 0.2683 | 0.1606 | 67108864 | 86081 | 0 | 0 | 57.6205 | 0.72026 |
| e22-prod-rtt12-cap10MB-r2 | prod | 12 | 10MB | 74.94 | 82.6 | 7164 | 3.6246 | 0.5060 | 0.2513 | 67108864 | 86068 | 0 | 0 | 54.0103 | 0.67513 |
| e22-prod-rtt12-cap20MB-r2 | prod | 12 | 20MB | 151.48 | 172.0 | 3544 | 3.1192 | 0.8801 | 0.4937 | 67108864 | 86063 | 0 | 0 | 46.4796 | 0.58099 |
| e22-prod-rtt12-cap30MB-r2 | prod | 12 | 30MB | 229.11 | 246.4 | 2343 | 3.0681 | 1.3093 | 0.6926 | 67108864 | 86061 | 0 | 0 | 45.7177 | 0.57147 |
| e22-prod-rtt71-cap5MB-r2 | prod | 71 | 5MB | 32.70 | 39.8 | 16416 | 3.5205 | 0.2145 | 0.1038 | 67108864 | 86201 | 0 | 0 | 52.4594 | 0.65574 |
| e22-prod-rtt71-cap10MB-r2 | prod | 71 | 10MB | 72.98 | 85.8 | 7356 | 3.4147 | 0.4642 | 0.2485 | 67108864 | 86071 | 0 | 0 | 50.8834 | 0.63604 |
| e22-prod-rtt71-cap20MB-r2 | prod | 71 | 20MB | 141.37 | 176.2 | 3798 | 2.9244 | 0.7701 | 0.4154 | 67108864 | 86065 | 0 | 0 | 43.5771 | 0.54471 |
| e22-prod-rtt71-cap30MB-r2 | prod | 71 | 30MB | 204.48 | 251.8 | 2626 | 2.8739 | 1.0946 | 0.5407 | 67108864 | 86062 | 0 | 0 | 42.8238 | 0.53530 |
| e22-prod-rtt12-cap5MB-r3 | prod | 12 | 5MB | 37.25 | 44.2 | 14413 | 3.7536 | 0.2604 | 0.1620 | 67108864 | 86081 | 0 | 0 | 55.9324 | 0.69916 |
| e22-prod-rtt12-cap10MB-r3 | prod | 12 | 10MB | 74.91 | 87.0 | 7167 | 3.4157 | 0.4766 | 0.2636 | 67108864 | 86069 | 0 | 0 | 50.8977 | 0.63622 |
| e22-prod-rtt12-cap20MB-r3 | prod | 12 | 20MB | 151.43 | 172.0 | 3545 | 3.0553 | 0.8618 | 0.4964 | 67108864 | 86062 | 0 | 0 | 45.5276 | 0.56909 |
| e22-prod-rtt12-cap30MB-r3 | prod | 12 | 30MB | 229.20 | 247.2 | 2342 | 2.9944 | 1.2783 | 0.6940 | 67108864 | 86061 | 0 | 0 | 44.6196 | 0.55774 |
| e22-prod-rtt71-cap5MB-r3 | prod | 71 | 5MB | 36.91 | 41.2 | 14545 | 3.8244 | 0.2629 | 0.1550 | 67108864 | 86086 | 0 | 0 | 56.9881 | 0.71235 |
| e22-prod-rtt71-cap10MB-r3 | prod | 71 | 10MB | 72.59 | 91.0 | 7396 | 3.3022 | 0.4465 | 0.2161 | 67108864 | 86786 | 0 | 0 | 49.2070 | 0.61509 |
| e22-prod-rtt71-cap20MB-r3 | prod | 71 | 20MB | 141.38 | 175.2 | 3797 | 2.9715 | 0.7825 | 0.4065 | 67108864 | 86064 | 0 | 0 | 44.2785 | 0.55348 |
| e22-prod-rtt71-cap30MB-r3 | prod | 71 | 30MB | 204.33 | 260.2 | 2628 | 2.8449 | 1.0827 | 0.5373 | 67108864 | 86061 | 0 | 0 | 42.3920 | 0.52990 |
| e22-prod-rtt12-uncap-r1 | prod | 12 | uncap | 521.54 | 550.6 | 1029 | 3.4962 | 3.3964 | 1.1667 | 67108864 | 88366 | 0 | 0 | 52.0976 | 0.65122 |
| e22-prod-rtt71-uncap-r1 | prod | 71 | uncap | 170.41 | 355.4 | 3150 | 2.8163 | 0.8940 | 0.3360 | 67108864 | 89514 | 0 | 0 | 41.9663 | 0.52458 |
| e22-prod-rtt12-uncap-r2 | prod | 12 | uncap | 530.66 | 565.0 | 1012 | 3.3411 | 3.3025 | 1.1495 | 67108864 | 86058 | 0 | 0 | 49.7863 | 0.62233 |
| e22-prod-rtt71-uncap-r2 | prod | 71 | uncap | 13.47 | 40.8 | 39860 | 3.9029 | 0.0979 | 0.0452 | 67108864 | 87049 | 0 | 0 | 58.1582 | 0.72698 |
| e22-prod-rtt12-uncap-r3 | prod | 12 | uncap | 533.35 | 553.8 | 1007 | 3.3797 | 3.3575 | 1.1429 | 67108864 | 86058 | 0 | 0 | 50.3611 | 0.62951 |
| e22-prod-rtt71-uncap-r3 | prod | 71 | uncap | 36.54 | 327.0 | 14691 | 3.1431 | 0.2139 | 0.1000 | 67108864 | 89915 | 0 | 0 | 46.8356 | 0.58544 |
| e22-prod-sz16MiB-rtt12-cap30MB-r1 | prod | 12 | 30MB | 251.06 | 244.2 | 535 | 0.6644 | 1.2428 | 0.4992 | 16777216 | 22096 | 0 | 0 | 39.6008 | 0.49501 |
| e22-prod-sz32MiB-rtt12-cap30MB-r1 | prod | 12 | 30MB | 236.88 | 247.2 | 1133 | 1.4627 | 1.2907 | 0.6215 | 33554432 | 43049 | 0 | 0 | 43.5904 | 0.54488 |
| e22-prod-sz128MiB-rtt12-cap30MB-r1 | prod | 12 | 30MB | 225.05 | 251.4 | 4771 | 5.9749 | 1.2523 | 0.6962 | 134217728 | 172084 | 0 | 0 | 44.5163 | 0.55645 |
| e22-prod-sz16MiB-rtt12-cap30MB-r2 | prod | 12 | 30MB | 254.44 | 244.4 | 528 | 0.7258 | 1.3759 | 0.5426 | 16777216 | 21543 | 0 | 0 | 43.2600 | 0.54075 |
| e22-prod-sz32MiB-rtt12-cap30MB-r2 | prod | 12 | 30MB | 237.13 | 248.4 | 1132 | 1.5397 | 1.3601 | 0.6277 | 33554432 | 43049 | 0 | 0 | 45.8859 | 0.57357 |
| e22-prod-sz128MiB-rtt12-cap30MB-r2 | prod | 12 | 30MB | 225.07 | 255.6 | 4771 | 5.8400 | 1.2241 | 0.6915 | 134217728 | 172085 | 0 | 0 | 43.5116 | 0.54389 |
| e22-tail-raw-rtt71 | raw | 71 | uncap | 38.45 | 144.4 | 6982 | 1.5470 | 0.2216 | 0.0979 | 33554432 | 44907 | 0 | 0 | 46.1052 | 0.57631 |
| e22-tail-prod-rtt12-uncap | prod | 12 | uncap | 528.26 | 559.8 | 1016 | 3.4617 | 3.4062 | 1.1530 | 67108864 | 87869 | 0 | 0 | 51.5833 | 0.64479 |
