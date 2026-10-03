# EXP37 — native (non-browser) v1 direct receiver: build + local validation — 2026-10-02

**Deliverable: a new Go program `agent/cmd/fieldrecv/` in the `benchdirect` worktree. It builds, `go vet ./...`
is clean, and the receive path + envelope accounting are validated end-to-end against the production
`transfer.NewManager` send path over real pion DataChannels. It has NOT been run on the field path.**

## What was built

`agent/cmd/fieldrecv/` (~700 lines incl. tests):

| file | role |
|---|---|
| `main.go` | flags: `--signal`, `--code`, `--path`, `--password`, `--duration`, `--insecure`, `--request-id`, `--dump-control` |
| `signaling.go` | browser-role `/ws/client` WebSocket client (`ice_config`→`knock`→`nonce`→`join`→`offer`→`answer`→`ice_candidate`), 30 s keepalive ping, HMAC-SHA256 join proof |
| `receiver.go` | pion **answerer**: accepts the agent's three lanes, runs the transport v2 handshake, counts bulk bytes, reports the selected ICE pair |
| `run.go` | orchestration: 1 Hz `PROGRESS` lines, drain-on-`chunk_end`, final `SUMMARY` line |
| `envelope.go` | 14-byte `file_chunk` / 3-byte `thumbnail` decoder mirroring `binaryEnvelope.js` |
| `*_test.go` | envelope unit tests + full in-process E2E |

It is deliberately bare: bulk-lane `OnMessage` decodes the envelope and adds payload bytes — no sink, no
assembly, no SHA-1. It counts `payload_bytes`, `wire_bytes`, `frames`, `bad_frames`; the E28 accounting gate
`wire_bytes == payload_bytes + 14 × frames` is asserted.

## Exact field command

Run from the client VM (CLIENT-EAST), pointed at the **same signalling origin the agent uses**:

```sh
cd /path/to/.worktrees/benchdirect/agent
go build -o bin/fieldrecv ./cmd/fieldrecv
./bin/fieldrecv --signal wss://<signalling-origin> --code <share-code> --duration 180s \
                [--path <file-in-share>] [--password <pw>]
```

If `--path` is omitted the receiver sends `list_request` and picks the largest non-directory entry from
`file_list`. Machine-parseable output:

```
PROGRESS t=12.0 payload_bytes=... wire_bytes=... frames=... bad=... mbps_inst=...
SUMMARY payload_bytes=... wire_bytes=... frames=... bad_frames=... header_bytes=... wall_s=... mbps=... status=complete selected="local=host remote=host"
```

`mbps` uses payload bytes over the first-frame→last-frame window (E28 used wire bytes; the difference is
14 B per ~16 KiB frame, ~0.085 %, negligible). `selected=` distinguishes host/srflx/prflx/relay.

## What was validated (locally)

1. `go build ./...`, `go vet ./...`, `gofmt -l cmd/fieldrecv/` — all clean; `bin/fieldrecv` built.
2. **Envelope unit tests** — exact payload accounting over mixed frame sizes, plus rejects for short/bad-version/zero-op/unknown-kind frames.
3. **Full E2E** (`TestRunEndToEndProductionSendPath`, 10/10 deterministic runs): a fake signalling server speaks the real `/ws/client` shape; a real `peer.Peer` offerer with `multilane.InstallHandshakeResponder`; the **production `transfer.NewManager`** streams a 2,109,497-byte file over real pion DataChannels. Result: `payload=2109497`, `frames=129`, `wire=2111303 == payload + 14×129`, `bad_frames=0`, `status=complete`. This exercises WS signalling, knock/nonce/join, offer/answer, the transport v2 handshake, `file_request` on the control DataChannel, and bulk counting.
4. **benchdirect `--mode prod`** builds and runs (8 MiB, 586 Mbps loopback, Chrome receiver) — harness healthy. `fieldrecv` was not pointed at it: `--mode prod` is hard-wired to chromedp/Chrome and a bespoke bench server, so the in-process E2E (same production send path, no Chrome) is the stronger validation.

## What remains unvalidated

- **Live signalling handshake against the real server — SKIPPED.** The idle check failed: at 20:51:34Z `sb-run` had been recreated at 20:50:12Z and session `uo4tyvfw` was mid-experiment (join 20:50:24, direct peer closed 20:50:34, relay channel up 20:50:35, container `running`). One field experiment at a time — no live validation was attempted.
- **The field path itself**: real RTT, STUN/TURN, NAT traversal, DTLS/SCTP over the real link.
- **Relay fallback is NOT implemented.** `fieldrecv` is direct-WebRTC only. E27/E28 saw the agent silently downgrade to the Noise relay on 54–60 % of *pinned* attempts. If that happens on the field run, `fieldrecv` sees no bulk frames and exits `status=deadline`. This is the biggest first-field-attempt risk.

## Protocol details that surprised me

1. **The browser is the answerer but the `transport_hello` initiator.** The agent offers and creates the lanes; its `InstallHandshakeResponder` waits for the browser's `transport_hello` (v2) on control and replies `transport_ready`. The browser sends it only after all three lanes are open.
2. **`knock`→`nonce`→`join` is mandatory even for password-less shares.** The brief's "sends `join` with empty hmac" omits the knock/nonce round trip; `handleJoin` rejects a join with no nonce. `hmac` is empty only because the password is empty.
3. **A silent handshake race.** The agent arms its `transport_hello` responder from *its own* control-lane `OnOpen`, and `directEndpoint.deliver` drops messages while `onMessage` is nil. A hello sent the instant our lanes open can be dropped with no error. Fixed with a 300 ms settle. The real browser has the same latent race; the agent's earlier `OnOpen` registration usually wins.
4. **`chunk_end` can overtake the final bulk frames.** It travels on the control lane while data is on bulk — no cross-DataChannel ordering. The receiver drains to the expected byte count (from `bytes_sent`) before declaring `complete`; without this it under-counts by exactly the last chunk.
5. **`file_request`/`list_request` go over the control DataChannel, not the signalling WebSocket.** My first cut sent them over WS and hung.

## State

No `git` command was run. Only files under `.worktrees/benchdirect/` were touched; the `e2e-v1` tree is
untouched. New files: `agent/cmd/fieldrecv/{main,signaling,receiver,run,envelope}.go` and
`{envelope,e2e}_test.go`. Built artifacts `agent/bin/fieldrecv` and `agent/bin/benchdirect` are untracked.
