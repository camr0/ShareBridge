# Experiment 28 — is the v1 client's residual ~66 % cost the browser's DataChannel receive or the app's `onmessage` JS? (bare receive path) — 2026-09-18

**Verdict: BOTH, and the split is ~2:1 — the browser's own DataChannel receive is ~68 % of E27's remaining
"receive path" bucket but the app's own `onmessage`/chunk-assembly JS is the other ~32 %, and it is worth
`1.47×` at 1 vCPU. The bare receiver does NOT reach 200+: it lands at **123.68 Mbps unpinned (n=2)** — i.e.
the same ~110–120 plateau E27 already measured, because at 4 vCPU the **v1 WebRTC sender/transport** binds
(E21's 100.64–125.69 plateau), not the client — and at **82.99 Mbps pinned to 1 vCPU (n=2)** versus E27's
sink-free **56.35** and E25's real-client **37.19**. So "v1 fixable in app code" is **half right**: at 1 vCPU
app-code changes buy **2.23×** (37.19 → 82.99) and there is real money there, but the residual **~83 Mbps**
floor is the browser's own receive path on one core — so the app cannot reach 200 and cannot lift the 4-vCPU
plateau, and **v2's HTTP path is still the architectural answer**. Deciding number: **82.99 Mbps at 1 vCPU**,
not 200+. The renderer costs **0.63 cores at 123.68 Mbps unpinned** and **0.35 cores at 82.99 Mbps pinned**,
i.e. never the "~0.3 cores at 200+" shape that would have said "the JS was the whole cost". Unpinned the app
JS is worth only **1.035×** (123.68 vs 119.45) because the client is not the constraint there.

**Setup:** Field rig. Agent = VERSA container `sb-run` ONLY (`sb-agent:pristine`, host networking,
`UI_PORT=7879`, `UI_ADDR=127.0.0.1`, `SB_SCTP_CA_STEP=32768`, no `SB_SCTP_MIN_CWND`, no `--cpus`;
`docker inspect`: `NanoCpus=0`, `StartedAt 2026-09-18T22:39:06Z` — **not recreated by this experiment**).
Signalling = TESTBOX v1 (Caddy TLS) + **test-only** web root `/opt/sharebridge-test/web/`.
Client = CLIENT-EAST (4 vCPU, b2-15, ~12 ms path). Payload = one 754 MiB file = 790,626,304 B =
6,325.01 Mb per download (share `a1k6q46n`, `relay_only=false`, downloads 86/500 at start,
expires 2026-09-20T04:49:25Z). Metric: agent-side `docker logs --timestamps sb-run`, window = first
`DataChannel lanes ready` → last `download complete`; Mbps = 6,325.01 ÷ window_s.

## Method — the bare receiver

One temporary patch to **TESTBOX's test copy only** of `/opt/sharebridge-test/web/src/app.js`,
activated by the explicit test query parameter `?bare=1`. `BARE_RECEIVE_MODE` does exactly three
things (full diff reproduced in §Patch below):

1. flags the mode and exposes `globalThis.__bare = { bytes, frames, header, startedAt, endedAt }`;
2. replaces the **bulk-lane** `onmessage` with `bareReceive.bytes += event.data.byteLength;
   bareReceive.frames += 1` — no `decodeBinaryEnvelope`, no `routeOrBufferFrame`, no chunk
   assembly, no early-frame buffer, no StreamSaver/service-worker writer, no SHA-1, no
   `concatBytes` copy, no growing buffer, no per-chunk DOM update;
3. early-returns from `handleControlMessage` for `file_header` / `chunk_end` so the app's download
   operation, `createDownloadPipeline`, and `buildDownloadSink` are never created.

**Everything else is the real client, unchanged:** the signalling socket, join/knock, offer/answer,
ICE/DTLS, the three-lane DataChannel configuration (`control`/`media`/`bulk`), the `file_list` →
`renderFileList` → click → `file_request` control exchange, the relay-standby path, the
`Connected (Direct)` badge, and the control lane's real `onmessage`. The control lane still carries
the file header and the chunk_end that frame the transfer.

**Byte-count gate (an invalid arm is not a fast arm).** The client counts *wire* bytes (the DataChannel
message byteLength, which is the 14-byte `CHUNK_ENVELOPE_BYTES` envelope plus payload). The driver
asserts `wire - 14 × frames == 790,626,304` exactly, so a bare receiver only counts as valid if it
accounted for every byte of the 754 MiB file. `BARE-ACCOUNTING … match=true` in the driver log is the
gate.

**Mode assertion (non-negotiable).** Every cell must show **`DataChannel lanes ready`** *and* an
**unused relay standby** (relay connected at join then closed by the relay with
`pending wait window exceeded`, no relay-carried data), plus a `● Connected (Direct)` badge in the
driver log read via the DOM **before the click** (`#connection-type`); the driver aborts on a Relay
badge with `RELAY-ABORT`. Relayed cells read ~44.7–100 Mbps and would masquerade as valid slow
measurements.

**Client CPU pinning.** The driver is launched under `taskset`, so Chromium and all descendants
inherit the mask. Arm A = unpinned (4 vCPU), arm B = `taskset -c 0` (1 vCPU) — directly comparable
with E27's unpinned sink-free 119.45 (n=2) / sink-ON control 108.76, and E27's pinned sink-free 56.35
(n=2) / E25's pinned sink-ON 37.19 (n=2).

**Per-process client CPU.** CLIENT-EAST `/tmp/exp28-<cell>.proc`: a 5 s interval sampler reading
`/proc/<pid>/stat` utime+stime deltas per PID (so the number is *interval* CPU, not `ps`'s lifetime
average), classifying each PID by role from its full `cmdline` (`renderer`, `net` =
`utility-sub-type=network.mojom.NetworkService`, `gpu`, `chrome-main`, `node`, `xvfb`).
`/tmp/exp28-<cell>.vmstat` is a 1 Hz aggregate stream.

## Part 0 — pre-change state (23:53–23:56Z)

- Rig: `sb-run` present, `sb-agent:pristine`, `NanoCpus=0`, `UI_PORT=7879`, `UI_ADDR=127.0.0.1`,
  `SB_SCTP_CA_STEP=32768`, no `SB_SCTP_MIN_CWND`; agent API `127.0.0.1:7879/api/v1/shares` = **200**.
- TESTBOX: `caddy` **active**, `sharebridge-test` **active**, `NRestarts=0` for both, local
  `http://127.0.0.1:8080/` = **200**, listening on `:80/:443/:8080`.
- Test TLS front (E25's `alert 80`): from CLIENT-EAST `curl https://TESTBOX/` = **200 ×5/5**;
  from VERSA it failed **3/3** with curl exit 0-byte responses (the intermittent front impairment is
  still live, but it does not affect the client origin used here). Recorded, not engineered around.
- CLIENT-EAST: `node`/`chrome`/`Xvfb`/`vmstat` counts **all 0** before the run.
- `/opt/sharebridge-test/web/src/app.js` = **SHA-256 `407ee7032f90e4298ee6b0416ed69dbf94147d0c6ff0933e7c6803f129bdf3c1`, 91,308 B, root:root 644** — byte-identical to the E15/E25/E27 restored state and to
  the repo copy at `.worktrees/benchdirect/signaling-server/web/src/app.js`; no `EXP` markers, no
  leftover backups. The web root was **pristine** at start.
- Post-patch (23:55Z): `0181c318bba853860f598a26010c7d889bb32b29cbf72423e8badef522b12673`, 93,008 B,
  root:root 644; `EXP28` markers present (4); a fresh HTTP retrieval of `/src/app.js` from
  `http://127.0.0.1:8080` returns the **same** `0181c318…`; the local patched copy and the served copy
  hash-identical. Backup `/tmp/exp28-app.js.orig` holds the original (`407ee703…`, 91,308 B, 644).

## Raw cells

All cells CLIENT-EAST, one at a time, agent-side window (`DataChannel lanes ready` → `download complete`),
6,325.01 Mb per download. `pin` = the `taskset` mask the driver (and therefore all of Chromium) ran under.

### Arm A — bare receive path, unpinned (4 vCPU) — n=2, 23:56–23:58Z

| # | cell | pin | lanes ready → download complete (UTC) | agent window s | **agent-side Mbps** | client window s | client-side Mbps | bytes accounted | direct? |
|---|---|---|---|---:|---:|---:|---:|---|---|
| 1 | **B-A1** | none (4 vCPU) | 23:56:00.774429815 → 23:56:52.495650471 | 51.721 | **122.29** | 52.23 | 121.09 | **790,626,304 / 790,626,304 (exact, 48,263 frames)** | yes |
| 2 | **B-A2** | none (4 vCPU) | 23:57:40.292755134 → 23:58:30.863114439 | 50.571 | **125.07** | 52.22 | 121.12 | **790,626,304 / 790,626,304 (exact, 48,261 frames)** | yes |

**Arm A mean = 123.68 Mbps (n=2; 122.29, 125.07), spread 2.3 %.**

Mode assertion held on both cells (worked example B-A1): agent log shows `relay_prepare` / `relaychannel:
connected to relay` / `sent hello with JWT` at **23:56:00.04–00.20** (the standby relay channel is set up at
join, before the direct peer), then **`DataChannel lanes ready for peer 60d78a4c…` at 23:56:00.774**, then
`download complete (count: 87)` at 23:56:52.495, then `peer … closed` at 23:57:05; the relay standby is
never used and is torn down at **23:56:45.214** with `reason = "relay: pending wait window exceeded"` —
**no relay-carried bytes**. The driver independently read `#connection-type` = `● Connected (Direct)`
*before* the click (B-A1 23:56:00.952, B-A2 23:57:40.402) and never saw a Relay badge. **0 relay cells
discarded in arm A (0 of 2 attempts).**

**Byte-accounting gate passed on both cells.** `BARE-ACCOUNTING B-A1 payload=790626304 frames=48263
wire=791301986 expected=790626304 match=true` and `B-A2 payload=790626304 frames=48261 wire=791301958
match=true`. Wire = payload + 14 × frames in both (the 14-byte `CHUNK_ENVELOPE_BYTES`), so every byte of
the 754 MiB file was received and counted; the arms are valid, not merely fast.

### Arm B — bare receive path, pinned to 1 vCPU (`taskset -c 0`) — n=2 valid of 5 attempts, 23:58–00:04Z

| # | cell | pin | attempt | lanes ready → download complete (UTC) | agent window s | **agent-side Mbps** | client window s | client-side Mbps | bytes accounted | direct? |
|---|---|---|---|---:|---:|---:|---:|---|---|
| 3 | **B-B1** | `-c 0` (1 vCPU) | 1st pinned | 23:58:55.606939476 → 00:00:14.910741261 | 79.304 | **79.76** | 80.49 | 78.58 | **790,626,304 / 790,626,304 (exact, 48,260 frames)** | yes |
| — | B2 | `-c 0` | 2nd pinned | *(no lanes ready)* | — | — | — | — | — | **no — relay** |
| — | B2r | `-c 0` | 3rd pinned | *(no lanes ready)* | — | — | — | — | — | **no — relay** |
| — | B2s | `-c 0` | 4th pinned | *(no lanes ready)* | — | — | — | — | — | **no — relay** |
| 4 | **B-B2t** | `-c 0` (1 vCPU) | 5th pinned | 00:03:33.621065765 → 00:04:46.984144870 | 73.363 | **86.22** | 74.40 | 85.01 | **790,626,304 / 790,626,304 (exact, 48,258 frames)** | yes |

**Arm B mean = 82.99 Mbps (n=2; 79.76, 86.22), spread 7.8 %.**

Mode assertion held on both valid cells (worked example B-B1): relay standby at join
(`relay_prepare received` 23:58:54.753, `relaychannel: connected` 23:58:54.916, `sent hello with JWT`
23:58:54.916), then **`DataChannel lanes ready for peer 61a712e8…` 23:58:55.607**, `download complete
(count: 89)` 00:00:14.911, `peer … closed` 00:00:28.442; the standby is never used and is closed at
**23:59:39.929** with `pending wait window exceeded` — no relay bytes. B-B2t identical in shape
(standby 00:03:32.939, **lanes ready 00:03:33.621**, standby closed unused 00:04:18.070, `download complete
(count: 90)` 00:04:46.984). Both read `● Connected (Direct)` from the DOM before the click
(B-B1 23:58:55.808, B-B2t 00:03:33.998).

**Byte-accounting gate passed on both cells** (`BARE-ACCOUNTING B-B1 … match=true`, `B-B2t … match=true`).

### Relay cells discarded: **3 of 5 pinned attempts (60 %), 0 of 2 unpinned**

The 1-vCPU relay fallback E27 discovered reproduced **exactly**, and with the same mechanical shape:
`browser joined` → **+10.0 s** → `peer closed` → `relay channel started successfully`, **no `DataChannel
lanes ready` ever logged**.

| cell | pinned attempt | browser joined (UTC) | peer closed (UTC) | delta | relay started | lanes ready | driver |
|---|---|---|---|---:|---|---|---|
| B2 | 2nd | 00:00:38.970 | 00:00:49.009 | **+10.04 s** | 00:00:49.048 | **none** | `RELAY-ABORT badge="● Connected (Relay)"` |
| B2r | 3rd | 00:02:05.439 | 00:02:15.475 | **+10.04 s** | 00:02:15.509 | **none** | `RELAY-ABORT badge="● Connected (Relay)"` |
| B2s | 4th | 00:02:53.002 | 00:03:03.028 | **+10.03 s** | 00:03:03.069 | **none** | `RELAY-ABORT badge="● Connected (Relay)"` |

All three were aborted with nothing measured (no agent-side direct window exists) and are retained in the
record, never tidied away. Combined with E27's 4/8, the pinned-relay hazard is now **7 of 13 (54 %)**
pinned attempts across two sessions versus **0 of 6** unpinned — a *pinned-client-specific* silent
protocol downgrade, and both the DOM badge and an agent-side lanes-ready check are required to catch it.
One timing detail differs from E27's description: the relay channel itself is established ~0.04 s after the
direct peer closes, so the **+10.0 s is the *direct* deadline, not relay setup** — and because that deadline
gates the page's own `file-list` render, the driver's `joined` line in a relay cell lands at the same
instant as `RELAY-ABORT` (B2r: `joined` 00:02:15.575, abort 00:02:15.589, agent `peer closed`
00:02:15.475), i.e. a driver that gates on the badge *after* clicking would be ~10 s too late. Our gate runs
before the click and still catches all three.

## Interpretation

### The answer to the question: at 1 vCPU it is ~2/3 browser, ~1/3 app JS — and the app JS is worth 1.47×

| pin | arm | Mbps (agent) | n | vs real client (sink ON) | vs sink-free (discard sink) | client cores used (vmstat, window) | client per-process CPU |
|---|---|---:|---:|---:|---:|---:|---|
| **1 vCPU** | **bare (E28)** | **82.99** | 2 | **2.23×** (37.19, E25) | **1.47×** (56.35, E27) | **0.784–0.854** | renderer 0.337/0.357, net 0.288/0.307, total ≈0.63–0.68 |
| 1 vCPU | sink-free (E27) | 56.35 | 2 | 1.52× | — | 1.004–1.05 (**pin saturated**) | renderer 0.27–0.47, net 0.18–0.31, total 0.49–0.83 |
| 1 vCPU | real sink ON (E25) | 37.19 | 2 | 1.00 | — | ≈1.0+ (pin saturated) | — |
| **4 vCPU** | **bare (E28)** | **123.68** | 2 | **1.137×** (108.76) | **1.035×** (119.45) | **1.274–1.282** | renderer 0.615/0.648, net 0.478/0.463 |
| 4 vCPU | sink-free (E27) | 119.45 | 2 | 1.098× | — | 1.46–1.55 | renderer 0.66–0.86, net 0.40–0.52 |
| 4 vCPU | real sink ON (E27 ON2 / E25) | 108.76 / 107.84 | 1 / 2 | 1.00 | — | 2.61–2.78 | renderer **1.95–2.01**, net 0.38–0.51 |

**The brief's decision rule, applied to the discriminating (pinned) arm.** `56.35 ÷ 82.99 = 0.679`: of E27's
"receive path" bucket at 1 vCPU, **≈68 % is the browser's own DataChannel receive path (DTLS/SCTP in the
network service + message delivery into the renderer with a no-op handler) and ≈32 % is the app's own
JavaScript** (per-message `decodeBinaryEnvelope` + its `slice()` copy, the `bulkChain` promise chain,
`routeOrBufferFrame`/`append`, `refreshCompletionTimeout`, and per-chunk DOM/`setTimeout` bookkeeping).
The unpinned arm gives the same answer from the other side: removing *all* per-frame app work at 4 vCPU buys
only **3.5 %** (119.45 → 123.68), because there the client has cores to spare.

**So E27's "66 % receive path" was a two-part bucket, and the split is roughly 2:1 browser:app-JS.**
Direct app-code consequences for v1:

- **It is worth doing.** A bare-equivalent receive path (no envelope `slice()` copy, no promise chain per
  message, no per-chunk DOM/`setTimeout`, control lane only for framing) is a **1.47×** win on a 1-vCPU
  client and **2.23×** over the current real client there — on the client shape E25/E27 identify as the
  binding constraint (~35 Mbps/core).
- **It cannot reach relay class.** Even with *zero* app receive code the 1-vCPU client stops at **82.99**, and
  the 4-vCPU client stops at **123.68** — both far from 200+ and both essentially what E27 already measured
  without the sink. The 4-vCPU ceiling is not the client at all: **123.68 is inside E21's sender plateau
  (100.64–125.69, mean 113.85)**, so the client is *not* binding at 4 vCPU and no client-side change can move
  it. That is the same conclusion E10c drew from the other direction (239 Mbps over the v2 HTTP/TLS path on
  this same VM).

### Renderer cores: neither arm has the "JS was the cost" shape

The brief's own diagnostic was "~0.3 cores at 200+ Mbps ⇒ the JS was the cost; ~0.8 cores at ~120 ⇒ the
browser is". Measured: **0.63 renderer cores at 123.68 Mbps unpinned** and **0.35 renderer cores at 82.99 Mbps
pinned** — the pinned renderer is *below* 0.3–0.4 **and** the throughput is only 83, i.e. the pinned bare
client is **not** CPU-starved (unlike every E25/E27 pinned arm, which saturated the pin at 1.004–1.05 cores).
The single permitted core is still the binding resource for a 1-vCPU client, but the app JS's removal
relieves the *saturation*, which is why the gain (1.47×) is larger than the CPU it frees (≈0.2 core).
A bare receiver is also a genuinely cheap one: the agent delivers each 754 MiB file as **48,258–48,263
DataChannel messages** (~16 KiB each), all of which this arm counts and drops.

### What this means for the v1 vs v2 decision

1. **v1 is not liftable to relay class in app code.** Best case measured with the app's receive path stripped
   to a counter: 82.99 Mbps on 1 vCPU, 123.68 on 4 vCPU (sender-bound). The remaining 68 % of the receive
   bucket is browser-internal and would need a fork (or a different transport) — i.e. **v2's HTTP path is the
   architectural answer**, exactly as E10c/E27 argued, now with the app-JS share removed from the question.
2. **But there is a cheap, protocol-independent ~1.5–2.2× v1 win** for CPU-starved clients: drop the
   per-message envelope `slice()` copy, drop the per-message promise chain, batch/coalesce DOM progress
   updates, and stop arming a timer per chunk. That is a strictly smaller change than a fork and it makes the
   ~35 Mbps/core client into an ~83 Mbps/core client.
3. **The pinned-relay hazard is worse than E27 reported in aggregate** (7/13 pinned attempts across the two
   sessions) and is silent — it must be gated in any future one-core-client measurement.

## Caveats

1. **n=2 per arm, and arm B's n=2 came from 5 attempts** (3 discarded to relay). The two valid pinned cells
   differ by 7.8 % (79.76, 86.22) — larger than arm A's 2.3 % (122.29, 125.07) — so the pinned mean 82.99
   carries roughly ±4 %.
2. **The pinned comparators are cross-session.** E27's sink-free 56.35 (23:36–23:41Z) and E25's sink-ON
   37.19 (23:02–23:12Z) were not re-measured here; three attempts at a fresh pinned SINK-ON control in E27
   all fell to relay. The justification is the same-session unpinned anchor: our bare unpinned mean
   (123.68) sits **3.5 % above** E27's unpinned sink-free (119.45), and E27 had already shown the session
   stable to 0.9 % (ON2 108.76 vs E25 107.84). No session-level 1.4× speedup exists that could explain the
   pinned delta away.
3. **The 1.47× pinned gain is not a like-for-like per-core efficiency number.** The bare arm is *not*
   pin-saturated (0.784–0.854 cores busy) while E27's sink-free pinned arms *were* (1.004–1.05). So part of
   the gain is relief from core saturation (scheduling/queueing) rather than the app JS's own CPU. The
   per-process profile is in fact nearly unchanged: renderer ≈0.34 both, network service ≈0.29–0.31 vs
   0.18–0.31. **The conservative reading is the ratio `56.35/82.99 = 0.68`, not the cores arithmetic** — and
   the mechanism by which ~0.2 core of app JS costs 1.47× in throughput at a pinned client is **not
   established here** (the plausible candidates are per-message microtask/allocation pressure and
   single-core scheduling latency).
4. **The bare arm is a decisive ablation, not a proposed implementation.** It removes the app's *entire*
   receive pipeline — including the parts a real download must keep (chunk assembly into a sink, progress
   UI). It bounds the app-JS share from above; a shippable optimisation would recover some fraction of the
   1.47×, not all of it.
5. **The 4-vCPU arm cannot answer the client-cost question at all** — it is at/above E21's v1 sender plateau
   (123.68 vs plateau range 100.64–125.69), so it is reported as a bound, not as a client measurement. The
   brief's "~200+ ⇒ app JS is the cost" threshold is unreachable on this path for that reason: v1's WebRTC
   *sender* caps near 120 Mbps at 4 vCPU regardless of the client.
6. **Arm A's and arm B's per-cell vmstat windows were reconstructed from deterministic offsets** (vmstat
   started ~0.2 s after the printed `T0`, 1 Hz rows) because the samplers were not stopped between cells, so
   each `/tmp/exp28-*.proc`/`.vmstat` file also contains later cells' rows. Only rows inside the cell's own
   transfer window (the timestamps quoted in the tables) were used for the per-role means; the vmstat row
   ranges are stated in the artifacts.
7. **The client-side byte counters are wire bytes, not payload bytes.** Validation is
   `wire − 14 × frames == 790,626,304` exactly, which is only satisfied if every byte of the file arrived;
   the frame count itself is not independently confirmed against the agent's chunker (the agent does not log
   a frame count). All four cells pass.
8. **Two relay-fallback shapes exist.** E27 saw `join → +10.0 s → closed → relay`; we see the same +10.0 s
   direct deadline but the relay channel is then established in ~0.04 s and the DOM badge flips essentially
   at join, so a driver that only polls the badge slowly can miss the distinction between "relay" and
   "direct that has not finished connecting". Our driver polls every 1 s and gates *before* the click.
9. **CLIENT-EAST only (~12 ms path).** E25 says CLIENT-WEST's ~68 Mbps cap is plausibly RTT/ACK-limited
   rather than CPU-limited, so nothing here transfers to the 71 ms path.
10. Every cell emitted the pre-existing, harmless app error
    `join() re-entered while browser signaling socket is still active (readyState=1)` at page load; all four
    valid cells joined, read the Direct badge and completed anyway.
11. The TESTBOX TLS front's E25 `alert 80` impairment was **partially reproducing** at start: `curl
    https://TESTBOX/` from CLIENT-EAST returned **200 ×5/5** but from VERSA **000 ×3/3** (curl transport
    failure, no alert-80 text captured). All cells ran over the plain HTTPS origin from CLIENT-EAST, which
    was healthy, so the cells are unaffected; the impairment is still live and unrepaired.

## Restoration

**Verified complete.** TESTBOX test web root restored from the pre-change copy at 00:06Z and checked five
ways: on-host `cmp` against the backup = **identical**; `sha256sum` =
**`407ee7032f90e4298ee6b0416ed69dbf94147d0c6ff0933e7c6803f129bdf3c1`** (the exact pre-change value) at
**91,308 bytes, mode 644, root:root**; `EXP28` marker count **0**; a fresh HTTP retrieval of `/src/app.js`
from `http://127.0.0.1:8080` returning the **same** `407ee703…`; and the pre-change served hash having been
byte-identical to the repo copy at `.worktrees/benchdirect/signaling-server/web/src/app.js` before the patch.
The temporary backup, patched copy and patch script were deleted (`/tmp/exp28-app.js*` absent). TESTBOX
`caddy` and `sharebridge-test` both **active**, local HTTP **200**. The patching was applied **only** to
TESTBOX's test copy under `/opt/sharebridge-test/web/`; **no repo file, no production web root, and no
signalling binary was changed.**

CLIENT-EAST: `node`/`chrome`/`Xvfb`/`vmstat` counts **all 0** after cleanup, and **0** `exp28-sampler`
processes remain (verified by count, not by pattern match). The pre-existing `~/sbtest` driver set is
untouched; new files left behind are the test-only driver `~/sbtest/drive_bare.js` and the sampler
`/tmp/exp28-sampler.py`, both documented in Artifacts. CLIENT-WEST was **never driven**, never unshelved
and is 0/0/0.

VERSA: `sb-run` = **`sb-agent:pristine`**, **`NanoCpus=0`**, `Restart=unless-stopped`,
**`StartedAt 2026-09-18T22:39:06.081756383Z` — unchanged, so the container was never recreated by this
experiment**; env `UI_PORT=7879`, `UI_ADDR=127.0.0.1`, `SB_SCTP_CA_STEP=32768`, **no** `SB_SCTP_MIN_CWND`
(0 matching env lines); API `127.0.0.1:7879/api/v1/shares` = **200**; container idle at **1.24 %** CPU.
Share `a1k6q46n` unchanged (`relay_only=false`, downloads 86 → 90 across four completed transfers).
Production `sharebridge-agent` (UI 7878), the v2 stack, `sharebridge-agent-test`, `production
sharebridge.app`, DNS/ACME, all non-test Immich and other containers, and all instance state were **never
touched**; VERSA was read-only apart from `docker logs/inspect/stats` on `sb-run`; **no `git` command was
run.**

## Artifacts

- Results file: this file (`.worktrees/benchdirect/docs/superpowers/spikes/results/`).
- Agent-side ground truth: `docker logs --timestamps sb-run` windows quoted per cell (share `a1k6q46n`,
  download counter 86 → 90).
- CLIENT-EAST per-cell telemetry (still on the VM): `/tmp/exp28-{A1,A2,B1,B2,B2r,B2s,B2t}.log` (driver +
  page console), `/tmp/exp28-<cell>.proc` (5 s per-role CPU), `/tmp/exp28-<cell>.vmstat` (1 Hz aggregate).
  **Caveat:** the samplers were not stopped between cells, so each `.proc`/`.vmstat` file also contains
  later cells' rows; the per-role means in this file were computed only from rows inside that cell's own
  transfer window. vmstat row ranges used (after the 2 header lines): A1 4–56, A2 5–55, B1 6–85, B2t 8–82.
- Client driver / sampler left on CLIENT-EAST as test-only tooling: `~/sbtest/drive_bare.js`,
  `/tmp/exp28-sampler.py` (new files; the pre-existing `~/sbtest` driver set is untouched).
- TESTBOX: the patch script, the patched copy and the backup were **deleted** at restoration
  (`/tmp/exp28-patch.py`, `/tmp/exp28-app.js`, `/tmp/exp28-app.js.orig` all absent); the patch is reproduced
  verbatim above. Hashes: pre-change and restored `407ee7032f90e4298ee6b0416ed69dbf94147d0c6ff0933e7c6803f129bdf3c1`
  (91,308 B), patched `0181c318bba853860f598a26010c7d889bb32b29cbf72423e8badef522b12673` (93,008 B).
- Source inspected (read-only): TESTBOX `/opt/sharebridge-test/web/src/{app.js,binaryEnvelope.js}` (pre- and
  post-patch) and the repo copy `.worktrees/benchdirect/signaling-server/web/src/app.js` (hash-identical to
  the pre-change served file); `agent/internal/daemon/daemon.go:765/856` to confirm the agent-side window
  metric is sender-side and independent of the client's receive handling.

## Patch (verbatim, applied to the TESTBOX test copy only)

```diff
@@ module state
+const BARE_RECEIVE_MODE = typeof location !== 'undefined' &&
+  new URLSearchParams(location.search).has('bare')
+const bareReceive = { bytes: 0, frames: 0, header: null, startedAt: 0, endedAt: 0 }
+if (BARE_RECEIVE_MODE) { globalThis.__bare = bareReceive }

@@ attachTransferChannel
-  channel.bulk.onmessage = (event) => {
-    if (!isCurrentSession()) return
-    bulkChain = bulkChain.then(() => isCurrentSession() && handleBulkMessage(event, { isCurrentSession })).catch(handleLaneError)
-  }
+  if (BARE_RECEIVE_MODE) {
+    channel.bulk.onmessage = (event) => {
+      const data = event.data
+      const len = typeof data?.byteLength === 'number' ? data.byteLength
+        : (typeof data?.length === 'number' ? data.length : 0)
+      bareReceive.bytes += len
+      bareReceive.frames += 1
+      const now = Date.now()
+      if (bareReceive.startedAt === 0) bareReceive.startedAt = now
+      bareReceive.endedAt = now
+    }
+  } else {
+    channel.bulk.onmessage = (event) => {
+      if (!isCurrentSession()) return
+      bulkChain = bulkChain.then(() => isCurrentSession() && handleBulkMessage(event, { isCurrentSession })).catch(handleLaneError)
+    }
+  }

@@ handleControlMessage
   const msg = JSON.parse(event.data)
+  if (BARE_RECEIVE_MODE && (msg.type === 'file_header' || msg.type === 'chunk_end')) {
+    if (msg.type === 'file_header') {
+      bareReceive.header = { name: msg.name, size: msg.size, estimatedSize: msg.estimated_size }
+    }
+    debugLog('EXP28 bare receive: framing control message', { type: msg.type, operation_id: msg.operation_id })
+    return
+  }
   switch (msg.type) {
```
