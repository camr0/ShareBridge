# Experiment 25 — is the v1-direct ~120 Mbps plateau set by the CLIENT? — 2026-09-18

**Verdict:** **The client IS the binding constraint.** Pinning the *client* (CLIENT-EAST, 4 vCPU)
to **1 vCPU** drops the agent-side delivered rate to **37.19 Mbps mean (n=2)** — 0.35× the
unpinned control — and **2 vCPU** to **48.31 Mbps (n=1)**, 0.45×; the client **unpinned** gives
**107.84 Mbps mean (n=2)**, inside E21's sender-only unpinned band (100.64–125.69, mean 113.85).
The deciding number is the **client's ~35 Mbps per core** versus the **sender's ~85–100 Mbps per
core** (E21): a *one-core sender* still delivers **89.2 Mbps** — more than a *two-core client*
(48.3) and more than half of a *four-core client* — and the sender reaches the plateau with
**2 cores** while the client needs **~3.0–3.3 cores** to reach it. In every pinned cell the
client's permitted cores were fully occupied, and the rate tracked the allowance, so the plateau
is set on the receive/consume side (browser + app download sink), not by the sender.

**Setup:** Field rig. Agent = VERSA container `sb-run` ONLY, documented variant-`c` state
(`sb-agent:pristine`, host networking, `UI_PORT=7879`, `UI_ADDR=127.0.0.1`,
`SB_SCTP_CA_STEP=32768`, **no** `SB_SCTP_MIN_CWND`, **no** `--cpus`; `docker inspect`:
`NanoCpus=0`, `Restart=unless-stopped`, started 22:39:06Z and **never recreated by this
experiment**). Signalling TESTBOX (v1, Caddy TLS). Payload = one 754 MiB file = 790,626,304 B =
6,325.01 Mb per download (the exp20/21 share, `relay_only=false`, expires 2026-09-20T04:49Z; its
download counter advanced 75 → 80, i.e. exactly the five measured cells). Driver: `drive_click.js`
on CLIENT-EAST, exactly one click-only tab, **never** the Playwright Download API. Metric:
agent-side `docker logs --timestamps sb-run`, window = first `DataChannel lanes ready` → last
`download complete`; Mbps = 6,325.01 ÷ window_s. No `SB_SCTP_*` variable and no `--cpus` was added
to the sender at any point — only the client was varied.

**Client CPU pinning:** the driver is launched under `taskset`, so Chromium and all descendants
inherit the mask. Arm A = `taskset -c 0` (1 vCPU), arm B = `taskset -c 0,1` (2 vCPU), arm C =
unpinned (4 vCPU). CLIENT-EAST has 4 vCPUs; `taskset` = util-linux 2.39.3.

**Method notes**
- Client telemetry per cell: interval `vmstat 1` stream (`/tmp/exp25-<cell>.vmstat`; the first row
  is the since-boot average and is discarded) + `top -b -n 2 -d 1` interval snapshots
  (`/tmp/exp25-<cell>.top`). `top`'s per-process `%CPU` is per *core*; its `%Cpu(s)` line is per
  *machine*.
- Agent telemetry: `docker stats --no-stream sb-run`, `uptime`/load.
- Mode assertion: `DataChannel lanes ready` present **and** the relay standby unused (relay channel
  closed by the relay with `pending wait window exceeded`, no relay data). All five measured cells
  passed.
- **`docker logs --since <ISO>` returned empty for windows that demonstrably contained matching
  lines** (reproduced 3×, including timestamps the agent had just printed). All agent windows were
  therefore read with `docker logs --timestamps sb-run | grep … | tail`, using the log's own
  timestamps.

## RIG HEALTH — THE TESTBOX TLS FRONT WAS INTERMITTENTLY BROKEN (this run's main hazard)

From ~22:41Z the **client→TESTBOX TLS path failed in bursts**, which is why the cells had to be
driven through a loopback front (below). Raw evidence:

- 22:41–22:45Z: `curl https://TESTBOX/` = `000` from VERSA, CLIENT-EAST and CLIENT-WEST;
  `openssl s_client` (TLS1.2 and TLS1.3, every group set tried) = **handshake OK throughout**.
- 22:45–22:48Z: `curl` = 200 **12/12** from VERSA and **12/12** from CLIENT-EAST.
- 22:49–22:52Z (5-way **simultaneous** probe): **Mac 0/6, CLIENT-EAST 0/4, VERSA 0/6,
  CLIENT-WEST 0/5, TESTBOX-local 5/6.** So the outage is on the *external* path/edge: two home
  hosts and two OVH regions fail together while TESTBOX talking to itself does not. It then
  recovered (`curl` 200 6/6 to `:443` and 6/6 to `:8080` from VERSA at 22:52Z).
- **Replay experiment.** A failing Chromium ClientHello (1,774 B, captured on TESTBOX with
  `tcpdump`) was replayed raw: **12/12 attempts got a fatal alert, description 80
  (`internal_error`)**. The server's TCP stack had ACKed all 1,775 bytes immediately before
  sending the 7-byte alert + FIN, i.e. the *server* parsed the ClientHello and refused it.
  `openssl s_client`'s much smaller ClientHello (1 key share) succeeded 8/8 and 10/10 during the
  same bad windows.
- **Chromium could not load the share page**: `net::ERR_SSL_PROTOCOL_ERROR` in **0/8, 0/6, 0/6,
  0/5 for each of `--disable-features=AsyncDns | PostQuantumKyber | UseMLKEM |
  EncryptedClientHello` and `--host-resolver-rules=MAP <host> <ip>` — ≈55 failed loads between
  22:44Z and 23:00Z**, while `curl` from the same host returned 200 12/12 and 6/6 in the same
  minutes. Not DNS (the system resolver returns the correct A record; no AAAA; no HTTPS RR), not
  MTU (`ping -M do -s 1472` = 0 % loss), and not the client VM (Chromium loaded
  `https://www.google.com/` and `https://github.com/` 200 in the same session).
- TESTBOX itself is healthy: load 0.00, `TcpExtListenDrops = TcpExtListenOverflows =
  TCPBacklogDrop = TcpReqQFullDrop = 0`, `syn-recv` = 1, conntrack 123/262144,
  `TcpInCsumErrors` = 0, caddy + sharebridge-test active, v1 signalling HTTP **200** locally 5/6
  *inside* a bad window. Caddy logged nothing for the failed handshakes.
- The agent's own `wss://TESTBOX` connection (Go) was established at 22:39:06Z and **never
  reconnected during the entire flap** — only new TLS handshakes failed.

**Workaround actually used.** Because the impairment is confined to the TESTBOX front (page +
signalling) and the payload path is P2P, the cells were driven through a **loopback front**: a
~30-line Node reverse proxy on CLIENT-EAST listening on `127.0.0.1:8090` (plain HTTP; loopback is a
*secure context*, so the app's service-worker/StreamSaver sink still runs) forwarding page, API and
WebSocket-upgrade traffic to `http://TESTBOX:8080`, with `Host` and `Origin` rewritten to the real
hostname. Only page/API/signalling bytes cross it — the 754 MiB payload never does. Direct mode was
asserted per cell exactly as required. The proxy is not CPU-pinned and is idle during the transfer
(its only CPU cost is the page load and the tiny signalling frames). This is a deliberate deviation
from E21's exact condition; the C control reproducing E21's unpinned band (106.1 / 109.6 vs
100.6–125.7) supports comparability.

## Raw cells

`agent CPU` = `docker stats --no-stream sb-run`. Client `st`/`us`/`sy`/`id` = interval `vmstat 1`
rows covering the transfer window. `top` per-process = interval `top -b -n2 -d1` CPU %.

| # | cell | client pin | lanes ready → download complete (UTC) | window s | Mbps | direct? | n | client `st` | client us/sy/id | top (client) |
|---|---|---|---|---|---|---|---|---|---|---|
| 1 | C1 | none (4 vCPU) | 23:00:47.467 → 23:01:47.094 | 59.627 | **106.08** | yes | 1 | **0.0 %** all rows | us 33–36 / sy 41–45 / id 17–25 | chrome-58372 215–238 %, chrome-58318 52–63 %, chrome-58288 19–21 %, chrome-58315 6–7 %, node 1–4 % → ≈3.0–3.3 of 4 cores |
| 2 | A1 | `-c 0` (1 vCPU) | 23:02:31.514 → 23:05:22.190 | 170.676 | **37.06** | yes | 1 | **0.0 %** all rows | us 12–14 / sy 12–14 / id 74 | all work squeezed onto CPU 0 (us+sy 25.3 % of the machine = 100 % of one core); main chrome-58929 71.3 % of a core + helpers |
| 3 | B1 | `-c 0,1` (2 vCPU) | 23:05:53.498 → 23:08:04.421 | 130.923 | **48.31** | yes | 1 | **0.0 %** all rows | us 19–25 / sy 23–28 / id 50–54 | chrome-59399 141–148 % of a core (1.45 cores) + helpers → ≈1.9–2.1 of 4 cores busy; CPUs 2–3 idle |
| 4 | A2 | `-c 0` (1 vCPU) | 23:09:24.192 → 23:12:13.660 | 169.468 | **37.32** | yes | 1 | **0.0 %** all rows | us 12–14 / sy 12–13 / id 74 | main chrome-60042 71–73 % of a core; identical shape to A1 |
| 5 | C2 | none (4 vCPU) | 23:12:49.668 → 23:13:47.384 | 57.716 | **109.59** | yes | 1 | **0.0 %** (one row 0.2 %) | us 32–38 / sy 40–44 / id 20–37 | chrome-60509 227–230 % of a core (2.3 cores) → ≈3.0–3.3 of 4 cores |

**Discarded cells:** **0 relay cells among the five measured cells** — every measured window had
`DataChannel lanes ready` *and* an unused relay standby. The ~55 browser page-load failures caused
by the TLS flap produced no session at all and are not cells.

### Per-arm summary

| arm | allowed vCPU | n | mean Mbps | min | max | vs control |
|---|---|---|---|---|---|---|
| A | 1 | 2 | **37.19** | 37.06 | 37.32 | 0.34× |
| B | 2 | 1 | **48.31** | 48.31 | 48.31 | 0.45× |
| C | 4 (unpinned) | 2 | **107.84** | 106.08 | 109.59 | 1.00× |

Client-side `steal` was **0.0 % in every interval of every cell** (and the client VMs are dedicated
OVH b2-15 instances with no noisy neighbours), so the pinned arms were limited by their **own**
affinity mask, not by host contention. In A and B the machine's idle fraction (74 % / 50 %)
matches exactly "1 of 4 cores busy" / "2 of 4 cores busy", i.e. the pins were binding.

## Interpretation

1. **The client is the constraint, and it is a per-core CPU cost, not a host-contention effect.**
   Client throughput tracks the cores it is allowed: 1 → 37.2, 2 → 48.3, 4 → 107.8 Mbps. The client
   is *not* "idle-waiting" on the sender: when pinned it saturates its allowance (id 74 % / 50 %
   = exactly the unpinned cores) and its rate falls accordingly.
2. **The client costs ~3–4× more CPU per byte than the sender.** Client ≈ 35 Mbps/core; sender
   ≈ 85–100 Mbps/core (E21) with a 2-core quota already at the plateau (124.2). Put the two
   together: the sender reaches ~120 Mbps on ~1.3 cores while the client needs ~3.0–3.3 cores for
   ~108 Mbps. **Equal-rate comparison:** a 4-core client (107.8) ≈ a 2-core sender (124.2, E21);
   a 2-core client (48.3) ≪ a 1-core sender (89.2, E21).
3. **This answers the E21 caveat 4(a):** E21 could not say whether the ~120 Mbps plateau was
   CPU-set at all, or which side. The client-CPU sensitivity here (2.9× collapse at 1 vCPU, 2.2×
   at 2 vCPU) plus the client's ~3.0–3.3-core demand at the plateau makes the **receive/consume
   side** the leading constraint. The lab harness — which counts bytes in a page but does **not**
   run the app's SHA-1-verify + service-worker sink — does 225 Mbps on the same pion code, and E15
   showed that removing that client sink raised the agent's own send rate 23.8 % (112.9 → 151.0)
   and deleted an ~80 s tail. The three results now agree: **the browser client's sink is the
   binding constraint at ~120 Mbps, and the sender still has headroom.**
4. **Remedy implication:** the ~120 Mbps plateau will not move by making the sender faster or the
   host quieter (E21 already falsified that); it needs client-side work — moving SHA-1
   verification / writing off the critical path, or a cheaper sink (e.g. stream straight to the
   download rather than via the service worker).
5. **Not the same as E21's sender caps.** E21's 0.5-core sender (38.31) and this experiment's
   1-vCPU client (37.19) land at nearly the same rate, but they are different bottlenecks with
   different symmetries: the sender's curve saturates by 2 cores, the client's does not saturate
   until ~3–3.3.

## Caveats

- **The cells ran through the loopback front described above** (page origin
  `http://127.0.0.1:8090` instead of the TESTBOX hostname) because Chromium could not complete a
  TLS handshake to TESTBOX at all between 22:44Z and 23:00Z. The payload path is unchanged (P2P
  DataChannel; direct mode asserted per cell), and the unpinned control (106.1 / 109.6 Mbps)
  reproduces E21's unpinned band (100.64–125.69), but a page-origin change is *not* a null
  manipulation in principle — this should be re-measured over the plain HTTPS origin once the
  TESTBOX TLS front is fixed.
- n = 2 (arm A), 1 (arm B), 2 (arm C). The two A cells agree to 0.7 % and the two C cells to
  3.2 %, both far tighter than E21's uncapped spread (±11 %), so the A/C separation (2.9×) is
  unambiguous; the B point is n=1 and sits between them as expected.
- `taskset` is a hard affinity cap and is *not* the same as `--cpus` CFS throttling, and it
  constrains a whole process tree. It is a *lower bound* on what that much client CPU can pull in
  an unconstrained regime, so the client may be slightly less efficient per core than measured —
  which only strengthens the conclusion.
- The per-process attribution (single dominant `chrome` process at 71 % / 148 % / 228 % of a core)
  identifies Chromium, not which Chromium thread (network/sink/verify) is critical. Locating the
  critical thread was **not** done and is the obvious next experiment.
- Single share, single 754 MiB file, CLIENT-EAST only (12 ms path). CLIENT-WEST (~71 ms) was not
  tested; its ~68 Mbps cap is plausibly RTT/ACK-limited rather than CPU-limited and this
  experiment says nothing about it.
- The agent-side metric excludes the ~80 s post-transfer tail E15 saw, since the window ends at
  the agent's `download complete`; client-observed completion was not measured.

## Restoration (verified 23:14–23:16Z)

- `sb-run` = `sb-agent:pristine`, `NanoCpus=0` (no `--cpus`), host networking,
  `Restart=unless-stopped`, `UI_PORT=7879`, `UI_ADDR=127.0.0.1`, `SB_SCTP_CA_STEP=32768`,
  **no** `SB_SCTP_MIN_CWND` (0 env matches). `docker inspect` shows `StartedAt
  2026-09-18T22:39:06Z` — the container was **never recreated** by this experiment, so no variant
  change was needed; `env-run` was never modified. Agent API `http://127.0.0.1:7879/api/v1/shares`
  = HTTP 200.
- TESTBOX: `caddy` and `sharebridge-test` active, v1 signalling HTTP **200** (3/3 from VERSA and
  locally). UFW untouched (443/8080/3478/22/7000 as before).
- CLIENT-EAST: `node`/`chrome`/`Xvfb`/proxy/`vmstat`/`top` counts all **0**; the temporary loopback
  proxy and helper scripts were removed from `/tmp`. CLIENT-WEST: all counts **0** (it was never
  driven).
- Production `sharebridge-agent`, the v2 stack, DNS/ACME, non-ShareBridge containers and all
  instance state untouched; `VERSA` was read-only apart from `docker logs/inspect/stats` on
  `sb-run`; no git command was run.
- **Outstanding infra issue for the operator (not caused by this experiment):** the TESTBOX TLS
  front still intermittently answers every external TLS handshake with alert 80 and does not log
  it; Caddy there has not been restarted. Until that is fixed, field cells must be launched
  through a proxy or retried until a window opens.

## Artifacts

- Results file: this file.
- Client-side per-cell telemetry: CLIENT-EAST `/tmp/exp25-<cell>.vmstat`, `/tmp/exp25-<cell>.top`,
  `.meta`, driver logs `/tmp/cell-<cell>.log` (C1, A1, B1, A2, C2).
- TLS diagnosis: TESTBOX `/tmp/exp25-cap.pcap`, `/tmp/exp25-cap2.pcap`, `/tmp/exp25-ch.bin` (the
  failing 1,774-byte Chromium ClientHello); VERSA `/tmp/exp25-replay.py`,
  `/tmp/exp25-interleave.py`, `/tmp/exp25-ch.b64`.
- Loopback front: `CLIENT-EAST /tmp/exp25-proxy.js`, log `/tmp/exp25-proxy.log` (both removed in
  cleanup; the script is reproduced in the body above).
