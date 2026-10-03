# Experiment 38 — non-browser (Go/pion) receiver on the real v1 direct field path — 2026-10-02

**Status:** COMPLETE — written incrementally after every cell; 14 field runs launched, **11 valid, 2 discarded by
the relay-mode gate, 1 supplementary diagnostic arm** (plus 2 diagnostic cells on a labelled tool variant).

## Verdict

**The browser attribution SURVIVES — but it is INCOMPLETE, and the campaign's absolute claim ("the v1-direct
ceiling *is* the browser's DataChannel receive path") is WRONG.**

At the load-bearing CLIENT-WEST cell (~70.75 ms) the native Go/pion receiver does **not** land at the browser's
~65 Mbps, so the alternative hypothesis — "the cap is the pion *sender's* congestion control" — is **refuted**:
the native receiver reaches **100.1 / 107.6 / 111.6 Mbps (mean 106.4)** against the browser's **64.54 Mbps** on the
same session. The browser's receive path therefore costs a real **~45 %** and is a genuine cap.

But the native receiver also does **not** reach the ~245 Mbps path capacity. It is capped at **≈115 Mbps**, and
the cap is exactly **pion's default 1 MiB SCTP advertised receive window** (`sctp.initialRecvBufSize = 1024*1024`,
`github.com/pion/sctp@v1.9.4 association.go:74`), which bounds throughput at `rwnd / RTT` =
1 MiB / 70.75 ms = **118.5 Mbps predicted vs 114.95 Mbps measured steady-state** (97 %). **This is a receiver-stack
default, not the sender's congestion control and not the path.** The diagnostic arm confirms the sender is not
CC-capped at 115: with the window lifted to 4 MiB (`fieldrecv-rwnd --max-rx-buf 4194304`) the sender drives a
sustained ramp to **232 Mbps (94 % of the 245 Mbps path capacity)** at 70 ms — then collapses cyclically and nets
only **84.9 Mbps**, worse than stock. The 70 ms path does not *sustain* 232 Mbps; it does sustain ~115.

At CLIENT-EAST (~10.94 ms) the pair **inverts** — browser **139.0 Mbps (n=2)** vs stock native receiver
**105.8 Mbps (n=3)** — but that deficit is a **tool artifact**: the client's default `net.core.rmem_default`
of **208 KiB** overflows on the low-RTT bursts. With `rmem` raised to 4 MiB the native receiver reaches
**138.98 Mbps**, i.e. **identical to the browser (139.0)**. So at 11 ms the browser is *not* a cap at all.

## Question

E1–E32 attributed the v1-direct ceiling to the **browser's WebRTC DataChannel receive path**, but every number was
measured *through a browser* — the attribution is inference from ablation. A **non-browser (Go/pion) receiver on
the real field path** splits the two competing explanations:

- **Browser is the cap** → the native receiver should reach ~245 Mbps (E32 controls: WEST TCP 4-flow 245, UDP 249).
- **pion sender congestion control is the cap** → the native receiver should also land at ~64–75 Mbps at 70 ms.

The campaign's 521–533 Mbps Go-receiver figure is **loopback only** (RTT≈0, no window physics) and does not
answer this.

## Setup

- Agent host VERSA; experiment container **`sb-run`** only (`sb-agent:pristine`, host networking, `UI_PORT=7879`).
  **Not recreated.** Verified stock immediately before the run: `SB_SCTP_CA_STEP=32768` present, **no
  `SB_SCTP_MIN_CWND`**, started 2026-10-02T21:18:04Z, boot log clean. Production `sharebridge-agent` (7878) and
  every non-ShareBridge container on VERSA untouched and up throughout.
- Signalling origin TESTBOX (v1 rig), left alone.
- Clients CLIENT-WEST and CLIENT-EAST, both idle before every cell (process gate).
- **RTT (data path, to VERSA's public egress):** CLIENT-WEST **70.753 ms** (69.553 / 71.956 / mdev 0.894);
  CLIENT-EAST **10.936 ms** (9.769 / 12.049 / mdev 0.934). TESTBOX is a *separate* host (WEST→TESTBOX 58.8 ms,
  EAST→TESTBOX 0.49 ms) — signalling RTT, not data-path RTT. VERSA is a home host (6 cores) behind NAT.
- Share: the orchestrator's direct share, 754 MiB = 790,626,304 B = 6,325.010432 Mb, 500 downloads.
- **Tool:** `fieldrecv` — native Go/pion receiver speaking the real `/ws/client` protocol (knock/nonce/join with
  empty HMAC, answers the agent's offer, accepts the 3 lanes, `transport_hello`/`transport_ready`,
  `file_request`), counting bulk payload via the 14-byte envelope decode (E28 "bare" arm equivalent: no sink, no
  SHA-1, no assembly). **Direct-only** — a relay fallback yields zero bulk frames and `status=deadline`, so a
  `SUMMARY` with `payload_bytes>0` **is** a direct-mode assertion. SHA-256
  `8b4799707532c72204e009085bca1692dab0b8cd16728d87b1cb4679ead0180c`, identical on both clients and to the local
  build. Deployed to `~/sbtest/fieldrecv` on both.
- **Diagnostic variant (clearly separated from the primary grid):** `fieldrecv-rwnd`, a rebuild of the same
  source with one added flag `--max-rx-buf` that calls `SettingEngine.SetSCTPMaxReceiveBufferSize`
  (sha256 `d7c425818a7f9946cb09c0cc3bbe83954099935716c1190855907a44630cf728`). The stock binary was **not**
  modified in place and every primary cell above used the stock binary.
- **Primary metric:** fieldrecv `SUMMARY mbps` = 6,325.010432 ÷ `wall_s`; cross-checked against the agent-side
  window, first `DataChannel lanes ready` → `download complete`. Browser cells use the agent-side window only
  (the driver is click-only; the Playwright Download API was never used).
- **Mode assertion:** every accepted browser cell required `DataChannel lanes ready` **and** the relay standby
  expiring unused (`relay: pending wait window exceeded`, never `relay channel started successfully`).

## Raw table — every run

| # | client | tool | payload_bytes | frames | wire_bytes | wire/payload | window s | Mbps | MB/s | mode | RTT ms | agent CPU | notes |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| W1 | CLIENT-WEST | fieldrecv (stock) | 790,626,304 | 48,263 | 791,301,986 | 1.00085 | 63.177 | **100.115** | 12.51 | direct (`local=host remote=host`) | 70.75 | n/m | complete, bad=0; one ~3 s stall at 275.0 MB, then 115 Mbps |
| W2 | CLIENT-WEST | fieldrecv (stock) | 790,626,304 | 48,260 | 791,301,944 | 1.00085 | 58.783 | **107.600** | 13.45 | direct (`local=host remote=prflx`) | 70.57 | 0.10–0.28 % host-wide | complete, bad=0, no stalls |
| W3 | CLIENT-WEST | fieldrecv (stock) | 790,626,304 | 48,260 | 791,301,944 | 1.00085 | 56.692 | **111.568** | 13.95 | direct (`local=host remote=prflx`) | ~70.6 | 0.43 cores (24.34 s / ~57 s) | complete, bad=0, no stalls; steady 119.4 inst |
| **B-W1** | CLIENT-WEST | **browser** | 790,626,304 (implied) | — | — | — | 98.0 | **64.54** | 8.07 | direct (lanes ready + relay unused) | ~70.6 | — | clicked 21:32:43; lanes ready 21:32:42 → complete 21:34:20. Reproduces E32 unfixed 64.84 (n=2) |
| E1 | CLIENT-EAST | fieldrecv (stock) | 790,626,304 | 48,258 | 791,301,916 | 1.00085 | 54.876 | **115.259** avg / peak **237.4** | 14.41 | direct (`local=host remote=prflx`) | 10.69 | 0.38 cores (21.83 s / ~57 s) | complete, bad=0; **sawtooth**: 5–7 s period, bursts to 234 then 1–3 s near-zero |
| E2 | CLIENT-EAST | fieldrecv (stock) | 790,626,304 | 48,258 | 791,301,916 | 1.00085 | 61.565 | **102.738** avg / peak **234.8** | 12.84 | direct (`local=host remote=prflx`) | 10.69 | 0.35 cores (22.09 s / ~64 s) | complete, bad=0; sawtooth: 26 s >150, 23 s <20, mean inst 101.7 |
| E3 | CLIENT-EAST | fieldrecv (stock) | 790,626,304 | 48,259 | 791,301,930 | 1.00085 | 63.664 | **99.351** avg / peak **232.9** | 12.42 | direct (`local=host remote=host`) | 10.69 | 0.36 cores (22.74 s / ~64 s) | complete, bad=0; sawtooth: 20 s >150, 19 s <20, mean inst 97.7 |
| ~~B-E1~~ | CLIENT-EAST | browser | (relayed) | — | — | — | 109.0 | ~~58.0~~ | — | **RELAY FALLBACK — DISCARDED** | 10.69 | — | joined 22:10:50, direct peer **closed** 22:11:00, relay started, complete 22:12:49. Not a direct measurement |
| **B-E2** | CLIENT-EAST | **browser** | 790,626,304 (implied) | — | — | — | 45.0 | **140.56** | 17.57 | direct (lanes ready + relay unused) | 10.69 | — | clicked 22:14:09; lanes ready 22:14:09 → complete 22:14:54 |
| ~~B-E3~~ | CLIENT-EAST | browser | (relayed) | — | — | — | 109.0 | ~~58.0~~ | — | **RELAY FALLBACK — DISCARDED** | 10.69 | — | two peers closed 22:22:58/22:23:00, relay started 22:23:00, complete 22:24:49 |
| **B-E4** | CLIENT-EAST | **browser** | 790,626,304 (implied) | — | — | — | 46.0 | **137.50** | 17.19 | direct (lanes ready + relay unused) | 10.69 | — | clicked 22:25:12; lanes ready 22:25:12 → complete 22:25:58 |
| D1 | CLIENT-EAST | fieldrecv (stock, **client `rmem` 4 MiB**) | 790,626,304 | 48,260 | 791,301,944 | 1.00085 | 45.509 | **138.983** | 17.37 | direct (`local=host remote=prflx`) | 10.69 | — | DIAGNOSTIC. Sawtooth strongly reduced (n>150 = 26, n<20 = 12, mean inst 136.7) |
| D2 | CLIENT-WEST | fieldrecv (stock, **client `rmem` 4 MiB**) | 790,626,304 | 48,260 | 791,301,944 | 1.00085 | 56.753 | **111.447** | 13.93 | direct (`local=host remote=prflx`) | 70.6 | — | DIAGNOSTIC. Unchanged from W3 (111.6) ⇒ WEST is not socket-buffer-limited |
| V1 | CLIENT-WEST | fieldrecv-**rwnd** (`--max-rx-buf 4194304`, `rmem` 4 MiB) | 790,626,304 | 48,261 | 791,301,958 | 1.00085 | 74.543 | **84.850** avg / peak **232.0** | 10.61 | direct (`local=host remote=host`) | 70.6 | — | DIAGNOSTIC VARIANT. Repeating ramp 50→230 Mbps over ~5 s then 1–2 s at zero; net *worse* than stock |

**Rates.** CLIENT-WEST fieldrecv stock n=3: **100.115 / 107.600 / 111.568, mean 106.43** vs browser **64.54**
(E32 unfixed reference 64.84, n=2). CLIENT-EAST fieldrecv stock n=3: **115.259 / 102.738 / 99.351, mean 105.78**
vs browser **140.56 / 137.50, mean 139.03**.

**Framing.** `wire_bytes = payload_bytes + 14 × frames` holds **exactly** in every cell (e.g. W3:
790,626,304 + 14×48,260 = 791,301,944). The resulting ratio **1.00085** is the *DataChannel message* ratio — the
14-byte `file_chunk` envelope only (0.085 %). It is **not comparable** to the campaign's 1.08–1.12× figure, which
E1 defined at **L4 both-directions including SACKs** (1.12× with IP+UDP headers). fieldrecv's counters do not
include SCTP/DTLS/UDP/IP headers or SACKs, so this run neither confirms nor contradicts E1's network-layer ratio.

**Relay discards.** 2 of 4 CLIENT-EAST browser attempts (50 %) silently fell back to the Noise relay — B-E1 and
B-E3, both at exactly 109 s ⇒ 58.0 Mbps on the relay path. Reported, not hidden. Zero relay fallbacks in all
7 fieldrecv cells and the single CLIENT-WEST browser cell.

## Interpretation — which explanation survives

**1. The browser-attribution verdict is CONFIRMED in direction but WRONG in its absolute form.**

- The native receiver beats the browser decisively at 70 ms: **106.4 vs 64.54 Mbps (+65 %)**. The browser's
  DataChannel receive path is real and large — it costs ~45 % of the achievable rate. E1–E32's core observation
  holds.
- The competing hypothesis — *the pion sender's congestion control is the cap* — is **refuted**. The native
  receiver does not land at 64–75; it lands at 106–115, and its steady-state rate is **flat** (114.95 Mbps every
  second in W2/W3, no window-limited ramp), i.e. it is window-blocked at a constant value, not CC-limited to a
  low ceiling.
- **But the campaign's conclusion that the browser is *the* ceiling does not survive.** The native receiver is
  capped at ~115 Mbps, and that cap is a *receiver-stack default*: pion advertises a 1 MiB SCTP receive window
  (`initialRecvBufSize`, pion/sctp v1.9.4 `association.go:74`), giving a hard ceiling of `rwnd / RTT` =
  1 MiB / 0.07075 s = **118.5 Mbps predicted vs 114.95 Mbps measured (97 %)**. The sender is not the limiter.
- **Direct proof that the sender is not CC-capped at 115:** V1 (`--max-rx-buf 4194304`, window ceiling becomes
  4 MiB/70.75 ms = 474 Mbps) lets the same pion sender ramp to **232.0 Mbps sustained-second peaks — 94 % of the
  245 Mbps path capacity** — at the same 70.75 ms RTT. The sender *can* nearly saturate the path. It then
  collapses cyclically (a ~5 s ramp 50→230 Mbps followed by 1–2 s at zero, RTO-collapse signature), netting
  **84.85 Mbps** — *worse* than stock. **The 70 ms path sustains ~115 Mbps, not 232.**

**2. The CLIENT-EAST pair does NOT contradict the WEST result — it exposes a tool artifact.**

The stock native receiver's EAST average (105.8) is *below* the browser (139.0), but the cause is the client's
default **208 KiB UDP socket receive buffer**, not the browser and not the path: at 10.94 ms the sender's window
grows fast and bursts larger than 208 KiB, overflowing the socket → SCTP loss → 1–3 s RTO stalls. Raising
`net.core.rmem_default` to 4 MiB (D1) lifts the same stock binary to **138.98 Mbps** — statistically identical to
the browser's **139.03** — and the sawtooth largely disappears. At 11 ms the browser is therefore **not a cap at
all**; browser and native receiver are equal once the receiver's socket buffer is sane. The same fix does nothing
at 70 ms (D2 111.4 vs W3 111.6) because there the connection is smoothly window-blocked and never bursts the
buffer.

**3. The corrected ordering at 70 ms:** path capacity **245** > pion sender + 4 MiB window, peak **232** (but
unstable, net 85) > stock pion receiver **115 ≈ rwnd/RTT (118.5)** > deployed browser **64.5**.

## Caveats

- **n=3 per fieldrecv cell, n=1–2 per browser cell**; browser cells are agent-side windows with ±1 s (second-
  resolution log) granularity, i.e. ±1.5 % at 45–98 s.
- The 1 MiB-rwnd identification is inferred from a **quantitative match** (118.5 predicted vs 114.95 measured)
  plus the source default and the V1 confirmation that lifting the window lifts the rate — I did not read the
  negotiated SCTP INIT off the wire.
- **D1/D2/V1 changed client state** (`net.core.rmem_default/max`, 208 KiB → 4 MiB). Both clients were restored to
  **212992** afterwards; the change only ever *increased* buffers, and it is not required for any primary cell.
  V1 used a **separate, clearly labelled binary**; the stock `fieldrecv` was never modified in place.
- The client socket-buffer artifact means the **stock fieldrecv numbers are a floor, not a ceiling**, at low RTT.
  Any future native-receiver measurement must set both `net.core.rmem_default` and pion's
  `SetSCTPMaxReceiveBufferSize`.
- **Safety-gate anomaly, reported for honesty:** at ~21:33:25, while the CLIENT-WEST browser cell was running,
  `ps -eo comm | grep -cE "^(node|chrome|Xvfb|fieldrecv)$"` returned **0**. The same command correctly returned
  11 during the CLIENT-EAST browser cell and 1 for a positive control, so the gate works; I could not reproduce
  the 0. It is harmless here — every fieldrecv cell ran *before* any browser was launched, and both clients were
  independently confirmed idle (`pgrep` empty) before the CLIENT-EAST cells and at the end. One 60 s SSH hang and
  several tool timeouts occurred when a backgrounded driver was launched in the same command as a follow-up
  probe; they did not affect any measurement.
- Relay fallback rate on CLIENT-EAST browser attempts was **2/4 (50 %)**, consistent with E27/E28's 54–60 % on
  pinned attempts. Only the relay-free cells are reported as browser references.
- The share expired 2026-10-04 with 500 downloads; consumption went 13 → 26 across this experiment.

## Artifacts

- `fieldrecv` source `agent/cmd/fieldrecv/` (see `2026-10-02-exp37-native-receiver-build.md`); stock binary
  `agent/bin/fieldrecv-linux-amd64` sha256 `8b479970…`; diagnostic variant
  `agent/bin/fieldrecv-rwnd-linux-amd64` sha256 `d7c42581…` (source identical except the `--max-rx-buf` flag in
  `main.go`/`run.go`; `gofmt` and `go vet` clean).
- Per-cell raw logs on the clients: `/tmp/fr-w{1,2,3}.log`, `/tmp/fr-e{1,2,3}.log`, `/tmp/fr-d{1,2}.log`,
  `/tmp/fr-v1.log`, `/tmp/cell-b-w1.log`, `/tmp/cell-b-e{1,2,3,4}.log`; agent side `docker logs sb-run`.
- Pion default located at `github.com/pion/sctp@v1.9.4/association.go:74` (`initialRecvBufSize = 1024*1024`),
  used as `advertisedReceiverWindowCredit` at `association.go:4336`.
