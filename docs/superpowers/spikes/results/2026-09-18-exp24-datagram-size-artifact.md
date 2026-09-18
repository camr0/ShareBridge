# Experiment E24 — is the lab's datagram size an artifact? (settling E23 vs the field) — 2026-09-18

**Verdict. E23's datagram claim does NOT survive, and neither does the fork recommendation built on it.**
The lab's *data* datagrams are **1237 B on the wire (1200 B SCTP = 28 B header + 1172 B app payload, plus a
37 B DTLS record)** — **exactly at pion's configured outbound MTU** — not 780 B and not "~40 % below MTU".
E23's "779.7 B of payload per datagram / 1.377 M datagrams per GB" is a **units error**: it divided
*application payload* by **all** forwarded datagrams, and ~1/3 of those datagrams are 65-byte SCTP SACKs.
Corrected, lab and field agree to within 0.2 %: **896 data datagrams per MiB of payload in both**. The
"collapse datagram count" lever that E23's fork recommendation rests on is therefore worth **~0–2 % at
pion's current MTU** (only the fragment tail), not the 1.54×–1.8× E23 claimed. What remains is a one-line
MTU change (~7–10 % of production CPU-s/GB, and PMTU-risky) and syscall batching (unquantified, bounded
above by ~10 CPU-s/GB, and mostly absent from the field's no-shim path).

## 0. Method, host health, and what was and was not measured

- Host is swap-thrashing (**6.20 GB of 7.17 GB used**, load 2.6–3.5 in every block). Per the runbook and
  E23's own caveat, **uncapped throughput and any absolute per-byte constant are untrustworthy tonight**.
  - **The §3 sanity gate FAILED: `--rtt 71 --mode raw --size 32MiB` gave 8.39 / 9.62 Mbps** against the
    ≈100 Mbps expectation (E23 got 10.72 / 13.72 on the same config, E1 10.6–245.7). The host is in the
    same or worse regime as E23.
  - Everything load-bearing below is either a **rate-capped** cell (deterministic datagram count, per the
    runbook) or a **datagram count / size / structure** fact — the two classes the runbook classifies as
    load-robust. **Numbers that are rate-capped (reproducible) are marked `[cap]`; the one uncapped
    diagnostic is marked `[uncapped]`.**
- Lab only. The field rig, containers and VMs were not touched; **no `git` command was run**; one
  `benchdirect` process at a time.
- Two **temporary, env-gated** harness instruments were added (exact diff in §7). Both are inert unless
  `SB_SHIM_HIST` is set; `go build ./...` and `go vet ./cmd/benchdirect/` pass (`BUILD_OK VET_OK`).

## 1. Task 1 — bytes per datagram, measured directly (two independent instruments)

### 1.1 Instrument A — a datagram-size histogram inside the shim

`shim.go`: a `[65536]atomic.Int64` per direction, incremented in `drainLoop` **only on a successful
forward**, split by direction (`toB` = written towards peer B = the Go/pion side was the *source* ⇒ DATA;
`toA` = towards peer A = the browser was the source ⇒ SACK). Dumped as TSV on shaper close.

Cell `exp24-raw16-r3` `[cap]`: `--mode raw --rtt 12 --bandwidth 10MB --size 64MiB --chunk 16KiB`;
achieved 74.43 Mbps, `shim_drop=0`, `shim_fwd=86198`, 100 % delivered.

| L4 datagram size (B) | direction | count | what it is |
|---|---|---|---|
| **1237** | Go→browser (DATA) | **53,260** | 1172 B app payload + 28 B SCTP + 37 B DTLS |
| **1213** | Go→browser (DATA) | **4,097** | 1148 B app payload — the last fragment of each 16 KiB message |
| **65** | browser→Go (SACK) | **28,551** | 28 B SCTP SACK + 37 B DTLS |
| 69 | browser→Go | 245 | SACK with extra gap-ack / duplicate-TSN blocks |
| 64, 81, 100, 104, 108, 129, 617 | handshake / control | ≤ 20 each | DTLS + SCTP control during setup |
| 48, 53, 75, 77, 85, 97, 113, 263, 283, 547, 1200 | both | ≤ 3 each | setup / teardown residue |
| **TOTALS** | toB = **57,380**, toA = **28,818**, all = **86,198** | | |

**The distribution is strictly bimodal and both modes have exactly predicted sizes.**
The DATA mode is **13:1 for 1237:1213 (measured 53,260:4,097 = 13.00:1)** — precisely the arithmetic of a
16 KiB message at a 1172-byte payload cap: `16384 = 13×1172 + 1148`. The SACK mode is a single spike at
65 B. There are **no** datagrams between 70 B and 1150 B: nothing in this path produces a "780 B payload"
datagram. E23's 779.7 B was an average over the two modes.

### 1.2 Instrument B — pion's own SCTP association state (independent of the shim)

Temporary stderr line in `rawbench.go` reading `pc.SCTP().Stats()` — **pion's internal association object,
not the shim's counters**:

```
exp24 sctp conn=0 mtu=1200 bytes_sent=68730340 bytes_recv=807468 cwnd=424542      (exp24-raw16-r3)
exp24 sctp conn=0 mtu=1200 bytes_sent=68745868 bytes_recv=806924 cwnd=...        (exp24-prod16-r2)
```

- **`mtu=1200`** — `Association.MTU()`, read from the association itself. **Not 1228.** This is the
  measurement that settles §3.
- `bytes_sent = 68,730,340` for 67,108,864 B of payload ⇒ **SCTP-level overhead exactly 2.39 %**, matching
  the model `16 KiB → 16,776 SCTP bytes` (64 × 16,776 = 68,714,496, +0.02 %).
- `68,730,340 / 57,380 data datagrams = 1197.8 B SCTP per data datagram`; the histogram's DATA mode gives
  `(13×1200 + 1×1176)/14 = 1198.3 B`. **Agreement to 0.04 % between the two instruments.**
- Prod's association reports the same `mtu=1200` and the same arithmetic.

### 1.3 The corrected per-datagram figures (64 MiB, `--chunk 16KiB`, clean cells)

| quantity | value | derivation |
|---|---|---|
| **payload per DATA datagram** | **1169.6 B** | 67,108,864 / 57,380 |
| **wire (L4) per DATA datagram** | **1236.6 B** | (53,260×1237 + 4,097×1213) / 57,380 |
| SCTP per DATA datagram | 1197.8 B | pion's `bytes_sent` / count |
| **data datagrams per MiB** | **896.6** | 57,380 / 64 |
| SACK datagrams per MiB | 450.3 | 28,818 / 64 |
| **all datagrams per MiB** | **1346.8** | 86,198 / 64 |
| **data datagrams per GB** | **939,300** (0.939 M) | — |
| **all datagrams per GB** | **1.3468 M** | E23's "1.377 M" is the same quantity within its cell-to-cell scatter |
| payload ÷ **all** datagrams | **778.5 B** | 67,108,864 / 86,198 ← **this is E23's "779.7 B"** |

E23's headline figure is reproduced exactly. E23's own raw cells (86,063–86,263 datagrams for 64 MiB) give
779.6–779.7 B; its prod cells give the same (see §3.1). **E23's metric was payload over data+SACK
datagrams. It was never a datagram size**, and the "~40 % below MTU" reading came from comparing that mixed
average to a *data*-datagram MTU.

### 1.4 Prod is structurally identical (measured, not assumed)

`exp24-prod16-r2` `[cap]` (prod ignores `--chunk`; `transfer.Manager` hardcodes 64 KiB + a 16 B envelope,
so each `dc.Send` is 65,552 B):
`shim fwd=86,961 = toB 57,539 (1237×56,483 + 1157×1,028 + 2×1200) + toA 29,422 (65×27,946 + 69×1,052 + …)`.

- The DATA mode here is **54.8:1 for 1237:1157** — the predicted `65552 = 55×1172 + 1092` ⇒ 55 full
  packets + 1 of 1157 B wire. Measured 56,483:1,028 = 54.96:1.
- **896–899 data datagrams per MiB in prod, 896.6 in raw, 896 in the field.** Same structure, same MTU,
  same fragment arithmetic. This is exactly why `--chunk` provably did nothing in E6/E23: `dc.Send`
  message size cannot change the datagram count, because pion fragments at the association MTU.

## 2. Task 2 — why the "780 B" reading appeared, and why there is no field/lab send-path difference

**The 779.7 B is not a datagram size, so there is nothing to explain about it in the send path.** The real
question is the one the brief flagged: *why* does pion fragment a 16 KiB (and a 64 KiB) message into
~1200-byte datagrams instead of MTU-sized ones? Answer, read out of the resolved module source
(`github.com/pion/webrtc/v4 v4.2.11`, `github.com/pion/sctp v1.9.4`):

1. `sctp/association.go:72-73` defines `receiveMTU = 8192` and **`initialMTU = 1228`** ("initial MTU for
   outgoing packets (to DTLS)"). `initialMTU` is only a **fallback**: `association.go:680 mtu := cfg.MTU`,
   and `WithMTU(size)` sets `c.MTU = size` (`association_options.go:87-95`).
2. `webrtc` **always supplies the MTU**: `sctptransport.go:120` calls
   `r.sctpClientOptions(dtlsTransport.conn, maxMessageSize)` from `SCTPTransport.Start()`, and line 164 of
   that function contains `sctp.WithMTU(outboundMTU)` with **`outboundMTU = 1200`** (`constants.go:41`).
3. `SCTPTransport.Start()` has **no server/client variant** — it is the single entry point, called from the
   single `PeerConnection.startSCTP` (`peerconnection.go:1650`, reached from `:2828` for the local
   description), for both the offerer and the answerer. Both pion roles therefore build an association with
   **MTU 1200**.
4. Nothing in the harness's `sdp.go` (it only rewrites the `a=candidate:` line) or in its `SettingEngine`
   touches the MTU, and `SettingEngine` has no SCTP-MTU setter at all — only `SetReceiveMTU`, which sizes
   the UDP read buffer, not the fragment size. Chrome's answer carries no attribute pion reads for MTU.
5. Therefore the effective fragment cap is **1172 B of payload per chunk (1200 − 12 common − 16 DATA
   header)**, i.e. the wire datagram is 1237 B: 94.9 % of the 1280-byte IPv6 minimum MTU and **100 % of
   pion's own MTU**. pion's own stats confirm it: `mtu=1200` (§1.2).

**So there is no field/lab difference in the send path at all.** Both sides are the same Go module
(one `go.mod`, `pion/webrtc/v4 v4.2.11`), on the same code path, with the same hardcoded outbound MTU;
`--mode raw` vs `--mode prod` changes only the application layer (E23 §0, unchanged), and `rawbench.go` vs
`prodbench.go` build the same transport. The field's own count — **896 data datagrams/MiB** — matches the
lab's 896.6 to within 0.2 %.

**The one genuine numeric disagreement is a code-read error, not a harness artifact.** E1's
"1228-B SCTP packets / 1265-B wire (1200-B payload + 28 SCTP + 37 DTLS)" is derived from `initialMTU=1228`,
which is **overridden** by `WithMTU(1200)` on this pion version. E1 could not detect the error from its own
data because the *mean* SCTP packet size is identical under both MTUs — `(16384 + 14×28)/14 = 1198.3 B`
either way, since a 16 KiB message is 14 fragments at 1172 *and* at 1200 bytes of cap. The histogram's
**modal** size (1237 B wire, i.e. 1200 B SCTP, with a 13:1 bump at 1213) is what distinguishes them, and
that is the new measurement this experiment adds. Since 1237 vs 1265 is a 2.2 % difference between two
numbers that are both "already at pion's MTU", it changes nothing about E23's premise — but the corrected
figure is **1200 B SCTP / 1237 B wire**, and the field number should be restated as such (it was inferred,
never measured, in the field).

### 2.1 E23's number *is* checkable in prod, contrary to the brief

The brief's premise that "no prod-mode JSON carries a packet count" is true but not blocking: `runProd`
prints the same counter to stderr (`prodbench.go`, `trace %s rtt=%d … | shim fwd=%d …`). E23's own prod
cells are therefore checkable, and they give **86,068 / 86,069 / 86,263 datagrams for 64 MiB** = 779.7 B —
identical to raw. So E23's figure was self-consistent across both modes; it was simply the wrong quotient.

## 3. Task 3 — is it a harness artifact? No harness fix is warranted

There is **no defect in the harness's send path to fix**. The measurement error is entirely in E23's
*derivation* (payload ÷ all datagrams, compared against a data-datagram MTU). The lab's datagrams are at
pion's MTU, the field's are too, and the counts match. Consequently:

- I did **not** change any behaviour of the send path, so there is no "corrected" wire geometry to
  re-measure — the corrected numbers are §1.3.
- The only harness artefacts that remain are the two *disclosed* instrumentation hooks (§7), both inert by
  default.
- What *does* need correcting is the **cost attribution** (E23's absolutes are contaminated by the bench's
  own in-process shim and poll loop), which is §4.

## 4. Task 5 — the corrected production-representative cost

All inputs are tonight's rate-capped cells plus one same-host replica run; the band is wide because the host
is swap-thrashing, and I will not quote a single digit.

| term | value (CPU-s/GB) | source |
|---|---|---|
| measured `go_cpu` per delivered GB, **raw** `[cap]` | **47.1 / 48.0 / 48.2 / 53.3** (clean cells 47.1, 53.3) | §6 cells; E23: 49.6–51.8 |
| − **shim relay+cop**y (read → `make`+copy → queue → `WriteToUDP`), size-insensitive | **−10.0 … −10.5** | §4.1 |
| − **shim token bucket** | ≈ 0 (within noise) | §4.1 |
| − **chromedp 50 ms poll loop** (inside the measured window) | **−3.2 … −5.4** | E22 c=0.03–0.05 cores; E23 §1.2 |
| = **pion raw path, minus lab instrumentation** | **≈ 31 … 40** | |
| + **prod application layer** (pipe copy, 3rd copy, envelope alloc, backpressure poll) | **+1.3 … +2.9** | E23 §1.1, the trustworthy 128 MiB paired pairs |
| **= production-representative sender cost** | **≈ 32 … 43 CPU-s/GB** (central ~37) | |

For scale this is **~150–200 Mbps per core** of payload (1 GB ≈ 37 CPU-s), versus E23's headline 53.2
CPU-s/GB ≈ 150 Mbps. **The corrected number is lower than both E23's raw `go_cpu` (53) and E22's field
bridge (46)** because tonight's raw figure contains the bench's own per-datagram work, which does not exist
in the field. This is the number to carry into the fork decision, and it is *still* overwhelmingly pion,
not the application.

### 4.1 The shim's own share, with the corrected datagram model

E23's replica instrument (`microbench-src/udprelay.go`, re-run unmodified on this host), now with the
**measured** datagram size rather than 780 B:

| relay variant (`-dg 1237`) | CPU-s/GB | µs/datagram | E23's `-dg 780` equivalent |
|---|---|---|---|
| relay only, unpaced firehose, no timer, no bucket | 5.96 / 6.31 | **7.4 / 7.8** | 8.9 / 9.2 |
| sender paced 100 µs/dg + token bucket 10 MB/s (E23's shim model) | 25.9 / 26.4 | 32.1 / 32.7 | 30.8 / 30.9 |

The per-datagram figures are **size-insensitive** (7.4 µs at 1237 B vs 8.9 µs at 780 B — the copy is not the
cost; the read/queue/write/scheduler path is), which is exactly the property needed to carry the number
across: `7.4–7.8 µs × 1.3468 M datagrams/GB = 10.0–10.5 CPU-s/GB`.

**I deliberately do not use the paced+bucket row as the shim's share**: its ~24 µs/dg premium over the relay
is the *modelled sender's* `time.Sleep` scheduler cost, which in the real harness is pion's own
flow-control cost and is already inside `go_cpu` — using it would double-count and would imply a shim share
(34–43 CPU-s/GB) larger than the entire non-pion residue. E23's own rows confirm the bucket branch itself is
free (paced, no timer, no bucket = 31.1 µs vs paced, rate 10 MB/s = 30.8 µs, i.e. ±1 %).

**Attempted in-harness ablation, reported as inconclusive.** The natural in-situ isolation of the shim's
per-datagram `time.NewTimer` is `--rtt 0` (then `due ≈ now`, `wait ≤ 0`, and `drainLoop` creates no timer)
versus `--rtt 12` at the same `--bandwidth 10MB` cap. Measured: rtt 0 → go_cpu 3.6761 / 3.6680 s
(54.8 / 54.7 CPU-s/GB) with `shim_fwd` 88,254 / 88,263 and 87 / 149 tail-drops; rtt 12 → 47.1–53.3 CPU-s/GB
with 86,198–86,827 datagrams and (in the clean cells) no drops. The rtt 0 cells are *not* cheaper, but they
also forwarded **+2.4 %** more datagrams and tail-dropped, so the ablation does not isolate the timer and I
will not claim a number from it. **The shim share quoted above is therefore a proxy (a replica of the shim's
structure), not an in-process ablation** — the same caveat E23 carried, now with the correct load model, and
one more reason the band is stated rather than a point value.

## 5. Task 4 — the fork recommendation, restated with evidence

**The lever E23 recommended first is spent.** Its evidence was "1.377 M datagrams/GB at 779.7 B of payload
each — ~40 % below a 1280-byte MTU; raising the payload to ~1200 B cuts datagram count 1.54×; filling a
1440-byte path MTU cuts it ~1.8×." Every clause after the datagram count is wrong: the payload per *data*
datagram is already **1169.6 B** and the packet is already **1200 B SCTP / 1237 B wire**, so there is no
1.54× to be had by "raising the payload to ~1200 B" — it is already there.

What actually remains, in order of measured headroom:

| lever | measured headroom | evidence | cost / risk |
|---|---|---|---|
| **1. Raise pion's hardcoded `outboundMTU` 1200 → path MTU** (1172 → ~1444 B payload; SCTP packet 1472) | data datagrams/MiB **896 → 768 (−14.3 %)** for 16 KiB messages, **896 → 736 (−17.9 %)** for 64 KiB; SACKs unchanged at 450/MiB; total datagrams **−9.5 … −12 %** ⇒ **≈ 3–5 CPU-s/GB ≈ 7–10 %** of the ~37 central production cost | §1.3 (896.6 data/MiB measured, 1172 B cap measured, `mtu=1200` and `outboundMTU=1200` read from source), fragment arithmetic re-derived for the larger cap | one-line fork of a constant, but it removes pion's deliberate 1200-B safety margin (`constants.go:41` — chosen to avoid PMTU black-holing on paths that cannot carry MTU-sized UDP). Real on the field path, risky in general. |
| **2. Batch the UDP send syscalls (GSO `UDP_SEGMENT` / `writev` / `sendmmsg`)** — pack N SCTP packets into one kernel send | **unquantified here; bounded above by ~10 CPU-s/GB** (the entire per-datagram shim share, which is a *userspace* read+copy+queue+write loop and therefore an over-estimate of what a *batched syscall* could save) | §4.1: 7.4 µs/datagram is size-insensitive; the per-datagram cost is scheduler/syscall, not copy | Linux-only API; in the **field there is no shim**, so the addressable portion is smaller than in this lab. The only lever that reduces per-datagram cost *without* changing protocol behaviour. |
| **3. Remove per-datagram allocations in the SCTP send-queue / DTLS record path** | unknown, but this is where the remaining **~31–40 CPU-s/GB of pion-internal work** lives (crypto is 0.16 CPU-s/GB = **0.3 %**) | §4 subtraction; E23 §3.3 | needs a `pprof` attribution to aim at; not done in E23 or here. |
| **4. Fragment tail** — the last chunk of each message is underfull (16 KiB: 1148 of 1172; prod 64 KiB: 1092 of 1172) | **≈ 0.1–0.15 %** | §1.1 counts | nil. This is all that is left of "collapse datagram count" at the current MTU. |
| **5. Bigger `dc.Send` messages** | **0** on CPU/byte; +11 % throughput uncapped | E23 §2, structure confirmed in §1.4 | nil; and prod already sends 64 KiB. |
| not worth doing | **crypto / cipher suite** 0.3 %; **app-layer copies** 2.4–5.5 % | E23 §3.2, §1.1 | — |

**Plainly: the evidence for forking pion for CPU efficiency is weaker than E23 stated.** The datagram-count
argument was its strongest item and it does not hold at the size claimed. What is left is (a) a one-line
`outboundMTU` change worth ~7–10 % of production CPU-s/GB, bought with PMTU risk; (b) syscall batching worth
≤ ~10 CPU-s/GB in a lab that has a shim and less in the field; (c) an unattributed ~31–40 CPU-s/GB of
pion-internal work that a fork *could* attack but that nobody has yet profiled. **A fork should not be
justified on E23's datagram-count evidence. The next experiment is a `pprof` attribution of the ~31–40
CPU-s/GB pion residual (send queue, DTLS record framing, allocations, GC), and only then a fork.**

## 6. What this does NOT establish

1. **The field's 1200-vs-1228 SCTP MTU is inferred, not measured.** I read it out of the library and the
   harness on this machine and confirmed it here (`mtu=1200`). The field agent is built from the same module
   and the same v1 code path, but no field capture was taken (lab-only constraint). A per-5-tuple field
   capture of the modal datagram size would close this; it is a 2 % question and it does not move the
   verdict either way.
2. **The ~31–40 CPU-s/GB pion-internal residue is still not decomposed** — this experiment only removes the
   lab's own shim and poll loop from the constant. SCTP chunking vs DTLS record framing vs send-queue
   plumbing vs GC remain one lump. That needs `pprof`.
3. **The shim's share is a replica measurement, not an in-process ablation** (§4.1); the one in-harness
   ablation attempted (`--rtt 0`) was confounded by drops and a +2.4 % datagram count and is reported as
   inconclusive rather than used.
4. **Absolutes are from a swap-thrashing host** (6.2 GB swap, sanity gate 8.4/9.6 vs ≈100 Mbps expected).
   Only the *structures and counts* (datagram size, split, per-MiB counts) and the *ratios* transfer; the
   CPU-s/GB band is host-specific and is quoted as a band for that reason.
5. **Prod's own cells tonight are unusable for a prod-vs-raw delta** (clean prod 42.6 CPU-s/GB vs a
   tail-dropping 61.2; raw 47.1–53.3, spread 12 %). I reused **E23's** paired 128 MiB delta (+1.3 … +2.9)
   rather than measuring a new one that the noise could not support.
6. Nothing here tests v2, the relay, or the client-sink hypothesis.

## 7. Harness changes (minimal, disclosed, revertible)

Two env-gated instrumentation hooks and **no behavioural change**. `go build ./...` → exit 0;
`go vet ./cmd/benchdirect/` → exit 0; a run **without** `SB_SHIM_HIST` produced no `exp24`/hist output and
wrote no `.hist` file (verified: `gate-check.log`).

| file | change | revert |
|---|---|---|
| `cmd/benchdirect/shim.go` | added `histPath`/`histEnabled()`/`dumpHistogram()`, a `[65536]atomic.Int64` per direction, a `toB bool` field on `packet` (set in `ingest` from `sameUDPAddr(target, st.b)`), one conditional atomic increment at the successful `WriteToUDP` in `drainLoop`, `histEnabled()` in `NewShaper`, `dumpHistogram()` at the end of `Close()`, and the `fmt/os/sort` imports | delete the block marked `TEMPORARY exp24`, the two marked call sites, the `toB` field, and the three imports |
| `cmd/benchdirect/rawbench.go` | prints one `exp24 sctp conn=… mtu=… bytes_sent=… bytes_recv=… cwnd=…` line to stderr, guarded by `os.Getenv("SB_SHIM_HIST") == ""` → early `break` | delete the `for i, c := range conns { … }` block marked `TEMPORARY exp24` |

Why: the histogram is the only way to see the datagram *size distribution* (E23 had only a total count and a
byte sum, which is exactly the ambiguity that produced the error); the SCTP-stats line reads the association's
own `MTU()` and byte counters so the answer does not rest on one field's semantics. Both are inert by
default because other agents share this worktree.

## 8. Artifacts (`docs/superpowers/spikes/results/raw/exp24-datagram-size-artifact/`)

| file | what |
|---|---|
| `exp24-raw16-r{1..5}.{json,log,hist}` | raw, `--chunk 16KiB`, `[cap]` 10 MB/s, rtt 12; r3/r4 clean, r1/r2/r5 tail-dropped |
| `exp24-raw16-rtt0-r{1,2}.{json,log,hist}` | the `--rtt 0` timer ablation (inconclusive, §4.1) |
| `exp24-prod16-r{1,2}.{json,log,hist}` | prod `[cap]`; the prod datagram structure (§1.4). `shim_fwd` is in the `.log`, not the JSON |
| `sanity-r{1,2}.{json,log,hist}` | the §3 sanity gate — **FAILED** (8.39 / 9.62 Mbps) |
| `gate-check.log` | proof the instrumentation is inert without `SB_SHIM_HIST` |
| `microbench.txt` | exp24 re-run of E23's `udprelay.go` replica at `-dg 1237` |
| `runner.log` | START/OK per cell, with `uptime` + swap before each |
| `system-snapshots.txt` | host snapshot at block start |
| `run-exp24.sh` | the runner |
| `mistargeted-initial-run/` | three extra `exp24-raw16-r1..r3` runs whose `-out` path was mis-resolved into `agent/` by a first, buggy invocation of the runner (48.08 / 40.95 / 45.36 Mbps, `go_cpu` 2.99 / 3.17 / 3.04 s, `fwd` 87,027 / 87,666 / 86,927). Moved here rather than deleted — same config as the `exp24-raw16` block and consistent with it; **not** used in any number above. The `agent/` worktree was left clean. |

E23's microbench source was copied to `/tmp/exp24mb/` and run unmodified from there; nothing in E23's
artifact directory was altered.

### Appendix — every cell (incremental; `cs/GB` = `go_cpu / (received/1e9)`)

| label | mode | rtt | cap | Mbps | shim_fwd | drop | go_cpu s | cs/GB |
|---|---|---|---|---|---|---|---|---|
| exp24-raw16-r1 | raw | 12 | 10MB | 46.37 | 87,514 | 1111 | 3.1155 | 46.42 |
| exp24-raw16-r2 | raw | 12 | 10MB | 41.21 | 87,657 | 1275 | 3.2366 | 48.23 |
| exp24-raw16-r3 | raw | 12 | 10MB | 74.43 | 86,198 | 0 | 3.5797 | 53.34 |
| exp24-raw16-r4 | raw | 12 | 10MB | 56.61 | 86,827 | 0 | 3.1619 | 47.12 |
| exp24-raw16-r5 | raw | 12 | 10MB | 46.63 | 87,330 | 1242 | 3.2230 | 48.03 |
| exp24-prod16-r1 | prod | 12 | 10MB | 55.21 | 89,405 | 1568 | 4.1085 | 61.22 |
| exp24-prod16-r2 | prod | 12 | 10MB | 60.33 | 86,961 | 0 | 2.8576 | 42.58 |
| exp24-raw16-rtt0-r1 | raw | 0 | 10MB | 68.08 | 88,254 | 87 | 3.6761 | 54.78 |
| exp24-raw16-rtt0-r2 | raw | 0 | 10MB | 69.87 | 88,263 | 149 | 3.6680 | 54.66 |
| sanity-r1 `[uncapped]` | raw | 71 | uncap | 8.39 | 40,764 | 0 | 2.176 | 69.42 (partial: 31,342,592/33,554,432) |
| sanity-r2 `[uncapped]` | raw | 71 | uncap | 9.62 | 43,764 | 0 | 2.303 | 68.63 |
