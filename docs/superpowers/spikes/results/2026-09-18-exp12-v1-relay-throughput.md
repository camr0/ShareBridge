# Experiment 12 — v1 relay-mode download throughput — 2026-09-18

**Verdict:** v1 relay delivered **41.989 Mbps on CLIENT-EAST (n=2, 40.826–43.153)** and **55.306 Mbps on CLIENT-WEST (n=2, 54.449–56.163)**. It is far below v2 relay, and—unexpectedly—WEST was faster than EAST despite the longer relay path.

**Setup:** Existing v1 rig only: `sb-run` (`sb-agent:pristine`, test UI 7879) on VERSA, v1 Caddy/signalling/secure-relay on TESTBOX, and click-only `drive_click.js` on CLIENT-EAST / CLIENT-WEST. No rig switch and no v2 or production resource touched. One browser tab per cell. Payload is the same 754 MiB source as the direct-mode cells: exactly **790,626,304 bytes = 6,325.010432 megabits**.

Health check at 2026-09-18 16:52–16:54 UTC: `sb-run` present, TESTBOX HTTPS returned 200, both clients reachable, and neither client had a `node`, `chrome`, or `Xvfb` process. Initial `sb-run` sample: 0.00% CPU, 13.94 MiB memory. Five-ping averages to TESTBOX were 0.524 ms from CLIENT-EAST, 62.221 ms from CLIENT-WEST, and 11.997 ms from VERSA, implying relay path RTTs of roughly 12.5 ms EAST and 74.2 ms WEST; the table retains the comparable prior direct RTT assumptions of ~12 / ~71 ms.

## Relay-only share creation

Request actually sent (secrets and infrastructure names redacted per runbook; the JSON keys, non-secret values, method, endpoint, and headers are exact):

```http
POST http://127.0.0.1:7879/api/v1/shares
X-API-Key: <test key from VERSA env-run>
Content-Type: application/json

{"share_url":"<same 754 MiB OpenCloud source URL as direct share>","share_type":"opencloud","expiry_hours":48,"max_downloads":500,"relay_only":true}
```

POST response: `code=<redacted>`, `public_url=https://TESTBOX/s/<redacted>`, `expires_at=2026-09-20T16:54:00.268116714Z`. Immediate authenticated GET returned these share fields: `code=<redacted>`, `public_url=https://TESTBOX/s/<redacted>`, `share_url=<redacted same source URL>`, `file_id=<redacted source identifier>`, `downloads=0`, `max_downloads=500`, `relay_only=true`, `expires_at=2026-09-20T16:54:00.268116714Z`, `created_at=2026-09-18T16:54:00.268116714Z`.

## Metric

Primary window is **agent-side `relaychannel: handshake complete` → agent-side `download complete`**, using Docker's nanosecond timestamps. This is the closest relay analogue to the direct-mode agent window and avoids cross-host clock error. The numerator is the known source size, exactly 790,626,304 bytes. `download complete` is emitted only after all file bytes and `chunk_end` have been written by the agent, and its internal callback receives the actual streamed byte count (which is then reported to signalling), but the log message itself does not print that count. Weaknesses: relay handshake completion precedes the driver's click by about 0.17–0.34 s, so the window includes file-list/request latency and slightly understates transport throughput; agent completion is sender-side and can precede browser verification/service-worker persistence; and it cannot independently detect corruption at the browser. Direct-mode `DataChannel lanes ready` is explicitly inapplicable and is required to be absent.

## Raw cells

| cell | client | assumed RTT to VERSA | bytes | metric start → end | seconds | Mbps | sb-run CPU sample | client vmstat idle | relay proof / notes |
|---|---|---:|---:|---|---:|---:|---|---:|---|
| E-R1 | CLIENT-EAST | ~12 ms (relay path ping sum ~12.5 ms) | 790,626,304 | 16:55:47.629310740 → 16:58:22.554124254 UTC | 154.924814 | **40.826** | 5.37–17.82%, ~11.4% mean (8 in-flight samples) | 40–48%, median ~44% | Relay handshake + started-successfully; agent count 0→1; no `DataChannel lanes ready`; driver click at 16:55:47.815 UTC. |
| E-R2 | CLIENT-EAST | ~12 ms (relay path ping sum ~12.5 ms) | 790,626,304 | 17:00:01.318965510 → 17:02:27.892169643 UTC | 146.573204 | **43.153** | 7.95–18.91%, ~13.1% mean (10 in-flight samples) | 42–47%, median ~45% | Relay handshake + started-successfully; agent count 1→2; no `DataChannel lanes ready`; driver click at 17:00:01.489 UTC. |
| W-R1 | CLIENT-WEST | ~71 ms (relay path ping sum ~74.2 ms) | 790,626,304 | 17:03:44.701116166 → 17:05:37.319435783 UTC | 112.618320 | **56.163** | 14.23–24.23%, ~18.4% mean (3 in-flight samples) | 43–45%, median 44% | Relay handshake + started-successfully; agent count 2→3; no `DataChannel lanes ready`; driver click at 17:03:45.034 UTC. |
| W-R2 | CLIENT-WEST | ~71 ms (relay path ping sum ~74.2 ms) | 790,626,304 | 17:06:48.693182822 → 17:08:44.856897791 UTC | 116.163715 | **54.449** | 14.90–24.52%, ~19.2% mean (3 in-flight samples) | 44–48%, median 47% | Relay handshake + started-successfully; agent count 3→4; no `DataChannel lanes ready`; driver click at 17:06:49.029 UTC. |

## Interpretation

Per-client summary:

| client | n | mean Mbps | min | max | vs v1 direct | vs v2 relay |
|---|---:|---:|---:|---:|---|---|
| CLIENT-EAST | 2 | **41.989** | 40.826 | 43.153 | only 37.8–41.2% of 102–111 Mbps; **2.43–2.64× slower** | 18.0% of 233 Mbps; **5.55× slower** |
| CLIENT-WEST | 2 | **55.306** | 54.449 | 56.163 | 91.9% of 60.2 Mbps; **1.09× slower** | 24.3–25.7% of 215–228 Mbps; **3.89–4.12× slower** |

Mode is genuinely relay: across the four exact cell windows, the agent log contained four `relaychannel: handshake complete`, four `relay_prepare: relay channel started successfully`, four `download complete`, and **zero** `DataChannel lanes ready`. The authenticated share record remained `relay_only=true` and its download count rose 0→4. Three later relay read-loop endings were normal WebSocket closures after completed cells during deliberate driver cleanup, not transfer failures.

v1 relay is therefore not a high-throughput target for v1 direct on this rig: EAST relay is less than half of direct and WEST relay is slightly below direct. Both v1 modes remain dramatically below v2 relay. The EAST/WEST inversion suggests the legacy browser/Noise/WebSocket relay path is dominated by an endpoint or implementation cost rather than simple RTT; n=2 is too small to localize it. During transfer, Docker's instantaneous `sb-run` CPU samples were approximately 5–19% EAST and 14–25% WEST, while client aggregate idle was about 40–48% EAST and 43–48% WEST.

## Caveats

1. The primary interval begins at relay handshake completion, 0.17–0.34 s before the recorded click. It therefore includes request latency and biases Mbps slightly low (well under 0.3% here). It ends when the agent has written all bytes plus `chunk_end`, not after browser hashing or service-worker persistence.
2. The exact 790,626,304-byte numerator is the known source size and the internal completion callback's byte argument, but the production log prints only `download complete`, not the byte argument. The matching file and 0→4 share count were independently checked; a browser-side exact-byte completion signal was not available without using the forbidden Playwright Download API.
3. `docker stats` values are sparse instantaneous samples (Docker CPU percentage convention), not integrated CPU time. `vmstat` idle is aggregate across four vCPUs and cannot exclude one saturated critical thread.
4. Only n=2 per client, sequential, on one night. The repeated values are close within each client, but the unexpected cross-client inversion needs replication before causal interpretation.
5. Every driver logged a `join() re-entered while browser signaling socket is still active` page error during startup, then logged `joined` and `clicked`, and each agent transfer completed. It did not invalidate the cells, but is retained as a driver artifact.

## Artifacts

Client driver logs: `/tmp/exp12-*.log` on the respective CLIENT VM. Agent evidence is from timestamped `docker logs sb-run` ranges on VERSA; share codes and relay session identifiers are intentionally omitted from this committed result.
