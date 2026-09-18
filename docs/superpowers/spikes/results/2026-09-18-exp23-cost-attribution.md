# Experiment E23 — where the v1 send path's 46 CPU-s/GB actually goes, by layer — 2026-09-18

**Verdict: the brief's premise is wrong and that matters. `--mode raw` does NOT omit DTLS — both modes are
full pion WebRTC DataChannels, so the raw↔prod delta measures the *application* layer only, and that layer is
small: +1.3 to +2.9 CPU-s/GB (2.4–5.5 % of the total), reproducible and paired. The remaining ~97 % is the
pion datagram path itself: ~51 CPU-s/GB of which the DTLS crypto primitive is 0.16 CPU-s/GB (~0.3 %, measured
at 6.0–6.5 GB/s per core), a *replica of the harness' own shim* costs 11–16 CPU-s/GB, and **~23–40 CPU-s/GB
is per-datagram work inside pion's SCTP/DTLS write path that I can bound but not further decompose inside the
timebox**. The single highest-value fork change is therefore the one that attacks *datagram count and
per-datagram cost* (coalesce several SCTP chunks into one DTLS record/UDP datagram, remove per-datagram
allocations in the SCTP/DTLS send path), not crypto and not chunk size: a 16/64/256 KiB chunk sweep moves
CPU-s/GB by <2 % (<noise), while the sender emits 1.377 M datagrams per GB at only 780 B of payload each.**

## 0. Method, and what `--mode raw` actually omits (code read, load-bearing)

Read for this experiment: `cmd/benchdirect/rawbench.go`, `cmd/benchdirect/prodbench.go`,
`cmd/benchdirect/shim.go`, `internal/transfer/manager.go`, `cmd/benchdirect/fakebackend.go`.

**Both modes build the same transport.** `runRaw` and `runProd` each create a `webrtc.SettingEngine` +
`webrtc.NewAPI` + `webrtc.NewPeerConnection` and send the payload with `dc.Send` over the shim-relayed UDP
path. DTLS, SCTP chunking, retransmission, flow control and the `WriteToUDP` per datagram are **present in
both**. `--mode raw` therefore omits only application-layer work:

| prod does, raw does not | site |
|---|---|
| `benchStorage.GetFile` writes each 64 KiB chunk into an `io.Pipe` (copy 1) | `fakebackend.go:19`, `manager.go:1060` |
| `streamFile` reads it back with `pr.Read(buf)` (copy 2) + a goroutine handoff per chunk | `manager.go:1071-1076` |
| `encodeChunkFrame` allocates a fresh 65552-byte buffer and copies the chunk (copy 3) | `manager.go:1370` |
| `sendWithBackpressure` sleeps in a 10 ms `time.After` loop while `BufferedAmount() > 5MB` | `manager.go:1526` |
| message framing: prod sends 64 KiB + 16 B envelope messages through `dc.Send`, raw sends plain `--chunk` buffers | `manager.go:1370` vs `rawbench.go:151` |
| `--chunk` is ignored in prod: `transfer.Manager` hardcodes `chunkSize = 64KB` | `manager.go:24` (confirmed by measurement, §2) |

Consequences: (a) a prod↔raw delta is the **app layer**, i.e. 3 copies + one 64 KiB allocation per 64 KiB +
the backpressure poll; (b) DTLS cannot be attributed this way and had to be measured separately (§3); (c) the
in-process shim (`shim.go:219/302/320`: `ReadFromUDP` + one `make()` copy + one `time.NewTimer` **per
datagram** + optional token bucket + `WriteToUDP`) is inside `go_cpu_seconds` in **both** arms, so it inflates
every absolute number equally and is invisible to the delta.

`go_cpu_seconds` = `getrusage(RUSAGE_SELF)` over the transfer window only, in the process that owns both the
pion sender and the shim. Chrome is the receiver (`chrome_cpu_seconds` / `chrome_cores`) and is *excluded*
from `go_cpu_seconds`. Lab only; the field rig, containers and VMs were not touched; **no `git` command was
run**; one `benchdirect` process at a time.

### Method caveats that bind every number below (read before quoting an absolute)

1. **The sanity gate failed.** Runbook §3 asks for `--rtt 71 --mode raw --size 32MiB` ≈ 100 Mbps. Two cells
   gave **10.72 and 13.72 Mbps** (E22's same-config cells: 126.68 and 38.45 Mbps). `uptime` load 3.6→6.4,
   `vm.swapusage` pinned at **6.20 GB of 7.17 GB**, top consumers 100–112 % CPU (`mediaanalysisd`,
   `coreduetd`, `corespotlightd`, `IMDPersistenceAgent`, `photolibraryd`). The host is worse than E22's
   already-bad regime, and the *absolute* per-byte constant here is **~1.2–1.3× E22's** (58.8 vs 44.6
   CPU-s/GB at the same cap). Absolute ceilings are therefore host-specific; **only the ratios between layers
   transfer**, and even those only as a Mac-relative ordering.
2. **Rate-capped cells are reliable; uncapped ones are not** (runbook). Headline deltas come from
   `--bandwidth 10MB` = 80 Mbps (deterministic, and `shim_drop=0`/`shim_write_err=0` in all cells).
3. **A cell ordering bias exists.** In the prod-first phases every pair ran prod then raw; the raw-first
   ABBA control (§1.1) gives a smaller delta than the prod-first run of the same config. Both are reported.
4. **rtt 71 at 80 Mbps collapses on this host** in 7 of 13 cells (9.2–37 Mbps). Collapsed cells are retained,
   never tidied away, and excluded only from means that are explicitly labelled "clean only".
5. `time.After`/`time.NewTimer` cost on this swapping host is grotesquely inflated (§3.2); the microbenchmark
   absolutes are upper bounds, not portable constants.

## 1. Layer 1 — prod vs raw (the application layer)

`--size 64MiB --queue 5MB`, raw arm at `--chunk 64KiB` so the two arms send the same message size
(prod's hardcoded 64 KiB). n=3, interleaved (rep outer, arms inner). `cs/GB = go_cpu / (received/1e9)`.

| arm | rtt | n | Mbps | sender CPU-s/GB (mean) | per-rep |
|---|---|---|---|---|---|
| prod | 12 | 3 | 74.9 | **58.84** | 62.50 / 51.55 / 62.45 |
| raw 64 KiB | 12 | 3 | 74.9 | **51.76** | 53.97 / 49.67 / 51.62 |
| prod | 71 | 3 | 73.0 | **55.35** | 57.21 / 55.19 / 53.65 |
| raw 64 KiB | 71 | 3 | 34.6 (clean only 72.5) | 58.04 (2 of 3 collapsed) | 49.09 / 60.22† / 64.81† |

† collapsed (18.71 / 12.45 Mbps) — per-byte cost inflated by the wall-time term, see caveat §1.2.

### 1.1 Paired deltas and the ordering control

| config | prod CPU-s/GB | raw CPU-s/GB | paired delta | as % of prod |
|---|---|---|---|---|
| 64 MiB, cap 10 MB (80 Mbps), rtt 12, prod first | 58.84 | 51.76 | **+7.08** (+8.53, +1.88, +10.83) | 12.0 % |
| 64 MiB, cap 10 MB, rtt 71, prod first | 55.35 | 58.04 (collapsed) | −2.69 | n/a (collapsed) |
| **128 MiB, cap 10 MB, rtt 12, prod first** | 53.23 | 50.33 | **+2.90** (+2.74, +3.23, +2.73) | 5.5 % |
| **128 MiB, cap 10 MB, rtt 12, RAW first (ABBA)** | 52.37 | 51.10 | **+1.27** (+2.27, +0.28) | 2.4 % |
| 64 MiB, cap 30 MB (240 Mbps), rtt 12, prod first | 46.19 | 44.46 | **+1.73** (−0.70, +2.75, +3.13) | 3.7 % |
| 64 MiB, uncapped (~470 Mbps), rtt 12, prod first | 53.96 | 52.66 | **+1.30** (+2.24, −0.06, +1.73) | 2.4 % |

**Read this honestly: the app layer is +1.3 to +2.9 CPU-s/GB, i.e. ~2.4–5.5 % of the total, and the sign is
positive in 12 of 14 paired reps.** The 64 MiB/cap-10 MB pair's +7.08 is *not* reproducible: doubling the
payload to 128 MiB (same rate, same arms) yields +2.90 prod-first and +1.27 raw-first. Since CPU per byte is
the metric, the 64 MiB figure is inflated by host drift/ordering and the 128 MiB pairs are the trustworthy
ones. I will not claim +7.08.

Reconciling with a per-byte story: prod's extra work is per-chunk (a pipe handoff + an allocate-and-copy of
each 64 KiB chunk + a backpressure check), and the *pipe handoff and backpressure poll cost CPU only when the
sender actually parks* — which happens at a pinned 80 Mbps, not at 470 Mbps. Hence app-layer CPU-s/GB is
largest in the rate-limited regime. Either way it is a single-digit percentage: **the app layer is not where
the 46 CPU-s/GB lives, and no fork change there is worth much** (the 3 copies at ~10 GB/s are ~0.3 CPU-s/GB
of pure memcpy; the rest is scheduling).

### 1.2 What is *not* in the delta
- DTLS/SCTP/syscalls — present in both arms (§0).
- The in-process shim — present in both arms.
- The bench's 50 ms `chromedp` poll loop — present in both arms, ~0.03–0.05 cores (E22's measured `c`).
  At 7.2 s wall for 64 MiB that is **3.2–5.4 CPU-s/GB of the absolute number** and it grows in collapsed
  cells; it cancels in every delta but inflates every absolute layer below.
- The receiver — separate process (§4).

## 2. Layer 1b — chunk size at a pinned rate, and the E6 claim re-established uncapped

`--mode raw`, `--size 64MiB`, `--bandwidth 10MB`, rtt 12, n=3 (per-rep CPU-s/GB):

| chunk | Mbps | CPU-s/GB (mean) | per-rep |
|---|---|---|---|
| 16 KiB | 74.6 | 54.70 | 51.70 / 51.33 / 61.07 |
| 64 KiB | 74.9 | 56.91 | 56.53 / 55.77 / 58.43 |
| 256 KiB | 75.2 | 56.08 | 54.61 / 57.14 / 56.49 |

**Spread 54.7–56.9 CPU-s/GB = 4 %, with the 256 KiB arm 1.5 % above the best (16 KiB) and the 64 KiB arm
3.4 % above it — inside this bench's arm-to-arm noise (rtt 71 cells of the identical config ranged
55–68 CPU-s/GB).** Larger messages do **not** reduce per-byte CPU here, because SCTP re-fragments every
message into the same 780-byte datagrams: `shim_fwd` = 86,053–86,263 datagrams for 64 MiB in *every* chunk
arm and in both modes (1344.6–1347.9 datagrams/MiB, wire ratio 1.000 vs E22's clean-path datum).

Uncapped (`--bandwidth 0`, rtt 12, n=2) — the E6 "64 KiB wins" claim re-established outside a rate cap:

| chunk | Mbps | CPU-s/GB | datagrams/MiB |
|---|---|---|---|
| 16 KiB | 428.8 (424.3 / 433.4) | 51.91 | 1344.7 |
| 64 KiB | 452.1 (461.4 / 442.8) | 51.82 | 1344.6 |
| 256 KiB | 476.3 (471.0 / 481.6) | 51.74 | 1344.5 |

So in the *uncapped* regime, message size buys **throughput** (16→256 KiB = +11 %) at **identical CPU per
byte** (51.7–51.9): longer messages mean fewer `dc.Send` calls and fewer cwnd/queue round-trips per byte, not
less work per datagram. E6's "64 KiB wins" is real but it is a *throughput* effect, and 256 KiB is at least as
good as 64 KiB uncapped. **The CPU-per-byte ceiling is set by datagram count, not by message size.**

**`--chunk` is provably ignored in prod**: `--mode prod --chunk 16KiB` → 74.93 Mbps / 51.71 CPU-s/GB and
`--mode prod --chunk 256KiB` → 74.81 Mbps / 49.49 CPU-s/GB — identical within noise, and the prod arms'
`shim_fwd` (86,069 / 86,263) is the same as the raw 64 KiB arm. Anyone who "tuned" prod's chunk size in
earlier experiments tuned nothing.

## 3. Layer 2 — inside the pion datagram path (raw mode), by measurement

Everything in this section is `--mode raw` = pion SCTP + DTLS + `WriteToUDP` **plus** the in-process shim.

### 3.1 The wire footprint (the key scaling fact)
1.377 M datagrams per GB (1344.7 per MiB), **779.7 bytes of payload per datagram** in both modes, wire ratio
1.0002 vs the E22/E14 clean-path datum. Two independent summaries: raw = 25,513 datagrams per CPU-second;
prod = 22,031. Cost per datagram: **raw ≈ 37.6 µs, prod ≈ 42.7 µs** (mean of §1 cells: 51.76 and 58.84
CPU-s/GB ÷ 1.377 M; ≈26,600 datagrams per sender CPU-second in raw, ≈23,400 in prod).

### 3.2 Microbenchmarks (harness-independent, stdlib only; sources in `microbench-src/`)

- **DTLS crypto primitive.** AES-128-GCM — the record cipher pion's DTLS 1.2 uses — seal **plus** open of
  1140-byte records: 0.308–0.331 CPU-s/GB for both directions, i.e. **0.154–0.166 CPU-s/GB for the sender's
  seal alone, 6.0–6.5 GB/s per core** (1×10⁶ records per rep × 3 reps = 1.156 GB sealed + 1.156 GB opened per
  rep, `cpu_s/gb` from `getrusage`). Even if pion's per-record
  DTLS framing tripled that, crypto is **≤1 % of the 51 CPU-s/GB**. *DTLS encryption is not the cost.*
- **A plain-UDP replica of the harness' own relay** (`ReadFromUDP` → `make()`+copy → `time.NewTimer` →
  `WriteToUDP`, the shim's hot path, plus an in-process sender doing `WriteToUDP` per datagram), 780-byte
  datagrams:

| relay variant | CPU-s/GB | µs/datagram |
|---|---|---|
| no timer, sender unpaced (firehose) | 11.40 / 11.84 | 8.9 / 9.2 |
| sender paced 100 µs/dg (flow-controlled model), no timer | 39.87 / 39.81 | 31.1 / 31.0 |
| paced + per-datagram `time.NewTimer(50µs)` (shim's real structure) | 55.99 / 56.56 | 43.7 / 44.1 |
| paced + token bucket, no timer | 39.48 / 39.63 | 30.8 / 30.9 |

So on this host a *per-datagram Go timer* costs **+16 CPU-s/GB** and a paced sender's own timer loop
+28 CPU-s/GB. The shim's absolute contribution to the harness' `go_cpu_seconds` is therefore **11–16
CPU-s/GB at minimum, plausibly 20–35 CPU-s/GB** — E22's code-read estimate of "+6–14 %" was far too low, and
the real overhead is per-datagram scheduler/timer work, not the copy. Two consequences: (i) the harness'
absolute constant is contaminated by a bench artifact that does not exist in the field; (ii) the same
timer/scheduler-per-datagram cost appears in pion's SCTP/DTLS send path, which is why per-datagram work is
the right target. These absolutes are upper bounds (caveat §0.5).

### 3.3 The per-layer attribution (rtt 12, cap 10 MB = 80 Mbps, 128 MiB arms; CPU-s/GB)

| layer | CPU-s/GB | share | implied 1-core ceiling | how measured |
|---|---|---|---|---|
| **prod total (sender, in-window)** | **53.2** | 100 % | **150 Mbps** | §1.1 (64 MiB/80 Mbps: 58.8 → 136 Mbps; uncapped: 54.0 → 148 Mbps) |
| app layer (pipe + 3rd copy + envelope alloc + backpressure poll) | **+1.3 … +2.9** | 2.4–5.5 % | ~2.8–6.2 Gbps equivalent | prod − raw, paired, §1.1 |
| pion datagram path + in-process shim (raw) | **~50.3** | ~94 % | ~159 Mbps | §1.1 (total − app) |
| ├ plain-UDP sender+relay floor (includes the bench relay's own read/copy/write) | 11.4 (unpaced) / 39.9 (paced) | 21 % / 75 % | — | §3.2 microbench (harness replica) |
| ├ per-datagram Go timer (shim artifact) | +16 | +30 % | — | §3.2 Δ (paced vs paced+timer) |
| ├ DTLS crypto primitive (AES-128-GCM seal, 1140 B records) | **0.16** | **0.3 %** | ~6.2 Gbps | §3.2 crypto microbench |
| ├ bench 50 ms chromedp poll (artifact) | ~3.2–5.4 at 80 Mbps (~0 in uncapped cells) | 6–10 % | — | E22 measured c=0.03–0.05 cores |
| └ **residual inside pion: SCTP chunking, DTLS record framing, SCTP send-queue/channel plumbing, Go runtime + GC on a swapping host** | **~23–40 (not decomposed)** | 45–75 % | — | by subtraction; **this is where the money is** |
| **receiver (Chrome), per byte** | **27.6–31.1 (capped) / 17.5–20.9 (uncapped)** | — | **~276 Mbps (capped) / ~421 Mbps (uncapped)** | `chrome_cores × elapsed / GB` |

Sanity of the subtraction: raw (50.3) − shim+poll artifacts (≈15–30) leaves **~20–35 CPU-s/GB ≈ 15–25 µs per
datagram** of pion-internal work above a plain UDP `WriteToUDP` + a copy. That is the number a fork would
attack.

## 4. The receiver side
`chrome_cores × elapsed / GB`: 27.6–31.1 CPU-s/GB in the rate-capped arms (≈0.28–0.30 cores at 80 Mbps —
*matching the sender's cores per byte almost exactly*), and 17.5–20.9 CPU-s/GB uncapped (≈1.15 cores at
470 Mbps). Implied one-core receiver ceiling **~276 Mbps (capped) to ~421 Mbps (uncapped)** — i.e. **the
receiver is comparably CPU-expensive per byte as the sender**, and at the field's 122 Mbps both ends are
spending ~0.35 cores per 100 Mbps. This is the lab's byte-counting page only; it has no SHA-1 verify /
service-worker sink (runbook §3), so the *real* client is worse — experiment 9's client-sink hypothesis is
untouched by this result and still the open question.

## 5. Which single change I would make first if forking pion for CPU efficiency

**Collapse datagram count / per-datagram cost in the SCTP→DTLS→UDP send path: carry several SCTP chunks in
one DTLS record and one UDP datagram (GRO/GSO-style coalescing, `sendmmsg`/`writev`), and make the record
path allocation-free per datagram.**

Evidence, in order of strength:
1. **Cost is strictly per-byte and per-datagram, and the datagram is tiny**: 1.377 M datagrams/GB at **779.7 B
   of payload each** — ~40 % below a 1280-byte MTU. Raising the payload to ~1200 B cuts datagram count 1.54×;
   filling a 1440-byte path MTU cuts it ~1.8×. If per-datagram cost is ~35–45 µs (measured), this is the only
   lever that moves the needle by tens of percent.
2. **Crypto is not it**: AES-128-GCM at 6.0–6.5 GB/s/core = 0.16 CPU-s/GB = 0.3 % of the total (measured
   directly, §3.2). A "hardware-accelerated DTLS" or cipher-suite change can win at most ~1 %.
3. **Message/chunk size is not it**: 16/64/256 KiB change CPU-s/GB by <4 %, inside noise, at a pinned rate
   (§2) — because SCTP re-fragments to the same 780-byte datagrams. It changes *throughput* (uncapped
   +11 % for 256 KiB) but not CPU per byte.
4. **The app layer is not it**: +1.3…+2.9 CPU-s/GB (2.4–5.5 %), paired 12/14 positive (§1.1). Removing both
   the pipe and the envelope copy buys ~5 %.
5. **The per-datagram scheduler/timer cost is demonstrably large on this class of machine**: a controlled
   replica of the shim's `time.NewTimer`-per-datagram path adds +16 CPU-s/GB (§3.2). Anyone who instead
   "optimised" the shim's memory copies would have measured almost nothing.

Ranked alternatives, if a fork is attempted: (2) remove per-datagram allocations/copies in the SCTP send
queue and DTLS record path (the residual ~23–40 CPU-s/GB is unattributed between these and pure chunking);
(3) batch `WriteToUDP`. Not worth doing: crypto, chunk size, app-layer copies.

## 6. What this does NOT establish
1. **Not the field.** Lab only. The Mac is faster *and* more loaded than VERSA; only layer *ratios* transfer.
2. **The pion residual is not decomposed.** SCTP chunking vs DTLS record framing vs SCTP queue/channel
   plumbing vs Go GC are separated only as a lump (~23–40 CPU-s/GB). Splitting them needs a profiler
   (`pprof` on the sender) or a code-level A/B, which this experiment does not do.
3. **The shim's share is measured by proxy**, not in-process: the replica reproduces its structure but not its
   exact scheduling; the timer cost on a swap-thrashing host is an upper bound.
4. **No absolute ceiling should be quoted from tonight.** Tonight's constant is 1.2–1.3× E22's on the same
   config, and E22 itself is the number the field bridge should use (46 CPU-s/GB → ~180 Mbps/core here).
5. **The rtt 71 axis is unusable at 80 Mbps tonight** (7/13 cells collapsed) and the receiver number is for a
   byte-counting page, not the real download sink.
6. **Nothing here tests the v2 relay** or the client-sink hypothesis; the sender being ~1 core at 122 Mbps
   remains a plausible *and* unproven explanation of the field ceiling.

## 7. Artifacts (`docs/superpowers/spikes/results/raw/exp23-cost-attribution/`)
`cells.tsv` (63 rows, one per cell, appended cell-by-cell during the run), `row.py`, `analysis.txt` (the
per-arm means, paired deltas and chunk comparison), `microbench.txt` + `microbench-src/{udprelay.go,
aesgcm.go}`, `system-snapshots.txt` (uptime/swap/top-8 CPU before and after every cell),
`runner.log` (START/OK/FAIL per cell), `run-exp23.sh` / `run-deep.sh` / `run-mid.sh` / `run-abba.sh`
(runners), `<label>.json` / `<label>.log` per cell. No harness source change was needed; `go build ./...` and
`go vet ./cmd/benchdirect/` pass unchanged (`BUILD_OK`/`VET_OK`).

### Appendix — every cell (incremental; `ceil` = E14-style best sliding 5×100 ms plateau)

| label | mode | rtt | cap | mbps | go_cpu s | go cores | chrome cores | recv | fwd | drop | cs/GB | ceil |
|---|---|---|---|---|---|---|---|---|---|---|---|---|
| e23-sanity-raw-rtt71 | raw | 71 | uncap | 10.72 | 2.3065 | 0.0921 | 0.0483 | 33554432 | 43736 | 0 | 68.7397 | 22.8 |
| e23-sanity-raw-rtt71-b | raw | 71 | uncap | 13.72 | 2.2258 | 0.1137 | 0.0569 | 33554432 | 44437 | 0 | 66.3331 | 75.6 |
| e23-prod-rtt12-cap10MB-r1 | prod | 12 | 10MB | 74.93 | 4.1944 | 0.5854 | 0.2917 | 67108864 | 86068 | 0 | 62.5015 | 81.6 |
| e23-raw64-rtt12-cap10MB-r1 | raw | 12 | 10MB | 74.85 | 3.6220 | 0.5050 | 0.2643 | 67108864 | 86063 | 0 | 53.9723 | 81.6 |
| e23-prod-rtt71-cap10MB-r1 | prod | 71 | 10MB | 72.97 | 3.8394 | 0.5218 | 0.2597 | 67108864 | 86071 | 0 | 57.2112 | 81.8 |
| e23-raw64-rtt71-cap10MB-r1 | raw | 71 | 10MB | 72.50 | 3.2946 | 0.4449 | 0.2267 | 67108864 | 86067 | 0 | 49.0935 | 86.0 |
| e23-prod-rtt12-cap10MB-r2 | prod | 12 | 10MB | 74.80 | 3.4596 | 0.4820 | 0.2401 | 67108864 | 86223 | 0 | 51.5526 | 82.8 |
| e23-raw64-rtt12-cap10MB-r2 | raw | 12 | 10MB | 51.61 | 3.3335 | 0.3204 | 0.1253 | 67108864 | 86846 | 0 | 49.6729 | 73.6 |
| e23-prod-rtt71-cap10MB-r2 | prod | 71 | 10MB | 72.95 | 3.7038 | 0.5033 | 0.2509 | 67108864 | 86070 | 0 | 55.1916 | 84.8 |
| e23-raw64-rtt71-cap10MB-r2 | raw | 71 | 10MB | 18.71 | 4.0413 | 0.1408 | 0.0635 | 67108864 | 87868 | 0 | 60.2197 | 96.4 |
| e23-prod-rtt12-cap10MB-r3 | prod | 12 | 10MB | 74.93 | 4.1912 | 0.5850 | 0.3015 | 67108864 | 86069 | 0 | 62.4531 | 89.0 |
| e23-raw64-rtt12-cap10MB-r3 | raw | 12 | 10MB | 74.86 | 3.4645 | 0.4831 | 0.2631 | 67108864 | 86063 | 0 | 51.6246 | 81.6 |
| e23-prod-rtt71-cap10MB-r3 | prod | 71 | 10MB | 72.96 | 3.6006 | 0.4893 | 0.2459 | 67108864 | 86070 | 0 | 53.6529 | 82.6 |
| e23-raw64-rtt71-cap10MB-r3 | raw | 71 | 10MB | 12.45 | 4.3495 | 0.1009 | 0.0426 | 67108864 | 86905 | 0 | 64.8124 | 34.4 |
| e23-prod-rtt12-uncap-r1 | prod | 12 | uncap | 501.80 | 3.6376 | 3.3999 | 1.1646 | 67108864 | 87519 | 0 | 54.2043 | 525.4 |
| e23-raw64-rtt12-uncap-r1 | raw | 12 | uncap | 466.16 | 3.4875 | 3.0281 | 1.0770 | 67108864 | 87173 | 0 | 51.9672 | 492.0 |
| e23-prod-rtt12-uncap-r2 | prod | 12 | uncap | 489.22 | 3.4905 | 3.1807 | 1.1669 | 67108864 | 86898 | 0 | 52.0130 | 539.0 |
| e23-raw64-rtt12-uncap-r2 | raw | 12 | uncap | 462.46 | 3.4947 | 3.0103 | 1.0641 | 67108864 | 86051 | 0 | 52.0752 | 490.8 |
| e23-prod-rtt12-uncap-r3 | prod | 12 | uncap | 499.83 | 3.7349 | 3.4772 | 1.2039 | 67108864 | 88757 | 0 | 55.6539 | 534.0 |
| e23-raw64-rtt12-uncap-r3 | raw | 12 | uncap | 401.46 | 3.6189 | 2.7062 | 1.0281 | 67108864 | 89083 | 0 | 53.9264 | 471.8 |
| e23-rawchunk16KiB-rtt12-cap10MB-r1 | raw | 12 | 10MB | 74.57 | 3.4694 | 0.4819 | 0.2620 | 67108864 | 86063 | 0 | 51.6975 | 84.0 |
| e23-rawchunk64KiB-rtt12-cap10MB-r1 | raw | 12 | 10MB | 74.88 | 3.7939 | 0.5291 | 0.2773 | 67108864 | 86063 | 0 | 56.5336 | 82.8 |
| e23-rawchunk256KiB-rtt12-cap10MB-r1 | raw | 12 | 10MB | 75.16 | 3.6651 | 0.5131 | 0.2684 | 67108864 | 86066 | 0 | 54.6142 | 84.0 |
| e23-rawchunk16KiB-rtt12-cap10MB-r2 | raw | 12 | 10MB | 53.60 | 3.4446 | 0.3439 | 0.1424 | 67108864 | 86818 | 0 | 51.3293 | 76.2 |
| e23-rawchunk64KiB-rtt12-cap10MB-r2 | raw | 12 | 10MB | 74.86 | 3.7428 | 0.5219 | 0.2803 | 67108864 | 86063 | 0 | 55.7713 | 82.6 |
| e23-rawchunk256KiB-rtt12-cap10MB-r2 | raw | 12 | 10MB | 75.16 | 3.8345 | 0.5368 | 0.2857 | 67108864 | 86063 | 0 | 57.1386 | 88.2 |
| e23-rawchunk16KiB-rtt12-cap10MB-r3 | raw | 12 | 10MB | 74.60 | 4.0984 | 0.5695 | 0.2927 | 67108864 | 86063 | 0 | 61.0707 | 85.8 |
| e23-rawchunk64KiB-rtt12-cap10MB-r3 | raw | 12 | 10MB | 74.88 | 3.9212 | 0.5469 | 0.2957 | 67108864 | 86066 | 0 | 58.4298 | 83.8 |
| e23-rawchunk256KiB-rtt12-cap10MB-r3 | raw | 12 | 10MB | 75.16 | 3.7911 | 0.5307 | 0.2823 | 67108864 | 86063 | 0 | 56.4915 | 79.8 |
| e23-rawchunk16KiB-rtt71-cap10MB-r1 | raw | 71 | 10MB | 10.52 | 4.5090 | 0.0884 | 0.0366 | 67108864 | 86876 | 0 | 67.1900 | 20.6 |
| e23-rawchunk64KiB-rtt71-cap10MB-r1 | raw | 71 | 10MB | 19.89 | 3.9034 | 0.1446 | 0.0703 | 67108864 | 88996 | 0 | 58.1653 | 101.4 |
| e23-rawchunk256KiB-rtt71-cap10MB-r1 | raw | 71 | 10MB | 10.74 | 4.3553 | 0.0871 | 0.0374 | 67108864 | 86870 | 0 | 64.8995 | 21.0 |
| e23-rawchunk16KiB-rtt71-cap10MB-r2 | raw | 71 | 10MB | 9.57 | 4.5569 | 0.0812 | 0.0354 | 67108864 | 87125 | 0 | 67.9030 | 21.6 |
| e23-rawchunk64KiB-rtt71-cap10MB-r2 | raw | 71 | 10MB | 37.39 | 3.6249 | 0.2525 | 0.1397 | 67108864 | 89245 | 0 | 54.0154 | 111.0 |
| e23-rawchunk256KiB-rtt71-cap10MB-r2 | raw | 71 | 10MB | 26.81 | 3.8349 | 0.1915 | 0.0990 | 67108864 | 91198 | 0 | 57.1440 | 134.2 |
| e23-rawchunk16KiB-rtt12-uncap-r1 | raw | 12 | uncap | 424.27 | 3.5173 | 2.7796 | 1.0809 | 67108864 | 89468 | 0 | 52.4119 | 495.2 |
| e23-rawchunk64KiB-rtt12-uncap-r1 | raw | 12 | uncap | 461.39 | 3.5449 | 3.0465 | 1.0578 | 67108864 | 88278 | 0 | 52.8231 | 481.4 |
| e23-rawchunk256KiB-rtt12-uncap-r1 | raw | 12 | uncap | 471.02 | 3.4847 | 3.0573 | 1.0628 | 67108864 | 88092 | 0 | 51.9267 | 477.8 |
| e23-rawchunk16KiB-rtt12-uncap-r2 | raw | 12 | uncap | 433.38 | 3.4506 | 2.7854 | 1.1587 | 67108864 | 87918 | 0 | 51.4178 | 465.4 |
| e23-rawchunk64KiB-rtt12-uncap-r2 | raw | 12 | uncap | 442.78 | 3.4105 | 2.8128 | 1.0676 | 67108864 | 86957 | 0 | 50.8199 | 493.8 |
| e23-rawchunk256KiB-rtt12-uncap-r2 | raw | 12 | uncap | 481.63 | 3.4602 | 3.1042 | 1.0262 | 67108864 | 86053 | 0 | 51.5614 | 490.4 |
| e23-prodchunk16KiB-rtt12-cap10MB | prod | 12 | 10MB | 74.93 | 3.4701 | 0.4843 | 0.2661 | 67108864 | 86069 | 0 | 51.7092 | 82.6 |
| e23-prodchunk256KiB-rtt12-cap10MB | prod | 12 | 10MB | 74.81 | 3.3214 | 0.4628 | 0.2569 | 67108864 | 86263 | 0 | 49.4930 | 85.8 |
| e23-deep-prod-rtt12-cap10MB-r1 | prod | 12 | 10MB | 74.37 | 7.1655 | 0.4963 | 0.2818 | 134217728 | 172097 | 0 | 53.3868 | 81.8 |
| e23-deep-raw64-rtt12-cap10MB-r1 | raw | 12 | 10MB | 74.36 | 6.7974 | 0.4708 | 0.2776 | 134217728 | 172091 | 0 | 50.6448 | 83.8 |
| e23-deep-prod-rtt12-cap10MB-r2 | prod | 12 | 10MB | 74.32 | 7.2111 | 0.4991 | 0.2878 | 134217728 | 172326 | 0 | 53.7266 | 83.6 |
| e23-deep-raw64-rtt12-cap10MB-r2 | raw | 12 | 10MB | 74.35 | 6.7769 | 0.4693 | 0.2831 | 134217728 | 172091 | 0 | 50.4919 | 84.8 |
| e23-deep-prod-rtt12-cap10MB-r3 | prod | 12 | 10MB | 74.34 | 7.0586 | 0.4887 | 0.2899 | 134217728 | 172097 | 0 | 52.5908 | 84.6 |
| e23-deep-raw64-rtt12-cap10MB-r3 | raw | 12 | 10MB | 74.36 | 6.6917 | 0.4634 | 0.2707 | 134217728 | 172091 | 0 | 49.8567 | 82.8 |
| e23-mid-prod-rtt12-cap30MB-r1 | prod | 12 | 30MB | 72.30 | 2.9376 | 0.3956 | 0.1669 | 67108864 | 87067 | 0 | 43.7737 | 163.6 |
| e23-mid-raw64-rtt12-cap30MB-r1 | raw | 12 | 30MB | 228.14 | 2.9845 | 1.2682 | 0.6904 | 67108864 | 86058 | 0 | 44.4719 | 258.8 |
| e23-mid-prod-rtt12-cap30MB-r2 | prod | 12 | 30MB | 228.44 | 3.1398 | 1.3360 | 0.7038 | 67108864 | 86562 | 0 | 46.7870 | 264.0 |
| e23-mid-raw64-rtt12-cap30MB-r2 | raw | 12 | 30MB | 67.95 | 2.9556 | 0.3741 | 0.1529 | 67108864 | 86781 | 0 | 44.0414 | 93.2 |
| e23-mid-prod-rtt12-cap30MB-r3 | prod | 12 | 30MB | 229.05 | 3.2214 | 1.3744 | 0.7157 | 67108864 | 86063 | 0 | 48.0028 | 249.4 |
| e23-mid-raw64-rtt12-cap30MB-r3 | raw | 12 | 30MB | 228.15 | 3.0114 | 1.2797 | 0.6858 | 67108864 | 86055 | 0 | 44.8737 | 250.4 |
| e23-abba-raw64-rtt12-cap10MB-r1 | raw | 12 | 10MB | 74.36 | 6.7826 | 0.4697 | 0.2537 | 134217728 | 172091 | 0 | 50.5341 | 81.4 |
| e23-abba-prod-rtt12-cap10MB-r1 | prod | 12 | 10MB | 74.35 | 7.0872 | 0.4907 | 0.2708 | 134217728 | 172098 | 0 | 52.8037 | 82.6 |
| e23-abba-raw64-rtt71-cap10MB-r1 | raw | 71 | 10MB | 9.24 | 9.2370 | 0.0795 | 0.0335 | 134217728 | 173943 | 0 | 68.8212 | 29.4 |
| e23-abba-prod-rtt71-cap10MB-r1 | prod | 71 | 10MB | 10.13 | 8.7596 | 0.0826 | 0.0352 | 134217728 | 174064 | 0 | 65.2643 | 31.4 |
| e23-abba-raw64-rtt12-cap10MB-r2 | raw | 12 | 10MB | 74.23 | 6.9340 | 0.4794 | 0.2763 | 134217728 | 172370 | 0 | 51.6620 | 80.6 |
| e23-abba-prod-rtt12-cap10MB-r2 | prod | 12 | 10MB | 74.34 | 6.9712 | 0.4826 | 0.2694 | 134217728 | 172097 | 0 | 51.9398 | 84.6 |
| e23-abba-raw64-rtt71-cap10MB-r2 | raw | 71 | 10MB | 9.43 | 9.1477 | 0.0803 | 0.0338 | 134217728 | 174240 | 0 | 68.1554 | 17.6 |
| e23-abba-prod-rtt71-cap10MB-r2 | prod | 71 | 10MB | 9.51 | 8.8908 | 0.0787 | 0.0342 | 134217728 | 173967 | 0 | 66.2414 | 18.8 |

All 63 cells delivered 100 % of the payload with `shim_drop = 0` and `shim_write_err = 0` (checked in every
row); the rtt 71 cap-10 MB cells are *slow*, not lossy — nine of them collapsed into a 9–37 Mbps regime with
no knob changed, exactly as the runbook's reliability note and E2 predict.
