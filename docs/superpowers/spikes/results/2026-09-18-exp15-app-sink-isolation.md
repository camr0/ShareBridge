# Experiment 14 — app sink isolation — 2026-09-18

**Verdict:** A legitimate test-only A/B was possible, and the application sink is a material part of the v1 ceiling: removing SHA-1 + StreamSaver writes shortened the mean **agent-side** window by **23.8% on CLIENT-EAST** and **15.6% on CLIENT-WEST**. Bucket B is real, but the large residual to the ~246 Mbps UDP path—especially at high RTT—shows bucket A remains substantial.

**Setup:** VERSA test agent `sb-run` (`sb-agent:pristine`, `UI_PORT=7879`, `SB_SCTP_CA_STEP=32768`) / TESTBOX v1 signalling and **test-only** web root / CLIENT-EAST first, then CLIENT-WEST. Payload 754 MiB = 790,626,304 B = 6,325.01 Mb. Driver: temporary `drive_exp14.js`, one click-only tab, DOM observation only, no Playwright Download API. Primary metric: agent-side `DataChannel lanes ready` → `download complete`. Arms were interleaved control/bypass on the same direct-mode share, n=2 per arm per client.

## Feasibility investigation (recorded before field changes)

- The real share page is served by the v1 signalling server: `/s/{code}` serves `./web/index.html`; `/src/{path...}` serves `./web/src`; the page loads `/src/app.js`. On TESTBOX this is the test namespace under `/opt/sharebridge-test/web/`, not production.
- For an ordinary file share, a click sends `file_request`; `file_header` initializes `createDownloadPipeline`; every bulk DataChannel frame is awaited through `pipeline.append()`.
- The normal desktop-Chrome sink is `createStreamingSink`: each append runs incremental SHA-1 (`hash-wasm`) and then awaits StreamSaver's writer after holding a 1 MiB validation tail. StreamSaver routes bytes through its service-worker download stream. Finalization checks exact size and SHA-1, flushes the tail, and closes the writer.
- Existing preview/stream bypass is gallery/Immich-specific (`asset_preview_request`) and is not a legitimate equivalent for this ordinary 754 MiB file share. The lab receiver is also ineligible because it uses its own shim/signalling.
- A legitimate A/B is possible by temporarily patching **only TESTBOX's test copy** of `/opt/sharebridge-test/web/src/app.js`: when an explicit test query parameter is present, `buildDownloadSink()` returns a discard sink whose `append` is a no-op and whose `finalize` retains only exact byte-count validation. The normal URL remains the unmodified verify + StreamSaver path. This preserves the real signalling session, WebRTC/DataChannel receive/decode path, request, payload, and agent behavior while removing only SHA-1 and disk writing. The original will be checksum-recorded, backed up, restored byte-for-byte, and checksum-verified.
- Pre-change field verification at 18:29Z: TESTBOX `sharebridge-test` and Caddy active, signalling HTTP 200; VERSA `sb-run` healthy with the required tuning; both clients reachable and had no `node`/`chrome`/`Xvfb`. The deployed test `app.js` is byte-identical to the v1-tree source (SHA-256 prefix `407ee703…`, size 91,308 bytes). No production resource was read or changed.
- At approximately 18:31Z the original test asset was copied to TESTBOX `/tmp/exp14-app.js.orig` and verified byte-identical before replacement. The temporary asset (SHA-256 prefix `0c455782…`) is live and HTTP retrieval verifies the same checksum. A new click-only DOM-observing driver was placed first on CLIENT-EAST and, after the EAST block, on CLIENT-WEST (it never imports/calls Playwright's Download API); it records click→terminal-UI time and will be deleted during restoration. It was absent on both clients before creation. The chosen unexpired share is direct-mode with 463 downloads remaining and references the same underlying 754 MiB payload as the prior baselines; the newer share was rejected because it is relay-only.

## Raw cells

| cell | arm | client | agent window (UTC) | window s | agent Mbps | driver completion s | client idle% | `sb-run` CPU | direct? | notes |
|---|---|---|---|---:|---:|---:|---:|---:|---|---|
| A1 | control: verify + write | CLIENT-EAST | 18:33:07.536 → 18:34:07.796 | 60.259 | **104.963** | **138.306** | 49 | 63.50% | yes | Client UI reached `verified` 78.66 s after agent completion; relay standby closed at +44 s without carrying data. |
| B1 | sink removed | CLIENT-EAST | 18:36:48.629 → 18:37:24.958 | 36.328 | **174.107** | **36.413** | n/a | n/a | yes | UI `done`; tail after agent completion ~0.21 s. Resource samples accidentally landed post-transfer because launch SSH lingered, so omitted rather than mislabeled. |
| A2 | control: verify + write | CLIENT-EAST | 18:38:24.852 → 18:39:17.178 | 52.326 | **120.877** | **134.189** | 20 | 88.27% | yes | Client UI reached `verified` 82.02 s after agent completion; relay standby closed at +44 s without carrying data. |
| B2 | sink removed | CLIENT-EAST | 18:41:49.616 → 18:42:39.065 | 49.449 | **127.910** | **49.401** | 64 | 91.41% | yes | UI `done`; terminal UI followed agent completion by ~0.22 s. Relay standby only. |
| WA1 | control: verify + write | CLIENT-WEST | 18:44:18.159 → 18:46:03.649 | 105.490 | **59.959** | **107.692** | 43 | 66.36% | yes | UI `verified`; only 2.58 s tail after agent completion. Relay standby only. |
| WB1 | sink removed | CLIENT-WEST | 18:47:14.293 → 18:48:31.586 | 77.293 | **81.831** | **77.300** | 80 | 90.94% | yes | UI `done`; ~0.37 s tail after agent completion. Relay standby only. |
| WA2 | control: verify + write | CLIENT-WEST | 18:49:37.575 → 18:51:10.823 | 93.248 | **67.830** | **109.854** | 24 | 64.58% | yes | UI `verified`; 16.98 s tail after agent completion. Relay standby only. |
| WB2-attempt1 | sink removed | CLIENT-WEST | — | — | — | — | — | — | **no** | **Aborted, not a measurement:** direct peer closed before lanes-ready and browser selected relay; driver killed immediately. |
| WB2-retry | sink removed | CLIENT-WEST | 18:53:57.385 → 18:55:27.863 | 90.478 | **69.906** | **90.422** | 62 | 99.39% | yes | Valid retry; UI `done`, ~0.38 s tail. Relay standby only. |

### Cell A1 detail (appended immediately after cell)

- Pre-cell: both clients had no `node`/`chrome`/`Xvfb`; agent log was silent for the preceding 3 minutes; signalling HTTP 200; `sb-run` present.
- Driver joined with badge `Connected (Direct)`, clicked at 18:33:08.183Z, and reached terminal UI at 18:35:26.458Z (`✓ intact`, SHA-1 shown). The known benign `join() re-entered` page error occurred during connection setup.
- Agent log: lanes ready 18:33:07.536541915Z; download complete (count 38) 18:34:07.795708121Z. Window 60.259166 s; `6325.01 / 60.259166` = **104.963 Mbps**.
- Mid-transfer: CLIENT-EAST `vmstat` us 19 / sy 32 / **id 49** / wa 0; VERSA `docker stats` for `sb-run`: **63.50% CPU**, 50.9 MiB. The driver exited cleanly and left no node/chrome/Xvfb.

### Cell B1 detail (appended immediately after cell)

- Pre-cell: both clients had no node/chrome/Xvfb and the agent log was quiet after A1 teardown.
- Driver joined direct, clicked at 18:36:48.789Z, and reached terminal `✓ done` at 18:37:25.171Z. Agent lanes ready 18:36:48.629196588Z; download complete (count 39) 18:37:24.957544973Z. Window 36.328348 s; **174.107 Mbps**.
- This is **1.659× A1** agent-side and shortens the decisive agent window by **23.931 s (39.7%)**. Unlike A1's 78.66 s client tail, terminal UI followed agent completion by only ~0.21 s.
- The remote driver-launch SSH lingered for 30 s, so the attempted `vmstat`/`docker stats` samples occurred after completion (idle 100%, agent 0.25%); they are explicitly excluded. The driver then exited cleanly with no node/chrome/Xvfb left.

### Cell A2 detail (appended immediately after cell)

- Pre-cell process listings were empty on both clients; only B1's attributable teardown lines appeared in the recent agent log.
- Driver joined direct, clicked 18:38:25.034Z, terminal `✓ intact` 18:40:39.202Z. Agent lanes ready 18:38:24.851908838Z; complete (count 40) 18:39:17.177959269Z. Window 52.326050 s; **120.877 Mbps**. Client tail after agent completion was 82.02 s.
- Mid-transfer CLIENT-EAST: us 36 / sy 44 / **id 20** / wa 0. `sb-run`: **88.27% CPU**, 44.34 MiB. Driver exited cleanly.

### Cell B2 detail (appended immediately after cell)

- Pre-cell process listings empty on both clients; recent agent log quiet.
- Driver joined direct, clicked 18:41:49.912Z, terminal `✓ done` 18:42:39.282Z. Agent lanes ready 18:41:49.616144917Z; complete (count 41) 18:42:39.065164737Z. Window 49.449020 s; **127.910 Mbps**. Terminal UI followed agent completion by ~0.22 s.
- Mid-transfer CLIENT-EAST: us 21 / sy 15 / **id 64** / wa 0. `sb-run`: **91.41% CPU**, 50.93 MiB. Driver exited cleanly.
- With n=2 per arm, agent-side control mean **112.920 Mbps** (104.963–120.877; mean window 56.293 s) versus sink-removed mean **151.008 Mbps** (127.910–174.107; mean window 42.889 s). Removing the sink shortened the mean agent window by **13.404 s / 23.8%** and raised mean agent-side Mbps by **33.7% (1.337×)**. Both sink-removed cells were faster than both controls, though the closest separation is only 5.8% and run-to-run spread is material.

### Cell WA1 detail (appended immediately after cell)

- Pre-cell process listings empty on both clients; agent log quiet. Driver joined direct, clicked 18:44:18.559Z, terminal `✓ intact` 18:46:06.229Z.
- Agent lanes ready 18:44:18.159286808Z; complete (count 42) 18:46:03.648830245Z. Window 105.489543 s; **59.959 Mbps**, matching the prior tuned WEST baseline. Unlike EAST controls, terminal UI followed agent completion by only 2.58 s.
- Mid-transfer CLIENT-WEST: us 23 / sy 34 / **id 43** / wa 0. `sb-run`: **66.36% CPU**, 47.22 MiB. Driver exited cleanly.

### Cell WB1 detail (appended immediately after cell)

- Pre-cell process listings empty on both clients; agent log quiet. Driver joined direct, clicked 18:47:14.686Z, terminal `✓ done` 18:48:31.959Z.
- Agent lanes ready 18:47:14.292915483Z; complete (count 43) 18:48:31.586130031Z. Window 77.293215 s; **81.831 Mbps** — **1.365× WA1**, shortening the agent window by 28.196 s (26.7%).
- Mid-transfer CLIENT-WEST: us 10 / sy 9 / **id 80** / wa 0. `sb-run`: **90.94% CPU**, 47.37 MiB. Driver exited cleanly.

### Cell WA2 detail (appended immediately after cell)

- Pre-cell listings empty; only WB1's attributable peer-close remained in recent logs. Driver joined direct, clicked 18:49:37.964Z, terminal `✓ intact` 18:51:27.800Z.
- Agent lanes ready 18:49:37.574940548Z; complete (count 44) 18:51:10.822828580Z. Window 93.247888 s; **67.830 Mbps**. Client UI tail 16.98 s.
- Mid-transfer CLIENT-WEST: us 32 / sy 44 / **id 24** / wa 0. `sb-run`: **64.58% CPU**, 46.38 MiB. Driver exited cleanly.

### WB2 attempt 1 abort (appended immediately)

At 18:52:43Z the WebRTC peer closed before any `DataChannel lanes ready`; the relay handshake completed and the driver badge reported `Connected (Relay)`. This violates the direct-only cell requirement, so the cell was aborted immediately and excluded. The CLIENT-WEST processes were attributable to this driver and were cleaned with exact-name `pkill`; no other client was touched.

### Cell WB2 retry detail (appended immediately after cell)

- After a quiet interval, both client process listings were empty. Retry joined direct, clicked 18:53:57.837Z, terminal `✓ done` 18:55:28.240Z.
- Agent lanes ready 18:53:57.384574999Z; complete (count 45) 18:55:27.862743939Z. Window 90.478169 s; **69.906 Mbps**. Client terminal followed agent completion by ~0.38 s.
- Mid-transfer CLIENT-WEST: us 20 / sy 17 / **id 62** / wa 0. `sb-run`: **99.39% CPU**, 45.93 MiB. Driver exited cleanly.
- WEST n=2 control mean **63.894 Mbps** (59.959–67.830; mean window 99.369 s) versus sink-removed mean **75.869 Mbps** (69.906–81.831; mean window 83.886 s): mean agent window **15.483 s / 15.6% shorter**, mean Mbps **18.7% higher (1.187×)**. Both bypass cells exceeded both controls, though the closest separation was only 3.1%.

## Interpretation

### Summary

| client | control Mbps (n=2) | sink-removed Mbps (n=2) | mean gain | control window mean | bypass window mean | decisive window change |
|---|---:|---:|---:|---:|---:|---:|
| CLIENT-EAST | 104.963, 120.877 (mean **112.920**) | 174.107, 127.910 (mean **151.008**) | **+33.7%, 1.337×** | 56.293 s | 42.889 s | **−13.404 s, −23.8%** |
| CLIENT-WEST | 59.959, 67.830 (mean **63.894**) | 81.831, 69.906 (mean **75.869**) | **+18.7%, 1.187×** | 99.369 s | 83.886 s | **−15.483 s, −15.6%** |

The number that decides the question is the **agent-side window**, not the client UI tail. It became materially shorter on both clients when only SHA-1 and StreamSaver/service-worker writing were removed. Therefore those per-host app operations do feed back through DataChannel flow control and constrain the agent: **bucket B is causal**, not merely post-transfer client overhead.

The effect is larger on CLIENT-EAST: removing the sink adds **38.09 Mbps** to the mean and eliminates the enormous control tail (78.7–82.0 s after agent completion; bypass ~0.2 s). On CLIENT-WEST it adds **11.97 Mbps**; control client tails were already much smaller (2.6–17.0 s), consistent with the transport's slower high-RTT feed leaving the sink more time to keep up. Valid bypass cells also showed much higher client idle (EAST 64% in the usable bypass sample versus 20–49% controls; WEST 62–80% versus 24–43%).

This does **not** make bucket A disappear. Even with the sink removed, mean agent-side delivery was only **151.0 Mbps EAST** and **75.9 Mbps WEST** versus the path's prior ~246 Mbps UDP result. In observed-Mbps terms, the bypass recovered about **38 Mbps EAST** and **12 Mbps WEST**; roughly **95 Mbps EAST** and **170 Mbps WEST** still separate the bypass means from that path reference. Those residuals, the RTT sensitivity, and established pion loss-window collapse point to a substantial transport-side limit. The honest verdict is **A + B**, with B materially responsible for part of the ceiling and A still dominant in the remaining path gap, particularly WEST.

All eight valid cells were direct. The relay connection was only standby and closed without data. One additional WEST bypass attempt selected relay after direct failed; it was aborted immediately, reported, and excluded.

## Caveats

- n=2 per arm/client; cells were interleaved but not randomized or blinded. Run-to-run spread was material, especially EAST bypass. Both bypass observations nevertheless exceeded both controls on each client, with closest separations of 5.8% EAST and 3.1% WEST.
- The discard arm still performs browser DTLS/SCTP processing, envelope decoding, JS dispatch, byte accounting, and UI progress updates. It isolates SHA-1 plus StreamSaver/service-worker disk writing, not all browser-side work.
- The ~246 Mbps UDP result is a prior path reference, not a simultaneous protocol-identical control, so residual-Mbps figures are descriptive rather than a formal decomposition.
- `vmstat` idle is aggregate across vCPUs and cannot exclude one saturated critical thread. B1 resource samples missed the transfer and were honestly omitted.
- Driver completion includes navigation/click alignment and app finalization; it is secondary to the timestamped agent metric.

## Restoration

**Verified complete at the end of the run.** TESTBOX `app.js` was restored byte-for-byte from the pre-change copy: SHA-256 prefix `407ee703…`, 91,308 bytes, root:root mode 0644; both on-host `cmp` and a fresh HTTP retrieval matched. The `EXP14` marker/query branch was absent after restore, and the remote backup/patched temporary files were removed. The temporary driver was deleted from both clients and verified absent; both clients had no node/chrome/Xvfb. TESTBOX Caddy and `sharebridge-test` remained active with HTTP 200. VERSA `sb-run` remained `sb-agent:pristine` on 7879 with `SB_SCTP_CA_STEP=32768`. Production, the v2 stack, DNS/ACME, and non-test resources were never touched. No git command was run.

## Artifacts

- CLIENT-EAST: `/tmp/exp14-{A1,B1,A2,B2}.log`
- CLIENT-WEST: `/tmp/exp14-{WA1,WB1,WA2,WB2,WB2r}.log` (`WB2` is the excluded relay attempt; `WB2r` the valid direct retry)
- VERSA: timestamped `docker logs sb-run` windows quoted above; share counter 37→45 across eight valid completes.
- Source paths inspected: v1 `signaling-server/web/src/{app.js,downloadPipeline.js,downloadSinks.js}` and `signaling-server/cmd/server/main.go`.
