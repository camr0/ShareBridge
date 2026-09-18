# Experiment 17 — field SCTP congestion-window floor — 2026-09-18

**Verdict:** NEGATIVE, decisively — in the field, `SB_SCTP_MIN_CWND=2097152` does **not** restore throughput; it **breaks/degrades the direct path catastrophically** (open-lanes-then-zero-data stalls ×3, or a ~2.7 Mbps trickle ≈40× below stock), contradicting lab E14. Stock controls tonight re-confirmed the baselines (EAST 131.2/103.1, WEST 59.5 Mbps). Secondary finding: the v1 page silently falls back to the relay (~44.7 Mbps EAST) when the direct handshake fails within 10 s — a confound any field cell must exclude.

**Setup:** VERSA test agent `sb-run` only (`sb-agent:pristine`, host networking, `UI_PORT=7879`) / TESTBOX v1 signalling / CLIENT-EAST / CLIENT-WEST. Payload 754 MiB = 790,626,304 B = 6,325.01 Mb. Driver: `drive_click.js`, one click-only tab, no Playwright Download API. Primary metric: agent-side first `DataChannel lanes ready` → `download complete`. Experimental tuning is `SB_SCTP_CA_STEP=32768` plus `SB_SCTP_MIN_CWND=2097152`.

## Pre-state and rig restoration verification

Recorded before any change:

- TESTBOX's test-only `web/src/app.js` is restored to the original: 91,308 bytes, mode 0644, root:root, SHA-256 prefix `407ee703…`; on-host and fresh HTTP copies match, the `EXP14` marker is absent, and the prior temporary backup is absent. No restoration action was needed.
- TESTBOX `caddy` and `sharebridge-test` are active; signalling HTTP returned 200.
- VERSA `sb-run` is running image `sb-agent:pristine`, with `UI_PORT=7879`, `UI_ADDR=127.0.0.1`, and `SB_SCTP_CA_STEP=32768`; `SB_SCTP_MIN_CWND` is absent before the experiment. The agent log shows authentication and two restored sessions. The chosen existing share is unexpired, has downloads remaining, and refers to the same 754 MiB payload.
- CLIENT-EAST and CLIENT-WEST are reachable, contain `drive_click.js`, and had no `node`, `chrome`, or `Xvfb` processes.
- Production, v2, DNS/ACME, and non-test resources were not touched. No git command was run.

## Experiment change and knob verification

At 19:00:57Z, only `VERSA:/home/ali/sharebridge-test/env-run` was changed: its original mode-preserving copy was saved temporarily as `/tmp/exp17-env-run.orig`, then the single line `SB_SCTP_MIN_CWND=2097152` was appended. Only `sb-run` was recreated with the documented image/network/volume/env-file command. Container inspection confirms all four required values: `UI_PORT=7879`, `UI_ADDR=127.0.0.1`, `SB_SCTP_CA_STEP=32768`, and `SB_SCTP_MIN_CWND=2097152`; the recreated agent authenticated and loaded both sessions. The per-peer `applySCTPTuning` echo will be recorded verbatim from the first cell below (it is emitted when a WebRTC peer is created, not at container boot).

## Raw cells

All times UTC, agent clock. Mbps = 6,325.01 Mb ÷ window s (single file per cell). "rx-rate" = client NIC `/proc/net/dev` receive delta sampled over 3–4 s.

**Establishment failure pattern (floored container, 3 for 3):** every peer on the recreated `sb-run` opened lanes and was clicked, then **zero payload bytes flowed** (client NIC ≈ 320 B/s, heartbeat-level; agent CPU 34% then idle; relay standby closed at its 45 s window without carrying data; no `download complete`, no agent-side error). The dead agent's 19:02 peer showed the identical pattern for 5 minutes until its driver was killed.

| cell | client | agent window (UTC) | window s | Mbps | client idle% | `sb-run` CPU / memory | tuning echoed | direct? | notes |
|---|---|---|---:|---:|---:|---|---|---|---|
| dead-agent | EAST | 19:02:08 lanes → never | — | — | — | — | yes (boot echo 19:02:07) | relay standby only | lanes ready but zero data for 5 min; driver killed externally; not a measurement |
| E1 | EAST | 19:09:12.069 lanes → never | — | — | 96–97 | 34.19% / 63.23 MiB (mid-"transfer"), 2.32% after kill | n/a (once-per-boot, see below) | relay standby only | click at 19:09:12.4; rx-rate ≈ 290 B/s at t+~100 s; killed at ~19:13 |
| E1b | EAST | 19:14:06.618 lanes → never | — | — | — | 2.32% / 89.06 MiB after kill | n/a | relay standby only | rx-rate ≈ 320 B/s at t+~25 s; killed at ~19:16 |
| EC0 (stock control) | EAST | click 19:16:48.543 → 19:19:10.100 | 141.56 | 44.7 (RELAY, not direct) | — | — | `[cwndCAStep=32768]` (boot echo 19:16:38) | **no — relay fallback** | direct peer failed lanes in 10 s and closed 19:16:48.036; relaychannel handshake complete 19:16:48.067; data flowed over relay; window = click→complete |
| EC1 (stock control) | EAST | 19:19:44.987 lanes → 19:20:33.168 complete | 48.18 | **131.2** | 24 | 135.19% / 33.39 MiB | `[cwndCAStep=32768]` (once-per-boot, this boot) | yes — relay standby closed without carrying data; NIC rx ≈ 114 Mbps mid-transfer | fastest EAST cell on record (baseline 102–111; E15 104.963/120.877) |
| EC2 (stock control) | EAST | 19:21:36.127 lanes → 19:22:37.490 complete | 61.36 | **103.1** | 19 (32 us/47 sy) | 0.48% / 17.49 MiB after complete | n/a | yes — relay standby only | within baseline range |
| FC1 (floored, fresh recreation) | EAST | 19:24:02.099 lanes → not complete in ~65 s | — | **≈2.7 Mbps steady-state (NIC rx)** | — | 1.88% / 35.99 MiB | `[cwndCAStep=32768 minCwnd=2097152]` (boot echo 19:24:01, this boot) | yes — relay standby only | rx-rate 746 KB/s at t+15–19, 349 KB/s at t+~25–33, 332 KB/s at t+~40–50; ≈11 MB of 754 MiB in ~60 s; projected window ≈ 31 min; killed at ~19:25:55 |
| WC1 (stock control) | WEST | 19:26:14.501 lanes → 19:28:00.800 complete | 106.30 | **59.5** | 40 | 90.46% / 33.56 MiB mid-transfer | `[cwndCAStep=32768]` (boot echo of the 19:25:37 stock boot) | yes — relay standby only | matches WEST baseline 60.2–63.9 (E15: 60.0/67.8) |

**Floored-cell summary (EAST, n=4 incl. dead agent's):** 3 cells = open lanes + zero payload (rx ≈ 300 B/s, heartbeat-level); 1 cell (fresh recreation, boot echo verified) = open lanes + trickle at ≈2.7 Mbps, a ~40× degradation vs stock. **Stock (EAST, n=2): 131.2 and 103.1 Mbps direct; stock (WEST, n=1): 59.5 Mbps direct.** No floored cell ever reached `download complete`. Floored WEST cells were not run — EAST's catastrophic result plus the timebox made them redundant.

**How the A/B was closed:** the dead agent's floored container was not a botched recreation — `run-variant.sh` regenerates `env-run` from scratch (it ignores/wipes extra lines), so their floored `sb-run` was created by hand-running the script's `docker rm/run --env-file` command after appending the line. I replicated exactly that for FC1 (fresh boot, fresh boot-echo `[cwndCAStep=32768 minCwnd=2097152]`, same share, same driver) and it still could not transfer: the floor, not the recreation, is the cause. EC0 exposed a confound that changes field methodology: when the direct peer fails to open lanes within ~10 s, the page silently completes over the **relay** instead (relaychannel handshake completes milliseconds later) — a relay cell looks like a slow-but-working cell unless the agent log is checked for `peer … closed` before `relaychannel: handshake complete`.

**Tuning-echo mechanics (code-verified):** `agent/internal/peer/peer.go` `apiForPeers()` wraps both the tuning application and its log line in `defaultAPIOnce.Do` — the echo `peer: enabling custom SCTP tuning: [cwndCAStep=32768 minCwnd=2097152]` prints **once per container boot** (it did, 19:02:07, this boot), and every later peer silently reuses the cached tuned API via `newPeerConnection`. Per-cell echo verification is therefore impossible by design; container-env inspection plus the boot echo are the verification. Observed 45 prior downloads on this share earlier today on the pre-recreation (stock-tuning) container.

**Diagnostic fork:** stock-tuning control cell run next on a reverted container — completes at baseline ⇒ the 2 MiB floor breaks field data flow (contradicts lab E14); stalls too ⇒ the recreation itself broke the agent/rig (report, stop).

## Interpretation

1. **The lab-to-field transfer of E14's conclusion fails.** In the lab (E14, same 240 Mbps cap), `minCwnd=2 MiB` restored the full rate under loss and was a no-op on a clean path. In the field, on a clean ~12 ms path, the same knob (applied via the identical `SettingEngine` mechanism, boot-echo-verified) either freezes the association after lanes open (3/4 cells, zero payload, association idle at heartbeat rate) or collapses it to ≈2.7 Mbps (1/4). Stock tuning on the same share/driver/metric delivered 103–131 Mbps (EAST) and 59.5 Mbps (WEST) within minutes of the floored failures. The floor is not merely useless in the field — it is actively harmful to v1 direct.
2. **Candidate mechanism (not proven here):** the clamp raises cwnd to 2 MiB (≈1,400 datagrams) on every write including the RTO path; on the WAN (real NAT/buffered broadband upstream, unlike the lab's controlled 240 Mbps shim) that burst overload plausibly produces sustained heavy loss and a persistent retransmit/RTO regime — consistent with both the idle association (requests/opens starved inside a wedged send queue) and the ≈2.7 Mbps ratchet. The lab shim's drop model evidently does not reproduce whatever the real upstream does with a 2 MiB instantaneous burst.
3. **Consequence for the v1-vs-v2 argument:** E14's "configuration-only fix" is off the table for v1 direct. The E11 window-collapse problem remains unsolved by this knob; v2's relay retains its performance rationale (233 Mbps vs v1 direct 102–131 EAST / 59.5–64 WEST, and the relay fallback measured tonight at 44.7 Mbps is the v1 page's own safety net, not a fast path).
4. **Baseline health:** tonight's stock cells (EAST 131.2/103.1 — the 131.2 is the fastest EAST cell on record — and WEST 59.5) re-confirm the tuned baselines on the current rig, share, and payload, so the comparison is anchored same-day.

## Caveats and efficiency observations

- n is small and asymmetric by necessity: floored EAST n=4 (3 stall + 1 trickle; the trickle's steady-state ≈2.7 Mbps is a NIC-counter measurement, not a completion window — no floored cell completed, so no window/Mbps cell value exists), stock EAST n=2, stock WEST n=1, floored WEST n=0. The direction and magnitude (≥40×) are far outside any plausible noise band, but the exact floored steady-state rate is unmeasured to completion.
- `vmstat` idle is aggregate across vCPUs and cannot exclude one saturated critical thread.
- The field rig does not expose the lab shim's wire/payload and tail-drop counters, so the burst-loss mechanism in Interpretation §2 is a hypothesis, not an observation. Efficiency assessment is limited to client NIC rates, `vmstat`, agent `docker stats`, and completion behavior.
- The ≈2.7 Mbps trickle was sampled over three 4–10 s windows (746→349→332 KB/s); calling it "steady ≈2.7 Mbps" is an approximation from t+15..t+50 s.
- EC0 (44.7 Mbps) is a **relay** cell retained in the table for the record; it must not be counted as a direct baseline. Any future field cell where `peer … closed` precedes `relaychannel: handshake complete` is relay, not direct.
- The 19:02 dead-agent peer's five-minute stall is included as floored evidence on the strength of the boot-echo at 19:02:07; its driver was externally killed, so its cell was not instrumented like mine.

## Restoration

Restored at 19:25:37Z and re-verified at ~19:28Z:

- `VERSA:/home/ali/sharebridge-test/env-run` regenerated stock by `HOME=/home/ali bash run-variant.sh c`; byte-identical to `/tmp/exp17-env-run.orig` (diff clean; `SB_SCTP_MIN_CWND` absent). The backup `/tmp/exp17-env-run.orig` is still present and untouched.
- `sb-run` recreated via the documented variant-`c` path: image `sb-agent:pristine`, host networking, `UI_PORT=7879`, `UI_ADDR=127.0.0.1`, `SB_SCTP_CA_STEP=32768`, **no** `SB_SCTP_MIN_CWND`; agent authenticated, 2 sessions loaded, web UI on 127.0.0.1:7879 (log tail 19:25:37Z).
- TESTBOX `caddy` and `sharebridge-test` active; signalling HTTP 200.
- CLIENT-EAST and CLIENT-WEST: `node`/`chrome`/`Xvfb` counts all zero.
- Production, v2 stack, DNS/ACME, and non-test resources untouched; no instance state changed; no git command run.

## Artifacts

- Client driver logs: CLIENT-EAST `/tmp/cell-e1.log`, `/tmp/cell-e1b.log`, `/tmp/cell-ec0.log`, `/tmp/cell-ec1.log`, `/tmp/cell-ec2.log`, `/tmp/cell-fc1.log`; CLIENT-WEST `/tmp/cell-wc1.log`.
- Agent log windows: `docker logs --timestamps --since <t0> sb-run` over 19:09–19:28Z (container recreated twice: floored 19:01 and 19:23 boots, stock 19:16 and 19:25 boots — logs of removed boots are gone with their containers; the cells above quote the lines captured live from each boot).
- NIC-rate samples: client `/proc/net/dev` deltas quoted per cell. Env backup: `VERSA:/tmp/exp17-env-run.orig`.
