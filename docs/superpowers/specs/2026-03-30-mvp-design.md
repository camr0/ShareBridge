# OpenCloudShare MVP — Implementation Design

**Date**: 2026-03-30
**Status**: Approved
**Reference**: [OpenCloudShare_plan.md](../../../OpenCloudShare_plan.md)

## Decisions Made

| Decision | Choice | Reason |
|---|---|---|
| Repo structure | Monorepo | Signaling server and agent are versioned together; shared WebSocket protocol |
| Deployment compose files | Two separate (`signaling-server/` and `agent/`) | Deployed to different machines — public VPS vs private OpenCloud server |
| HTTP framework | Gin (`github.com/gin-gonic/gin`) | Rate limiting and logging middleware available out of the box; less code to write |
| WebSocket library | `github.com/coder/websocket` | Used by both signaling server and agent |
| WebRTC library | `github.com/pion/webrtc/v4` | Agent only; signaling server does not need pion |
| Config loading | `github.com/spf13/viper` | Agent config (YAML). Signaling server uses env vars only. |
| Development environment | Local first | Build and test on Mac, wire up to real VPS/domain in Phase 4a |
| Testing | Unit tests alongside code | Per-package `_test.go` files for session management, HMAC generation, WebDAV client, signaling client |
| Signaling server persistence | In-memory only (always) | Intentionally stateless; agent re-registers sessions on reconnect. SQLite deferred to Phase 4b for API keys only. |
| Agent session persistence | JSON file (`~/.opencloudshare/sessions.json`) | Zero dependencies, human-readable, sufficient for single-agent use |
| OpenCloud public share endpoint | `remote.php/dav/public-files/{token}` | Verified on OpenCloud 5.2.0. Basic Auth: `token:` (empty password). |

## Repository Structure

```
opencloudshare/
├── signaling-server/
│   ├── cmd/server/main.go
│   ├── internal/
│   │   ├── config/           — env var config loading
│   │   ├── session/          — in-memory session store + expiry
│   │   │   ├── manager.go
│   │   │   └── manager_test.go
│   │   ├── handler/          — Gin REST + WebSocket handlers
│   │   │   ├── rest.go       — POST /api/v1/sessions
│   │   │   ├── agent_ws.go   — WS /ws/agent
│   │   │   ├── browser_ws.go — WS /ws/client
│   │   │   └── handler_test.go
│   │   └── turn/             — Coturn HMAC credential generation
│   │       ├── credentials.go
│   │       └── credentials_test.go
│   ├── web/                  — browser UI (vanilla JS + HTML, no frameworks)
│   │   ├── index.html
│   │   └── app.js
│   ├── docker-compose.yml    — signaling server + Coturn sidecar
│   └── go.mod
│
├── agent/
│   ├── cmd/agent/main.go
│   ├── internal/
│   │   ├── config/           — YAML config loading (viper)
│   │   ├── store/            — JSON session + AgentID persistence
│   │   │   ├── store.go
│   │   │   └── store_test.go
│   │   ├── client/           — WebSocket client + reconnect/backoff
│   │   │   ├── signaling.go
│   │   │   └── signaling_test.go
│   │   ├── peer/             — pion RTCPeerConnection + DataChannel
│   │   │   ├── peer.go
│   │   │   └── peer_test.go
│   │   └── opencloud/        — WebDAV PROPFIND + streaming file download
│   │       ├── client.go
│   │       └── client_test.go
│   ├── docker-compose.yml    — agent sidecar for OpenCloud stack
│   └── go.mod
│
├── docs/
└── OpenCloudShare_plan.md
```

No shared packages between signaling server and agent. They are separate binaries
deployed to different machines — sharing code adds coupling with no benefit.

## Build Sequence: Three Vertical Slices

Each slice produces a working, end-to-end testable system. No half-finished states.

### Slice 1 — The Pipe
**Goal**: agent connects → browser joins by code → WebRTC DataChannel opens → both sides log "DataChannel open".

What's included:
- Signaling server: in-memory session store, `POST /api/v1/sessions`, `/ws/agent`, `/ws/client`, SDP/ICE relay, STUN-only ICE config (no Coturn yet)
- Agent: WebSocket connect + `register` message, create session via REST, handle `join` event, pion offer/answer/ICE exchange, DataChannel open callback
- Browser: minimal HTML page, code entry form, WebRTC answer + ICE exchange, DataChannel `onopen` alert

What's excluded (added in later slices):
- File transfer
- Coturn / TURN
- Session persistence
- Reconnect/backoff
- CLI
- Password protection
- Rate limiting

### Slice 2 — File Transfer
**Goal**: recipient clicks a file in the browser, it downloads end-to-end via the DataChannel.

What's included:
- Agent: OpenCloud WebDAV PROPFIND to list files, stream file bytes as 64KB chunks over DataChannel with backpressure, `file_header` / `chunk` / `chunk_end` protocol messages
- Browser: request file list on DataChannel open, render file list, click to request download, receive chunks, assemble + trigger browser download
- Dead connection detection (BufferedAmount stall monitoring)

### Slice 3 — CLI + Persistence + Coturn
**Goal**: production-ready Phase 1 — survives restarts, handles reconnects, TURN relay works.

What's included:
- Agent CLI: `opencloudshare share <url> [--ttl 24h] [--password x] [--max-downloads 10]`
- JSON session persistence + stable AgentID
- WebSocket reconnect with exponential backoff
- Session re-registration on reconnect
- Coturn in `signaling-server/docker-compose.yml` with HMAC shared secret
- Signaling server HMAC credential generation + ICE config with TURN
- Rate limiting (Gin middleware) on REST + WebSocket endpoints

## Testing Strategy

Unit tests cover pure logic only — no network, no WebRTC:

| Package | What's tested |
|---|---|
| `signaling-server/internal/session` | Create, get, expire, max-download enforcement |
| `signaling-server/internal/turn` | HMAC credential generation, TTL capping at 24h |
| `agent/internal/store` | Read/write/merge sessions.json, AgentID stability |
| `agent/internal/opencloud` | PROPFIND XML parsing, URL validation (SSRF check) |
| `agent/internal/client` | Backoff calculation |

WebRTC (`agent/internal/peer`) and WebSocket integration are tested manually during
each slice by running both components locally and verifying the expected outcome
(DataChannel open, file download, etc.). `peer_test.go` exists as a placeholder
for any pure logic that emerges (e.g. message parsing helpers) but WebRTC
connection behaviour is not unit-testable.

## OpenCloud Integration Notes

- **Verified endpoint**: `https://{host}/remote.php/dav/public-files/{token}`
- **Auth**: Basic Auth with token as username, empty password: `Authorization: Basic base64(token:)`
- **Folder listing**: `PROPFIND` with `Depth: 1`
- **File download**: `GET /remote.php/dav/public-files/{token}/{filepath}` — stream response body directly into DataChannel chunks, no buffering
- **Single file share**: `PROPFIND` the root to discover the filename, then `GET` with filename appended
- **SSRF protection**: validate share URL hostname against `allowed_opencloud_host` config before any request
