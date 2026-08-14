# Direct-TCP Mode — P2P Data Plane over Native HTTPS

**Date:** 2026-08-14
**Status:** Proposed — pending review
**Companion to:** `docs/superpowers/specs/2026-07-25-native-https-tls-passthrough-design.md` (the "FRP spec")
**Supersedes for direct shares:** WebRTC DataChannel transport (the slow SCTP path)
**Reuses from the FRP spec:** certificate architecture (§9), DNS/hostname model (§8), identifiers/namespaces (§7), origin→share binding (§11), share-origin lifecycle (§12), agent HTTPS server (§15)

## 1. Summary

Direct mode currently uses WebRTC DataChannels (SCTP), which collapse to ~8 MB/s at high latency due to userspace congestion control (see `agent/cmd/benchdirect/BENCH_RESULTS.md`). The relay path uses kernel TCP and reaches 20–30 MB/s — but it burns VPS bandwidth, which is the exact cost direct mode exists to avoid.

This spec makes direct mode run over **ordinary browser HTTPS against the agent's own hostname**, terminating TLS at the agent, so the file data flows **peer-to-peer with no relay in the data path**. It reuses the FRP spec's certificate and DNS architecture unchanged — only the data plane (§13 of that spec) changes: instead of an SNI gateway + FRP tunnel, the agent makes itself directly reachable via **UPnP/NAT-PMP/PCP automatic port mapping** and the content hostname resolves straight to the agent's public IP.

The relay (FRP) remains as the automatic fallback and as the "hide my IP" mode.

## 2. Motivation

Direct mode has two distinct value propositions that the current WebRTC implementation fails to deliver:

1. **Quota-free bandwidth.** File bytes bypass the relay VPS, so there is no server bandwidth cost.
2. **Independence from the relay.** The transfer continues even if the relay VPS is down; only negotiation/redirect needs the control plane.

The current direct mode is technically quota-free, but its ~8 MB/s SCTP ceiling makes it unattractive ("may be slower due to browser protocol limitations" — see `2026-07-18-relay-default-design.md`). Replacing SCTP with kernel TCP recovers throughput at least equal to the relay (20–30 MB/s) and likely higher, because the direct path skips the relay's extra network hop (lower RTT → higher TCP window-delay product).

## 3. High-Level Architecture

```text
                          CONTROL PLANE (sharebridge.app)

  Agent ◄── authenticated control channel ──► Control plane
    │          (enrollment, cert CSR,        (share discovery, route
    │           public endpoint reports,       allocation, DDNS update,
    │           revocation)                    CA DNS-01 issuance)

                          DATA PLANE — DIRECT

  Recipient browser
    │  HTTPS (TLS terminated at agent, agent-owned key)
    │  hostname: <origin>.<namespace>.sharebridgeusercontent.com[:port]
    ▼
  Agent public IP:port   (reached via UPnP/NAT-PMP/PCP mapping)
    │
    ▼
  Local ShareBridge agent  ──►  Immich / Nextcloud / OpenCloud (LAN)
```

The data plane has no intermediary server. The control plane's only runtime roles are: the canonical-link redirect, DNS record updates, and certificate issuance. It never sees file bytes.

## 4. Transport Selection — Fallback Ladder

Share resolution produces, in order:

1. **Direct** — when the agent reports a live, reachable public endpoint (UPnP mapping succeeded).
2. **Relay (FRP)** — fallback when the agent has no reachable endpoint (CGNAT, UPnP disabled/unavailable, no usable port).
3. **`relayOnly`** — the existing user flag forces relay regardless of direct availability, to hide the home IP.

Selection is made by the control plane at redirect time from the agent's latest endpoint report. A share that falls back to relay does not "fail" — it simply routes through the VPS. The user never configures a router in any path.

## 5. Reachability — NAT Traversal

The agent obtains a public endpoint **automatically, with zero router configuration**, using UPnP IGD / NAT-PMP / PCP:

1. Discover public IP via UPnP `GetExternalIPAddress` (STUN as a fallback for IP discovery only).
2. Request an `AddPortMapping` (external port → agent internal listen address).
3. Verify the mapping with a self-probe.
4. Report `publicIP:port` to the control plane over the authenticated control channel.

Go libraries: `github.com/huin/goupnp` (UPnP) and `github.com/jackpal/gateway` (NAT-PMP/PCP).

Requirements for direct mode: the home router exposes a public IP (no CGNAT) and supports UPnP/NAT-PMP/PCP (default on essentially all consumer routers). If either is absent, the agent reports "no endpoint" and the control plane uses relay.

## 6. Port Strategy

DNS carries no port; `https://host` defaults to 443. Strategy:

1. **443 preferred** — when UPnP can map external 443, URLs are clean (no `:port`).
2. **Non-standard port fallback** — when 443 is unavailable (blocked by ISP, or already mapped to another service), the agent maps any free port (e.g. 8443) and the redirect URL includes it: `https://…:8443/…`. The wildcard certificate is valid for any port, so this is purely cosmetic.
3. The agent must never require the user to forward a port manually.

A port is not required to be static across agent restarts: the control plane stores the agent's *current* endpoint and emits redirects accordingly.

## 7. DNS and DDNS

`<agent-namespace>.sharebridgeusercontent.com` and its wildcard child `*.<agent-namespace>.sharebridgeusercontent.com` resolve to the agent's public IP via A records owned by the control plane.

- The control plane updates the A record when the agent reports a changed public IP (dynamic DNS).
- DNS readiness is part of agent enrollment (as in FRP §8), not share creation.
- Short TTL (e.g. 60s) bounds the propagation window after an IP change. A stale A record is harmless: the agent's origin→share binding rejects unrelated hostnames, and a wrong IP simply fails to connect (then falls back to relay for new sessions).

## 8. Certificate Architecture (reused from FRP §9)

Unchanged: the agent generates and holds its TLS private key locally (it never leaves the agent), submits a wildcard CSR for `*.<agent-namespace>.sharebridgeusercontent.com`, and the control plane's certificate coordinator completes DNS-01 ACME issuance and returns only the public chain. Renewal, atomic reload, CA abstraction, and CT handling are identical to the FRP spec. TLS is the end-to-end encryption; the custom Noise layer is not used for the new data plane.

## 9. Agent HTTPS Server (reused from FRP §15)

The agent is the authoritative recipient web server: TLS termination, `Host`/origin/share binding, gallery/file UI, password forms, `Range`/`206`, ZIP, uploads, WebSockets, and security headers. The service worker that currently simulates HTTP over the message channel is retired for migrated shares.

## 10. Canonical Link and Redirect Flow

```text
Recipient → GET https://sharebridge.app/s/<token>
  → control plane resolves active share + agent endpoint
  → 302 to https://<origin>.<namespace>.sharebridgeusercontent.com[:port]/s/<token>
  → browser connects directly to the agent's public IP:port
  → agent terminates TLS, validates (Host, route kind, native code), serves content
```

The native share code remains the bearer capability. No redundant bootstrap token is added.

## 11. Security Properties

### 11.1 Provided

- File data encrypted end-to-end (browser↔agent) with the agent-owned TLS key; the control plane cannot read it.
- No server bandwidth in the data path; transfer survives relay-VPS outage.
- Strict agent-side origin→share binding (FRP §11) is the enforcement point.
- Content served from a separate registered domain (`sharebridgeusercontent.com`), so a compromised agent is browser-isolated from control-plane cookies, storage, and service workers.
- **Lockdown mode**: the agent can immediately stop listening and revoke all active origins, closing the public socket on demand.

### 11.2 Accepted Tradeoffs (deliberate)

- **Home IP exposure** — direct mode publishes the agent's public IP (in DNS and the TCP connection). This is already disclosed in the UI; `relayOnly` remains the "hide my IP" option.
- **No edge gateway** — abuse/DoS limits move from a hosted gateway to the agent and the control plane. A recipient who already knows an origin can still complete a TLS handshake toward the agent after revocation until the DNS record is removed or lockdown closes the listener; the agent rejects the actual requests via binding, and the control plane stops emitting the origin for new recipients.
- **Volumetric DoS** saturates the home link rather than a hosted edge. Lockdown mode mitigates but does not eliminate this.

## 12. Failure Handling

- **No endpoint (CGNAT/UPnP off):** control plane selects relay; share still works.
- **IP change mid-session:** in-flight TCP connections break; the browser retries, and the next resolution uses the updated endpoint or relay.
- **Agent offline:** control plane serves the existing "agent offline" page; no redirect to a dead endpoint.
- **Port remapped at restart:** stored endpoint is stale; control plane uses the latest report, falling back to relay for the overlap.
- **Cert not ready:** direct shares are not published until cert + DNS readiness pass (FRP §20.3).

## 13. Validation Before Implementation Commitment

These mirror FRP §27 and are the "does it actually work" gate for the direct path:

1. Prove UPnP/NAT-PMP mapping + self-probe works on representative consumer routers (including the project owner's own port-forwarded setup, where 443 is already occupied).
2. Prove the non-standard-port fallback (HTTPS on 8443) works in Chrome/Safari/Firefox.
3. Prove DDNS update and short-TTL propagation with the chosen DNS provider.
4. Reuse the benchdirect harness (or a variant) to measure direct-TCP throughput vs the relay at RTT ≥ 50 ms — confirming ≥ relay throughput and the absence of the SCTP collapse.
5. Prove strict origin→share binding rejects revoked/unrelated shares at the agent.

Failure of the UPnP spike does not block the overall direction (relay remains the guaranteed path) but does determine how often direct mode is available in practice.

## 14. Decisions Recorded

- Direct mode becomes P2P native HTTPS, terminating TLS at the agent.
- Reuse the FRP spec's certificate and DNS architecture unchanged.
- Reachability via UPnP/NAT-PMP/PCP automatic port mapping — no manual router configuration, ever.
- 443 preferred; non-standard port acceptable (cosmetic `:port` in URL).
- Direct-first with relay fallback; `relayOnly` forces relay.
- IP exposure is an accepted, already-disclosed property of direct mode.
- Content remains on `sharebridgeusercontent.com` for browser site isolation.
- The custom Noise/browser-framing layer is retired for migrated shares.

## 15. Open Questions

1. Select the concrete UPnP/NAT-PMP library and its self-probe method.
2. Decide the DDNS provider and TTL policy.
3. Decide whether direct mode is enabled by default for new shares, or opt-in (the current relay-default doc makes relay the default; this spec's fallback ladder implies direct-first, which needs a product decision).
4. Define the lockdown-mode UX (where the button lives, whether it also forces all active shares to relay).

## 16. References

- FRP native-HTTPS spec: `docs/superpowers/specs/2026-07-25-native-https-tls-passthrough-design.md`
- Relay-default UX: `docs/superpowers/specs/2026-07-18-relay-default-design.md`
- SCTP collapse root cause: `agent/cmd/benchdirect/BENCH_RESULTS.md`
- UPnP: `github.com/huin/goupnp`; NAT-PMP/PCP: `github.com/jackpal/gateway`
