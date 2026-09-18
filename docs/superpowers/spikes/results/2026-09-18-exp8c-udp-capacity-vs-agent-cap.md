# Experiment 8c — UDP capacity versus agent cap — 2026-09-18

**Verdict:** The approximately 100 Mbps WebRTC aggregate is **not explained by the path's UDP capacity**. Both paths received about **246 Mbps** from one 250 Mbps UDP flow with **<1% loss**, and four UDP flows plateaued at the same approximately 246 Mbps only when offered 400–952 Mbps. The earlier 300→243 Mbps at 18% loss is consistent with a roughly 246 Mbps path ceiling, not a 100 Mbps ceiling. E8b's **software/transport-side** conclusion therefore stands (though this experiment does not distinguish SCTP/WebRTC behavior from an agent implementation bottleneck).

**Setup:** Field rig only. Agent was the permitted `sb-run` container on VERSA (`sb-agent:pristine`, UI 7879, `SB_SCTP_CA_STEP=32768`, host networking); production and the stopped v2 stack/test agent were not touched. Signalling health at TESTBOX was HTTP 200. Both CLIENT-EAST and CLIENT-WEST were reachable and had no unexpected `node`, `chrome`, `Xvfb`, or `iperf3` process before each cell. UDP direction was VERSA `iperf3 -c` → client `iperf3 -s`, matching the ShareBridge download direction. iperf3 3.18, UDP block size 1448 B, 15 s. WebRTC payload was one 754 MiB file = 6,325.01 Mb per session; click-only `drive_click.js`; metric was agent timestamps from first `DataChannel lanes ready` to last `download complete`.

## Raw UDP loss-versus-rate curves

All rows are one 15 s run (`n=1`). “Sent” and “received” are iperf3's sender and receiver summaries. Packets are receiver `lost/total`.

### CLIENT-EAST, one UDP flow

| Offered Mbps | Sent Mbps | Received Mbps | Loss % | Jitter ms | Packets lost/total |
|---:|---:|---:|---:|---:|---:|
| 50 | 49.998 | 49.965 | 0.000 | 0.342 | 0 / 64,747 |
| 100 | 99.997 | 99.695 | 0.217 | 0.179 | 281 / 129,484 |
| 150 | 149.998 | 149.202 | 0.461 | 0.089 | 895 / 194,235 |
| 200 | 199.997 | 198.502 | 0.653 | 0.077 | 1,692 / 258,996 |
| 250 | 249.998 | 246.608 | 0.900 | 0.040 | 2,915 / 323,711 |

### CLIENT-WEST, one UDP flow

| Offered Mbps | Sent Mbps | Received Mbps | Loss % | Jitter ms | Packets lost/total |
|---:|---:|---:|---:|---:|---:|
| 50 | 49.998 | 49.752 | 0.032 | 0.342 | 21 / 64,745 |
| 100 | 99.999 | 99.310 | 0.230 | 0.164 | 298 / 129,496 |
| 150 | 149.998 | 148.633 | 0.451 | 0.117 | 877 / 194,247 |
| 200 | 199.994 | 197.670 | 0.708 | 0.073 | 1,834 / 258,984 |
| 250 | 249.996 | 245.790 | 0.842 | 0.046 | 2,725 / 323,744 |

There is no loss cliff at 100 Mbps. Loss rises smoothly and stays below 1% through 250 Mbps offered. Under a pragmatic “<1% loss” definition, clean UDP capacity is at least the top tested single-flow rate, with approximately 246 Mbps delivered.

### Four parallel UDP flows

iperf3 applies `-b` **per stream**. Thus the requested literal `-b 100M -P 4` offers 400 Mbps total and `-b 250M -P 4` attempts 1,000 Mbps total. The sender actually reached about 950 Mbps in the latter rows.

| Host | Per-flow target Mbps | Flows | Aggregate sent Mbps | Aggregate received Mbps | Loss % | Jitter ms | Packets lost/total |
|---|---:|---:|---:|---:|---:|---:|---:|
| CLIENT-EAST | 100 | 4 | 399.997 | 246.643 | 38.043 | 0.062 | 197,052 / 517,967 |
| CLIENT-EAST | 250 | 4 | 950.275 | 246.760 | 73.899 | 0.345 | 909,464 / 1,230,563 |
| CLIENT-WEST | 100 | 4 | 399.997 | 245.851 | 38.009 | 0.079 | 196,883 / 517,981 |
| CLIENT-WEST | 250 | 4 | 951.508 | 245.715 | 73.939 | 0.326 | 911,069 / 1,232,023 |

The four-flow runs expose a common approximately 246 Mbps receive ceiling. Their loss is almost exactly the excess offered load being discarded. Four flows do not lift that common ceiling, but the ceiling is about 2.5× the disputed WebRTC aggregate.

For completeness, I also held the **aggregate** four-flow offer at 100/250 Mbps by using 25/62.5 Mbps per stream:

| Host | Aggregate target Mbps | Flows | Received Mbps | Loss % | Jitter ms |
|---|---:|---:|---:|---:|---:|
| CLIENT-EAST | 100 | 4 | 99.696 | 0.234 | 0.376 |
| CLIENT-EAST | 250 | 4 | 246.536 | 0.902 | 0.090 |
| CLIENT-WEST | 100 | 4 | 99.307 | 0.232 | 0.387 |
| CLIENT-WEST | 250 | 4 | 245.788 | 0.840 | 0.101 |

These match the one-flow rows to within 0.1 Mbps and 0.01 percentage point loss at 250 Mbps.

## Same-time WebRTC cells

The CLIENT-EAST N=1 cell began about one minute after the UDP ladder, so this is the requested same-condition comparison. Each field cell has `n=1`.

| Cell | Agent window (UTC) | Window s | Completed | Aggregate Mbps | Notes |
|---|---|---:|---:|---:|---|
| CLIENT-EAST N=1 | 06:55:55.323 → 06:56:47.082 | 51.759 | 1/1 | **122.2** | Same-time baseline; higher than the earlier 97.3 baseline. |
| 1 EAST + 1 WEST | 06:57:48.861 → 07:00:20.314 | 151.454 | 2/2 | **83.5** | First session completed after 61.530 s; last after 150.155 s from its own lanes-ready. No mid-window close. |
| 2 EAST + 2 WEST, attempt 1 | 07:01:00.728 onward | — | 0/4 | — | **Aborted, not a measurement:** only 3/4 lanes became ready; one driver logged an SDP `Called in wrong state: stable` error. All client processes were cleaned and all three peers were allowed to close before retry. |
| 2 EAST + 2 WEST, valid retry | 07:03:29.203 → 07:07:55.191 | 265.988 | 4/4 | **95.1** | Four lanes ready within 3.853 s; completions at 07:05:40, 07:05:48, 07:07:48, 07:07:55; no mid-window close. |

The current N=1 result itself exceeds the supposed hard 100 Mbps cap, showing that the WebRTC value is time-varying. More importantly, even the valid four-session aggregate used only 39% of the approximately 246 Mbps UDP receive ceiling.

## Agent CPU and per-thread evidence

VERSA exposes 6 logical CPUs. `sb-run` had no CPU quota or cpuset restriction.

**Percentage definitions:** Linux Docker `CPU %` uses **100% = one logical CPU**, not 100% of the whole six-CPU host. Thus 156.94% means 1.569 core-equivalents, or 26.2% of total host CPU capacity. `top -H` similarly uses 100% for one thread occupying one logical CPU. `ps -eLo ... %CPU` is a lifetime average since the process/thread started; it is included as requested but is not a live interval measurement.

| Cell/phase | Live peers | `docker stats` CPU | Instantaneous `top -H` evidence | Client vmstat idle |
|---|---:|---:|---|---|
| 2-session at 06:58:25–35 | 2 | 121.86%, then 90.01% | No pinned thread: peak 20%; five agent threads at 20% plus two at 10% in one snapshot | EAST 34%, WEST 52% |
| 2-session after first completed | 1 | 39.09% | Peak 20%; two threads at 20% and two at 10% | — |
| valid 4-session at 07:04:11 | 4 | 126.30% | Peak 20%; work spread across at least eight threads | EAST 6% (plus 1% wait), WEST 56% |
| valid 4-session at 07:04:37–48 | 4 | 156.94% | Second 1 s `top` interval: 18, 17, 17, 17, 15, 13, 13, 5, 4% across nine threads (119% summed); no thread near 100% | — |
| valid 4-session at 07:05:25 | 4 | 147.58% | — | — |
| after prior cell/peer cleanup | 0 | 4.22–4.80% | idle-ish | — |

Requested host commands also showed the container process as host PID 2013621 and `ps -eLo` process lifetime average around 25–26%; individual long-lived agent threads averaged approximately 1–3% over the container's multi-hour lifetime. Those lifetime averages must not be compared with the 1 s `top` or Docker deltas.

**Reconciliation of prior 81.6/165.2 versus 1.53:** E8b's 81.6% (2 peers) and 165.2% (4 peers) are the same scale and phase as this experiment's live 90–122% and 126–157%, so E8b is reproducible. Experiment 9's 1.53% was not a valid live-cell Docker percentage under this workload. It was likely sampled outside the busy interval and/or divided by six and reported as whole-host percent. For example, my immediately post-completion N=1 sample was 7.57% in Docker units, which would be 1.26% of total six-core capacity if divided by six. The CPU is **spread**, not one goroutine/OS thread pinned at approximately 100%; nevertheless, the agent consumes roughly 0.9–1.6 cores during these cells, so CPU/locking/scheduling inside the agent/transport remains plausible.

## Exact commands (sanitized host placeholders required by the safety rules)

Every SSH invocation used the required options:

```bash
SSH_OPTS='-o ConnectTimeout=10 -o ServerAliveInterval=5 -o ServerAliveCountMax=3'

# Health and pre-cell interference checks
ssh $SSH_OPTS VERSA 'docker ps --format "{{.Names}} {{.Image}} {{.Ports}}" | grep -E "^(sb-run|sharebridge-agent|sharebridge-agent-test) "'
curl --connect-timeout 10 --max-time 20 -s -o /dev/null -w '%{http_code}\n' https://TESTBOX/
ssh $SSH_OPTS CLIENT-X 'ps -eo pid,comm,args | grep -E "(node|chrome|Xvfb|iperf3)" | grep -v grep || true'

# UDP server; launched alone, then polled separately
ssh $SSH_OPTS CLIENT-X 'cd ~/sbtest && setsid nohup iperf3 -s > /tmp/exp8c-iperf-server.log 2>&1 < /dev/null & echo started'

# One-flow ladder
ssh $SSH_OPTS VERSA 'iperf3 -c CLIENT-X -u -b 50M  -t 15 -J'
ssh $SSH_OPTS VERSA 'iperf3 -c CLIENT-X -u -b 100M -t 15 -J'
ssh $SSH_OPTS VERSA 'iperf3 -c CLIENT-X -u -b 150M -t 15 -J'
ssh $SSH_OPTS VERSA 'iperf3 -c CLIENT-X -u -b 200M -t 15 -J'
ssh $SSH_OPTS VERSA 'iperf3 -c CLIENT-X -u -b 250M -t 15 -J'

# Literal four-flow requested controls (rate is per stream)
ssh $SSH_OPTS VERSA 'iperf3 -c CLIENT-X -u -b 100M -P 4 -t 15 -J'
ssh $SSH_OPTS VERSA 'iperf3 -c CLIENT-X -u -b 250M -P 4 -t 15 -J'

# Aggregate-normalized auxiliary controls
ssh $SSH_OPTS VERSA 'iperf3 -c CLIENT-X -u -b 25M   -P 4 -t 15 -J'
ssh $SSH_OPTS VERSA 'iperf3 -c CLIENT-X -u -b 62.5M -P 4 -t 15 -J'

# Click-only field driver; share URL was obtained from the allowed sb-run API and is redacted
ssh $SSH_OPTS CLIENT-X 'cd ~/sbtest && SHARE_URL=https://TESTBOX/s/<REDACTED> NTABS=N setsid nohup xvfb-run -a node drive_click.js > /tmp/exp8c-<cell>.log 2>&1 < /dev/null & echo started'
ssh $SSH_OPTS VERSA 'docker logs --timestamps --since <t0> sb-run 2>&1 | grep -E "lanes ready|download complete|closed"'

# CPU evidence during active cells
ssh $SSH_OPTS VERSA 'docker stats --no-stream sb-run'
ssh $SSH_OPTS VERSA 'docker top sb-run -eo pid,ppid,comm,args'
ssh $SSH_OPTS VERSA 'ps -eLo pid,tid,pcpu,comm --sort=-pcpu | head -20'
ssh $SSH_OPTS VERSA 'pid=$(docker inspect -f "{{.State.Pid}}" sb-run); top -H -b -d 1 -n 2 -p "$pid"'
ssh $SSH_OPTS CLIENT-X 'vmstat 1 2 | tail -1'

# Cleanup (the only field agent was confirmed before use)
ssh $SSH_OPTS CLIENT-X 'pkill -9 -x node || true; pkill -9 -x chrome || true; pkill -9 -x Xvfb || true; pkill -x iperf3 || true'
```

## Interpretation

1. **The earlier 300 Mbps control was misread.** A roughly 246 Mbps bottleneck predicts about 18% loss at 300 Mbps: `(300-246)/300 = 18%`, exactly the earlier observation. The earlier 100 Mbps result only showed that 100 was below the ceiling; it did not locate the ceiling at 100.
2. **The new ladder locates the UDP ceiling near 246 Mbps on both client paths.** One flow reaches it, and four flows do not move it. There is no path policer/shaper at approximately 100 Mbps.
3. **WebRTC is well below available UDP delivery.** Same-time CLIENT-EAST N=1 delivered 122.2 Mbps; two cross-host sessions delivered 83.5 Mbps; four delivered 95.1 Mbps. All are far below approximately 246 Mbps, despite independent UDP flows proving the path can carry that rate.
4. Therefore E8b's control should indeed have used UDP, but doing so **strengthens rather than collapses** its result: the limitation is in the WebRTC/SCTP/agent software stack (possibly its response to small packet loss), not raw UDP path capacity.
5. This supports investigating or replacing v1's transport path. It does not by itself prove v2 native TLS/relay will reach 246 Mbps; that requires a matched v2 field measurement.

## Error bars and caveats

- `n=1` per UDP rate and WebRTC cell due the field timebox. There is no defensible run-to-run confidence interval; displayed precision is measurement precision, not statistical certainty.
- The practical temporal error bar is large: CLIENT-EAST N=1 was 97.3 Mbps earlier and 122.2 Mbps here (range 24.9 Mbps, about 26% of the earlier value). The verdict does not depend on that spread because the UDP receive ceiling is still roughly 2× the faster N=1 and 2.6× the valid N=4 aggregate.
- Agent timing start skew was 1.298 s in the 2-session cell (0.9% of window) and 3.853 s in the 4-session cell (1.4%); the specified first-ready→last-complete metric intentionally includes it.
- UDP iperf is open-loop. Sub-1% packet loss can reduce SCTP congestion-controlled goodput much more than iperf receive rate; that would be a WebRTC/transport response to the path, not evidence of a 100 Mbps raw UDP capacity ceiling.
- During the first failed N=4 attempt VERSA briefly showed an unrelated `unpigz` process at 80% and 40% host I/O wait. The valid retry still had elevated load average and 6–10% I/O wait in early samples. This may depress the N=4 throughput, but cannot create the earlier UDP ladder's approximately 246 Mbps result.
- The optional second-agent experiment was skipped. Safely reproducing the test container's mounts/credentials without ambiguity around the protected production/v2 slots was not justified after steps 1–3 were decisive.

## Artifacts

- Local raw iperf JSON: `/tmp/exp8c-{east,west}-{50,100,150,200,250}-p1.json`, `/tmp/exp8c-{east,west}-{100,250}-p4.json` (aggregate-normalized), `/tmp/exp8c-{east,west}-{100each,250each}-p4.json` (literal per-flow rates).
- Local CPU captures: `/tmp/exp8c-2session-cpu{1,2,3}.txt`, `/tmp/exp8c-2session-cpu-one-remaining.txt`, `/tmp/exp8c-4session-retry-cpu{1,2}.txt`, `/tmp/exp8c-4session-retry-top2.txt`, `/tmp/exp8c-4session-retry-stats3.txt`.
- CLIENT-EAST: `/tmp/exp8c-east-n1.log`, `/tmp/exp8c-2session-east.log`, `/tmp/exp8c-4session-east.log` (failed attempt), `/tmp/exp8c-4session-retry-east.log`.
- CLIENT-WEST: `/tmp/exp8c-2session-west.log`, `/tmp/exp8c-4session-west.log` (failed attempt), `/tmp/exp8c-4session-retry-west.log`.
- Final cleanup process listings were empty on both client VMs. No production/v2 container was changed.
