# ShareBridge transport investigation — complete experiment record (2026-09-15)

Everything measured in this investigation, in order, with headline results and pointers to the
raw data. Two phases: **lab** (loopback with a synthetic bottleneck shim, plus kernel TCP under
`tc netem`) and **field** (real internet: campus browser → home agent → public VPS).

## Where the artefacts live

| what | branch / worktree |
|---|---|
| lab harness, raw lab data, lab report | `benchdirect` @ `.worktrees/benchdirect` |
| v1 code change (SCTP tuning), field reports, bug report | `main` (v1) @ `.worktrees/e2e-v1` |
| `v2` / `phase-4a` | **never modified** (read-only inspection only) |

Raw data files (all append-only JSONL, one JSON object per run):

| file | branch | rows |
|---|---|---|
| `agent/multiconn_results.jsonl` | benchdirect | 268 |
| `agent/sctppatch_results.jsonl` | benchdirect | 24 |
| `agent/sctppatch2_results.jsonl` | benchdirect | 36 |
| `agent/bbr_results.jsonl` | benchdirect | 36 |
| `agent/bigpayload_results.jsonl` | benchdirect | 17 |
| `agent/tcp_300mb.jsonl` | benchdirect | 2 |
| `agent/sizesweep_results.jsonl` | benchdirect | 30 |
| `agent/tcp_sizesweep.jsonl` | benchdirect | 5 |

---

# Part 1 — Lab

Common harness: a Go shim in `agent/cmd/benchdirect` that forwards UDP through a shared
bottleneck (one rate limiter + one FIFO queue with tail-drop, configurable delay/jitter/loss),
driving real Chrome over real pion SCTP DataChannels. `--conns N` stripes the payload across N
independent PeerConnections.

## E1. Multi-connection scaling — `multiconn_results.jsonl` (268 runs)

Question: does striping across N PeerConnections help, and why?

| condition | N=1 | N=2 | N=4 |
|---|---|---|---|
| rtt=0 ceiling | 494.2 | 566.9 | 582.7 |
| rtt=100 ms, clean (n=10) | mean 132.7, p10 54.9, **p90 254.3**, spread 4.85 | mean 232.6, spread 10.49 | mean 392.1, p10 362.1, **spread 1.38** |
| rtt=100 + 10 ms jitter | 209.0 | 279.6 | 341.9 |
| shared 8 MB/s cap | 9.2 | 12.4 | 13.2 (1.44×) |
| 8 MB/s *each* | 8.9 | 23.6 | 22.3 (2.50×) |
| 1% loss @ rtt=25 | 4.3 | 7.2 | 15.8 (3.66×) |

- The rtt=0 "ceiling" is a **harness artifact** (single shim drain goroutine) — Chrome used only
  ~1.3–1.4 cores.
- **Multi-connection fixes the high-RTT *tail*, not the mean**: p10 improved 6.6× (55→362), p90
  only 1.6×; spread 4.85→1.38. Worst N=4 run (296.9) beat the best N=1 run (254.3).
- Mechanism is **independent congestion windows**, not load balancing: per-connection balance was
  1.00 in every run. RFC 8831 — all SCTP streams in one association share one cwnd.

## E2. Bottleneck sweeps — same file

Queue depth (64 Mbps, 32 MiB, n=3):

| queue | throughput | tail drops |
|---|---|---|
| 800 KB | 9.6 (15%) | 1107 |
| 2 MB | 16.0 | 1996 |
| 5 MB | 55.6 (87%) | 0 |
| 16 MB | 56.4 (88%) | 0 |

Bandwidth sweep (buffer sized as bw/10): 16→8.0, 32→7.6, 64→7.3, 128→6.9, 256→5.3 Mbps — i.e.
throughput is roughly **constant at 5–15 Mbps and does not scale with the link**.

Loss sweep (rtt=100+jitter, 8 MiB, uncapped), N=1 / N=4:
0.01% → 41.5/87.6; 0.1% → 6.5/21.1; 0.3% → 2.1/8.3; 1% → 1.0/4.1 Mbps.

## E3. Kernel TCP control — `agent/run_tcpcontrol.sh`

Kernel TCP under `tc netem` in a container (`alpine:3.20`, `--cap-add=NET_ADMIN`), both endpoints
over `lo`, matched to the shim's conditions. Default CC is **cubic** (BBR is not available in this
kernel).

| condition | kernel TCP |
|---|---|
| 0% loss | **54.7 Mbps (86% of 64 Mbps), 0 drops** |
| 0.1% loss | ~56 Mbps |
| 1% loss | 47.2 Mbps (74%) |
| 5% loss | 24.6 Mbps |

Queue 530 pkt vs 3500 pkt: 57.0 vs 56.3 — queue depth is irrelevant to TCP here. Loopback
baseline 140 Gbps. **One** TCP connection beat **four** SCTP associations by 3.7×.

## E4. The root cause — pion's SCTP behaviour

- `sctp/association.go:803,1809` — `a.ssthresh = a.RWND()`: initial ssthresh is set to the peer's
  receiver window (~5 MB from Chrome) rather than the BDP (~800 KB at 64 Mbps × 100 ms).
- `sctp/rtx_timer.go:17` — `rtoMin = 1000 ms` hardcoded.
- On RTO: `ssthresh = max(cwnd/2, 4*MTU)`, `cwnd = 1 MTU`.
- Congestion avoidance: `cwnd += max(MTU, cwndCAStep)` per RTT; unset means **+1 MTU/RTT ≈ 12 KB/s**.
- `SettingEngine` exposes exactly six SCTP knobs; **ssthresh is not one of them** (needs a fork).

## E5. pion fork patch matrix — `sctppatch_results.jsonl`, `sctppatch2.jsonl`

64 Mbps, 800 KB queue, 32 MiB, n=3, N=1 / N=4:

| variant | N=1 | N=4 | % of TCP |
|---|---|---|---|
| unpatched | 14.5 | 14.5 | 23% |
| unpatched + CA 32 KB | 22.8 | 27.3 | 36% |
| ssthresh 256 KB | 21.8 | 26.3 | 34% (spread **1.00**) |
| ssthresh 256 KB + CA 8 KB | 33.9 | 32.5 | — |
| **ssthresh 256 KB + CA 32 KB** | **34.2** | **40.4** | **53% / 63%** |
| ssthresh 256 KB + CA 128 KB | 27.6 | 35.1 | — |
| ssthresh 768 KB | 43.7 | 16.1 | 68% / 25% |
| kernel TCP | 54.7 | — | 86% |

- A fixed ssthresh cannot win across link speeds: 768 KB (≈BDP) is best at N=1 but collapses at
  N=4 (4 × 768 KB > the shared 800 KB queue); 256 KB would strangle a 256 Mbps link.
- `rtoMin` hypothesis **not** confirmed — `rtoMin` is irrelevant once overshoot is removed (no
  drops ⇒ no RTOs).

## E6. BBR-lite — `bbr_results.jsonl` (36 runs) — **FAILED**

~70 lines implementing a BDP-aware cwnd cap in the fork (delivery-rate sampling, min-RTT, startup
exit, clamp).

| config | ssthresh+CA | BBR-lite | unpatched |
|---|---|---|---|
| 64 Mbps, 100 ms, 32 MiB, N=1 | 33.7 | 7.6 | 8.7 |
| 64 Mbps, 100 ms, 32 MiB, N=4 | 41.2 | 22.7 | 14.7 |
| 300 MB, N=4 | 49.9 | **4.7 (514 s)** | 39.2 |

**Worse than doing nothing**, and worse at scale (its 10 s rate-decay strangles a long transfer).
Cause: pion does not **pace**, so cwnd growth becomes bursts that overflow the buffer during
startup, before the rate estimator can know anything. Real BBR depends on pacing, which means
changing pion's send loop.

## E7. Realistic payload — `bigpayload_results.jsonl`, `tcp_300mb.jsonl`

32 MiB at 40 Mbps is ~6 s, so `size/wall` could have been measuring a startup burst. Tested at
300 MB instead. The result inverted: **throughput rises with payload size**, because the dominant
cost is the CA ramp.

| variant | 32 MiB | 300 MB |
|---|---|---|
| unpatched N=1 | 8.7 | 27.5 |
| unpatched N=4 | 14.7 | 39.2 |
| ssthresh+CA N=1 | 33.7 | 40.4 |
| ssthresh+CA N=4 | 41.2 | 49.9 |

The 100 ms trace of unpatched N=1 at 300 MB rises monotonically for the whole 87 s and never
plateaus (1 → 56 Mbps) — textbook `+1 MTU/RTT`, ~66 s to reach an 800 KB BDP. **A 32 MiB transfer
never leaves the ramp**, so short-payload numbers measure the ramp, not the link.

## E8. Payload sweep 8 MiB → 1 GB — `sizesweep_results.jsonl`, `tcp_sizesweep.jsonl`

64 Mbps, 100 ms, 800 KB queue, 0% loss, n=1. Kernel TCP is *itself* payload-dependent
(31.6 / 49.4 / 56.2 / 56.6 / 57.1 Mbps) — part of the small-payload penalty is inherent to any CC.

% of kernel TCP, wall Mbps:

| variant | 8 MiB | 32 MiB | 128 MiB | 300 MB | 1 GB |
|---|---|---|---|---|---|
| **ssthresh+CA N=4** | **74%** | **87%** | **85%** | **85%** | **91%** |
| CA only N=4 | 42% | 46% | 88% | 87% | 86% |
| unpatched N=1 | 55% | 19% | 33% | 50% | 76% |

- A static policy **does** hold, but only with both knobs: ssthresh+CA at N=4 stays 74–91% and
  raises the worst cell from 19–27% to 63% (N=1) / 74% (N=4).
- The fork is needed for **small** payloads: CA-only is 27–42% at 8 MiB vs 74–95% with the
  ssthresh cap. The cap protects the *start*; `cwndCAStep` accelerates the *ramp*.
- **N is genuinely payload-dependent** and is the only knob without a single static value: N=4
  *hurts* at 8 MiB (0.77×) and helps from 32 MiB up (1.12–1.39×). Payload size is known at t=0,
  so this is policy, not congestion control.
- Unpatched steady state is fine: at 1 GB it hits 56.2 vs CUBIC's 57.1. Its deficit is the ramp.

## E9. Do the patches survive loss? — `bbr_results.jsonl`

| loss | N=1 unpatched → ssthresh+CA | N=4 unpatched → ssthresh+CA |
|---|---|---|
| 0% | 8.7 → 33.7 (3.87×) | 14.7 → 41.2 (2.81×) |
| 0.1% | 4.1 → 15.5 (3.81×) | 10.1 → 38.5 (3.82×) |

Consistent ~3.8× at both 0% and 0.1% loss, spread ≤1.22×. Not a no-loss-only optimisation.

---

# Part 2 — Field

Full writeups: `2026-09-15-e2e-campus-home-field-test.md`, `2026-09-15-e2e-sctp-patch-ab-field-test.md`.

## Topology

| node | location | role |
|---|---|---|
| Browser | <campus> (two campus networks) | campus Wi-Fi client |
| Agent | <home-agent-host> | v1 agent on a home ISP, behind home NAT |
| Signaling + relay | Hetzner, Ashburn VA | v1 signaling server; real Let's Encrypt cert |

Implication: direct is a ~15-mile path; the relay detours ~200 miles.

## E10. First field test (old campus) — 754 MB, SHA-1 verified

Ceilings: campus Wi-Fi **213 Mbps**; home upload **~200–224 Mbps**. Neither saturated.

| run | transport | throughput |
|---|---|---|
| Direct, unpatched | pion SCTP | 49.6 Mbps |
| **Relay** | kernel TCP / Noise | **96 Mbps** |
| Direct, `SB_SCTP_CA_STEP=32768` | pion SCTP | 93.6 Mbps |

- **A direct STUN-hole-punched P2P path formed from campus to home**: `srflx`(<campus-client-ip>) ↔
  `prflx`(<home-public-ip>), RTT 12–24 ms, **no TURN, no relay, no port forwarding**. This was the
  key unmeasured assumption behind the whole WebRTC-direct architecture.
- The relay was ~2× faster than unpatched direct — vindicating v1's `DefaultRelayOnly=true`.
- `cwndCAStep` alone closed the gap: 49.6 → 93.6 Mbps (1.89×), with no fork.
- Host networking advertises **~70 ICE candidates** (every Docker bridge, Tailscale, IPv6) when
  two matter — a real risk against the 10 s direct-connect timeout.

## E11. Second field test — 4-config A/B (new campus)

Ceilings: campus Wi-Fi **562 / 411 Mbps**. One persistent session, one agent at a time, swapped by
container recreation. All Direct mode, all SHA-1 verified, n=1 per cell.

| id | config | throughput | vs unpatched |
|---|---|---|---|
| u | unpatched | 40.0 Mbps | 1.00× |
| s | fork (`ssthresh=256K`) only | 67.2 Mbps | 1.68× |
| f | fork + CA step | 70.4 Mbps | 1.76× |
| c | CA step only (**no fork**) | 75.2 Mbps | 1.88× |
| — | relay | **262.4 Mbps** | **6.6×** |

- All three interventions help by a similar amount (~1.7–1.9×); **none is clearly better** at
  n=1, and the fork buys nothing measurable over the zero-fork knob.
- **The relay is ~4× faster than the best patched direct path** here. On the *slower* campus the
  patched path reached parity. So the relay's advantage **grows with access-link speed** — kernel
  TCP scales with bandwidth, SCTP does not.
- The CA-step effect replicates: **1.88×** here vs **1.89×** on the old campus.
- Direct connectivity is **intermittent** — some runs connected direct, one fell back to relay.

## E12. Bugs found (see `2026-09-15-v1-transport-bugs.md`)

1. **Relay fallback structurally unreachable** — `RELAY_PENDING_WAIT_WINDOW` default 7 s
   (`config.go:35`) vs `DEFAULT_DIRECT_TIMEOUT_MS` 10 s (`connectTransferChannel.js:2`). Whenever
   direct fails the client loops forever. Verified fix: 45 s window.
2. **One agent connection per API key** (`hub.go:20`) — silent takeover. Also present in
   v2/phase-4a at `control/internal/hub/hub.go:20`.
3. **Agent daemon exits on signaling disconnect** (`daemon.go:328`) instead of reconnecting.

---

# Headline conclusions

1. **The direct P2P premise holds** — STUN hole-punching works from a campus network to a home
   server with no TURN and no port forwarding, at 12–24 ms RTT.
2. **The dominant pathology is the congestion-avoidance ramp**, not the ssthresh overshoot it was
   originally blamed on: `+1 MTU/RTT` needs ~66 s to reach an 800 KB BDP at 100 ms RTT. This
   explains almost every "collapse" seen early on, and it means **short-payload benchmarks
   systematically mislead**.
3. **v1's relay default is the right call.** Kernel TCP beats pion SCTP by 2× (slow campus) to
   6.6× (fast campus), and the gap widens with link speed.
4. **The recommended cheap fix is the no-fork one**: `SettingEngine.SetSCTPCwndCAStep(32768)`,
   one public API call, worth ~1.9× on the direct path. The ssthresh fork adds nothing measurable
   in the field and only helps small payloads in the lab.
5. **Multi-connection is orthogonal and irreplaceable** — RFC 8831 gives one congestion window per
   association, so N PeerConnections = N windows. It fixed the RTT tail (p10 6.6×) where no
   window-tuning could.
6. **A real congestion controller remains unbuilt.** BBR-lite made things worse because pion does
   not pace; closing the last gap means adding a pacer plus a controller to pion's send loop.
