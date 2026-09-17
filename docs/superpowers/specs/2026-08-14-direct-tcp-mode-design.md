# Direct-TCP Mode — P2P Data Plane over Native HTTPS

**Date:** 2026-08-14
**Status:** Proposed — pending review
**Companion to:** `docs/superpowers/specs/2026-07-25-native-https-tls-passthrough-design.md` (the "FRP spec")
**Supersedes for direct shares:** WebRTC DataChannel transport (the slow SCTP path)
**Reuses from the FRP spec:** certificate architecture (§9), origin→share binding (§11), share-origin lifecycle (§12), agent HTTPS server (§15)
**Amends the FRP spec:** identifiers/namespaces (§7) and DNS/hostname model (§8) — the namespace is split into a direct namespace (clean) and a relay namespace (`.relay.`); see §7.

## 1. Summary

Direct mode currently uses WebRTC DataChannels (SCTP), which collapse to ~8 MB/s at high latency due to userspace congestion control (see `agent/cmd/benchdirect/BENCH_RESULTS.md`). The relay path uses kernel TCP and reaches 20–30 MB/s — but it burns VPS bandwidth, which is the exact cost direct mode exists to avoid.

This spec makes direct mode run over **ordinary browser HTTPS against the agent's own hostname**, terminating TLS at the agent, so the file data flows **peer-to-peer with no relay in the data path**. It reuses the FRP spec's certificate architecture and origin→share binding, but splits the DNS namespace (§7): instead of an SNI gateway + FRP tunnel for the data plane, the agent makes itself directly reachable via **UPnP/NAT-PMP/PCP automatic port mapping** (opened on demand, closed by default — see §5.1) and the direct content hostname resolves straight to the agent's public IP, while the relay path keeps the gateway under a `relay.` sub-namespace.

The relay (today the custom Secure Relay) remains as the automatic fallback and as the "hide my IP" mode. The FRP L4-passthrough design is a candidate relay transport that would share the agent HTTPS server (§3.1).

## 2. Motivation

Direct mode is already quota-free and relay-independent — WebRTC direct transfers bypass the relay VPS today. The problem is purely a transport issue: WebRTC DataChannels (SCTP) collapse to ~8 MB/s at high latency due to userspace congestion control (see `agent/cmd/benchdirect/BENCH_RESULTS.md`), while the relay's kernel TCP reaches 20–30 MB/s.

Replacing SCTP with a direct kernel-TCP (native HTTPS) connection therefore:

1. **Recovers throughput** — at least equal to the relay (20–30 MB/s) and likely higher, because the direct path skips the relay's extra network hop (lower RTT → higher TCP window-delay product).
2. **Enables native HTTP semantics** — real `Range`/`206`, browser video seeking, standard downloads, and — because the agent becomes a first-class HTTPS server — a credible path to Collabora/ONLYOFFICE document editing (iframes, WebSockets, cookies, redirects), which SCTP/WebRTC cannot support natively.

The existing quota-free and relay-independent properties are preserved, not newly introduced.

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

## 3.1 Terminology and the Unified Agent Server

- **Control plane** = the hosted ShareBridge backend (the signaling server: database, redirects, certificate coordinator, route signals).
- **Agent** = the software on the owner's home device. It terminates TLS, serves the recipient page and content, and enforces origin→share binding. The agent independently re-validates source authorization; the control plane's signal only activates a route the agent already knows.

Direct and relay share **one agent HTTPS server** (TLS termination, page, content, binding). The two modes differ only in how the browser's connection reaches that server, which is selected by the hostname (DNS) rather than any server-side switch:

- **Direct** (default): `<origin>.<namespace>.sharebridgeusercontent.com` resolves to the agent's public IP (DDNS); browser → agent public IP:port (UPnP-mapped, on demand).
- **Relay**: `<origin>.relay.<namespace>.sharebridgeusercontent.com` resolves to the hosted L4 gateway; browser → gateway → outbound tunnel → the same agent server (the FRP design).

This unified model is the point of the FRP spec and is what lets the custom Noise layer be deleted: both paths are browser↔agent TLS end-to-end, so TLS is the single encryption layer and the Noise/browser-framing protocol is retired (FRP §19.4).

**Repo layout:** the monorepo gains a third top-level folder in Phase 3 — `agent/` (home server), `control/` (VPS, the control plane — today's `signaling-server/`, renamed), and `relay/` (VPS — `sharebridge-relay-gateway` + `sharebridge-relay-frps`). Phases 1–2 only touch `agent/` and `control/`.

## 4. Transport Selection — Fallback Ladder

Share resolution produces, in order:

1. **Direct** — when the agent reports a live, reachable public endpoint (UPnP mapping succeeded).
2. **Relay** — fallback when the agent has no reachable endpoint (CGNAT, UPnP disabled/unavailable, no usable port). This is the existing custom Secure Relay today; the FRP L4-passthrough design is a candidate replacement that would share the agent HTTPS server (§3.1).
3. **`relayOnly`** — the existing user flag forces relay regardless of direct availability, to hide the home IP.

Selection is made by the control plane at redirect time from the agent's latest endpoint report. The redirect target's hostname encodes the choice — the clean namespace for direct, the `relay.` namespace for relay (§7, §10). A share that falls back to relay does not "fail" — it simply routes through the VPS. The user never configures a router in any path.

## 5. Reachability — NAT Traversal

The agent obtains a public endpoint **automatically, with zero router configuration**, using UPnP IGD / NAT-PMP / PCP:

1. Discover public IP via UPnP `GetExternalIPAddress` (STUN as a fallback for IP discovery only).
2. Request an `AddPortMapping` (external port → agent internal listen address).
3. Verify the mapping with a self-probe.
4. Report `publicIP:port` to the control plane over the authenticated control channel.

Go libraries: `github.com/huin/goupnp` (UPnP IGD port mapping — primary, covers most consumer routers) with `github.com/jackpal/go-nat-pmp` (NAT-PMP — fallback for Apple/older routers). PCP (RFC 6887) can be added later if needed. `github.com/jackpal/gateway` provides gateway IP discovery only, not port mapping.

Requirements for direct mode: the home router exposes a public IP (no CGNAT) and supports UPnP/NAT-PMP/PCP (default on essentially all consumer routers). If either is absent, the agent reports "no endpoint" and the control plane uses relay.

**Lease management:** the agent requests a short lease (60–120s) and renews it while the port is open, so a crash leaves at most the lease window of exposure before the router reclaims the mapping. On graceful shutdown the agent sends `DeletePortMapping` explicitly. Some routers enforce leases laxly (keeping the mapping until a router reboot), so the on-demand/closed-by-default model (§5.1) is the primary defense — the port is only mapped during active sessions.

**Self-probe and trust boundary:** the external probe is performed by the control plane, not the agent itself (hairpin NAT makes a LAN self-probe unreliable). To prevent the probe from becoming an SSRF/scanning primitive, the control plane:

1. Probes only the `publicIP:port` the agent just reported, and only when that IP is a public address (rejecting private, reserved, link-local, and multicast ranges) that matches the agent's own `GetExternalIPAddress` result.
2. Includes a control-plane-generated nonce in the probe request; the agent's HTTPS server answers the nonce only for the share being opened, so a correct echo proves the mapping reaches *this* agent and not some other host on the IP.
3. Rate-limits probes per agent.

The agent never asks the control plane to probe arbitrary hosts, and the control plane never probes an address the agent did not just claim as its own. Because both the reported IP and `GetExternalIPAddress` are agent-supplied, the control plane additionally cross-checks the claimed public IP against an independently observed source address (STUN) and treats any mismatch as relay-only, so a compromised agent cannot induce probes toward arbitrary public targets.

## 5.1 On-Demand Port Opening (Closed by Default)

To avoid advertising an open port at the home IP to random scanners (Shodan, masscan, opportunistic probes), the public port is **closed by default** and opened only for a legitimate, in-flight share access.

The open/close switch is the **UPnP mapping, not the local listener**: the agent's TLS listener stays up internally, but the external port mapping exists only during an active access window.

```text
recipient opens canonical link (presenting the bearer token = the "request for access")
→ control plane validates share active, sends "open" to agent (short lease)
→ agent creates UPnP mapping, acks
→ control plane 302-redirects the browser (only after the ack, so the port is live)
→ agent validates SNI + origin binding, serves content
→ agent removes the mapping when: lease expires unused, or last session ends + inactivity timeout
```

What this buys:

- Random port scanners see the port closed almost always; the home IP is not a visible open-port target.
- A browser cannot set a custom `User-Agent`, so there is no literal "ShareBridge user-agent" check. The effective markers a legitimate browser presents are: the **random origin hostname** (SNI, unguessable, rejected-before-serving per §11 binding) and **presence in the short-lived open window** (timed to a real user action). Post-handshake, the native share code + binding authorize content.
- For public shares, "approved" = "possesses the link" (per-user identity is a separate future feature, FRP §7.6). On-demand opening therefore defends against *untargeted* scanning, not against someone who already holds the link.

**Strict open-signal validation:** the agent never opens a port merely because the control plane asked. The open signal only activates a route for a share the agent has independently registered and source-verified; it cannot create arbitrary listeners or reach arbitrary local services (the HTTPS server is never a generic proxy). A hijacked control plane is therefore bounded to toggling routes for shares that already exist, and the agent rate-limits open signals per unit time. (This bounds *port-opening* abuse only; a control plane that also holds DNS/ACME authority has a wider capability — see §11.3.)

**Signal format:** an open signal is a short-lived, versioned, idempotent message bound to `(agent, share, route, nonce)`, carrying an expiration and a per-share open lease. The agent (a) drops signals that are expired, replayed, or already applied; (b) re-verifies the share is currently registered and source-authorized locally (the signal activates, never creates, a route); (c) refuses all open signals while in lockdown; and (d) rate-limits signals per unit time and per share.

Tradeoffs:

- **First-load latency:** control-channel round trip + UPnP mapping (~1–3s) before the redirect. Hideable with a brief "preparing secure connection" interstitial on the control plane.
- **Concurrent recipients:** the mapping stays open while any session is active.
- **Reconnect/resume:** a generous inactivity timeout (e.g. 5–15 min) keeps the port open between a video's range requests; a lapsed port is recovered by the browser re-opening the canonical link.

## 6. Port Strategy

DNS carries no port; `https://host` defaults to 443. Strategy:

1. **Never clobber existing mappings** — before mapping 443, the agent queries the router's existing port mappings (where the device supports listing them) and refuses to delete or overwrite any mapping it did not create. If 443 is already mapped (to another service or a stale ShareBridge mapping), the agent falls back to a random high external port instead of disrupting it.
2. **443 preferred** — when UPnP can map external 443 cleanly, URLs are clean (no `:port`).
3. **Non-standard port fallback** — when 443 is unavailable (blocked by ISP, or already mapped), the agent maps any free port (e.g. 8443) and the redirect URL includes it: `https://…:8443/…`. The wildcard certificate is valid for any port, so this is purely cosmetic.
4. The agent must never require the user to forward a port manually.

A port is not required to be static across agent restarts: the control plane stores the agent's *current* endpoint and emits redirects accordingly.

## 7. DNS and DDNS

Because direct and relay resolve to **different IP targets** (the agent's public IP vs the hosted gateway), a single wildcard A record cannot serve both. The namespace is therefore split into two, both under the same registered domain (`sharebridgeusercontent.com`, preserving browser site isolation):

| Namespace | Wildcard A record → | Used by |
|---|---|---|
| `*.<agent-namespace>.sharebridgeusercontent.com` | **agent public IP** (DDNS) | direct (the default) |
| `*.relay.<agent-namespace>.sharebridgeusercontent.com` | **gateway** (static) | relay (FRP path) |

- The clean `<agent-namespace>` namespace belongs to **direct** because direct is the default mode; the relay (FRP) path is the labeled `.relay.` exception. This **amends FRP §7/§8**, which assigned the clean namespace to the relay-only gateway model.
- The control plane updates the direct A record when the agent reports a changed public IP (dynamic DNS). The relay A record is static (the gateway IP changes rarely).
- DNS readiness is part of agent enrollment (as in FRP §8), not share creation.
- Short TTL (e.g. 60s) bounds the propagation window after an IP change. A stale direct A record is harmless: the agent's origin→share binding rejects unrelated hostnames, and a wrong IP simply fails to connect (then falls back to relay for new sessions).
- The agent namespace is a random, replaceable value (privacy: not linked to identity — FRP §7.3). **One certificate per agent namespace — with two wildcard SANs — covers unlimited share origins in both namespaces.** Creating a share requires no certificate and no DNS mutation; it is just a route already covered by the wildcards. Per-share origins are permanent and tombstoned (FRP §7.4). The Let's Encrypt default of 50 certificates/registered-domain/week bounds *agent enrollment* rate (one cert — regardless of SAN count — per new agent namespace), not share creation; Google Public CA (100 orders/hour) and ZeroSSL (unlimited) are the scale candidates (FRP §9.5). Namespace rotation is possible but infrequent for the same reason.

## 8. Certificate Architecture (reused from FRP §9)

The agent generates and holds its TLS private key locally (it never leaves the agent), submits a CSR for **both wildcard SANs** — `*.<agent-namespace>.sharebridgeusercontent.com` and `*.relay.<agent-namespace>.sharebridgeusercontent.com` — and the control plane's certificate coordinator completes DNS-01 ACME issuance and returns only the public chain. This is **one certificate order with two SANs**, so the per-agent certificate count and CA rate-limit cost are unchanged. Renewal, atomic reload, CA abstraction, and CT handling are identical to the FRP spec. TLS is the end-to-end encryption; the custom Noise layer is not used for the new data plane. The certificate coordinator and DDNS updater remain OSS code behind provider-neutral interfaces (FRP §9.4); DNS-provider and CA credentials are operator-supplied secrets (env/config), never committed to the repo — self-hosters bring their own domain, DNS provider, and CA credentials (FRP §18.3).

## 9. Agent HTTPS Server (reused from FRP §15)

The agent is the authoritative recipient web server: TLS termination, `Host`/origin/share binding, gallery/file UI, password forms, `Range`/`206`, ZIP, uploads, WebSockets, and security headers. The service worker that currently simulates HTTP over the message channel is retired for migrated shares.

## 10. Canonical Link and Redirect Flow

```text
Recipient → GET https://sharebridge.app/s/<token>
  → control plane resolves active share + agent endpoint + mode
  → direct: control plane signals agent to open the port on demand; waits for ack
  → 302 to https://<origin>.<namespace>.sharebridgeusercontent.com[:port]/s/<token>      (direct)
     or https://<origin>.relay.<namespace>.sharebridgeusercontent.com/s/<token>          (relay)
  → browser connects to the agent's public IP:port (direct) or the hosted gateway (relay)
  → agent terminates TLS, validates (Host, route kind, native code), serves content
```

The native share code remains the bearer capability. No redundant bootstrap token is added.

## 11. Security Properties

### 11.1 Provided

- File data encrypted end-to-end (browser↔agent) with the agent-owned TLS key; the control plane cannot read it under normal operation (see the trust boundary in §11.3).
- No server bandwidth in the data path; transfer survives relay-VPS outage.
- Strict agent-side origin→share binding (FRP §11) is the enforcement point.
- Content served from a separate registered domain (`sharebridgeusercontent.com`), so a compromised agent is browser-isolated from control-plane cookies, storage, and service workers.
- **Lockdown mode**: the agent immediately (a) removes the UPnP mapping, (b) revokes all active origins, and (c) closes all established direct TCP/TLS connections it is serving (tracked separately from relay connections, so relay sessions can continue if desired). Removing the mapping alone does not reliably terminate in-flight NAT flows, so connection tracking + explicit close is required.
- **Closed-by-default port**: the public port is open only during a control-plane-triggered access window (§5.1), so the home IP is not a visible open-port target for scanners.

### 11.2 Accepted Tradeoffs (deliberate)

- **Home IP exposure** — direct mode publishes the agent's public IP (in DNS and the TCP connection). This is already disclosed in the UI; `relayOnly` remains the "hide my IP" option.
- **No edge gateway** — abuse/DoS limits move from a hosted gateway to the agent and the control plane. A recipient who already knows an origin can still complete a TLS handshake toward the agent after revocation until the DNS record is removed or lockdown closes the listener; the agent rejects the actual requests via binding, and the control plane stops emitting the origin for new recipients.
- **Volumetric DoS** saturates the home link rather than a hosted edge. Lockdown mode mitigates but does not eliminate this.

### 11.3 Trust Boundary (carried from FRP §9.7)

The confidentiality guarantee ("control plane cannot read file data") holds **under normal operation**, not against a compromised control plane. Because ShareBridge controls the parent DNS domain and the ACME credentials, a compromised certificate coordinator or DNS worker can complete domain validation with an attacker-controlled key, obtain a replacement certificate for an agent namespace, repoint DNS, and intercept browser↔agent traffic. The design mitigates this with credential isolation, least-privilege DNS tokens, CAA records, append-only issuance logs, Certificate Transparency monitoring, and agent-side alerts — but the property is "confidential against passive and network adversaries," not against a fully compromised control plane.

## 12. Failure Handling

- **No endpoint (CGNAT/UPnP off):** control plane selects relay; share still works.
- **IP change mid-session:** in-flight TCP connections break; the browser retries, and the next resolution uses the updated endpoint or relay.
- **Agent offline:** control plane serves the existing "agent offline" page; no redirect to a dead endpoint.
- **Port remapped at restart:** stored endpoint is stale; control plane uses the latest report, falling back to relay for the overlap.
- **Cert not ready:** direct shares are not published until cert + DNS readiness pass (FRP §20.3).
- **Open-ack timeout:** if the agent does not ack the open signal within a short timeout, the control plane falls back to relay for that session instead of redirecting to a closed port.
- **Direct connection failure (TLS timeout, ISP block, stale DNS, path firewall):** a 302 to a dead direct endpoint cannot be retried server-side — the browser has already left. The canonical page therefore serves an interstitial that (a) attempts the direct connection while hiding the open latency, and (b) on a bounded timeout with no completed TLS handshake, redirects the browser to the relay hostname for the same share. This is the *recipient-path* reachability check: an agent/control-plane self-probe alone does not prove the recipient's own path works.
- **Lapsed port:** a recipient whose port closed due to inactivity re-opens the canonical link, which re-triggers on-demand opening.

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
- Reuse the FRP spec's certificate architecture and origin→share binding; the DNS namespace model is amended (split into direct + relay namespaces — §7).
- Direct and relay share one agent HTTPS server; the relay migrates to FRP-style L4 passthrough so TLS is the single e2e encryption layer and the Noise/browser-framing protocol is deleted.
- The open signal only activates agent-registered, source-verified shares; the agent never opens arbitrary ports or reach arbitrary local services.
- Reachability via UPnP/NAT-PMP/PCP automatic port mapping — no manual router configuration, ever.
- 443 preferred; non-standard port acceptable (cosmetic `:port` in URL).
- Direct is the default mode with relay as automatic fallback; `relayOnly` forces relay (hide IP). Speed parity (≥ relay throughput) is validated by the benchdirect spike before direct becomes the default.
- IP exposure is an accepted, already-disclosed property of direct mode.
- Content remains on `sharebridgeusercontent.com` for browser site isolation.
- Two DNS namespaces per agent under one domain: the clean `<namespace>` resolves to the agent IP (direct, the default) and `relay.<namespace>` resolves to the gateway (relay). One certificate with two wildcard SANs covers both — no additional certificate or rate-limit cost.
- The custom Noise/browser-framing layer is retired for migrated shares.
- The public port is closed by default and opened on demand by the control plane for an active share access; the open/close switch is the UPnP mapping.
- Lockdown mode is a prominent red button in the agent dashboard: it immediately removes the UPnP mapping, revokes all active origins, closes established direct connections, and notifies the control plane to deactivate shares — reversible via an explicit unlock. A separate, less prominent action stops the agent process entirely.
- Sequencing: build the agent HTTPS server + direct wiring first (the custom Secure Relay remains the fallback), then migrate the relay to FRP L4 passthrough and delete Noise/WebRTC. There is no legacy-compatibility constraint (pre-release, no external users), so this is a clean v2 rewrite.
- The direct transport (UPnP + on-demand + endpoint reporting) is packaged as a reusable internal library behind a narrow transport interface (sibling to the FRP tunnel); DDNS and the HTTP/reverse-proxy layer are separate concerns.
- UPnP: `huin/goupnp` (IGD, primary) + `jackpal/go-nat-pmp` (NAT-PMP, fallback); PCP later if needed.
- DNS: Cloudflare (registrar Porkbun, DNS managed on Cloudflare). ACME DNS-01 via lego's built-in Cloudflare provider; DDNS A-record updates via `github.com/cloudflare/cloudflare-go`; TTL 60s.
- Service naming: relay path is `sharebridge-relay` (components `sharebridge-relay-gateway` for L4 SNI routing + `sharebridge-relay-frps` for the tunnel); control plane is `sharebridge-control` (accounts + direct-mode negotiation + share lifecycle).

### 14.1 Recorded limitation (2026-09-17): direct mode requires host-level cooperation

Phase-4a live acceptance exposed a deployment constraint the design above did not account for.
The §14 bullet "Reachability via UPnP/NAT-PMP/PCP automatic port mapping — no manual router
configuration, ever" holds for the **router**, but not for the **agent host**. Three conditions
must all be true for direct mode, and the product controls none of them:

1. **The agent container must use `network_mode: host`.** With bridge networking, UPnP/NAT-PMP
discovery targets the Docker bridge instead of the LAN router (so no mapping is ever created),
and a router port-forward to the host cannot reach the container (Docker's NAT cannot publish a
port chosen at runtime). Consequence: compose `-p` mappings are irrelevant in host mode and
insufficient in bridge mode — **bridge networking ⇒ relay-only**.
2. **The host firewall must allow the mapped port.** The container shares the host's network
namespace but cannot manage the host firewall, and neither Docker nor compose can either. The
router's PnP forward lands on a host port that `firewalld`/`ufw`/`nft` drops unless an operator
explicitly opens it. Observed live: NAT-PMP granted external port 49152 and the agent reported its
endpoint successfully, yet control's probe failed (`direct_status_reason=probe_failed`) **solely**
because the host firewall rejected the inbound connection. The product behaved correctly
(fail-closed to relay) but nothing in the product creates — or even mentions — the required rule.
3. **The router must support UPnP/NAT-PMP, or be manually forwarded.** A meaningful share of the
self-hosting audience deliberately disables UPnP as a security risk, and CGNAT makes mapping
impossible regardless.

Aggravating factor: the agent picks its external port dynamically from the whole IANA range
(`firstFreePort`, `agent/internal/direct/portmap.go` — 49152–65535, hard-coded), so "open the
firewall for the agent" currently means opening 16,384 ports. A narrow, configurable range and/or
an operator-declared preferred port is a prerequisite for any host-firewall guidance.

**Consequence recorded: relay is the default and the optimization target; direct is best-effort
and requires an opted-in host.** Relay needs nothing inbound — no port, no UPnP, no firewall rule —
and works behind CGNAT and double NAT. Measured on the live test deployment, relay delivered
~20 MB/s (~160 Mbps) end-to-end with zero configuration, versus direct's best-case ~130–278 Mbps
that varies with the recipient's peering. If direct is pursued later it must be (a) explicitly
opt-in, (b) documented with its host-networking/firewall/UPnP prerequisites per platform, and
(c) diagnosed explicitly — a distinct "mapping succeeded but the mapped port is unreachable; check
the host firewall" reason rather than a generic `probe_failed`.

## 15. Open Questions

1. Select and register the content domain (`sharebridgeusercontent.com` is the working name but not yet purchased — FRP §27.1); add it to Cloudflare once bought.
2. Whether to publish the "WebTCP"-style transport library publicly (internal packaging is decided in §14).
3. Frontend model: (a) custom ShareBridge frontend — lean (Immich Public Proxy-style: server-side fetch of the share's assets + minimal gallery, ~one API call) or rich (current full connector); vs (b) transparent route-proxy to the source's own share page (11notes/immich-share-proxy style — least code, non-unified UX, route-filter maintenance). The agent HTTPS server is frontend-agnostic, so this is decoupled from the transport and can be decided later.

## 16. References

- FRP native-HTTPS spec: `docs/superpowers/specs/2026-07-25-native-https-tls-passthrough-design.md`
- Relay-default UX: `docs/superpowers/specs/2026-07-18-relay-default-design.md`
- SCTP collapse root cause: `agent/cmd/benchdirect/BENCH_RESULTS.md`
- UPnP IGD: `github.com/huin/goupnp`; NAT-PMP: `github.com/jackpal/go-nat-pmp`; gateway discovery: `github.com/jackpal/gateway`
- ngrok/zrok (reference for the outbound-tunnel relay pattern; not used — stock frontends terminate TLS at the edge, and they are generic proxies — FRP §3 and §23.6)
