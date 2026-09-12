# Cross-module control-sync integration test

This module proves the §11.3 control↔gateway sync channel end to end over a
real mutually-authenticated TLS 1.3 socket, with **both halves being the
production types**:

- **control half (in-process):** `sharebridge/control/internal/relayctl` —
  the real `Server`, the real `Publisher` as `RouteSource`/`StatusSink`, and
  the real `PresenceView` as `PresenceSink`, over an ephemeral `127.0.0.1`
  listener with in-test CAs.
- **gateway half (subprocess):** a throwaway module built from
  `gatewayharness/main.go.txt` that imports and runs the real
  `sharebridge/relay/internal/controlsync` `Client`, `Applier`, `Loop`, and
  `PresencePublisher`. It is driven over a JSON-lines stdin/stdout protocol.

## Why a separate module (and a subprocess)

The two production packages are `internal`: Go's internal-package rule is an
import-path prefix rule, so one package can be rooted under
`sharebridge/relay/` **or** `sharebridge/control/`, never both. This module's
path (`sharebridge/control/integration`) is under the control tree, so it
imports the real control listener in-process; the gateway half is built as a
subprocess whose module path (`sharebridge/relay/gwharness`) is under the
relay tree. Neither production module gains a cross-module dependency.

## Coverage

`TestCrossModuleSyncAckPresenceAndEpochRecovery`:

1. snapshot fetch + accepted `/status` ack advances control's watermark and
   the gateway's control-sync health;
2. an ordered delta fetch applies and advances both;
3. presence republish survives past the 45 s lease with the republish loop
   running (injected clocks; no real 45 s wait);
4. a control restart refuses a stale-epoch ack with `acknowledged:false` /
   `reason:"foreign_epoch"`, the gateway records nothing and withdraws health,
   then recovers on the new epoch;
5. mTLS rejects a client with the wrong CA or SAN.

`TestCrossModulePresenceBootAdoption` proves an empty presence snapshot
carrying the gateway boot id is adopted, so the same boot's event batch that
was discarded before adoption applies afterwards.

## Running

```
cd integration/controlsync
go test ./...                       # default
go test -race -count=5 -timeout 900s .
```

Hermetic: ephemeral loopback ports, in-test CAs, a PocketBase app in
`t.TempDir()`, and injected clocks. The harness build runs with
`GOPROXY=off` (the relay module has no external requirements).
