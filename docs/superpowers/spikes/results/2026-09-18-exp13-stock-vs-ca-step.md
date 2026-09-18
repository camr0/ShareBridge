# Experiment 13 — stock vs CA-step (field, real path) — 2026-09-18

**Question:** how much is `SB_SCTP_CA_STEP=32768` worth on the real path in the current rig
configuration? Tuned (`c`) baselines exist (E10b: CLIENT-EAST 102.0–111.0 Mbps, CLIENT-WEST 60.2);
this run measures stock (`u` = same image, **no** `SB_SCTP_*` env) with the identical method, payload,
and driver.

**Setup:** hosts VERSA (agent host, container `sb-run`, image `sb-agent:pristine`) / TESTBOX (v1
signalling) / CLIENT-EAST / CLIENT-WEST. Variant switch `VERSA:/home/ali/sharebridge-test/run-variant.sh`
( `u` = no tuning, `c` = `SB_SCTP_CA_STEP=32768`; `s`/`f` reference a nonexistent fork image — not
attempted). Payload 754 MiB = 790,626,304 B = 6,325.01 Mb. Driver: `drive_click.js`, NTABS=1
(click-only, no Playwright Download API). Metric: agent-side first `DataChannel lanes ready` →
`download complete` (UTC agent clock).

## Pre-state (recorded 2026-09-18T17:24Z)

- `docker ps`: `sb-run` = `sb-agent:pristine`, Up 10 h (started 07:54:02Z). Production
  `sharebridge-agent` also up — not touched.
- `sb-run` env: `UI_PORT=7879`, `UI_ADDR=127.0.0.1`, `SB_SCTP_CA_STEP=32768` → rig was on variant
  `c` (documented state) before this run.
- TESTBOX: `caddy` active, `sharebridge-test` active; v1 signalling HTTP 200.
- CLIENT-EAST and CLIENT-WEST reachable; **no** `node`/`chrome`/`Xvfb` running on either.

## Cells (appended after each cell)

| cell | variant | window (UTC) | window s | Mbps | idle% | direct? |
|---|---|---|---|---|---|---|
| E1 CLIENT-EAST rep1 | u (stock) | 17:25:47.019 → 17:27:01.912 | 74.89 | **84.45** | 35 (mid-transfer) | yes — relay standby channel closed by relay at +44 s ("pending wait window exceeded"), no relay data |
| E2 CLIENT-EAST rep2 | u (stock) | 17:27:54.229 → 17:29:07.194 | 72.96 | **86.68** | 24 (mid-transfer) | yes — same relay-standby-drop pattern at +44 s, no relay data |
| W1 CLIENT-WEST rep1 | u (stock) | 17:29:47.674 → 17:31:53.225 | 125.55 | **50.38** | 39 (mid-transfer) | yes — same relay-standby-drop pattern at +44 s, no relay data |
| W2 CLIENT-WEST rep2 | u (stock) | 17:33:09.132 → 17:35:18.462 | 129.33 | **48.90** | 37 (mid-transfer) | yes — same relay-standby-drop pattern at +44 s, no relay data |

## Verdict, interpretation, caveats

**Verdict:** in the current rig configuration, `SB_SCTP_CA_STEP=32768` is worth **~1.2–1.3× on
CLIENT-EAST** and **~1.21–1.24× on CLIENT-WEST** on the real path — a real, reproducible ~20–25 %
throughput gain, but far below the 1.89× recorded in the 2026-09-15 campus field test.

**Raw summary (u = stock, this run; c = tuned, E10b/E9 reference):**

| client | variant | Mbps cells | mean | gain (c ÷ u) |
|---|---|---|---|---|
| CLIENT-EAST | u (n=2) | 84.45, 86.68 | **85.6** | vs c 102.02–110.97 → **1.19–1.30×** |
| CLIENT-WEST | u (n=2) | 50.38, 48.90 | **49.6** | vs c 60.24–61.4 → **1.21–1.24×** |

**Interpretation:** the tune is confirmed worth it in this configuration — every stock cell is slower
than every tuned cell for that client, with no overlap (EAST 84.5–86.7 vs 102–111; WEST 48.9–50.4 vs
60.2–61.4), and stock rep-to-rep spread is small (±1.3 % EAST, ±1.5 % WEST), so the ~15 Mbps (EAST) /
~11 Mbps (WEST) deltas are well outside run-to-run noise. Both paths sit at ~25–35 % / ~20–25 % of the
~246 Mbps UDP path capacity when stock, vs ~45 % / ~25 % tuned. The campus 1.89× does not generalize
to this path/rig; the honest current-configuration number is **~1.2×, ~+20 %**. None of this says the
production gap is closed: the production container still ships without the env var, so production
users get the stock numbers until it is set.

**Caveats:** n=2 per client per variant (matches E10b's n); single share, single 754 MiB file; vmstat
idle% is an aggregate across vCPUs and cannot exclude one saturated critical thread (EAST idle 24–35 %,
WEST 37–39 % during transfers). The tuned baselines come from a different session (E10b/E9, same
payload/driver/metric/rig state), not interleaved same-session A/B — acceptable given the tight rep
spread and cross-run reproducibility noted in E10b. All cells direct (relay standby channel dropped by
the relay at +44 s, "pending wait window exceeded", no relay data path observed).

**Rig restoration (done 17:35:49Z):** `run-variant.sh c` re-run — `sb-run` on `sb-agent:pristine`,
`UI_PORT=7879`, `UI_ADDR=127.0.0.1`, `SB_SCTP_CA_STEP=32768`, script reported `auth=1 sessions=1`;
TESTBOX `caddy`/`sharebridge-test` active, v1 signalling HTTP 200; both client VMs free of
node/chrome/Xvfb. Production `sharebridge-agent` never touched. No `git` commands run.

**Artifacts:** driver logs `/tmp/cell-{e1,e2,w1,w2}.log` on CLIENT-EAST (e*) / CLIENT-WEST (w*);
agent log source `docker logs sb-run` (windows quoted in the table); share downloads counter
33→37 across the four cells (limit 500).

- Cell E1 detail: share `a1k6q46n` (downloads counter 33→34), NTABS=1 click-only, driver log `/tmp/cell-e1.log`
  (CLIENT-EAST). vmstat mid-transfer: us 28 / sy 37 / **id 35** / wa 0. Launch ssh call lingered past its echo
  (channel-close lag) but driver started normally.
- Cell E2 detail: same share (34→35), NTABS=1 click-only, driver log `/tmp/cell-e2.log`. vmstat mid-transfer:
  us 34 / sy 42 / **id 24** / wa 0.
- Cell W1 detail: same share (35→36), NTABS=1 click-only, driver log `/tmp/cell-w1.log` (CLIENT-WEST).
  vmstat mid-transfer: us 27 / sy 34 / **id 39** / wa 0. A `peer ... closed` line at 17:29:52 belonged to
  cell E2's teardown, not W1.
