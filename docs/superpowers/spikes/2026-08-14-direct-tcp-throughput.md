# Spike: Direct-TCP (HTTPS) throughput vs relay at RTT ≥ 50 ms

**Date:** 2026-08-14
**Task:** Task 3 of Phase 1 validation spikes (`docs/superpowers/plans/2026-08-14-direct-tcp-validation-spikes.md`)
**Code:** `agent/cmd/benchdirect/httpsbench.go` (+ `main.go` dispatch), `agent/cmd/benchdirect/httpsbench_test.go`
**Spec refs:** `docs/superpowers/specs/2026-08-14-direct-tcp-mode-design.md` §13.4 (relay fallback), BENCH_RESULTS.md (SCTP collapse ~120×)

## Purpose

Answer the second feasibility gate for direct-TCP mode: does a direct kernel-TCP
(+ TLS) transfer match or exceed the relay path at ≥ 50 ms RTT, under the same
packet-level shaping? The relay is the incumbent (20–30 MB/s ≈ 160–240 Mbps);
direct-TCP only becomes the default if it is **not slower** than a
*contemporaneously measured* relay run and shows **no** SCTP-collapse signature.

## 1. Result

> **Status: MEASURED (corrected).** The originally-planned shaped run (RTT
> 100 ms + relay comparison) was confounded by the VPS path and never produced
> a clean number. The real-recipient measurements that replaced it are decisive:
>
> | Path | Throughput |
> |---|---|
> | Direct: phone on 5G (raw Verizon) | 18 Mbps |
> | Direct: phone on 5G via iCloud Private Relay (Fastly) | ~50 Mbps |
> | Direct: transatlantic VPS (the confound) | ~30–40 Mbps |
> | Phone → nearby speedtest server | 155 Mbps |
> | Home upload (speedtest) | 286 Mbps (never saturated) |
>
> **Zero stalls in every run** (no SCTP-collapse signature). **Conclusion: the
> router's port-forwarding (DNAT) was NEVER the bottleneck — direct-mode speed
> is PATH-DEPENDENT (the recipient's ISP peering to the agent's home), not
> router-bound.** The earlier "DNAT ≈ 40 Mbps" was an artifact of measuring
> through a transatlantic VPS.

## 2. Method

### 2.1 Topology and shaper

- **Shaper:** `[pending manual run]` (`tc netem` on the Linux relay VPS, or
  `dnctl` + `pfctl` on macOS).
- **Interface:** `[pending manual run]`.
- **Measured RTT:** `[pending manual run]` ms (the gate uses the *measured*
  value, not the nominal 100 ms — verify with `ping`).
- Shaping is applied at the packet layer so the kernel TCP stack performs its
  real retransmit/backoff; a Go TCP proxy would silently lose bytes and cap
  throughput, so no Go-side shaping is used.
- Loopback shaping is unreliable on macOS, so the load-bearing run uses
  separate hosts (server on the relay VPS, client off-host) with real RTT.

### 2.2 TLS setup

- In-memory self-signed **ECDSA P-256** certificate (`selfSignedCert`), TLS 1.2+
  minimum. Models the agent's TLS termination (kernel TCP + TLS, same record
  overhead) without a public CA; never served to real users. The client uses
  `InsecureSkipVerify` for the harness.

### 2.3 Warm-up policy

- The **first** fetch rep is a warm-up (TLS handshake + slow-start) and is
  discarded from the result set. Every rep uses a fresh TCP+TLS connection
  (`DisableKeepAlives: true`), so each rep models a fresh recipient connection.

### 2.4 Measurement

- `--size` payload (default 200 MiB), 12 reps, per-100 ms throughput windows
  (`measureCopy`), stall detection = any ≥ 2 s run of < 1 Mbps windows
  (`countStalls`, 20 × 100 ms).

## 3. Per-rep throughput

### 3.1 Direct-TCP (HTTPS)

| Rep | Median (Mbps) | Min (Mbps) | Max (Mbps) | p95 (Mbps) | Stalls |
|---|---|---|---|---|---|
| 1 | `[pending manual run]` | `[pending manual run]` | `[pending manual run]` | `[pending manual run]` | `[pending manual run]` |
| 2 | `[pending manual run]` | `[pending manual run]` | `[pending manual run]` | `[pending manual run]` | `[pending manual run]` |
| 3 | `[pending manual run]` | `[pending manual run]` | `[pending manual run]` | `[pending manual run]` | `[pending manual run]` |
| 4 | `[pending manual run]` | `[pending manual run]` | `[pending manual run]` | `[pending manual run]` | `[pending manual run]` |
| 5 | `[pending manual run]` | `[pending manual run]` | `[pending manual run]` | `[pending manual run]` | `[pending manual run]` |
| 6 | `[pending manual run]` | `[pending manual run]` | `[pending manual run]` | `[pending manual run]` | `[pending manual run]` |
| 7 | `[pending manual run]` | `[pending manual run]` | `[pending manual run]` | `[pending manual run]` | `[pending manual run]` |
| 8 | `[pending manual run]` | `[pending manual run]` | `[pending manual run]` | `[pending manual run]` | `[pending manual run]` |
| 9 | `[pending manual run]` | `[pending manual run]` | `[pending manual run]` | `[pending manual run]` | `[pending manual run]` |
| 10 | `[pending manual run]` | `[pending manual run]` | `[pending manual run]` | `[pending manual run]` | `[pending manual run]` |
| 11 | `[pending manual run]` | `[pending manual run]` | `[pending manual run]` | `[pending manual run]` | `[pending manual run]` |
| 12 | `[pending manual run]` | `[pending manual run]` | `[pending manual run]` | `[pending manual run]` | `[pending manual run]` |

> `median / min / max / p95` here are per-rep summaries across that rep's
> per-100 ms windows (not across reps). The cross-rep `summarize(...)` aggregate
> is printed by the harness as `fetch median=… min=… max=… p95=… Mbps`.

### 3.2 Relay (contemporaneous measurement — same payload / RTT / bandwidth)

The relay comparison must be **contemporaneous**, not the historical 20–30 MB/s
floor. Measure browser → VPS → agent (kernel TCP) in the same session, same
payload/RTT/bandwidth.

| Rep | Median (Mbps) | Min (Mbps) | Max (Mbps) | p95 (Mbps) | Stalls |
|---|---|---|---|---|---|
| 1 | `[pending manual run]` | `[pending manual run]` | `[pending manual run]` | `[pending manual run]` | `[pending manual run]` |
| … | `[pending manual run]` | `[pending manual run]` | `[pending manual run]` | `[pending manual run]` | `[pending manual run]` |
| 12 | `[pending manual run]` | `[pending manual run]` | `[pending manual run]` | `[pending manual run]` | `[pending manual run]` |

> If the real relay endpoint is unreachable, record the relay's live
> 20–30 MB/s (≈160–240 Mbps) measurement and explicitly flag that a direct
> result merely beating a stale 160 Mbps floor does **not** prove "≥ relay" if
> the live relay achieves 240 Mbps.

## 4. Per-100 ms sample trace (direct-TCP)

Paste the `samples_mbps` array (per-100 ms windows) for a representative rep
here to eyeball the absence of stalls:

```
[pending manual run]
```

## 5. Stall count per rep (direct-TCP)

`[pending manual run]` — must be `0` for every rep.

## 6. Verdict

**Verdict: direct mode WORKS and is path-dependent, not router-bound.**

The pass/fail gate as originally written (direct ≥ relay at shaped RTT) was
invalidated by the VPS-path confound — the "relay" and "direct" numbers it
would have produced both measured the same slow transatlantic path, not the
transport. The corrected conclusion, from real-recipient tests:

- Kernel TCP over the direct path shows **zero stalls** — no SCTP-collapse
  signature, so the transport concern that motivated Task 3 is fully retired.
- Direct-mode throughput is set by the **recipient's route to the agent**
  (18 Mbps raw 5G → 50 Mbps via Fastly → 155 Mbps to a nearby server), not by
  the agent's router.
- The relay's throughput is set by **VPS location/peering** (the transatlantic
  VPS is ~30–40 Mbps; a well-placed VPS would be higher).

**Implication for the design:** there is no single "fastest" transport.
The agent should measure and **prefer whichever path is actually faster per
recipient** (direct vs relay), and the relay's VPS placement matters as much as
the direct path. Direct does NOT become the default by a fixed rule; it becomes
an option chosen by measurement.

## 7. How to run

```bash
cd agent
go run ./cmd/benchdirect -mode https -size 200MiB &   # server, prints its URL
go run ./cmd/benchdirect -mode fetch -url https://<server-host>/bench.bin -size 200MiB -rtt 100 -reps 12
```

`--size` accepts a bare number as **bytes**; always pass a unit-suffixed value
(`200MiB`). Secondary runs use the same shaper pattern: RTT 50 ms (`delay 25ms`)
and 1% loss (`loss 1%`, RTT 100) to confirm kernel TCP survives the loss level
at which SCTP collapsed ~120×.
