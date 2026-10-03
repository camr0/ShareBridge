# v1-direct falsification pass — campaign log (2026-10-02)

**Orchestrator:** pi session in `/Users/ali/Git/ShareBridge`. **Status:** RUNNING.
Companion to `2026-10-02-v1-direct-candidate-experiments-review.md` (the plan). This file is the
orchestrator's log; each experiment also has its own results file.

**Scope:** the review's Tier-1 falsification pass — probes that could still overturn the E1–E32 verdict —
plus two cheap mechanisms the review ranked. Nothing here is a result until its own results file says so.

---

## 1. Infra bring-up (completed 19:16–19:22Z)

All three OVH instances were `SHELVED_OFFLOADED`. Unshelved (operator authorised the pass; the runbook's
"no shelve/stop/resize" rule concerns teardown, which stays with the operator):

| host | role | state |
|---|---|---|
| `TESTBOX` | signalling + TLS | ACTIVE, swapped to the **v1** rig |
| `CLIENT-EAST` | ~12 ms client | ACTIVE, idle |
| `CLIENT-WEST` | ~70 ms client | ACTIVE, idle |

- **TESTBOX swap:** it was found in the **v2 stack** state (`sharebridge`, `sharebridge-relay-gateway`,
  `sharebridge-relay-frps` active) — a peer left it there. Per the documented procedure, stopped the three
  v2 units and started `caddy` + `sharebridge-test`. Verified: `:443`/`:80` owned by `caddy`, `:8080` by the
  v1 `server`, public origin **200**, `/src/app.js` **200**. The v2 units remain `enabled` but stopped.
- **VERSA:** started the experiment agent `sb-run` (`sb-agent:pristine`, `UI_PORT=7879`,
  `SB_SCTP_CA_STEP=32768`, no min-cwnd). Verified `agent authenticated`. **Production `sharebridge-agent`
  (7878) and all non-ShareBridge containers untouched.**
- **Shares:** the campaign's share (`i4b81ri4`, 48 h from 09-16) had **expired**, and the agent had **0
  shares**. Verified the OpenCloud source is still live via PROPFIND — it serves
  `debian-13.4.0-amd64-netinst.iso`, **790,626,304 B = 754 MiB**, byte-identical to the campaign payload —
  then created a fresh **direct** share (48 h, 500 downloads) and a **relay** share. Codes are deliberately
  not recorded here (runbook rule 5); they live in the live rig only.
- A peer heads-up was sent to the other `ShareBridge` sessions before the swap. The v2 stack will be
  **restored at the end** (§4).

## 2. Rig validation — baseline cell (CLIENT-EAST, ~12 ms)

| cell | mode gate | window | Mbps | MB/s |
|---|---|---|---|---|
| baseline, direct, CA-step on, n=1 | `DataChannel lanes ready`, no relay handshake | **51.014 s** (19:22:09.579 → 19:23:00.593) | **124.0** | 15.5 |

Consistent with the campaign's fixed-client EAST range (E30 119.17 n=4; E32 137.71 n=2; E31 149.09 n=1).
**The rig is healthy.** Metric is agent-side only (`lanes ready` → `download complete`), 6,325.010432 Mb ÷ window.

## 3. Workstreams dispatched (background agents)

| # | experiment | env | agent | results file |
|---|---|---|---|---|
| E33 | **A5** min-cwnd sweep at **70 ms** + **D2** 250 ms series | field (`CLIENT-WEST`) | `field-mincwnd` | `2026-10-02-exp33-mincwnd-70ms.md` |
| E34 | **N1** DataChannel message granularity | lab (local) | `lab-message-granularity` | `2026-10-02-exp34-message-granularity.md` |
| E35 | **D1** pion in-flight instrumentation (build) | local Go | `build-instrumentation` | `2026-10-02-exp35-instrumentation-build.md` |

**Why A5 is the sharpest cheap probe:** F8's "min-cwnd has no field value at any size" closure rests on
**E17/E18**, and E18's own results file says *"CLIENT-WEST (71 ms, BDP ≈ 1 MB) not swept"*. The closure was
derived at a **12 ms** BDP (≈165 KiB); at 70 ms a 256–640 KiB floor sits **below 1× BDP**, and E32 showed
70 ms is a *distinct, window-shaped* regime. The doc banner for this was applied in `b7667d76`.

**Why D2 matters:** every campaign number is a window average. A **ramp** at 70 ms means slow-start/window
(improvable); a **flat plateau from second one** means a hard dispatch cap (not).

**Why N1:** the plateau carries 48,258 × 16 KiB `onmessage` events. If the serialization is **per-message**
rather than per-byte, larger DataChannel messages (SCTP re-fragments and reassembles before delivery, so
this is not the closed chunk-size lever) cut dispatch count 4–16×. The lab receiver is a bare byte counter,
so the lab can test it — the agent's first job is to check whether E6/E23 already answered it.

**Why D1:** the 70 ms penalty was inferred from rate + CPU alone. Without `cwnd`/`rwnd`/outstanding/retransmit
cause, A5 vs A1 vs A2/N5 cannot be discriminated.

## 4. Scope corrections and open items (for the operator)

- **E2 is NOT a measurement — it is a build.** The review said "tooling exists (benchdirect prodbench
  receiver)", but the benchdirect harness is a **loopback sender/receiver pair over an in-process shim**; it
  has no signalling, no share-join, no HMAC, no ICE against the real agent. A non-browser receiver on the
  **field** path requires a **native v1 client** (port the `app.js` join flow to Go). That is a multi-hour
  build, not a cell. **Deferred pending the operator's call.**
- **C1 (8–16 vCPU client) needs a resource decision.** The review itself flagged this ("testing it needs a
  resource decision, not another run"). `b2-30` (8 vCPU/30 GB) and `b2-60` (16 vCPU) **are** available in
  `US-EAST-VA-1`, so it is feasible — but it means provisioning (and later tearing down) a new instance.
  **Not started; awaiting the operator.**
  Note the tension that makes it worth doing: E30's pin gradient is **39.0 / 68.3 / 119.2 Mbps at 1 / 2 / 4
  vCPU** — still *rising* at 4 cores. "Not helped by more cores" is an inference from 0.98-core
  utilisation, not a measurement at 8 cores.
- **C2 (Firefox) is prep-only for now** — it needs no new infra, but its cells must not run concurrently
  with the field agent (runbook rule 11: one field experiment at a time).

## 5. Hard rules in force
Never touch production `sharebridge-agent` (7878) or non-ShareBridge containers · never the v2 relay stack
during the run · no `git` in shared worktrees · one field experiment at a time · never `pkill` blind · never
call Playwright's Download API · no IPs/hostnames/share codes in committed files · results written
incrementally after every cell.

## 6. Log
- 19:16Z — unshelve requested for all three instances.
- 19:19Z — TESTBOX swapped to v1; `sb-run` started and authenticated.
- 19:21Z — direct + relay shares created against the verified 754 MiB source.
- 19:22Z — baseline cell: **124.0 Mbps**, 51.014 s, direct asserted. Rig healthy.
- 19:2xZ — E33/E34/E35/E36 dispatched as background agents.
- 20:0xZ — **E35 (D1 instrumentation) COMPLETE.** Instrumented pion built: `forks/_trace/sctp_trace.go` (+test),
  one hook line in `createAssociationFromConfigWithTsn`, `apply_sctp_patch.sh trace` variant (composable:
  `rtomin+trace`), `Dockerfile.instrumented`. Emits ~1 Hz JSONL `cwnd`, `ssthresh`, `rwnd`, `peerRwnd`,
  `effectiveWindow=min(cwnd,rwnd)`, `outstandingBytes/Chunks`, `pendingBytes`, `srttMs`, cumulative counters,
  and `retransRTO/retransFast/retransTotal` (the RTO-vs-fast split came free from pion's existing
  `association_stats.go`). Gated on `SB_SCTP_TRACE`; off = one `os.Getenv`, no goroutine/timer/write.
  `go build`/`go vet` clean patched *and* unpatched; 4 fork tests pass incl. an inertness test (0 bytes,
  0 goroutines). Two semantics flagged by the builder: pion's `rwnd` is *remaining credit*, not the
  advertised window (hence `peerRwnd`/`effectiveWindow`); and `nFastRetrans` counts chunks retransmitted,
  not "fast recovery entered". **Not done:** per-event retransmit cause (needs a second insertion),
  browser-side SCTP state (unobservable), `docker build` unverified (local daemon down).
- 20:0xZ — **Methodology finding: the rig's client is the UNFIXED (production) client.** Verified by hash
  through the real origin: `src/app.js` = `407ee703…`, `src/downloadSinks.js` = `5c7ff322…`,
  `src/hashWorker.js` = **404** — i.e. exactly the pre-E29 state. The E29 fix was never landed and E32
  restored the web root byte-for-byte. Consequence: E33's controls (64.29 / 67.09 Mbps) are **correct** and
  match E32's **unfixed** WEST arm (64.84, n=2), *not* the 74.71 fixed-client figure. This is the
  deployment-relevant configuration, and it is the like-for-like comparison with E17/E18. E33 was steered
  accordingly; it must not "fix" the client mid-sweep.

## 7. Results as they land

### E34 — N1 (DataChannel message granularity): **REFUTED**, and it was already answered
Lab, 85 cells, 0 failures, `shim_write_err=0`, 100 % payload delivered in every cell. Full detail in
`results/2026-10-02-exp34-message-granularity.md`.

- The receiver's **marginal cost per DataChannel message is real**: **7.1 / 8.7 / 9.8 µs** of Chrome CPU at
  uncapped / 64 Mbps / 240 Mbps (in-block least-squares over a 1 KiB→256 KiB ladder at constant delivered
  rate).
- **But it cannot be the field's limiter.** At the field's own message rate (48,258 messages for 754 MiB =
  **943.6 msg/s** at the E28 bare arm's 123.68 Mbps) that term is **0.007–0.009 core = 0.5–0.7 %** of the
  1.27 client cores that arm spent.
- **The lever N1 proposes buys nothing:** 16 KiB→256 KiB (a 16× message-count cut) moves receiver CPU per
  byte by **−6.2 % / −0.1 % / −3.0 %** — at or below the same bench's in-block baseline spread
  (6.1–31.8 %).
- **Decisive:** the lab's *bare* Chrome receiver sustains **443 Mbps at 54,018 msg/s** — **57× the field's
  message rate** — and 526 Mbps at 3,812 msg/s (4.3× the field's throughput) on the same 16 KiB framing.
- **E23 §2 had already run this exact test** (sender CPU-s/GB flat within 4 %, receiver cores flat 0.26–0.30
  across 16/64/256 KiB). E6 could not have answered it — E6 measured no CPU. E6's 256 KiB "trap" did not
  reproduce (228/231/228 Mbps at the same cap).
- **Bonus lab/field discrepancy worth keeping:** at `--rtt 71` **all 27 cells collapse to 7–19 Mbps
  regardless of message size**, while the *field* at 70 ms delivers 64–75 Mbps. Another confirmation that
  the lab shim is not representative at high RTT.

**Consequence for the plan:** the review's "if you only had one thing" trio loses N1. Remaining Tier-1
probes: **D1** (instrumentation — built, image staged, measurement pending) and **E2** (built — see below).

### E37 — E2's native Go receiver: **BUILT and locally validated** (field run pending)
`agent/cmd/fieldrecv/` (~700 lines incl. tests), design note in `results/2026-10-02-exp37-native-receiver-build.md`.
`go build ./...`, `go vet ./...`, `gofmt` clean.

- It speaks the real client protocol: WS `/ws/client?session=<code>`, knock/nonce/**join with empty HMAC**,
  answers the agent's offer, accepts the lanes, then does the control-channel handshake
  (`transport_hello`/`transport_ready`, `file_request`) and counts **bulk payload bytes with the 14-byte
  envelope decode** — the E28 "bare" equivalent, no sink/hash/assembly.
- **Validated end-to-end 10/10, deterministically**, against a fake `/ws/client` signalling server driving the
  **real `peer.Peer` offerer** and the **production `transfer.NewManager`** over real pion DataChannels:
  `payload=2,109,497`, `frames=129`, `wire=2,111,303 == payload + 14×129`, `bad_frames=0`, `status=complete`.
  Envelope unit tests cover exact accounting and every reject path.
- **Live handshake deliberately skipped** — the builder checked and found `sb-run` mid-experiment, which is
  exactly right (one field experiment at a time).
- **Field command:** `./bin/fieldrecv --signal wss://<origin> --code <share> --duration 180s`; emits
  `PROGRESS`/`SUMMARY payload_bytes=… wire_bytes=… frames=… mbps=…`.
- **First-attempt risk (named by the builder):** `fieldrecv` is direct-WebRTC only, and E27/E28 saw the agent
  silently downgrade to the Noise relay on 54–60 % of *pinned* attempts — if that happens the receiver sees
  zero bulk frames and exits `status=deadline`. The harness must assert direct mode, as every field cell does.

**This makes E2 a measurement, not a build.** It is now the highest-value remaining cell: it is the only test
that can split "browser receive path" from "pion sender congestion control" as the cause of the 70 ms penalty.

### E33 — A5 (min-cwnd at 70 ms) + D2 (time series): **NEGATIVE**, and it corrects E18's BDP rule
CLIENT-WEST (~70 ms), v1 direct, CA-step on, **n=3 per floor + 4 interleaved controls**, all arms through the
deployed **unfixed** client (like-for-like with E17/E18). Full detail in `results/2026-10-02-exp33-mincwnd-70ms.md`.

| arm | mean Mbps | cells | vs control |
|---|---|---|---|
| control (unset) | **67.12** | 64.29 / 67.09 / 69.58 / 67.54 | — |
| 262144 (256 KiB) | 73.19 | 68.27 / 70.51 / **80.78** | no benefit — the 80.78 is a session outlier; the control run immediately after read 67.54 |
| 393216 (384 KiB) | **51.77** | 2 of 3 below the entire control range | unreliable |
| 524288 / 655360 | **~15** | 6/6 cells in a tight **14.7–16.8** band; 0/3 and 1/3 completions | **4.4× worse** |

- **The lever does not flip to helpful at high RTT.** Harmless ≤256 KiB, unstable at 384 KiB, destructive ≥512 KiB.
  A lever that is sometimes inert and sometimes 4.4× destructive is strictly worse than no lever.
- **Substantive correction to E18:** the safe ceiling moves **up in bytes** (128 KiB at 12 ms → 256 KiB at 70 ms)
  but **down as a fraction of BDP** (0.70× → **0.44×**). Harm begins **below 0.9× BDP** at 70 ms — *inside* what
  E18 would have called the harmless zone. **The harmless zone is not defined by BDP multiples**, so a floor
  chosen to sit "safely below 1× BDP" on a high-RTT path can still land in the destructive regime.
- **Mechanism:** the degraded arms sit at an effective in-flight volume of ~130–150 KB (15.5 Mbps × 70 ms / 8),
  far *below* the 512 KiB floor — so the connection is not window-limited by the floor at all. The oversized
  instantaneous burst overshoots the bottleneck and the resulting loss cycles pin throughput at ~15 Mbps.
  Empirical rule: **at ~70 ms, ≤256 KiB harmless, 384 KiB unstable, ≥512 KiB collapse.**
- **D2 explains the null:** there is **no ramp anywhere** — a flat plateau by t=5–15 s, slow start (~0.4 s)
  negligible. A cwnd floor can only help a path whose throughput is ramp-limited; this one is not. Degraded arms
  sit on a hard 16.2 Mbps plateau halving to 8.2 every 30–40 s with **RTT unchanged (69.7–75.0 ms)** — i.e.
  loss-driven collapse, not queueing.
- 3 cells discarded to relay fallback (zero bytes); rig restored to stock.

**Consequence:** A5 is closed at 70 ms as well as 12 ms. Combined with E34 (N1 refuted), the review's remaining
Tier-1 probes are **D1** (instrumentation — built, image staged) and **E2** (receiver built — field run in flight).

### E38 — E2 field: native Go/pion receiver vs browser. **THE CAMPAIGN'S CENTRAL CLAIM IS MATERIALLY CORRECTED.**
7/7 fieldrecv cells completed the full 790,626,304 B, `status=complete`, zero relay fallbacks. Full detail in
`results/2026-10-02-exp38-native-receiver-field.md`.

| cell | tool | Mbps | notes |
|---|---|---|---|
| WEST (~70.75 ms) | **fieldrecv** (stock) | **100.1 / 107.6 / 111.6** (mean **106.4**) | flat, no ramp; `bad=0` |
| WEST | **browser** | **64.54** | reproduces E32's unfixed 64.84 (n=2) |
| WEST | fieldrecv `--max-rx-buf 4 MiB` | peak **232.0**, net **84.85** | ramps 50→230 over ~5 s then 1–2 s at zero, repeating |
| EAST (~10.94 ms) | fieldrecv (stock) | 115.3 / 102.7 / 99.4 (mean **105.8**), **peaks 233–237** | sawtooth: 5–7 s period, bursts to ~234 then 1–3 s near-zero |
| EAST | **browser** | **140.56 / 137.50** (mean **139.0**) | n=2 |
| EAST | fieldrecv, client `rmem` 4 MiB | **138.98** | sawtooth strongly reduced |
| WEST | fieldrecv, client `rmem` 4 MiB | 111.45 | unchanged from stock ⇒ WEST is not socket-buffer-limited |

**What survives, what breaks:**
- **Refuted:** "the cap is the pion *sender's* congestion control." The native receiver lands at 106.4, not 64–75.
- **Survives:** the browser's high-RTT receive path is a **real ~45 % cost** (64.54 vs 106.4 at 70 ms).
- **WRONG:** "the v1-direct ceiling *is* the browser's DataChannel receive path." **At 11 ms the browser is not a
  cap at all** — a properly-buffered native receiver lands on the *same* number (138.98 vs 139.0). The campaign's
  ~120–139 Mbps near-field plateau is therefore **not browser-specific**.
- **New lever #1 — pion's default 1 MiB SCTP receive window** (`initialRecvBufSize`, pion/sctp v1.9.4
  `association.go:74`). A stock pion receiver is hard-capped at `rwnd/RTT` = 1 MiB / 70.75 ms = **118.5 predicted
  vs 114.95 measured (97 %)**. Lifting it to 4 MiB lets the same sender ramp to **232 Mbps = 94 % of the 245 Mbps
  path capacity** — but it then collapses cyclically, netting 84.9. **The 70 ms path sustains ~115, not 232.**
- **New lever #2 — the client's OS UDP receive buffer** (`net.core.rmem_default` = **208 KiB**). At low RTT the
  sender's bursts overflow it → SCTP loss → 1–3 s RTO stalls. At 4 MiB the native receiver goes 105.8 → **138.98**.
  This is a *tool* artifact for native receivers (Chrome sets its own socket buffers) — but it is a real,
  previously-untested OS-level knob, and it means **stock native-receiver numbers are a floor, not a ceiling**.
- **Corrected ordering at 70 ms:** path capacity **245** > pion sender + 4 MiB window, peak **232** (unstable,
  net 85) > stock pion receiver **115 ≈ rwnd/RTT** > deployed browser **64.5**.
- 2 of 4 CLIENT-EAST browser attempts (50 %) silently relayed (109 s ⇒ 58 Mbps) and were discarded — consistent
  with E27/E28's 54–60 %.

**Open question this raises (not answered):** what caps *sustained* 11 ms at ~139 for **both** the browser and the
native receiver, when the sender demonstrably bursts to **233–237** and the path carries 245? Candidates: residual
RTO sawtooth (the 4 MiB `rmem` only *reduced* it), the agent's uplink, or a client-side ceiling. **This is now the
sharpest follow-up**, because if the near-field plateau is not the browser, the campaign's headline needs rewriting.

*(appended as results land)*

### E36 — C2 (Firefox arm): prep **FAILED / probably invalid**
The prep agent died silently after ~4 minutes (no completion, no tool calls for 2.5 h; stopped by the orchestrator).
Its partial finding: on `CLIENT-EAST`, Playwright Firefox **registered the click but wrote no download artifact
anywhere** (no `/tmp/playwright-artifacts-*`, empty `~/Downloads`) — the Chromium click-only driver does not
transfer to Firefox as written. Combined with the project memory's own note that the app's save-picker ladder
treats **Firefox as "harmful"** (only Chrome/Edge 105+ have the save pickers), **C2 is probably not a valid
transport probe**: the client app's download path is unsupported there, so any rate measured would confound
transport with a broken sink. Not pursued. If C2 is wanted it needs a Firefox-specific sink path first.

### E39 — what caps sustained 11 ms at ~139? **RECEIVE-SIDE ARTIFACT.** The near-field headline needs rewriting.
Full detail in `results/2026-10-02-exp39-nearfield-cap.md`. Sysctls restored (212992/212992) and confirmed.

**In-session path controls (no transfer running):** TCP 1-flow **244.80**, TCP 4-flow **244.80**, UDP
300-offered → **247.34** Mbps; repeated at the end (244.79 / 247.27). **The path is ~245, not 139** — hypothesis
(c) refuted.

**Sustained vs peak by pion's advertised SCTP window** (client `rmem` 4 MiB unless noted):

| `--max-rx-buf` | sustained Mbps (cells) | mean | peaks | seconds <20 Mbps |
|---|---|---|---|---|
| 1 MiB (default) | 149.7 / 118.5 / 99.0 / 168.2, stock 106.0 | **128.3** (n=5) | 231–237 | 30 % |
| 8 MiB | 158.8 / 155.9 / 159.0 / 148.8 / 153.6 | **155.4** (n=5) | 232–235 | 10 % |
| 16 MiB | 182.4 / 172.0 / 175.4 / 178.5 / 173.9 | **176.5** (n=6) | 234–292 | 8 % |
| 32 MiB | 177.0 | **177.0** | — | saturated |

- **The lever is pion's advertised window, not the OS `rmem`:** a 16 MiB window gives 176.2 at 4 MiB `rmem` ≈
  176.6 at 16 MiB `rmem`. So E38's `rmem` finding was a secondary effect; the dominant one is
  `SetSCTPMaxReceiveBufferSize`.
- **"Browser-at-139 is a ceiling" does not stand.** Browser this session: **132.0 / 133.1 (mean 132.5)** vs the
  native receiver's **176.5** — the native is **+33 %** above the browser.
- **Corrected near-field ordering:** path capacity **245** > residual **buffer-insensitive sender-side cap ~176**
  > browser **132.5–139** > default native receiver **128**.

**Consequences:**
1. The campaign's "~120–139 Mbps **serialized browser** ceiling" was **partly an artifact of an untuned
   receiver** and must not be quoted as the browser's limit.
2. The browser is still a **real ~25 % cost** at 11 ms (132.5 vs 176 achievable) — the direction survives, the
   magnitude and framing do not.
3. **New open item:** a **residual ~176 Mbps cap that receive buffers do not move** (32 MiB saturates at 177.0).
   It is buffer-insensitive and therefore **sender-side** — and it is now the most interesting unexplained limit
   in the near-field regime. The campaign exonerated the sender on a 0.65-core argument (E21) and a loopback
   figure (E22); that does not obviously square with a hard ~176 field cap, so it deserves its own experiment.

*(appended as results land)*
