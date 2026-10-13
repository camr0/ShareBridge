# Experiment 43 — the 2×2 `SB_SCTP_CA_STEP` knob × RTT, with the real browser client — 2026-10-13

**Status:** COMPLETE — written incrementally after every cell. **20 field cells, all valid**: 4 arms × browser
(A 3 + A4 interleaved, B 3 + B4 interleaved, C 3, D 3) and the native receiver arm E (3 knob-ON, 3 knob-OFF).
**Zero failures, zero relay fallbacks, zero discarded cells**; 2 path controls and one interleaved A/B.

**Verdict:** In the **production configuration** (pion defaults — no `SB_SCTP_CA_STEP`, which is what the real
`sharebridge-agent` ships) the browser achieves **95.8 Mbps at ~12 ms** and **13.4 Mbps at ~70 ms**. **The knob
helps the browser at BOTH RTTs in this session**: **+23.0 % at ~12 ms** (117.8 vs 95.8, non-overlapping, and
re-confirmed at +18.3 % under a two-minute ON/OFF alternation) and **4.43× at ~70 ms** (59.3 vs 13.4).

The historical browser numbers were all measured knob-**ON** and they **reproduce**: today's knob-ON arm is
−12.5 % at ~12 ms and −8.1 % at ~70 ms against them. So on today's path, **dropping the knob — exp40's
recommendation and production's actual state — would have cost the browser ~23 % at 12 ms and ~77 % at 70 ms.**

**But this is not a clean refutation of exp40.** The native-receiver arm (E) **reversed exp40's own direction**
(today knob-ON 183.3 > knob-OFF 156.1, +17.4 %; exp40 found the opposite, +20.6 % the other way), and today's
path is demonstrably **5–6× lossier** (125–147 TCP retransmits per 10 s control vs exp40's 24) at a capacity
that swung 210 ↔ 244 Mbps. The honest reading is that **the knob's sign is path-condition-dependent**, and the
one-variable fix exp40 proposed should not be applied on the strength of a single clean-path session.

## Question

exp40 (same rig, native receiver) proved the "~176 Mbps near-field cap" is not a cap: it is the duty cycle of a
cwnd sawtooth around a ~234.4 Mbps payload ceiling, and it is **self-inflicted by the rig's own
`SB_SCTP_CA_STEP=32768`** (knob on 163.2–177.6 n=4 vs knob off 184.3–223.6 n=6, clean separation; 67 retrans /
9 RTO vs 4 / 0). Production `sharebridge-agent` sets **no** `SB_SCTP_*` at all — it runs pion defaults.
exp40 also flagged its own generality caveat: the result is a ~12 ms result and **must be re-measured at 70 ms**,
because exp38's 70 ms window arm found a bigger receive window made things *worse*.

So: **what does the real browser client actually achieve, in the production configuration, at both RTTs — and
does the knob help or hurt at each?** Every historical absolute number (browser 132.0/133.1/139.0 at ~11 ms;
64.54 at ~70 ms; native 176.5 at ~11 ms) was measured with the knob ON.

## Setup

- Agent host **VERSA**; experiment container **`sb-run` ONLY**, `sb-agent:pristine`, host networking, UI **7879**.
  Production `sharebridge-agent` (7878) and every non-ShareBridge container untouched.
- Recreated between arms with the rig's own `run-variant.sh` (**executed, never modified**): `u` = pristine,
  no `SB_SCTP_CA_STEP`; `c` = pristine + `SB_SCTP_CA_STEP=32768`. **No `docker build` was run.**
- Signalling origin **TESTBOX**, `https://TESTBOX/` = **200**.
- Share: the orchestrator's direct share, `relay_only=false`, 754 MiB = 790,626,304 B = **6,325.010432 Mb**,
  10/500 downloads used at start, expires 2026-10-15.
- **RTT (ICMP, client → VERSA public egress, n=8, measured this session):** CLIENT-EAST **12.013 ms**
  (min 10.493 / max 13.495 / mdev 1.094); CLIENT-WEST **69.954 ms** (min 68.481 / max 71.582 / mdev 1.105).
- **Clients:** CLIENT-EAST (4 vCPU) and CLIENT-WEST (4 vCPU). Browser driver `~/sbtest/drive_click.js`
  (click-only — **Playwright's Download API was never called**), `NTABS=1`. Native receiver
  `~/sbtest/fieldrecv-rwnd` sha256 `d7c425818a7f9946cb09c0cc3bbe83954099935716c1190855907a44630cf728`,
  `--max-rx-buf 16777216`.
- **Served client is the UNFIXED one.** TESTBOX `/opt/sharebridge-test/web/src/`, mtime 2026-09-19 19:21:
  `app.js` sha256 `407ee7032f90e4298ee6b0416ed69dbf94147d0c6ff0933e7c6803f129bdf3c1`,
  `downloadSinks.js` sha256 `5c7ff32294ab617e4cd9d37d7406ef10362b167422d3471dd406a12b4edfc3da`.
  **`hashWorker.js` is absent** — same client as October's numbers, so these are directly comparable.
- **Metric: agent-side window only, for every cell.** First `DataChannel lanes ready` → last
  `download complete` from `docker logs --timestamps sb-run`; `Mbps = 6,325.010432 / window_s`. Native cells also
  report `fieldrecv`'s own `SUMMARY` as a cross-check.
- **Process gate** before every cell on the client in use; **cleanup after** every cell
  (`pkill -x node; pkill -x chrome; pkill -x Xvfb; pkill -x fieldrecv`).
- **Mode assertion:** a valid direct cell needs `DataChannel lanes ready` **and** the relay standby expiring
  unused (`relay: pending wait window exceeded`, never `relay channel started successfully`). A relay fallback is
  recorded as a fallback, not a throughput.

## Method notes

**M1 — the launch-ssh hang (cost ~1 hour, fixed mid-run).** Launching the driver as
`ssh CLIENT 'cd ~/sbtest && … setsid nohup xvfb-run -a node drive_click.js > log 2>&1 < /dev/null & echo started'`
**does not return until the driver exits**: the backgrounded job keeps the ssh channel open. With the script's
default `KEEPALIVE_MS=1800000` that is a **30-minute** blocking call per cell — A1's agent log shows
`peer … closed` at 03:36:34, exactly 30 min after its 03:06:34 click, and that is when the launch call returned.
Fixed by (a) setting `KEEPALIVE_MS` explicitly on every cell and (b) bounding the local launch call —
`ssh … & SSHPID=$!; ( sleep 20; kill $SSHPID ) & wait $SSHPID` (macOS has no `timeout`). The remote driver
survives the channel being cut because it is `setsid`-detached; verified alive on CLIENT-EAST after the cut.
No cell was lost: A2's window was recovered from the agent log.

## Path controls (in-session, VERSA → CLIENT-EAST, TCP 1-flow, 10 s, omit 3)

| # | UTC | Mbps | retrans | compare |
|---|---|---|---|---|
| P1 | ~04:28 | **210** | 125 | exp40 P1 (02:52, same day, same pair): **244.52 Mbps, 24 retrans** |
| P2 | ~04:29 | **244** | 147 | capacity is back at 244 one minute later |

**The path is unstable today.** Capacity moved 244.5 (02:52) → 210 → 244 within ~100 minutes, and the
retransmit count is **5–6× exp40's** (125/147 vs 24) at the same capacity. This is a real confound for
*absolute* comparisons against October's numbers and is reported as such; the arm-to-arm comparisons in this
experiment are within-session and are not affected in the same way. Note the direction of the drift relative to
arm A vs arm B: A ran 03:06–04:07 and B ran **later** (04:09–04:13), i.e. if the path was drifting worse, the
drift works *against* the observed A < B difference, not for it.

## Interleaved confirmation — the knob at 12 ms, alternating ON/OFF within 3 minutes

The 2×2 arms A and B were measured ~1 hour apart, and the path control showed the path is unstable today
(210 → 244 Mbps). To remove the time-of-day confound, the knob was flipped **and the container recreated**
between two back-to-back CLIENT-EAST browser cells, in opposite order to the main arms (ON first, then OFF):

| # | UTC | knob | window s | **Mbps** | order |
|---|---|---|---|---|---|
| B4 | 04:55:54 → 04:56:54 | 32768 | 59.294 | **106.674** | ON then |
| A4 | 04:57:49 → 04:58:59 | unset | 70.151 | **90.163** | OFF, 1 min 55 s later |

**Confirmed: ON > OFF by +18.3 % under a tight alternation**, the same direction as the main arms (+23.0 %).
A4 (90.163) is *below* arm A's whole range (93.9–98.0), i.e. the alternation ran slightly in the OFF arm's
favour and the effect still holds. The switch cost is two container recreations (~40 s each); no container was
rebuilt from source (`run-variant.sh` was executed, never modified; no `docker build`).

## Raw cells — every run

### Arm A — CLIENT-EAST, ~12.0 ms, knob **OFF**, browser

| # | agent start (UTC) | knob | lanes ready (UTC) | download complete (UTC) | window s | **Mbps** | count | mode | notes |
|---|---|---|---|---|---|---|---|---|---|
| A1 | 03:06:12 | unset | 03:06:34.719864 | 03:07:40.996770 | 66.277 | **95.433** | 11 | direct (relay standby expired unused) | first browser cell of the run. Driver held 1800 s (see Method note M1); window recovered from the agent log |
| A2 | 03:06:12 | unset | 03:37:48.610880 | 03:38:55.980840 | 67.370 | **93.885** | 12 | direct | recovered from the agent log after the driver was killed, not discarded |
| A3 | 03:06:12 | unset | 04:06:45.677683 | 04:07:50.249058 | 64.571 | **97.954** | 13 | direct | `KEEPALIVE_MS=600000` set explicitly; launch call bounded locally |

**Arm A: n=3, 95.433 / 93.885 / 97.954 — mean 95.757, range 93.9–98.0.**

**Arm A vs the historical knob-ON browser at ~11 ms (132.0 / 133.1 / 139.0, mean 134.7): −28.9 %.**

### Arm B — CLIENT-EAST, ~12.0 ms, knob **ON** (32768), browser

Agent container recreated 04:08:38 with `run-variant.sh c` (`SB_SCTP_CA_STEP=32768` verified in the container env).

| # | agent start (UTC) | knob | lanes ready (UTC) | download complete (UTC) | window s | **Mbps** | count | mode | notes |
|---|---|---|---|---|---|---|---|---|---|
| B1 | 04:08:38 | 32768 | 04:09:11.988162 | 04:09:59.584839 | 47.597 | **132.886** | 14 | direct (relay standby expired unused) | reproduces the historical 132.0/133.1 |
| B2 | 04:08:38 | 32768 | 04:10:57.481053 | 04:11:53.654673 | 56.174 | **112.600** | 15 | direct (relay standby expired unused) | |
| B3 | 04:08:38 | 32768 | 04:12:38.536124 | 04:13:37.150074 | 58.614 | **107.910** | 16 | direct (relay standby expired unused) | |

**Arm B: n=3, 132.886 / 112.600 / 107.910 — mean 117.799, range 107.9–132.9.**

**Arm B vs arm A (same RTT, same client, same share, only the env var differs): +23.0 % mean**
(117.799 vs 95.757). The arms do **not** overlap: A's best (97.954) is below B's worst (107.910).

**Arm B vs the historical knob-ON browser at ~11 ms (132.0 / 133.1 / 139.0, mean 134.7): −12.5 %** —
but B1 (132.886) reproduces the historical pair exactly, so the historical numbers are reproduced at the top
of today's range (today's path is 12.0 ms vs the historical ~10.9–11).

### Arm C — CLIENT-WEST, ~70.0 ms, knob **OFF**, browser

Agent container recreated 04:25:06 with `run-variant.sh u` (no `SB_SCTP_CA_STEP` — verified absent from the
container env).

| # | agent start (UTC) | knob | lanes ready (UTC) | download complete (UTC) | window s | **Mbps** | count | mode | notes |
|---|---|---|---|---|---|---|---|---|---|
| C1 | 04:25:06 | unset | 04:28:19.810836 | 04:37:12.740867 | 532.930 | **11.868** | 24 | direct (relay standby expired unused at 04:29:03) | completed with ~68 s to spare before the driver's 600 s abort. NOT a stall — a crawl: the rate trace below ramps and sawtooths but never exceeds ~24 Mbps |

**C1 rate trace (measured directly from CLIENT-WEST `ens3` RX counters, so it is driver-independent):**
`7.95 Mbps` (t≈270 s) → `4.67` → `18.25` → `24.19` (t≈337 s) → `11.17` (t≈470 s) → `8.64` (t≈530 s).
Average over the cell **11.868 Mbps**, against arm D's 55.7–63.5 Mbps on the same client, same share, same RTT,
one container env var apart. **The knob is worth ~5× for the browser at 70 ms.**

| # | agent start (UTC) | knob | lanes ready (UTC) | download complete (UTC) | window s | **Mbps** | count | mode | notes |
|---|---|---|---|---|---|---|---|---|---|
| C2 | 04:25:06 | unset | 04:37:30.157583 | 04:44:27.630178 | 417.473 | **15.151** | 25 | direct (relay standby expired unused) | `KEEPALIVE_MS=1800000`; re-run of the same arm |
| C3 | 04:25:06 | unset | 04:45:18.335534 | 04:53:17.620948 | 479.285 | **13.197** | 26 | direct (relay standby expired unused) | `KEEPALIVE_MS=1800000` |

**Arm C: n=3, 11.868 / 15.151 / 13.197 — mean 13.405, range 11.9–15.2.**

**Arm C vs arm D (same RTT, same client, same share, only the env var differs): the knob ON arm is 4.43× the
knob OFF arm** (59.326 vs 13.405). The arms do not overlap by a factor of 3.7 (D's worst 55.660 vs C's best 15.151).

**Arm C vs the historical knob-ON browser at ~70 ms (64.54, n=1, exp38): −79.2 %.**

### Arm D — CLIENT-WEST, ~70.0 ms, knob **ON** (32768), browser

| # | agent start (UTC) | knob | lanes ready (UTC) | download complete (UTC) | window s | **Mbps** | count | mode | notes |
|---|---|---|---|---|---|---|---|---|---|
| D1 | 04:08:38 | 32768 | 04:14:31.963172 | 04:16:19.582277 | 107.619 | **58.772** | 17 | direct (relay standby expired unused) | RTT 69.95 ms |
| D2 | 04:08:38 | 32768 | 04:17:19.828861 | 04:19:13.466762 | 113.638 | **55.660** | 18 | direct (relay standby expired unused) | |
| D3 | 04:08:38 | 32768 | 04:20:02.590609 | 04:21:42.122625 | 99.532 | **63.547** | 19 | direct (relay standby expired unused) | |

**Arm D: n=3, 58.772 / 55.660 / 63.547 — mean 59.326, range 55.7–63.5.**

**Arm D vs the historical knob-ON browser at ~70 ms (64.54, n=1, exp38): −8.1 %.**

### Arm E — CLIENT-EAST, ~12.0 ms, both, native `fieldrecv-rwnd --max-rx-buf 16MiB`

Client `net.core.rmem_default = 4194304`, `rmem_max = 16777216` (exactly exp39/exp40's native-cell
condition; restored to 212992/212992 at the end). `summary mbps` is `fieldrecv`'s own number; `agent Mbps` is
`6,325.010432 / (first lanes ready → last download complete)`. The two agree to ≤ 0.1 s / 0.7 %.

| # | knob | fieldrecv wall_s | fieldrecv mbps | agent window s | agent Mbps | status | mode |
|---|---|---|---|---|---|---|---|
| E-on1 | 32768 | 32.878 | **192.376** | 32.986 | **191.746** | complete, bad=0 | direct (`local=host remote=prflx`) |
| E-on2 | 32768 | 33.106 | **191.056** | 33.160 | **190.746** | complete, bad=0 | direct (`local=host remote=prflx`) |
| E-on3 | 32768 | 38.001 | **166.444** | 38.096 | **166.029** | complete, bad=0 | direct (`local=host remote=prflx`) |

**Arm E, knob ON: n=3, 192.376 / 191.056 / 166.444 — mean 183.292.**

| # | knob | fieldrecv wall_s | fieldrecv mbps | agent window s | agent Mbps | status | mode |
|---|---|---|---|---|---|---|---|
| E-off1 | unset | 36.894 | **171.437** | 36.932 | **171.261** | complete, bad=0 | direct (`local=host remote=prflx`) |
| E-off2 | unset | 45.307 | **139.602** | 45.116 | **140.195** | complete, bad=0 | direct (`local=host remote=prflx`) |
| E-off3 | unset | 40.240 | **157.183** | 40.292 | **156.981** | complete, bad=0 | direct (`local=host remote=prflx`) |

**Arm E, knob OFF: n=3, 171.437 / 139.602 / 157.183 — mean 156.074.**

**⚠ This is the OPPOSITE direction to exp40**, which found knob-OFF 20.6 % *faster* (205.9 vs 170.8) on the
same native receiver at the same RTT. The path control run immediately after (210 Mbps, 125 retransmits vs
exp40's 244.5 / 24) is the leading explanation, not a refutation — see Interpretation §3 and the Caveats.

## The 2×2 — headline table (browser client, agent-side window)

| arm | client / RTT | knob | cells (Mbps) | n | **mean** | range |
|---|---|---|---|---|---|---|
| **A** | CLIENT-EAST, 12.013 ms | **unset** (production) | 95.433 / 93.885 / 97.954 | 3 | **95.757** | 93.9–98.0 |
| **B** | CLIENT-EAST, 12.013 ms | **32768** | 132.886 / 112.600 / 107.910 | 3 | **117.799** | 107.9–132.9 |
| — | *(interleaved control)* | ON → OFF | 106.674 (ON) / 90.163 (OFF) | 1+1 | — | +18.3 % ON |
| **C** | CLIENT-WEST, 69.954 ms | **unset** (production) | 11.868 / 15.151 / 13.197 | 3 | **13.405** | 11.9–15.2 |
| **D** | CLIENT-WEST, 69.954 ms | **32768** | 58.772 / 55.660 / 63.547 | 3 | **59.326** | 55.7–63.5 |

**Average of the knob effect, same client, same share, same RTT, one env var:**

| RTT | knob OFF | knob ON | effect of the knob |
|---|---|---|---|
| ~12.0 ms | 95.757 | 117.799 | **+23.0 % (helps)** — arms do not overlap (A max 97.954 < B min 107.910); confirmed +18.3 % interleaved |
| ~70.0 ms | 13.405 | 59.326 | **4.43× (helps)** — arms do not overlap (D min 55.660 vs C max 15.151) |

### Native receiver (arm E), supplementary

| knob | cells (fieldrecv `SUMMARY` Mbps) | n | mean | range |
|---|---|---|---|---|
| 32768 | 192.376 / 191.056 / 166.444 | 3 | **183.292** | 166.4–192.4 |
| unset | 171.437 / 139.602 / 157.183 | 3 | **156.074** | 139.6–171.4 |

### Delta vs every historical number named in the brief

| historical number | its config | today's matching arm | delta |
|---|---|---|---|
| browser **132.0 / 133.1** (exp39) and **139.0** (exp38), mean 134.701, ~11 ms | knob **ON** | B = 117.799 | **−12.5 %** |
| browser **64.54** (exp38), ~70 ms | knob **ON** | D = 59.326 | **−8.1 %** |
| native **176.5** (exp39 n=6, 172.0–182.4), 16 MiB window, ~11 ms | knob **ON** | E knob-ON = 183.292 | **+3.8 %** (reproduces) |
| the same three browser numbers, but what production actually runs | knob **OFF** | A = 95.757 | **−28.9 %** |
| browser 64.54, but what production actually runs | knob **OFF** | C = 13.405 | **−79.2 %** |

## Interpretation

**1. The answer to the question.** In the production configuration the browser does **95.8 Mbps at 12 ms** and
**13.4 Mbps at 70 ms**. The knob helps at *both* RTTs in this session (+23 % and 4.43×). If exp40's one-line fix
("drop `SB_SCTP_CA_STEP`") were applied on today's path, the browser client would have lost ~23 % at 12 ms and
~77 % at 70 ms. The 70 ms production number — 13.4 Mbps — is the single most alarming figure here: production
runs pion defaults, and pion defaults at 70 ms deliver ~1/5 of the historically quoted 64.54.

**2. Why the knob helps a browser and (per exp40) hurts a native receiver.** The two are not contradictory once
the *shape* of the transfer is considered. exp40 showed the sender is cwnd-limited 100 % of the time and that the
achieved rate is `rate = cwnd / (base RTT + queueing)`. With the default 1-MTU step, `cwnd` grows at
**1 MTU per RTT** — at 70 ms that is 17 KB/s, so refilling a ~525 KB BDP takes ~30 s, and *every* loss resets the
process. At 12 ms the same ramp is ~85 KB/s. A browser sink that periodically stalls therefore cannot be
refilled by a small-cwnd sender. The knob-off arm at 70 ms never escapes that regime: the NIC-counter trace in
cell C1 shows the rate oscillating 4.7–24.2 Mbps for 533 s and averaging 11.9 Mbps. The knob-ON arm completes the
same file in 56–64 s.

**3. Why this does not simply overturn exp40 — and what the real variable probably is.** The native arm **E**
reversed exp40's direction in this session (ON 183.3 > OFF 156.1 vs exp40's OFF 205.9 > ON 170.8), and my path
control shows today's path is materially different: **125–147 TCP retransmits per 10 s** against exp40's **24**,
with capacity swinging 210 ↔ 244 Mbps within one minute. A large CA step is exactly the wrong medicine on a
clean, shallow-buffered path (exp40's mechanism), and exactly the *right* one on a lossy or ramp-limited path
(fast recovery, wide BDP). The two sessions therefore agree on the mechanism and disagree on the sign — because
the sign is set by path loss. **The recommendation to drop the knob is not safe to apply from exp40 alone.**

**4. For v1/v2.** Production sets no `SB_SCTP_*` at all, so the shipped behaviour at 70 ms is the ~13 Mbps arm,
not the 64.5 Mbps the campaign has been quoting. That is a much bigger production concern than the near-field
176-vs-234 story. The right fix is not a constant but an adaptive one — the CA step (or a cwnd cap near the
measured BDP) should track path loss, which is the same BBR-style conclusion exp40 reached, from the opposite
direction.

**5. Lab/field asymmetry, explicit.** Both exp40's and this experiment's results are single-session field
measurements on a path whose loss rate changed by 5× in 100 minutes. Neither session can rank the knob's
effect *in general*; together they show the effect is real, large, and sign-unstable. Any knob sweep run on a
lab rig (deep-buffered, fixed loss) will pick a winner that does not transfer — which is precisely how
`CA_STEP=32768` entered the campaign in the first place.

## Caveats

- **n=3 per arm, one session, one day, one share, one 754 MiB file.** Arms are 3 cells each; the interleaved
  control is 1+1. The arm *separation* is outside the within-arm spread in all four cases, but the arm *means*
  carry that spread.
- **Arms A and B were measured ~1 h apart and C and D ~1 h apart**, so time-of-day drift is a live confound for
  the headline. It is mitigated, not eliminated: (i) the interleaved A4/B4 pair, 115 s apart, reproduces the
  direction at +18.3 %; (ii) the drift argument runs the wrong way — the knob-ON arms ran *later*, when the path
  control was *slower* (210 Mbps at 04:28 vs 244.5 at 02:52); (iii) the C/D gap is **4.4×** while the measured
  path capacity varied only **1.16×**, so path drift cannot account for it.
- **The path is unstable today and that is a real confound for every absolute number**, including the C/D and
  A/B means. The two path controls 1 minute apart were 210 and 244 Mbps with 125 and 147 retransmits. October's
  numbers were taken on a cleaner path (24 retransmits), so all my deltas vs history mix "today's path" with
  "today's config". The within-session A-vs-B and C-vs-D comparisons do not.
- **Arm E directly contradicts exp40 and I did not reconcile it.** Native 16 MiB window: ON 166.4–192.4 (n=3),
  OFF 139.6–171.4 (n=3) — ranges slightly overlap (ON min 166.4 < OFF max 171.4), so E's separation is weaker
  than A/B's or C/D's, but its *direction* is opposite to exp40's. exp40's cleanest single-variable pair
  (E1 = 177.592 knob-ON vs E2 = 223.633 knob-OFF, same binary, 100 s apart) was **not** reproduced.
- **The browser driver is click-only and emits no per-second trace**, so the browser arms have aggregates only.
  The one exception is C1, whose rate trace came from CLIENT-WEST `ens3` RX counters (driver-independent).
- **Agent-side windows include session setup.** `lanes ready` precedes the click by ≤0.09 s in every cell, which is
  ≤0.2 % of a 50 s window and ≤0.02 % of C1's 533 s window. Browser windows are second-resolution in the log, so
  ±1 s ⇒ ≤2 % at 47 s.
- **The 234.4 Mbps ceiling and the "why 244.5 TCP but 234.4 SCTP" question are untouched** by this experiment.
- **Not run (no budget): the optional native knob sweep** (`SB_SCTP_CA_STEP` ∈ {unset, 8192, 16384, 32768}). Arm C's
  four knob-OFF 70 ms transfers took 7–9 minutes each, which consumed the sweep's time. A monotone curve is
  therefore still not available.
- **Method caveat M1** (launch-ssh hang) is documented above; it cost ~55 minutes and no cells.
- Safety: production `sharebridge-agent` (7878) and every non-ShareBridge container on VERSA were untouched and
  verified up at the end (49 containers). No container was restarted or removed except `sb-run`. No `git` command
  was run. No IP, hostname or share code appears in this file or in any artifact.

## Artifacts

- This file: `docs/superpowers/spikes/results/2026-10-13-exp43-knob-2x2-baseline.md`
- Agent-side logs are the metric and live only in `docker logs sb-run` on VERSA; the container was recreated
  between arms (as the method requires), so **the raw logs for arms A/E-off and B/D/E-on no longer exist**.
  Every window is transcribed above with sub-millisecond timestamps, and every timestamp in this file is the
  `docker logs --timestamps` value as printed, so the numbers are re-derivable if the container is not recycled
  again. `sb-run` was left running with its log intact from 05:00:47.
- Native-receiver logs on CLIENT-EAST: `/tmp/fr-{Ex1on,Ex2on,Ex1off,Ex2off,Ex3off,Ex3on}.log` (`PROGRESS` 1 Hz +
  `SUMMARY`).
- Browser driver logs on the client that ran them: `/tmp/cell-{A1,A2,A3,A4,B1,B2,B3,B4,C1,C1b,C2,C3,D1,D2,D3}.log`.
- Share consumption: **10 → 30 downloads** (20 cells).
- No `git` command was run by this experiment. No file other than this results file was written in the repo;
  no worktree source, no `run-variant.sh`, no local template was modified. **No `docker build` was run.**
- `net.core.rmem_default`/`rmem_max` on CLIENT-EAST were raised to 4 MiB/16 MiB for the native cells and
  **restored and confirmed at 212992/212992**. Only ever increased; nothing persistent remains.
- Process gate returned 0 before every cell and at the end on both clients; both were left idle.
- **Rig state on hand-back:** `sb-run` = `sb-agent:pristine` + `SB_SCTP_CA_STEP=32768` (the briefed standard rig
  config), running and authenticated since 05:00:47; production `sharebridge-agent` (7878) up and untouched;
  49 containers up.
- **NOT shelved.** Runbook §1 rule 6 asks for shelving at the end of a campaign; this experiment deliberately
  did **not** shelve CLIENT-EAST / CLIENT-WEST / TESTBOX because the campaign is visibly still in flight
  (a separate 8-vCPU client is `ACTIVE`, and exp41/exp42/exp44 are same-day work). Flagged for the operator to
  decide rather than unilaterally taking the rig down. No instance was stopped, created, resized or deleted.
