# Spike: On-demand port + SNI binding + open-signal prototype

**Date:** 2026-08-14
**Task:** Task 4 of Phase 1 validation spikes (`docs/superpowers/plans/2026-08-14-direct-tcp-validation-spikes.md`)
**Code:** `agent/internal/direct/ondemand.go`, `agent/internal/direct/sni.go`, `agent/internal/direct/opensignal.go`
**Spec refs:** `docs/superpowers/specs/2026-08-14-direct-tcp-mode-design.md` §5.1 (open signal), §6 (port strategy), §7 (DNS namespaces), §11 (origin→share binding)

## Purpose

Prove the three production-shaped agent-side behaviors for direct mode:

1. **`OnDemandPort`** — a public port that is *closed by default* and opened
   only for active, source-verified shares, with the router mapping (not a
   local listener) as the open/close switch.
2. **`Binder`** — strict SNI admission + HTTP origin→share binding (unknown SNI
   rejected at the handshake, then Host + route-kind + share-code authorized
   per request).
3. **`SignalGate`** — the versioned, expiring, idempotent,
   `(agent, share, route, nonce)`-bound open-signal gate.

These are the building blocks Phase 2's agent HTTPS server will consume.

> **Scope note:** this spike is *pure in-process unit tests* — the fake clock
> drives the state loop deterministically and `httptest` exercises real TLS +
> HTTP. There are **no real-world router measurements here**, so every claim
> below cites the test that verifies it rather than holding placeholders for
> external results.

## 1. Result

> **Status: PASS.** All three units behave as designed under in-process tests.

| Unit | File | Tests |
|---|---|---|
| `OnDemandPort` | `ondemand.go` | `TestOnDemandPort_ClosedByDefaultAndClamp`, `_LeaseExpiresUnusedCloses`, `_ConcurrentSessionsKeepOpenAndRenew`, `_ActivityPostponesIdleClose`, `_CloseIdempotent`, `_CloseRetriesThenSucceeds`, `_CloseEscalatesAfterMaxAttempts` |
| `Binder` | `sni.go` | `TestBinder_EndToEndAuthorization`, `_EmptySNIRejected`, `_Normalization`, `_Revocation`, `_DuplicateAndMalformedHost`, `_AbsoluteFormRequestTarget`, `_WrongRouteKindAndWrongCode`, `_BothNamespaces` |
| `SignalGate` | `opensignal.go` | `TestSignalGate_ReplayReorderReuse`, `TestSignalGate_ExpiryLockdownSourceAuthRateLimit` |

`go test ./internal/direct/ -v` passes all 15 new tests **and** the pre-existing
Task 1 tests (`TestPortMapper*`, `TestChooseExternalPort*`,
`TestDeleteOwnedMapping*`, `TestIsEndOfList`). `go build ./...` and
`go vet ./internal/direct/` are clean.

## 2. `OnDemandPort` — closed-by-default, on-demand, granted-port

- **Closed by default.** `Open()` is false until `OpenFor` succeeds; the switch
  is the `PortMapper` mapping, never a local listener.
  (`TestOnDemandPort_ClosedByDefaultAndClamp`)
- **Granted port.** `AddPortMapping`'s return value (NAT-PMP may remap) is
  stored and exposed via `GrantedPort()`; deletion and renewal use the granted
  port, not the requested one. (`ondemand.go` `loop` / `tryDelete`)
- **Min-lease clamp.** A sub-second lease would collapse to
  `int(lease.Seconds()) == 0`, which some UPnP devices treat as a *permanent*
  mapping. `OpenFor` clamps any lease below `minValidLease` (5s) up.
  (`TestOnDemandPort_ClosedByDefaultAndClamp`)
- **Renew-before-expiry.** While any session is active, the mapping is renewed
  (`AddPortMapping` again) at `lease - renewWindow` (2s), avoiding a gap where
  the router reclaims the mapping mid-transfer.
  (`TestOnDemandPort_ConcurrentSessionsKeepOpenAndRenew`)
- **Session reference counting + idle close.** `BeginSession` / `EndSession`
  / `Activity` track recipient sessions. The port stays open while any session
  is live and is renewed; the last `EndSession` (or activity going stale)
  starts the inactivity/idle-timeout close, and `Activity` postpones it.
  (`TestOnDemandPort_ConcurrentSessionsKeepOpenAndRenew`,
  `TestOnDemandPort_ActivityPostponesIdleClose`)
- **Idempotent close + retry/escalation.** `Close` is idempotent (second call
  is a no-op). Deletion goes through `DeleteOwnedMapping` (never clobbers a
  foreign mapping). A failed delete returns `ErrDeleteRetry` and retries every
  `closeRetryDelay`; after `maxCloseAttempts` the failure is escalated and stays
  visible via `CloseError()`.
  (`TestOnDemandPort_CloseIdempotent`, `_CloseRetriesThenSucceeds`,
  `_CloseEscalatesAfterMaxAttempts`)

### 2.1 The single generation-guarded state loop

All state transitions run on **one goroutine** (`loop`). A **single** timer is
stopped-and-drained, then re-armed, on *every* transition (`clearTimer` →
`arm`), so a stale timer can never close a newly renewed mapping. The timer
branch re-derives from current state rather than trusting the timer's identity;
this is the generation guard the fake-clock test drives deterministically.

## 3. `Binder` — SNI admission vs HTTP authorization

Two distinct layers:

- **TLS admission (`AdmitSNI` / `TLSConfig`)** — the ClientHello SNI must be a
  non-empty, exact, active origin. Unknown or empty SNI fails the handshake
  *before any HTTP is read*. (`TestBinder_EndToEndAuthorization`,
  `TestBinder_EmptySNIRejected`)
- **HTTP authorization (`Authorize` / `Handler`)** — for a connection already
  admitted, the request must carry: (a) a single well-formed `Host` equal to the
  admitted origin (case/trailing-dot/port-insensitive), (b) a route kind matching
  the Host's namespace, and (c) the native share code in a `/s/<code>` path.
  Violations return 403. (`TestBinder_EndToEndAuthorization`,
  `TestBinder_WrongRouteKindAndWrongCode`)

Hardening verified in-process:

- **Normalization** — uppercase + trailing dot at registration match lowercase
  no-dot requests; `Host` with `:port` is accepted (port ignored).
  (`TestBinder_Normalization`)
- **Duplicate / malformed Host rejected** (`ErrBadHost`).
  (`TestBinder_DuplicateAndMalformedHost`)
- **Absolute-form request target** still authorizes correctly.
  (`TestBinder_AbsoluteFormRequestTarget`)
- **Revocation** takes effect immediately (`ErrUnknownOrigin` after `Revoke`).
  (`TestBinder_Revocation`)
- **Route-kind double-check at request time** — a binding mislabeled with the
  wrong kind (injected directly) is rejected during authorization, not just at
  registration. (`TestBinder_WrongRouteKindAndWrongCode`)

### 3.1 Both DNS namespaces (spec §7)

`routeKindFor` classifies a normalized origin by suffix:

- `<origin>.<ns>.sharebridgeusercontent.com` → `RouteDirect`
- `<origin>.relay.<ns>.sharebridgeusercontent.com` → `RouteRelay`

`Allow` refuses a route kind that doesn't match the origin's namespace, and
`Authorize` re-checks so a relay-origin `Host` can never authorize against a
direct binding (cross-namespace bleed). (`TestBinder_BothNamespaces`,
`TestBinder_WrongRouteKindAndWrongCode`)

## 4. `SignalGate` — the open-signal gate (spec §5.1)

An `OpenSignal` is versioned, expiring, idempotent, and bound to
`(agent, share, route, nonce)`. `Admit` rejects, in order:

1. wrong `Version` (`ErrBadVersion`),
2. wrong `AgentID` (`ErrWrongAgent`),
3. expired `ExpiresAt` (`ErrExpiredSignal`) or expiry too far in the future
   (`ErrBadLifetime`),
4. `Lease` ≤ 0 or above the ceiling (`ErrBadLease`),
5. non-`RouteDirect` route (`ErrWrongRouteKind` — the direct public port only),
6. a replayed nonce (`ErrReplaySignal`) or a nonce reused for a different
   share/route (`ErrNonceReuse`),
7. lockdown (`ErrSignalLockdown`),
8. a share the agent has not independently registered/source-authorized
   (`ErrSignalNotAuth` — the signal only *activates* a known route, never
   creates one),
9. rate-limit breach (`ErrSignalRate` — per-window and per-share).

Verified: replay/reorder/reuse (`TestSignalGate_ReplayReorderReuse`) and
expiry/lockdown/source-auth/rate-limit
(`TestSignalGate_ExpiryLockdownSourceAuthRateLimit`). Admitted nonces are
retained through `maxSignalLifetime` + skew and pruned best-effort to bound
memory.

### 4.1 Strict open-signal rule

The signal handler must call `SignalGate.Admit` **first** and only pass
agent-registered, source-verified shares to `OnDemandPort.OpenFor`. `OpenFor`
itself does not re-verify — that is the gate's job — and `OnDemandPort` only
*activates* the mapping for shares the agent already trusts. Re-admitting an
already-applied share is idempotent because `OpenFor` refreshes the lease.

## 5. What is NOT covered here

- Real-router behavior (multi-vendor UPnP/NAT-PMP, double NAT/CGNAT, strict vs
  lease-lax) — that is Task 1's matrix and lives in
  `2026-08-14-upnp-reachability.md`.
- Actual Phase 2 wiring: no agent HTTPS server, no open-signal handler, no
  share-authorizer implementation is produced here — only the three reusable
  types and their tests.

## 6. How to run

```bash
cd agent
go test ./internal/direct/ -v   # all units + Task 1 tests
go build ./...
go vet ./internal/direct/
```
