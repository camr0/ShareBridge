# OpenCloudShare - WebRTC-Based File Sharing System

## Overview

OpenCloudShare enables secure, revocable file sharing from OpenCloud instances running behind NAT/VPN (like Tailscale) to anyone on the public internet without requiring port forwarding or reverse proxies.

## Problem Statement

- **Current limitation**: OpenCloud instances behind Tailscale/VPN cannot easily share files with external users
- **Existing solutions**: Public links require reverse proxy or Tailscale Funnel (exposes entire instance)
- **Goal**: Share specific files/folders to anyone while keeping server private

## Security Design Philosophy

The agent uses **OpenCloud share links** rather than admin WebDAV credentials. This is intentional:

- **Least privilege**: the agent can only access what the user explicitly shared in OpenCloud — nothing else
- **Two independent access control layers**: OpenCloud enforces its own share permissions; OpenCloudShare adds the session code/password on top
- **Minimal credentials in agent config**: no WebDAV username/password stored — only a signaling server token
- **Blast radius**: if the agent is compromised, the attacker gets only the contents of active share links, not the entire OpenCloud instance

**User workflow**:
1. Create a share link in OpenCloud (as normal)
2. Open the agent's admin UI (e.g. `http://localhost:7878`), paste the share URL, click "Create Share"
3. Copy the generated link/code and send it to the recipient
4. Agent streams files from OpenCloud directly into WebRTC DataChannel chunks on demand — never writes to disk, never buffers the full file, never has broader access than the share link allows

The share URL is **not stored in agent config** — sessions are created on demand via the admin UI and persisted by the agent to `~/.opencloudshare/sessions.json`, surviving restarts. The signaling server holds only an in-memory copy (re-registered by the agent on reconnect); it is intentionally stateless.

> **Phase 1** uses a minimal CLI in place of the admin UI (`opencloudshare share <url>`) to keep scope small while the core WebRTC machinery is built. The admin UI replaces this in Phase 2.

## Architecture

```
┌─────────────────┐         ┌──────────────────┐         ┌─────────────────┐
│  OpenCloud      │◄───────►│  Public Signaling │◄───────►│  Recipient      │
│  Server Agent   │ WebRTC  │  Server           │         │  Browser        │
│  (Tailscale)    │◄·······►│  (Rendezvous)     │◄·······►│  (public net)   │
└─────────────────┘   ↑     └──────────────────┘   ↑     └─────────────────┘
                      │                              │
               [Direct P2P]                   [via TURN if
                (preferred)                    P2P fails]
```

**Connection fallback chain** (tried in order):
```
1. WebRTC P2P via STUN          — free, no bandwidth cost, ~70-80% of connections succeed
       ↓ ICE fails
2. WebRTC via third-party TURN  — user-configured (Cloudflare Calls, Metered.ca, etc.)
       ↓ not configured
3. WebRTC via bundled Coturn    — included in docker-compose, uses your VPS bandwidth
```

All three paths use identical WebRTC code — only the ICE server config differs.
The signaling server never handles file data in any scenario.

## Components

### 1. Signaling Server (Public)

**Language**: Go
**Purpose**: Matchmaker for WebRTC connections; distributes ICE config to peers

**Features**:
- WebSocket-based signaling
- Session management with expiration (in-memory; sessions are lost on restart by design)
- Serves ICE server config (STUN + TURN credentials) to connecting browsers
- Rate limiting and abuse prevention

**API Endpoints**:
- `POST /api/v1/sessions` - Create sharing session
- `GET /s/:code` - Access shared files (serves UI)
- `WS /ws/agent` - Agent WebSocket connection
- `WS /ws/client` - Browser WebSocket connection

**Data Flow**:
```
1. Agent authenticates via token
2. Agent creates session with file metadata
3. Server returns alphanumeric code (e.g. a3f9k2xp)
4. Browser connects with code
5. Server pairs agent ↔ browser, sends ICE config (STUN + TURN if available)
6. WebRTC handshake via server
7. P2P or TURN-relayed connection established
8. Server steps back — file data never touches it
```

### 2. OpenCloud Agent (Private)

**Language**: Go
**Location**: Runs alongside OpenCloud instance

**Responsibilities**:
- Connect to signaling server (outbound WebSocket only — no inbound ports required)
- Reconnect automatically with exponential backoff on disconnect
- Handle multiple concurrent WebRTC peer connections (one per active browser session)
- Download files via OpenCloud share links (no admin credentials required)
- Stream files to browser via WebRTC DataChannel (chunked)
- Serve admin web UI on localhost for configuration and share management (Phase 2)

**Configuration**:
```yaml
signaling_server: "wss://share.yourdomain.com"
auth_token: "your-secret-token"

# Only share links from this host will be accepted (prevents SSRF)
allowed_opencloud_host: "opencloud.example.com"

# No OpenCloud credentials needed — access is via share links only

# TURN server (optional)
# If omitted, the bundled Coturn instance is used automatically.
# Supported providers: static credentials, Cloudflare Calls API, Metered.ca API
turn:
  provider: "static"            # static | cloudflare | metered
  urls:
    - "turn:your-turn-server.com:3478"
  username: "your-turn-username"
  credential: "your-turn-credential"
  # For cloudflare provider:
  # api_token: "${CF_TURN_TOKEN}"
  # For metered provider:
  # api_key: "${METERED_API_KEY}"
```

### 3. Coturn (Bundled TURN Server)

**Image**: `coturn/coturn:latest`
**Purpose**: Default TURN relay — used automatically when no third-party TURN is configured

Coturn runs as a sidecar in the same docker-compose as the signaling server. The
signaling server generates short-lived HMAC credentials for each session using a
shared secret, so Coturn requires no per-user account management.

### 4. Web UI (Browser)

**Technology**: Vanilla JS + HTML5 (no frameworks)
**Served by**: Signaling server

**Features**:
- File browser (list folders)
- File download via WebRTC DataChannel
- Progress indicator
- Mobile-responsive

## Technical Details

### Session Lifecycle

```go
type Session struct {
    ID           string        // 8-character alphanumeric code, e.g. "a3f9k2xp" (crypto/rand + base36 lowercase, ~2.8T combinations)
    AgentID      string        // Agent identifier
    ShareURL     string        // OpenCloud public share URL (e.g. https://opencloud.example.com/s/XYZ789)
    CreatedAt    time.Time
    ExpiresAt    time.Time
    MaxDownloads     int    // Optional limit (0 = unlimited)
    Password         string // OpenCloud share password — empty if the share has no password (OpenCloud requires one on creation, but it can be removed after). If non-empty, recipients must provide it to connect.
    Downloads        int    // Current download count
}
```

**MaxDownloads behaviour**: when `Downloads` reaches `MaxDownloads`, the session is
automatically revoked — subsequent `join` attempts receive a `session_expired` error.
In-progress transfers at the moment of revocation are not interrupted.

The **agent** persists sessions to a local JSON file (`~/.opencloudshare/sessions.json`)
— shares survive agent container restarts. On startup the agent re-registers all
non-expired sessions with the signaling server automatically. JSON is used over
SQLite deliberately: zero dependencies, human-readable, and trivially sufficient
for the number of sessions a single agent will ever hold.

The **signaling server** is stateless (in-memory only). A signaling server restart
drops active WebSocket connections; the agent's reconnect logic re-registers sessions
within seconds. Recipients mid-transfer are unaffected (WebRTC connection is independent
of signaling).

### File Transfer: Chunking

WebRTC DataChannels have a practical per-message limit that varies by browser. The
default chunk size is **64KB** — well within the supported range of all modern browsers
(Chrome, Firefox, Safari 15.4+) and a significant improvement over the conservative
16KB recommendation:

- **16KB** was the safe default for old Safari (<15.4, pre-March 2022). At gigabit
  speeds this produces ~7,800 JS events/second in the browser — heavy enough to bottleneck
  the event loop before the network does.
- **64KB** reduces that to ~1,950 events/second, which is comfortable at gigabit. Safe
  on Safari 15.4+ (iPhone 6s and newer, Macs with updated Safari).
- **256KB** is Chrome's upper limit but Safari 15.4's exact ceiling is less certain —
  not worth the risk for marginal gains over 64KB.

```
Agent                          Browser
  │── chunk_header (size, name) ──►│
  │── chunk (64KB) ───────────────►│
  │── chunk (64KB) ───────────────►│
  │── ...                          │
  │── chunk_end ──────────────────►│
```

**Protocol messages over DataChannel**:
```json
{"type": "file_header", "name": "vacation.jpg", "size": 5242880, "mime": "image/jpeg"}
{"type": "chunk", "index": 0, "data": "<base64>"}
{"type": "chunk_end", "checksum": "sha256:..."}
{"type": "error", "message": "file not found"}
```

Backpressure: agent checks `datachannel.BufferedAmount` and pauses sending if it
exceeds a threshold (5MB) to avoid overwhelming the browser.

Dead connection detection uses **no-progress monitoring** rather than a fixed drain
timeout. Every 500ms the agent samples `BufferedAmount`; if it hasn't decreased at
all for 5 consecutive seconds, the connection is considered dead and the DataChannel
is closed. A legitimately slow connection (even sub-1 Mbps) will always be draining
something and never triggers this — only a truly frozen/disconnected peer does.

```
every 500ms:
  if BufferedAmount < lastBufferedAmount → reset stall timer
  else → increment stall timer
  if stall timer ≥ 5s → close DataChannel, cancel transfer
  lastBufferedAmount = BufferedAmount
```

This makes the 5MB buffer and the 5-second stall threshold fully independent — buffer
size can be tuned for throughput without affecting dead-connection sensitivity.

### WebRTC / ICE Flow

1. **Agent** connects to signaling server, registers with auth token
2. **Browser** joins with session code; signaling server sends ICE config:
   - **Coturn enabled** (default): Coturn serves as both STUN and TURN — fully self-hosted, no third parties
   - **Coturn disabled**: falls back to `stun.cloudflare.com:3478` for STUN; TURN only available if a third-party provider is configured
   - Google STUN is not used
3. **Both peers** create `RTCPeerConnection` with the same ICE config
4. **Agent** creates data channel, generates SDP offer
5. **Browser** generates SDP answer; ICE candidate exchange happens via signaling server
6. **ICE negotiation** tries candidates in priority order: host → srflx (STUN) → relay (TURN)
7. **Connection established** — data flows via best available path; signaling server is done

### TURN Credential Generation

For bundled Coturn, the signaling server generates short-lived HMAC credentials
per session (no static passwords, no per-user accounts):

```go
// Coturn HMAC credential generation
// TTL is capped at 24h regardless of session length — long-lived sessions
// (e.g. 7 days) should not produce week-long TURN credentials.
// On reconnect the browser receives fresh credentials for the next 24h window.
turnExpiry := session.ExpiresAt
if cap := time.Now().Add(24 * time.Hour); turnExpiry.After(cap) {
    turnExpiry = cap
}
username := fmt.Sprintf("%d:%s", turnExpiry.Unix(), session.ID)
mac := hmac.New(sha1.New, []byte(coturnSecret))
mac.Write([]byte(username))
credential := base64.StdEncoding.EncodeToString(mac.Sum(nil))
```

Coturn enforces the expiry timestamp embedded in the username field. TURN credential
TTL is capped at 24h — shorter sessions get a matching TTL, longer sessions get
rolling 24h credentials refreshed on reconnect.

For third-party providers (Cloudflare Calls, Metered.ca), the signaling server
calls their API at session creation time to fetch short-lived credentials, then
embeds them in the ICE config sent to the browser.

### OpenCloud Integration via Share Links

The agent downloads files using standard OpenCloud/ownCloud public share URLs.
No admin credentials are required. The share token is extracted from the URL and
used as a credential against OpenCloud's public WebDAV endpoint.

**Share URL validation**: on session creation the agent validates that the share URL
hostname matches the configured `allowed_opencloud_host` to prevent SSRF — the agent
will refuse to fetch from arbitrary URLs. Note: hostname-only validation is vulnerable
to DNS rebinding; for Phase 1 this is acceptable given the homelab threat model, but
a future hardening option is to resolve and pin the IP on first validation.

**Authentication**: Basic Auth with the share token as username and empty password
(or share password if the share is password-protected). The `Public-Token: {token}`
header is also accepted as an alternative to Basic Auth.

> **Verified on OpenCloud 5.2.0** against a real instance. Note: `/dav/public-files/{token}`
> returns 401 — the `remote.php` prefix is required even though OpenCloud is pure Go.

**Single file share** — PROPFIND first to get the actual filename, then GET to stream:
```
PROPFIND https://opencloud.example.com/remote.php/dav/public-files/{token}
  Authorization: Basic {base64(token:)}

GET https://opencloud.example.com/remote.php/dav/public-files/{token}/{filename}
  Authorization: Basic {base64(token:)}
```

**Folder share** — list contents then stream individual files:
```
PROPFIND https://opencloud.example.com/remote.php/dav/public-files/{token}
  Depth: 1
  Authorization: Basic {base64(token:)}

GET https://opencloud.example.com/remote.php/dav/public-files/{token}/{filepath}
  Authorization: Basic {base64(token:)}
```

The agent streams response bodies directly into DataChannel chunks to avoid loading
entire files into memory. This approach works for Nextcloud too, though Nextcloud's
public share endpoint differs (it is PHP-based: `/public.php/webdav/`).

### Security Model

**Agent → Server**:
- WebSocket over TLS (wss://)
- Token-based authentication (static shared secret; rotation is manual)
- Rate limiting per token: max 10 session creations/minute, max 100 active sessions per token

**Browser → Signaling Server**:
- Rate limiting per IP: max 5 failed `join` attempts/minute (unknown session code) with progressive backoff:
  ```
  1st block:  60s cooldown
  2nd block:  10min cooldown
  3rd block:  1hr cooldown
  4th block:  24hr auto-expiring ban
  ```
  - 24hr expiry rather than permanent to avoid collateral damage from CGNAT (shared IPs across thousands of users); IPv6 rotating addresses make permanent IP bans ineffective anyway
  - Note: a `join` with a *valid* code always succeeds at this layer regardless of whether the password is correct — the signaling server doesn't know the password
- Max 3 concurrent WebSocket connections per IP to limit scanning
- **Phase 4b only**: deploy signaling server behind Cloudflare — their bot detection and IP reputation replace DIY IP banning for distributed attacks. WebSockets work through Cloudflare proxy; DTLS file transfer remains end-to-end unaffected.

**Browser → Agent (via DataChannel)**:
- Max 3 wrong password attempts per session per IP — on the 3rd failure the agent closes the DataChannel and blacklists that IP from rejoining that session
  - This catches attackers who guessed a valid session code but don't know the password

**Agent ↔ Browser (WebRTC)**:
- DTLS encryption regardless of path (P2P or TURN relay)
- Signaling server never sees file data in any scenario
- Password is verified by the agent over the DTLS DataChannel — the signaling server never sees it. If the share has a password, the recipient must prove they know it directly to the agent before any files are accessible.

**TURN relay**:
- DTLS-encrypted — even Coturn cannot inspect file contents
- Short-lived HMAC credentials per session (bundled Coturn)
- Third-party TURN credentials fetched fresh per session via provider API

**Browser**:
- Alphanumeric code required to initiate connection
- Password protection if the OpenCloud share has one (checked server-side before pairing; no bypass)
- Session expiration (24h default)
- Optional download limits

### Agent Reconnection

The agent maintains a persistent WebSocket to the signaling server. On disconnect:

```
retry delay = min(base * 2^attempt, max_delay) + jitter
base = 1s, max = 60s
```

The agent's `AgentID` is a stable UUID generated once and persisted in the JSON
state file alongside sessions. The file structure is:

```json
{
  "agent_id": "e3b0c442-98fc-1c14-9afb-f4c8996fb924",
  "sessions": [
    {
      "id": "a3f9k2xp",
      "share_url": "https://opencloud.example.com/s/XYZ789",
      "expires_at": "2026-03-30T12:00:00Z"
    }
  ]
}
```

This allows the signaling server to associate reconnecting agents with their existing sessions.

**Important**: AgentID lives in the state file, not the config file. If you copy
your config to a second machine, a fresh AgentID is generated automatically — no
collision. Only copying the entire state file would cause two agents to share an ID,
which would cause sessions to bounce between them. Don't share state files between machines.

Active WebRTC sessions survive a signaling disconnect — the P2P/TURN connection is
independent of signaling. In-flight transfers are not interrupted by a reconnect.

### Concurrent Sessions

One agent instance handles multiple concurrent sessions. Each session gets its
own `RTCPeerConnection`. The agent does not limit concurrency at the protocol
level; operators can set `MAX_SESSIONS` on the signaling server to cap load.

## Multi-Tenant / Public Hosting

The signaling server is designed to be run as a public service shared by many users
(each with their own agent and OpenCloud instance). The single `auth_token` model
is replaced with per-user API keys when running in public mode.

### Registration Flow

```
User visits share.yourdomain.com/register
  → provides email (no password — magic link or just display key once)
  → server issues a unique API key
  → user puts key in agent config: auth_token: "ocs_abc123..."
```

No passwords, no OAuth complexity — just API keys, similar to how Metered.ca,
Tailscale, and most developer-facing services work.

### New Signaling Server API Endpoints (public mode)

- `POST /api/v1/register` — create account, returns API key
- `GET /api/v1/account` — view account (usage, session count)
- `DELETE /api/v1/account` — delete account and revoke all sessions

### Isolation Guarantees

| Threat | Mitigation |
|---|---|
| User A reads User B's files | Impossible — WebRTC routes to the agent that owns the session; Agent B never receives User A's signaling |
| User A guesses User B's session code | Impractical — 2.8T combinations + 5 failed attempts/min rate limit |
| User A exhausts server resources | Per-API-key session cap (default: 100 active sessions) + rate limiting |
| Signaling server operator reads files | Not possible passively — DTLS end-to-end, password verified agent-side over DataChannel (signaling server never sees the password). Active MitM of the WebRTC handshake is theoretically possible but requires sustained active interception. |
| Compromised agent leaks other users' data | Impossible — agent only accesses its own sessions' share links |

### Updated Agent Config (public mode)

```yaml
signaling_server: "wss://share.opencloudshare.com"
auth_token: "ocs_a3f9k2..."   # unique per user, issued at registration
allowed_opencloud_host: "opencloud.example.com"
```

### Abuse Prevention

- Per-key rate limits (10 session creations/min, 100 active sessions max)
- Per-IP browser rate limits (5 failed join attempts/min)
- Admin can revoke individual API keys without affecting other users
- Optional: invite-only registration to limit signaling server load

### Self-Hosted vs Public

The same binary supports both modes:

```yaml
# Single-user / homelab (default)
mode: "single"
auth_token: "your-static-token"

# Multi-user / public service
mode: "multi"
registration: "open"    # open | invite-only | closed
```

## Implementation Phases

### Phase 1: MVP

**Signaling Server**:
- [ ] WebSocket server with in-memory session management
- [ ] Create session endpoint (`POST /api/v1/sessions`)
- [ ] Pair agent with browser, relay WebRTC signaling
- [ ] ICE config endpoint — serve STUN + Coturn HMAC credentials
- [ ] Simple web UI for code entry and file list

**Coturn**:
- [ ] Add to docker-compose with HMAC shared secret config
- [ ] Signaling server generates per-session HMAC credentials

**OpenCloud Agent**:
- [x] **Verified** public share WebDAV endpoint: `remote.php/dav/public-files/{token}`, Basic Auth `token:` (OpenCloud 5.2.0)
- [ ] JSON file persistence for sessions and stable AgentID (`~/.opencloudshare/sessions.json`)
- [ ] WebSocket client with reconnect/backoff + session re-registration on reconnect
- [ ] WebRTC peer connection with configurable ICE servers
- [ ] File listing via public share WebDAV PROPFIND
- [ ] Single file download via share link with chunking + backpressure
- [ ] Phase 1 CLI: `opencloudshare share <url> [--ttl 24h] [--password x] [--max-downloads 10]`

### Phase 2: Admin UI + Enhanced Features

The agent gains a local web UI (served on `localhost:7878` by default) for
configuration and share management — modelled on the arr suite (Sonarr, Radarr).
No separate app required; just open a browser on the machine running the agent.

**Admin UI — Settings page**:
- Signaling server URL + auth token
- Allowed OpenCloud host (SSRF protection)
- TURN provider selection (bundled Coturn / static / Cloudflare / Metered) + credentials
- Default session TTL and download limits

**Admin UI — Dashboard**:
- List of active shares: code, OpenCloud share URL, expiry, download count, status
- "Create share" button: paste OpenCloud share URL → choose TTL/password/limit → get code + copyable link + optional QR code (for in-person sharing)
- Revoke button per share
- Connection status indicator (agent ↔ signaling server)

**Other Phase 2 items**:
- [ ] Agent local web server + admin UI (Settings + Dashboard)
- [ ] ~~Agent local JSON persistence~~ (moved to Phase 1)
- [ ] Third-party TURN provider support (Cloudflare Calls, Metered.ca)
- [ ] DNS rebinding hardening: resolve and pin `allowed_opencloud_host` IP on first use
- [ ] Binary DataChannel protocol — `[1B type][2B length][payload]` replacing JSON+base64 (~33% less bandwidth, matters for large files); chunk size remains 64KB
- [ ] Folder navigation in recipient browser UI
- [ ] QR code generation in admin UI (for in-person sharing)
- [ ] Password protection on sessions
- [ ] Download limits
- [ ] Session revocation
- [ ] Mobile optimizations for recipient UI

### Phase 3: OpenCloud Extension

Build a native OpenCloud extension that adds a "Share externally via OpenCloudShare"
button to the OpenCloud UI. OpenCloud (OCIS) has an extension/app framework — the
extension calls the agent's local API to create a session and displays the code/QR
in a modal.

> ⚠️ **TODO**: Review OpenCloud extension development docs before starting Phase 3.
> Check https://opencloud.eu/docs and the `opencloud-eu` GitHub org for the app
> framework reference. **Do not rely on OCIS (`owncloud/ocis`) extension docs** —
> OpenCloud's app framework may have diverged.

- [ ] Research OpenCloud extension API and scaffolding
- [ ] Build extension: "Share externally" button on file/folder context menu
- [ ] Extension calls `127.0.0.1:7878/api/local/sessions` on the local agent
- [ ] Displays session code + copyable link + optional QR code in a modal
- [ ] Integration testing with real OpenCloud instance

### Phase 4a: Production Hardening

- [ ] Docker containerization (finalize images)
- [ ] `GET /healthz` endpoints on both signaling server and agent for Docker health checks
- [ ] Structured logging with `LOG_LEVEL=debug` env var for WebRTC troubleshooting
- [ ] Documentation
- [ ] Monitoring/logging
- [ ] Performance optimization

### Phase 4b: Public Multi-Tenant Hosting

- [ ] Multi-tenant mode: per-user API key registration (`POST /api/v1/register`)
- [ ] Registration UI at `/register` (email → API key)
- [ ] Per-key usage tracking and admin dashboard
- [ ] **SQLite** — persists API keys and user registrations on the signaling server; without this, all registered agents lose access on every server restart
- [ ] **Redis** — persists rate limit state across restarts (prevents abuse window during deploys); not needed for API key storage (that's SQLite)
- [ ] **Cloudflare proxy** — put signaling server behind Cloudflare for bot detection, IP reputation, and DDoS protection; replaces DIY progressive IP banning for distributed attacks

## API Specification

### Signaling Server

**Create Session**:
```http
POST /api/v1/sessions
Authorization: Bearer {agent-token}
Content-Type: application/json

{
  "share_url": "https://opencloud.example.com/s/XYZ789",
  "password": "U@y$yx353GYs",  // OpenCloud share password — omit or empty string if the share has no password. If present, recipients must enter it.
  "expires": "24h",
  "max_downloads": 10
}

Response:
{
  "code": "a3f9k2xp",
  "url": "https://share.yourdomain.com/s/a3f9k2xp",
  "expires_at": "2026-03-30T12:00:00Z"
}
```

**WebSocket Protocol**:

Agent messages:
```json
{"type": "register", "token": "..."}
{"type": "offer", "sdp": "...", "session_id": "a3f9k2xp"}
{"type": "ice_candidate", "candidate": "...", "session_id": "a3f9k2xp"}
```

Browser messages:
```json
{"type": "join", "session_id": "a3f9k2xp"}
{"type": "answer", "sdp": "...", "session_id": "a3f9k2xp"}
{"type": "ice_candidate", "candidate": "...", "session_id": "a3f9k2xp"}
```

The signaling server checks only that the session code exists — **no password is sent
to or checked by the signaling server**. The password is verified by the agent directly
over the encrypted DataChannel after the WebRTC connection is established:

```
Browser                        Agent (over DTLS DataChannel)
  │◄── {"type": "auth_required"} ──│  (only sent if session has a password)
  │─── {"type": "auth", "password": "U@y$yx353GYs"} ──►│
  │◄── {"type": "auth_ok"} ─────────│  proceed to file listing
       OR
  │◄── {"type": "auth_failed"} ─────│  DataChannel closed immediately
```

If the session has no password, the agent skips the challenge entirely and proceeds
directly to file listing.

> **Tradeoff**: wrong passwords are now rejected at the agent after a full WebRTC
> handshake rather than at the signaling server before one — more work per failed
> attempt. Two-layer mitigation:
> - **Signaling server** blocks brute-forcing of session codes (max 5 unknown-code failures/min per IP)
> - **Agent** blocks brute-forcing of passwords (max 3 wrong attempts per session per IP, then blacklisted for that session)

> **Note**: OpenCloud currently requires a password when creating a share link, but allows
> removing it afterwards. To create an open (password-free) session, remove the password
> from the share in OpenCloud first. A feature request to make OpenCloud passwords optional
> at creation would eliminate this extra step.

Server → Browser (on successful join):
```json
{
  "type": "session_info",
  "offer": "...",
  "ice_servers": [
    {"urls": "stun:share.yourdomain.com:3478"},
    {
      "urls": "turn:share.yourdomain.com:3478",
      "username": "1743292800:a3f9k2xp",
      "credential": "<hmac-sha1-base64>"
    }
  ]
  // If Coturn is disabled, STUN falls back to: {"urls": "stun:stun.cloudflare.com:3478"}
  // Google STUN is never used
}
```

## Go Dependencies

**Signaling Server**:
```go
import (
    "github.com/coder/websocket"
    "github.com/gin-gonic/gin"  // or net/http
    "crypto/rand"               // session code generation
    "crypto/hmac"               // Coturn HMAC credential generation
)
```

**OpenCloud Agent**:
```go
import (
    "github.com/pion/webrtc/v4"
    "github.com/coder/websocket"
    "github.com/spf13/viper"    // config
    // Share link downloads + JSON persistence: stdlib net/http + encoding/json only
)
```

Note: The signaling server does not need pion/webrtc — it only routes signaling
messages and generates TURN credentials. Only the agent needs pion.

## Deployment Options

### Signaling Server + Bundled Coturn

```yaml
# docker-compose.yml
version: '3'
services:
  share-server:
    image: opencloud/share-server:latest
    ports:
      - "8080:8080"
    environment:
      - SIGNING_KEY=${SIGNING_KEY}
      - MAX_SESSIONS=1000
      - SESSION_TTL=24h
      - COTURN_HOST=coturn
      - COTURN_SECRET=${COTURN_SECRET}
      # Optional: override with third-party TURN
      # - TURN_PROVIDER=cloudflare
      # - CF_TURN_TOKEN=${CF_TURN_TOKEN}

  coturn:
    image: coturn/coturn:latest
    ports:
      - "3478:3478/udp"
      - "3478:3478/tcp"
    command: >
      --use-auth-secret
      --static-auth-secret=${COTURN_SECRET}
      --realm=share.yourdomain.com
      --no-cli
```

### OpenCloud Agent

```yaml
# docker-compose.yml (add to existing OpenCloud stack)
services:
  opencloud-share-agent:
    image: opencloud/share-agent:latest
    ports:
      - "127.0.0.1:7878:7878"    # admin UI — accessible from Docker host only
      # For remote access (e.g. over Tailscale), either:
      # a) SSH tunnel: ssh -L 7878:localhost:7878 yourserver
      # b) Change to "0.0.0.0:7878:7878" and protect with Tailscale ACLs
    environment:
      - SIGNALING_SERVER=wss://share.yourdomain.com
      - AUTH_TOKEN=${SHARE_TOKEN}
      - ADMIN_UI_PORT=7878
      # No OpenCloud credentials — access is via share links only
    networks:
      - opencloud_network
```

## Cost Analysis

**Signaling Server** (public instance):
- Traffic: ~10-20KB per session (SDP offer/answer ~2-4KB each + ICE candidates — no file data ever)
- CPU: Minimal
- Cost: $5-10/month VPS (Hetzner, DigitalOcean)

**Coturn** (bundled, runs on same VPS):
- Only handles connections that fail P2P (~20-35% of users)
- Bandwidth: full file size for those relayed connections, shared with VPS allowance
- Cost: $0 extra (same VPS)

**Third-party TURN** (optional upgrade):
- Offloads relay bandwidth from your VPS entirely
- Cloudflare Calls, Metered.ca: free tiers available — verify current limits
- Cost: $0 on free tier for homelab use; ~$0.05-0.10/GB beyond

**Agent** (runs on your server):
- Bandwidth: $0 — file data goes P2P or via TURN, not through agent's host
- CPU: WebRTC encryption/decryption
- Cost: $0 (uses existing OpenCloud instance)

## Success Metrics

- [ ] Can share files from Tailscale-protected OpenCloud
- [ ] Recipient needs only browser (no VPN, no account)
- [ ] Transfer speed comparable to direct download over P2P
- [ ] Works for all recipients including mobile/corporate NAT via TURN fallback
- [ ] Signaling server handles zero file data in all scenarios
- [ ] Sessions auto-expire and are revocable

## Future Enhancements

- [ ] Multi-file selection and zip download
- [ ] Upload capability (reverse sharing)
- [ ] Real-time collaboration (WebRTC channels for messaging)
- [ ] Integration with other WebDAV-compatible storage systems
- [ ] Mobile app for native experience
- [ ] ~~Binary DataChannel protocol~~ (moved to Phase 2)
- [ ] WebRTC stats exposure in admin UI (RTT, packet loss, transfer speed) for troubleshooting

## Future Platform Support

The agent is designed around a simple abstraction: given a share URL, list its
contents and stream individual files. This maps cleanly onto other platforms:

### Nextcloud
Near-identical to OpenCloud — same ownCloud lineage, same public share link format
(`/s/{token}`), same OCS API for folder listing. Agent support requires minimal
changes, mostly config/branding. Could ship in Phase 2.

### Immich
Immich has shared album links with its own REST API:
- `GET /api/shared-link/{key}` — fetch album metadata and asset list
- `GET /api/assets/{id}/original` — download individual photo/video
- Password-protected albums supported via `X-Api-Key` or `password` query param

Immich is photo/video focused, so the browser UI would need a media preview mode
rather than a generic file list. Planned for a dedicated phase post-OpenCloud
integration.

### Generic WebDAV
Any server exposing a standard WebDAV endpoint (Seafile, nginx with mod_dav, etc.)
can be supported with a `webdav://` share URL scheme — the agent detects the URL
type and uses the appropriate download strategy.

## Contributing to OpenCloud

This system is designed to be contributed upstream to OpenCloud:

1. **Phase 1-2**: Standalone project for testing
2. **Phase 3**: OpenCloud extension API integration
3. **Phase 4**: Merge as official OpenCloud feature

The share-link-based agent is portable beyond OpenCloud — any server with a
compatible public share URL format can act as the file source, broadening the
potential upstream contribution target.
