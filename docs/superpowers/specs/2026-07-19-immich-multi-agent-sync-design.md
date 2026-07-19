# Immich Multi-Agent Sync Conflict Design

## Goal

Allow multiple ShareBridge agent installations to use the same ShareBridge API
key and Immich instance without one agent's shared links preventing another
agent from completing its Immich synchronization cycle.

## Problem

The ShareBridge API key authenticates an account, while each agent installation
persists a distinct `agent_id`. A signaling-server session is bound to the agent
that created it. This prevents another installation using the same API key from
silently taking over the session.

The Immich poller lists every active album share in the configured Immich
instance. When it finds a link that is absent from its local session store, it
tries to register that link. If the link is already owned by another agent, the
server returns `session owned by different agent`. The poller currently treats
that expected ownership conflict like an operational failure and returns
immediately, so later links in the same Immich response are not processed.

## Scope

This change preserves per-agent session ownership and changes only how the
Immich synchronization loop handles a known ownership conflict. It does not
allow session takeover, merge agent stores, change API-key authentication, or
change the behavior of non-Immich shares.

## Protocol Behavior

The signaling server will retain the human-readable error message and add a
stable protocol field:

```json
{
  "type": "error",
  "error_code": "session_owned_by_different_agent",
  "message": "session owned by different agent"
}
```

`error_code` is separate from the existing `code` field because `code` already
contains the registered share code in successful registration responses.

The agent signaling client will map this protocol value to an exported sentinel
error that callers can identify with `errors.Is`. During a rolling deployment,
the client will also recognize the existing exact message from an older server
as the same sentinel error. Other server errors remain ordinary registration
errors.

## Immich Sync Behavior

When registration of an Immich album returns the ownership sentinel, the poller
will:

1. leave the other agent's server session untouched;
2. not add the link to the current agent's in-memory or persistent store;
3. log the raw Immich shared-link key with an explanation that account/API-key
   authentication succeeded but the persisted agent IDs differ; and
4. continue with the remaining Immich links.

The log will use this form:

```text
immich sync: skipping shared link <key>: owned by another agent installation (API key/account access is valid; agent ID differs)
```

Raw link keys are acceptable in the private agent logs and make it possible for
the server owner to identify the affected Immich share. API keys and agent UUIDs
will not be logged.

Any other registration error will still abort the current sync cycle. This
keeps network, authentication, persistence, and unexpected server failures
visible instead of accidentally downgrading them to warnings.

## Testing

Tests will cover each boundary:

- signaling-server handler: ownership conflicts include the stable
  `error_code` and existing message;
- signaling client: both the structured error and the legacy message map to the
  ownership sentinel, while unrelated errors do not;
- Immich sync: an ownership-conflicted link placed before a valid new link is
  logged and skipped, the valid link is registered and persisted, and the
  overall sync succeeds;
- Immich sync: an unrelated registration failure still stops the cycle.

Both the agent and signaling-server Go test suites must pass. Deployment should
update the signaling server before publishing and deploying the agent, although
the legacy-message fallback keeps the new agent compatible with the old server.

## Operational Result

Laptop and Versa agents may continue sharing one ShareBridge API key and one
Immich instance. Each installation retains ownership of the links it registered,
and a link owned by one installation no longer blocks the other installation
from discovering or removing its own links later in the same poll cycle.
