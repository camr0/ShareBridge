# Experiment E26 — sizing the last two pion-fork levers: syscall batching, and sender allocations/GC — 2026-09-18

**Verdict.** Both remaining levers are now sized, and both are **small**.
(i) **Syscall batching is worth at most ~2.1 CPU-s/GB** — measured directly, and it is an *upper bound*
because the macOS proxy removes more kernel work than real `sendmmsg`/GSO can. That is **5.6 % of the
37 CPU-s/GB baseline**, not the "≤10 CPU-s/GB" E24 could only bound.
(ii) **Allocation is ~10.3 % of sender CPU, but GC itself is 0.2–0.5 %** — so the brief's lever (ii) is really
a *buffer-churn* lever, not a GC lever: pion allocates **12.4 bytes for every byte delivered** across ~10
fresh ~1.2 KB buffers per datagram, and the cost is `mallocgc` + `memclr`, not collection.
(iii) **MTU is not reachable without patching pion** (`outboundMTU = 1200` is a package-level const) and is
therefore an **unmeasured −13.9 % estimate**.
**Bounded total for a fork doing all three: ≈28 CPU-s/GB, i.e. −24 % from 37** — and that is the *optimistic*
end, since two of the three contributions are ceilings and the third is an estimate.

## 0. Method, host health, and the standing caveat

**Lab only.** The field rig, containers, VMs, `sb-run` and the production container were not touched, and
**no `git` command was run**. One `benchdirect` process (or one microbench process pair) at a time.

**The §3 sanity gate FAILED, for the third experiment running:**

| check | result | expected |
|---|---|---|
| `--rtt 71 --mode raw --size 32MiB` rep 1 | **9.63 Mbps** | ≈100 Mbps |

Host during the whole session: load average **3.7–4.3**, `vm.swapusage used = 6198.31 M of 7168.00 M`
(**6.06 GB of 7.0 GB swap in use, 969 MB free**), pinned at that value in every `runner.log` block. Per the
runbook and E23/E24's own caveats, this is the same or a worse regime. Consequences, applied consistently
below:

- **Absolute CPU-s/GB here are inflated ~1.5–1.6×** versus the E22 regime (this session's prod cells are
  **56.7 / 57.4 CPU-s/GB** where E23 saw 55–59 and E22 saw ~38–46). Every absolute is therefore quoted as a
  **band**, and cross-host absolute comparisons are avoided.
- **What transfers is shares and ratios** (the profile's `flat%` / `cum%`, and the microbench's
  *within-block* deltas). Those are what the fork decision is made on.
- The microbench cells are self-paced (sender-bound, see §1) and their within-cell spread is **≤4 %**, with a
  same-block **control re-run** of the baseline (A1b) landing 3.8 % above A1 — i.e. the run-to-run drift is
  smaller than the effects reported, except where explicitly marked.

**Harness change (Task B), minimal, disclosed, revertible.** One new file
`agent/cmd/benchdirect/profile.go` (~130 lines) and **three** inserted statements in
`agent/cmd/benchdirect/main.go` (two flag registrations `-cpuprofile`/`-memprofile`, one
`startGoProfiling(...)` call and one `profiler.Stop(...)` call). The instrumentation is **inert unless a
profile path is passed**. `go build ./...` and `go vet ./cmd/benchdirect/` both pass (`BUILD=0 VET=0`).
To revert: delete `profile.go` and the four lines it references.

## 1. Task A — the syscall-batching ceiling, measured

### 1.1 What the platform allows, and the proxy used (load-bearing)

`agent/go.mod` already carries `golang.org/x/{net,sys}` (v0.50.0 / v0.47.0), so both intended routes were
attempted first, and **both are unavailable on this host**:

- **`sendmmsg`**: `golang.org/x/sys/unix@v0.47.0` exports **no `Sendmmsg` symbol at all**; on Linux the
  supported route is `x/net/ipv4.(*PacketConn).WriteBatch`. (Implementation provided and cross-compiled —
  see §1.4 — but not runnable here.)
- **UDP GSO (`UDP_SEGMENT`)**: the macOS SDK defines **no `UDP_SEGMENT`** option (`netinet/udp.h` declares
  only `UDP_NOCKSUM`), so `setsockopt(IPPROTO_UDP, UDP_SEGMENT)` cannot even be attempted.

So the batching ceiling was measured with a **same-bytes/fewer-syscalls proxy**: send the **identical total
datagram volume (200,000 datagrams of 1237 B, = 0.2179 GB of the 918 k-datagrams-per-GB rate)** but issue
one `WriteToUDP` per **K** datagrams. This is an **upper bound** on real batching, because a real
`sendmmsg`/GSO keeps doing genuine per-datagram work in the kernel (`sendto` per message; GSO segment
headers) that the proxy does not. That direction of bias is stated up front and is the reason the numbers
below are a *ceiling*.

`connwrite` is a second, *semantics-preserving* variant: the same one-write-per-datagram rate, but on a
**connected** socket via `Write` instead of an unconnected `WriteToUDP`.

### 1.2 Results

`N = 200,000` datagrams × 1237 B per rep, **n = 3**, loopback, sender CPU from `getrusage(RUSAGE_SELF)`.
`cpu_s/GB payload` is normalised so that **918,000 datagrams = 1 GB**, making it directly comparable to the
37 CPU-s/GB baseline. Source: `raw/exp26-remaining-levers/A*.jsonl`.

| cell | mode | datagrams/syscall | syscalls per GB payload | **CPU-s/GB payload** (mean, min–max) | ns/syscall |
|---|---|---|---|---|---|
| **A1** | `write` (= today's pion) | 1 | 918,000 | **2.40** (2.37–2.46) | 2616 |
| **A2** | `connwrite` | 1 | 918,000 | **1.68** (1.65–1.71) | 1831 |
| A3 | `mmsg` (batch 8) | 8 | 114,750 | **unsupported on darwin** | — |
| A4 | `gso` (batch 8) | 8 | 114,750 | **unsupported on darwin** | — |
| A5 | `writebig` batch 2 | 2 | 459,000 | **1.22** (1.20–1.24) | 2650 |
| A6 | `writebig` batch 4 | 4 | 229,500 | **0.63** (0.63–0.64) | 2755 |
| A7 | `writebig` batch 8 | 8 | 114,750 | **0.33** (0.33–0.34) | 2906 |
| A8 | `writebig` batch 16 | 16 | 57,375 | **0.20** (0.18–0.22) | 3418 |
| A9 | `writebig` batch 32 | 32 | 28,688 | **0.12** (0.10–0.16) | 4186 |
| **A1b** | `write` (control, re-run at the end of the block) | 1 | 918,000 | **2.49** (2.46–2.52) | 2713 |

The machine was **sender-bound** in every cell (wall ≈ CPU: 0.52 s of CPU in 0.53 s of wall for A1), so these
are not receiver-limited artefacts.

### 1.3 The delta, and the noise band

`cpu ≈ a·syscalls + b·bytes` with the bytes held fixed, so the batching saving is read straight off:

- **Marginal syscall cost (measured, not assumed):** regression on batch 1→4,
  `(2.40 − 0.63) / (918000 − 229500)` = **2.57 µs/syscall**; the A1b control gives 2.70 µs/syscall.
  Quoted as **~2.6–2.7 µs per unconnected `WriteToUDP` of 1237 B on this host**.
- **Batching delta, batch ≥ 8: `2.07 CPU-s/GB`** (A1 − A7 = 2.40 − 0.33). Using the A1b control baseline:
  2.16. Using the worst baseline in the spread (2.37) against the worst batched value (0.34): 2.03.
  → **≈2.1 CPU-s/GB, band 2.0–2.2.** The baseline spread (2.37–2.52) and the batched spread (0.33–0.34)
  **do not overlap**, so this delta is ~5× the run-to-run noise, not noise.
- **Ceiling at any batch K ≥ 8 is within 0.2 CPU-s/GB of the K = 8 figure**; going to 32 buys only
  another 0.21. There is nothing beyond batch ~8.
- **As a share of the baseline: 2.1 / 37 = 5.6 %.**
- **Connected-socket delta (free, no batching): `0.72 CPU-s/GB`** (2.40 → 1.68), **1.9 %** of 37. Spreads
  (2.37–2.46 vs 1.65–1.71) do not overlap. This is a semantics-preserving one-line change (connect the
  socket) and is the cheapest of the whole set — but note pion shares one unconnected `PacketConn` across
  ICE candidates (`pion/transport/v3 udp/conn.go:330` → `c.listener.pConn.WriteTo(p, c.rAddr)`), so it may
  not be available without restructuring.

**Statement of scope, as required.** This microbench **isolates the kernel/syscall side only**. It transmits
the real path's datagram count and size to a local receiver and charges `getrusage(RUSAGE_SELF)` of the
sending process. Pion's own per-datagram work — allocations, DTLS record marshal/encrypt, SCTP bookkeeping —
is **entirely absent from these numbers** and is measured separately in Task B. The two are added up, with
their overlap accounted for, only in §4.

### 1.4 Linux implementations (compile-verified, not run here)

Because the production target is Linux, real implementations of both levers are included so the experiment
can be repeated where it matters, without rewriting anything:

- `microbench-src/batch_linux.go` — `sendmmsg` via `x/net/ipv4.(*PacketConn).WriteBatch`, and `UDP_SEGMENT`
  via `unix.SetsockoptInt(fd, IPPROTO_UDP, 103, dg)` followed by a single `WriteToUDP` of `dg*batch` bytes.
- `microbench-src/batch_other.go` — the darwin path, which reports
  `sendmmsg/UDP_SEGMENT not available on this platform (darwin)` rather than silently measuring something else.
- Cross-compiles clean: `GOOS=linux GOARCH=arm64 go build` → exit 0. Darwin builds and runs (exit 0).

**Consequence for the fork decision:** the ~2.1 CPU-s/GB is measured on **macOS loopback**, where
`sendto` is relatively expensive (2.6 µs). **Linux `sendto` on a real NIC is typically cheaper**, and GSO's
own per-segment kernel cost is non-zero, so the Linux figure is likely to be **lower**, not higher. Expect
**~1–2 CPU-s/GB (3–6 % of 37)** in production. It is **not** a 10 %+ lever.

## 2. Task B — sender allocations and GC, profiled in prod mode

### 2.1 Instrumented cells

`--mode prod --rtt 12 --bandwidth 10MB --size 64MiB`, `cpuprofile` + `memprofile`. Two prod reps plus one
raw rep as the app-layer control. The rate cap is used deliberately (runbook: rate-capped cells are the
robust class; uncapped ones are not trustworthy on this host).

| cell | mode | n | Mbps (cap 80) | go_cpu_s | **CPU-s/GB** | alloc bytes/GB payload | objects/GB | GC count/GB | GC pause % of go CPU | GC CPU fraction (Δ) |
|---|---|---|---|---|---|---|---|---|---|---|
| B1-prod-r1 | prod | 1 | 52.13 | 3.806 | **56.7** | **12.49 GB** | 46.2 M | 1490 | **0.274 %** | **0.184 %** |
| B1-prod-r2 | prod | 1 | 47.45 | 3.855 | **57.4** | **12.33 GB** | 45.7 M | 1475 | **0.265 %** | **0.185 %** |
| B2-raw-r1 | raw | 1 | 61.91 | 3.405 | 50.7 | 10.72 GB | 44.0 M | 2235 | 0.448 % | 0.313 % |

Both prod reps agree to **1.3 % on allocation** and **0.03 pp on GC pause** — the memory instrumentation is
reproducible even where throughput is not. Per-data-datagram, prod allocates **13,604 / 13,432 B** in
**50.4 / 49.7 objects** for **1172 B** of application payload: **≈12.4 bytes allocated per byte delivered,
across ~50 objects per datagram**.

### 2.2 Answer to "how much is allocation/GC-related" — two separate numbers

**GC proper is negligible: 0.27 % of sender CPU in pauses, 0.18 % in GC CPU fraction.** 100 GC cycles per
64 MiB is a lot of cycles, but each is cheap and mostly concurrent. **Lever (ii), read as a GC lever, is
dead — this closes E24's open question with a measurement.** (It also eliminates the obvious adjacent
non-lever: raising `GOGC`/`GOMEMLIMIT` cannot recover more than ~0.3 %.)

**Allocation is the real cost, at 10.3 % of sender CPU.** From the CPU profile (`profiles/top-r1.txt`),
total samples 3.12 s over 12.34 s wall (25 % duty cycle — the sender is flow-controlled at the 80 Mbps cap):

| bucket | flat | share of samples | what it is |
|---|---|---|---|
| `syscall.rawsyscalln` | 1.09 s | 34.9 % | all syscalls (see split below) |
| `runtime.kevent` | 0.64 s | 20.5 % | netpoller |
| **`runtime.memclrNoHeapPointers`** | **0.29 s** | **9.3 %** | **zeroing freshly allocated buffers** |
| `runtime.pthread_cond_wait` / `_signal` | 0.45 s | 14.4 % | runtime parking/scheduling |
| `runtime.usleep` | 0.14 s | 4.5 % | |
| **`runtime.mallocgc` (cum)** | **0.32 s** | **10.3 %** | **allocation, incl. the memclr above** |
| `runtime.tryDeferToSpanScan`, `mspan.init`, `nextFreeFast` | 0.14 s | 4.5 % | allocator bookkeeping |

Syscall split (cum): pion's own data write (`candidateBase.writeTo` → `UDPConn.WriteTo`) **0.34 s = 10.9 %**;
the **harness shim's** write (`Shaper.drainLoop` → `WriteToUDP`) **0.40 s = 12.8 %**; `recvfrom` (shim read +
pion read) **0.31 s = 9.9 %**. Note the shim is in-process and accounts for ~13 % of sender CPU and ~11 % of
allocated bytes — **the field has no shim**, so both are harness overhead the field does not pay. This is
consistent with E22's 46 → 37 correction and with E24's warning that the lab lacks the field's no-shim path.

**Allocation attribution by bytes** (`profiles/allocspace-r1.txt`, 812 MB total per 64 MiB delivered = the
12.4× above):

| site | cum | share | note |
|---|---|---|---|
| `sctp.(*Association).writeLoop` | 493.6 MB | 60.8 % | pion SCTP send path |
| `dtls.(*Conn).Write` | 257.3 MB | 31.7 % | pion DTLS send path |
| `transfer.(*Manager).streamFile` | 145.6 MB | 17.9 % | app layer |
| `main.(*Shaper).readLoop` | 85.9 MB | 10.6 % | **harness/shim only** |
| `transfer.encodeChunkFrame` | 67.0 MB | 8.3 % | app layer (fresh 65552-byte buffer per 64 KiB) |

The **flat** hot spots are the smoking gun — seven or eight independent ~1.2 KB-per-datagram marshal buffers
per datagram, each ≈ 78 MB per 64 MiB: `sctp.(*packet).marshal` 78.6 MB, `dtls recordlayer.(*RecordLayer).Marshal`
80.1 MB, `sctp.(*chunkHeader).marshal` 75.1 MB, `sctp.(*chunkPayloadData).marshal` 69.6 MB,
`dtls.(*ApplicationData).Marshal` 72.6 MB, `dtls aead.encrypt` 81.1 MB, `sctp.(*Stream).packetize` 78.6 MB.
**Object counts** per 64 MiB: `netctx.(*packetConn).WriteToContext` 300,774 (**5.2 objects per datagram**),
`(*packet).marshal` 291,120 (**5.1/datagram**), `handleSack` 185,688 (the *receive* path allocates for SACKs),
`movePendingDataChunkToInflightQueue` 176,134. (Also visible: `strings.genSplit` 131,186 objects from
`chromeCPUSeconds()` parsing `ps` every 100 ms — pure harness.)

This is the classic pion "marshal into a fresh `[]byte` at every layer" design, and it is **the** thing a fork
can attack: **~10.3 % of sender CPU, ≈3.8 CPU-s/GB at the 37 baseline**, of which the app layer is 14–26 % of
the bytes (raw-vs-prod mode difference 14 %; profile tree 26 % — the two disagree because prod's longer wall
time admits more GC and more backpressure-poll iterations; both are reported rather than picked between).

**Ceiling, stated as a ceiling:** removing *all* allocation is not achievable (DTLS ciphertext and SCTP
chunk bodies must live somewhere), so 3.8 CPU-s/GB is an upper bound, not a forecast. The realistic change is
to stop *re-allocating the same 1.2 KB* at 8 layers — i.e. buffer reuse/pooling — which should capture a
large fraction of the mallocgc+memclr share but will not reach zero.

## 3. Task C — MTU: **not reachable, therefore an estimate**

`outboundMTU` is a **package-level const**, not a `SettingEngine` knob:

```
github.com/pion/webrtc/v4@v4.2.11/constants.go:41:   outboundMTU = 1200
github.com/pion/webrtc/v4@v4.2.11/sctptransport.go:164:   sctp.WithMTU(outboundMTU),
```

There is no exported setter (`grep outboundMTU` finds only these two sites plus tests). **Raising it requires
patching pion** — so per the task instruction this is reported as an **estimate only, and is labelled
unmeasured wherever it appears.**

**Estimate:** 1237 → ~1437 wire bytes per data datagram at the same 1200→1400 SCTP MTU step
(`1200 + 37 DTLS` → `1400 + 37`), i.e. **−13.9 % datagrams per GB** (918 k → ~790 k). Only the
**per-datagram** component of the cost scales; the per-byte, poller and runtime-parking components do not.
At the profile's ~70 % per-datagram share this is **≈−3.0 CPU-s/GB**, but with a wide plausible range
(−2 to −5) that **this experiment cannot narrow**, because the lever cannot be exercised without a pion patch.

Two risks that are *not* costed here: path-MTU black-holing above 1200 is exactly why pion hard-codes 1200,
and a fork that raises it needs PMTU probing/DPLPMTUD it does not have.

## 4. Bounded total for a fork doing all three

Applied to the **37 CPU-s/GB production-representative baseline**, in order, using only measured shares and
explicitly flagging the estimate. (The three contributions are treated as additive; they are nearly
disjoint — batching removes syscalls, pooling removes `mallocgc`/`memclr`, MTU removes datagrams — but MTU
does *shrink the other two*, which is why it is applied last and mildly discounted.)

| step | lever | Δ CPU-s/GB | basis | confidence |
|---|---|---|---|---|
| 0 | baseline | **37** (band 32–43) | E22/E23 | measured (E22/E23) |
| 1 | **allocation / buffer pooling** | **−3.8** | 10.3 % of sender CPU, profile share | measured **share**; ceiling (cannot reach 0) |
| 2 | **syscall batching** (batch ≥ 8) | **−2.1** | microbench A1−A7, 2.0–2.2 band | measured **ceiling** (proxy over-credits) |
| 3 | **MTU 1200 → ~1400** | **−3.0** | −13.9 % datagrams × 70 % per-datagram share | **ESTIMATE ONLY — unmeasured** |
| | **best case** | **≈28 CPU-s/GB** | | **−24 %** |

**Implied one-core ceiling.** Using the brief's own stated pairing (37 CPU-s/GB ⇒ ~85–100 Mbps/core), a
−24 % per-byte cost ⇒ **~112–132 Mbps/core** at the same core count. *This extrapolation is the brief's
linear mapping applied unchanged and is not verified here* — and it is visibly inconsistent with the field's
own measured ~63–113 CPU-s/GB (≈90 CPU-s/GB at 1 core → 89.2 Mbps), which is 2–3× the corrected lab figure.
Read the **−24 %** as the deliverable; read the Mbps figure as the brief's convention, clearly flagged.

**If the MTU lever is dropped** (it needs a pion patch and is unmeasured), the two *measured* levers bound at
**37 − 3.8 − 2.1 = 31.1 CPU-s/GB, −16 %**, and the honest central estimate is below that, since both are
ceilings.

**Still unmeasurable, flagged rather than guessed:**
- **The Linux cost of `sendto`/GSO/`sendmmsg`.** macOS exposes neither syscall, so the batching ceiling is a
  darwin-measured upper bound. The production target is Linux and the number is expected to be **smaller**.
  *This is the one gap that most changes the answer, and it closes by running the included
  `batch_linux.go` on any Linux box.*
- **The MTU effect** (§3) — needs a pion patch, do not cost it without one.
- **Allocation on a clean host.** The 10.3 % is a *share* measured on a host whose memory bandwidth is
  contended by 6.06 GB of swap-in-use; `memclrNoHeapPointers` at 0.29 s for ~812 MB is only ~2.8 GB/s,
  far below a quiet machine's 10–20 GB/s. The share may fall on a clean host; the direction is stated, the
  magnitude is not measured.
- **Anything about the field.** No field run was made (safety: field rig off-limits). The field's 63–113
  CPU-s/GB and the shim-free path are both unaccounted for in the absolute numbers above.

## 5. Per-block host health (method rule)

Every measuring block recorded `uptime` + `vm.swapusage`; full detail in `runner.log`. Summary — load and
swap were **flat** across the whole session, so the bands above are not drift artefacts:

| block | load (1/5/15 min) | swap used / total |
|---|---|---|
| sanity gate (before) | 3.72 / 3.50 / 3.23 | 6198.31 M / 7168.00 M |
| Task A block | 4.01 / 3.78 / 3.37 → 3.69 / 3.72 / 3.36 | 6198.31 M / 7168.00 M (unchanged) |
| Task B block | 4.26 / 3.81 / 3.38 | 6198.31 M / 7168.00 M (unchanged) |

`load > 4` and `swap > 2 GB` **both** triggered, per the method rule: **all absolutes are quoted as bands.**

## 6. Caveats — what this does NOT establish

1. **It does not establish the production saving.** Only shares and ratios transfer off this host; the
   absolute CPU-s/GB here (56.7–57.4 prod) is 1.5–1.6× the E22 regime and ~1.5× the 37 baseline.
2. **Two of three levers are ceilinged, not forecast.** Batching's proxy removes more kernel work than
   `sendmmsg`/GSO will; allocation removal cannot go to zero. The §4 total is therefore **optimistic**.
3. **The profile includes the harness.** The in-process shim contributes ~13 % of CPU and ~11 % of allocated
   bytes and does not exist in the field; `chromeCPUSeconds()`'s `ps` parsing contributes 131 k objects per
   64 MiB. Neither is a pion finding.
4. **GC numbers are for one GC configuration** (default `GOGC=100`), which is the right one to report against
   the shipped agent, but no `GOGC`/`GOMEMLIMIT` sweep was run — moot, since GC is 0.2–0.5 %.
5. **Not tested end-to-end.** No prototype of any of the three levers was built and run through the real
   sender; these are instrumented measurements of the *existing* path plus one isolated microbenchmark. A
   fork's actual delta may differ (in either direction) from the sum of these parts.
6. **Single host, single architecture** (M4 Air, arm64, Go 1.26.1). Nothing here was repeated on Linux.

## 7. Artifacts

All under `/Users/ali/Git/ShareBridge/.worktrees/benchdirect/docs/superpowers/spikes/results/raw/exp26-remaining-levers/`:

- `sanity-r1.json`, `sanity-r1.log`, `sanity-system.txt` — the **failed** sanity gate (9.63 Mbps vs ≈100).
- `runner.log` — per-block `uptime` + `vm.swapusage` for every cell, plus per-cell summary lines.
- Task A: `A1-write*.jsonl` … `A9-writebig-b32*.jsonl`, `A1b-write.jsonl` (control), plus `.log`/`.sink.log`;
  `run-exp26-taskA.sh` (the runner).
- Task A source: `microbench-src/udpbench.go`, `microbench-src/batch_linux.go`,
  `microbench-src/batch_other.go`, `microbench-src/go.mod`.
- Task B: `B1-prod-cap10MB-r{1,2}.json` + `.log`, `B2-raw-cap10MB-r1.json` + `.log`, `taskB.jsonl`
  (the `PROFILE {...}` lines verbatim).
- Task B profiles: `profiles/B1-prod-cap10MB-r{1,2}.cpu.pprof`, `.mem.pprof`,
  `profiles/B2-raw-cap10MB-r1.{cpu,mem}.pprof`, and the pprof text dumps `top-r1.txt`, `topcum-r1.txt`,
  `allocspace-r1.txt`, `allocobj-r1.txt`.
- Harness instrument (in the worktree, not here): `agent/cmd/benchdirect/profile.go` + 4 lines in
  `agent/cmd/benchdirect/main.go`.
