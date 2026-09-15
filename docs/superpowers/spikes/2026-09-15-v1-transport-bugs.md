# v1 transport: three bugs found during the field test (2026-09-15)

Found while running v1 (`main`) end-to-end: a campus browser in New Jersey downloading a 754 MB
file from a home agent, via a signaling server on a public VPS. All three were hit in practice,
not theorised. Line numbers are for `main` as of `41d30f29`.

---

## BUG 1 — The relay fallback is structurally unreachable (severity: high)

**If the direct WebRTC path fails, the relay fallback cannot succeed, and the client hangs
forever instead of transferring.**

### Sites

| what | where | value |
|---|---|---|
| relay pending-wait window | `signaling-server/internal/config/config.go:35` | **7 s** default |
| enforced at | `signaling-server/internal/relay/registry.go:120`, `:177` | `errors.New("relay: pending wait window exceeded")` |
| browser's direct-connect timeout | `signaling-server/web/src/connectTransferChannel.js:2` | **10 000 ms** |

### Why it cannot work

`connectTransferChannel.js` tries **direct first** and only falls through to the relay after
`DEFAULT_DIRECT_TIMEOUT_MS` = **10 s**. But the agent prepares the relay session at browser-join
time, and the server expires that pending session after **7 s**. So by the time the browser
reaches for the relay, the session is already gone — the fallback that exists precisely for the
case "direct failed" is dead in exactly that case.

The browser then reconnects and repeats the whole cycle indefinitely, because each retry creates
a fresh relay session with a fresh 7 s window that always expires before the 10 s direct timeout
elapses.

### Observed

Browser UI stuck on `Connecting…`, console `Error: websocket closed during handshake`, looping
every ~13 s. Agent log, repeating identically:

```
22:03:12  browser joined session <code> - starting WebRTC handshake
22:03:20  start relay channel sid=sid_…: read msg1: … StatusPolicyViolation
          and reason = "relay: pending wait window exceeded"
22:03:22  peer <id> closed (session <code>)        <- browser's 10 s direct timeout
22:03:25  browser reconnects -> same failure
```

Server log for the same window:

```
22:03:20  relay_ws: agent wait for browser failed sid=sid_…: relay: pending wait window exceeded
22:03:23  relay_ws: browser bind failed for sid=sid_…: relay: unknown sid
```

### Workaround verified in the field

Setting `RELAY_PENDING_WAIT_WINDOW=45s` on the signaling server fixes it: a subsequent run whose
direct attempt failed fell back to relay and completed the full 754 MB at 262 Mbps. So the
diagnosis is confirmed and the fallback code itself is fine — only the timing is wrong.

### Suggested fix (pick one, ideally the first)

1. Make the window strictly greater than the client's direct timeout — e.g. default
   `RELAY_PENDING_WAIT_WINDOW` to `2 × DEFAULT_DIRECT_TIMEOUT_MS` — and add a test asserting
   `RelayPendingWaitWindow > DEFAULT_DIRECT_TIMEOUT_MS` so the two constants can never drift
   apart again.
2. Have the browser request a **fresh** relay session at the moment it decides to fall back,
   rather than relying on one prepared before the direct attempt.
3. Surface the failure: the client currently retries forever with no user-visible explanation.

### Test

Start a session, block the direct path (e.g. force relay-only infrastructure failure or block the
STUN server), and assert the transfer still completes via relay within a bounded number of
retries.

---

## BUG 2 — One agent connection per API key, silently (severity: medium)

**A second agent using the same API key silently replaces the first, and every share belonging
to the replaced agent becomes unreachable.**

### Site

```go
// signaling-server/internal/hub/hub.go:20
agents          map[string]*websocket.Conn // apiKey → conn (one conn per API key)
```

```go
// signaling-server/internal/hub/hub.go:41
func (h *Hub) RegisterAgent(apiKey string, conn *websocket.Conn) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.agents[apiKey] = conn          // <- overwrites any existing connection
	h.ensureWriteMuLocked(conn)
}
```

### Why it matters

The map is keyed by API key alone, so the last agent to register wins. The previously registered
agent stays connected and believes it is serving, but browser lookups via `GetAgentConn` resolve
to the newer connection, so its shares fail with `agent not connected`. Nothing is logged to
explain it.

### Observed

Running four test agents (four configurations) on one API key, only one was reachable at a time.
The browser repeatedly showed `Error: agent not connected` for the others. Cost roughly an hour
of debugging before the map declaration was found.

### Suggested fix

- Key by `(apiKey, agentID)` — the agent already sends an `agentID` in `handleHello`, and the
  server already logs `agent_id`, so the identity exists.
- Or, at minimum, **log a warning** when a registration displaces an existing live connection,
  and consider rejecting it.

### Also present in v2 / phase-4a

The same code exists verbatim at `control/internal/hub/hub.go:20` on both `v2` and `phase-4a`
(`agents map[string]*websocket.Conn // apiKey → conn (one conn per API key)`). Read-only check;
those branches were not modified. Worth fixing there too if multi-agent-per-account is ever
intended.

---

## BUG 3 — The agent daemon exits when the signaling connection drops (severity: medium)

**A signaling-server restart (deploy, crash, or a network blip) terminates the agent instead of
it reconnecting.**

### Sites

```go
// agent/internal/daemon/daemon.go:328
errChan <- fmt.Errorf("signaling listener: %w", err)
```

```go
// agent/cmd/agent/main.go:148
log.Printf("daemon error: %v", err)
```

The listener error propagates to the daemon's error channel, which unwinds the daemon: the web
server is stopped and the process exits. There is no reconnect-with-backoff on this path.

### Observed

Restarting the signaling server produced, on the agent:

```
22:05:10  daemon error: signaling listener: failed to get reader: failed to read frame header: EOF
22:05:10  peer <id> closed (session <code>)
22:05:10  web server stopped
22:05:10  web server shutting down due to context cancellation
```

The agent did **not** come back on its own; it only returned because the container was restarted.
For a home-server agent that must survive routine server maintenance, this is a robustness gap.

(Note: an earlier, shorter disconnection *was* survived — the agent logged `agent disconnected`
followed by `share registered … reconnected=true`. So some reconnect path exists; a full server
restart is the case that kills it. Worth understanding which paths are covered.)

### Test setup caveat

My test compose had no `--restart` policy, which is a flaw in *my* test rig, not the product.
But even with one, a container restart is not the same as the agent reconnecting on its own.

### Related observation

`v2` ships `agent/internal/signaling/backoff.go`, which suggests the rewrite added
reconnect-with-backoff. Not deeply verified — flagged as a lead, not a finding.

---

## Summary

| # | bug | scope | severity |
|---|---|---|---|
| 1 | relay fallback expires before the client tries it | v1 | **high** — turns a recoverable failure into a permanent hang |
| 2 | one agent connection per API key, silent takeover | v1 **and** v2/phase-4a (`control/internal/hub/hub.go:20`) | medium |
| 3 | agent daemon exits on signaling disconnect | v1 | medium |

Bug 1 is the one I would fix first: it is user-visible, it is deterministic whenever direct
fails, and the fix is a constant plus a guard test.
