# Experiment 8b — cross-host striping (field) — 2026-09-18

**Verdict:** The per-client-host ceiling hypothesis is **falsified**. With 1 tab on each of two client hosts, aggregate was **89.9 Mbps** — far below the 158.7 Mbps sum of the two solo baselines, and *both* hosts ran slower simultaneously than they do alone. The cap is **shared upstream of the clients** (agent and/or transport), and its magnitude (~90–100 Mbps) coincides with the single-TCP-flow capacity of the path (98.6 Mbps) while the path itself carries 240 Mbps over 4 TCP flows.

**Setup:** Field rig. Agent = `sb-run` on VERSA (`sb-agent:pristine`, `UI_PORT=7879`, `SB_SCTP_CA_STEP=32768`, host net). Signalling = TESTBOX (v1, Caddy TLS, HTTP 200 at health check). Share on one 754 MiB file = 6,325.01 Mb; fresh share used (48 h, `max_downloads=500`, 15 downloads used pre-experiment; the previous share `i4b81ri4` expired 06:31:55Z mid-session). Drivers: `drive_click.js` (click-only, no Playwright Download API — verified by grep on both VMs), 1 tab per host (Cell X) then 2 tabs per host (Cell Y), started within ~2 s of each other. Metric: agent-side `docker logs --timestamps` only; window = first `DataChannel lanes ready` → last `download complete`. All times UTC (agent clock; this Mac is UTC−4).

## Concurrency-guard audit (per orchestrator addendum)

- Pre-attempt-1: `ps` listings on both VMs empty (no node/chrome/Xvfb); `docker logs --since 3m sb-run` tail empty → proceeded.
- Pre-Cell-X-retry and pre-Cell-Y: listings empty after cleanup; agent log silent in the prior 1–3 min → proceeded. Orphaned `node` PIDs from the aborted attempt (4727 CLIENT-EAST / 8590 CLIENT-WEST, attributable by PID+etime) were SIGKILLed before the retry; `pkill -x node` (SIGTERM) alone left them alive >2 s twice.
- No `download complete` advanced without a driver of mine at any point; no foreign processes seen all session.

## Raw

| cell | config | n | window (UTC) | window s | sessions done | aggregate Mbps | notes |
|---|---|---|---|---|---|---|---|
| baselines (prior, same payload/tuning) | CLIENT-WEST N=1 alone / CLIENT-EAST N=1 alone | 1 each | — | 103 / 65 | 1/1 each | 61.4 / 97.3 | reference |
| X-attempt-1 | 1 tab EAST + 1 tab WEST | 1 | lanes 06:31:38, no completes | — | 0/2 | — | **interference-aborted (orchestrator-caused), NOT a measurement**: operator's cleanup of the stale prior agent pkill'd both client browsers at 06:32:42 (~64 s in). Also showed relay `pending wait window exceeded` closes at 06:32:22 (45 s after join) and a `join() re-entered` PAGEERROR on both tabs. |
| **X (valid)** | 1 tab EAST + 1 tab WEST | 1 | 06:35:05.829 → 06:37:26.547 | 140.72 | **2/2** | **89.9** | EAST 71.20 s → 88.8 Mbps (solo 97.3); WEST 139.19 s → 45.4 Mbps (solo 61.4). No `closed` in window. |
| **Y (valid)** | 2 tabs EAST + 2 tabs WEST | 1 | 06:39:14.330 → 06:43:29.608 | 255.28 | **4/4** | **99.1** | EAST pair done 06:41:21/06:41:27 (133.4 s → 94.8 Mbps host rate); WEST pair 06:43:25/06:43:29 (254.2 s → 49.8 Mbps). No mid-transfer `closed`; the four `closed` at 06:45:09–20 are my own cleanup pkills. |
| iperf3 1 flow | VERSA→CLIENT-WEST | 1 | ~06:44 | 10 s | — | **98.6** sender / 95.7 recv (1 retr) | control |
| iperf3 4 flows | VERSA→CLIENT-WEST | 1 | ~06:45 | 10 s | — | **240** sender / 231 recv (162 retr) | control; Exp 9 had measured 156 — path capacity is time-varying and ≥156 |

Agent-side log lines (share code redacted):

```
X:  06:35:05.829 lanes ready peer 14edc444… (EAST)   06:35:07.354 lanes ready peer 0e8c700f… (WEST)
    06:36:17.024 download complete (count: 16)       06:37:26.547 download complete (count: 17)
Y:  06:39:14.330 / 06:39:15.395 / 06:39:15.401 / 06:39:17.874 lanes ready (4 peers)
    06:41:21.338 (18) / 06:41:27.738 (19) / 06:43:25.764 (20) / 06:43:29.608 (21) download complete
    (a full-window grep incl. `relay|error|fail` for Y returned only the 4 completes + post-run closes)
```

CPU/vmstat during transfer (single samples; docker stats % is of all host cores; vmstat idle% is an aggregate across vCPUs and cannot exclude one saturated critical thread):

| cell | agent `sb-run` CPU | CLIENT-EAST idle% | CLIENT-WEST idle% |
|---|---|---|---|
| X (1+1) | 81.6% | 29 (us31/sy40) | 85 (us8/sy8) |
| Y (2+2) | 165.2% | 11 (us32/sy58) | 18 (us33/sy50) |

## Interpretation

1. **Cell X is decisive and lands in the "shared cap" branch (67–100 Mbps), not the 158 branch.** Under a per-client-host cap, each host should have held its solo rate; instead EAST fell 97.3→88.8 and WEST fell 61.4→45.4 *while WEST sat 85% idle*. A host that is mostly idle yet delivers 26% less than solo is being throttled upstream: the bottleneck is shared, located in the agent and/or the WebRTC transport, not the client sink. (EAST at 29% idle and 11% idle in Y is near-busy, so a client-side *contributing* limit at EAST cannot be excluded — but it cannot explain WEST's slowdown.)
2. **The shared ceiling is ~90–100 Mbps and does not widen with hosts or sessions**: 2 tabs on one host = 67 Mbps (Exp 9) → 1+1 cross-host = 89.9 → 2+2 cross-host = 99.1. Adding a second host bought only ~1.3–1.5×, then saturated.
3. **The ceiling coincides with single-TCP-flow capacity (98.6 Mbps) while the path carries 240 Mbps over 4 TCP flows.** So this is not raw path/uplink capacity; the DC transport's aggregate behaves like *one* TCP-flow-sized pipe. For the v1-vs-v2 decision this is the key fact: v1's transport (or the agent's shared send/ACK path) cannot exploit the path's parallel capacity even across independent client hosts — exactly the class of limit a redesigned v2 transport (or a fixed shared send loop) would need to remove.
4. **N=4 session death did NOT reproduce**: 4/4 completed in Cell Y with zero mid-transfer `closed`. No evidence of the `agent/internal/hub.go:20` one-connection-per-API-key takeover tonight. (Exp 9's 1-of-4 early death remains unexplained but did not recur in the only N=4 cell here.)
5. **Agent CPU anomaly to follow up**: I measured 81.6% (2 peers) and 165.2% (4 peers) vs Exp 9's reported 1.53%. Unresolved — possibly different sampling phase/method. If real, a single hot thread (SCTP send / hashing / pacing) at ~1 core is a candidate mechanism for the ~100 Mbps shared ceiling that docker stats cannot isolate.

## Caveats

- n=1 per cell (time-boxed field session); no repeats, no interleaving. Windows include ~2 s cross-host start skew (≤1.5% effect).
- Baselines were measured earlier tonight by a prior agent (same payload, share, tuning); treated as given.
- Single 754 MiB file; single share; UDP/NAT conditions per host unknown beyond RTT (~12 ms EAST, ~71 ms WEST).
- Agent-side metric only; client-side counters not captured (driver is click-only by design).
- `docker stats` NET I/O reads 0 B (host networking) — no wire-rate cross-check from the container.
- The relay `pending wait window exceeded` lines were confirmed only in the aborted attempt's window; for Cell X I did not grep the full log for `relay` lines (Cell Y's full-window grep was clean).
- The `join() re-entered` PAGEERROR appears whenever a tab's join races the signalling socket (seen on tab1 of both hosts in Y, and on both single tabs in the aborted attempt); tabs always still joined/clicked and all sessions completed — apparently benign, but worth a code look.
- Lab/field asymmetry: N/A — all measurements here are field. Nothing here measures the client sink in isolation.

## Artifacts

- CLIENT-EAST: `/tmp/cell-X-east.log` (aborted), `/tmp/cell-X2-east.log` (Cell X), `/tmp/cell-Y-east.log` (Cell Y)
- CLIENT-WEST: `/tmp/cell-X-west.log`, `/tmp/cell-X2-west.log`, `/tmp/cell-Y-west.log`, `/tmp/iperf3-server.log`
- Agent log excerpts quoted above (timestamps UTC, share code redacted); t0 markers 06:31:23Z / 06:34:51Z / 06:39:06Z
- iperf3 client outputs quoted in Raw table
