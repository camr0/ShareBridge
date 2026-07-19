# Immich Agent Reinstall Sync and Reservation Ownership Design

## Goal

Allow an agent that was reinstalled or reset to recover the Immich share-code
reservations belonging to its ShareBridge account. Recovery must work when the
installation has a new `agent_id`, and when it uses a replacement API key, while
preserving the original defense against another account hijacking an existing
share code.

## Problem

Each agent installation persists a distinct `agent_id`. Signaling-server session
records currently retain both the API key and agent ID that last registered the
share code. Registration already establishes whether the existing and attempting
API keys belong to the same account, but it then rejects a same-account claim
when the persisted and attempting agent IDs differ.

This blocks legitimate recovery. For example, rotating an API key transfers the
account's session records to the replacement key but intentionally leaves their
agent IDs unchanged. A reset agent using that replacement key therefore cannot
re-register the Immich links discovered from the same Immich instance.

The Immich poller also lists every active album share in the configured Immich
instance. If a link is legitimately active on another installation under the
same account, registration returns an ownership conflict. The poller currently
treats that expected conflict like an operational failure and returns
immediately, so later links in the same Immich response are not processed.

## Trust and Ownership Model

The account is the durable share-code reservation owner. An API key is a
revocable credential and the currently bound signaling identity. An `agent_id`
identifies an installation for reconnection and diagnostics, but it is not an
authorization boundary.

A successful recovery always **reclaims and rebinds** the reservation to the
registering API key and agent ID. This terminology applies equally when only the
agent ID changes and when both the API key and agent ID change.

The server will decide an existing-code registration as follows:

1. An expired reservation is treated as released and may be registered normally.
2. A non-expired reservation owned by another account is rejected as a suspected
   session hijacking attempt.
3. A reservation owned by the same account and already bound to the registering
   API key is reclaimed and rebound, even when the agent ID changed.
4. A same-account reservation bound to another API key is rejected while that
   key has a live agent connection.
5. A same-account reservation bound to another API key is reclaimed and rebound
   when that key is disconnected.

Successful reclaim and rebind updates `api_key_id`, `agent_id`, and the ordinary
registration metadata in the existing session record and returns the normal
successful reconnected response. It does not create a second reservation.

The live-owner guard is intentionally based on the currently bound API key, not
the UUID. ShareBridge supports separate API keys for separate installations;
sharing one key between concurrently running agents remains unsupported.

## Scope

This change:

- makes reservation authorization account-scoped;
- permits recovery after an agent reset, reinstall, or API-key replacement;
- prevents a second live same-account installation from silently taking over a
  reservation;
- records cross-account claims as suspicious security events; and
- lets Immich synchronization skip an expected active-owner conflict and
  continue reconciliation.

It does not add simultaneous-agent routing for one shared API key, merge agent
stores, notify account owners, automatically block callers, define security-event
retention, or integrate with fail2ban.

## Protocol Behavior

The signaling server will retain the existing human-readable error message for
an expected same-account live-owner conflict and add a stable protocol field:

```json
{
  "type": "error",
  "error_code": "session_owned_by_active_agent",
  "message": "session owned by different agent"
}
```

`error_code` is separate from the existing `code` field because `code` contains
the registered share code in successful registration responses.

The agent signaling client will map this protocol value to an exported sentinel
error that callers can identify with `errors.Is`. During a rolling deployment,
the client will also recognize the existing exact message from an older server
as the same sentinel error. Other server errors remain ordinary registration
errors.

A cross-account caller continues to receive only the generic `code already in
use` response. The response must not reveal the owning account, agent, API key,
or whether the reservation is currently online.

## Immich Sync Behavior

When registration of an Immich album returns the active-owner sentinel, the
poller will:

1. leave the live agent's server session untouched;
2. not add the link to the current agent's in-memory or persistent store;
3. log the raw Immich shared-link key with an explanation that account access is
   valid but another agent installation is active; and
4. continue with the remaining Immich links and the removal phase of the same
   reconciliation cycle.

The log will use this form:

```text
immich sync: skipping shared link <key>: owned by another active agent installation on this account
```

Raw link keys are acceptable in the private agent logs and make it possible for
the server owner to identify the affected Immich share. API-key secrets and agent
UUIDs will not be logged by the agent.

Any other registration error, including a generic cross-account collision, will
still abort the current sync cycle. This keeps network, authentication,
persistence, security, and unexpected server failures visible instead of
accidentally downgrading them to warnings.

A successful reinstall reclaim follows the ordinary registration path and is
persisted locally; it requires no special Immich-poller branch.

## Suspicious Claim Recording

Only an authenticated attempt to claim a non-expired reservation owned by a
different account is classified as suspicious. A same-account active-owner
conflict is expected operational behavior and must not create a security event.

The server will reject a cross-account claim without modifying the existing
session, emit a stable high-severity log beginning with:

```text
SECURITY session_hijack_attempt
```

and create one record in a new internal-only `security_events` collection. The
record will contain:

- `event_type`, set to `session_hijack_attempt`;
- the raw attempted share `code`;
- the attempting account ID;
- the attempting API-key ID;
- the attempting agent ID;
- the owning account ID;
- the source address captured from the agent WebSocket request; and
- an automatic creation timestamp.

The serious log will include the same identifiers needed for immediate operator
correlation and later fail2ban parsing. It must never include an API-key secret.
The raw share code is intentionally retained: it already exists as plaintext
server state, and direct correlation is more useful than adding a second
confidentiality convention for this internal audit data.

The collection will have no public PocketBase collection rules. Its account and
API-key identifiers are audit attributes and must not cascade-delete the event
when related records are later removed.

The security event must be persisted outside any transaction that returns the
claim rejection, so the rejection cannot roll the event back. If persistence
fails, the server will still reject the claim and emit an additional
high-severity `SECURITY session_hijack_attempt_persistence_failed` log. A
telemetry failure must never permit takeover or replace the generic response
with ownership details.

## API-Key Rotation

The existing Rotate button and endpoint remain the explicit administrative
recovery path. Rotation atomically creates a replacement key, transfers every
session from the old key to the new key, revokes the old key, disconnects the old
agent, and returns the new secret once.

Rotation is authorized through the account UI, so it intentionally bypasses the
live-owner guard and does not create a suspicious security event. When a reset
agent connects with the replacement key, each reservation is already bound to
that key; registering it reclaims and rebinds the new agent ID.

Creating an independent replacement key instead of rotating also works. In that
case, Immich recovery occurs lazily per share and only after the previously bound
key has no live agent connection.

## Testing

Tests will cover each boundary.

### Migration and security events

- `security_events` exists with every required field, internal-only rules, and
  non-cascading audit identifiers;
- the migration is idempotent;
- a non-expired cross-account claim is rejected, leaves the session unchanged,
  creates exactly one complete security event, and emits the stable serious log;
- a security-event persistence failure still rejects the claim and emits the
  telemetry-failure log;
- an expired cross-account reservation can be registered without creating a
  security event; and
- a same-account active-owner conflict creates no security event.

### Ownership and rotation

- the same account and API key with a new agent ID reclaim and rebind
  successfully;
- a different API key on the same account reclaims and rebinds when the
  previously bound key is disconnected, updating both `api_key_id` and
  `agent_id`;
- the replacement key receives `session_owned_by_active_agent` while the
  previously bound key is connected;
- cross-account rejection never changes the existing session owner or metadata;
- rotation transfers every reservation, revokes and disconnects the old key,
  and allows a new agent ID using the replacement key to reclaim successfully;
  and
- rotation produces neither an active-owner conflict nor a security event.

### Agent protocol and Immich reconciliation

- both the structured active-owner error and the legacy exact message map to the
  ownership sentinel, while unrelated errors do not;
- an active-owner link placed before a valid new link is logged and skipped, the
  valid link is registered and persisted, and the overall sync succeeds;
- reconciliation continues into removal and removes a stale locally owned link
  after skipping the conflict;
- the conflicted link is absent from the current agent's in-memory and persistent
  stores; and
- an unrelated registration failure still stops the cycle.

Both the agent and signaling-server Go test suites must pass. Deployment should
apply the signaling-server migration and server changes before publishing and
deploying the agent. The legacy-message fallback keeps the new agent compatible
with an older server during a rolling deployment.

## Operational Result

After resetting or reinstalling a home-server agent, the account owner may rotate
its API key or create a replacement key, configure the new installation, and let
the Immich poller recover the account's existing share reservations. A different
account cannot take those reservations, and any such attempt is immediately
visible in serious server logs and durable internal security-event records.
