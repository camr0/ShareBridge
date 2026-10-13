# Experiment 46 — native-HTTPS reference for the browser-DataChannel attribution (candidate N3) — 2026-10-13

**Status: COMPLETE.** 13 completed transfer cells (5 native-HTTPS, 6 DataChannel, 2 non-browser conduit
controls) + 3 direct `iperf3` path controls + 1 aborted cell (retained, flagged) + 1 incomplete DataChannel
cell (retained, flagged). Every cell was appended to this file as it finished.

**Verdict — the attribution must be split by RTT, and that split is the result:**

| RTT | browser **native HTTPS** (same client, same hour, 754 MiB) | browser **DataChannel** knob OFF (production) | browser **DataChannel** knob ON | ratio native ÷ DC-OFF |
|---|---|---|---|---|
| **~12.1 ms** (CLIENT-EAST) | **231.57 / 231.57** (conduit A) | **97.69 / 103.99** | 117.80 (exp43) | **2.23–2.37×** |
| **~70.0 ms** (CLIENT-WEST) | **222.79 / 230.54 / 232.66** (conduit A) · **52.33 / 37.80** (conduit B) | **13.78** (incomplete) / **35.42** | **66.82 / 62.32** | 6.3–16.6× (but see §3) |

- **At 12 ms the attribution is airtight.** The browser's native download runs at **231.6 Mbps = 97.5 % of a
  non-browser `curl` on the identical conduit (237.5)** and **95.0 % of the direct `iperf3` path control
  (243.7)**, while the same browser's DataChannel in the production configuration does 97.7–104.0. Neither the
  conduit, the tunnel, the path, nor Chromium's download machinery can explain 2.3×. The ceiling is inside the
  WebRTC/SCTP/DTLS DataChannel + app-sink path. **The campaign's attribution is confirmed at 12 ms by direct
  measurement rather than by inference.**
- **At 70 ms the attribution does NOT survive in its strong form.** The *direct VERSA→CLIENT-WEST path itself*
  only carried **72.8–120.3 Mbps of single-flow TCP** in this session, and a tunnelled HTTPS stream over that
  same path delivered **67.2–75.8 Mbps** — while the browser's DataChannel with the knob ON delivered
  **62.3–66.8 Mbps**. The DataChannel is at the level of an ordinary TCP stream over the same path; it is not
  the demonstrated ceiling. The alarming production number at 70 ms (13.8–35.4, and one hard stall) is a
  **sender-side cwnd** phenomenon, exactly as exp43 already argued — not a browser receive limit.
- **The brief's premise for 70 ms was wrong on this day.** It assumed a 111–163 Mbps native-TCP control band at
  70 ms. Today the 70 ms path gave 72.79 Mbps at 06:18 and 120.27 Mbps at 07:01 — **the path moved 1.65×
  within the session**, which is itself a first-order confound reported below.

## Questions (verbatim from the brief)

> If native HTTPS hits ~111–163 Mbps at 70 ms while the DataChannel does 13.4 (knob off) / 59.3 (knob on), the
> attribution is airtight. If the native download also lands low, the attribution collapses and the
> path/browser-download story reopens.

**At 12 ms: airtight (2.3×).** At 70 ms the native download *did* land low relative to the DataChannel — but
for a reason that is not the browser: the path itself is ~73–120 Mbps, and the tunnel needed to reach VERSA
costs another 26–44 %. See Interpretation §3 for why that makes the 70 ms attribution collapse anyway, and §4
for what remains unresolved.

## Method — read this first; the briefed method is not physically available on this rig

### 1. VERSA cannot accept an inbound TCP connection, so a direct HTTPS server is impossible

`VERSA` is behind a residential NAT. Measured from `CLIENT-EAST` against VERSA's public addresses:

| probe | result |
|---|---|
| TCP `:22 :80 :443 :7878 :8080 :8443 :10000 :18443 :18444` | only `:443` answers — and `ss -ltn` on VERSA shows **nothing** listening on 443, so that is a *different* LAN host |
| TCP `:18443` on VERSA's public IPv6 (a live listener bound `*:18443` on the host) | connection times out — the home IPv6 firewall drops unsolicited inbound |

The agent's DataChannel reaches VERSA only because the residential NAT is **full-cone for the UDP mapping the
ICE agent created** (ICE succeeds, relay standby expires unused). That does not generalise to an inbound TCP
listener, so "a spare port on VERSA" is not available. No router/DNS/forwarding change was made.

### 2. What was built instead: the payload is served **from VERSA** through a reverse SSH tunnel

```
chrome (CLIENT-*) --https--> TESTBOX:18444           [ufw: ONE temporary allow rule, tag exp46-temp]
                                   | python TCP relay (dumb, never parses the stream)
                                   v
                              TESTBOX 127.0.0.1:18443 [ssh -R, loopback-bound (sshd GatewayPorts=no)]
                                   | ssh transport, opened OUTBOUND from VERSA
                                   v
                              VERSA 127.0.0.1:18444   [python TLS origin, self-signed cert]
                                   |
                              /home/ali/sbhttps/payload.bin — 790,626,304 B, byte-exact
```

Two conduits answer two different questions, and **both were measured**:

| conduit | route | what its number means |
|---|---|---|
| **A — `CLIENT-* -> TESTBOX:18444`** | VERSA → home uplink → TESTBOX → client | what the **browser's own download machinery** can do when neither leg is the bottleneck. **No extra process runs on the measured client** (the tunnel terminates on TESTBOX), so the client-side CPU picture is unchanged from the campaign's cells. |
| **B — `CLIENT-WEST -> 127.0.0.1:18443`** (tunnel anchored on the client itself) | VERSA → **the direct VERSA→client network path** → client | what the **same network path the DataChannel uses** can carry |

Conduit B is the faithful "same path" arm; conduit A is the faithful "same client, same browser, no path
limit" arm. In both, the payload originates on VERSA and crosses the home uplink exactly once.

### 3. Measurement

- **Primary: the origin's own byte counters** (`VERSA:/home/ali/sbhttps/server.log`). Window =
  `REQ-START` → `REQ-END` around the single `GET /payload`; `bytes` is what the server actually wrote to the
  socket. Driver-independent. 5-second `PROGRESS` lines give the rate trace.
- **Cross-checks:** the client's own NIC RX (`/sys/class/net/ens3/statistics/rx_bytes`) sampled before launch
  and after the transfer; `TESTBOX:/tmp/exp46-relay.log`.
- **Playwright's Download API was never called.** `drive_native.js` does a plain `page.goto()` to a
  `Content-Disposition: attachment` URL, so **every byte is fetched by Chromium's own download manager with
  zero page JS** (F18's technique); the `download` event is consumed only to be logged.
- **DataChannel cells** use the unchanged campaign path: `drive_click.js`, `NTABS=1`, agent-side window
  (`first DataChannel lanes ready` → `last download complete` from `docker logs --timestamps sb-run`), with
  `relay: pending wait window exceeded` as the direct-mode assertion (every cell below shows it).
- Payload = **790,626,304 B = 6,325.010432 Mb** in every cell.
- Every client cell was gated (`node=0 chrome=0 Xvfb=0`) before launch and cleaned afterwards; cleanup uses
  TERM, 3 s, then **KILL** (exp45's M1: the driver's `node` catches SIGTERM).

## Raw cells — native HTTPS

### Conduit A (via TESTBOX) — Chromium native download manager

| # | client / RTT | UTC REQ-START | UTC REQ-END | window s | **Mbps** | bytes | NIC RX delta | gate |
|---|---|---|---|---|---|---|---|---|
| N-EAST-1 | CLIENT-EAST ~12.1 ms | 06:13:26.145 | 06:13:53.458 | 27.313 | **231.577** | 790,626,304 | 794,780,649 | GATE-CLEAN |
| N-EAST-2 | CLIENT-EAST ~12.1 ms | 06:14:25.442 | 06:14:52.755 | 27.313 | **231.571** | 790,626,304 | 794,863,250 | GATE-CLEAN |
| N-WEST-1 | CLIENT-WEST ~70.0 ms | 06:16:18.902 | 06:16:47.292 | 28.390 | **222.788** | 790,626,304 | 793,977,529 | GATE-CLEAN |
| N-WEST-2 | CLIENT-WEST ~70.0 ms | 06:17:08.572 | 06:17:36.008 | 27.436 | **230.541** | 790,626,304 | 793,963,549 | GATE-CLEAN |
| N-WEST-3 | CLIENT-WEST ~70.0 ms | 06:17:58.422 | 06:18:25.608 | 27.186 | **232.656** | 790,626,304 | 794,072,637 | GATE-CLEAN |

- **12 ms: 231.577 / 231.571 — mean 231.574, spread 0.003 %.** Identical windows to the millisecond.
- **70 ms: 222.788 / 230.541 / 232.656 — mean 228.662, range 222.8–232.7.**
- Rate traces are flat, not crawling (12 ms: 227.6→237.2 Mbps; 70 ms: 208.3→229.2 Mbps).
- NIC RX is 1.004–1.005 × the payload in every cell (SSH + TLS framing ≈ 0.5 %).
- The browser's `download` event fired in every cell (`suggested=debian-13.4.0-amd64-netinst.iso`). No
  artifact was retained on disk (Playwright's temp artifacts directory is cleaned when the browser is torn
  down), so byte-level completeness is evidenced by the origin's `REQ-END bytes=790626304` plus the NIC delta
  (794.8 MB), not by a file.

### Conduit B (direct VERSA→CLIENT-WEST path, tunnel anchored on the client)

| # | UTC REQ-START | UTC REQ-END | window s | **Mbps** | bytes | notes |
|---|---|---|---|---|---|---|
| N-WEST-D1 | 06:21:17.953 | 06:23:18.810 | 120.857 | **52.334** | 790,626,304 | complete; rate decays 60.0 → 52.4 Mbps across the cell |
| N-WEST-D2 | 06:23:59.386 | 06:26:05.157 (**aborted**) | 125.77 | *(≈44.6 at abort)* | 789,839,872 | **INVALID — aborted by my own cleanup 0.1 % short of the end** (`REQ-ERR BrokenPipeError`). Retained as measured; not a throughput |
| N-WEST-D3 | 06:55:27.470 | 06:58:14.806 | 167.336 | **37.798** | 790,626,304 | complete; rate ramps 17.8 → 37.8 Mbps across the cell |
| **(A, for contrast)** | — | — | — | **228.662** | — | the same browser, same file, same hour, no path limit |

**Conduit B: 52.334 / 37.798 — mean 45.07, spread 38 %.** These are the only native-HTTPS numbers that ride
the direct path, and they are **confounded by TCP-over-TCP** (see Interpretation §3).

## Raw cells — non-browser controls

| # | when (UTC) | what | Mbps | note |
|---|---|---|---|---|
| C-EAST-A | 06:12:18→06:12:45 | `curl` through conduit A, CLIENT-EAST | **237.51** | the identical conduit the browser used, 70 s earlier |
| C-WEST-A | 06:15:43→06:16:12 | `curl` through conduit A, CLIENT-WEST | **222.16** | |
| C-WEST-A2 | ~07:03 | `curl` through conduit A, CLIENT-WEST (post) | **231.02** | conduit A is stable: 222.16 → 231.02 |
| C-WEST-B | 06:19:45→06:21:09 | `curl` through conduit B, CLIENT-WEST | **75.79** | direct-path ceiling for this conduit, early |
| C-WEST-B2 | 07:01:22→07:02:56 | `curl` through conduit B, CLIENT-WEST (post) | **67.16** | later — got *worse* while raw TCP got better |
| P-EAST-1 | ~06:11 | direct `iperf3` VERSA→CLIENT-EAST, 1 flow, 10 s, omit 3 | **243.72** | 204 retransmits / 10 s |
| P-WEST-1 | ~06:18 | direct `iperf3` VERSA→CLIENT-WEST, 1 flow, 10 s, omit 3 | **72.79** | 2 retransmits / 10 s |
| P-EAST-2 | ~07:01 | direct `iperf3` VERSA→CLIENT-EAST (post) | **244.84** | 0 retransmits |
| P-WEST-2 | ~07:01 | direct `iperf3` VERSA→CLIENT-WEST (post) | **120.27** | 0 retransmits |

**The 70 ms path moved 72.79 → 120.27 Mbps (+65 %) between 06:18 and 07:01 on the same pair, while the 12 ms
path moved 243.72 → 244.84 (+0.5 %).** That is the single largest confound in this experiment and it is why the
70 ms arrow points where it does.

## Raw cells — DataChannel (agent-side window)

`sb-run` = `sb-agent:pristine`, `UI_PORT=7879`. For the OFF cells `docker inspect` shows **no `SB_SCTP_*` at
all** (the production configuration); for the ON cells `SB_SCTP_CA_STEP=32768` (verified in the container env
after recreation with `run-variant.sh`, which was executed and never modified).

| # | client / RTT | knob | UTC lanes ready | UTC download complete | window s | **Mbps** | count | mode |
|---|---|---|---|---|---|---|---|---|
| DC-EAST-OFF-1 | CLIENT-EAST 12.1 ms | unset | 06:29:19.900289132 | 06:30:24.648450604 | 64.748 | **97.687** | 49 | direct (standby expired unused 06:30:04) |
| DC-EAST-OFF-2 | CLIENT-EAST 12.1 ms | unset | 06:31:08.566492563 | 06:32:09.387172489 | 60.821 | **103.993** | 50 | direct (standby expired 06:31:52) |
| DC-WEST-OFF-1 | CLIENT-WEST 70.0 ms | unset | 06:32:54.988049693 | **never completed** | 855 (killed) | **~13.78 active, then stall** | — | direct (standby expired 06:33:38) |
| DC-WEST-OFF-2 | CLIENT-WEST 70.0 ms | unset | 06:51:49.050850725 | 06:54:47.627244251 | 178.576 | **35.420** | 53 | direct (standby expired 06:52:33) |
| DC-WEST-ON-1 | CLIENT-WEST 70.0 ms | 32768 | 06:46:19.431485620 | 06:47:54.091788466 | 94.660 | **66.818** | 51 | direct (standby expired 06:47:03) |
| DC-WEST-ON-2 | CLIENT-WEST 70.0 ms | 32768 | 06:48:59.047113430 | 06:50:40.536354035 | 101.489 | **62.322** | 52 | direct (standby expired 06:49:43) |

**DC-WEST-OFF-1 — the incomplete cell, in full, because it is data.** It never produced a `download complete`.
Its NIC RX (driver-independent) went 10,994,345,853 (last reading before launch, 06:26; no traffic in the
intervening 6.7 min) → 11,554,079,593 at 06:38:20, i.e. **559,733,740 B in 325 s = 13.78 Mbps**, a rate that
reproduces exp43's 13.405 mean; from 06:38:20 to the kill at ~06:47:10 the NIC delta was **0.003–0.005 Mbps**
over 10 s and 30 s windows — a **hard stall**, with `node`/`chrome` alive, `sb-run` at 0.81 % CPU and 0 B/s.
Whole-cell average to the kill ≈ 5.2 Mbps. There is **no `download complete` and no agent-side error line**.
It is retained as an incomplete cell, not as a throughput; its active-phase rate is reported under the caveat
that it assumes negligible idle traffic in the 6.7-minute gap before launch.

**Reference arms from exp43** (same rig, same share, same clients, 02:00–05:00 UTC, i.e. 1.5–4 h earlier):
CLIENT-EAST knob OFF **95.757** (n=3, 93.9–98.0), knob ON **117.799** (n=3, 107.9–132.9);
CLIENT-WEST knob OFF **13.405** (n=3, 11.9–15.2), knob ON **59.326** (n=3, 55.7–63.5).

## Answer table

### 12 ms — CLIENT-EAST

| level | measurement | Mbps | note |
|---|---|---|---|
| direct path, `iperf3` 1 flow | P-EAST-1 / P-EAST-2 | 243.72 / 244.84 | the path's own capacity |
| conduit A, `curl` | C-EAST-A | 237.51 | tunnel + path, no browser |
| **browser native HTTPS** | N-EAST-1 / N-EAST-2 | **231.577 / 231.571** | 97.5 % of the conduit control |
| browser DataChannel, knob OFF (production, same hour) | DC-EAST-OFF-1 / 2 | 97.687 / 103.993 | |
| browser DataChannel, knob ON (exp43) | — | 117.799 | |

**Native ÷ DataChannel-OFF = 2.23–2.37×. Native is 95.0 % of the raw path. The DataChannel is 40–43 % of the
raw path.** Every alternative explanation is excluded by a same-hour control.

### 70 ms — CLIENT-WEST

| level | measurement | Mbps | note |
|---|---|---|---|
| direct path, `iperf3` 1 flow | P-WEST-1 → P-WEST-2 | 72.79 → **120.27** | **the path moved 1.65× inside the session** |
| conduit B, `curl` | C-WEST-B / B2 | 75.79 / 67.16 | tunnel + path, no browser |
| **browser native HTTPS via conduit B** | N-WEST-D1 / D3 | **52.334 / 37.798** | confounded by TCP-over-TCP |
| **browser native HTTPS via conduit A** | N-WEST-1/2/3 | **222.788 / 230.541 / 232.656** | no path limit; isolates the browser |
| browser DataChannel, knob ON (same hour) | DC-WEST-ON-1 / 2 | **66.818 / 62.322** | |
| browser DataChannel, knob OFF (same hour) | DC-WEST-OFF-1 / 2 | **13.78 (incomplete) / 35.420** | volatile; one cell hard-stalled |
| browser DataChannel, knob OFF/ON (exp43) | — | 13.405 / 59.326 | |

## Interpretation

**1. At 12 ms the campaign's attribution is confirmed by direct measurement.** Native HTTPS **231.57** vs
DataChannel knob OFF **97.69 / 103.99** = **2.23–2.37×**, with two independent controls ruling out every
alternative: the `curl` control on the *identical conduit* is 237.51 (so the browser costs 2.5 % over a raw
syscall reader) and the direct `iperf3` control is 243.72–244.84 (so the tunnel costs 2.6 %). Neither the
conduit, the tunnel, the path, nor Chromium's native download manager can explain a 2.3× gap, so the gap is
inside the WebRTC/SCTP/DTLS DataChannel plus the app sink. This is what F18/E28 inferred across experiments;
it is now measured on one client in one hour.

**2. Conduit A at 70 ms isolates the browser from the path, and the browser is again not the problem.** On a
conduit whose own `curl` ceiling is 222.16–231.02 Mbps, the browser's native download does **222.8 / 230.5 /
232.7** — it equals or exceeds the `curl` control. Whatever limits the campaign's 70 ms numbers,
**Chromium's ability to receive 754 MiB over HTTPS is not it.**

**3. At 70 ms the attribution collapses — and the brief's premise is why it took measuring to see it.** The
brief expected a 111–163 Mbps native-TCP control band at 70 ms. The *direct path* VERSA→CLIENT-WEST gave
**72.79 Mbps at 06:18 and 120.27 Mbps at 07:01**. Against that denominator:

- the knob-**ON** DataChannel (**62.3 / 66.8 Mbps**) sits **at the level of an ordinary tunnelled HTTPS stream
  over the same path (67.2–75.8)**. There is no longer a browser-sized gap to explain: the DataChannel is
  performing like TCP does on that path.
- the knob-**OFF** DataChannel (**13.8 / 35.4**, plus one hard stall) is 11–29 % of the path and is a
  **sender-side cwnd** story — exactly exp43's mechanism (1 MTU per RTT of cwnd growth against a ~525 KB BDP,
  reset by every loss), not a receive-path story. The hard stall in DC-WEST-OFF-1 (0.00 Mbps for ~9 minutes
  with client idle and agent at 0.81 % CPU) is a new, more extreme instance of the same regime.
- so at 70 ms **the ceiling is the path and the sender, not the browser's DataChannel receive path**. This is
  the branch the brief called "the attribution collapses and the path/browser-download story reopens".

**4. What is genuinely unresolved at 70 ms, stated as such.** The two native-HTTPS cells that ride the direct
path (52.33, 37.80) are **lower** than the knob-ON DataChannel (62.32–66.82) and lower than the conduit-B
`curl` control (67.16–75.79). That is not evidence that native HTTPS is slow — it is evidence that **the
conduit itself is defective at 70 ms**: the TLS stream is carried inside an SSH tunnel (TCP-over-TCP) over a
73–120 Mbps, occasionally lossy path, and TCP-over-TCP is known to underperform there. Conduit B's own `curl`
runs at 56–69 % of the raw `iperf3` number in the same minutes. **I therefore do not claim a clean
native-HTTPS-over-the-direct-path number at 70 ms.** Getting one needs a TCP listener on VERSA that a client
VM can reach — which this rig cannot provide (Method §1). The defensible 70 ms statements are the raw path
capacity (72.8–120.3), the tunnelled-stream capacity (67.2–75.8), and the DataChannel numbers (13.8–66.8).

**5. For v1 vs v2.** At 12 ms a v1-direct client pays **~2.3×** for the DataChannel versus a native HTTPS
download of the same bytes — F18's client-architecture story is confirmed by direct measurement rather than by
inference across experiments, and this prices the "v1-direct fallback over HTTPS byte-range" middle path at
~2.3× at 12 ms. At 70 ms the same move would buy **little to nothing** on this path (a tunnelled HTTPS stream
gets 67–76 Mbps; the knob-ON DataChannel already gets 62–67), so the 70 ms problem should be attacked as a
sender-side cwnd problem first. The campaign's one-line conclusion should be split by RTT.

## Caveats

- **The briefed direct-from-VERSA HTTPS server was impossible** (Method §1). Conduit B recovers the *network
  path* but wraps TLS in SSH; its cost is bounded by its own `curl` control (67–76 vs 73–120 raw), and the
  browser cells on it are n=2 and 38 % apart — I do not treat them as a browser ceiling.
- **The 70 ms path moved 1.65× inside the session** (72.79 → 120.27 Mbps), and the conduit-B `curl` control
  moved the *other* way (75.79 → 67.16). Every 70 ms comparison in this file is therefore cross-minute, and
  the DataChannel-vs-native gap at 70 ms must be read against control pairs taken minutes apart, not seconds.
- **Small n.** Native 2 (12 ms) and 3 + 2 (70 ms); DataChannel 2 per same-hour arm; exp43 supplies n=3 per arm
  1.5–4 h earlier. Arm separations are far outside within-arm spread; means carry it.
- **No queueing discipline was applied** — conduit A's two legs are STRIDE/plain TCP; this is not N4.
- **One invalid cell (N-WEST-D2)** aborted by my own cleanup at 99.9 %, retained and flagged; **one incomplete
  cell (DC-WEST-OFF-1)**, retained with its active-phase rate under the stated assumption.
- **`sb-run` was recreated twice** (ON, then OFF) with the rig's own `run-variant.sh`; the ON-arm log was not
  dumped before recreation, so the ON cells' raw log lines exist only as transcribed above. No cell was lost.
- Client CPU during DC-WEST-OFF-1 was `vmstat` idle = **100 %** — the client was not the limit. The aggregate
  `vmstat` caveat (cannot exclude one saturated thread) applies, but at 0 % it cannot hide anything.
- **Two `iperf3` controls had 0 retransmits (post) versus 204 (EAST, 06:11) and 2 (WEST, 06:18)** — the EAST
  path was lossy early and clean later, so "the path" is not a constant.

## Method failure worth recording (cost ~25 minutes, no cells lost)

The signalling server was never broken. My first health probes used the **dotted** hostname
`135.148.235.157.sslip.io`, but Caddy is configured for the **dashed** `135-148-235-157.sslip.io` and has a
certificate only for that name. Any client sending the dotted SNI correctly receives a TLS
`internal_error` alert — which `curl` reports as `000`, and which Chromium reports as
`net::ERR_SSL_PROTOCOL_ERROR` (it killed my first DataChannel launch). `openssl s_client -servername
<dashed>` succeeded throughout, which is what eventually exposed the mistake. **A health check must use the
configured hostname, not an equivalent-looking one.**

**A consequence to flag: I restarted Caddy on TESTBOX (`sudo systemctl restart caddy`) while chasing this.**
It changed nothing (loopback TLS 200 and dashed-name TLS 200 before and after; dotted name 000 before and
after) and it was unnecessary. Restarting was within a ~2 s no-op window with no cell in flight; it is
reported here rather than hidden. Caddy and `sharebridge-test` are `active` at hand-back.

## Restoration — verified

**VERSA**
- HTTPS origin and **both** `ssh -N` tunnels killed; `:18443` and `:18444` free; no `ssh -N` processes remain.
- `/home/ali/sbhttps/payload.bin` (754 MiB) and `payload2.bin` removed; `server.log`, the scripts, the cert and
  the experiment keypair are kept as artifacts. Nothing was written outside `/home/ali/sbhttps`.
- **Production untouched:** `docker ps` = **49 containers** (same count as exp43's and exp45's hand-back);
  `sharebridge-agent` (7878) `running`, `StartedAt 2026-10-12T19:54:16Z` — predates this run, never restarted.
  No non-ShareBridge container was started, stopped, restarted or exec'd. No v2 container was touched.
- **Rig state on hand-back:** `sb-run` = `sb-agent:pristine`, **`SB_SCTP_CA_STEP` unset (knob OFF — the
  production configuration, and the same state exp45 left)**, `UI_PORT=7879`, running, API 200.
- No `netem`/`tc` change was ever made on VERSA. No sysctl was changed on VERSA.

**TESTBOX**
- `ufw` rule for `18444/tcp` **added and removed**; `ufw status` at hand-back shows only 22, 443, 7000, 8080.
- Relay process killed; `/tmp/exp46-relay.py`, `/tmp/run_relay.sh`, `/tmp/ch46.pcap` removed
  (`/tmp/exp46-relay.log` kept as an artifact).
- **`~/.ssh/authorized_keys` restored** from the pre-experiment copy: 1 line, 0 `sb-exp46-tunnel` references.
- Web root **untouched** — no deploy, no revert, no file written under `/opt/sharebridge-test/web/`.
- `caddy` and `sharebridge-test` both `active`. Signalling: dashed hostname 200.

**CLIENT-EAST / CLIENT-WEST**
- All processes zero at hand-back: `node=0 chrome=0 Xvfb=0 iperf3=0`.
- `net.core.rmem_default = rmem_max = 212992` — never changed.
- `tc qdisc show dev ens3` = stock `fq_codel` — no qdisc was ever added, nothing to remove.
- CLIENT-WEST `~/.ssh/authorized_keys` restored: 1 line, 0 `sb-exp46-tunnel` references.
- Driver files left in `~/sbtest` (`drive_native.js`, `sbcell_native.sh`) and logs in `/tmp/cell-*`.

**Never done:** no `git` command of any kind; no instance created, shelved, stopped, resized or deleted (the
campaign is still in flight — flagged for the operator, exactly as exp43 and exp45 did); no DNS, ACME,
production or v2-relay change; no router change.

## E1 — not run, and why

Candidate E1 (v2 direct, native HTTPS mode) was **not measured**. The v2 relay stack is stopped on TESTBOX and
starting it collides with the v1 rig on 443/8080. The brief forbids swapping the rig, so E1 is left for a later
experiment. Note that this experiment weakly informs it anyway: conduit A **is** a native-HTTPS download with
zero page JS against the same 754 MiB file (228.66 Mbps at 70 ms, 231.57 at 12 ms), so "v2-native-HTTPS class
throughput" is reproducible on this hardware; what is missing is the v2 *relay stack* end-to-end.

## Artifacts

- This file: `docs/superpowers/spikes/results/2026-10-13-exp46-native-https-reference.md`
- VERSA: `/home/ali/sbhttps/{serve_https.py,run_https.sh,run_tunnel.sh,run_tunnel_west.sh,probe.sh,cert.pem,key.pem,id_exp46,id_exp46.pub,server.log}` — `server.log` holds every `REQ-START/PROGRESS/REQ-END` line quoted above.
- TESTBOX: `/tmp/exp46-relay.log`
- Clients: `~/sbtest/drive_native.js`, `~/sbtest/sbcell_native.sh`; driver logs `/tmp/cell-N-*.log` and
  `/tmp/cell-DC-*.log` on the client that ran them.
- Share consumption: **48 → 53 downloads** (5 DataChannel downloads; native-HTTPS cells do not touch the share).
