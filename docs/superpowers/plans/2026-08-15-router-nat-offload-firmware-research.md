# Plan: Reverse-engineer top consumer router firmwares for inbound NAT offload

**Date:** 2026-08-15
**Status:** proposed (research; agents to be spun up later)
**Spec context:** `docs/superpowers/specs/2026-08-14-direct-tcp-mode-design.md`, Phase-1 throughput findings.

## Goal

Determine, for the most common home routers, whether the firmware hardware-offloads
*inbound port-forwarded* (DNAT) flows — and specifically whether *UPnP-created*
(dynamic, leased) mappings get offloaded or stay on the CPU.

## Background

Phase-1 throughput validation on a Verizon CR1000A found direct-TCP (UPnP
port-forward) caps at ~27-45 Mbps **total** (single or parallel connections),
while outbound (SNAT) runs ~286 Mbps (speedtest) and the relay ~240 Mbps. Root
cause hypothesis: the router CPU processes inbound/UPnP flows; outbound flows are
hardware-offloaded.

**Open refinement (user-reported, unverified):** a *manual/static* port-forward on
the same router reportedly runs fast. If confirmed, the finding narrows to
"UPnP-created mappings aren't offloaded, static ones are" — a more actionable,
possibly fixable distinction.

## Questions to answer per router

1. Does the firmware contain a flow-offload engine (Broadcom `bcm_nat`/`pktflow`,
   Qualcomm `NSS`/`ECM`, MediaTek `mtk_hnat`, Linux `nf_flow_table`/`xt_FLOWOFFLOAD`)?
2. Does that engine's rule matching cover **DNAT** (inbound destination-rewrite)
   flows, or only SNAT (outbound)?
3. Are **UPnP-created** mappings eligible for offload, or pinned to the CPU path
   (separate netfilter hook / conntrack mark / ephemeral-lease handling)?

## Target routers (top ~10 by US home market share)

1. Verizon CR1000A (FiOS) — reference unit (already available)
2. Comcast Xfinity XB8 (Technicolor/Arris)
3. AT&T BGW320-500
4. Spectrum SAX1V1R / ET2251
5. ASUS RT-AX88U (Broadcom)
6. TP-Link Archer AX55 / AX21
7. Netgear Nighthawk RAX54
8. Eero Pro 6E
9. Google Nest Wifi Pro
10. Ubiquiti UniFi Dream Machine

## Method per router

1. **Obtain firmware** — third-party: vendor/GPL website download or OpenWrt source.
   ISP: extract from device (serial/UART, TR-069 endpoint, or community leaks);
   ISP firmware is often NOT published.
2. **Extract** — `binwalk` / `firmware-mod-kit` / `ubi_reader` (squashfs/UBI/JFFS2).
3. **Identify cheap tells first** (before any disassembly):
   - kernel modules present: `bcm_nat.ko`, `pktflow`, `nss-*`, `mtk_hnat`,
     `nf_flow_table`, `xt_FLOWOFFLOAD`
   - config: nvram vars (`ctf_disable`, `pktflow_enable`), `/proc/*flow*` entries,
     firewall init scripts, OpenWrt `flow_offloading` / `hw_flow_offloading`
   - netfilter/nftables rules referencing offload (`-j FLOWOFFLOAD`, `ct status dnat`)
4. **Determine DNAT + UPnP eligibility** — inspect the offload rule-matching logic
   (source for GPL routers; RE the `.ko` for ISP routers).
5. **Cross-check with a live throughput test** where hardware is available
   (agent→VPS fetch through a *manual* port-forward vs a *UPnP* mapping) —
   capability ≠ measured throughput.

## Output

A matrix `router × {offload engine, covers DNAT?, covers UPnP-created?, notes}`,
plus a short writeup per router. Commit findings under `docs/superpowers/spikes/`.

## Scope & caveats

- Yields *capability*, not *measured Mbps*; pair with the self-test/telemetry
  (agent measures its own direct-vs-relay throughput) for real numbers at scale.
- ISP firmwares are proprietary and often unobtainable or re-hashed; expect to RE
  closed `.ko` blobs for those.
- Primary practical payoff: confirm/refute the "UPnP mappings are the slow path"
  hypothesis — which determines whether a firmware/setting/workaround exists
  (vs. "direct is just router-capped, use relay").
- This is research, not product code; no changes to the transport path.
