# Experiment 10 — matched v2 vs v1 field throughput — 2026-09-18

**Verdict:** On CLIENT-EAST, v1 delivered **112.034 Mbps** and the v2 **relay** delivered **233.285 Mbps** for exactly 790,626,304 client bytes: v2 was **2.082× v1**, or **94.60%** of the EAST 246.6 Mbps UDP path reference versus v1's **45.43%**. This is a byte-volume/direction match, not a byte-identical-content or browser-sink match (details below).

**Setup:** Both arms delivered 754 MiB (790,626,304 bytes = 6,325.01 Mb) agent → CLIENT-EAST in the same field session. v1 used one `drive_click.js` tab and the required agent-side window from first `DataChannel lanes ready` to `download complete`. v2 used the already-deployed phase-4a TEST systemd stack plus `docker compose start sharebridge-agent-test` from its existing TEST compose project, then the existing harness's relay-content request shape (`prepare-route` → relay origin → full asset curl). The v2 client stopped a larger TEST Immich original after exactly 790,626,304 received bytes; its wall interval used nanosecond timestamps around curl-to-sink. Hosts are named only as VERSA, TESTBOX, CLIENT-EAST, and CLIENT-WEST.

## Initial state recorded before switch

Recorded before changing any service or container:

- TESTBOX: `caddy=active`, `sharebridge-test=active`; v2 units `sharebridge=inactive`, `sharebridge-relay-gateway=inactive`, `sharebridge-relay-frps=inactive`.
- TESTBOX listeners: Caddy owned TCP/UDP `:443`; v1 `server` owned TCP `:8080`; no v2 `:3478`/`:7000` listener. Docker is not installed on TESTBOX, so there is no compose/container project to record there; this deployment uses the documented systemd units.
- VERSA: `sb-run` (`sb-agent:pristine`) was Up 2 hours; `sharebridge-agent-test` (`sb-agent:phase4a-test`) was Exited (2) 2 hours ago. The production `sharebridge-agent` was observed Up and was not touched.
- CLIENT-EAST and CLIENT-WEST: no `node`, `chrome`, `Xvfb`, or `iperf3` processes were present.

## Raw

| arm | client | payload | mode | metric window | seconds | Mbps | agent bytes | CPU | notes |
|---|---|---:|---|---|---:|---:|---:|---|---|
| v1 | CLIENT-EAST | 754 MiB | WebRTC DataChannel | agent `lanes ready` → `download complete` | 56.456306 | 112.034 | 790,626,304 implied by the fixed file | peak sample 101.51% Docker CPU (≈1.02 cores) | client `vmstat` sample: 23% idle aggregate; no close/error/retransmission line in the bounded agent log |
| v2 | CLIENT-EAST | 754 MiB received | **relay** (TLS/HTTP over gateway + FRP TCP tunnel) | client curl start → exact-byte sink completion | 27.112764 | 233.285 | gateway relayed-byte counter delta **793,602,114** | active Docker sample 17.86% (≈0.18 cores) | exact client bytes 790,626,304; gateway delta was 2,975,810 B (0.376%) higher because the client intentionally closed a larger response at the target byte count |

### v2 mode and health evidence

- Every one of the 11 persisted TEST shares returned `status=relay` from `prepare-route`; the measured URL was the returned relay origin.
- TESTBOX control health returned 200; gateway health reported `frps_process_healthy=true` and `route_ready=true`; the gateway tunnel metric was online=1.
- The gateway global relayed-byte counter moved from 1,557,278 to 795,159,392 during the measured cell. The corresponding counter rate was 234.163 Mbps, close to the client goodput rate; it includes the small post-threshold amount already buffered toward the client.
- Mid-transfer TCP info on VERSA's FRP data connection showed 244.241 Mbps delivery rate, 39,096 retransmitted bytes and 27 total retransmitted segments (`retrans:0/27`, so none outstanding). CLIENT-EAST showed two out-of-order receive packets. Host-global `TcpRetransSegs` changed by +1 on CLIENT-EAST and +2,391 on busy VERSA; the latter is not attributable because VERSA carries unrelated production traffic. No gateway/frps application error was observed.
- CLIENT-EAST had 95% aggregate CPU idle during its v2 sample. For comparison, its v1 sample had 23% aggregate idle.

**Interpretation:** The observed v2 relay rate was 121.251 Mbps faster than v1 and essentially filled the known path: 233.285/246.6 = 94.60%. v1 reached 112.034/246.6 = 45.43%. Thus this field cell strongly supports the existing conclusion that v1's ceiling is in its WebRTC/SCTP/browser path, while v2's relay transport can use nearly all available path capacity. The observed v2/v1 rate ratio is 2.082×.

**Caveats:** This is a **partial matched measurement**. The deployed v2 TEST image had only Immich sessions. Searching all 7,142 items in its nine healthy manifests found no 790,626,304-byte asset, and its API rejected registering the v1 OpenCloud source because that build does not support the `opencloud` share type. To avoid touching any non-test/production Immich resource, the v2 cell used exactly 790,626,304 bytes from the beginning of an existing larger TEST Immich original and intentionally closed the stream at that byte count. Therefore byte volume, client VM, direction, hour, and network path match, but source bytes and sink do not: v1 was Chrome + the application download sink, while v2 was curl + `head`, matching the existing v2 harness's HTTP client style. Curl's exit 23 is the expected broken pipe after the exact-byte sink completed, not a transport failure. v1 byte count is implied by the known file size; v2 has both the client count and gateway counter. CPU values are single active samples, not integrated CPU time. No CLIENT-WEST v2 cell was run.

**Artifacts:** CLIENT-EAST `/tmp/exp10-v1-east.log` and `/tmp/exp10-v2-east.log`; local `/tmp/exp10-v1-t0`, `/tmp/exp10-v2-t0`, `/tmp/exp10-v2-gateway-bytes-before`, and the before/after `nstat` captures. Sensitive transient share/route URLs and manifests were kept only in mode-0600 `/tmp` files and removed after extracting aggregate evidence. The first v1 launch attempt used the API-returned private/Tailscale `share_url`, failed immediately with `ERR_NAME_NOT_RESOLVED`, transferred no bytes, and was excluded. The measured retry reconstructed the documented TESTBOX `/s/<code>` URL. The retry's launch SSH remained attached and timed out at 30 s despite `setsid nohup`; independently polled driver and agent logs proved the single job was running, so it was not relaunched.

## Restoration

**Fully restored and verified.** TESTBOX ended with `caddy=active`, `sharebridge-test=active`, and v2 `sharebridge`, `sharebridge-relay-gateway`, and `sharebridge-relay-frps` all inactive. Its listeners were again Caddy on `:443` and the v1 server on `:8080`, with no v2 listeners. VERSA ended with `sb-run` running and `sharebridge-agent-test` stopped; the production `sharebridge-agent` remained up and was never touched. The v1 TEST signalling URL returned HTTP 200. Both client VMs were reachable and had no `node`, `chrome`, `Xvfb`, or `iperf3` process.
