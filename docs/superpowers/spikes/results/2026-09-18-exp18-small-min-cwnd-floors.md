# Experiment 18 — small SCTP min-cwnd floors on the fast field path — 2026-09-18

**Verdict:** CONFIRMS THE BDP HYPOTHESIS, BUT CLOSES THE LEVER. Floors ≤ 128 KiB (≈0.2–0.8× BDP) are **harmless** on CLIENT-EAST — every cell completed direct at 100.7–122.2 Mbps, inside the same-session stock spread — while 256 KiB (≈1.6× BDP) degrades 3–4× (**36.5 Mbps**) and 2 MiB (≈12× BDP, E17) breaks outright. But **no floor beat the interleaved stock controls** (117.7 / 124.4 / 98.9): the clean fast path has no loss-induced collapse for a floor to remove, so the E14 lab benefit does not transfer. Recommendation: stop — `SB_SCTP_MIN_CWND` has no field value on this path at any size.

**Hypothesis:** E17's catastrophic failure used a 2 MiB floor ≈ 12× the CLIENT-EAST BDP (~165 KB at 12 ms × 110 Mbps); the lab tolerated it only because its shim has a 5 MB queue. A floor near/below BDP should be harmless-or-helpful; if even 32 KiB (≈0.2× BDP) breaks transfer, the lever is unusable in the field and we stop.

**Setup:** VERSA test agent `sb-run` only (`sb-agent:pristine`, host networking, `UI_PORT=7879`) / TESTBOX v1 signalling / CLIENT-EAST. Payload 754 MiB = 790,626,304 B = 6,325.01 Mb. Driver `drive_click.js`, one click-only tab (`NTABS=1`), no Playwright Download API. Metric: agent-side first `DataChannel lanes ready` → `download complete`, agent clock UTC; Mbps = 6,325.01 ÷ window s. Each floor value gets its own container recreate (env inspected + boot echo captured — the tuning echo is once-per-container-boot by design, E17 code-verified). Stock controls interleaved in the same session via `run-variant.sh c`.

## Pre-state (19:31:44Z)

- VERSA `sb-run`: image `sb-agent:pristine`, `UI_PORT=7879`, `UI_ADDR=127.0.0.1`, `SB_SCTP_CA_STEP=32768`, `SB_SCTP_MIN_CWND` **absent**; authenticated 19:25:37Z, 2 sessions loaded; boot echo `[cwndCAStep=32768]` at 19:26:13Z (this boot, E17's WC1 peer).
- `env-run` byte-identical to E17's backup `/tmp/exp17-env-run.orig` (diff clean; 361 bytes, mode 600).
- TESTBOX: signalling HTTP 200; `caddy` + `sharebridge-test` active (E17 verified 19:28Z; re-verified 200 at 19:31Z).
- CLIENT-EAST reachable, `drive_click.js` present, no `node`/`chrome`/`Xvfb`. CLIENT-WEST reachable, process-clean. I am the only field agent (both VMs process-checked before every cell).
- Share: newest unexpired share with downloads remaining, same 754 MiB payload (code not recorded here per runbook §1.5).

## Rig-change procedure (per floor value, reusing E17's)

`cat /tmp/exp17-env-run.orig > env-run && echo "SB_SCTP_MIN_CWND=<V>" >> env-run` (mode/inode preserved), then hand-run run-variant.sh's recreate command: `docker rm -f sb-run; docker run -d --name sb-run --restart unless-stopped --network host -v sb-run-data:/root/.sharebridge --env-file env-run sb-agent:pristine` (run-variant.sh itself would regenerate env-run stock and wipe the line). Stock controls: `HOME=/home/ali bash run-variant.sh c`.

## Raw cells (appended after every cell)

| cell | client | floor | agent window (UTC) | window s | Mbps | echo | direct? | notes |
|---|---|---|---|---|---|---|---|---|
| C0 (stock) | EAST | none | 19:32:37.604 lanes → 19:33:31.376 complete | 53.77 | **117.7** | `[cwndCAStep=32768]` (boot echo 19:26:13, this boot) | yes — relay standby expired unused 19:33:21 ("pending wait window exceeded"), no peer close; click 19:32:37.737 | baseline 102–131; container unchanged from E17 restore |
| F32 (32768) | EAST | 32 KiB | 19:35:22.366 lanes → 19:36:14.141 complete | 51.78 | **122.2** | `[cwndCAStep=32768 minCwnd=32768]` (boot echo 19:35:21.580, this boot) | yes — relay standby expired unused 19:36:06; no peer close | rx-rate ≈ 16.4 MB/s (≈131 Mbps) mid-transfer; fresh container recreate, env-verified |
| F64 (65536) | EAST | 64 KiB | 19:37:21.874 lanes → 19:38:24.706 complete | 62.83 | **100.7** | `[cwndCAStep=32768 minCwnd=65536]` (boot echo 19:37:21.079, this boot) | yes — relay standby expired unused 19:38:06; no peer close | rx-rate ≈ 19.8 MB/s (≈159 Mbps) mid-transfer; below F32/C0 but inside/near today's stock spread (103.1–131.2) |
| C1 (stock) | EAST | none | 19:39:24.368 lanes → 19:40:15.216 complete | 50.85 | **124.4** | `[cwndCAStep=32768]` (boot echo 19:39:23.663, this boot) | yes — relay standby expired unused 19:40:08; no peer close | interleaved control mid-sweep, `run-variant.sh c` recreate |
| F128 (131072) | EAST | 128 KiB | 19:41:18.012 lanes → 19:42:12.739 complete | 54.73 | **115.6** | `[cwndCAStep=32768 minCwnd=131072]` (boot echo 19:41:17.221, this boot) | yes — relay standby expired unused 19:42:02; no peer close | ≈0.77× BDP; rx sample 2.6 KB/s at ~19:42:10 caught the transfer tail, completion 3 s later — healthy |
| F256 (262144) | EAST | 256 KiB | 19:43:14.630 lanes → 19:46:08.022 complete | 173.39 | **36.5** | `[cwndCAStep=32768 minCwnd=262144]` (boot echo 19:43:13.853, this boot) | yes — relay standby expired unused 19:43:58; no peer close | ≈1.6× BDP: steady degraded transfer, rx ≈ 4.3–4.5 MB/s (≈34–36 Mbps) at t+35/70/135 s, agent CPU 50%; completed inside fail-fast budget; 3–4× below same-session stock, far outside today's stock spread (103.1–131.2) — E17's failure mode at reduced intensity |
| C2 (stock, post-restore sanity) | EAST | none | 19:48:00.288 lanes → 19:49:04.255 complete | 63.97 | **98.9** | `[cwndCAStep=32768]` (boot echo 19:47:59.489, this boot) | yes — relay standby expired unused 19:48:44; no peer close | run on the fully restored stock rig; proves restoration transferred at baseline |

**Summary (EAST, n=1 per arm, agent-side, all direct-mode-asserted):** stock controls C0/C1/C2 = **117.7 / 124.4 / 98.9** (same session; today's earlier stock: 131.2, 103.1). Floors: **32 KiB 122.2**, **64 KiB 100.7**, **128 KiB 115.6** — all inside the stock spread, i.e. indistinguishable from stock. **256 KiB 36.5** — 2.7× below the *worst* same-day stock cell (98.9), 3.1× below the control mean; the only floor outside noise. Dose-response vs BDP multiple: 0.2×/0.4×/0.8× → stock-like; 1.6× → 3–4× loss; 12× (E17) → stall/2.7 Mbps trickle. No floor value showed a benefit.

## Interpretation

1. **The BDP hypothesis is confirmed.** E17's catastrophic failure was overshoot: the CLIENT-EAST BDP is ≈165 KB, and degradation begins between 0.8× and 1.6× BDP, exactly where a real (shallow-queue) path stops absorbing a floor-clamped cwnd. Floors at/below BDP are inert — E14's "no-op on a clean path" transfers to the field.
2. **But the lever buys nothing on this path.** E14's 7–19× wins existed only under synthetic loss; the field fast path at operating rate evidently lacks the loss-driven window collapse a floor would prevent (E11's ~1.7×10⁻⁴ inference). With stock at 98.9–131.2 across six same-day cells, no floor value separates from noise. A harmless knob with no benefit = **do not ship**; and the margin between "harmless" and "3–4× degradation" (0.8×→1.6× BDP) is one doubling — far too fragile for a configuration lever on heterogeneous real paths.
3. **Field variance discipline worked.** C0 (117.7) and C1 (124.4) bracket the floor cells; F64's 100.7 looked low only until C2 read 98.9 on stock. Without interleaved controls, F64 would have been misread as a degradation; with them, the only real effect in the sweep is F256.

## Caveats

- n=1 per floor value (timebox); the 32/64/128 KiB arms sit inside a ±13% same-day stock spread, so only their *harmlessness* is established, not exact parity. F256's 2.7× gap to the worst stock cell is far outside that spread and consistent with E17's mechanism, so its degradation is real.
- The 128→256 KiB breakpoint brackets, not pins, the degradation threshold (somewhere in 0.8–1.6× BDP ≈ 128–256 KiB on this path).
- CLIENT-WEST (71 ms, BDP ≈ 1 MB) not swept: EAST answered the question (harmless-but-useless below BDP, harmful above), and the timebox favored the restore.
- One share (56 downloads consumed of 500); no budget pressure.

## Restoration (19:47:29Z, verified)

- `env-run` regenerated stock by `HOME=/home/ali bash run-variant.sh c`; **byte-identical to E17's backup** `/tmp/exp17-env-run.orig` (diff clean); `SB_SCTP_MIN_CWND` absent (grep count 0). Backup left in place.
- `sb-run`: image `sb-agent:pristine`, `UI_PORT=7879`, `UI_ADDR=127.0.0.1`, `SB_SCTP_CA_STEP=32768`, no `SB_SCTP_MIN_CWND`; authenticated 19:47:29Z, 2 sessions loaded. C2 cell on this container completed direct at 98.9 Mbps with stock-only echo `[cwndCAStep=32768]`.
- TESTBOX: `caddy` active, `sharebridge-test` active, signalling external HTTP 200.
- CLIENT-EAST and CLIENT-WEST: `node`/`chrome`/`Xvfb` counts zero at close.
- Production, v2 stack, DNS/ACME, non-test resources untouched; no instance state changed; no git command run; no IPs/hostnames/codes written to this file.

## Artifacts

- Client driver logs: CLIENT-EAST `/tmp/cell-e18-c0.log`, `/tmp/cell-e18-f32.log`, `/tmp/cell-e18-f64.log`, `/tmp/cell-e18-c1.log`, `/tmp/cell-e18-f128.log`, `/tmp/cell-e18-f256.log`, `/tmp/cell-e18-c2.log`.
- Agent log windows: `docker logs --timestamps --since <t0> sb-run` per cell (boot echoes quoted per cell above; removed intermediate containers' logs are gone with the containers).
- NIC-rate samples: client `/proc/net/dev` deltas quoted per cell. Env backup: VERSA `/tmp/exp17-env-run.orig`.
