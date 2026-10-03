# Experiment 39 — is the ~139 Mbps near-field plateau a receive-side artifact or a real shared cap? — 2026-10-02

**Status:** COMPLETE — written incrementally after every cell; **20 field cells** (17 native-receiver, 2 browser,
1 anchor) plus **5 iperf3 path-control cells**. Every cell reported, none hidden.

## Verdict

**(a) SURVIVES. The ~139 Mbps sustained plateau at ~11 ms is a RECEIVE-SIDE ARTIFACT, not a real shared cap.**

Raising **pion's SCTP advertised receive window** (not the OS socket buffer) lifts the native receiver's
*sustained* throughput monotonically from **128.3 Mbps** (1 MiB window, n=5) to **155.4** (8 MiB, n=5) to
**176.5 Mbps** (≥16 MiB, n=6) — **+37 %**. The near-zero-second fraction collapses from **30 % → 10 % → 8 %**.
The 1 MiB arm's *best* run (168.2) is still below the ≥16 MiB arm's *worst* (172.0), so the separation is not
noise. Sustained never stalls at 139.

**Therefore the campaign's near-field claim ("the v1-direct ceiling *is* the browser's DataChannel receive
path" at low RTT) does NOT stand as written.** In this session the browser delivered **132.0 / 133.1 Mbps
(mean 132.5)**, reproducing E38's 139.0, while a correctly-configured native Go/pion receiver on the *same path
in the same session* delivered **176.5 Mbps — +33 %**. The browser's receive path costs a real ~25–33 %, but it
is **not the ceiling**: receiver-side SCTP window configuration is worth more than the browser/native
difference, and *neither* receiver is near the path limit.

**(b) is not fully dead, but it is a different cap at a different value.** A buffer-insensitive residual stall
survives at *every* window size: instantaneous peaks of **230–329 Mbps** persist, the path carries **245**, yet
the sustained mean saturates at **~176** by 16 MiB (32 MiB = 177.0, no further gain) with ~8 % of seconds still
near-zero. That residual (~28 % of the burst rate) is present in the native receiver, so it is **not
browser-specific** — most likely sender-side pion CC/RTO collapse, but **this experiment did not isolate it**.
The 139 was never that residual: 139 sits *below* 176 and is window-fixable.

**Corrected near-field ordering (~11 ms):** path **245** > sender burst peaks **233–329** > native receiver,
16 MiB window, sustained **176.5** > browser **132.5–139.0** > native receiver, 1 MiB window / stock **128.3**.

**Campaign consequence:** the near-field headline needs rewriting from "the ceiling *is* the browser's
DataChannel receive path" to "the browser's receive path is a real ~25–33 % cost at low RTT, but the binding
limit at 11 ms is receiver-side SCTP window configuration, and a residual sender-side sawtooth caps everyone at
~176 of the 245 Mbps path."

## Question

E38 measured, at CLIENT-EAST (~10.94 ms RTT), **browser 139.0 Mbps (n=2)** vs a **native Go/pion receiver
`fieldrecv` 138.98 Mbps** once the client's UDP `rmem` was raised to 4 MiB — *identical*. But E38's stock
native receiver also **bursts to 233–237 Mbps** in a sawtooth (5–7 s period), and the path carries **245 Mbps**
(E32 controls). Both receivers landing on ~139 while the sender demonstrably reaches ~234 raised the
possibility that **~139 is a shared cap (residual RTO sawtooth / agent uplink / client-side), not the
browser's DataChannel receive path at all** — in which case the campaign's near-field headline is wrong at low
RTT.

This experiment decides between:
- **(a) receive-side artifact** — a bigger receive buffer lifts *sustained* native throughput clearly above
  139 towards the 233–237 peaks / 245 path → the 139 was an artifact, the browser is not proven to be a cap.
- **(b) real shared cap** — sustained stays ~139 across every buffer setting while iperf3 shows ~245 → 139 is
  a real shared cap (sender / uplink / client-side), again not browser-specific.
- **(c) path/uplink cap** — if iperf3 itself reads ~139 today, the path is the cap and the near-field framing
  changes entirely.

## Setup

- Agent host VERSA; experiment container **`sb-run`** only (`sb-agent:pristine`, host networking, UI 7879).
  **Not recreated.** Verified stock immediately before the run: `SB_SCTP_CA_STEP=32768` present, **no
  `SB_SCTP_MIN_CWND`**, `State.StartedAt = 2026-10-02T21:18:04Z` — byte-identical to E38, still running after.
  Production `sharebridge-agent` (7878) and every non-ShareBridge container on VERSA untouched and up
  throughout (43 containers observed up at the final check).
- Signalling origin TESTBOX (v1 rig), left alone.
- Client **CLIENT-EAST** only. Idle before every cell (process gate). CLIENT-WEST not touched.
- Data-path RTT to VERSA's public egress: **10.936 ms avg** (9.634 / 11.822 / mdev 0.984, n=5) — identical to
  E38's 10.936.
- Share: the orchestrator's direct share, 754 MiB = 790,626,304 B = 6,325.010432 Mb.
- **Tool note — deviation from the brief.** The *stock* `fieldrecv` binary (sha256 `8b479970…`) does **not**
  contain `--max-rx-buf`; that flag lives only in the labelled diagnostic binary `fieldrecv-rwnd`
  (sha256 `d7c42581…`), exactly as E38 recorded. The brief stated the stock binary already has the flag; it
  does not — verified with `strings` on both local builds (`max-rx-buf` count: stock 0, rwnd 1). Therefore the
  **buffer ladder uses `fieldrecv-rwnd` for every arm** so the advertised receive window is the only variable,
  and the **stock `fieldrecv` binary is run once as anchor S1** to reproduce E38's D1 = 138.98.
- **Client sysctl.** Before the run: `net.core.rmem_default = 212992`, `rmem_max = 212992` (E38's restored
  state). Set to `4194304 / 16777216` for the `rmem` 4 MiB arms, `16777216 / 16777216` for the `rmem` 16 MiB
  arms, back to `212992 / 212992` before the browser cells. **Confirmed `212992 / 212992` at the end** (a
  transient `rmem_default = 4194304 / rmem_max = 212992` was observed after the last field cell and corrected —
  see Caveats).
- **Per-cell metric.** `fieldrecv` `SUMMARY mbps` = 6,325.010432 ÷ `wall_s`, cross-checked against the agent-side
  window (first `DataChannel lanes ready` → `download complete`); the two agree to **≤ 0.03 s** on every native
  cell (e.g. B1: SUMMARY 39.829 s vs agent 39.848 s). Browser cells use the agent-side window only (the driver
  is click-only; the **Playwright Download API was never used**).
- **Trace shape.** The `PROGRESS` line emits one row per second (`mbps_inst`); all shapes below are derived from
  those rows (first second dropped as ramp-in). "near-zero" = `mbps_inst < 20`.
- **Mode assertion.** Every native cell returned `status=complete` with `payload_bytes = 790,626,304` and
  `selected="local=host remote=…"` — a direct-mode assertion (a relay fallback yields zero bulk frames and
  `status=deadline`). Every browser cell required `DataChannel lanes ready` **and** the relay standby expiring
  unused (`relay: pending wait window exceeded`, never `relay channel started successfully`).

## Raw table — every run

### Phase 1 — in-session path control (no transfer running)

| # | time (UTC) | direction | type | offered | received | retrans | notes |
|---|---|---|---|---|---|---|---|
| P1 | 22:33 | VERSA→CLIENT-EAST | TCP 1-flow | — | **244.80 Mbps** | 0 | `-t 12 -O 3`; sent 246.12 |
| P2 | 22:33 | VERSA→CLIENT-EAST | TCP 4-flow | — | **244.80 Mbps** | 22 | `-t 12 -O 3 -P 4`; sent 245.10 |
| P3 | 22:34 | VERSA→CLIENT-EAST | UDP 1-flow | 300 Mbps | **247.34 Mbps** | — | `-t 12 -O 3 -l 1200`; 17.6 % loss at 300 offered |
| P4 | 23:58 | VERSA→CLIENT-EAST | TCP 1-flow | — | **244.79 Mbps** | 0 | repeat after the 23:56 cell; path unchanged |
| P5 | 23:58 | VERSA→CLIENT-EAST | UDP 1-flow | 300 Mbps | **247.27 Mbps** | — | repeat; path unchanged |

The path carries **~245 Mbps** in this session, at both ends of the run — **identical to E32 (245 / 249)**.
Hypothesis **(c) is refuted**: the path is not 139.

### Phase 2 — native receiver buffer ladder

`fieldrecv-rwnd --max-rx-buf <W>` with client `net.core.rmem_default = <R>`, unless noted.

| # | tool | `--max-rx-buf` (W) | `rmem_default` (R) | window s | **Mbps** | peak inst | mean inst | s >150 | s <20 | % <20 | max stall s | mode |
|---|---|---|---|---|---|---|---|---|---|---|---|---|
| A1 | fieldrecv-rwnd | 1,048,576 | 4,194,304 | 42.243 | **149.730** | 230.8 | 148.7 | 26 | 9 | 22 % | 3 | direct |
| A2 | fieldrecv-rwnd | 1,048,576 | 4,194,304 | 53.388 | **118.473** | 236.6 | 118.5 | 22 | 18 | 34 % | 4 | direct |
| A3 | fieldrecv-rwnd | 1,048,576 | 4,194,304 | 63.881 | **99.012** | 236.5 | 98.7 | 21 | 26 | 41 % | 3 | direct |
| A4 | fieldrecv-rwnd | 1,048,576 | 4,194,304 | 37.599 | **168.222** | 234.4 | 168.9 | 26 | 7 | 19 % | 3 | direct |
| S1 | **fieldrecv (stock)** | *n/a (1 MiB default)* | 4,194,304 | 59.690 | **105.964** | 230.8 | 105.8 | 23 | 19 | 32 % | 4 | direct |
| B1 | fieldrecv-rwnd | 8,388,608 | 4,194,304 | 39.829 | **158.803** | 232.4 | 159.3 | 25 | 4 | 10 % | 1 | direct |
| B2 | fieldrecv-rwnd | 8,388,608 | 4,194,304 | 40.565 | **155.922** | 232.0 | 156.3 | 24 | 5 | 12 % | 2 | direct |
| B3 | fieldrecv-rwnd | 8,388,608 | 4,194,304 | 39.771 | **159.035** | 232.9 | 159.7 | 23 | 2 | 5 % | 1 | direct |
| C1 | fieldrecv-rwnd | 8,388,608 | 16,777,216 | 42.506 | **148.802** | 234.5 | 149.5 | 24 | 5 | 12 % | 1 | direct |
| C2 | fieldrecv-rwnd | 8,388,608 | 16,777,216 | 41.166 | **153.645** | 235.1 | 152.3 | 24 | 5 | 12 % | 1 | direct |
| D1 | fieldrecv-rwnd | 16,777,216 | 16,777,216 | 34.675 | **182.407** | 234.4 | 183.3 | 26 | 2 | 6 % | 1 | direct |
| D2 | fieldrecv-rwnd | 16,777,216 | 16,777,216 | 36.784 | **171.952** | 241.6 | 173.4 | 23 | 2 | 6 % | 1 | direct |
| D3 | fieldrecv-rwnd | 16,777,216 | 16,777,216 | 36.058 | **175.414** | 250.0 | 175.5 | 25 | 3 | 9 % | 2 | direct |
| E1 | fieldrecv-rwnd | 16,777,216 | 4,194,304 | 35.442 | **178.461** | 292.4 | 178.6 | 26 | 4 | 11 % | 1 | direct |
| E2 | fieldrecv-rwnd | 16,777,216 | 4,194,304 | 36.362 | **173.947** | 276.2 | 174.5 | 24 | 3 | 8 % | 1 | direct |
| F1 | fieldrecv-rwnd | 33,554,432 | 4,194,304 | 35.739 | **176.977** | 329.3 | 178.5 | 27 | 2 | 6 % | 2 | direct |

**Aggregates (arm means):**

| arm | n | Mbps | sd | range | mean % of seconds < 20 |
|---|---|---|---|---|---|
| **1 MiB** window (A1–A4 + S1) | 5 | **128.28** | 29.5 | 99.0 – 168.2 | 29.6 % |
| **8 MiB** window (B1–B3 + C1–C2) | 5 | **155.42** | 4.3 | 148.8 – 159.0 | 10.2 % |
| **≥16 MiB** window (D1–D3, E1–E2, F1) | 6 | **176.53** | 3.9 | 172.0 – 182.4 | 7.7 % |
| 16 MiB, `rmem` 4 MiB (E1–E2) | 2 | 176.20 | — | 173.9 – 178.5 | 9.5 % |
| 16 MiB, `rmem` 16 MiB (D1–D3) | 3 | 176.59 | — | 172.0 – 182.4 | 7.0 % |
| 32 MiB, `rmem` 4 MiB (F1) | 1 | 176.98 | — | — | 6 % |

**Trace shape, verbatim first-12 / last-8 `mbps_inst` (rounded):**

- A1 1 MiB: `209 216 231 231 231 100 13 193 196 231 97 4` … `39 217 231 45 11 185 231 231`
- A3 1 MiB (worst): `8 170 234 178 2 5 3 91 216 77 88 233` … `146 11 199 234 221 2 9 10`
- A4 1 MiB (best): `8 182 234 234 234 234 234 234 234 234 234 234` … `2 200 234 234 234 234 234 234`
- B3 8 MiB: `81 226 226 231 80 124 231 224 12 160 231 215` … `231 56 73 82 229 231 62 96`
- D1 16 MiB: `230 124 171 226 73 187 233 234 234 187 15 215` … `232 234 101 175 234 234 91 152`
- E1 16 MiB: `140 190 239 176 153 225 235 234 51 248 180 134` … `234 234 191 10 237 234 165 2`
- F1 32 MiB: `214 231 231 86 249 220 191 153 212 205 230 167` … `193 233 214 127 214 80 6 2`

**The sawtooth persists at every setting** — peaks of 230–329 Mbps and 1–4 s near-zero stalls remain — but its
*frequency* falls sharply as the window grows (30 % → 10 % → 8 % of seconds). The 1 MiB arm is **bimodal**: it
either runs at ~234 almost continuously (A4, 168.2) or collapses repeatedly (A3, 99.0); the ≥16 MiB arm is
*tight* (sd 3.9).

### Phase 3 — one (two) browser cells in the same session

| # | time (UTC) | tool | lanes ready | download complete | window s | **Mbps** | mode | notes |
|---|---|---|---|---|---|---|---|---|
| BB1 | 22:51:31.368 | browser (xvfb Chrome, click-only) | 22:51:31.367939 | 22:52:19.277060 | 47.909 | **132.020** | direct (lanes ready + relay unused) | `relay: pending wait window exceeded` at 22:52:15; `count: 42` |
| BB2 | 23:24:00.472 | browser (xvfb Chrome, click-only) | 23:24:00.471533 | 23:24:48.009363 | 47.538 | **133.052** | direct (lanes ready + relay unused) | `relay: pending wait window exceeded` at 23:24:45; `count: 43` |

Browser mean this session **132.54** vs E38's **139.03** (n=2). Zero relay fallbacks in 2/2 browser attempts
this session (E38 saw 2/4). `rmem` was at the restored default **212992** for both browser cells — exactly
E38's browser condition.

## Interpretation — which explanation survives

**1. (a) survives and (c) is refuted.**

- The path control is unambiguous and reproduced in-session at both ends of the run: **TCP 244.8 / 244.8 /
  244.8 Mbps, UDP 300→247.3 / 247.3 Mbps**. Hypothesis (c) — the path is the cap — is **dead**.
- Sustained native throughput is a clean, monotone function of **pion's advertised SCTP receive window**:
  **128.3 (1 MiB) → 155.4 (8 MiB) → 176.5 (≥16 MiB) Mbps**, sd 29.5 / 4.3 / 3.9. Even the 1 MiB arm's best
  run (168.2) sits below the ≥16 MiB arm's worst (172.0). The stall fraction falls **30 % → 10 % → 8 %**.
- **The lever is the advertised window, not the OS socket buffer.** Holding the window at 16 MiB, `rmem`
  4 MiB gives 176.2 (n=2) and `rmem` 16 MiB gives 176.6 (n=3) — identical. Holding `rmem` at 16 MiB, an 8 MiB
  window gives **151.2** and a 16 MiB window gives **176.6**. So E38's `rmem` fix (which lifted stock from
  105.8 to 138.98) was addressing the *socket* half of the same artifact; the dominant half is pion's
  `sctp.initialRecvBufSize = 1 MiB` default.
- **The plateau saturates at ~176 by 16 MiB**, not at the path: 32 MiB = **177.0**, indistinguishable from
  16 MiB.

**2. (b) is partially alive — but at ~176, not at 139, and it is not browser-specific.**

No window size eliminates the sawtooth. At every setting the receiver still shows instantaneous bursts of
**230–329 Mbps** (above the 245 Mbps TCP control — the path tolerates short bursts) separated by 1–4 s
near-zero stalls on a 5–7 s period, leaving ~8 % of seconds at <20 Mbps even at 16–32 MiB. That is a *second*,
**buffer-insensitive** limiter capping the mean at ~176 of the 245 Mbps path (~28 % of the burst rate lost).
It is visible in the **native** receiver, so it is **not browser-specific** — most plausibly the sender's pion
congestion-control/RTO collapse (the agent runs `SB_SCTP_CA_STEP=32768`), but **this experiment did not
isolate it** and makes no claim about its mechanism. Importantly, **139 is not that cap**: 139 lies *below*
176 and *is* window-fixable.

**3. Does the campaign's near-field claim stand? No — it needs rewriting.**

- E38 already showed the native receiver is not *slower* than the browser at 11 ms. E39 shows it is
  **substantially faster**: browser **132.5** (this session, n=2) / **139.0** (E38, n=2) vs native receiver
  with a 16 MiB window **176.5** (n=6) — **+33 %**. So "the v1-direct ceiling **is** the browser's DataChannel
  receive path" is **wrong at low RTT** as an absolute ceiling claim.
- The browser's receive path *is* a real cost — it is ~25–33 % below a correctly-configured native receiver —
  but the binding constraint at 11 ms is **receiver-side SCTP window configuration**, worth **more** than the
  browser/native gap (128 → 176 = +37 % vs 176 → 133 = −25 %).
- And *nobody* is at the path limit: 176.5 of 245 = **72 %**. The near-field story is therefore three caps
  stacked, not one: path **245** ≫ residual sender-side sawtooth cap **~176** > browser **132.5–139** >
  default-configured native receiver **128**.

## Caveats

- **n is small and the 1 MiB arm is bimodal by nature.** n=5 / 5 / 6 for the three arms; the 1 MiB arm has
  sd 29.5 and spans 99.0–168.2 because the stall count is chaotic at a 1 MiB window. The ≥16 MiB arm is tight
  (sd 3.9, range 172.0–182.4) so the *arm separation* is far outside the noise, but the arm *means* carry the
  stated spread. A4 (168.2) ran ~32 minutes after the other native cells; the path was re-controlled at 23:58
  (TCP 244.79, UDP 247.27) and was unchanged, so the gap is not a network change.
- **One cluster of adaptation.** The 1 MiB arm's bimodality means the honest summary is "the 1 MiB window is
  *sometimes* as good as ~168 and often as bad as ~99–106"; the ≥16 MiB window removes that variance.
- **Mechanism is inferred, not read off the wire.** That the lever is pion's *advertised SCTP receive window*
  is established by the ladder's monotonicity, by the `rmem` cross-over controls (E vs D, C vs B) and by the
  source flag, but the negotiated SCTP INIT/`a_rwnd` was **not** captured off the wire in this experiment.
- **The residual ~176 cap is not explained.** Its existence is demonstrated (16 vs 32 MiB identical; stalls
  present at every setting) but its mechanism was not isolated; no claim is made that it is the sender.
- **Browser cells have no per-second trace** (the driver is click-only and reports a single completion), so
  the browser's stall structure is unknown; only its aggregate is comparable. Browser windows are
  agent-side, second-resolution (±1 s ⇒ ±2 % at 47.5 s).
- **Gate anomaly, reported for honesty (reproducing E38's).** `ps -eo comm | grep -cE "^(node|chrome|Xvfb|fieldrecv)$"`
  returned **0** on CLIENT-EAST during both browser cells *and* returned **0** at a moment when the empty
  process table was independently confirmed (`ps -eo pid,comm` showed no node/chrome/Xvfb). The gate was 0
  before every native cell and the process table was independently verified empty before each launch, so no
  cell was contaminated — but the `comm`-based gate does **not** reliably see the `xvfb-run`/Chrome tree and
  should not be trusted alone in future runs.
- **Sysctl restoration.** Restored to `212992 / 212992`. After the final field cell the client transiently
  showed `rmem_default = 4194304 / rmem_max = 212992` (the per-cell helper sets only `rmem_default`); this was
  corrected and the final state was **confirmed `212992 / 212992`**. Only ever *increased* buffers; no
  persistent change remains.
- The stock `fieldrecv` binary was **never modified in place**; the `--max-rx-buf` flag lives only in the
  separate, clearly labelled `fieldrecv-rwnd` binary. The brief's statement that the stock binary already has
  the flag is incorrect and is documented above.

## Artifacts

- Native receiver source `agent/cmd/fieldrecv/`; stock binary `agent/bin/fieldrecv-linux-amd64`
  sha256 `8b4799707532c72204e009085bca1692dab0b8cd16728d87b1cb4679ead0180c`; window-flag variant
  `agent/bin/fieldrecv-rwnd-linux-amd64`
  sha256 `d7c425818a7f9946cb09c0cc3bbe83954099935716c1190855907a44630cf728` (source identical except the
  `--max-rx-buf` flag; `SetSCTPMaxReceiveBufferSize` applied at `run.go:223–230`).
- Per-cell native logs on CLIENT-EAST: `/tmp/fr-{A1,A2,A3,A4,S1,B1,B2,B3,C1,C2,D1,D2,D3,E1,E2,F1}.log`
  (`PROGRESS` rows + `SUMMARY`); browser driver logs `/tmp/cell-b1.log`, `/tmp/cell-b2.log`.
- iperf3 results (JSON + server logs) locally `/tmp/iperf-{tcp1,tcp4,udp300,tcp1b,udp300b}.json`; helper
  `/tmp/iperfrun.sh`, per-cell helper `/tmp/fieldcell.sh`.
- Agent side: `docker logs sb-run` — `DataChannel lanes ready` / `download complete` pairs used as the
  cross-check and as the browser window; share download counter went 26 → 44 across this experiment.
- Pion default located at `github.com/pion/sctp@v1.9.4/association.go:74`
  (`initialRecvBufSize = 1024*1024`), used as `advertisedReceiverWindowCredit` at `association.go:4336`.
