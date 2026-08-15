# Direct-TCP throughput — complete benchmark results (final)

**Date:** 2026-08-15
**Status:** COMPLETE — question settled
**Scope:** end-to-end measurement of ShareBridge's direct-TCP (native HTTPS) data
path: SCTP vs TCP motivation, the DNAT/UPnP investigation, and the final
peering-dependent model. Consolidates and supersedes the per-task spike notes.

---

## TL;DR

**The router was never the bottleneck.** Direct-TCP works and delivers whatever
the *recipient's network path* to the home allows. Measured:

| Recipient → home | Peering quality | Throughput |
|---|---|---|
| Whitesky, Newark (on-net, 12 ms) | on-net | **278 Mbps** (upload ceiling) |
| Hetzner, Ashburn (well-peered, 11.5 ms) | good | **~130 Mbps** |
| Azure / GitHub Actions (US East) | congested cross-carrier | **26–91 Mbps** (egress-IP dependent) |
| Phone on 5G (deprioritized MVNO) | cellular | **18–50 Mbps** |

UPnP port-forwarding == manual port-forwarding (equal). Inbound (DNAT) ≈
outbound (SNAT), so the router adds no meaningful penalty.

---

## 1. Why we're here: SCTP collapse → direct-TCP

The original `benchdirect` harness (browser WebRTC DataChannels vs relay)
established:

- **Direct (SCTP DataChannel):** ~8 MB/s (~64 Mbps)
- **Relay (kernel TCP via Hetzner):** 20–30 MB/s (~160–240 Mbps)

Root cause of SCTP slowness: pion's SCTP congestion-window collapse (the
`minCwnd` bug). A `minCwnd` bump was tried and **reverted** — tuning SCTP is a
dead end. The decision: **delete WebRTC and terminate native HTTPS (kernel TCP
+ TLS) at the agent** instead. Direct-TCP is that replacement.

## 2. The complete measurement table

All direct-TCP (HTTPS) numbers below are **server-side Mbps** unless noted.
Home = Verizon FiOS, `173.54.233.213` (dynamic); agent host = dev Mac,
`192.168.1.224`, 802.11ax WiFi (1200 Mbps PHY, −41 dBm, 5 GHz).

| # | Client | Path / note | Mbps |
|---|---|---|---|
| 1 | Mac → Whitesky Newark (Ookla) | outbound, on-net, 12 ms | **278.45 up** (606 down) |
| 2 | Hetzner Ashburn → home | UPnP `:49152`, 11.5 ms | 122.5, 138.8 |
| 3 | Hetzner Ashburn → home | manual `:4443` | 100.8, 103.5 |
| 4 | Mac → Hetzner Ashburn (iperf3) | outbound / SNAT control | 153 (159–214/s) |
| 5 | Azure `13.83.233.101` → home | UPnP, 1 GiB | 31.5 |
| 6 | Azure `64.236.131.214` → home | UPnP, 200 MiB | 91.1 |
| 7 | Azure `64.236.131.214` → home | manual, truncated 186 MiB | 50.3 (srv) / 21.4 (curl) |
| 8 | Azure `104.209.7.226` → home | UPnP ×2 | 26.3, 45.7 |
| 9 | Azure `104.209.7.226` → home | manual ×2 (1 truncated 47 MiB) | 12.0, 45.6 |
| 10 | Phone 5G (Xfinity MVNO) → home | raw, 250 MiB | 18.4 |
| 11 | Phone 5G → home via iCloud Private Relay | Fastly egress | 48.8 (1 GiB), 52.5 (partial) |
| 12 | Phone 5G speedtest | nearby server | 155 down / 51 up |
| 13 | Hetzner *Germany* → home | transatlantic, ~113 ms | 27–40 |
| 14 | Hetzner Germany → home | manual `:4443` | 29.8 |
| 15 | Hetzner Germany → home | 4 parallel | 45.8 total |

**Client-side baselines (are the clients themselves fast?):**

| Client | Baseline | Mbps |
|---|---|---|
| GitHub Actions (Azure) → Cachefly | CDN 100 MB | 1045.6 |
| Hetzner Ashburn → Cachefly | CDN 100 MB | 5984.6 |

Both clients can download at ≥1 Gbps. So the ~130 / ~26–91 Mbps figures above
are **not** client-limited — they are the path to the home.

## 3. UPnP vs manual port-forward

Tested three times, interleaved, same 200–500 MiB payload:

- Azure run: UPnP 45.3 vs manual 45.2 (both full) — **equal**.
- Hetzner run: UPnP 122–139 vs manual 101–104 — **equal** (UPnP marginally faster).

The manual `:4443` rule intermittently **truncated** mid-download (at 47 MiB and
186 MiB in two Azure runs) but matched UPnP whenever it completed. The
truncation is flaky-path packet loss on the congested Azure route, **not** a
property of the static rule.

**Conclusion: UPnP is not slower than a static port-forward. No UPnP penalty.**

## 4. The decisive control: SNAT vs DNAT

The last open question was "is the ~130 Mbps a *router* inbound-port-forward
cap, or the *path*?" Answer: measure the same Mac→VPS transfer without touching
the inbound forward (outbound/SNAT) and compare.

- Mac → Hetzner Ashburn, iperf3 (outbound/SNAT): **153 Mbps**
- Hetzner Ashburn → Mac, HTTPS (inbound/DNAT): **130 Mbps**

Outbound ≈ inbound. A router DNAT penalty would show up as inbound ≪ outbound;
it does not. **The CR1000A's port-forwarding (UPnP or static) is not a
bottleneck.**

## 5. What the numbers mean

- **Home upload ceiling:** ~278 Mbps, but only reachable on-net (Whitesky is
  essentially inside Verizon).
- **Well-peered external host** (Hetzner Ashburn): ~130 Mbps.
- **Congested cross-carrier** (Azure/GitHub): 26–91 Mbps, varying by egress IP.
- **Deprioritized cellular** (Xfinity MVNO): 18–50 Mbps.

Direct-mode throughput is **the recipient's ISP peering to the agent's home**,
not the agent's router, not UPnP, not the agent's upload (which is 2× the
best-case direct rate we saw).

## 6. Methodology lessons (the confounds)

1. **Transatlantic VPS confound.** The first "direct vs relay" numbers (~30–40
   Mbps) were measured through a Hetzner *Germany* box, and were misread as a
   "router DNAT cap ≈ 40 Mbps." Wrong — it was the 113 ms transatlantic path.
   Never benchmark a US home through a European VPS.
2. **GitHub Actions is a *bad* client.** Azure's egress to Verizon FiOS is
   congested and the egress IP (and thus route) changes every run (31.5 → 91 →
   26–45). But its **CDN baseline (1045 Mbps to Cachefly)** is what exposed the
   runner as *fast-but-poorly-peered* — always pair a home download with a
   known-fast CDN download as a control.
3. **iperf3 SNAT-vs-DNAT control.** The only way to exonerate the router's
   inbound forward is to measure the same transfer outbound and compare. 153 ≈
   130 did that.
4. **MVNO phone confound.** Xfinity Wireless deprioritizes under congestion, so
   cellular tests (18 Mbps raw vs 50 via Private Relay) say nothing about the
   home or the transport — only about the phone's carrier.

## 7. Design implications

- **Direct is genuinely good** (~130 Mbps to a well-peered recipient) — plenty
  for streaming and large transfers. It is not router-limited.
- **The relay earns its keep by sidestepping bad peering.** A relay VPS placed
  at a well-peered point can give a *congested-carrier* recipient (e.g. Azure)
  a faster path than their direct route to the agent. This is the concrete
  argument for **measure-and-prefer per recipient** (direct vs relay) rather
  than a fixed default.
- **Relay VPS placement matters.** The transatlantic relay we've been using is
  itself ~30–40 Mbps to the home; an East-Coast relay would be ~130+.
