# Relay Default Design

**Date:** 2026-07-18
**Status:** Approved

## Goal

Make ShareBridge's relay path the clear, consistent default across every share-creation surface. Remove outdated TURN-specific warnings and explain the real tradeoffs between relay and direct transfers.

## User Experience

Connection modes appear in this order:

1. **Relay (recommended)** — selected by default. Description: “End-to-end encrypted, hides your IP, and provides consistent performance.” The existing relay quota remains visible where the UI currently displays it.
2. **Direct** — not selected by default. Description: “Peer-to-peer, quota-free.” An amber advisory says: “Direct transfers expose your IP address and may be slower due to browser protocol limitations. Use Relay for more consistent performance.”

No share-creation surface displays a warning that TURN must be configured. User-facing mode copy uses “Relay,” not “TURN.”

## Scope

- Agent admin create-share form
- Agent admin default-mode setting
- Agent configuration default
- OpenCloud create-share form
- Nextcloud create-share form
- Tests that cover mode defaults, ordering, labels, descriptions, advisories, and removed TURN warnings

This is a pre-release product with no external users, so the new relay-first default may replace the previous Direct default without migration compatibility work.

## Behavior and Data Flow

The existing `relay_only` boolean and transfer behavior remain unchanged. Only its default presentation/value changes from `false` to `true` when no preference is supplied. Explicit form choices continue to be submitted through the existing APIs.

Relay remains quota-metered. Direct remains quota-free and may expose the host IP. Both modes remain end-to-end encrypted; the Relay description explicitly identifies its IP-hiding property.

## Error Handling

No transport or error-handling logic changes. Existing quota-unavailable and transfer errors remain intact. Obsolete TURN-availability warnings are removed because TURN is no longer a deployment prerequisite.

## Testing

- Render the agent share form and verify Relay is first and selected, Direct is second, the approved copy is present, and obsolete TURN copy is absent.
- Verify a new agent configuration defaults `DefaultRelayOnly` to `true`.
- Update OpenCloud and Nextcloud component tests to verify their fallback default is relay-only and that no TURN warning is rendered.
- Run focused Go and extension test suites, followed by broader relevant package tests and builds.
