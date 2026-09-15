# Field A/B: does the SCTP patch hold up? (2026-09-15, second campus)

Follow-up to `2026-09-15-e2e-campus-home-field-test.md`. Same topology; the laptop moved from
<campus A> to <campus B> mid-session, so the network changed and the
ceilings were re-measured.

## Ceilings

| | old campus | new campus |
|---|---|---|
| Campus Wi-Fi download | 213 Mbps | **562 / 411 Mbps** |
| Home upload (<home-agent-host> → Cloudflare) | ~200–224 Mbps | (unchanged) |

## Variants (one agent at a time, one persistent session `<session-code>`)

| id | binary | `SB_SCTP_CA_STEP` | what it tests |
|---|---|---|---|
| u | pristine pion | unset | baseline |
| c | pristine pion | 32768 | the no-fork knob alone |
| s | forked pion (`ssthresh = 256 KiB`) | unset | the fork alone |
| f | forked pion (`ssthresh = 256 KiB`) | 32768 | both |

## Results — 754 MB each, SHA-1 verified, Direct mode

| variant | throughput | vs unpatched |
|---|---|---|
| **u** unpatched | **40.0 Mbps** | 1.00× |
| **s** fork only | **67.2 Mbps** | 1.68× |
| **f** fork + CA | **70.4 Mbps** | 1.76× |
| **c** CA only | **75.2 Mbps** | 1.88× |
| **relay** (kernel TCP) | **262.4 Mbps** | 6.6× |

(n=1 per cell on this campus; one further unpatched run measured 61.6 Mbps, and a relay run
measured 262 Mbps.)

### Reading

1. **All three interventions help by a similar amount (~1.7–1.9×), and none is clearly better
   than the others.** 67.2 / 70.4 / 75.2 Mbps are within run-to-run noise of each other. This
   *contradicts* the lab sweep, where ssthresh+CA was the consistent winner — but that sweep was
   also n=1 per cell, and the lab used 64 Mbps / 100 ms / 800 KB queue rather than this link.
2. **The fork buys nothing measurable over the no-fork knob here.** Both are ~50 lines-vs-zero
   lines of change for the same result, so the no-fork `SetSCTPCwndCAStep` remains the rational
   choice.
3. **The relay is ~4× faster than the best patched direct path** (262 vs 75 Mbps) on this link.
   On the *old, slower* campus the patched direct path reached parity with the relay (93.6 vs
   96 Mbps) — so the relay's advantage *grows* as the access link gets faster, because SCTP does
   not scale with available bandwidth while kernel TCP does.
4. The CA-step effect itself replicates: 1.88× here vs 1.89× on the old campus.

## Three real bugs found in v1

### 1. The relay fallback is structurally unreachable (most serious)

- `signaling-server/internal/config/config.go:35` — `RELAY_PENDING_WAIT_WINDOW` defaults to **7 s**.
- `signaling-server/web/src/connectTransferChannel.js:2` — `DEFAULT_DIRECT_TIMEOUT_MS = 10000`.

The browser tries direct first and only reaches for the relay after **10 s**, but the relay
session the agent prepared has already expired at **~7 s**. So whenever direct fails — exactly
when the relay is needed — the fallback cannot work. The browser then retries the whole cycle
forever, producing `Error: websocket closed during handshake` and a UI stuck on "Connecting…".

Observed directly:

```
22:03:12  browser joined - starting WebRTC handshake
22:03:20  relay: pending wait window exceeded
22:03:22  peer closed                      (browser's 10s direct timeout)
22:03:25  browser retries -> same failure
```

**Fix verified:** setting `RELAY_PENDING_WAIT_WINDOW=45s` on the signaling server makes the
fallback work — a subsequent run whose direct attempt failed fell back to relay and completed
the full 754 MB at 262 Mbps. This should be fixed properly (window ≫ direct timeout, or the
browser should request a fresh relay session immediately before using it).

### 2. One agent connection per API key

`signaling-server/internal/hub/hub.go:20`:

```go
agents map[string]*websocket.Conn // apiKey → conn (one conn per API key)
```

`RegisterAgent` does `h.agents[apiKey] = conn`, so a second agent using the same key silently
replaces the first, and all shares belonging to the replaced agent become unreachable ("agent not
connected"). Running four test agents on one key made exactly one reachable at a time. Fine for
one-agent-per-account, but it fails silently and should at least be logged.

### 3. The agent daemon exits when the signaling connection drops

Restarting the signaling server produced `daemon error: signaling listener: ... EOF`, after which
the daemon shut down its web server and the process exited. The agent did *not* reconnect on its
own; it only came back because the container was restarted. For a home-server agent that should
ride out a signaling-server restart, this looks like a robustness gap (the agent also had no
`--restart` policy in the test compose, which is a test-setup issue, not a product one).

## Caveats

- **n=1 per configuration** on the new campus. The 40 vs 67–75 Mbps gap is large relative to the
  observed spread, but the ordering among c/s/f is not resolvable at this n.
- **Direct connectivity is intermittent.** Of the runs attempted, some established a direct path
  and one fell back to relay. This intermittency is itself a finding: a user cannot rely on
  getting the direct path, and the broken fallback (bug 1) is what turns that into a hang.
- The OpenCloud share link and the test credentials are throwaway; the relay is co-located with
  the signaling server, so relay throughput is bounded by the VPS's bandwidth, not just the home
  upload.
