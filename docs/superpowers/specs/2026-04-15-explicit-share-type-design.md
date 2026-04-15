# Explicit Share Type for Share Creation

## Summary

Replace runtime WebDAV backend fallback and endpoint-preference caching with an explicit `share_type` chosen at share creation time and persisted with each session.

Allowed values:
- `opencloud`
- `nextcloud`

This applies to:
- Agent create-share API and web UI
- OpenCloud extension
- Nextcloud extension
- Session persistence and daemon reload behavior

## Motivation

The fallback approach started as a convenience but now creates real product risk:
- It hides backend mismatches behind automatic retries
- It can generate repeated bad requests against the wrong DAV endpoint
- It makes protocol errors and server errors harder to distinguish
- It already contributed to Nextcloud rate limiting (`429`) because the wrong endpoint was probed repeatedly before the right one was used
- The later endpoint-preference cache reduced repeated bad probes, but it still preserved fallback semantics that hide real backend mismatches

Because the project is still pre-public, this is the right time to make a breaking cleanup rather than carry forward migration complexity.

## Product Decision

`share_type` is required for all new shares.

The user does not need auto-detection:
- OpenCloud extension always sends `opencloud`
- Nextcloud extension always sends `nextcloud`
- Agent web UI shows an explicit selector for manual share creation

Existing persisted sessions that do not contain `share_type` are invalid and will be skipped on startup. Users can recreate them.

## Architecture

### Session Model

Persist `share_type` in both:
- `store.SessionEntry`
- in-memory `daemon.Session`

This field is part of the canonical session identity and is required to reconstruct the correct WebDAV client on restart.

### WebDAV Client Behavior

Remove both:
- backend fallback between server-specific paths
- endpoint-preference caching added to soften that fallback behavior

Instead:
- OpenCloud sessions use the OpenCloud public WebDAV pathing behavior
- Nextcloud sessions use the Nextcloud public WebDAV pathing behavior

The backend-specific choice is made once when the share is created, then reused for all later operations:
- `GetRootFileID`
- `ListFiles`
- `GetFile`

### Backend Selection

Add an explicit backend selector in the agent layer:
- `opencloud`
- `nextcloud`

Construction should fail fast if the value is missing or invalid.

The WebDAV package should expose an explicit way to build the correct backend behavior instead of probing both. Once `share_type` is known, the client should use exactly one backend-specific path strategy.

## API Changes

### Agent JSON API

`POST /api/v1/shares` requires:
- `share_url`
- `share_type`

If `share_type` is missing or invalid, return a `400` with a clear validation error.

### Agent Web UI Form POST

The create-share form must include a required `share_type` field.

Recommended UI:
- label: `Share Type`
- options:
  - `OpenCloud`
  - `Nextcloud`

Default selection:
- `OpenCloud`

Rationale:
- preserves current mainline/manual usage
- keeps human-created shares explicit

## Extension Changes

### OpenCloud Extension

When creating a ShareBridge share, always send:
- `share_type: "opencloud"`

No UI change required in the extension because the platform is known.

### Nextcloud Extension

When creating a ShareBridge share, always send:
- `share_type: "nextcloud"`

No UI change required in the extension because the platform is known.

## Persistence and Startup

### New Sessions

When a session is created:
1. validate `share_type`
2. build the matching backend-specific client
3. extract `file_id`
4. persist `share_type` with the session

### Existing Sessions Without `share_type`

On daemon startup, persisted sessions missing `share_type` are skipped.

Expected behavior:
- log a warning that the session is invalid because it predates explicit backend typing
- do not re-register it with signaling
- leave the session absent from active runtime state

This is intentionally breaking. Recreating the share is the supported recovery path.

## Error Handling

Expected failures should be explicit:
- missing `share_type` -> validation error
- unknown `share_type` -> validation error
- mismatched `share_type` for the actual backend -> direct backend error, not hidden by retry/fallback

This is a design goal, not a downside: wrong configuration should fail clearly.

## Testing

### Agent

- [ ] create-share rejects missing `share_type`
- [ ] create-share rejects invalid `share_type`
- [ ] OpenCloud sessions build OpenCloud-specific client behavior
- [ ] Nextcloud sessions build Nextcloud-specific client behavior
- [ ] sessions without `share_type` are skipped on load
- [ ] persisted sessions with valid `share_type` reload correctly
- [ ] no fallback probing remains in normal operations
- [ ] no endpoint-preference caching remains in the WebDAV client

### OpenCloud Extension

- [ ] create-share request includes `share_type: "opencloud"`

### Nextcloud Extension

- [ ] create-share request includes `share_type: "nextcloud"`

### Manual Verification

- [ ] Agent web UI can create an OpenCloud share
- [ ] Agent web UI can create a Nextcloud share
- [ ] OpenCloud extension create-share still works
- [ ] Nextcloud extension create-share still works
- [ ] Restarting the daemon skips old sessions without `share_type`

## Risks

### Breaking Existing Sessions

Intentional and acceptable. The project is still pre-public and this is cleaner than carrying migration logic for legacy sessions that lack required backend identity.

### Backend Drift

This design accepts that OpenCloud and Nextcloud are similar but not identical. Persisting `share_type` makes future drift easier to manage instead of harder.

## Out of Scope

- automatic share type detection
- migration of old sessions by probing stored links
- user-selectable backend type inside either extension UI
- support for additional backends beyond `opencloud` and `nextcloud`
