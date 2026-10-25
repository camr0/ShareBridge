# V1 signaling ownership fix

The user approved enforcing ownership on every agent-to-browser message and pushing the fix to v1 main. Preserve the deployed v1 protocols; no v2 migration or live deployment is part of this change.

1. Add real WebSocket regressions for cross-account and same-account/different-key offers, ICE candidates, nonce/auth results, and relay policy. Observe failures against the current server. Use an ordered registration response as a processing barrier, then a legitimate nonce as a positive control.
2. Bind browser conn IDs atomically to their share, owning API key, and current agent socket in the hub. Require those identities for all forwarding, failure counters, and closes. Match auth_ok's code to the bound browser and persisted session owner before issuing relay tokens. Fence stale sockets and make disconnect cleanup conditional on the exact socket.
3. Verify legitimate direct and relay flows, multiple browser connections, reconnect cleanup, unknown targets, and rejected messages without counter/token side effects. Run signaling-server tests, race checks, vet, and build. Request an independent code review and address findings.
4. Commit the previously applied reboot-recovery policy separately from the ownership fix. Verify origin/main has not advanced; push the tested commits to main without force. Do not deploy the application server or include unrelated v2/audit artifacts.

## Completed validation

The real WebSocket regressions failed on the original code for all six message types across accounts and across API keys within one account. Reconnect and duplicate-hello regressions also failed before the corresponding lifecycle corrections. The final implementation rebinds still-owned browser/pair snapshots atomically during agent replacement and ignores repeated enrollment on an already enrolled socket.

Final checks passed: `go test ./...`, `go test -race ./internal/handler ./internal/hub`, `go vet ./...`, `go build -o /tmp/sharebridge-v1-signaling-security ./cmd/server`, and `git diff --check`. Independent review found no remaining important ownership issues. A pre-existing relay timeout test-fixture race was reproduced against the unchanged deployment snapshot and corrected in a separate test-only commit by synchronizing upgraded-handler teardown before restoring the timeout.

Pushing these commits updates Git only; the live signaling server requires a separate deployment. No PocketBase dependency or database changes are included.
