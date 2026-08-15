# Phase 2 — Direct-Mode Transport (Agent HTTPS Server + Control Wiring)

**Date:** 2026-08-15
**Status:** Approved (design) — pending implementation plan
**Companion to:** `docs/superpowers/specs/2026-08-14-direct-tcp-mode-design.md` (the "v2 direct-TCP" spec this phase implements)
**Implements:** the transport core of the v2 direct data plane — the agent HTTPS server, cert lifecycle, enrollment, endpoint reporting, and the control-plane open-signal/redirect loop.

## 1. Summary

Phase 1 proved the building blocks and settled the throughput model. Phase 2 is the
**transport-first milestone**: wire the validated primitives into a real, end-to-end
direct path — a browser reaches the agent over native HTTPS, with a real Let's
Encrypt certificate and an on-demand UPnP/NAT-PMP port, and downloads a file — with
the control plane coordinating enrollment, certificate issuance, DDNS, and the
open-signal/redirect flow. Content is a **placeholder** (a synthetic file); the real
content-serving rewrite and the recipient UI are the next phase (item 2).

**In scope:**
- Agent cert lifecycle (key custody → CSR → control-plane ACME → validate → install → renew).
- Agent direct HTTPS server (`direct.Binder` + `direct.OnDemandPort` + installed cert).
- Agent endpoint reporting (public IP:port).
- Control-plane enrollment (namespace assignment) + `agents` schema.
- Control-plane open-signal emission → `open_ack` → 302 redirect.
- Control-plane DDNS trigger on IP change.
- Placeholder content served over the direct path.

**Out of scope (deferred):**
- Native-HTTP content serving + recipient UI (item 2; the UI is copied from `signaling-server/web/`).
- Measure-and-prefer per recipient (item 4).
- Relay fallback / FRP tunnel (Phase 3); this milestone is **direct-only**.
- `signaling-server/` → `control/` rename + `relay/` layout (Phase 3).

## 2. Architecture & Components

Two new pieces on the agent, four on the control plane, one schema change, all
speaking over the existing authenticated agent WebSocket. The Phase-1 libraries
(`agent/internal/direct`, `agent/internal/cert`, `certcoordinator`, `ddns`) are
wrapped, not rewritten.

### Agent (new)
1. **Cert lifecycle manager** — key custody, CSR generation, chain validation +
   install + atomic reload, renewal. Reuses `cert/csr.go` + `cert/validate.go`.
2. **Direct HTTPS server** — a TLS listener combining `direct.Binder` (SNI +
   origin→share admission) + `direct.OnDemandPort` (the public port) + the installed
   cert, serving placeholder content.
3. **Endpoint reporter** — reports `publicIP:port` over the WS on open / close /
   IP-change.

### Control plane (new)
4. **`agents` collection** — per-agent state (schema in §3).
5. **Enrollment handler** — on `hello`: load-or-create the agent, assign a namespace
   if new, send `enrolled`.
6. **Cert coordinator (WS-integrated)** — on `csr_submit`: validate the CSR, run ACME
   DNS-01 (lego), push `cert_issue`/`cert_error`.
7. **Open-signal emitter + redirect** — on `/s/<code>`: resolve agent + origin, send
   `open_signal`, wait `open_ack`, 302 to the direct hostname.
8. **DDNS trigger** — on endpoint report with a changed IP, update the wildcard A record.

### WebSocket protocol (extend the existing `/ws/agent`)

| Direction | Message | Payload |
|---|---|---|
| agent → control | `csr_submit` | `{ csr_pem }` |
| agent → control | `report_endpoint` | `{ ip, port }` (`port: 0` = closed) |
| agent → control | `open_ack` | `{ share_id, granted_port, public_ip }` |
| agent → control | `register_share` | *(existing) + `origin` field* |
| control → agent | `enrolled` | `{ namespace }` |
| control → agent | `cert_issue` | `{ chain_pem }` |
| control → agent | `cert_error` | `{ reason }` |
| control → agent | `open_signal` | `{ version, agent_id, share_id, route, nonce, seq, expires_at, lease }` |

`share_id` is the share code; the agent maps it to its registered origin via the
`Binder`/`shareAuthorizer`.

## 3. Schema

New `agents` collection — **1:1 with `api_keys` via a unique `api_key_id` relation**
(CascadeDelete). Keyed by the *authenticated* api_key identity, never the
self-reported `agent_id`.

| Field | Type | Notes |
|---|---|---|
| `api_key_id` | relation → `api_keys` | unique, CascadeDelete |
| `namespace` | text | unique, random (`sb` + hex), assigned at enrollment |
| `endpoint_ip` | text | last reported public IP |
| `endpoint_port` | number | last reported port; **0 = closed**; informational only |
| `cert_status` | text | `pending` / `ready` |
| `cert_expires_at` | date | leaf NotAfter |
| `last_report_at` | date | last endpoint report |

`sessions` gains `origin` (text) — the random per-share origin hostname
(`<random>.<ns>.sharebridgeusercontent.com`), assigned by the agent at `register_share`.

**DNS carries the IP, the redirect URL carries the port, the DB carries the origin and
namespace bookkeeping.** There are exactly two wildcard A records per namespace
(`*.<ns>` and `*.relay.<ns>`); no per-share DNS. `endpoint_port` is informational
only — the redirect always uses the live `open_ack` granted port (§5).

## 4. Enrollment & Cert Lifecycle

Enrollment is a **startup gate**: the agent cannot register direct shares until it is
enrolled and cert-ready.

**First connect:**
1. `hello { agent_id }` (API-key authenticated). Control loads `agents` by
   `api_key_id`; missing → create with a fresh random namespace, `cert_status =
   "pending"`.
2. Control → `enrolled { namespace }`. Agent persists the namespace.
3. Agent reports its public IP (`report_endpoint { ip, 0 }`) → control runs DDNS to
   create/update the wildcard A record. *(Parallel with cert issuance — the A record
   serves the browser; ACME uses TXT records, independent.)*
4. Agent generates an ECDSA P-256 key (never leaves the agent) + a 2-SAN wildcard CSR
   (`*.ns` + `*.relay.ns`) → `csr_submit`.
5. Control validates the CSR (§6) and runs ACME DNS-01 (lego) in a background
   goroutine (10–60s) → `cert_issue { chain }` or `cert_error { reason }`.
6. Agent validates the chain (`cert.ValidateChain`: public-key equality, exact SANs,
   serverAuth EKU), installs it, persists key+chain to its data dir, atomically
   reloads the TLS config (no listener restart).
7. When **both** DDNS and cert are complete → `cert_status = "ready"`. The agent may
   now register direct shares.

**Restart/reconnect:** namespace + key + chain are persisted; a reconnect re-sends
`enrolled` and the agent skips the CSR if its chain is still valid.

**Renewal (agent-driven):** when the leaf `NotAfter` is < ~30 days out, the agent
submits a fresh CSR (reusing the same key); control re-issues; agent installs +
reloads. No control-side push.

**Control-side requirement:** the ACME account key must be **persisted** (the Phase-1
spike generated an ephemeral one each call — see the `acmeAccount` comment). This is
required for correct renewals.

## 5. Endpoint Reporting, DDNS, and the Runtime Share Flow

### Endpoint reporting & DDNS
The agent reports `{ ip, port }` whenever its endpoint changes. On **port open** the
`open_ack` itself carries the fresh `public_ip` + `granted_port` (serving as the
open report); `report_endpoint` covers the remaining transitions — enrollment
(port 0), IP change (periodic external-IP check via UPnP `GetExternalIPAddress`,
STUN fallback), and port close (port 0). Control stores the values; on IP change it
runs DDNS to update the wildcard A record (TTL 60s).

### Share creation & origin
At `register_share`, the agent generates a random **origin**
(`<random>.<ns>.sharebridgeusercontent.com`), registers
`binder.Allow(origin, direct, code)` locally, and reports `origin` in the message.
The agent stays authoritative over its bindings.

### Runtime flow (canonical link → direct HTTPS)
```
recipient → GET https://sharebridge.app/s/<code>
  1. control resolves session → api_key_id (agent) + origin
  2. guard: agent enrolled + cert ready + connected, else "direct unavailable"
  3. control builds OpenSignal { agent, share=code, route=direct, nonce, seq, lease, expiry }
     (lease short, e.g. 120s — v2 spec §5; SignalGate bounds it ≤15 min)
  4. control sends open_signal over WS; waits for open_ack (timeout ~2–3s)
  5. agent: SignalGate.Admit (version/agent/expiry/nonce/seq/lockdown/local-authz/rate-limit)
          → OnDemandPort.OpenFor(code, lease)
  6. agent → open_ack { share_id, granted_port, public_ip }   ← live port from THIS open
  7. control 302 → https://<origin>[:granted_port]/s/<code>   (port omitted if 443)
  8. browser → agent; Binder admits SNI, authorizes Host + code; serves placeholder
```

**The port is dynamic** (NAT-PMP may remap; the preferred port may be occupied on a
later open). The redirect is built **only** from the live `open_ack` granted port —
never from `agents.endpoint_port`, which is informational.

**The control plane is stateless w.r.t. port-open state.** It **always** sends the
open-signal and waits for the ack; it never "skips because the DB says the port is
open" (a stale-DB trap). Latency is recovered on the agent side (below), not by
skipping.

### Concurrency & the `OpenFor` fast-path
One external port multiplexes all of the agent's shares; `OnDemandPort` tracks each
TLS connection as a session (`BeginSession`/`Activity`/`EndSession`) and keeps the
mapping open while any session is active, closing on idle timeout after the last.

**Phase-2 change to `OnDemandPort.OpenFor`:** today it re-runs `AddPortMapping` on
every call. Change it to a **no-op fast-path when the port is already open** — ack
immediately without touching the router (the renewal loop already keeps the router
lease fresh). This makes repeat open-signals cheap idempotent acks and removes the
~1–3s UPnP latency for everyone after the first recipient (warm opens ≈ one WS
round-trip).

**Rate limit:** `SignalGate` currently caps 3 signals/share/min, which would reject
concurrent recipients of a hot share. Drop the tight per-share cap in favor of a
generous global ceiling (e.g., ~60/min) so legitimate bursts pass; the limit's real
job is bounding cold-opens/abuse, and a no-op ack is cheap. Exact numbers land in
the plan.

### Placeholder content
The agent serves a minimal HTML page (showing the share is being served P2P over
direct HTTPS, with the origin and granted port) plus a synthetic byte stream (size
as a query parameter, so throughput can be eyeballed). The handler strips the
`/s/<code>` prefix and routes the remainder — deliberately decoupled from the real
gallery UI (item 2).

## 6. Security Properties

### Key custody & cert issuance
The agent holds its TLS private key locally; only the CSR (public material) crosses
the wire. The control plane proves domain ownership via DNS-01 and signs the agent's
public key. The agent verifies the returned chain matches its key and exact SANs.

**CSR authorization (cross-tenant boundary):** `CompleteCSR` enforces, before
issuance, that the CSR (a) is self-signed (proves key possession), (b) requests
**exactly** the enrolling agent's two wildcard SANs (no extras, no IP/email/URI SANs),
and (c) has a CommonName that is empty or one of the two authorized names (blocks
CN-smuggling). This is what stops a malicious agent from obtaining a cert for another
agent's namespace, the parent zone, or the control-plane domain.

### Threat model — malicious agent
A malicious (or compromised) agent is confined to its own namespace:
- **Cert:** can only obtain certs for its own two wildcard SANs (§6 CSR authorization).
- **DNS:** DDNS is keyed off the authenticated namespace, so it can only move its own
  A record.
- **Content:** `Binder` admits only origins it has registered, so it can only serve
  its own shares.
- **SSRF:** the control-plane self-probe is bounded (public-IP-only, nonce echo, STUN
  cross-check).

Residual (accepted): a malicious agent can serve malicious content to its *own*
recipients (inherent — the agent is the content source), and can spam the control
plane (bounded by rate limits). The asymmetry is intentional: a malicious control
plane can MITM everyone (it is the TLS root of trust, spec §11.3 of the v2 design);
a malicious agent can only hurt itself.

## 7. Error Handling

- **Binder:** unknown SNI → handshake rejected; wrong Host / wrong code → 403
  (already implemented).
- **SignalGate:** expired / replayed / nonce-reused / not-authorised / rate-limited
  signals dropped.
- **Open-ack timeout or agent rejection** → control serves "direct unavailable" (no
  relay fallback this milestone).
- **Not enrolled / cert not ready / offline** → control does not emit a direct
  redirect; "direct unavailable".
- **Direct connection failure after redirect** (TLS timeout, stale DNS, path
  firewall) → recipient re-opens the canonical link (no server-side retry or
  interstitial yet — item 2).
- **Cert issuance failure** → agent retries with bounded backoff; direct shares not
  registered while `cert_status != "ready"`.

## 8. Testing

- **Unit:** cert lifecycle manager (CSR → validate → install → renew), endpoint
  reporter, each new WS handler (`enrolled`, `csr_submit`, `open_signal`,
  `report_endpoint`), the redirect handler, the DDNS trigger, the `OpenFor` fast-path.
  Phase-1 `direct/` + `cert/` tests already cover the primitives.
- **Integration:** promote `spike-e2e` into a real test — enroll → cert (staging CA +
  test DNS zone) → register share → open-signal → redirect → download. No production
  certs or live UPnP in CI.
- **Regression:** existing agent + signaling-server suites stay green.

## 9. Decisions Recorded

- Transport-first: placeholder content + full cert/enrollment/redirect loop; content
  serving + UI + measure-and-prefer + relay deferred.
- Direct-only (no relay fallback) this milestone.
- New `agents` collection, 1:1 with `api_keys` via unique `api_key_id`; `sessions.origin`.
- All new control messages ride the existing agent WebSocket (no new HTTP surface).
- Origin is agent-generated at `register_share`; the agent is authoritative over its
  bindings (the signal activates, never creates, a route).
- Control plane is stateless w.r.t. port state — always signals, always uses the live
  `open_ack` granted port for the redirect (never the DB port).
- `OnDemandPort.OpenFor` gains a no-op fast-path when already open; rate limit raised.
- Cert renewal is agent-driven (30-day threshold, key reuse); ACME account key
  persisted control-side.
- Enrollment is an automatic startup gate (API-key possession is the approval).

## 10. References

- v2 direct-TCP spec: `docs/superpowers/specs/2026-08-14-direct-tcp-mode-design.md`
- FRP native-HTTPS spec: `docs/superpowers/specs/2026-07-25-native-https-tls-passthrough-design.md`
- Throughput model: `docs/superpowers/spikes/2026-08-15-direct-tcp-benchmark-results.md`
- Phase-1 primitives: `agent/internal/direct/`, `agent/internal/cert/`,
  `signaling-server/internal/certcoordinator/`, `signaling-server/internal/ddns/`
