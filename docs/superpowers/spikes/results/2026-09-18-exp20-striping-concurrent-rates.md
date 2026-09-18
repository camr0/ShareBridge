# Experiment 20 — striping, per-session concurrent rates — 2026-09-18

**Verdict (written by the orchestrator — the agent died on a provider rate limit after cell 4, before writing
this section; cells 1–4 were complete and are unaffected):**
**Both a per-host and a shared component are real, and the SHARED one dominates.** Two tabs on one host add
essentially nothing (**1.07×** its solo), two hosts add only a little more (**1.16×** the best single host), and
in the cross-host cell **both** hosts slowed simultaneously (EAST to 0.79× and WEST to 0.66× of their own solos)
while the concurrent aggregate reached only **75%** of the sum of the two solos. So E8b's "per-client-host cap
FALSIFIED" claim should be **withdrawn** — but no strong per-host claim survives either. Striping is not a
viable v1 performance strategy (consistent with the earlier N-concurrency results).

**Question:** Does v1 direct's ceiling live on the client host or upstream (agent/transport)? E8b
compared a window-average aggregate (89.9 Mbps) against a sum of solo baselines (158.7) and called
the per-host hypothesis falsified — but the correct comparison for that cell is the **concurrent**
rate 88.8 + 45.4 = 134.2 Mbps (1.38x a single EAST host) vs only 1.09x for two tabs on one host.
This experiment re-measures with per-session agent-side timelines and reports, per cell:
(a) per-session Mbps over each session's own lanes-ready→complete window, (b) concurrent aggregate
= sum of per-session rates, (c) window-average aggregate = N × 6325.01 Mb ÷ (first lanes-ready →
last complete).

**Setup:** Field rig. Agent `sb-run` on VERSA (`sb-agent:pristine`, `UI_PORT=7879`,
`SB_SCTP_CA_STEP=32768`, host net; untouched — no recreation). Signalling TESTBOX (v1, Caddy TLS,
HTTP 200). Share = one 754 MiB file = 790,626,304 B = 6,325.01 Mb per download; direct mode with
relay standby. Drivers: `drive_click.js` per VM, click-only, no Playwright Download API (verified by
grep). Metric: agent-side `docker logs --timestamps sb-run` only. All times UTC. Payload count per
cell recorded via share `downloads` counter deltas.

**Health (20:34–20:35Z):** `sb-run` up (sb-agent:pristine, env verified via inspect), `sharebridge-agent-test`
exited, TESTBOX HTTP 200, CLIENT-EAST + CLIENT-WEST reachable, no node/chrome/Xvfb on either VM.
Prior agent's last log line 20:31:36Z (peer closed); log quiet ≥3 min before my first cell.
KEEPALIVE_MS driver default is 30 min → every cell ends with `pkill -x node; pkill -x chrome;
pkill -x Xvfb` on the driven VM(s).

**Mode assertion per cell (rule):** every session must show `DataChannel lanes ready` AND an unused
relay standby (relay channel closed with `pending wait window exceeded` and no relay data lines).
A relayed cell is not a measurement.

**Plan:** (1) EAST solo → (2) WEST solo → (3) WEST N=2 → (4) cross 1+1 → (5) EAST solo control
(variance) → (6) cross 2+2 if time allows. n=1 per cell.

## Raw cells

| # | cell | window (UTC, first lanes → last complete) | per-session windows s | (a) per-session Mbps | (b) concurrent Σ | (c) window-avg Mbps | EAST idle% | WEST idle% | sb-run CPU | relay | notes |
|---|---|---|---|---|---|---|---|---|---|---|---|
| 1 | EAST solo N=1 | 20:36:15.224 → 20:37:06.975 | 51.752 | **122.22** | **122.22** | **122.22** | 21 (us35/sy44) | n/a | 84.1% | standby unused (closed +44 s) | count 61; click 20:36:15.385Z; benign `join() re-entered` PAGEERROR |
| 2 | WEST solo N=1 | 20:38:13.669 → 20:39:47.057 | 93.388 | **67.73** | **67.73** | **67.73** | n/a | 33 (us27/sy39) | 116.1% | standby unused (closed +44 s) | count 62; click 20:38:14.137Z; benign `join() re-entered` PAGEERROR |
| 3 | WEST N=2 (one host) | 20:40:43.574 → 20:43:42.959 | 173.044 / 176.901 | **36.55 / 35.75** | **72.30** | **70.52** | n/a | 18 (us30/sy51) | 101.1% | both standbys unused (+44 s each) | counts 63, 64; tab clicks 20:40:46.513/.531 (18 ms skew); lanes skew 2.48 s |
| 4 | cross-host 1+1 | 20:45:02.354 → 20:47:27.951 | 65.193 (E) / 141.712 (W) | **97.03 (E) / 44.63 (W)** | **141.66** | **86.88** | 39 (us25/sy36) | 80 (us10/sy10, early) | 76.8% | both standbys unused (+44 s each) | counts 65, 66; clicks 20:45:02.499 E / 20:45:06.670 W (4.2 s skew); peers b873365e=EAST, 920ee180=WEST (timing-mapped) |

### Cell 1 detail — EAST solo N=1 (appended 20:38Z)

- Pre-cell: both VMs clean, agent log quiet ≥3 min. Driver launched 20:36:07Z (ssh channel
  lingered 20 s — known hazard; launch verified via separate call before proceeding).
- Agent: lanes ready 20:36:15.223648895Z (peer c7afda95…); download complete 20:37:06.975168222Z
  (count 61). Window 51.751523 s → 6325.01 ÷ 51.751523 = **122.219 Mbps**.
- Mode: **direct asserted** — relay standby closed unused (`pending wait window exceeded`, +44 s
  after join), no relay data lines.
- Mid-transfer (~t+20 s): EAST vmstat us 35 / sy 44 / **id 21**; `sb-run` **84.13% CPU**, 40.18 MiB.
- Cleanup: pkill -x node/chrome/Xvfb; node survived SIGTERM 2 s → SIGKILLed by attributable PID;
  VM verified empty.

### Cell 2 detail — WEST solo N=1 (appended 20:41Z)

- Pre-cell: both VMs clean, agent log showing only Cell 1's attributable peer close (20:37:59).
- Agent: lanes ready 20:38:13.668910622Z (peer 5b71e8ea…); download complete 20:39:47.056929339Z
  (count 62). Window 93.388018 s → **67.725 Mbps**.
- Mode: **direct asserted** — relay standby closed unused (`pending wait window exceeded`, +44 s),
  no relay data lines.
- Mid-transfer (~t+35 s): WEST vmstat us 27 / sy 39 / **id 33**; `sb-run` **116.05% CPU**, 43.61 MiB
  (higher than Cell 1's 84% — sampling phase varies; docker stats is of all host cores).
- Cleanup: pkill then SIGKILL by attributable PIDs; VM verified empty.

### Cell 3 detail — WEST N=2, one host (appended 20:45Z)

- Pre-cell: both VMs clean. Both tabs joined (tab0 20:40:44.005, tab1 20:40:46.495); clicks
  20:40:46.513 / 20:40:46.531 (18 ms apart). Both had the benign `join() re-entered` PAGEERROR.
- Agent: lanes ready 20:40:43.574187295Z (peer 8db00497…) and 20:40:46.057982064Z (peer 3d70c2b5…);
  download complete 20:43:36.617727241Z (count 63) and 20:43:42.959085610Z (count 64).
- Per-session windows (order-paired; pairing ambiguity ≤ lanes skew 2.48 s):
  173.043507 s → **36.549 Mbps**; 176.901036 s → **35.753 Mbps**.
- (b) concurrent Σ = **72.30 Mbps** = **1.068× WEST solo (67.73)** this session.
- (c) window-average = 2×6325.01 ÷ 179.384779 = **70.52 Mbps**.
- Mode: **direct asserted for both sessions** — both relay standbys closed unused
  (`pending wait window exceeded` at 20:41:27 / 20:41:29), no relay data lines.
- Mid-transfer (~t+65 s): WEST vmstat us 30 / sy 51 / **id 18**; `sb-run` **101.10% CPU**, 68.81 MiB.
- Cleanup: pkill + SIGKILL attributable node PID; VM verified empty.

### Cell 4 detail — cross-host 1+1 (appended 20:49Z)

- Pre-cell: both VMs clean, agent log showing only Cell 3's completes. Launched with `ssh -f`
  (EAST 20:44:59Z, WEST 20:45:02Z). Clicks: EAST 20:45:02.499Z, WEST 20:45:06.670Z (4.2 s skew).
- Agent: lanes ready 20:45:02.353913675Z (peer b873365e…) and 20:45:06.239912029Z
  (peer 920ee180…); download complete 20:46:07.546782609Z (count 65) and
  20:47:27.951321446Z (count 66).
- Peer→host mapping: b873365e = EAST (lanes-ready coincides with EAST's click, 4 s before WEST
  even joined; completion speed 65 s is EAST-class), 920ee180 = WEST (lanes at WEST's click;
  141.7 s is WEST-class). Staged-cleanup close confirmation not yet in log at write time (peer
  closes lag pkills by ~90 s; will append if seen).
- Per-session: EAST 65.192634 s → **97.03 Mbps** (solo today 122.22 → 0.79×);
  WEST 141.711917 s → **44.63 Mbps** (solo today 67.73 → 0.66×).
- (b) concurrent Σ = **141.66 Mbps** = **1.159× EAST-solo** (122.22) and **0.746× the solo-sum**
  (189.95).
- (c) window-average = 2×6325.01 ÷ 145.598 = **86.88 Mbps**.
- Mode: **direct asserted for both** — both relay standbys closed unused (20:45:46 / 20:45:50),
  no relay data lines.
- Mid-transfer (~t+25 s): EAST vmstat us 25 / sy 36 / **id 39**; WEST us 10 / sy 10 / **id 80**
  (early window; WEST ramps slower at 71 ms RTT); `sb-run` **76.84% CPU**, 71.24 MiB.
- Cleanup: pkill + SIGKILL attributable node PIDs both VMs; verified empty.

## Interpretation (orchestrator, from cells 1–4)

All four cells asserted **direct** mode (relay standbys closed unused, no relay data). Baselines were measured
**in the same session**, which matters because the day's rates run high (EAST solo 122.22 here vs 102–111 in the
earlier session; WEST 67.73 vs 60.2).

| cell | per-session Mbps | concurrent Σ | window-avg | vs best single host | vs sum of solos |
|---|---|---|---|---|---|
| 1. EAST solo | 122.22 | 122.22 | 122.22 | 1.00× | — |
| 2. WEST solo | 67.73 | 67.73 | 67.73 | — | — |
| 3. WEST N=2 (one host) | 36.55 / 35.75 | **72.30** | 70.52 | 1.07× (of WEST solo) | 1.07× |
| 4. cross-host 1+1 | 97.03 (E) / 44.63 (W) | **141.66** | 86.88 | **1.159×** (of EAST solo) | **0.746×** |

1. **The per-host component is real but small.** Two tabs on one host: 72.30 Mbps = **1.07×** that host's solo,
   with each tab at ~54% of solo — heavy intra-host contention. Two hosts: 141.66 = **1.16×** the best single
   host. The ordering (cross-host > same-host) is what a per-host term predicts, and it is consistent with E15's
   per-host app sink being real.
2. **The shared component dominates.** In the cross-host cell both hosts fell below their own solos (EAST 0.79×,
   WEST 0.66×) and the aggregate was only **75%** of the sum — the ceiling does not move to the clients.
3. **E8b's headline must be withdrawn.** Its "falsified" claim compared a window-average (89.9) against a sum of
   per-session rates (158.7). The correct concurrent figures here are 1.07× (one host) and 1.16× (two hosts),
   i.e. per-host effects are present; but no strong per-host claim is supported either. The honest statement is
   **"a modest per-host component plus a dominant shared upstream cap"**.
4. **Nothing here identifies the shared cap's mechanism.** Agent CPU was 84.1% (EAST solo, 122 Mbps), 116.1%
   (WEST solo, 68 Mbps) and 76.8% (cross-host 1+1, 141.66 Mbps) — single `docker stats` samples, mutually
   inconsistent as a CPU-bound story, and E8c already showed the load spread over 8–9 threads with no pinned
   thread. Treat CPU as unresolved.
5. **Practical conclusion:** striping gains are 1.07–1.16×, so it is not a performance strategy for v1 — and
   the ceiling it fails to move is the same loss-independent, upstream limit identified in E19/F10.

**Caveats:** n=1 per cell; cell 5 (EAST solo control) was never reached, so the session's own variance is
bounded only by the two solos (122.22 and 67.73) plus the earlier session's range; the 1.16× cross-host figure
rests on a single cell with a 4.2 s start skew and a 2×6325.01/145.598 s window average; peer→host mapping in
cell 4 was inferred from lanes-ready timing and completion speed, not from a per-peer label.
