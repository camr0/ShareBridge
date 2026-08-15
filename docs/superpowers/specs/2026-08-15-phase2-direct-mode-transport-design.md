# Phase 2 — Direct-Mode Transport (Agent HTTPS Server + Control Wiring)

**Date:** 2026-08-15
**Status:** Approved (design) — revision 2 (incorporates design review findings)
**Companion to:** `docs/superpowers/specs/2026-08-14-direct-tcp-mode-design.md` (the "v2 direct-TCP" spec this phase implements)
**Implements:** the transport core of the v2 direct data plane — the agent HTTPS server, cert lifecycle, enrollment, endpoint reporting, and the control-plane open-signal/redirect flow.

## 1. Summary

Phase 1 proved the building blocks and settled the throughput model. Phase 2 is the
**transport-first milestone**: wire the validated primitives into a real, end-to-end
direct path — a browser reaches the agent over native HTTPS, with a real Let's
Encrypt certificate and an on-demand UPnP/NAT-PMP port, and downloads a file — with
the control plane coordinating enrollment, certificate issuance, DDNS, reachability
probing, and the open-signal/redirect flow. Content is a **placeholder** (a synthetic
file); the real content-serving rewrite and the recipient UI are the next phase
(item 2).

**In scope:**
- Agent cert lifecycle (key custody → CSR → control-plane ACME → validate → install → renew).
- Agent direct HTTPS server (`direct.Binder` + `direct.OnDemandPort` + installed cert).
- Agent endpoint reporting (public IP:port).
- Control-plane enrollment (namespace assignment) + `agents` schema.
- Control-plane open-signal emission → `open_ack` → reachability probe → 302 redirect.
- Control-plane DDNS trigger on IP change.
- Placeholder content served over the direct path.

**Out of scope (deferred):**
- Native-HTTP content serving + recipient UI (item 2; the UI is copied from `signaling-server/web/`).
- Measure-and-prefer per recipient (item 4).
- Relay fallback / FRP tunnel (Phase 3); this milestone is **direct-only**.
- `signaling-server/` → `control/` rename + `relay/` layout (Phase 3).
- STUN cross-check of the agent's reported public IP (the *independent* observation
  half of the v2 self-probe) — see §6.

## 2. Architecture & Components

Two new pieces on the agent, five on the control plane, one schema change, all
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
   `open_signal`, wait `open_ack`, probe reachability (cold opens), 302 to the direct
   hostname.
8. **DDNS trigger** — on endpoint report with a changed IP, update the wildcard A record.

### WebSocket protocol (extend the existing `/ws/agent`)

Wire types are normative: `lease_seconds` is an integer (seconds, not Go
`time.Duration`); timestamps are RFC3339 strings; `seq` is a `uint64`; `fingerprint`
is a lowercase SHA-256 hex digest of the DER leaf. Unknown fields are ignored;
unexpected values are a protocol error.

| Direction | Message | Payload |
|---|---|---|
| agent → control | `csr_submit` | `{ csr_pem: string }` |
| agent → control | `report_endpoint` | `{ ip: string, port: int, status?: "close_failed" }` — status present only for `close_failed`; `port: 0` = closed, `port > 0` = open |
| agent → control | `open_ack` | `{ share_id, nonce, seq, granted_port, public_ip, was_already_open: bool, status: "ok" \| "error", error?: code }` |
| agent → control | `tls_ready` | `{ fingerprint, not_after: RFC3339 }` |
| agent → control | `tls_error` | `{ reason }` |
| agent → control | `register_share` | *(existing)* |
| control → agent | `enrolled` | `{ namespace }` |
| control → agent | `enrollment_ready` | `{}` |
| control → agent | `cert_issue` | `{ chain_pem }` |
| control → agent | `cert_error` | `{ reason }` |
| control → agent | `open_signal` | `{ version, agent_id, share_id, route, nonce, seq, lease_seconds, expires_at: RFC3339 }` |
| control → agent | `share_registered` | *(existing)* `+ { origin }` |

The origin is **control-allocated** and returned in `share_registered` (§5). The
`open_ack` echoes `nonce` + `seq` so the control can correlate it to a specific
in-flight open request (§5). `share_id` is the share code.

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
| `cert_fingerprint` | text | SHA-256 of the latest issued leaf (generation tracking) |
| `cert_expires_at` | date | leaf NotAfter |
| `last_report_at` | date | last endpoint report |

`sessions` gains `origin` (text, **unique**) — the random per-share origin hostname,
**allocated by the control plane** at `register_share` (§5). Origins are permanent and
never reused: sessions are **soft-deleted** (marked inactive) rather than hard-deleted,
so the unique `origin` value is never released for reuse (or, equivalently, expired
origins move to a tombstone table).

**API-key rotation preserves the agent.** `agents` is keyed by `api_key_id`; rotation
is **ordered**: (1) create the new key record, (2) re-point `agents.api_key_id` to it,
(3) fence/disconnect the old key's socket (hub unregister uses compare-and-delete, so a
stale old-socket disconnect cannot unregister the replacement), (4) revoke the old key.
The namespace + cert survive. Revocation without a replacement cascade-deletes the
agent (fresh namespace on re-enroll — acceptable).

**Migration:** a **new forward migration** adds `agents` and `sessions.origin` with
unique indexes (`agents.api_key_id`, `agents.namespace`, `sessions.origin`). Do not
edit `1_create_collections.go`, which early-returns on existing databases.

**DNS carries the IP, the redirect URL carries the port, the DB carries the origin and
namespace bookkeeping.** There are exactly two wildcard SANs per namespace on one
certificate; only the direct `*.<ns>` A record is provisioned this phase — the
`*.relay.<ns>` A record is deferred with the Phase 3 gateway (the cert still includes
the relay SAN so the namespace is relay-ready). `endpoint_port` is informational only
— the redirect always uses the live `open_ack` granted port (§5).

## 4. Enrollment & Cert Lifecycle

Enrollment is a **startup gate**: the agent cannot register direct shares until it is
enrolled and cert-ready. Readiness is an explicit handshake, not inferred from ACME
returning a chain.

**First connect:**
1. `hello { agent_id }` (API-key authenticated). Control loads `agents` by
   `api_key_id`; missing → create with a fresh random namespace, `cert_status = "pending"`.
2. Control → `enrolled { namespace }`. Agent persists the namespace.
3. Agent reports its public IP (`report_endpoint { ip, 0 }`) → control runs DDNS to
   create/update the wildcard A record. *(Parallel with cert issuance — the A record
   serves the browser; ACME uses TXT records, independent.)*
4. Agent generates an ECDSA P-256 key (never leaves the agent) + a 2-SAN wildcard CSR
   (`*.ns` + `*.relay.ns`) → `csr_submit`.
5. Control validates the CSR (§6) and runs ACME DNS-01 (lego) in a bounded worker
   (§6) → `cert_issue { chain }` or `cert_error { reason }`.
6. Agent validates the chain (`cert.ValidateChain`: public-key equality, exact SANs,
   serverAuth EKU), installs it, persists key+chain to its data dir, atomically
   reloads the TLS config (no listener restart), then sends
   **`tls_ready { fingerprint, not_after }`** (or `tls_error`).
7. Control marks `cert_status = "ready"` only after **both** DDNS is provisioned **and**
   `tls_ready` is received; it then sends **`enrollment_ready`**. The agent may now
   register direct shares.

**Reconnect reconciliation:** on reconnect, `enrolled` is re-sent; the agent re-sends
`tls_ready` if its persisted chain is still valid (not expiring), else submits a fresh
CSR. The control **accepts `tls_ready` for any still-valid fingerprint it has issued
for this agent** — it does not gate on the latest generation (a lost `cert_issue`
that never reached the agent would otherwise stall reconciliation). If the agent's
reported fingerprint is behind the latest issued chain, the control re-delivers the
latest cached chain. Readiness is **connection-epoch-local**: a persisted
`cert_status = "ready"` does not by itself authorize redirects — the *current*
connection must have completed `enrolled` → `tls_ready` → `enrollment_ready`.

**Renewal (agent-driven):** when the leaf `NotAfter` is < ~30 days out, the agent
submits a fresh CSR (reusing the same key); control re-issues; agent installs +
reloads + re-sends `tls_ready`. `cert_fingerprint` lets the control ignore stale or
out-of-order generation acks.

**Issuance controls (shared CA quota is a shared resource):** the cert coordinator
must (a) persist the ACME account key (the Phase-1 spike generated an ephemeral one
each call), (b) singleflight per agent (one in-flight issuance), (c) bound global
concurrency, (d) enforce an issuance cooldown per agent, (e) dedupe by CSR fingerprint,
and (f) cap CSR/chain payload sizes. The CSR fingerprint is the SHA-256 of the DER CSR;
an idempotent retry of the same fingerprint returns the cached issued chain without
re-issuing or counting against the cooldown (chains are cached, persistently or in
memory, keyed by fingerprint). `ACMEConfig.Namespace` is bound exclusively from
the authenticated `agents` row, never from message data.

## 5. Endpoint Reporting, DDNS, and the Runtime Share Flow

### Endpoint reporting & DDNS
The agent reports `{ ip, port, status? }` whenever its endpoint changes. On **cold
open** the `open_ack` carries the fresh `public_ip` + `granted_port` (serving as the
open report); `report_endpoint` covers enrollment (port 0), IP change (periodic
external-IP check via UPnP `GetExternalIPAddress`, STUN fallback), and close. Control
stores the values; on IP change it runs DDNS to update the wildcard A record (TTL 60s).
A redirect issued immediately after an IP change may briefly resolve the stale A
record for up to the TTL; the recipient re-opens the link to recover.

### Port state machine & reporter coupling
`OnDemandPort` states: `closed`, `open` (possibly with sessions), `closing`, and
`close-failed`. A cold open from `close-failed` first re-attempts deletion of the
previous mapping (bounded), and only re-selects a safe port once the old mapping is
confirmed removed — it never leaves two mappings behind. The endpoint reporter must
observe timer-driven transitions, so `OnDemandPort` gains an **optional transition
callback** `func(old, new state, grantedPort int)` invoked synchronously from the
single state loop (non-blocking; must not call back into `OnDemandPort`; the initial
state is not emitted). The reporter never reports `port: 0` unless deletion
**succeeded** (or the lease is known to have expired); a `close-failed` state reports
`{ ip, port, status: "close_failed" }` with a nonzero port — the mapping may still
exist, so claiming "closed" would be a false security statement. Wire invariant:
`port: 0` ⇒ closed (status omitted); `status: "close_failed"` requires a nonzero port; any other status value is a protocol error.

### Share creation & origin
At `register_share`, the agent sends the share metadata (as today). The **control
plane allocates** a random, globally-unique, never-reused origin under the agent's
namespace and returns it in `share_registered { …, origin }`. On receipt the agent
registers `binder.Allow(origin, direct, code)` locally. The agent is authoritative
over its bindings (the open-signal only *activates*, never *creates*, a route), but
the control plane owns origin allocation — so an agent-supplied origin (an open-
redirect vector) is never trusted.

### Runtime flow (canonical link → direct HTTPS)
```
recipient → GET https://sharebridge.app/s/<code>
  1. control resolves session: live expiry/revocation check → api_key_id (agent) + origin
  2. guard: agent enrolled (cert ready) + connected, else "direct unavailable"
  3. control builds OpenSignal { agent, share=code, route=direct, nonce, seq, lease_seconds, expiry }
     (lease short, e.g. 120s — v2 spec §5; SignalGate bounds it ≤15 min)
  4. control sends open_signal over WS; waits for open_ack (timeout ~2–3s)
  5. agent: SignalGate.Admit (version/agent/expiry/nonce/seq/lockdown/local-authz/rate-limit)
          → OnDemandPort.OpenFor(code, lease)
  6. agent → open_ack { share_id, nonce, seq, granted_port, public_ip, was_already_open, status }
  7. control probes reachability when the (public_ip, granted_port) tuple changed (§below)
  8. control 302 → https://<origin>[:granted_port]/s/<code>   (port omitted if 443)
      with Cache-Control: no-store (a cached 302 can pin an obsolete port)
  9. browser → agent; Binder admits SNI, authorizes Host + code; serves placeholder
```

**The port is dynamic** (NAT-PMP may remap; the preferred port may be occupied on a
later open). The redirect is built **only** from the live `open_ack` granted port —
never from `agents.endpoint_port`, which is informational.

**Open-ack correlation:** `open_ack` echoes `nonce` + `seq` and carries an explicit
`status` (+ bounded `error` code). The control matches acks to in-flight waiters by
nonce; late acks after timeout are discarded; an HTTP waiter that disconnects is
cleaned up (its open still succeeds harmlessly). A negative ack (`status: "error"`)
maps to "direct unavailable".

**Sequence authority:** `SignalGate`'s high-water mark is scoped to the **authenticated
WS connection epoch** — a fresh (or reset) gate is instantiated per connection, and the
old connection's reader is fenced (its in-flight messages are dropped), so a control-
plane restart (which resets its own counter) does not brick subsequent signals and a
replacement socket cannot inject stale signals into a new epoch. Within one epoch the
control's counter is monotonic.

**Control plane is stateless w.r.t. port-open state:** it **always** sends the
open-signal and waits for the ack; it never "skips because the DB says the port is
open" (a stale-DB trap).

### Concurrency & the `OpenFor` fast path
One external port multiplexes all of the agent's shares. The HTTPS server drives
`OnDemandPort` session tracking **explicitly** — `BeginSession` on TLS admission
(via `ConnContext`/connection wrapper), `Activity` per request, `EndSession` on
disconnect — the primitive itself does not observe connections. The mapping stays
open while any session is active, closing on idle timeout after the last.

The TLS config is a single `tls.Config.GetConfigForClient` that (a) runs
`Binder.AdmitSNI(hello.ServerName)` and (b) returns a config whose `Certificates` is
the cert manager's *current* certificate; installing/renewing swaps that certificate
in memory, so there is no listener restart and the Binder admission path is never
bypassed.

**`OpenFor` behavior change:** today it re-runs `AddPortMapping` on every call. Change
it to a **fast path when already open**: no router call, but *extend the local
unused-open deadline* to at least the requested lease so a signal arriving near the
close deadline cannot ack "open" and then immediately lapse. If the remaining router
lease is too short to cover the requested lease, perform a guarded renewal instead.
This makes repeat open-signals cheap idempotent acks (warm opens ≈ one WS round-trip)
while remaining correct at the deadline boundary.

**Cold open re-selects a safe port:** every cold open re-enumerates the router's
mappings and re-selects a safe external port (`ChooseExternalPort`), so a port that
became occupied while closed is never clobbered (v2 §6 "never clobber"). `OnDemandPort`
must therefore allow its requested/granted port to change across cold opens.

**Rate limit:** `SignalGate` currently caps 3 signals/share/min, which would reject
concurrent recipients of a hot share. Drop the tight per-share cap in favor of a
generous global ceiling (e.g., ~60/min) so legitimate bursts pass; the limit's real
job is bounding cold-opens/abuse, and a no-op ack is cheap. Exact numbers land in
the plan.

### Reachability probe (core only)
The control verifies the mapping before redirecting whenever the endpoint may have
changed — i.e. when the `open_ack` reports a `(public_ip, granted_port)` tuple that
differs from the last **successfully verified** tuple (tracked per agent). A probe is
skipped only when the current tuple was already verified; this avoids both the
stale-state trap and the "failed cold probe, then warm skip" hole.
1. Reject non-public IPs (private / reserved / link-local / multicast) before probing.
2. Issue one HTTPS request to `https://<public_ip>:<granted_port>/s/<code>/probe?nonce=<nonce>`
   with **both** TLS SNI **and** the HTTP `Host` header set to the **origin** (not the
   IP — otherwise `Binder` rejects the `Host` mismatch). Cert verification is relaxed —
   the probe authenticates via the nonce, not the chain. The client does **not** follow
   redirects (preserves the public-IP SSRF boundary).
3. The agent echoes the nonce **iff** it matches a recently-admitted open-signal for
   that share (SignalGate tracks seen nonces); anything else gets 404/403.
4. A correct echo proves the mapping reaches *this* agent; the control records the
   verified tuple and then redirects.

The **STUN cross-check** of the reported IP (the "independent observation" half of the
v2 self-probe) is **deferred** — an agent-reported STUN result is not genuinely
independent of a malicious agent, and the nonce echo already prevents redirecting to
the wrong host.

### Placeholder content
The agent serves a minimal HTML page (showing the share is being served P2P over
direct HTTPS, with the origin and granted port) plus a synthetic byte stream (size as
a query parameter, capped by a small configurable maximum). GET/HEAD only; stream
without buffering; honor request cancellation; Range is not required this phase. The
handler strips the `/s/<code>` prefix and routes the remainder — deliberately
decoupled from the real gallery UI (item 2).

## 6. Security Properties

### Key custody & cert issuance
The agent holds its TLS private key locally; only the CSR (public material) crosses
the wire. The control plane proves domain ownership via DNS-01 and causes the CA to
issue a certificate for the agent's public key. The agent verifies the returned chain
matches its key and exact SANs.

**CSR authorization (cross-tenant boundary):** `CompleteCSR` enforces, before
issuance, that the CSR (a) is self-signed (proves key possession), (b) requests
**exactly** the enrolling agent's two wildcard SANs (no extras, no IP/email/URI SANs),
and (c) has a CommonName that is empty or one of the two authorized names (blocks
CN-smuggling). This is what stops a malicious agent from obtaining a cert for another
agent's namespace, the parent zone, or the control-plane domain.

**Mapping ownership (cross-instance):** UPnP mapping descriptions carry a **per-agent
ownership token** (e.g. `sharebridge-<agent-token>`), and deletion verifies the exact
description *plus* internal client/port/protocol before removing — the Phase-1
`"sharebridge"` prefix is shared across agents/stale installs and lets one agent
delete another's mapping. NAT-PMP cannot enumerate mappings, so its ownership
guarantee is best-effort (documented).

### Threat model — malicious agent
A malicious (or compromised) agent is confined to its own namespace:
- **Cert:** can only obtain certs for its own two wildcard SANs (§6 CSR authorization);
  issuance is additionally singleflight/rate-limited so it cannot burn shared CA quota.
- **DNS:** DDNS is keyed off the authenticated namespace, so it can only move its own
  A record.
- **Content:** `Binder` admits only origins the control allocated for it and it
  registered, so it can only serve its own shares.
- **Probe SSRF:** the control probes only the agent's own reported public IP, rejects
  non-public ranges, and rate-limits probes.

Residual (accepted): a malicious agent can serve malicious content to its *own*
recipients (inherent — the agent is the content source), and can spam the control
plane (bounded by rate limits). The asymmetry is intentional: a malicious control
plane can MITM everyone (it is the TLS root of trust, v2 spec §11.3); a malicious
agent can only hurt itself and, transiently, the shared CA quota (mitigated above).

## 7. Error Handling

- **Binder:** unknown SNI → handshake rejected; wrong Host / wrong code → 403
  (already implemented).
- **SignalGate:** expired / replayed / nonce-reused / not-authorised / rate-limited
  signals dropped.
- **Open-ack timeout, negative ack, or probe failure** → control serves "direct
  unavailable" (no relay fallback this milestone).
- **Not enrolled / cert not ready / offline** → control does not emit a direct
  redirect; "direct unavailable".
- **Direct connection failure after redirect** (TLS timeout, stale DNS, path
  firewall) → recipient re-opens the canonical link (no server-side retry or
  interstitial yet — item 2).
- **Cert issuance failure** → agent retries with bounded backoff; direct shares not
  registered while `cert_status != "ready"`.
- **Close-failed** → reported truthfully (`status: "close_failed"`), escalated via
  `OnDemandPort.CloseError`; never reported as closed.

## 8. Testing

- **Unit:** cert lifecycle manager (CSR → validate → install → renew → `tls_ready`),
  endpoint reporter (incl. close-failed + transition callback), each new WS handler,
  the redirect handler (live expiry check, no-store), the DDNS trigger, the probe
  (nonce echo + IP filtering), `OpenFor` fast-path (deadline-boundary + short-lease
  renewal), cold-port reselection. Phase-1 `direct/` + `cert/` tests cover primitives.
- **High-risk cases (required):** origin cross-tenant validation (control-allocated,
  never agent-supplied), API-key rotation preserving the agent, open-ack correlation
  (concurrent + late ack), sequence reset on control reconnect, replacement WS
  disconnect (compare-and-delete unregister), IP-change/DNS propagation, cold port
  reselection when 443 becomes occupied, deletion failure → close-failed, fast-path
  near expiry, duplicate CSR renewal races, CSR quota abuse bounds.
- **Integration:** promote `spike-e2e` into a real test — enroll → cert (staging CA +
  test DNS zone) → register share → open-signal → probe → redirect → download. No
  production certs or live UPnP in CI.
- **Regression:** existing agent + signaling-server suites stay green.

## 9. Decisions Recorded

- Transport-first: placeholder content + full cert/enrollment/probe/redirect loop;
  content serving + UI + measure-and-prefer + relay deferred.
- Direct-only (no relay fallback) this milestone.
- New `agents` collection, 1:1 with `api_keys` via unique `api_key_id`; key rotation
  transactionally re-points the agent record (namespace + cert survive).
- `sessions.origin` is **control-allocated** (unique, permanent, tombstoned), returned
  in `share_registered`; the agent registers the binding locally and stays
  authoritative over its bindings (the signal activates, never creates, a route).
- All new control messages ride the existing agent WebSocket (no new HTTP surface).
- Readiness is an explicit handshake: `tls_ready`/`enrollment_ready`, with reconnect
  reconciliation; never inferred from ACME returning a chain.
- `open_ack` echoes `nonce` + `seq` + explicit status, so the control correlates acks
  to in-flight requests.
- `SignalGate` sequence is scoped to the authenticated WS connection epoch (reset on
  reconnect), so control restarts don't brick signals.
- Control plane is stateless w.r.t. port state — always signals, always uses the live
  `open_ack` granted port for the redirect (never the DB port).
- `OnDemandPort.OpenFor` gains a fast path (extend local deadline, guarded renewal if
  the remaining lease is short); cold opens re-select a safe port; per-agent UPnP
  ownership token; transition callback for the reporter; rate limit raised.
- Reachability probe (nonce echo + public-IP filtering) is in scope on cold opens;
  the STUN cross-check is deferred.
- Cert renewal is agent-driven (30-day threshold, key reuse); ACME account key
  persisted control-side; issuance singleflight + cooldown + CSR-fingerprint
  idempotency.
- Enrollment is an automatic startup gate (API-key possession is the approval).

## 10. References

- v2 direct-TCP spec: `docs/superpowers/specs/2026-08-14-direct-tcp-mode-design.md`
- FRP native-HTTPS spec: `docs/superpowers/specs/2026-07-25-native-https-tls-passthrough-design.md`
- Throughput model: `docs/superpowers/spikes/2026-08-15-direct-tcp-benchmark-results.md`
- Phase-1 primitives: `agent/internal/direct/`, `agent/internal/cert/`,
  `signaling-server/internal/certcoordinator/`, `signaling-server/internal/ddns/`
