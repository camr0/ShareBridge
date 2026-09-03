# Phase 4a — FRP Relay MVP

**Date:** 2026-09-02
**Status:** Draft — pending user review
**Companion to:** `docs/superpowers/specs/2026-07-25-native-https-tls-passthrough-design.md` and `docs/superpowers/specs/2026-08-14-direct-tcp-mode-design.md`
**Builds on:** Phase 2 transport and Phase 3 Immich content serving on branch `v2` at `700df65`
**Implements:** the availability fallback for CGNAT, disabled/unavailable UPnP, broken hairpin NAT, and recipient-specific direct-path failure

## 0. Open Questions

**No blocking open questions.** The Phase 4a decisions that were open at the start of
this design are resolved in §4. Items that require implementation-time proof rather
than a product decision are listed as validation gates in §23.

## 1. Summary

Phase 4a adds a persistent, outbound-only FRP tunnel from every enrolled agent to a
public Layer-4 relay. A custom SNI gateway on public TCP 443 reads only the bounded TLS
ClientHello needed to obtain SNI, looks up an exact active relay origin, and forwards
the original TLS byte stream through FRP to the same agent HTTPS server Phase 3 uses
for direct traffic.

Browser TLS still terminates only at the agent. The relay has no agent content
certificate or private key, performs no browser TLS termination, and does not parse
HTTP. The relayed recipient URL is:

```text
https://<origin>.relay.<namespace>.sharebridgeusercontent.com/s/<code>
```

The control plane selects direct when the direct path is eligible and prepared, relay
when it is not, and relay unconditionally for a share with `relay_only=true`. If the
control-plane probe says direct is reachable but the recipient's path cannot complete
the direct browser connection—for example, the owner opens their own link behind a
router without hairpin NAT—the `sharebridge.app` interstitial times out the direct
attempt and navigates to the relay origin.

Relay routes do **not** use `open_signal` or `open_ack`. There is no inbound port to
open: the tunnel is outbound, persistent, and independently presence-checked at the
gateway. Phase 4a also decouples baseline agent readiness from direct DDNS/UPnP, so an
agent with no usable direct transport can still enroll, register shares, and serve
them through relay.

This milestone changes transport reachability only. Both hostnames reach the same
Phase 3 HTTP handlers, resolver, snapshot, membership checks, download accounting,
Range implementation, and content backend.

## 2. Motivation and Current State

Phases 1–3 proved and shipped native browser-to-agent HTTPS, certificate enrollment,
on-demand UPnP/NAT-PMP mapping, control-plane reachability probing, strict
origin-to-share binding, Immich gallery/content serving, byte-exact archive downloads,
and `206 Partial Content` video seeking.

The current `v2` route is direct-only:

```text
canonical link
  → open_signal
  → agent opens a router mapping
  → control probes it
  → 302 to direct origin
```

That fails with a `503` when:

- the home is behind CGNAT;
- UPnP/NAT-PMP is disabled or unsupported;
- an ISP or recipient network blocks the selected direct port;
- DDNS is stale; or
- the recipient is on the same LAN and the router does not support hairpin NAT.

The last case is confirmed on the project owner's router: the control-plane probe can
reach the public mapping while the owner's browser cannot. Therefore a server-side
probe alone can never establish recipient-path reachability.

Phase 4a closes this availability gap. It is the minimum reliable path beneath the
existing product, not a new content feature and not the Phase 4b route-performance
optimizer.

## 3. Scope

### 3.1 In scope

- Top-level `relay/` service containing:
  - `sharebridge-relay-gateway`, the public L4 SNI router and FRP authorization/
    presence adapter;
  - a pinned `frps` release with a locked-down static configuration.
- An agent-owned tunnel manager supervising a pinned `frpc` child process.
- One persistent TCP proxy per agent from a gateway-local loopback port to the agent's
  existing HTTPS listener.
- Static `*.relay.<namespace>.sharebridgeusercontent.com` A records pointing at the
  relay gateway.
- Exact active relay-origin route distribution to the gateway.
- Gateway-authoritative tunnel presence, pushed to the control plane with a lease.
- Direct-first route selection with relay fallback.
- End-to-end support for the existing `relay_only` share flag.
- A control-hosted recipient interstitial that falls back after a bounded direct
  browser-connection timeout.
- A control-observed STUN cross-check before any public direct probe.
- Lockdown and shutdown behavior that stop the tunnel and close relayed streams.
- Agent-offline classification using relay tunnel presence for the relay path.
- Resource limits, operational metrics, systemd deployment, and failure recovery.

### 3.2 Deferred from Phase 4a

- **Measure-and-prefer:** Phase 4b; it requires the packet-level RTT, jitter, loss,
  bandwidth, and bottleneck-buffer harness described by the benchmark methodology.
- **On-demand direct DNS:** separate privacy spike using the existing route-selection
  seams; Phase 4a keeps the shipped direct DDNS behavior.
- **Manual endpoint override:** separate availability spike using `report_endpoint` and
  the same direct-eligibility seam.
- **Password and Nextcloud/OpenCloud parity restoration:** separate content/auth
  milestone; Phase 4a continues to serve the Phase 3 public Immich contract only.
- **CT monitoring and CAA:** control-plane quick win performed separately by the user;
  this design retains the trust-boundary warning and references the security review.

## 4. Architecture Questions and Decisions

### 4.1 Gateway composition

**Decision:** keep the settled `frps` + custom SNI gateway composition. Give every
agent exactly one FRP `tcp` proxy on a control-assigned loopback-only remote port:

```text
gateway exact SNI route
  → 127.0.0.1:<agent-relay-port>
  → frps TCP proxy
  → frpc
  → 127.0.0.1:8443 agent HTTPS server
```

FRP's built-in `https` proxy is itself an SNI passthrough router, so it is technically
capable of multiplexing TLS without termination. It is not selected because Phase 4a
also needs control-owned exact-share activation/revocation, active-stream termination,
per-IP/origin/agent limits, and a gateway presence lease independent of an agent-side
proxy configuration reload. Keeping those policies in the small ShareBridge gateway
avoids duplicating dynamic share state into FRP `customDomains` and preserves FRP as a
replaceable byte-tunnel implementation.

FRP `tcpmux` is not suitable because a normal browser does not issue an HTTP CONNECT
request before TLS. A new custom multiplexing protocol is rejected because it would
recreate transport machinery that FRP already supplies.

### 4.2 Relay availability authority

**Decision:** the gateway is authoritative. Agent-reported `frpc` state is telemetry,
not a routing fact.

The gateway exposes FRP's local authorization/presence plugin endpoint to `frps` for
`Login`, `NewProxy`, `CloseProxy`, and `Ping`. A tunnel is available only when all are
true:

1. the signed control-issued tunnel credential is valid;
2. `frps` has accepted the one expected TCP proxy on its assigned loopback port;
3. the gateway has observed a `Ping` within the 45-second presence lease;
4. the gateway has the active exact share route and its route revision is current.

The gateway pushes online/offline events to control. Control treats those events as an
ephemeral lease, never as durable truth from the `agents` row. On control restart it
loads a fresh gateway snapshot. On gateway restart the snapshot is empty until FRP
clients reconnect and re-register.

This definition lets an already-established relay continue across a temporary agent
control-WebSocket reconnect. Direct still requires the live WebSocket because it needs
an `open_signal`.

### 4.3 Interstitial ownership

**Decision:** `sharebridge.app` owns the interstitial. The agent cannot own a page whose
purpose is to recover when the browser cannot reach the agent.

The control page prepares direct, asks the browser to make a small CORS-enabled request
to the direct agent origin, and navigates to the relay origin if that request does not
complete in four seconds. It never loads content through control and never proxies a
recipient request.

### 4.4 Timeout budgets

**Decision:** use one overall four-second control-preparation budget and one four-second
recipient-path budget.

- `open_signal` + `open_ack` + control probe share a four-second context. Existing
  three-second component timeouts may remain but cannot exceed the overall budget.
- The interstitial direct `GET /s/<code>/connect` has a four-second
  `AbortController` deadline.
- Gateway ClientHello read: five seconds.
- Gateway-to-FRP loopback connect: two seconds.

Warm direct opens normally complete far below these ceilings. The worst bounded delay
before relay navigation is approximately eight seconds. The UI displays progress and
an immediate “Use relay now” action; that action cancels the direct attempt and uses
the already-returned relay URL.

### 4.5 FRP configuration ownership

**Decision:** control owns tunnel policy and assignments; the agent owns process
lifecycle and renders the resulting configuration; operators own the static `frps`
configuration.

- Control allocates the stable relay port and signs a short-lived, epoch-bound
  credential containing agent identity, namespace, proxy name, port, generation, and
  expiry.
- The agent tunnel manager writes a mode-0600 generated config in its data directory,
  starts/stops the bundled pinned `frpc`, and never accepts arbitrary proxy definitions
  from a user or from control.
- The only allowed local target is the compiled/configured ShareBridge HTTPS listener
  at loopback `:8443`.
- `/etc/sharebridge/relay/frps.toml` is root-owned deployment configuration. It binds
  agent proxy ports to loopback, limits each client to one proxy, enables the mandatory
  authorization plugin, and does not expose an FRP dashboard publicly.

Embedding FRP internals as a Go library was considered and rejected for the MVP: it
would couple ShareBridge to non-public FRP APIs and make FRP upgrades part of the agent
build graph. A supervised child keeps the integration narrow and crash-isolated while
the user still installs one ShareBridge release artifact.

### 4.6 Deployment topology

**Decision:** deploy the MVP relay on a separate small Hetzner VM in the same region and
private network as control, using systemd for `sharebridge-relay-gateway` and `frps`.

Collocation is functionally possible, but the current control ingress already owns
public TCP 443. Sharing one address would require making the new gateway the outer
router for `sharebridge.app` as well, coupling control availability to bulk relay
traffic. A second VM gives the relay unambiguous ownership of 443, isolates file-traffic
CPU/NIC/file descriptors, and allows independent restart and scaling. This is justified
for the availability backbone even at MVP scale.

Development may collocate the processes using a second bound IP or nonstandard public
port. Production acceptance uses the separate relay VM.

## 5. High-Level Architecture

```text
                               CONTROL PLANE

 Agent  ◄──── authenticated WebSocket ────►  sharebridge.app
   │        enroll, cert, relay_config,       sessions, route selection,
   │        open_signal (direct only),        route distribution, STUN
   │        share registration                observation, interstitial
   │                                                │
   │                                                │ exact relay routes
   │                                                │ tunnel presence
   │                                                ▼
   │                                      sharebridge-relay-gateway
   │                                      + FRP auth/presence adapter
   │
   │ outbound FRP transport TLS
   ▼
 frpc ═════════════════════════════════════════════► frps


                                DATA PLANE

 Direct:
 Browser ── browser TLS ──► home public IP:port ──► agent HTTPS server

 Relay:
 Browser ── browser TLS ──► SNI gateway ──► frps ══► frpc ──► agent HTTPS server
                            (peek SNI only)          (same listener and handlers)
```

The browser-TLS envelope begins in the recipient browser and ends only in the agent.
FRP transport TLS is an additional outer protection on the agent-to-gateway leg; it
does not replace or terminate browser TLS.

## 6. Names, DNS, and Origin Binding

The Phase 2 certificate already contains both SANs:

```text
*.<namespace>.sharebridgeusercontent.com
*.relay.<namespace>.sharebridgeusercontent.com
```

Phase 4a provisions the second wildcard A record at enrollment:

```text
*.relay.<namespace>.sharebridgeusercontent.com → relay gateway public IPv4
```

It is static for the lifetime of the gateway assignment and does not change when a
share is created. The existing direct record remains:

```text
*.<namespace>.sharebridgeusercontent.com → agent public IPv4 via DDNS
```

The control plane continues storing the direct origin in `sessions.origin`:

```text
<origin>.<namespace>.sharebridgeusercontent.com
```

It deterministically derives the relay origin by inserting `relay` before the
namespace:

```text
<origin>.relay.<namespace>.sharebridgeusercontent.com
```

`share_registered` returns both values. The agent installs two bindings that point at
the same local content session:

```text
(direct origin, RouteDirect, code)
(relay origin,  RouteRelay,  code)
```

Revocation removes both bindings and the gateway's exact relay route. The gateway
never routes the bare namespace, the wildcard itself, an arbitrary child, or a
tombstoned origin. The agent independently repeats Host + route-kind + native-code
authorization after TLS termination.

The relay hostname is not a new credential. The existing native share code remains the
bearer capability, and no new content authentication model is introduced.

### 6.1 `relayOnly` privacy wording

`relay_only=true` guarantees that ShareBridge never selects, prepares, probes, or
navigates to the direct origin for that share. The recipient's TCP connection therefore
ends at the relay IP rather than the home IP.

It does **not** guarantee historical unlinkability of the agent's home IP when the same
agent namespace also has direct mode enabled: the shared wildcard namespace and public
CT/DNS history can permit correlation, as documented in the security review. Stronger
agent-wide no-direct-DNS mode and on-demand DNS are separate privacy work. UI copy must
say “Always use relay for this share” rather than promise anonymity.

## 7. Tunnel Enrollment, Authentication, and Lifecycle

### 7.1 Readiness is no longer direct-dependent

Phase 2 currently makes `enrollment_ready` depend on both TLS readiness and direct DDNS.
That deadlocks the exact agents Phase 4a must support: an agent without a port mapper
cannot report a usable direct endpoint and therefore never registers a share.

Phase 4a changes the invariant:

```text
baseline enrollment ready = current-epoch TLS ready + relay DNS provisioned
direct eligible            = baseline ready + live WS + STUN match + DDNS + endpoint
relay eligible             = baseline ready + active gateway tunnel-presence lease
```

An agent may register supported shares after baseline enrollment even when direct is
unavailable. Direct state becomes an optional route capability, not an enrollment gate.

### 7.2 Tunnel credential

After baseline enrollment, control sends `relay_config` with a signed credential. The
credential is short-lived for admission (default ten minutes), but an accepted tunnel
may remain connected beyond token expiry. A reconnect requires a fresh credential.

Normative claims:

```text
issuer, audience=sharebridge-relay, api_key_id, agent_record_id,
namespace, proxy_name, relay_port, generation, issued_at, expires_at, jti
```

Control signs with an Ed25519 relay-auth key. The gateway holds only the verification
key. The token is sent inside the authenticated agent WebSocket and then inside FRP
transport TLS metadata. It is never logged.

The gateway authorization adapter rejects:

- an invalid, expired, replayed, or superseded credential;
- a proxy user/name/port/generation mismatch;
- any proxy type other than `tcp`;
- more than one proxy for the agent;
- a port outside the assigned loopback-only range;
- compression or unapproved proxy-level options visible to the plugin.

FRP does not expose the client-side `localIP`/`localPort` to the server plugin. The
official agent therefore fixes the local target in its generated config, but this is
not treated as a cross-trust-boundary claim: a compromised agent could substitute a
different local TLS service. It would still be confined to its own exact active share
origins and certificate namespace, equivalent to that agent serving arbitrary content
from its normal handler.

FRP's single shared token is not distributed to agents. Login authorization is
mandatory through the fail-closed local plugin. FRP transport TLS verifies the relay
server identity; the relay may hold a dedicated tunnel-transport key, but it holds no
certificate or key valid for any browser content origin and no DNS/ACME credential.

### 7.3 Presence state machine

```text
absent
  └─ valid Login + exact NewProxy ─► online
online
  ├─ current Ping renews 45s lease ─► online
  ├─ CloseProxy / logout ──────────► absent
  ├─ lease expiry ─────────────────► absent
  └─ replacement generation ───────► fenced, then new online
```

Every event carries the gateway boot ID and a monotonically increasing revision.
Control discards events from an older boot/revision. Persisted `relay_last_seen_at` is
diagnostic only; route selection uses the in-memory leased view.

### 7.4 Agent process lifecycle

The tunnel manager:

1. validates `relay_config` shape and that the local target is the fixed HTTPS port;
2. atomically writes the generated mode-0600 config;
3. starts the pinned `frpc` child without a shell;
4. restarts it with bounded exponential backoff and jitter;
5. replaces it when a higher config generation arrives;
6. stops it on lockdown or daemon shutdown.

The agent may report `relay_client_state` for diagnostics, but control never uses that
message to mark relay available.

The local HTTPS server is moved out of the control-WebSocket epoch lifecycle. Once a
valid certificate is installed, it stays up until daemon shutdown or lockdown. This is
required so an established FRP tunnel can serve during a temporary control-WebSocket
reconnect. `SignalGate` still resets per control epoch and governs only direct port
opening.

## 8. Route Distribution and Gateway Routing

The gateway keeps two in-memory tables:

```text
exact relay hostname → agent record, relay port, session ID, limits, route revision
agent record         → tunnel generation, presence expiry, relay port
```

Control pushes a versioned full snapshot on gateway connection/startup, then ordered
add/revoke/limit deltas. The gateway rejects public traffic until the initial snapshot
is applied. It never queries PocketBase on a public connection.

A public connection is forwarded only when:

1. ClientHello parsing succeeds within size/time bounds;
2. SNI normalizes to an exact active relay hostname;
3. the hostname's route and the tunnel presence refer to the same agent, port, and
   generation;
4. connection limits admit the stream.

The gateway buffers the ClientHello bytes it inspected and writes them unchanged to
the selected loopback FRP port before copying the rest of the stream bidirectionally.
It indexes active streams by exact route. A route revoke or lockdown event closes all
matching streams immediately.

Unknown SNI, missing SNI, ECH that hides SNI, malformed ClientHello, inactive routes,
and absent tunnels receive a generic TCP close. The gateway cannot return an HTTP
offline page because it cannot terminate TLS.

The FRP proxy bind address is `127.0.0.1`; assigned proxy ports are unreachable from
the public network. The SNI gateway is the only process allowed to dial them. Thus
`frps` cannot become a generic public port-forwarder even if an agent attempts to
register an unexpected proxy.

## 9. Control-Plane Route Selection

### 9.1 Selection algorithm

For an active supported public Immich session:

```text
if relay_only:
    if relay available: redirect to relay
    else: show agent-offline/unavailable page

else if direct eligible:
    serve interstitial
    interstitial asks control to prepare direct
    if preparation succeeds:
        browser tests direct for at most 4s
        success → direct origin
        failure → relay origin if available, otherwise offline page
    else:
        relay origin if available, otherwise offline page

else if relay available:
    redirect to relay

else:
    show agent-offline/unavailable page
```

Phase 4a does not compare measured direct and relay performance. “Direct eligible” is a
boolean safety/availability predicate, not a speed score.

### 9.2 Relay path has no open signal

When route=`relay`, control does **not** call `EmitOpen`, allocate a nonce, wait for an
`open_ack`, run the direct reachability probe, or mutate direct port state. It checks
the gateway presence lease and constructs the relay URL.

The existing `open_signal` wire contract stays RouteDirect-only. The agent's
`SignalGate.Admit` continues rejecting any other route. This is an intentional
integration invariant, not an omission.

### 9.3 Interstitial flow

Both canonical routes retain their lifecycle statuses, but active route resolution is
split from navigation:

```text
GET /share/<code> or /s/<code>
  → unknown/revoked: 404
  → expired/unsupported: 410
  → relayOnly or direct-ineligible + relay-present: 302 relay
  → direct candidate: 200 no-store interstitial
  → no usable transport: 503 agent-offline page

POST /api/shares/<code>/prepare-route
  → live lifecycle re-check
  → direct open_signal + ack + STUN/verified-tuple check + probe, overall ≤4s
  → JSON containing control-constructed direct URL, optional relay URL,
    and directTimeoutMs=4000
```

The interstitial performs a CORS request to:

```text
GET https://<direct-origin>[:port]/s/<code>/connect
```

The agent returns `204`, `Cache-Control: no-store`, and
`Access-Control-Allow-Origin: https://sharebridge.app` only after normal SNI, Host,
route-kind, and code binding succeeds. This endpoint touches no content backend and
sets no cookie. A `204` completes the direct check; timeout or network/TLS failure
navigates to the relay URL. The page provides a manual immediate-relay action and
never embeds either content origin in an iframe.

The prepare response and page are non-cacheable. Control constructs both origins from
persisted origin/namespace state; neither an agent nor a request parameter can provide
an arbitrary redirect target.

### 9.4 Mixed direct/relay behavior

Transport selection is per canonical navigation, not per asset request. A page loaded
on one content origin keeps all relative requests on that origin. Control route changes
affect only a new canonical open.

If a direct connection fails after the page loads, the UI offers/retries through the
canonical URL, which can select relay. There is no transparent cross-origin migration
of an in-flight response in Phase 4a. Video can reissue Range requests after navigation;
an interrupted original/archive download may restart. A live direct stream and a live
relay stream for the same share may coexist, and both use the same content-session
limits and download ledger.

## 10. STUN Cross-Check

The Phase 2 control probe currently trusts an agent-asserted public IP before probing
it. Phase 4a adds an independent, control-observed STUN path and does not probe until
the comparison succeeds.

### 10.1 Flow

1. Control sends a short-lived `stun_challenge` over the authenticated agent WebSocket.
2. The agent sends a STUN Binding request to the configured control STUN listener on
   UDP 3478, using one-use short-term credentials and STUN `MESSAGE-INTEGRITY` to bind
   the request to the challenge.
3. The listener records the UDP source IP and returns the normal mapped address plus a
   fresh unpredictable receipt value bound to the challenge and transaction ID in the
   integrity-protected response.
4. The agent echoes that receipt in `stun_result` over the WebSocket.
5. Control accepts the observation only when the receipt, connection epoch,
   transaction, and expiry match.
6. Control compares the observed IP with both `report_endpoint.ip` and
   `open_ack.public_ip` before DDNS update or direct probing.

The receipt proves the agent received the response at the observed address and prevents
a blind source-spoofed UDP packet from manufacturing an observation. Challenges are
single-use, expire after 60 seconds, and are rate-limited per agent.

### 10.2 Policy

- Exact IPv4 match and a fresh observation (default five minutes) are required for
  direct eligibility.
- Mismatch, timeout, blocked STUN, private/reserved/CGNAT address, or stale observation
  makes the agent relay-only until a later observation succeeds.
- A mismatch stops before the HTTPS reachability probe, closing the Phase 2 SSRF/scanner
  gap.
- Relay availability is independent of STUN.

The policy deliberately prefers a working relay over guessing that asymmetric NAT or
multi-WAN behavior is safe for direct mode.

## 11. WebSocket and Internal Protocol Changes

All public-agent messages remain on the authenticated `/ws/agent` connection. Wire
types remain versioned, bounded, and tolerant of unknown fields.

### 11.1 New agent WebSocket messages

| Direction | Message | Payload / role |
|---|---|---|
| control → agent | `relay_config` | `{ version, generation, gateway_addr, gateway_port, proxy_name, relay_port, credential, expires_at }` |
| agent → control | `relay_client_state` | `{ generation, status: "starting"|"running"|"stopped"|"error", reason? }` — telemetry only |
| control → agent | `stun_challenge` | `{ version, challenge, server, expires_at }` |
| agent → control | `stun_result` | `{ challenge, transaction_id, receipt }` |
| agent → control | `lockdown_status` | `{ generation, locked: bool }` — changes route availability, not share lifecycle |
| control → agent | `lockdown_ack` | optional acknowledgement of control-side transport deactivation |

`share_registered` adds `relay_origin`. `origin` remains the direct origin. No agent
message may supply either origin.

### 11.2 Existing messages

- `open_signal` and `open_ack` remain direct-only and unchanged.
- `report_endpoint` remains direct state; a relay tunnel does not report a fake public
  endpoint or port.
- `tls_ready` now advances baseline enrollment independently of direct DDNS (§7.1).
- `enrollment_ready` means the agent can register supported shares and start its HTTPS
  server/tunnel. It no longer claims direct reachability.

### 11.3 Gateway ↔ control internal protocol

The separate Hetzner hosts use mutually authenticated private-network HTTP with:

- gateway boot ID and monotonic revision;
- full route/presence snapshot fetch;
- ordered route add/revoke/limit deltas;
- tunnel online/offline lease events;
- explicit acknowledgements and last-applied revision;
- bounded payloads and fail-closed authentication.

The interface is not exposed publicly. A lost delta triggers snapshot reconciliation;
neither side guesses across a revision gap.

## 12. Database Changes

A new forward migration extends `agents`:

| Field | Type | Meaning |
|---|---|---|
| `relay_port` | number | stable assigned gateway-loopback FRP port; unique when nonzero |
| `relay_generation` | number | increments whenever credentials/assignment are fenced |
| `relay_last_seen_at` | date | diagnostic gateway observation; never sufficient for routing |
| `stun_observed_ip` | text | last control-observed public IPv4 |
| `stun_observed_at` | date | observation freshness/audit timestamp |
| `direct_status` | text | `unknown`, `eligible`, or `relay_only` |
| `direct_status_reason` | text | bounded code such as `stun_mismatch`, `stun_timeout`, `no_mapper`, `probe_failed` |

Indexes:

- partial unique index on nonzero `relay_port`;
- the existing unique `api_key_id` and `namespace` indexes remain.

No new `sessions` field is required:

- `sessions.origin` remains permanent and tombstoned;
- relay origin is deterministic from it;
- `sessions.relay_only` becomes routable instead of being classified unsupported;
- route selection is per request and is not persisted as session state.

Tunnel online/offline is deliberately not a durable boolean. On process restart,
persisted presence is stale by definition and must be reacquired from the gateway.

## 13. Agent Changes and Shared Serving Contract

### 13.1 Binding and listener changes

The shipped `Binder` already distinguishes `RouteDirect` and `RouteRelay`. Phase 4a:

- allows both control-returned origins for every supported share;
- makes local authorization accept the same share for either route;
- keeps `SignalGate` authorization direct-only;
- re-allows both origins after namespace/listener rebuild;
- revokes both origins together.

The same TLS listener and `DirectServer` handlers serve both. Rename of Go types is not
required in this milestone; behavior, not package cosmetics, is normative.

### 13.2 Connection accounting

The listener derives route kind from admitted SNI and tracks it per connection:

- direct connections participate in `OnDemandPort` session/activity/hold accounting;
- relay connections do **not** open, renew, or hold the UPnP mapping;
- both participate in the existing content streaming semaphores, archive transaction
  rules, membership snapshots, and download accounting;
- the connection registry can close by route or close all during lockdown/revocation.

This prevents relayed video/HTTP keepalive traffic from accidentally keeping the home
router mapping open.

### 13.3 `relayOnly` restoration

Phase 3 intentionally rejected relay-only shares because no relay existed. Phase 4a
removes that temporary rejection for supported public Immich gallery shares at every
entry point:

- polling registration;
- manual creation;
- persisted-session restoration;
- control `register_share` validation;
- canonical route resolution.

Password-protected and Nextcloud/OpenCloud shares remain unsupported. Previously
tombstoned Phase 3 rows are not resurrected; zero-user v2 creates clean registrations.

### 13.4 Lockdown

Lockdown is an availability state, not permanent source-share revocation. Activating it
atomically/best-effort concurrently:

1. sets `SignalGate` lockdown so new direct opens fail;
2. removes the UPnP/NAT-PMP mapping;
3. stops `frpc`, causing gateway presence to expire/close;
4. removes local Binder admissions while retaining source/session state for unlock;
5. closes established direct and relay recipient connections;
6. reports locked state to control so canonical links show unavailable.

Unlock requires explicit local action, restores bindings from source-verified sessions,
starts the HTTPS listener/tunnel, and reacquires fresh transport presence. It does not
reuse an expired tunnel credential.

This clarification avoids tombstoning origins during a reversible emergency stop.
Actual share revocation remains permanent and follows the Phase 3 lifecycle.

## 14. Resource Bounds and Operational Limits

Defaults are configuration, covered by tests, and may be tuned from production
telemetry without changing the protocol:

| Resource | MVP default |
|---|---|
| ClientHello buffered bytes | 64 KiB maximum |
| ClientHello read time | 5 seconds |
| Gateway → loopback proxy connect | 2 seconds |
| No-byte stream idle timeout | 5 minutes |
| Absolute connection lifetime | 24 hours, hard close |
| Concurrent public streams per source IP | 16 |
| Concurrent streams per exact origin | 32 |
| Concurrent streams per agent | 64 |
| Global streams per gateway process | 8,192 or lower host file-descriptor budget |
| FRP proxies per agent | exactly 1 |
| Tunnel presence lease | 45 seconds, renewed by authenticated Ping |
| Gateway route lease | 120 seconds, refreshed every 30 seconds while control sync is healthy |
| Agent reconnect backoff | exponential with jitter, capped at 60 seconds |

Limits are enforced before opening an FRP user connection. Rejection is a generic TCP
close because the gateway cannot produce an HTTP response. Counters are decremented on
all close/error paths. Buffers are pooled and bounded; the gateway never buffers a
complete response or file.

Per-agent byte and active-stream counters are mandatory. FRP's server-side
`bandwidthLimitMode` remains available and the signed assignment may carry an operator
cap. Phase 4a ships with no product-tier bandwidth throttle by default so it can gather
honest relay throughput data; host egress, per-agent usage, and saturation alerts are
still measured. A hard emergency cap is operator-configurable without agent trust.

Compression is disabled for content traffic: files are commonly pre-compressed, and
compression wastes CPU and creates unnecessary content-dependent behavior at the
tunnel layer.

`frps` is configured with `maxPortsPerClient=1`, a narrow `allowPorts` range,
`proxyBindAddr=127.0.0.1`, forced verified transport TLS, bounded heartbeat/user-
connection timeouts, and no public dashboard. systemd sets `LimitNOFILE`, `MemoryMax`,
restart policy, and log-rate bounds for both relay processes.

## 15. Failure Handling

### 15.1 Gateway restart

The public listener stays closed until the gateway has authenticated with control and
loaded a full route snapshot. Gateway boot ID changes, invalidating old presence
events. `frps` is either restarted with it or all prior proxy presence is treated
absent. Agent `frpc` reconnects with jitter, re-registers its exact proxy, and only then
does control regain a relay-available lease. Canonical links show preparing/offline or
use direct during the gap; they never redirect based on persisted `relay_last_seen_at`.

### 15.2 `frps` restart without gateway restart

All tunnel presence is cleared immediately. Existing relayed streams terminate. Agent
clients reconnect and `NewProxy` re-establishes presence. Gateway route state remains
but is unroutable until the matching tunnel generation is online.

### 15.3 Agent restart

The tunnel drops, presence expires, and active streams close. On restart the agent
loads its namespace/certificate/session state, reconnects to control, obtains fresh
relay credentials, starts the local HTTPS server, hydrates Phase 3 content snapshots,
and starts `frpc`. Relay becomes selectable only after the gateway confirms the proxy.
Before snapshot hydration the existing agent resolver may return `503`; no stale
membership is invented.

### 15.4 Agent control-WebSocket loss

Direct cannot open a new port because no authenticated signal path exists. A healthy
existing relay tunnel and agent HTTPS listener may continue serving already-active
routes under finite gateway route leases. If control cannot renew route state, the
gateway fails closed when the route lease expires. Reconnect establishes a new
`SignalGate` epoch and fresh tunnel credentials without killing a healthy tunnel until
replacement succeeds.

### 15.5 Agent offline page

When neither direct can be prepared nor a fresh gateway tunnel lease exists, the
canonical control route returns the existing no-store agent-offline/unavailable page.
The gateway itself emits only a TCP close. A browser already on a content origin must
return to the canonical link to receive the offline page.

### 15.6 Direct failure and mixed sessions

- Failure before navigation: the interstitial falls back to relay within the bounded
  window.
- Failure after navigation: the UI returns through the canonical link; Phase 4a does
  not splice an in-flight response across origins.
- A direct IP/DDNS change does not affect relay.
- A relay restart does not affect an established direct TCP connection.
- Revocation closes both route kinds and all indexed gateway streams.

### 15.7 Stale or contradictory state

- Control says online, gateway says absent: gateway closes; control lease expires and
  stops redirecting.
- Agent says running, gateway says absent: unavailable; agent telemetry is ignored for
  selection.
- Gateway has tunnel, route missing/inactive: close; tunnel presence alone never makes
  an agent generic-routable.
- Route exists, generation/port mismatch: close and request snapshot reconciliation.
- STUN mismatch: never probe direct; use relay.

## 16. Security Properties and Threat Model

### 16.1 Required L4 confidentiality property

The public relay is Layer 4 only. It forwards the browser's TLS records and can never
decrypt them under normal operation:

- the browser content TLS private key is generated and stored only on the agent;
- no agent content key or certificate is copied to control, gateway, `frps`, or disk on
  the relay host;
- the gateway does not call a TLS server stack for recipient connections;
- it parses only the bounded ClientHello needed to extract SNI;
- it does not parse HTTP, cookies, paths, passwords, Range headers, filenames, or body
  bytes;
- it does not inject PROXY protocol or any bytes into the browser TLS stream in 4a;
- the bytes delivered to the agent's TLS listener are byte-identical to the browser TLS
  stream.

The relay can observe source/destination IPs, SNI, connection timing, duration, and byte
counts. These metadata are sufficient for routing and resource enforcement but not
content inspection.

### 16.2 Generic-proxy prevention

Defense in depth:

1. control allocates every origin, relay port, proxy name, and credential claim;
2. the FRP plugin permits one exact TCP proxy per authenticated agent generation;
3. `frps` binds agent proxy ports only on loopback;
4. the gateway dials only those assigned loopback ports;
5. public SNI must exactly match an active control-distributed share route;
6. the agent repeats exact SNI/Host/route/code binding and connector authorization;
7. the official agent configuration exposes no user/control option for selecting a
   local target; a compromised agent remains confined as described in §16.4.

### 16.3 Network and relay attacker

A passive network observer or honest-but-curious relay sees metadata and ciphertext.
A malicious relay may drop, delay, replay, truncate, or redirect ciphertext, but browser
TLS detects modification and certificate/SNI mismatch. Redirecting a stream to another
agent also fails that agent's SNI Binder and certificate check.

Volumetric attack can still exhaust the gateway or its uplink. Provider protection,
pre-tunnel limits, circuit breaking, and separate deployment reduce impact but do not
promise DDoS immunity.

### 16.4 Malicious agent

A malicious agent can serve malicious content to its own recipients, which is inherent
to the product. It cannot claim another agent's FRP port/name/generation, add an
arbitrary public route, route unknown SNI, or make the gateway dial an arbitrary
destination. STUN observation must match the public endpoint before control probes it.
Rate limits bound credential requests and reconnect churn.

### 16.5 Compromised control plane

The existing §11.3 limitation remains: control of parent DNS and ACME credentials could
obtain a replacement browser certificate and repoint DNS. L4 relay does not worsen or
solve that WebPKI trust boundary. CT monitoring, CAA, credential isolation, and
least-privilege DNS tokens are the separate mitigations named in the security review.

The relay gateway is deliberately not given parent-zone DNS or ACME credentials. Its
dedicated FRP transport identity cannot authenticate a browser content origin.

### 16.6 Logs and telemetry

Relay logs may include agent record ID, proxy generation, normalized SNI hash or exact
origin under the existing operational policy, source IP under bounded retention,
timestamps, close reason, and byte counts. They must never include:

- native share codes or full HTTP paths;
- cookies, passwords, headers, or body fragments;
- tunnel credentials;
- agent TLS private keys or certificate material;
- buffered ClientHello bytes.

## 17. Deployment and Operations

### 17.1 Relay VM

The production MVP VM runs:

```text
sharebridge-relay-gateway.service
  public: 0.0.0.0:443/tcp
  private: control sync + local FRP plugin endpoint

sharebridge-relay-frps.service
  public: <relay-tunnel-host>:7000/tcp (FRP transport TLS)
  local:  127.0.0.1:<assigned-range>/tcp (agent proxies)
```

Only 443 and the FRP transport port are publicly firewalled open. Assigned proxy ports,
plugin endpoints, metrics, health, and admin surfaces are private/loopback. Services run
as distinct unprivileged users with read-only filesystem policy except their explicit
state/run directories.

The gateway must pass health only after route snapshot readiness and control sync.
`frps` health is separate. Deployment/restart ordering never treats process-up as
tunnel-present.

### 17.2 Control deployment

Control gains the internal relay sync client/server, STUN UDP listener, route selector,
and interstitial assets. Its existing HTTP server remains out of the bulk byte path.
Static relay DNS records are created through the existing provider abstraction during
enrollment. Cloudflare's record ceiling remains a scaling trigger for Route53, not an
MVP blocker.

### 17.3 Metrics

Required metrics:

- tunnels online, offline, reconnecting, and presence-lease expirations;
- route snapshot/delta revision and propagation lag;
- public connections accepted/rejected by reason;
- concurrent connections by IP/origin/agent and global total;
- bytes by agent/origin and gateway NIC saturation;
- ClientHello parse timeout/oversize/malformed counts;
- gateway-to-FRP connect latency/failure;
- direct-preparation and browser-fallback rates;
- STUN match/mismatch/timeout counts;
- time from gateway/frps restart to tunnel restoration;
- route revocation to active-stream close latency.

No metric requires plaintext content.

## 18. Testing Strategy

### 18.1 Unit tests

**Gateway:**

- fragmented/multi-record ClientHello SNI parsing within 64 KiB;
- missing SNI, ECH-hidden SNI, malformed and oversized hello fail closed;
- exact active origin succeeds; bare/wildcard/random/tombstoned origin fails;
- inspected prefix is replayed byte-for-byte;
- route/tunnel generation mismatch fails;
- IP/origin/agent/global limits and all counter-release paths;
- idle timeout and bounded connect timeout;
- route revoke closes indexed active streams;
- FRP plugin accepts only the signed exact one-proxy assignment;
- old token/generation/revision/boot ID is fenced.

**Control:**

- complete route-selection matrix (`relayOnly`, direct eligible/ineligible, relay
  present/absent, lifecycle statuses);
- relay selection never emits `open_signal` or runs a direct probe;
- direct preparation observes the overall timeout;
- interstitial response/JSON is no-store and redirect targets are control-derived;
- gateway snapshot and revision-gap reconciliation;
- durable fields never make stale tunnel presence available;
- relay port allocation uniqueness and API-key rotation preservation;
- STUN receipt, freshness, spoof/mismatch/timeout, and no-probe-on-mismatch.

**Agent:**

- managed `frpc` config has fixed local target, one proxy, correct generation, 0600
  permissions, and no shell expansion;
- reconnect/backoff/credential replacement/lockdown;
- both origins bind to one content session and revoke together;
- relay traffic does not open or hold the direct mapping;
- local HTTPS server survives control-WebSocket reconnect;
- `relay_client_state` cannot affect route availability;
- restored/polled/manual relay-only public Immich shares are accepted while deferred
  share types remain rejected.

### 18.2 Integration tests

A hermetic suite runs real pinned `frps` and `frpc`, the gateway, control, agent HTTPS
server with a test certificate, and a fake Phase 3 content backend:

```text
register share
→ receive direct + relay origins
→ gateway route snapshot
→ tunnel NewProxy/presence
→ TLS through gateway
→ page/items/thumb/preview/original/playback/archive
```

It verifies HTTP status, headers, byte equality, Range/seek, cancellation, download
accounting, concurrency limits, revocation, and tunnel restart. Existing Phase 3 parity
tests run unchanged against both direct and relay base URLs.

### 18.3 Recipient/browser tests

- interstitial direct success navigates direct without touching relay;
- simulated direct blackhole times out and navigates relay within the bound;
- manual “Use relay now” cancels the direct attempt;
- relay-only never issues a request to the direct origin;
- gallery/lightbox/video seeking/original/archive work over relay in the supported
  browser matrix;
- direct failure after load returns through the canonical route and recovers on relay;
- CSP has zero violations and the connect endpoint exposes no content/cookie.

### 18.4 L4 passthrough proof

The required confidentiality test sends unique canary strings in an HTTP header, path,
and request/response body through real browser TLS over gateway + FRP while capturing:

1. gateway public-interface packets;
2. gateway-to-`frps`/loopback packets and gateway buffers/logs;
3. agent post-TLS handler input.

The canaries must be absent from every gateway capture, buffer, and log, yet present at
the agent handler. The agent must observe the original SNI/Host and byte-exact body.
The test also asserts the relay host contains no content-origin key/certificate and that
the gateway process has no configured TLS acceptor on public 443.

FRP transport TLS may add an outer envelope on the tunnel leg; the browser TLS record
stream delivered to the agent after FRP decapsulation must still be byte-identical.

### 18.5 Failure and load tests

- kill/restart gateway and `frps` independently;
- restart agent during idle and active transfers;
- drop the agent control WebSocket while keeping the tunnel alive;
- expire presence without `CloseProxy` and verify bounded offline detection;
- revoke a share during a long relay transfer;
- force lockdown during direct and relay transfers;
- saturate per-IP/origin/agent/global limits without cross-agent starvation;
- stream longer than the five-minute idle window while bytes remain active;
- pause all bytes past idle and verify closure;
- confirm no unbounded goroutine, file-descriptor, or buffer growth.

Phase 4a load tests establish safety and a baseline only. They do not choose the faster
route. Phase 4b must use bandwidth-bounded packet impairment; RTT/loss-only loopback
tests previously produced a false SCTP conclusion.

## 19. Milestone Acceptance Tests

Phase 4a is complete only when all automated gates pass and the live topology passes
the applicable manual network cases:

1. **Owner hairpin:** the owner opens their own canonical share link on the same LAN
   behind the confirmed non-hairpin router. The control probe may approve direct, the
   browser direct check times out, and the same share loads over the relay hostname.
2. **Cellular/no direct path:** a phone on cellular with the direct route deliberately
   blocked or unavailable loads the gallery and a video over relay, including multiple
   valid `206` seeks.
3. **`relayOnly` end to end:** a supported share with `relay_only=true` registers,
   canonical resolution selects the relay, no direct DNS lookup or mutation, probe,
   open signal, or browser request is initiated for that access, and content succeeds.
4. **Interstitial fallback:** a deterministic browser test blackholes the direct
   endpoint after successful control preparation; within the documented bound it
   navigates to the exact relay hostname and serves the same content.
5. **No plaintext at gateway:** the §18.4 canary/capture test proves the gateway sees
   only TLS ciphertext plus permitted metadata.
6. **CGNAT/UPnP-off enrollment:** an agent with no mapper completes baseline enrollment,
   registers a public Immich share, establishes only its outbound tunnel, and serves it.
7. **No relay open-signal:** an integration assertion proves route=relay generates no
   `open_signal`, `open_ack`, direct probe, or port-mapper call.
8. **Exact routing:** unknown/random/bare/tombstoned SNI never reaches any agent; a valid
   exact route reaches only its owning agent.
9. **Restart recovery:** gateway, `frps`, and agent restart cases restore availability
   only after fresh authoritative presence and never route on stale DB state.
10. **Lockdown:** one action drops direct mapping and FRP tunnel, closes both kinds of
    active connection, and makes canonical links unavailable until explicit unlock.
11. **Content parity:** Phase 3 gallery, preview, original, archive, accounting, and
    Range/seek tests pass unchanged through relay.
12. **STUN mismatch:** a mismatch marks direct relay-only, prevents the public probe,
    and successfully routes through relay.

## 20. Rollout and Compatibility

1. Deploy relay gateway/`frps` dark, with public 443 and transport port firewalled but
   no DNS/routes.
2. Deploy control schema/protocol support and gateway snapshot sync.
3. Provision static relay wildcard DNS for enrolled test namespaces.
4. Deploy the agent tunnel manager and dual-origin binding.
5. Pass hermetic integration, browser fallback, plaintext, restart, and limit tests.
6. Run live Hetzner + real Immich acceptance, including owner hairpin and cellular.
7. Enable automatic fallback for test accounts, then all v2 accounts.

There are zero external v2 users and no legacy compatibility requirement. Phase 4a does
not restore deleted v1 protocol code, Noise, WebRTC, SCTP, or service-worker transport.
Rollback disables relay selection and route distribution; direct Phase 3 behavior
remains available. Database additions are forward-compatible and can remain unused.

## 21. Alternatives Considered

### 21.1 FRP built-in HTTPS virtual host only

Technically preserves TLS passthrough and removes one process. Rejected for 4a because
dynamic exact-share lifecycle, active-stream revocation, source-IP/origin/agent resource
limits, and gateway-authoritative presence would either be lost or coupled to frequent
agent FRP configuration reloads. The custom gateway keeps policy under ShareBridge
control while FRP remains a narrow tunnel.

### 21.2 One public FRP TCP port per agent

Simpler than SNI routing, but leaks routable generic ports, puts the port into recipient
URLs, weakens exact share-level edge revocation, and cannot distinguish active origins
before traffic reaches the agent. Rejected.

### 21.3 Custom multiplexed tunnel instead of FRP

Could combine SNI routing, presence, and streams in one binary. Rejected for MVP because
it recreates authentication, multiplexing, flow control, reconnect, and backpressure.
There is no evidence that FRP's raw TCP path is inadequate, and the architecture keeps
it replaceable if later measurements show otherwise.

### 21.4 Collocate relay with control

Acceptable for development or with a second bound public IP. Rejected for production
MVP because public 443 is already control ingress and bulk relay traffic would share its
failure/resource domain. A separate same-region Hetzner VM is a small operational cost
for a cleaner availability boundary.

### 21.5 Agent-reported relay readiness

Rejected because a stale, compromised, or partitioned agent can claim a tunnel that
`frps` does not have. Only gateway-observed proxy registration plus a fresh heartbeat
lease is authoritative.

### 21.6 Agent-hosted or gateway-hosted fallback page

Agent-hosted cannot recover from agent unreachability. Gateway-hosted would require TLS
termination or a gateway content certificate, violating the core threat model. The
control-hosted interstitial is the only layer reachable before either content path.

## 22. Decisions Recorded

- Use a custom public L4 SNI gateway plus pinned `frps`.
- Use one stable, loopback-only FRP TCP proxy port per agent.
- Supervise a pinned `frpc` child behind an agent `TunnelManager`; do not import FRP
  internals into the agent.
- Keep browser TLS end-to-end to the agent; give relay no content cert/key or ACME/DNS
  capability.
- Use the amended relay hostname and existing two-SAN agent certificate.
- Derive relay origin from the permanent direct origin; store no second session origin.
- Register both route kinds in the same Binder and serve the identical Phase 3 handler.
- Keep `open_signal`, `open_ack`, `SignalGate`, public endpoint probe, and UPnP strictly
  direct-only.
- Define relay availability from gateway-observed proxy registration + fresh Ping lease
  + active route revision.
- Keep tunnel presence ephemeral; persisted last-seen is diagnostics only.
- Decouple baseline enrollment from direct DDNS/UPnP readiness.
- Put the bounded fallback interstitial and offline page on `sharebridge.app`.
- Use four-second preparation and recipient-path budgets, with immediate manual relay.
- Land the control-observed STUN cross-check before any direct public probe.
- Restore `relayOnly` for public Immich shares and document its precise privacy limit.
- Keep agent HTTPS/tunnel alive across control-WebSocket reconnects under finite leases.
- Make lockdown stop the tunnel, close both route kinds, and remain reversible without
  tombstoning shares.
- Deploy relay on a separate same-region Hetzner VM under systemd for the production
  MVP.
- Defer performance-based selection to Phase 4b's packet-level impairment work.

## 23. Validation Gates Before Implementation Commitment

These are evidence gates, not unresolved product choices:

1. Prove the selected pinned FRP release invokes fail-closed `Login`, `NewProxy`,
   `CloseProxy`, and `Ping` plugin operations with the metadata and disconnect behavior
   assumed by §7.
2. Prove `proxyBindAddr=127.0.0.1`, `allowPorts`, one-proxy enforcement, transport TLS
   verification, and server-side bandwidth configuration on the target release.
3. Prove fragmented ClientHello replay through gateway → FRP → agent for browser TLS
   1.2/1.3 and HTTP/1.1 + HTTP/2.
4. Prove the four-second browser check is reliable in supported Safari/iOS, Chrome, and
   Firefox and that CORS `204` is observable without cookies.
5. Prove the STUN authenticated extension/receipt works through representative NATs and
   fails closed under spoof, timeout, and mismatched egress.
6. Measure one-agent and multi-agent relay throughput/CPU/memory only to establish
   capacity and safe default limits; do not turn the result into Phase 4b selection.
7. Confirm separate-VM private-network control sync and restart ordering on the actual
   Hetzner environment.

Failure of gates 1–5 blocks enabling automatic relay fallback. It does not authorize a
plaintext-terminating proxy or custom SCTP/Noise revival as a shortcut.

## 24. Self-Review and Expected Fresh-Reviewer Concerns

The document was checked against the shipped Phase 2/3 code and contracts. There are no
placeholder decisions or implicit route behaviors. A fresh reviewer is expected to
focus on:

1. **Why not FRP built-in HTTPS?** §4.1 and §21.1 justify the custom gateway with
   ShareBridge-specific route lifecycle and resource policy, while acknowledging FRP's
   L4 HTTPS capability.
2. **Can relay work when UPnP is absent?** §7.1 explicitly removes DDNS/UPnP from the
   baseline enrollment gate; this is a required correction to current code.
3. **Is tunnel presence actually authoritative?** §4.2/§7.3 make gateway events leased
   and ephemeral and define restart reconciliation rather than trusting agent or DB.
4. **Does relay accidentally open/hold the direct port?** §13.2 requires route-aware
   connection accounting at the shared listener.
5. **Does the interstitial prove recipient reachability?** §9.3 uses a real cross-origin
   agent `204` with a browser-local deadline; the control probe alone is not treated as
   sufficient.
6. **Can a malicious agent claim another tunnel?** §7.2 binds a signed short-lived
   credential to the exact identity/name/port/generation and enforces it in the local
   FRP plugin.
7. **Can the gateway decrypt?** §16.1 and §18.4 define both the architectural absence of
   keys/termination and a packet/canary proof.
8. **What does `relayOnly` really hide?** §6.1 avoids overstating privacy under the
   shipped shared namespace/DDNS model and keeps stronger DNS privacy out of 4a.
9. **Why a separate box?** §4.6 identifies the existing 443 conflict and isolates the
   availability backbone from control and bulk traffic.
10. **Are timeout/limit numbers product law?** §4.4/§14 make them tested MVP defaults,
    observable and tuneable without changing wire semantics.
11. **What happens mid-transfer?** §9.4/§15 state the honest boundary: new canonical
    navigation recovers; 4a does not migrate an in-flight cross-origin response.
12. **Could the STUN design become bespoke security protocol?** §10 keeps it narrow—an
    authenticated observation feeding a boolean direct-eligibility check—and gate 5
    requires proof before enablement.

## 25. References

- `docs/superpowers/specs/2026-07-25-native-https-tls-passthrough-design.md`
- `docs/superpowers/specs/2026-08-14-direct-tcp-mode-design.md`
- `docs/superpowers/specs/2026-08-15-phase2-direct-mode-transport-design.md`
- `docs/superpowers/specs/2026-08-16-phase3-content-serving-design.md`
- `docs/security-review-v2-direct-2026-09-02.md`
- `docs/superpowers/spikes/2026-08-15-dns-record-limit.md`
- `docs/superpowers/spikes/2026-08-15-direct-tcp-benchmark-results.md`
- Historical benchmark detail: `agent/cmd/benchdirect/BENCH_RESULTS.md` at commit
  `c8295eb` (deleted with the v1 transport at `dcaf1ff`)
- FRP HTTPS passthrough: <https://gofrp.org/en/docs/examples/vhost-http/>
- FRP TCP proxy: <https://gofrp.org/en/docs/features/tcp-udp/>
- FRP server plugins: <https://gofrp.org/en/docs/features/common/server-plugin/>
- FRP server configuration: <https://gofrp.org/en/docs/reference/server-configures/>
- FRP proxy configuration and health/limits: <https://gofrp.org/en/docs/reference/proxy/>
- FRP transport TLS: <https://gofrp.org/en/docs/features/common/network/network-tls/>
