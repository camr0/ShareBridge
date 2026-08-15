# Spike: UPnP/NAT-PMP reachability for direct mode

**Date:** 2026-08-14
**Task:** Task 1 of Phase 1 validation spikes (`docs/superpowers/plans/2026-08-14-direct-tcp-validation-spikes.md`)
**Code:** `agent/internal/direct/portmap.go`, `agent/cmd/spike-upnp/main.go`
**Spec refs:** `docs/superpowers/specs/2026-08-14-direct-tcp-mode-design.md` §5 (reachability), §5.1 (on-demand port), §6 (port strategy)

## Purpose

Prove the agent can discover a UPnP/NAT-PMP gateway, map an external port with
zero manual router configuration, and report the **granted** external port —
the first feasibility gate for the direct-TCP data plane. Library discovery
success is *not* the same as reachability; a mapping is only proven reachable
when an off-LAN probe reaches the agent through it.

## 1. Result

> **Status: PASS (single router, off-LAN confirmed).** An external probe from a
> cellular (non-LAN) vantage reached the mapping and downloaded `/file`
> (~1.2 MB `spike.bin`). Recorded 2026-08-14 on the human's home router.

| Field | Value |
|---|---|
| Mapper that succeeded | UPnP IGD (WANIP/WANPPP flavor not logged by the spike — `MapperForRouter` tries UPnP first, then NAT-PMP) |
| Observed external IP | `173.54.233.213` |
| Requested external port | 443 |
| Granted external port | 49152 (fallback — not a NAT-PMP remap; UPnP honors the requested port) |
| 443 available (free) or fell back | **fell back** — 443 occupied by a foreign mapping, so `ChooseExternalPort` selected 49152 |

## 2. Port strategy (spec §6)

- 443 was **occupied** by a pre-existing foreign mapping; the spike fell back
  to 49152 (first free port in the IANA dynamic range 49152–65535).
- No pre-existing foreign mapping was clobbered (observed: the existing 443
  service stayed up; `ChooseExternalPort` enumerated and skipped the occupied
  port). In-process, the unit tests (`TestChooseExternalPort_FallsBackWhen443Taken`,
  `TestDeleteOwnedMapping_RefusesForeign`) assert the same behavior.

## 3. Off-LAN probe result

- Reachable from off-LAN: **YES** — a phone on cellular downloaded `/file`
  (~1.2 MB `spike.bin`). A TLS handshake from the cellular IP
  (`172.226.203.117`) also appears in the server log, confirming the off-LAN
  path reached the listener before the successful download.
- Vantage: cellular (off-LAN) — the load-bearing result. A Wi-Fi (hairpin)
  download also succeeded but is not load-bearing.
- **Pass/fail criterion:** the spike PASSES only if the off-LAN probe reaches
  the mapping (`reachable via <ip>:<granted-port>` and a successful `/file`
  download). A *library-discovery success with a failed external probe is
  recorded as a FAILURE* — the likely causes (double NAT, CGNAT, ISP inbound
  block, hairpin-only, lease-lax router) are noted in the matrix below.

## 4. Self-probe trust boundary (spec §5)

In production the external probe is performed by the **control plane, not the
agent** (hairpin NAT makes a LAN self-probe unreliable). The control plane
probes only the `publicIP:port` the agent just reported — restricted to public
IPs that match the agent's own `GetExternalIPAddress` result **and an
independently observed source address (STUN); a mismatch forces relay-only** —
with a control-plane-generated nonce that the agent's HTTPS server echoes only
for the share being opened, and probes are rate-limited per agent.

This spike is single-process, so a real control-plane probe is out of scope;
the phone-on-cellular check is the manual stand-in for that probe.

## 5. Representative-router matrix

The following dimensions are the minimum coverage for declaring direct mode
viable. Each cell records a pass/fail **and the failure mode** — a cell where
"library discovery succeeded" but "external probe failed" is recorded as FAIL
with the reason.

| Dimension | Values to cover |
|---|---|
| Vendor / model | ≥ 3 distinct vendors (e.g. ASUS, TP-Link, Ubiquiti, Fritz!Box, Eero) |
| Protocol | UPnP IGD v1, UPnP IGD v2, NAT-PMP |
| UPnP service flavor | WANIPConnection vs WANPPPConnection |
| External port state | 443 free, 443 occupied (foreign mapping present) |
| Lease behavior | strict lease expiry vs lease-lax routers that keep the mapping until reboot |
| NAT topology | single NAT, double NAT, CGNAT |
| Hairpin | hairpin NAT present vs absent (why the agent never self-probes from LAN) |

### Observed matrix

| Router (vendor/model) | Protocol | Service flavor | 443 state | Lease | NAT topology | Hairpin | Discovery | Off-LAN probe | Verdict |
|---|---|---|---|---|---|---|---|---|---|
| home router (vendor/model TBD) | UPnP IGD | WANIP/WANPPP (not logged) | occupied → 49152 | 300s lease (strict-vs-lax not yet observed) | single NAT | hairpin present (Wi-Fi reachable) | ✅ | ✅ cellular | **PASS** |
| `[pending manual run]` | `[pending manual run]` | `[pending manual run]` | `[pending manual run]` | `[pending manual run]` | `[pending manual run]` | `[pending manual run]` | `[pending manual run]` | `[pending manual run]` | `[pending manual run]` |
| `[pending manual run]` | `[pending manual run]` | `[pending manual run]` | `[pending manual run]` | `[pending manual run]` | `[pending manual run]` | `[pending manual run]` | `[pending manual run]` | `[pending manual run]` | `[pending manual run]` |

> Coverage note: 1 of ≥ 3 distinct router vendors observed. The multi-vendor
> matrix (and the strict-lease vs lease-lax and double-NAT/CGNAT cells) remains
> open — a single PASS on one home router proves the mechanism, not universality.

## 6. How to run

```bash
cd agent
go run ./cmd/spike-upnp                          # plain HTTP first (isolates mapping from TLS)
go run ./cmd/spike-upnp -https -ext-port 443     # Phase 0 HTTPS smoke test
```

Then from a host NOT on the same LAN (phone on cellular):

```bash
curl http://<external-ip>:<granted-port>
curl http://<external-ip>:<granted-port>/file
```

The spike logs the mapped endpoint as `MAPPED external=<ip>:<granted-port> ->
local :<internal-port>`.
