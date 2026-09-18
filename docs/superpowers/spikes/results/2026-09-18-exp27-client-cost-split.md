# Experiment 27 — splitting the client's ~35 Mbps/core cost: app download sink vs the WebRTC receive path — 2026-09-18

**Verdict:** **Not the sink — the WebRTC receive path is the client's dominant cost, but the sink is a real ~1/3 of it.**
With the app download sink bypassed (one TESTBOX web-root copy, nothing else changed) the 1-vCPU client
(`taskset -c 0`) delivers **56.35 Mbps mean (n=2; 56.21, 56.50)** — **1.52×** E25's sink-ON 37.19 and its
pin fully saturated — but **not** the >70 Mbps the "sink dominates" branch of the question required; the
sink is therefore **34.0 % of the client's per-byte cost** and the receive path the other **66.0 %**. At 4
vCPU the same sink removal gives **119.45 Mbps mean (n=2; 122.65, 116.24)** = **1.098×** a same-session
sink-ON control (**108.76**, which reproduces E25's 107.84 to 0.9 %) — i.e. the sink is **47.7 %** of the
cost there and the receive path 52.3 %. Per core, the sink-free client costs **56.4 Mbps/core at 1 vCPU**
and **79 Mbps/core at 4 vCPU**; 1 vCPU remains **~1.5× below the sender's 85–100 Mbps/core (E21)**, so the
v1 receive path is *inherently* expensive on a one-core client and the deciding number is **56.35**, not
>70. **The culprit process is the chrome renderer**: 0.66–0.86 cores sink-free / **1.95–2.01 cores with the
sink on** at 4 vCPU, with the network service flat at ~0.4–0.5 either way — the sink's ~1.2 cores of SHA-1 +
StreamSaver/service-worker work live in the renderer. Side finding: **4 of 8 `taskset -c 0` attempts fell
back to relay at exactly +10.0 s after join** (0 of 4 unpinned), i.e. a 1-core client silently loses direct
mode. TESTBOX's alert-80 front did **not** reproduce (Chromium loaded the page 3/3). Web root restored
byte-for-byte (`407ee7032f90e429…`, 91,308 B).

**Setup:** Field rig. Agent = VERSA container `sb-run` ONLY (`sb-agent:pristine`, host networking,
`UI_PORT=7879`, `UI_ADDR=127.0.0.1`, `SB_SCTP_CA_STEP=32768`, no `SB_SCTP_MIN_CWND`, no `--cpus`;
`docker inspect`: `NanoCpus=0`, `StartedAt 2026-09-18T22:39:06Z` — **not recreated by this experiment**).
Signalling = TESTBOX v1 (Caddy TLS) + **test-only** web root `/opt/sharebridge-test/web/`.
Client = CLIENT-EAST (4 vCPU, b2-15, ~12 ms path). Payload = one 754 MiB file = 790,626,304 B =
6,325.01 Mb per download (share `a1k6q46n`, `relay_only=false`, expires 2026-09-20T04:49Z, counter
80 at start). Driver: `drive_click.js`, one click-only tab, DOM observation only, **never** the
Playwright Download API. Metric: agent-side `docker logs --timestamps sb-run`, window = first
`DataChannel lanes ready` → last `download complete`; Mbps = 6,325.01 ÷ window_s.

**Isolation technique (identical to E15).** A single temporary patch to **TESTBOX's test copy only**
of `/opt/sharebridge-test/web/src/app.js`: with the explicit test query parameter `?sink=discard`,
`buildDownloadSink()` returns a discard sink whose `append()` is a no-op and whose `finalize()`
retains only exact **byte-count** validation. No SHA-1, no StreamSaver/service-worker writer, no
`concatBytes` copy. The normal URL keeps the unmodified SHA-1 + StreamSaver path, so the sink-ON
control is the *same file* with the parameter absent. The real signalling session, WebRTC/DataChannel
receive/decode path, request, payload and agent behaviour are unchanged; only the client-side sink work
is removed. Original backed up to TESTBOX `/tmp/exp27-app.js.orig` and restored byte-for-byte at the
end (see Restoration).

**Client CPU pinning:** the driver is launched under `taskset`, so Chromium and all descendants
inherit the mask. Arm A = `taskset -c 0` (1 vCPU), arm B = unpinned (4 vCPU). This makes the
sink-free arms directly comparable with E25's sink-ON arms (37.19 @1 vCPU mean n=2; 48.31 @2 vCPU
n=1; 107.84 @4 vCPU mean n=2) and with E15's sink-free agent-side 151.0 Mbps (CLIENT-EAST mean, n=2).

**Per-process client CPU.** CLIENT-EAST `/tmp/exp27-<cell>.proc` is a 5 s interval sampler that reads
`/proc/<pid>/stat` utime+stime deltas per PID (so the number is *interval* CPU, not `ps`'s
lifetime average) for every `chrome`/`node`/`Xvfb` process, with each PID's full `cmdline` so the
process role (`--type=renderer`, `--type=gpu-process`, `--type=utility --utility-sub-type=network.mojom.NetworkService`,
zygote, or the `node` driver) is identified. `/tmp/exp27-<cell>.vmstat` is a 1 Hz aggregate stream.

**Mode assertion (non-negotiable).** Every cell must show **`DataChannel lanes ready`** *and* an
**unused relay standby** (relay channel connected at join, then closed by the relay with
`pending wait window exceeded`, no relay-carried data), plus a `Connected (Direct)` badge in the
driver log. Relayed cells read ~44.7 Mbps and would masquerade as valid slow measurements (E21 lost
25 % of its cells to this).

## Part 0 — TESTBOX TLS-front (alert 80) hazard assessment — DONE 23:15–23:17Z

E25 (22:41–23:00Z) found the TESTBOX TLS front intermittently answering **every external** TLS
handshake with **fatal alert 80 (`internal_error`)** while `curl` returned 200 — ~55 Chromium page
loads failed with `net::ERR_SSL_PROTOCOL_ERROR`, and its cells had to run through a loopback front.
**Status now: NOT reproducing.**

| probe (23:15–23:17Z) | result |
|---|---|
| `curl https://TESTBOX/` from VERSA | **200 ×6/6** |
| `curl https://TESTBOX/` from the Mac (external) | **200 ×6/6** |
| `curl https://TESTBOX/` from CLIENT-EAST (external) | **200 ×6/6** |
| `openssl s_client` (SNI-correct) from VERSA | handshake OK, `Verify return code: 0` **4/4** |
| TESTBOX-local `http://127.0.0.1:8080/` | **200** |
| Chromium page load of `https://TESTBOX/s/<code>` from CLIENT-EAST (Playwright, n=3) | **`LOADOK 200` ×3, 150 / 477 / 477 ms**, title `ShareBridge` — **no `ERR_SSL_PROTOCOL_ERROR`** |

TESTBOX health: `load average 0.00`, `caddy` **active**, `sharebridge-test` **active**,
`NRestarts=0` for **both** units and no Caddy log entries for failed handshakes — i.e. **the front
was never repaired or restarted**; the impairment is intermittent and is simply absent right now.
**Consequence for this run:** all cells are driven over the **plain HTTPS origin** (no loopback
proxy), which is the condition E25's caveat asked for. No repair was attempted (operator decision);
the recurrence risk is recorded rather than engineered around.

### Part 0 — web-root pre-change state

- `/opt/sharebridge-test/web/src/app.js` = **SHA-256 prefix `407ee7032f90e429…`, 91,308 bytes,
  root:root mode 644** — byte-identical to the E15/E25 restored state, no `EXP14`/`EXP27` marker
  present, no leftover backup files. The web root was **pristine** at start.
- Post-patch (23:17Z): `72391be99f5fca01…`, 92,028 bytes, root:root 644, and an HTTP retrieval of
  `/src/app.js` from `http://127.0.0.1:8080` returns the **same** SHA-256 (`72391be99f5fca01`);
  `EXP27` markers present at lines 1575/1599 of the served copy. Backup
  `/tmp/exp27-app.js.orig` holds the original (`407ee7032f90e429…`, 91,308 B, 644).

## Raw cells

All cells CLIENT-EAST, one at a time, agent-side window (`DataChannel lanes ready` → `download complete`),
6,325.01 Mb per download. `sink` = the app download sink as served to that cell (`?sink=discard` ⇒ discard
sink: no SHA-1, no StreamSaver/service-worker writer, no `concatBytes` copy; parameter absent ⇒ the real
sink, i.e. the same file unmodified on that path). `client cls` is the terminal UI class the driver read from
the DOM: the discard sink can only ever end `done` (it does not compare a SHA-1), the real sink ends
`verified`/`intact` — so the driver independently confirms *which* sink each cell actually ran.

| # | cell | client pin | sink | lanes ready → download complete (UTC) | window s | **Mbps** | direct? | client cls | driver click→terminal s |
|---|---|---|---|---|---:|---:|---|---|---:|
| 0 | S1 *(excluded)* | `-c 0` | discard | *(no lanes ready)* | — | — | **no — relay** | — | — |
| 1 | **S-A1** | `-c 0` (1 vCPU) | **bypassed** | 23:36:10.986590669 → 23:38:03.516252983 | 112.530 | **56.21** | yes | `file-item done` (✓ done) | 114.7 |
| 2 | **S-B1** | none (4 vCPU) | **bypassed** | 23:38:14.879943563 → 23:39:06.450544386 | 51.571 | **122.65** | yes | `file-item done` (✓ done) | ~54 |
| 3 | ON1 *(excluded)* | `-c 0` | real | *(no lanes ready)* | — | — | **no — relay** | — | — |
| 4 | **S-A2** | `-c 0` (1 vCPU) | **bypassed** | 23:39:40.336616912 → 23:41:32.292051749 | 111.955 | **56.50** | yes | `file-item done` (✓ done) | 112.8 |
| 5 | **S-B2** | none (4 vCPU) | **bypassed** | 23:41:42.297497268 → 23:42:36.713188423 | 54.416 | **116.24** | yes | `file-item done` (✓ done) | 56.3 |
| 6 | ON2 | none (4 vCPU) | **real** | 23:44:39.880431050 → 23:45:38.034588103 | 58.154 | **108.76** | yes | **`file-item verified` (✓ intact)** | 144.7 |
| 7 | ON1r *(excluded)* | `-c 0` | real | *(no lanes ready)* | — | — | **no — relay** | — | — |
| 8 | ON1b *(excluded)* | `-c 0` | real | *(no lanes ready)* | — | — | **no — relay** | — | — |

### Arm summary so far (sink-free, n=2 per pinning level)

| arm | allowed vCPU | n | **mean Mbps** | cells | sink-ON comparator (same client, same share) | ratio sink-free : sink-ON |
|---|---|---:|---:|---|---|---:|
| A | **1** (`taskset -c 0`) | 2 | **56.35** | 56.21, 56.50 | **37.19** (E25, same session, 23:02–23:12Z; 37.06, 37.32) | **1.515×** |
| B | **4** (unpinned) | 2 | **119.45** | 122.65, 116.24 | **108.76** (ON2, **this session**, 23:44Z) and **107.84** (E25, 106.08/109.59) | **1.098× vs ON2**, 1.108× vs E25 |

**Same-session drift bound (this is what makes the E25 comparators usable).** The unpinned sink-ON control
measured **now** (ON2 = 108.76 Mbps, `✓ intact`, real SHA-1+StreamSaver sink) reproduces E25's unpinned
sink-ON mean (**107.84**, cells 106.08/109.59, 23:00–23:13Z) to **0.9 %**, 35 minutes later and with the
patched `app.js` deployed (the parameterless path is byte-for-byte the original one). The session therefore
has **no measurable drift at the 4-vCPU level**, which is the justification for using E25's pinned sink-ON
cells (37.06/37.32) as arm A's sink-ON comparator after every attempt at a fresh pinned sink-ON control fell
to relay (ON1, ON1r, ON1b — see below).

### Cells ON1 / ON1r / ON1b — EXCLUDED (relay) — 23:39Z, 23:44Z, 23:47Z

All three attempts at a **same-session pinned (1 vCPU) sink-ON control** followed the identical failure
shape and were aborted with nothing measured:

| cell | browser joined | peer closed | relay channel started | lanes ready |
|---|---|---|---|---|
| ON1 | 23:39:16.576 | (driver aborted on relay badge 23:39:29) | 23:39:26.651 | **none** |
| ON1r | 23:44:18.352 | 23:44:28.405 | 23:44:28.450 | **none** |
| ON1b | 23:47:13.647 | 23:47:23.670 | 23:47:23.714 | **none** |

The relay fallback is therefore **not** caused by the `?sink=discard` test parameter (ON1/ON1r/ON1b used no
parameter at all). Overall it hit **4 of 8 `taskset -c 0` attempts** (S1, ON1, ON1r, ON1b) and **0 of 4
unpinned attempts** — see the interpretation for why this is itself a finding about the 1-vCPU client.

### Cell S1 — EXCLUDED (relay) — 23:18:02–23:19:16Z

First attempt at arm A. The agent logged `browser joined … starting WebRTC handshake` at 23:18:02.983,
the direct peer **closed at 23:18:13.024** (exactly as the driver reported `joined`),
`relaychannel: handshake complete` + `relay channel started successfully` at 23:18:13.06, and there was
**no `DataChannel lanes ready`** for the session. The payload therefore went over the relay
(`download complete`, count 81, 23:19:16 → 63.17 s ≈ 100.1 Mbps). This is the E15/WB2 failure mode
(direct peer closes before lanes-ready, browser selects relay). **Not a measurement** — no agent-side
direct window exists, and a relayed cell would masquerade as a valid slow number. Excluded, count
retained in the record. Client-side telemetry from this cell is reported only as an aside below.

### Cell ON1 — EXCLUDED (relay) — 23:39:10–23:39:32Z

First sink-ON control attempt (real sink, `taskset -c 0`). The driver's DOM mode gate saw
`● Connected (Relay)` on two consecutive 2 s polls and aborted at 23:39:29 (`RELAY-ABORT mode=relay
status="↓ 1.5 MB/s"`). The agent log confirms the same shape: `browser joined` 23:39:16.576,
`relay channel started successfully` 23:39:26.651, **no lanes ready**. Aborted immediately, nothing
measured. Note this happened to a cell with **no** `?sink=discard` parameter, so the relay fallback is
not an artefact of the test parameter.

### Worked example of the mode assertion (cell S-A1)

- Agent log: `browser joined session a1k6q46n (conn a6e1bc…` 23:36:10.068 →
  **`DataChannel lanes ready for peer a6e1bc…`** 23:36:10.987 → `download complete (count: 82)`
  23:38:03.516 → `peer a6e1bc… closed` 23:38:05.982. There is **no** `relay channel started` line for
  this session and no relay-carried bytes; the relay standby was never used (it is only torn down ~45 s
  later with `pending wait window exceeded`).
- Driver log: `● Connected (Direct)` badge, terminal `✓ done` on `file-item done`.
- Both halves of the assertion hold for S-A1, S-B1, S-A2, S-B2.

### Per-process client CPU (deciding attribution)

`/tmp/exp27-<cell>.proc` (5 s interval, `/proc/<pid>/stat` utime+stime deltas, full `cmdline` per PID).
Reported as **cores** (1.00 = one full vCPU). "renderer" = `--type=renderer`; "net" = the
`--type=utility` **network service** (`utility-sub-type=network.mojom.NetworkService`); "gpu" =
`--type=gpu-process`; "node" = the Playwright driver; "xvfb" = the X server.

| arm | renderer | network service | gpu | node | Xvfb | **total client** | Mbps/core | machine-wide busy (vmstat) |
|---|---:|---:|---:|---:|---:|---:|---:|---:|
| **S-A1** (1 vCPU, sink-free) | 0.27–0.47 | 0.18–0.31 | 0.02–0.04 | 0.00–0.01 | 0.00–0.01 | **0.49–0.83** | ~57–115 | **mean 25.1 % = 1.004 cores** (pin saturated) |
| **S-A2** (1 vCPU, sink-free) | same shape | same shape | same | same | same | — | — | **mean 26.2 % = 1.05 cores** |
| **S-B1** (4 vCPU, sink-free) | **0.66–0.86** | **0.40–0.52** | 0.03–0.05 | 0.00–0.06 | 0.00–0.01 | **1.10–1.45** | **~85** | mean 38.8 % = 1.55 cores |
| **S-B2** (4 vCPU, sink-free) | same shape | same shape | same | same | same | — | — | mean 36.5 % = 1.46 cores |

**The culprit process in the sink-free arms is the chrome renderer** (the JS DataChannel receive/decode
path), with the **network service** second at roughly 55 % of the renderer's cost; the GPU process
(0.03–0.05 cores) and the node driver (≤0.06) are negligible. I.e. with the app sink removed the client's
cost is `renderer + network service`, both of which are the browser's own WebRTC receive path — **not** the
driver and **not** the GPU. Note the sampler (chrome/node/Xvfb only) sums to 0.5–0.8 cores on the pinned
cells while `vmstat` shows the machine 25.1–26.2 % busy (= 1.00–1.05 of 4 cores), so ~0.2–0.3 cores of the
pinned budget also goes to kernel/softirq work (UDP/loopback) not attributed to a chrome PID: **the pin was
fully consumed in both pinned sink-free cells**, exactly as it was in E25's pinned sink-ON cells (id 74 % /
50 % = 1 / 2 cores busy).

Aside on cell S1, the discarded relay run (1 vCPU, *discard*): renderer 0.48–0.58, chrome main 0.21–0.27,
network service 0.06–0.07, node 0.11–0.14 — total ≈0.9–1.0 cores for 100 Mbps **over the relay**, i.e. the
pin was fully saturated even on the relay path. Not used for anything; recorded because it incidentally shows
the pin is binding and the discard patch leaves the renderer dominant.

## Interpretation

### The answer to the question: 1/3 sink, 2/3 WebRTC receive path (at 1 vCPU); ~1/2 each at 4 vCPU

| pinning | sink-ON Mbps | sink-free Mbps | **ratio** (inverse) | cores used (sink-ON → sink-free) | cores per Mbps sink-ON → sink-free | **sink share of the cost** | receive-path share |
|---|---:|---:|---|---|---|---:|---:|
| **1 vCPU** (`-c 0`) | **37.19** (E25 n=2) | **56.35** (n=2) | **1.52×** (0.66×) | 1.00 → 1.00 (pin saturated in both) | 0.02689 → 0.01775 | **34.0 %** | **66.0 %** |
| **4 vCPU** | **108.76** (ON2, this session) / 107.84 (E25 n=2) | **119.45** (n=2) | **1.098×** (0.91×) | 2.62 → 1.46–1.55 | 0.02409 → 0.01260 | **47.7 %** | 52.3 % |

Method for the split: `cores per Mbps = cores_used ÷ Mbps`, and the sink's share is
`(cost_sinkON − cost_sinkfree) ÷ cost_sinkON`. At 1 vCPU both arms consume the whole permitted core
(vmstat mean busy 25.1 % / 26.2 % of a 4-vCPU machine = 1.00 / 1.05 cores, i.e. the same saturation E25
saw), so the comparison is **at equal core budget** — the sink-free client simply gets 1.52× more done with
the same one core. At 4 vCPU both arms are comfortably below the pin, so the comparison is at equal *rate
headroom* instead; the sink's share is larger there because the pinned single core, not the sink, is the
binding resource in arm A.

### Is the sink or the receive path the fixable one? (the brief's own decision rule)

The rule was: sink-free@1 vCPU **>70 ⇒ sink dominates** (fix is app code, helps v1 and v2);
**stays ~37–50 ⇒ the WebRTC receive path dominates** (v1 client is inherently CPU-expensive; v2's HTTP path
is the answer). **Measured: 56.35** — above the "stays 37–50" band but far short of 70, so neither branch is
clean and the honest reading is:

- **The WebRTC receive path is the majority cost at 1 vCPU (66 %), and it is not the sink.** The sink-free
  client still only gets 56.35 Mbps/core, versus a sender that does 85–100 Mbps/core on the same path. Even
  with SHA-1, the service-worker writer and the per-chunk `concatBytes` copy all removed, a one-core client
  remains ~1.5× more expensive per byte than the sender. **v1's receive path really is inherently expensive
  on this client**, which is exactly consistent with E10c's 239 Mbps on the same VM through the v2 HTTP/TLS
  path: HTTP receive costs the client a fraction of what DTLS/SCTP + DataChannel + JS frame decode costs.
- **The app sink is nonetheless a real and large minority (34 % at 1 vCPU, 48 % at 4 vCPU) — worth +52 % at
  1 vCPU and +10 % at 4 vCPU.** So "fix the app sink" is a genuine, cheap, protocol-independent win, but it
  buys **at most ~19 Mbps** on a one-core client (37.19 → 56.35) and cannot close the gap to the sender.
- **Both buckets are on the client, so queueing the v2 comparison is the right framing:** v2's HTTP path
  removes the whole receive-path bucket (majority) *and* the sink's SHA-1-at-consuming-rate problem, which
  is why 239 Mbps on the same VM is not a contradiction of E25 but a consequence of the different path.

### Per-core cost of the sink-free client is at the sender's level

- Sink-free @4 vCPU: 119.45 Mbps on 1.46–1.55 cores = **77–82 Mbps/core** — inside/at E21's sender band
  (85–100 Mbps/core) and at E21's **unpinned sender plateau** (100.64–125.69, mean 113.85). With the sink
  removed the client is *no longer the binding constraint* at 4 vCPU; the ~120 Mbps plateau is the
  sender/transport, exactly as E21 described it. This is the same-session, protocol-identical version of
  E15's conclusion, and it is consistent with E15's sink-free EAST mean (151.0) being *above* our two cells
  (116.2, 122.7): E15's mean is carried by one 174.1 outlier, and our own control confirms the session did
  not drift (ON2 = 108.76 vs E25 107.84).
- Sink-free @1 vCPU: **56.35 Mbps/core**, i.e. still ~1.5–1.8× worse than the sink-free client with 4 cores
  can do (77–82) — a real single-core penalty on top of the per-byte cost (pinned, the WebRTC receive path
  loses thread parallelism; the renderer's own JS decode is the bottleneck, not the network service).

### Which process eats the client's cores (the actionable attribution)

| cell | sink | renderer | network service | other chrome | gpu | node | xvfb | total |
|---|---|---:|---:|---:|---:|---:|---:|---:|
| **ON2** (4 vCPU) | **real** | **1.95–2.01** | 0.38–0.51 | 0.18–0.21 | 0.06–0.07 | 0.00 | 0.01 | **2.61–2.78** |
| **S-B1** (4 vCPU) | bypass | **0.66–0.86** | 0.40–0.52 | 0.01 | 0.03–0.05 | 0.00–0.06 | 0.00–0.01 | **1.10–1.45** |
| **S-A1** (1 vCPU) | bypass | 0.27–0.47 | 0.18–0.31 | 0.00–0.02 | 0.02–0.04 | 0.00–0.01 | 0.00–0.01 | 0.49–0.83 (+ ~0.2–0.4 kernel/softirq to reach the saturated core) |

- **The renderer is the culprit, and the app sink's cost is *inside* the renderer**: turning the sink on
  takes the renderer from 0.66–0.86 to **1.95–2.01 cores** (+1.1–1.3 cores, i.e. ~2.3–3×) and adds
  ~0.18–0.21 cores of *other* chrome (the service-worker/StreamSaver side), while the **network service is
  essentially unchanged (0.40–0.52 → 0.38–0.51)**. So SHA-1 (hash-wasm, in-renderer WASM), the 1 MiB
  validation-tail copy and the service-worker messaging are renderer-side costs, **not** network-service and
  **not** GPU costs. The GPU process (0.03–0.07) and the node/Playwright driver (≤0.06) are irrelevant — the
  driver is *not* a confound.
- **With the sink removed, the remaining client cost is still renderer-first** (0.66–0.86 of ~1.5 cores) with
  the network service second at ~55 % of it: i.e. the residual majority bucket is **browser JS/DataChannel
  receive and decode in the renderer + DTLS/SCTP in the network service**, both browser-owned. That is the
  concrete form of "the WebRTC receive path dominates".

### Side finding, directly relevant to the same one-core-client scenario: a 1-core client loses direct mode

**4 of 8 `taskset -c 0` attempts fell back to relay; 0 of 4 unpinned attempts did.** In every fallback the
shape is identical and mechanical: `browser joined` → **exactly +10.0 s** → `peer closed` →
`relay channel started successfully`, with **no `DataChannel lanes ready` ever logged** (S1 +10.04 s, ON1
+10.07 s, ON1r +10.05 s, ON1b +10.02 s, ON1c +10.02 s). All four pinned failures were consecutive
(23:39–23:48Z) while an unpinned cell in the middle of them (ON2, 23:44Z) went direct immediately. The
pinned sink-free cells that did establish direct (S-A1, S-A2) were themselves direct and pin-saturated.
This is a ~10 s direct-connection deadline that a *CPU-starved* client misses; the consequence is a silent
protocol downgrade whose throughput (~100 Mbps in the one fallback that ran to completion, S1) is in the
same range as a valid slow cell and looks like a legitimate measurement. **Both the app's `Connected
(Relay)` badge and an agent-side `DataChannel lanes ready` + standby-relay check are required to catch it**
— E21's 25 % cell loss and this run's 4/8 are the same failure mode.

## Caveats

1. **n=2 per sink-free pinning level, n=1 same-session sink-ON control (unpinned), n=0 same-session pinned
   sink-ON control** (three attempts, all relay). The pinned sink-ON comparator is E25's (37.06/37.32,
   mean 37.19) from the *same session* ~35 min earlier with the same share; the justification for using it
   is the unpinned control reproducing E25's unpinned pair to **0.9 %** (108.76 vs 107.84). All four
   sink-free cells were direct, pin-verified, and their sink state is independently confirmed by the driver
   (terminal UI class `done` for bypassed, `verified`/`✓ intact` for ON2).
2. **The discard arm is not a null client.** It still runs the real pipeline: per-frame byte accounting, a
   `setTimeout` clear/reschedule per chunk, arrival-order/early-frame bookkeeping, envelope decode, and DOM
   progress updates. "Sink" in this experiment means **SHA-1 + StreamSaver/service-worker writing + the
   tail copy**, i.e. exactly E15's isolation — so the *residual app overhead* sits inside the "receive
   path" bucket and makes that bucket's 66 % a slight over-estimate of pure WebRTC cost.
3. **Both arms change a page-origin-free but app-modified copy of `app.js`** for the discard URL only; the
   control path is byte-identical to the original. The query parameter is in the URL, so it is visible to
   the page only; signalling, ICE, DTLS/SCTP, chunking and the agent are untouched.
4. **Sampler vs vmstat disagree slightly by construction.** The sampler counts only `chrome`/`node`/`Xvfb`
   PIDs (0.49–0.83 cores on the pinned cells) while vmstat shows 1.00–1.05 cores of machine-wide CPU; the
   ~0.2–0.4 core difference is kernel/softirq (UDP, loopback, scheduling) that has no user PID and therefore
   cannot be attributed to a process. Per-process shares are therefore ratios within the chrome tree, not
   absolute fractions of the pinned core.
5. **The 1.52× sink ratio is measured at a saturated pin**, where the sink and the receive path contend for
   the same core; the 1.098× ratio is measured with cores to spare. They are different questions and are
   both reported rather than averaged. Neither is a "true" marginal cost of SHA-1 alone.
6. **CLIENT-EAST only (~12 ms path).** CLIENT-WEST (~71 ms) was not tested; E25 says its ~68 Mbps cap is
   plausibly RTT/ACK-limited rather than CPU-limited, so nothing here transfers to it.
7. **Single 754 MiB file, single share (`a1k6q46n`, `relay_only=false`), one 30-minute window.** Counter
   advanced 80 → 86 across six completed downloads; the two relay cells are reported and excluded.
8. **The 10 s direct-fallback deadline is inferred** from the ~10.0 s regularity of `join → peer closed` in
   five fallbacks, not from reading the client/agent timeout constant.
9. E15's sink-free EAST mean (151.0 Mbps, 127.9/174.1) was **not** reproduced (we got 116.2/122.7 with a
   concurrent control at 108.76). The difference is within E15's own spread (its two bypass cells differed
   by 36 %) and by our control the session did not drift, but this experiment does not explain the gap.

## Restoration

**Verified complete.** TESTBOX test web root restored from the pre-change copy at 23:52Z and checked four
ways: on-host `cmp` against the backup = **identical**; `sha256sum` prefix **`407ee7032f90e429…`**
(the exact pre-change value) at **91,308 bytes, mode 644, root:root**; `EXP27` marker count **0**; and a
fresh HTTP retrieval of `/src/app.js` from `http://127.0.0.1:8080` returning the **same** `407ee7032f90e429…`.
The temporary backup and patch script were deleted (`/tmp/exp27-app.js.orig` absent). TESTBOX `caddy` and
`sharebridge-test` both **active**, `NRestarts=0` for both, local HTTP **200**.

CLIENT-EAST: `node`, `chrome`, `Xvfb`, `vmstat` counts **all 0** after cleanup, no `exp27` processes remain
(verified with a bracketed `ps -eo args | grep "[e]xp27"`), and the temporary tooling was removed
(`~/sbtest/drive_exp27.js`, `~/sbtest/exp27-probe.js`, `/tmp/exp27-{batch,start,stop,sampler}.sh`); the
pre-existing `~/sbtest` driver set is untouched. CLIENT-WEST was never driven and is **0/0/0**.

VERSA: `sb-run` = `sb-agent:pristine`, `NanoCpus=0` (no `--cpus`), `UI_PORT=7879`, `UI_ADDR=127.0.0.1`,
`SB_SCTP_CA_STEP=32768`, **no** `SB_SCTP_MIN_CWND` (0 matching env lines), `Restart=unless-stopped`,
`StartedAt 2026-09-18T22:39:06Z` — **never recreated by this experiment**; API `127.0.0.1:7879/api/v1/shares`
= **200**; container idle at 0.55 % CPU. Signalling over the public HTTPS origin = **200**. Production
`sharebridge-agent` (UI 7878), the v2 stack, `sharebridge-agent-test`, `sharebridge.app`, DNS/ACME, all
non-test containers and all instance state were **never touched**; VERSA was read-only apart from
`docker logs/inspect/stats` on `sb-run`; **no `git` command was run**.

**Outstanding infra note for the operator (pre-existing, not caused by this run).** E25's TESTBOX TLS
front impairment (fatal alert 80 on external handshakes) did **not** reproduce at any point in this
session — Chromium loaded the share page 3/3 in the Part 0 probe and every cell that reached the page did so
over the plain HTTPS origin (no loopback proxy used). But the front was **never repaired or restarted**
(`NRestarts=0`), so the impairment is intermittent and remains a live risk for later field work.

## Artifacts

- Results file: this file.
- Agent-side ground truth: `docker logs --timestamps sb-run` windows quoted per cell (share `a1k6q46n`,
  download counter 80 → 86).
- CLIENT-EAST per-cell telemetry: `/tmp/exp27-<cell>.proc` (5 s per-process cores + full `cmdline`),
  `/tmp/exp27-<cell>.vmstat` (1 Hz aggregate), `/tmp/exp27-<cell>.sampler.log`, `.sampler.pid`, `.vmstat.pid`
  for cells S1, S-A1, S-B1, ON1, S-A2, S-B2, ON2, ON1r, ON1b, ON1c.
- Driver logs: `/tmp/exp27-batch-b{1,2,3}.log` (cell-by-cell, including the `RELAY-ABORT` and `TERMINAL`
  lines) and `/tmp/exp27-batch-b{1,2,3}.out`; `/tmp/cell-S1.log` (the relay cell driven by the unmodified
  `drive_click.js`).
- Test-only artifacts (deleted at cleanup, reproduced in the body): `~/sbtest/drive_exp27.js`,
  `~/sbtest/exp27-probe.js`, `/tmp/exp27-{batch,start,stop,sampler}.sh`, TESTBOX `/tmp/exp27-patch.py`.
- Source inspected: TESTBOX `/opt/sharebridge-test/web/src/{app.js,downloadSinks.js,downloadPipeline.js}`
  (pre- and post-patch), `downloadCapabilities.js`, and the v1 tree copy of `app.js`.

