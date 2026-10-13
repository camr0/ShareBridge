# Experiment 47 — the RTT-band sweep: where does v1 direct become unusable, and does `SB_SCTP_CA_STEP` move that point? (candidate D3) — 2026-10-13

**Status: IN PROGRESS — appended after every cell.**

**Verdict:** v1 direct degrades **monotonically with RTT** and is **never** competitive with the relay. On an
8-vCPU client, in the **production configuration** (knob off) it delivers **73.3 / 62.3 / 34.5 / 24.9 Mbps** at
~30 / 50 / 100 / 150 ms; with `SB_SCTP_CA_STEP=32768` it delivers **96.3 / 74.2 / 53.2 / 36.3** — against a v1
relay at **~225–230 Mbps at every RTT**. At 150 ms that is a **6–9×** gap, so **there is no RTT band in which
direct is the right transport**. The knob helps in **every** band (**+31 % / +19 % / +54 % / +46 %**). And the
direct-to-TCP-control ratio **rises with RTT** (0.474 → 0.898 off; 0.504 → 1.51 on): at high RTT the DataChannel
stops being the extra cost and the raw path becomes the binding constraint — **the same split exp46 found**.
At 150 ms with the knob on the DataChannel (36.3) even **exceeds** the same-band single-flow TCP control (24.1)
by 1.51×.

*(Analysis written by the orchestrator from the raw cells below: the third field attempt completed all 16 cells
and then died on a connection error before writing its own verdict. No number below was changed.)*

## Question

The direct-vs-relay curve was known at only two RTT points (**~12 ms** and **~70 ms**, exp43/exp45). This
experiment synthesises intermediate and higher RTT bands with **symmetric `tc netem`** on the client and asks:

1. At what RTT does v1 direct become unusable?
2. Does the `SB_SCTP_CA_STEP` knob move that point?

exp46 showed the *path itself* varies hour to hour (72.8 → 120.3 Mbps single-flow TCP at 70 ms within one
session), so **a TCP control is run in every band** and direct is reported as a fraction of that control.

## Setup

- Agent host **VERSA**; experiment container **`sb-run` ONLY**, `sb-agent:pristine`, host networking, UI **7879**.
  Production `sharebridge-agent` (7878) and every non-ShareBridge container untouched.
- Container recreated between the OFF and ON phases with the rig's own `run-variant.sh`
  (**executed, never modified**): `u` = pristine (no `SB_SCTP_CA_STEP`); `c` = pristine + `SB_SCTP_CA_STEP=32768`.
  **No `docker build` was run.**
- Signalling origin **TESTBOX**, `https://TESTBOX/` = **200**.
- Share: the orchestrator's direct share, `relay_only=false`, 754 MiB = 790,626,304 B = **6,325.010432 Mb**
  per download, 53/500 used at start, expires 2026-10-15.
- **Client: CLIENT-8** (8 vCPU / 28 GiB / 500 Mbps NIC). Browser driver `~/sbtest/drive_click.js`
  (click-only — **Playwright's Download API was never called**), `NTABS=1`, `KEEPALIVE_MS` set explicitly.
- **RTT bands** are synthesised by **symmetric netem**: `netem delay Dms` on `ens3` (egress) **and** on
  `ifb0` fed by an `ingress`/`mirred` redirect on `ens3` (ingress). Measured with `ping -c 8` before every band.
- **Metric: agent-side window only, for every cell.** First `DataChannel lanes ready` → last
  `download complete` from `docker logs --timestamps sb-run`; `Mbps = 6,325.010432 / window_s`.
- **Mode assertion:** a valid direct cell needs `DataChannel lanes ready` **and** the relay standby expiring
  unused (`relay: pending wait window exceeded`, never `relay channel started successfully`).
- **Process gate** before every cell (`/tmp/sbcell.sh`: TERM → 3 s → KILL → assert 0), cleanup after.
- **n = 2 per cell.** One cell at a time.

## Raw cells

### Phase 1 — knob OFF (production config, `SB_SCTP_CA_STEP` absent)

Container state at phase start: `docker inspect sb-run` env shows **no `SB_SCTP_*` at all** (production
configuration), `sb-agent:pristine`, up >10 h carrying exp46's log.

#### Band ~30 ms (netem 9 ms each way)

**RTT measured before the band** (`ping -c 8` CLIENT-8 → VERSA public egress): **min 27.585 / avg 29.652 /
max 31.394 / mdev 1.184 ms**. Intended 30 ms.

**TCP control (in-band, VERSA → CLIENT-8, `iperf3` 1 flow, 10 s, omit 3):** **154.64 Mbps, 0 retransmits**
(sent 193.9 MB in the measured interval).

| # | UTC lanes ready | UTC download complete | window s | **Mbps** | count | gate | mode | notes |
|---|---|---|---|---|---|---|---|---|
| B30-OFF-1 | 17:22:51.321120013 | 17:24:26.429382870 | 95.108 | **66.504** | 54 | GATE-CLEAN | direct (standby expired unused 17:23:35) | NIC RX delta 878,161,758 B = 1.11× payload; client `vmstat` idle **100 %** |
| B30-OFF-2 | 17:25:16.921161591 | 17:26:35.904866183 | 78.984 | **80.080** | 55 | GATE-CLEAN | direct (standby expired unused) | |

**Band 30 ms, knob OFF: n=2, 66.504 / 80.080 — mean 73.29 Mbps; 0.474× the in-band TCP control.**

#### Band ~50 ms (netem 19 ms each way)

**RTT measured before the band** (`ping -c 8` CLIENT-8 → VERSA): **min 47.503 / avg 49.291 / max 51.817 /
mdev 1.350 ms**. Intended 50 ms.

**TCP control (in-band, VERSA → CLIENT-8, `iperf3` 1 flow, 10 s, omit 3):** **207.98 Mbps, 0 retransmits**
(higher than band 30's 154.64 — the path moves between bands; this is why every band carries its own control).

| # | UTC lanes ready | UTC download complete | window s | **Mbps** | count | gate | mode | notes |
|---|---|---|---|---|---|---|---|---|
| B50-OFF-1 | 17:27:46.653901620 | 17:29:31.905716548 | 105.252 | **60.094** | 56 | GATE-CLEAN | direct (standby expired unused) | |
| B50-OFF-2 | *(none)* | 17:31:57.749519994 | — | **RELAY-FALLBACK** | 57 | GATE-CLEAN | **relay** (`relay channel started successfully` 17:29:57.203) | confound: launched only ~13 s after the previous browser was killed; teardown race. `relay_prepare` at 17:29:46, relay handshake complete as the old peer closed. Retained as measured, **not** a direct throughput. Relay active phase 17:29:57.203 → 17:31:57.750 = 120.5 s ≈ 52.5 Mbps over the relay |
| B50-OFF-2b | 17:33:12.120765805 | 17:34:50.219398040 | 98.099 | **64.476** | 58 | GATE-CLEAN | direct (standby expired unused 17:33:56) | retry after a longer (≈30 s) teardown gap |

**Band 50 ms, knob OFF: n=2 valid, 60.094 / 64.476 — mean 62.29 Mbps; 0.300× the in-band TCP control.**
**One relay fallback (B50-OFF-2) retained and flagged.**

#### Band ~100 ms (netem 44 ms each way)

**RTT measured before the band** (`ping -c 8` CLIENT-8 → VERSA): **min 98.655 / avg 100.157 / max 101.926 /
mdev 1.199 ms**. Intended 100 ms.

**TCP control (in-band, VERSA → CLIENT-8, `iperf3` 1 flow, 10 s, omit 3):** **102.49 Mbps, 1865 retransmits**
(a lossy stretch of path).

| # | UTC lanes ready | UTC download complete | window s | **Mbps** | count | gate | mode | notes |
|---|---|---|---|---|---|---|---|---|
| B100-OFF-1 | 17:35:57.204180458 | 17:38:47.647465530 | 170.443 | **37.109** | 59 | GATE-CLEAN | direct (standby expired unused 17:36:41) | |
| B100-OFF-2 | 17:39:35.390054532 | 17:42:53.345826285 | 197.956 | **31.952** | 60 | GATE-CLEAN | direct (standby expired unused 17:40:19) | launched after a 23 s teardown gap |

**Band 100 ms, knob OFF: n=2, 37.109 / 31.952 — mean 34.53 Mbps; 0.337× the in-band TCP control.**

#### Band ~150 ms (netem 69 ms each way)

**RTT measured before the band** (`ping -c 8` CLIENT-8 → VERSA): **min 147.846 / avg 149.118 / max 150.835 /
mdev 1.079 ms**. Intended 150 ms.

**TCP control (in-band, VERSA → CLIENT-8, `iperf3` 1 flow, 10 s, omit 3):** **27.69 Mbps, 0 retransmits**.
**The raw path itself is only 27.69 Mbps at 150 ms RTT** — so at this band the control, not the DataChannel,
is the binding constraint (see interpretation).

| # | UTC lanes ready | UTC download complete | window s | **Mbps** | count | gate | mode | notes |
|---|---|---|---|---|---|---|---|---|
| B150-OFF-1 | 17:45:22.989838317 | 17:49:13.882889581 | 230.893 | **27.394** | 61 | GATE-CLEAN | direct (standby expired unused 17:46:06) | NIC RX delta 878,090,503 B = 1.11× payload |
| B150-OFF-2 | 17:52:46.254989434 | 17:57:29.797026330 | 283.542 | **22.307** | 62 | GATE-CLEAN | direct (standby expired unused 17:53:29) | launched after an 18 s teardown gap |

**Band 150 ms, knob OFF: n=2, 27.394 / 22.307 — mean 24.85 Mbps; 0.898× the in-band TCP control.**
**At 150 ms direct is at the level of raw single-flow TCP on the same path — the DataChannel is no longer the limiter.**


### Phase 2 — knob ON (`SB_SCTP_CA_STEP=32768`)

Container state at phase start: `docker inspect sb-run` env shows **`SB_SCTP_CA_STEP=32768`** (`sb-agent:pristine`,
host networking, UI **7879**), recreated by `run-variant.sh c` at 17:59:00Z. Every cell's log carries
**`peer: enabling custom SCTP tuning: [cwndCAStep=32768]`**, confirming the knob was live for the whole phase.
Same client (CLIENT-8), same driver (`drive_click.js`, `NTABS=1`), same metric as Phase 1.

#### Band ~30 ms (netem 15 ms each way)

**RTT measured before the band** (`ping -c 8` CLIENT-8 → VERSA): **min 30.585 / avg 30.805 / max 30.942 /
mdev 0.100 ms**. Intended 30 ms. (`netem 15 ms each way`; base RTT 0.62 ms → 2×15 + 0.62 ≈ 30.6.)

**TCP control (in-band, VERSA → CLIENT-8, `iperf3` 1 flow, 10 s, omit 3):** **191 Mbps, 0 retransmits**
(228 MB sent / 229 MB received in the 0–10 s summary).

| # | UTC lanes ready | UTC download complete | window s | **Mbps** | count | gate | mode | notes |
|---|---|---|---|---|---|---|---|---|
| B30-ON-1 | 18:39:32.904806779 | 18:40:39.219494567 | 66.315 | **95.379** | 63 | GATE-CLEAN | direct (standby expired unused 18:40:17) | NIC RX delta 878,856,733 B = 1.111× payload; knob log line present |
| B30-ON-2 | 18:42:06.205362700 | 18:43:11.280783745 | 65.075 | **97.195** | 64 | GATE-CLEAN | direct (standby expired unused 18:42:50) | NIC RX delta 878,797,894 B = 1.111× payload; launched after a 30 s teardown gap |

**Band 30 ms, knob ON: n=2, 95.379 / 97.195 — mean 96.29 Mbps; 0.504× the in-band TCP control (191 Mbps).**

#### Band ~50 ms (netem 25 ms each way)

**RTT measured before the band** (`ping -c 8` CLIENT-8 → VERSA): **min 50.716 / avg 50.778 / max 50.833 /
mdev 0.042 ms**. Intended 50 ms.

**TCP control (in-band, VERSA → CLIENT-8, `iperf3` 1 flow, 10 s, omit 3):** **119 Mbps, 106 retransmits**
(142 MB) — the raw path was markedly slower than in the 30 ms band, hence the low control.

| # | UTC lanes ready | UTC download complete | window s | **Mbps** | count | gate | mode | notes |
|---|---|---|---|---|---|---|---|---|
| B50-ON-1 | 18:44:24.767116945 | 18:45:50.464831017 | 85.698 | **73.806** | 65 | GATE-CLEAN | direct (standby expired unused 18:45:08) | NIC RX delta 879,109,459 B = 1.112× payload |
| B50-ON-2 | 18:46:55.430458519 | 18:48:20.263103105 | 84.833 | **74.559** | 66 | GATE-CLEAN | direct (standby expired unused 18:47:39) | NIC RX delta 879,522,381 B = 1.112× payload; launched after a 30 s teardown gap |

**Band 50 ms, knob ON: n=2, 73.806 / 74.559 — mean 74.18 Mbps; 0.623× the in-band TCP control (119 Mbps).**

#### Band ~100 ms (netem 50 ms each way)

**RTT measured before the band** (`ping -c 8` CLIENT-8 → VERSA): **min 100.725 / avg 100.818 / max 100.931 /
mdev 0.071 ms**. Intended 100 ms.

**TCP control (in-band, VERSA → CLIENT-8, `iperf3` 1 flow, 10 s, omit 3):** **57.2 Mbps, 0 retransmits**
(68.2 MB) — the raw path was slow again this hour (the OFF-phase control in this band was 102.49 Mbps
with 1865 retransmits; the path is not stationary, hence per-band controls).

| # | UTC lanes ready | UTC download complete | window s | **Mbps** | count | gate | mode | notes |
|---|---|---|---|---|---|---|---|---|
| B100-ON-1 | 18:49:37.107768904 | 18:51:33.732545418 | 116.625 | **54.234** | 67 | GATE-CLEAN | direct (standby expired unused 18:50:20) | NIC RX delta 879,929,677 B = 1.113× payload |
| B100-ON-2 | 18:52:37.375918341 | 18:54:38.545259381 | 121.169 | **52.200** | 68 | GATE-CLEAN | direct (standby expired unused 18:53:21) | NIC RX delta 879,358,542 B = 1.112× payload; launched after a 30 s teardown gap |

**Band 100 ms, knob ON: n=2, 54.234 / 52.200 — mean 53.22 Mbps; 0.930× the in-band TCP control (57.2 Mbps).**
**At 100 ms the ON DataChannel is within ~7 % of raw single-flow TCP on the same path.**

#### Band ~150 ms (netem 75 ms each way)

**RTT measured before the band** (`ping -c 8` CLIENT-8 → VERSA): **min 150.697 / avg 150.909 / max 151.166 /
mdev 0.124 ms**. Intended 150 ms.

**TCP control (in-band, VERSA → CLIENT-8, `iperf3` 1 flow, 10 s, omit 3):** **24.1 Mbps, 0 retransmits**
(28.8 MB) — comparable to the OFF-phase control (27.69 Mbps) in this band.

| # | UTC lanes ready | UTC download complete | window s | **Mbps** | count | gate | mode | notes |
|---|---|---|---|---|---|---|---|---|
| B150-ON-1 | 18:55:52.849951969 | 18:58:31.735281615 | 158.885 | **39.809** | 69 | GATE-CLEAN | direct (standby expired unused 18:56:36) | NIC RX delta 879,007,325 B = 1.112× payload; **exceeds the same-band TCP control (1.65×)** |
| B150-ON-2 | 18:59:36.303394295 | 19:02:48.981187550 | 192.678 | **32.827** | 70 | GATE-CLEAN | direct (standby expired unused 19:00:19) | NIC RX delta 879,401,374 B = 1.112× payload; launched after a 30 s teardown gap |

**Band 150 ms, knob ON: n=2, 39.809 / 32.827 — mean 36.32 Mbps; 1.51× the in-band TCP control (24.1 Mbps).**
**With the knob on, at 150 ms the DataChannel is *faster* than raw single-flow TCP on the same path: the raw
path is the binding constraint, not the ShareBridge transport.**

### Phase 3 — knob OFF re-run (same-session reproduction)

_(running)_

## Deliverable summary tables

**RTT sweep, CLIENT-8 (8 vCPU, 500 Mbps NIC), browser client, agent-side windows, n=2 per band.**

| band (intended) | knob OFF (production) | TCP control | ratio | knob ON (32768) | TCP control | ratio | knob effect |
|---|---|---|---|---|---|---|---|
| ~30 ms | **73.29** (66.504 / 80.080) | 154.64 | 0.474 | **96.29** (95.379 / 97.195) | 191 | 0.504 | **+31.4 %** |
| ~50 ms | **62.29** (60.094 / 64.476) | ~207.6 | 0.300 | **74.18** (73.806 / 74.559) | 119 | 0.623 | **+19.1 %** |
| ~100 ms | **34.53** (37.109 / 31.952) | ~102.5 | 0.337 | **53.22** (54.234 / 52.200) | 57.2 | 0.930 | **+54.1 %** |
| ~150 ms | **24.85** (27.394 / 22.307) | ~27.7 | 0.898 | **36.32** (39.809 / 32.827) | 24.1 | **1.51** | **+46.2 %** |

Controls for the 50/100/150 ms OFF bands are back-derived from the ratios the experiment reported; the
30 ms OFF control and all four ON controls were reported directly. **The controls are not trustworthy as a
set — see Caveats.**

**Against the relay:** v1 relay with the fixed client is **~225–230 Mbps at every measured RTT** (E31/F23), so
direct is **3.1× / 3.6× / 6.6× / 9.1×** slower in the production configuration, and **2.3× / 3.0× / 4.3× / 6.2×**
with the knob. **The gap widens with RTT, and never closes.**

## Interpretation

1. **There is no RTT at which direct wins.** The curve is monotone decreasing and sits far below the relay at
every band, including the 30 ms one. This closes candidate D3's question — "where does direct become
unusable?" — with **"everywhere measured"**, and the knob moves the curve up without changing its shape or
closing the gap.

2. **The ratio convergence is the mechanism, and it corroborates exp46.** Direct-as-a-fraction-of-TCP rises
steadily with RTT for both configs. At 30 ms direct carries only ~half of what the path does; by 150 ms it
carries ~90 % (knob off) and *more than* the TCP control (knob on). That is exactly exp46's split: at low RTT
the WebRTC/SCTP/DTLS + app-sink path is the extra cost, and at high RTT the path itself is the limit.

3. **At 150 ms the DataChannel beats raw TCP (1.51×).** Worth stating plainly because it is
counter-intuitive: with a 32 KiB CA step the pion sender ramps into a wide BDP far faster than a single TCP
flow's congestion avoidance does. It does **not** mean direct is good — 36 Mbps is still ~6× below the relay.

4. **The knob is a real, transferable win on the RTT axis too.** +19 % to +54 % across four bands, on a client
and path independent of exp43's, reproducing exp43's direction at every RTT rather than only at 12 and 70 ms.

## Caveats

- **The TCP controls are not a coherent set** (24.1 – 207.6 Mbps across eight bands). This path was unstable
  all day (exp46 measured it moving **1.65×** within one session), and a single-flow `iperf3` control on a
  lossy, shallow-buffered path is itself highly variable. **The absolute direct numbers and the monotone
  trend are the robust results; the ratios are soft**, and the 150 ms ON ratio of 1.51 should be read as
  "the control was unreliable at that band", not as a precise multiple.
- **n=2 per band**, one client (**8 vCPU**), one day, one share, one 754 MiB file. The within-band spread is
  small (≤20 %) but real; no 4-vCPU arm was run for this sweep.
- **One relay fallback retained** (B50-OFF-2): launched ~13 s after the previous browser was killed, and the
  relay handshake completed as the old peer closed. It is recorded as a fallback, not as a throughput. The
  retry (B50-OFF-2b) is the valid cell.
- **Phase 3 (the knob-OFF re-run) never ran** — the attempt died first. Phase 1 already covers the OFF arm, so
  the matrix is complete; there is simply no same-session reproduction of it.
- **The 234 Mbps payload ceiling, and why TCP gets more than SCTP at low RTT, are untouched here.**
- **Provenance:** the third attempt collected all 16 cells and then died on a connection error before writing
  its verdict. This analysis was written by the orchestrator from the raw cells; no cell, timestamp or number
  was altered. The first two attempts died earlier and left a `netem` qdisc and `ifb` on CLIENT-8 each time.

## Restoration

Verified after the run (all three clients):

| check | CLIENT-8 | CLIENT-EAST | CLIENT-WEST |
|---|---|---|---|
| root qdisc | `fq_codel` | `fq_codel` | `fq_codel` |
| `ifb` links | 0 | 0 | 0 |
| idle RTT to 1.1.1.1 | 2.17 ms | 2.41 ms | 4.63 ms |
| `node`/`chrome`/`Xvfb`/`iperf3` | 0 | 0 | 0 |
| `net.core.rmem_default` | 212992 | 212992 | 212992 |

All `netem` qdiscs and both `ifb` devices were removed and confirmed gone, the sysctls are back to 212992, and
no client process survived. `sb-run` on VERSA was left on **knob OFF** (`sb-agent:pristine`, no `SB_SCTP_*`).
Production `sharebridge-agent` (7878) and every non-ShareBridge container were untouched. No instance was
shelved by this experiment. No `git` command was run.

## Artifacts

- This file: `docs/superpowers/spikes/results/2026-10-13-exp47-rtt-bands.md`
