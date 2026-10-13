# Experiment 40 — what caps the v1-direct sender at ~176 Mbps on the ~11 ms field path? — 2026-10-13

**Status:** COMPLETE — written incrementally after every cell. **10 field transfer cells** (all valid, none
discarded) + **2 in-session path-control cells** (TCP/UDP) + **5 SCTP send-side traces**. Every cell reported.

## Verdict

**The ~176.5 Mbps is neither a plateau nor a hardware cap. It is the *duty cycle* of a congestion-window
sawtooth around a real ~234.4 Mbps payload (≈240 Mbps SCTP) path ceiling — and the specific value ~176 is
produced by the rig's own `SB_SCTP_CA_STEP=32768`.**

Three findings, in order of importance:

1. **The sender is cwnd-limited at 100 % of samples and the shape is a SAWTOOTH, not a plateau.** Median
   `cwndFill` = 0.998 in every trace. The transfer is a repeating cycle of (i) a hard plateau at
   **234.357 Mbps payload** (printed exactly, as an integer number of 16,384-byte frames per second),
   (ii) a cliff, (iii) a **linear ramp back at 65–96 KB/s**. Every campaign number this campaign has ever
   quoted — 139, 176.5, 177, 232 — is a window average over this sawtooth. D2 (250 ms series) is in Phase 4.

2. **The ~176 is self-inflicted by `SB_SCTP_CA_STEP=32768`.** Holding everything else constant and flipping
   *only* that one environment variable on the *same* instrumented binary, 100 seconds apart, in the same
   session, against the same receiver on the same share: **177.592 Mbps with it set vs 223.633 Mbps with it
   unset (+26 %)**. Across all cells, the `SB_SCTP_CA_STEP=32768` arm is **163.2 / 167.8 / 174.5 / 177.6
   (n=4, never above 177.6)** and the no-knob arm is **184.3 / 201.4 / 202.5 / 206.7 / 216.9 / 223.6 (n=6,
   never below 184.3)** — a clean, non-overlapping separation. The knob was believed to be a *fix*
   (its comment: `SB_SCTP_CA_STEP` "raises the per-RTT step" and lifted a lab link 27.5 → 37.2 Mbps). On this
   path it is a large *regression*: it lets cwnd balloon to **2.04 MB**, drives `srtt` to **60.5 ms**, and
   produces **67 retransmissions including 9 RTOs** against **4 retransmissions and 0 RTOs** for the same
   binary with the knob off. The mechanism is in Phase 6.

3. **E39's residual is therefore explained, and E39's "buffer-insensitive" observation is explained too.**
   The cap is not the receiver: `peerRwnd` binds in ≤5 of 144 samples at a 16 MiB window (0/115 with the knob
   off), exactly as E39 predicted from throughput alone. The path is not 176: it carries **244.52 TCP /
   246.9 UDP Mbps** in-session. The app is not the limiter: the 5 MiB send queue holds a median 4.46–4.76 MiB
   and never drains. **H1 survives in modified form; H2, H3-as-cap and H4 are refuted; H5 is refuted.**

The near-field story becomes: **path 244.5–246.9 > SCTP sender at pion defaults, sustained
216.9–223.6 (89–91 % of the path) ≫ the ship configuration with `SB_SCTP_CA_STEP=32768`, sustained
163.2–177.6 > browser receive path 132.5–139.0.** The 176.5 was a *tuning regression*, not a physical limit.

## Question

E39 measured a native Go/pion receiver (`fieldrecv-rwnd`) at CLIENT-EAST (~10.9 ms) saturating at
**176.5 Mbps** with a ≥16 MiB advertised SCTP receive window, and **buffer-insensitive** (32 MiB = 177.0), while
the same session carried **245 Mbps** (E39 P1–P5) and the sender bursts to 233–329 in individual seconds. E21
measured the sender at 0.65 core; E22 measured 521–533 Mbps against a Go receiver on loopback. The brief asked
what caps the sender at ~176.

Hypotheses to discriminate: **H1** cwnd plateau at `cwnd/RTT ≈ 176 Mbps` (≈242 KB at 10.9 ms), loss- or
app-suppressed; **H2** the hardcoded `sendWithBackpressure` (64 KiB chunk, 5 MiB `maxBuffer`, 10 ms poll, no
`OnBufferedAmountLow`); **H3** sender CPU (E21's 0.65 core); **H4** the receive window binding; **H5** a path
policer at ~176.

## Setup

- Agent host **VERSA**; experiment container **`sb-run` only**, port **7879**, host networking,
  `--env-file env-run`, mirroring `run-variant.sh`. Production `sharebridge-agent` (7878) and all 43+
  non-ShareBridge containers untouched and up throughout.
- Signalling origin **TESTBOX**, `https://TESTBOX/` = **200**.
- Client **CLIENT-EAST** only for transfers (CLIENT-WEST not used). `net.core.rmem_default = 4194304`,
  `rmem_max = 16777216` during the native cells, restored to **212992 / 212992** at the end (confirmed).
- Path RTT CLIENT-EAST → VERSA egress **12.595 ms** avg (10.721 / 14.610, mdev 1.207, n=8 ICMP).
- Share: the orchestrator's direct share, `relay_only=false`, 754 MiB = 790,626,304 B = **6,325.010432 Mb**.
- Receiver: `fieldrecv-rwnd` (sha256 `d7c42581…` — the only binary with `--max-rx-buf`), every cell
  `--max-rx-buf 16777216`. Metric = `SUMMARY mbps` (cross-checked against the agent-side window where the
  container log still exists; E38/E39 established agreement to ≤0.03 s).
- **Mode assertion:** every cell returned `status=complete`, `payload_bytes = 790,626,304`, `bad_frames=0`
  and `selected="local=host remote=…"` (direct). Every cell also showed the relay standby expiring unused
  (`relay: pending wait window exceeded`); zero relay fallbacks in 10/10 attempts.
- **Three agent binaries were used** (this becomes the experiment's central confound, so it is stated up front):

  | binary | source | honours `SB_SCTP_*` tuning? | sha256 |
  |---|---|---|---|
  | `sb-agent:pristine` (campaign image) | v1-direct tree | **yes** — CA_STEP, MIN_CWND, FAST_RTX_WND, MAX_RX_BUF, MAX_MSG, RTO_MAX_MS | `32c0b872…` (image) |
  | E35 trace build | `benchdirect` tree + `apply_sctp_patch.sh trace` | **no** — only `SB_SCTP_TRACE*` | `0c29ca3302c831f8d23d3218fe1628a4b286d5973f43f5d91a9f84f4db131858` |
  | **faithful trace build** (built this session) | **`e2e-v1` tree** + `forks/_trace` + `apply_sctp_patch.sh trace` | **yes, both** — all `SB_SCTP_*` **and** `SB_SCTP_TRACE*` | `aa7bd8853b3793bae023f189c3754e4efdb6c1a72757ef715b0d4ca31a98e2bc` |

  `grep -ao "SB_SCTP_[A-Z_]*" <binary> | sort -u` is the evidence for column 3.
- **Instrumented-container deviation (reported, not hidden).** `sb-agent:instrumented` was **never built** on
  VERSA — only its build *context* `/home/ali/sharebridge-test/img-instrumented/` was staged
  (`docker images` lists `sb-agent:pristine`, `sb-agent:phase4a-test` and unrelated images; no
  `sb-agent:instrumented`). The brief forbids `docker build`, so instead of building I recreated `sb-run` from
  **`sb-agent:pristine`** with a prebuilt binary bind-mounted read-only over `/app/agent`:

  ```sh
  docker run -d --name sb-run --restart unless-stopped --network host \
    -v sb-run-data:/root/.sharebridge \
    -v /home/ali/sharebridge-test/img-<x>/agent:/app/agent:ro \
    --env-file env-run -e SB_SCTP_TRACE=1 -e SB_SCTP_TRACE_EVERY_MS=<ms> \
    sb-agent:pristine
  ```

  `Dockerfile.instrumented` only does `COPY agent .` over `alpine`, so a bind-mount is byte-equivalent to the
  image it would have produced. `sha256(/app/agent)` inside the container was verified against the local build
  for every arm. **No compiler ran on VERSA and no worktree source was touched.**
- **The `benchdirect` worktree's `internal/peer/peer.go` contains no `SB_SCTP_*` lookup at all** — so any agent
  built from it silently ignores every tuning knob. This is why the E35 binary is not a faithful instrument
  for this rig, and why I additionally built the faithful one from `.worktrees/e2e-v1/agent` in `/tmp`
  (**not** the modified worktree, and **no** file in either worktree was written).

## Raw cells — every run

`--max-rx-buf 16777216` on every row. `wall_s` is `fieldrecv`'s own first-frame→last-frame window.

| # | start (UTC) | agent binary | `SB_SCTP_CA_STEP` | wall_s | **Mbps** | payload_bytes | frames | bad | mode |
|---|---|---|---|---|---|---|---|---|---|
| C1 | 02:44:27 | pristine | 32768 | 36.239 | **174.537** | 790,626,304 | 48,261 | 0 | direct |
| C2 | 02:45:24 | pristine | 32768 | 37.684 | **167.843** | 790,626,304 | 48,258 | 0 | direct |
| I1 | 02:47:33 | E35 trace (`0c29ca33…`) | *ignored* | 31.236 | **202.489** | 790,626,304 | 48,263 | 0 | direct |
| I2 | 02:51:02 | E35 trace (`0c29ca33…`) | *ignored* | 30.599 | **206.705** | 790,626,304 | 48,269 | 0 | direct |
| C3 | 02:53:26 | pristine | 32768 | 38.745 | **163.246** | 790,626,304 | 48,262 | 0 | direct |
| C4 | 02:54:34 | pristine | **unset** | 31.408 | **201.380** | 790,626,304 | 48,264 | 0 | direct |
| C5 | 02:55:32 | pristine | **unset** | 34.316 | **184.314** | 790,626,304 | 48,258 | 0 | direct |
| E1 | 02:56:57 | **faithful trace** (`aa7bd885…`) | 32768 | 35.615 | **177.592** | 790,626,304 | 48,264 | 0 | direct |
| E2 | 02:58:40 | **faithful trace** (`aa7bd885…`) | **unset** | 28.283 | **223.633** | 790,626,304 | 48,263 | 0 | direct |
| E3 | 02:59:49 | **faithful trace** (`aa7bd885…`) | **unset** | 29.164 | **216.875** | 790,626,304 | 48,263 | 0 | direct |
| P1 | 02:52 | — | — | TCP 1-flow VERSA→CLIENT-EAST | — | **244.52 Mbps** | sent 246.20, retrans 24 | — | path control |
| P2 | 02:52 | — | — | UDP 1-flow, 300 Mbps offered | — | **246.9 Mbps** | 17.69 % loss | — | path control |

**Arm aggregates**

| arm | n | Mbps | mean | range | never exceeds / never below |
|---|---|---|---|---|---|
| `SB_SCTP_CA_STEP=32768` (C1,C2,C3,E1) | 4 | 174.54 / 167.84 / 163.25 / 177.59 | **170.80** | 163.2–177.6 | **never > 177.6** |
| no `SB_SCTP_*` tuning (C4,C5,I1,I2,E2,E3) | 6 | 201.38 / 184.31 / 202.49 / 206.71 / 223.63 / 216.88 | **205.90** | 184.3–223.6 | **never < 184.3** |
| — faithful binary only, knob off (E2,E3) | 2 | 223.63 / 216.88 | **220.26** | 216.9–223.6 | — |

**Zero failures, zero relay fallbacks, zero discarded cells.** One transient TLS failure on the very first
`curl` to TESTBOX (exit 35) that immediately succeeded on retry and was confirmed 200 on-host too.

### Phase 1 — the control reproduces E39

C1/C2 = 174.5 / 167.8. E39's ≥16 MiB arm was 172.0–182.4 (mean 176.5, sd 3.9). Reproduced within the spread
(today's path is 12.6 ms rather than E39's 10.9 ms).

**1 Hz `mbps_inst` series, verbatim:**

- C1: `0.170 4.388 227.045 204.210 232.522 234.357 94.110 134.480 206.176 233.177 234.357 222.298 15.991
  168.952 236.716 234.357 39.715 176.292 229.900 230.556 150.340 1.573 251.003 234.357 234.357 19.399 161.219
  235.799 228.327 18.088 236.061 234.357 229.638 15.335 198.050 236.585 234.357`
- C2: `3.325 74.711 219.021 234.357 97.649 137.363 213.123 233.832 234.357 87.556 22.413 14.418 292.684
  231.473 234.357 104.595 144.966 201.196 174.850 141.689 234.357 234.357 50.594 146.801 220.987 234.357 88.080
  12.321 285.475 234.357 221.250 0.131 200.540 226.623 234.488 234.226 55.181 211.681`
- E2 (the cleanest of all ten): `1.896 113.739 201.196 235.930 234.357 224.526 235.799 234.357 149.291 193.200
  236.454 234.357 234.357 234.357 234.357 234.357 234.357 234.357 234.357 234.357 234.357 234.357 234.357
  234.357 234.357 234.357 234.357 234.226 234.357` — **19 consecutive seconds at exactly 234.357**.
- E1 (same binary, knob ON): `2.869 0.524 315.210 160.301 160.301 212.468 234.357 117.965 5.112 238.813 234.357
  165.020 143.655 236.061 234.226 81.789 192.414 235.274 124.912 183.108 237.109 228.590 236.585 234.357 121.766
  8.913 239.075 235.536 234.357 81.789 63.963 262.144 234.357 85.197 188.088 234.357` — same ceiling, many
  more deep dips.

**Observation (reported, not explained):** the 1 Hz ceiling is **quantised to an exact integer count of
16,384-byte payload frames per second** — 234.357 Mbps ≡ 1788 frames/s, 230.818 ≡ 1761, 234.226 ≡ 1787 — and the
same value repeats for up to 19 consecutive seconds. All ten cells share the same ~234.4 payload ceiling
(≈240 Mbps at the SCTP layer). I did not identify the limiter that produces the integer-frame quantisation and
make no claim about it. It is *not* a simple wire policer: P1/P2 show the path carrying 244.5–246.9 Mbps of TCP
goodput, i.e. **more** than the SCTP ceiling.

**(Non-)confounder — the trace itself.** `SB_SCTP_TRACE` costs one read-lock per sample; E35 proved it inert
when off and correct when on. I did not run a trace-off control on the faithful binary, but the E35 binary with
the trace on (I1/I2, 202.5/206.7) and the pristine binary with no trace (C4, 201.4) agree to 0.5 %, so the
trace is not a throughput factor.

## Phase 4 — the trace: H1–H4

Raw traces: `docs/superpowers/spikes/results/raw/exp40/{i1,i2,e1,e2,e3}-trace.jsonl`. All samples in
`Established`; one association per cell.

| quantity | I1 (E35, knobs ignored) | I2 (E35) | **E1 (faithful, CA_STEP=32768)** | E2 (faithful, knob off) |
|---|---|---|---|---|
| Mbps (fieldrecv) | 202.489 | 206.705 | **177.592** | **223.633** |
| `retransTotal` over 790 MB | 10 | 20 | **67** | **4** |
| — of which RTO / fast | 1 / 9 | 0 / 20 | **9 / 58** | **0 / 4** |
| `packetsSent` | 676,109 | 676,108 | 681,025 | 676,081 |
| retransmit rate | 1.5 × 10⁻⁵ | 3.0 × 10⁻⁵ | **9.8 × 10⁻⁵** | **5.9 × 10⁻⁶** |
| cwnd cut events | 10 | 9 | **20** | **4** |
| median `cwndFill` | 0.998 | 0.998 | **0.999** | **0.999** |
| median `cwnd` / max `cwnd` | 361 KB / 656 KB | 394 KB / 744 KB | **527 KB / 2,042 KB** | **530 KB / 929 KB** |
| median `pendingBytes` / max | 4.76 / 5.25 MB | 4.71 / 5.15 MB | 4.46 / 5.23 MB | 4.55 / 5.09 MB |
| `peerRwnd` min | 1,262 (at the 1 RTO) | 16,758,474 | **188,300** | 16,760,818 |
| samples with `rwnd < cwnd` | 1 / 126 | 0 / 123 | **5 / 144** | **0 / 115** |
| median `srttMs` / max | 12.33 / 21.83 | 13.62 / 91.40 | **19.01 / 60.45** | **17.67 / 32.50** |
| median implied RTT (`cwnd`÷rate) | 12.61 ms | 13.47 ms | **27.32 ms** | **17.80 ms** |
| median cwnd growth per 250 ms | +24,000 B (96 KB/s) | +21,600 B (86 KB/s) | **+163,840 B (655 KB/s)** | **+16,200 B (65 KB/s)** |
| wire ÷ payload | 1.02567 | — | — | — |

### H1 — cwnd plateau → **PARTLY TRUE, but the shape is a sawtooth and it is LOSS-suppressed, not app-suppressed**

`cwndFill` = 0.998–0.999 in every trace: the sender has a full window outstanding at essentially every sample,
i.e. **it is cwnd-limited 100 % of the time**. But `cwnd` is never static. Verbatim E1 (the campaign
configuration) cut events:

```
02:56:59.799   207492 ->  202608   ss=103746   dFast=0 dRTO=1 dSS=-103746   srtt=13.8
02:57:01.299  1481066 ->  406545   ss=406545   dFast=2 dRTO=0 dSS=+239705   srtt=43.5
02:57:01.549   406545 ->  205030   ss=205030   dFast=2 dRTO=0 dSS=-201515   srtt=11.9
02:57:02.799   219503 ->  208253   ss=205909   dFast=2 dRTO=0 dSS=+22690    srtt=12.2
02:57:05.299  2042116 ->  522823   ss=522823   dFast=1 dRTO=0 dSS=+418697   srtt=20.5
02:57:06.300   522823 ->    4800   ss=4800     dFast=0 dRTO=1 dSS=-518023   srtt=10.5   <- RTO, cwnd -> 4 MTU
02:57:09.299  1750172 ->  924238   ss=924238   dFast=1 dRTO=0 dSS=+716506   srtt=53.6
02:57:09.549   924238 ->  240723   ss=240723   dFast=4 dRTO=0 dSS=-683515   srtt=12.9
02:57:10.549   240723 ->  166227   ss=97185    dFast=1 dRTO=1 dSS=-143538   srtt=12.7
02:57:13.050  2003557 ->  260687   ss=260687   dFast=3 dRTO=0 dSS=-2650     srtt=29.5
02:57:14.050   260687 ->  169830   ss=130343   dFast=0 dRTO=1 dSS=-130344   srtt=11.5
02:57:16.299  1718046 ->  222072   ss=222072   dFast=2 dRTO=0 dSS=+113174   srtt=16.2
02:57:19.299  1668995 ->  885993   ss=883649   dFast=1 dRTO=0 dSS=+626022   srtt=48.7
02:57:19.549   885993 ->  396016   ss=228660   dFast=3 dRTO=0 dSS=-654989   srtt=14.0
02:57:22.299  2036654 ->  517941   ss=517941   dFast=3 dRTO=0 dSS=+187567   srtt=24.0
02:57:23.300   517941 ->    4800   ss=4800     dFast=0 dRTO=1 dSS=-513141   srtt=12.2   <- RTO
02:57:27.049  1996079 ->  523602   ss=523602   dFast=4 dRTO=0 dSS=+268915   srtt=43.9
02:57:28.050   523602 ->    3544   ss=261801   dFast=0 dRTO=1 dSS=-261801   srtt=14.3   <- RTO
02:57:31.050  1876879 ->  139098   ss=139098   dFast=1 dRTO=0 dSS=-94693    srtt=32.0
02:57:32.050   139098 ->   71470   ss=69549    dFast=0 dRTO=1 dSS=-69549    srtt=10.2   <- RTO
```

- **At the top of every ramp `srtt` balloons**: 10 → 43–60 ms in E1. The excess over the 12 ms base RTT is
  standing queue, and `cwnd` grows *because* the queue grows — the achieved rate is pinned at ~240 SCTP-Mbps
  throughout. Median implied RTT equals median `srtt` (27.3 vs 19.0 in E1; 12.6 vs 12.3 in I1), so
  **rate = cwnd / (base RTT + queueing)** exactly.
- **The cut depth scales with how high cwnd got.** E1 cuts cwnd by 3.6×, 3.9×, 109×, 148× from ~2 MB; the
  no-knob arm (E2) peaks at 929 KB and its worst cut is 3.4× from 506 KB.
- **It is loss-suppressed, not app-suppressed.** Every cut is accompanied by `ssthresh` halving and, in E1,
  67 retransmissions and 9 RTOs. There is no sample where cwnd stops growing while `pendingBytes` is empty.
- **The path is not lossy.** 4–67 retransmitted packets out of ~676,000–681,000 sent (5.9 × 10⁻⁶ to
  9.8 × 10⁻⁵). These are a handful of *isolated* events, each of which costs seconds.

### H2 — sender app-side pacing → **REFUTED as the limiter**

The code is exactly as briefed (`manager.go:25–27`: `chunkSize = 64*1024`, `maxBuffer = 5*1024*1024`,
`sleepInterval = 10*time.Millisecond`; `sendWithBackpressure` at `manager.go:1535` loops on
`endpoint.BufferedAmount() > maxBuffer` with no `OnBufferedAmountLow`). But it is not the throughput limiter,
because **the queue it guards is permanently full**:

- `pendingBytes` (chunks queued in SCTP, not yet handed to the wire): median **4.46–4.76 MiB** against a 5 MiB
  `maxBuffer`; max observed 5,250,866 B (i.e. it does reach the ceiling).
- **E3 at 50 ms resolution** (1,537 trace lines): min 0, p05 **4.42 MB**, median **4.69 MB**, max **5.11 MB**;
  only **2.0 %** of samples below 4 MiB, and `pendingBytes == 0` in just **8 / 591** samples.
- `cwndFill` is 0.995–1.000 throughout, including inside the 50 ms series — there is **no** drain/refill cycle,
  no ~10 ms-period sawtooth, and the app is never waiting.

A `sendWithBackpressure` that gated the transfer would show `pendingBytes` collapsing and `cwndFill` dropping
well below 1 while the app polls. Neither happens in 591 samples of E3 or in any 250 ms trace. **The sender
waits on cwnd, never on the app.** (The 8 zero samples are at the tail of the transfer.)

### H3 — sender CPU → **re-measured; 0.65 core does not reproduce, but CPU is not the cap**

`docker stats --no-stream sb-run`:

- during C2 (167.8 Mbps), 8 samples at 2 s: `114.77 / 84.29 / 85.37 / 86.43 / 39.79 / 86.97 / 85.23 / 58.65` %
- during E3 (216.9 Mbps), 6 samples at 1.5 s while active: `78.24 / 78.31 / 78.10 / 81.55 / 78.90 / 79.76` %
  (then `45.90 / 0.86 / 0.69 / 0.82` after the transfer ended)

Median ~0.80–0.86 core at both 168 and 217 Mbps. That is **materially above E21's 0.65 core** and it does not
scale with throughput, so it is not a hard CPU cap here (a 100 % pinned core would be the signature, and the
same container reached 223.6 Mbps at 0.79 core). It does mean **E21's 0.65-core figure is not reproducible at
the plateau today** and should not be cited as settled.

### H4 — ack / receiver side → **REFUTED (E39 confirmed from the trace)**

At a 16 MiB advertised window, `peerRwnd` never falls below **16,758,474 B** in I2 and **16,760,818 B** in E2,
with `rwnd < cwnd` in **0 / 123** and **0 / 115** samples. I1 shows 1 / 126 — and that single sample *is* the
RTO instant (`peerRwnd` 1,262 with `outstanding` 1,172). E1 shows 5 / 144, all transient dips to ~188 KB while
a 2 MB cwnd is draining. **The receive window is not the constraint at ≥16 MiB** — which is exactly why E39
saw 32 MiB = 177.0 ≈ 16 MiB = 176.5.

### H5 — a path policer at ~176 → **REFUTED**

In-session, no transfer running: **TCP 1-flow 244.52 Mbps** (sent 246.20, 24 retransmits) and **UDP 300 →
246.9 Mbps at 17.69 % loss** — identical to E39's 244.80 / 247.34 @ 17.6 %. The path is ~245–250 Mbps at both
ends of the session. It is not 176. (A policer/queue *near* 240–250 interacting with the sender's bursts is the
leading explanation for why the sawtooth exists at all — but it is at ~240–250, not at 176, and I did not
isolate it; see Caveats.)

## D2 — the 250 ms throughput series

Raw series for E1 and E3 is in the artifacts; the shape is identical in all five traces and is reproduced here
from I1 (`sentBytes` deltas ÷ 0.25 s; these are **SCTP-level** Mbps, exactly 2.57 % above the receiver's
payload Mbps):

```
   ramp (cut recovery)          PLATEAU (234.4 payload / 240.9 SCTP)          cliff      ramp ...
02:47:35.0   2   35.3  78   35.5 112   35.8 138   36.0 150   36.3  92   36.5 115   36.8 141
02:47:37.0 163   37.3 189   37.5 216   37.8 238   38.0 246   38.3 241   38.5 241   38.8 241
02:47:39.0 240   39.3 241   39.5 241   39.8 241   40.0 240   40.3 240   40.5 241   40.8 241
02:47:41.0 240   41.3 240   41.5 242   41.8 240   42.0 241   42.3 241   42.5 239   42.8 232
02:47:43.0 242   43.3 241   43.5 240   43.8 241   44.0 241   44.3 241   44.5 241   44.8 241
02:47:45.0 241   45.3 241   45.5 241   45.8 215   46.0 128   46.3 150   46.5 179   46.8 201
02:47:47.0 225   47.3 249   47.5 240   47.8 242   48.0 241   48.3 241   48.5 241   48.8 241
02:47:49.0 240   49.3 227   49.5  61   49.8  83   50.0 107   50.3 131   50.5 156   50.8 182
02:47:51.0 207   51.3 233   51.5 250   51.8 241   52.0 239   52.3 242   52.5 241   52.8 241
02:47:53.0 200   53.3 217   53.5 242   53.8 247   54.0 241   54.3 241   54.5 241   54.8 156
02:47:55.0 100   55.3 124   55.5 148   55.8 176   56.0 193   56.3 224   56.5 246   56.8 244
02:47:57.0 242   57.3 241   57.5 241   57.8 241   58.0 241   58.3 241   58.5 241   58.8 241
02:47:59.0 240   59.3 241   59.5 240   59.8 241   00.0 241   00.3 238   00.5 244   00.8 225
02:48:01.0 246   01.3 241   01.5 241   01.8 236   02.0 113   02.3  99   02.5 108   02.8 153
02:48:03.0 174   03.3 195   03.5 223   03.8 250   04.0 241   04.3 242   04.5 241   04.8 241
```

**Shape: a SAWTOOTH, not a ramp-to-plateau.** Three unmistakable states repeating ~7 times in 31 s:

1. **Plateau** at 240–242 SCTP-Mbps (≈234.4 payload) held for 1–6 s with sub-1 % sample-to-sample variance.
2. **Cliff**: one to two 250 ms samples at 60–130 Mbps when the cwnd cut lands.
3. **Linear ramp**: an almost perfectly linear climb back from ~60 to ~240 over 1.5–3 s.

The ramp slope is additive-increase: `dcwnd/dt = (throughput ÷ cwnd) × CA_STEP`. I1's 96 KB/s with a ~361 KB
cwnd and E2's 65 KB/s with a ~530 KB cwnd both give **CA_STEP ≈ 1200 B = 1 MTU**, pion's default. E1's
655 KB/s with a ~527 KB–2 MB cwnd gives **CA_STEP ≈ 32768** as configured.

**This changes the diagnosis completely.** Every campaign number (139.0, 176.5, 177.0, 232, 245) is a window
average over this waveform. "The near-field plateau is ~176" is not a plateau; the instantaneous ceiling is
~234.4 payload-Mbps and 176 is the *duty cycle* of a slow recovery.

## Phase 6 — why `SB_SCTP_CA_STEP=32768` costs 15–26 %

Same binary (`aa7bd885…`), same receiver, same share, same session, 100 s apart. **E1 (knob on) = 177.592;
E2 (knob off) = 223.633.** The knob's own source comment says it exists because "pion … grows the congestion
window by one MTU per RTT … climbing to an 800 KB bandwidth-delay product takes ~66 seconds; any transfer
shorter than that is ramp-limited rather than link-limited" — a correct diagnosis *for the lab link it was
measured on* (64 Mbps / 100 ms / 800 KB queue: 27.5 → 37.2 Mbps). On this path it inverts:

| | E1 (`CA_STEP=32768`) | E2 (default 1 MTU) |
|---|---|---|
| Mbps | 177.592 | **223.633** |
| max `cwnd` reached | **2,042,116 B** | 928,518 B |
| max `srtt` | **60.45 ms** (vs 12 ms base) | 32.50 ms |
| `retransTotal` | **67** (9 RTO) | **4** (0 RTO) |
| cwnd cut events | 20 | 4 |
| peer-window binds (`rwnd<cwnd`) | 5/144 | 0/115 |
| 1 Hz deep dips <150 | 12 of 35 seconds | 1 of 29 seconds |

**Mechanism.** The CA increment only fires when a full cwnd of bytes has been ACKed (RFC 4960 §7.2.2 in the
fork, `forks/sctp/association.go:2394`: `step := max(a.MTU(), a.cwndCAStep)` guarded by
`partialBytesAcked >= a.CWND()`). A 32768-byte step therefore lets cwnd run up to ~2 MB — **3–5× the path's
BDP** (~360 KB clean, ~660 KB with the queue). The excess becomes a standing queue of up to **48 ms** of
delay, which then overflows in a *burst*: the loss burst triggers fast retransmit *and* RTOs, and each RTO
resets cwnd to ~4 MTU (4,800 / 3,544 / 4,800). With the default 1-MTU step cwnd only reaches ~930 KB, the
overshoot is mild, and in E2 there is **not a single RTO in 790 MB**. A ramp-limited link wants a big CA step;
a fast, shallow-queue path wants a small one.

This also completes the E39 picture: E39's 16 MiB window arm was measured entirely *with this knob on*, and the
"buffer-insensitive ~176 residual" it identified is this. Raising the receive window could never fix it,
because the receive window was never binding.

## Interpretation

**For v1-direct on the near-field path:**

- **The sender is not capped at 176 by anything physical.** With the campaign's own agent but the CA knob
  unset, the same receiver on the same path sustains **216.9–223.6 Mbps = 89–91 % of the 244.5 Mbps path** —
  better than any number this campaign has recorded on the field path at ~11 ms, and the first field result
  to *close* most of the gap to the path control.
- **Nothing ties the sender to 176**: not the receiver window (H4 dead in ≤5/144 samples), not the app queue
  (H2 dead — 4.5 of 5 MiB full, always), not CPU (H3 — 0.8 core at both 168 and 217 Mbps), not the path
  (H5 dead — 244.5/246.9). What remains is a loss-signalled cwnd sawtooth around a ~240 Mbps SCTP ceiling,
  whose *amplitude* is set by the CA step.
- **The fix is a one-variable configuration change, not code**: drop `SB_SCTP_CA_STEP` (or set it well below
  32768) for the field rig. That is worth **+26 %** on this path, and it is the difference between "the
  near-field ceiling is ~176" and "the near-field ceiling is ~224".
- **The campaign's near-field headline needs rewriting again.** E39 replaced "the ceiling *is* the browser's
  DataChannel receive path" with "receiver-side SCTP window configuration binds". Both are now superseded:
  at 11 ms the binding constraint is **sender-side congestion control on a policed path**, and the specific
  176.5 value is the campaign's own tuning.
- **Caveat on generality:** E38's 70 ms arm found `--max-rx-buf 4194304` made things *worse* at 70 ms
  (84.85 vs 111.4). The CA-step result here is a ~12 ms result and should be re-measured at 70 ms before being
  applied there.

**For v2:** the lever is not "make the client faster". It is CC behaviour on a policed path — BBR-style
pacing, a smaller CA step with a cwnd cap near the measured BDP, or simply not overriding pion's default.
The `SB_SCTP_CA_STEP` knob as shipped is a lab-link optimisation that is harmful on the field path.

**Lab/field asymmetry, explicit:** the lab bench that justified CA_STEP=32768 (64 Mbps, 100 ms RTT, 800 KB
queue) is ramp-limited and deep-buffered; the field path is policed at ~245 Mbps, ~12 ms, shallow-buffered.
The knob is right for the first and wrong for the second. This is a case where a lab-derived tuning constant
did not transfer, and the campaign carried it into every field measurement afterwards.

## Caveats

- **The "why 234.4 and not 244.5" question is NOT answered.** I established the ceiling exists (~234.4
  payload-Mbps in all ten cells; TCP does 244.5) and that it quantises to an exact integer number of 16 KiB
  frames per second, but I did not identify the limiter. It is above the sender's sustained rate in the
  no-knob arm (223.6 = 95 % of it), so it does not bind the headline result, but it *would* bind a further
  optimisation. A policer/queue near 240–250 Mbps interacting with SCTP's bursts is the leading hypothesis;
  it was not isolated.
- **n is small and the arms are separated by a confound-free single variable only for E1 vs E2/E3.** The
  `SB_SCTP_CA_STEP=32768` arm has n=4 across two binaries (C1,C2,C3 on pristine; E1 on the faithful trace
  build) spanning 163.2–177.6; the no-knob arm has n=6 across three binaries spanning 184.3–223.6. The arms do
  not overlap, and E1 vs E2 is a clean single-variable pair on one binary 100 s apart — but the *binary* is
  also a variable in the other cells, so the reported 15–26 % effect size should be read as "large and
  consistent", not as a precise constant.
- **C5 = 184.3 is a low outlier inside the no-knob arm** (the arm's other five are 201.4–223.6). It is reported
  as measured. If the true no-knob mean is ~205–220 rather than 205.9, the effect of the knob is larger still.
- **The mechanism trace exists for the CA_STEP arm (E1) and the default arm (I1/I2/E2/E3), not for the exact
  pristine image.** E1 used the faithful build from `.worktrees/e2e-v1/agent`, which is the tree the pristine
  image was built from plus the trace patch — but a byte-identical source match was not verified. E1's
  throughput (177.592) reproducing E39's 176.5 to within 0.6 % is the evidence that it is the same sender.
- **Agent-side window cross-check is available for C1/C2 only.** The instrumented container's logs were
  destroyed by the (necessary) container recreation between arms. The primary metric throughout is
  `fieldrecv`'s own `SUMMARY` window, which E38/E39 established agrees with the agent-side window to
  ≤0.03 s; C1 agreed to 0.246 s and C2 to 0.062 s in this session.
- **`sb-agent:instrumented` does not exist on VERSA** and was not built; the brief's premise was wrong. I used
  a bind-mount of a prebuilt binary (Phase-2 deviation above) rather than `docker build`, per the brief's
  prohibition. `sb-run` was restored to the briefed state at the end (`sb-agent:pristine`,
  `SB_SCTP_CA_STEP=32768`, removed 03:01:16Z, `agent authenticated`, 1 session).
- **Client state restored.** `net.core.rmem_default`/`rmem_max` were raised to 4 MiB / 16 MiB for the native
  cells and **restored and confirmed at 212992 / 212992**. Only ever increased; no persistent change remains.
  Process gate on both clients returned **0** before every cell and at the end.
- **Not attempted (out of time budget, would strengthen the result):** the N2 loss-correlation arm (E19's
  DTLS-record-seq-gap tooling against the 250 ms series) and the **A3/N4 `tc fq` egress-pacing arm on VERSA at
  ~1.1× delivered**. The `tc` arm is now clearly the highest-value follow-up: it directly tests the
  "bursts vs policer" hypothesis for the 234.4 ceiling and would also test whether pacing the CA_STEP=32768
  sender recovers the throughput. It was **not run and nothing was changed on the VERSA network stack** —
  there is nothing to revert.
- **H5's negative is a single pair of control cells.** TCP 244.52 and UDP 246.9 at 17.69 % loss, agreeing with
  E39's in-session 244.80 / 247.34 @ 17.6 % to within 0.1 %.
- Safety: production `sharebridge-agent` (7878) and every non-ShareBridge container on VERSA were untouched
  and verified up at the end. No container was restarted or removed except `sb-run`. No `git` command was run.
  No IP, hostname or share code appears in this file or in the artifact traces (leak-checked).

## Artifacts

- This file: `docs/superpowers/spikes/results/2026-10-13-exp40-sender-side-176-cap.md`
- **Traces** (JSONL, ~250 ms cadence except E3 at 50 ms):
  `docs/superpowers/spikes/results/raw/exp40/i1-trace.jsonl` (208 lines),
  `i2-trace.jsonl` (423), `e1-trace.jsonl` (262), `e2-trace.jsonl` (157), `e3-trace.jsonl` (1,537)
- **Per-cell receiver logs** (`PROGRESS` 1 Hz rows + `SUMMARY`):
  `docs/superpowers/spikes/results/raw/exp40/client-logs/fr-{C1,C2,C3,C4,C5,I1,I2,E1,E2,E3}.log`
- Binaries: pristine image `sb-agent:pristine` (`32c0b872…`); E35 trace build
  `0c29ca3302c831f8d23d3218fe1628a4b286d5973f43f5d91a9f84f4db131858`; **faithful trace build**
  `aa7bd8853b3793bae023f189c3754e4efdb6c1a72757ef715b0d4ca31a98e2bc` — staged on VERSA at
  `/home/ali/sharebridge-test/img-exp40/agent` (kept for follow-up).
- Faithful-build recipe (no worktree file written; everything in `/tmp`):
  `cp -R .worktrees/e2e-v1/agent /tmp/exp40/e2eagent`; copy `benchdirect/agent/forks/_trace/` and
  `benchdirect/agent/apply_sctp_patch.sh` in; `./apply_sctp_patch.sh trace`;
  `CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o agent-instr-e2e ./cmd/agent`.
- Share consumption went 4 → 14 across this experiment (10 cells, `max_downloads=500`).
