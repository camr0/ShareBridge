# Experiment 43 — the 2×2 `SB_SCTP_CA_STEP` knob × RTT, with the real browser client — 2026-10-13

**Status:** IN PROGRESS — written incrementally after every cell.

**Verdict:** <pending>

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

## Raw cells — every run

### Arm A — CLIENT-EAST, ~12.0 ms, knob **OFF**, browser

| # | agent start (UTC) | knob | lanes ready (UTC) | download complete (UTC) | window s | **Mbps** | count | mode | notes |
|---|---|---|---|---|---|---|---|---|---|
| A1 | 03:06:12 | unset | 03:06:34.719864 | 03:07:40.996770 | 66.277 | **95.433** | 11 | direct (relay standby expired unused) | knob-off arm, first browser cell of the run |

### Arm B — CLIENT-EAST, ~12.0 ms, knob **ON** (32768), browser

*(pending)*

### Arm C — CLIENT-WEST, ~70.0 ms, knob **OFF**, browser

*(pending)*

### Arm D — CLIENT-WEST, ~70.0 ms, knob **ON** (32768), browser

*(pending)*

### Arm E — CLIENT-EAST, ~12.0 ms, both, native `fieldrecv-rwnd --max-rx-buf 16MiB`

*(pending)*

## Artifacts

- This file.
