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

> **Status: [pending manual run]** — requires a real router and an off-LAN
> (cellular) probe vantage; both are supplied by the human. No result is
> recorded until those runs happen.

| Field | Value |
|---|---|
| Mapper that succeeded | `[pending manual run]` — UPnP WANIP / UPnP WANPPP / NAT-PMP (or none) |
| Observed external IP | `[pending manual run]` |
| Requested external port | `[pending manual run]` (443 by default) |
| Granted external port | `[pending manual run]` (may differ from requested for NAT-PMP) |
| 443 available (free) or fell back | `[pending manual run]` |

## 2. Port strategy (spec §6)

- Whether 443 was free or the spike fell back to a high dynamic-range port:
  `[pending manual run]`
- Confirmation that no pre-existing foreign mapping was clobbered:
  `[pending manual run]` — expected to hold by construction (`ChooseExternalPort`
  enumerates and never selects an occupied port; `DeleteOwnedMapping` refuses
  non-`sharebridge`-prefixed descriptions). This claim is covered in-process by
  the unit tests; the on-router confirmation is manual.

## 3. Off-LAN probe result

- Reachable from off-LAN: `[pending manual run]`
- Vantage: `[pending manual run]` (must be cellular / a host NOT on the same
  LAN — never a hairpin self-probe)
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
| `[pending manual run]` | `[pending manual run]` | `[pending manual run]` | `[pending manual run]` | `[pending manual run]` | `[pending manual run]` | `[pending manual run]` | `[pending manual run]` | `[pending manual run]` | `[pending manual run]` |
| `[pending manual run]` | `[pending manual run]` | `[pending manual run]` | `[pending manual run]` | `[pending manual run]` | `[pending manual run]` | `[pending manual run]` | `[pending manual run]` | `[pending manual run]` | `[pending manual run]` |
| `[pending manual run]` | `[pending manual run]` | `[pending manual run]` | `[pending manual run]` | `[pending manual run]` | `[pending manual run]` | `[pending manual run]` | `[pending manual run]` | `[pending manual run]` | `[pending manual run]` |

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
