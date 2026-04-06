# Slice 10a — Accounts (PocketBase)

## Goal

Add user accounts to the signaling server so OpenCloud self-hosters can register, log in, and self-serve their own API keys — without operator intervention. Replace the static `ADMIN_TOKEN` key management model with per-account ownership. Migrate sessions and API keys to be account-scoped.

---

## Architecture

PocketBase replaces Gin as the HTTP server and SQLite layer. The signaling server becomes a PocketBase application: all domain collections (`api_keys`, `sessions`) live in PocketBase's SQLite database alongside the built-in `users` auth collection. WebSocket handlers and REST routes register via PocketBase's `OnServe` hook.

```
Before:
  Gin HTTP server
  └─ SQLite (api_keys, sessions)
  └─ ADMIN_TOKEN for key management

After:
  PocketBase HTTP server (one process, one DB)
  ├─ users collection (PocketBase built-in auth)
  ├─ api_keys collection (owned by user)
  ├─ sessions collection (owned by api_key → user)
  ├─ /ws/agent, /ws/client, /sessions/:code (custom routes via OnServe)
  └─ /register, /login, /account (new user-facing pages)
```

**One process, one SQLite DB.** No Gin dependency, no separate auth service, no external infrastructure.

---

## PocketBase Collections

### `users` (built-in auth collection)

PocketBase's built-in auth collection. Provides:
- Email/password registration
- Email verification (SMTP configurable)
- Password reset via email
- JWT issuance on login

No custom fields needed for 10a. Future slices (10b, 10c) will add quota and billing fields.

### `api_keys` collection

| Field | Type | Notes |
|-------|------|-------|
| `id` | Auto (PocketBase) | Replaces `ak_xxx` format |
| `account_id` | Relation → users | Required, CascadeDelete: true |
| `key_hash` | Text | bcrypt hash of full key, not exposed via API |
| `label` | Text | Optional human-readable name (e.g. "home server") |
| `last_used_at` | Date | Nullable, updated on each use |
| `is_active` | Bool | Default true; false = revoked |
| `created` | Auto (PocketBase) | |

API rules: only the owning user can read/create/delete their own keys. `key_hash` is never returned in API responses (hidden field).

**Key format change:** Keys no longer use `ak_xxx.secret` format. The full key is `<record_id>.<random_secret>` — same pattern, different ID source. The `id` comes from PocketBase's auto-generated record ID.

**Key generation:** The secret is 32 bytes from `crypto/rand`, base64url-encoded (no padding), giving ~43 characters of entropy. The full key (`<record_id>.<secret>`) is returned once in the `POST /api/keys` response and never stored — only `key_hash` (bcrypt) is persisted. The `/account` UI displays the full key in a copy-once dialog with a warning.

### `sessions` collection

| Field | Type | Notes |
|-------|------|-------|
| `id` | Auto (PocketBase) | Internal ID |
| `code` | Text | Unique, 8 chars, the share code |
| `api_key_id` | Relation → api_keys | Required, CascadeDelete: true |
| `agent_id` | Text | UUID of connected agent, nullable |
| `expires_at` | Date | Nullable |
| `created` | Auto (PocketBase) | |
| `updated` | Auto (PocketBase) | |

`CascadeDelete: true` on `api_key_id` — deleting an API key deletes all its sessions. This is PocketBase's built-in relation cascade (application-layer, not SQLite foreign keys) — no manual hooks required. API rules: sessions are not directly readable by end users (only agents and the signaling server logic access them).

**Same-account reclaim rule:** if account A reconnects to an existing code using a different API key owned by the same account, the session is reassigned to the new `api_key_id` during reconnect. This transfer is required so revoking the old key does not cascade-delete the reclaimed session.

---

## Session Ownership Chain

```
users ──< api_keys ──< sessions
```

- A user owns N API keys
- An API key owns N sessions
- Deleting an API key cascades to its sessions
- Deleting a user cascades to their API keys (and transitively their sessions)

`IsCodeAvailable` is no longer the final ownership decision. Reconnect uses an atomic claim-or-reassign operation:
- If no session with the code exists, create it.
- If the code exists and belongs to the same account, update that session in place and set `session.api_key_id = requesting_api_key_id`.
- If the code exists and belongs to a different account, reject it as unavailable.

```
claim succeeds if:
  no session with this code exists
  OR
  session exists AND session.api_key.account_id == requesting agent's account_id
    -> then transfer session.api_key_id to the reconnecting key
```

A session owned by a *different* account blocks the code (treat as unavailable).

---

## HTTP Server Migration: Gin → PocketBase

`cmd/server/main.go` is rewritten to initialize PocketBase and register all routes via `OnServe`:

```go
app := pocketbase.New()

app.OnServe().BindFunc(func(se *core.ServeEvent) error {
    // Register custom routes
    se.Router.GET("/ws/agent", handler.AgentWS(app, hub, cfg))
    se.Router.GET("/ws/client", handler.BrowserWS(app, hub, cfg))
    se.Router.GET("/sessions/{code}", handler.GetSession(app))
    se.Router.GET("/s/{code}", handler.DirectLink())
    se.Router.GET("/", handler.ServeIndex())
    se.Router.GET("/app.js", handler.ServeAppJS())

    // Account-scoped API key management (replaces /admin/api/keys)
    se.Router.POST("/api/keys", handler.CreateAPIKey(app), apis.RequireAuth())
    se.Router.GET("/api/keys", handler.ListAPIKeys(app), apis.RequireAuth())
    se.Router.DELETE("/api/keys/{id}", handler.RevokeAPIKey(app), apis.RequireAuth())

    // Cron: clean up expired sessions every 5 minutes
    app.Cron().MustAdd("expiry_cleanup", "*/5 * * * *", func() {
        app.DB().NewQuery("DELETE FROM sessions WHERE expires_at < datetime('now')").Execute()
    })

    return se.Next()
})

app.Start()
```

`apis.RequireAuth()` is PocketBase's built-in middleware — validates the `Authorization: Bearer <jwt>` header and puts the authenticated user record into context.

---

## API Key Management Endpoints

These replace the old `/admin/api/keys` endpoints. No `ADMIN_TOKEN` required — the user authenticates with their PocketBase JWT.

| Endpoint | Auth | Description |
|----------|------|-------------|
| `POST /api/keys` | User JWT | Create new API key for authenticated user |
| `GET /api/keys` | User JWT | List authenticated user's API keys |
| `DELETE /api/keys/:id` | User JWT | Revoke a key (must own it) |

Each handler extracts `account_id` from the authenticated user record in context, then scopes all DB queries to that account. A user cannot read or delete another user's keys — enforced in the handler, not just PocketBase collection rules.

---

## Agent WebSocket Auth

No change to the agent-facing protocol. The agent still sends `?api_key=<key>` on WebSocket connect. The `APIKeyAuth` middleware is updated to validate against the PocketBase `api_keys` collection instead of the old SQLite table.

Validation logic:
1. Parse `<record_id>.<secret>` from the query param
2. Look up the `api_keys` record by `record_id` via `app.FindRecordById`
3. bcrypt compare `<secret>` against stored `key_hash`
4. Check `is_active = true`
5. Update `last_used_at`
6. Store `api_key_id` and `account_id` in the request context

---

## Agent Lifecycle on Key Revocation

When an API key is revoked (`DELETE /api/keys/{id}`):
1. Key is marked `is_active = false` in PocketBase.
2. The hub's `CloseAgent(apiKeyID)` is called immediately — closes the live WebSocket if one exists.
3. `CloseAgent` also removes in-memory code mappings and pairings for that key so revoked shares stop resolving immediately, not only after process restart.
4. The agent is ejected from the hub and will fail to reconnect (next connect attempt hits `ValidateAPIKey` → nil).

`hub.CloseAgent` must clean up all hub state tied to the revoked key, not just the socket. The `RevokeAPIKey` handler receives the hub as a parameter alongside `app`.

---

## User-Facing Web Pages

Three new pages served by the signaling server:

### `/register`
- Email + password registration form
- Calls PocketBase's `POST /api/collections/users/records`
- On success: redirects to `/login` with a generic post-registration notice
- If SMTP is configured, the notice tells the user to check email for verification before login
- If SMTP is not configured, email verification is treated as disabled for this self-hosted install and the user may log in immediately

### `/login`
- Email + password login form
- Calls PocketBase's `POST /api/collections/users/auth-with-password`
- On success: stores JWT in `localStorage`, redirects to `/account`

### `/account`
- API key management dashboard
- Lists existing keys (label, created date, last used, active status)
- "Create new key" button: prompts for optional label, calls `POST /api/keys`, shows full key once with copy button
- "Revoke" button per key
- JWT from `localStorage` sent as `Authorization: Bearer <jwt>` on all API calls

These are simple HTML+JS pages consistent with the existing browser UI style (Catppuccin dark theme, same CSS).

---

## Admin UI Access Control

PocketBase's `/_/` admin UI is for the server operator only. It must not be exposed on the public reverse-proxied site.

- Primary control: block `/_/` in nginx/Caddy/Traefik and return 404/deny before the request reaches PocketBase.
- Optional defense in depth: a server-side localhost check may remain for direct non-proxied access, but it is not sufficient by itself because a same-host reverse proxy makes `RemoteAddr` appear local.
- Operator access is via SSH tunnel or direct loopback-only access.

Superuser account: on first start with no superuser, PocketBase prints a one-time setup URL to the logs. Operator visits it once to set their superuser password.

---

## Migration: Existing Data

For operators upgrading from a pre-10a install:

1. Existing sessions and API keys in the old `signaling.db` are not automatically migrated — the DB schema is being replaced, not extended.
2. The migration path is: create a new account, generate a new API key, update the agent config.
3. Old `signaling.db` can be kept as a backup but is no longer read.

For the first deployment (fresh VPS), there is no migration concern.

This is acceptable for 10a — the project has no production users yet. A migration tool can be added in Slice 12 (Production Hardening) if needed.

---

## Environment Variables

| Variable | Purpose | Change |
|----------|---------|--------|
| `PORT` | HTTP listen port | Unchanged, but `cmd/server/main.go` must wire PocketBase to listen on this port explicitly |
| `DATABASE_PATH` | SQLite path | Unchanged, but `cmd/server/main.go` must point PocketBase at this data/DB location explicitly |
| `TURN_HOST`, `TURN_PORT`, `TURN_SECRET` | TURN config | Unchanged |
| `SMTP_HOST`, `SMTP_PORT`, `SMTP_USER`, `SMTP_PASSWORD` | Email for verification/reset | New (optional; disables email flows if absent) |
| `ADMIN_TOKEN` | Static admin token | **Removed** (replaced by PocketBase superuser) |
| `AUTH_TOKEN` | Deprecated token | **Removed** |

SMTP config is passed to PocketBase's mailer settings on startup. If absent, the server runs in no-verification mode for end-user accounts: registration still works, but login does not require a verified email.

---

## cmd/admin CLI Tool

The existing `cmd/admin` CLI tool is updated:

- `migrate` command removed (PocketBase runs migrations automatically on start)
- `create-key` command removed (users self-serve via `/account` page)
- `list-keys` command removed
- `revoke-key` command removed
- New: `create-superuser` — creates the initial PocketBase superuser (wraps PocketBase's superuser creation for scripted deploys)

The CLI tool becomes minimal — most management happens via the web UI.

---

## What Does Not Change

- WebRTC peer creation, ICE negotiation, DataChannel — unchanged
- HMAC pre-challenge protocol (knock/nonce/join) — unchanged
- Browser file transfer UI (`app.js`, `index.html`) — unchanged
- Agent code — unchanged (still sends `?api_key=` on connect)
- TURN credential generation — unchanged
- Session code format and uniqueness — unchanged

---

## Breaking Changes

| What | Before | After |
|------|--------|-------|
| API key format | `ak_xxx.secret` | `<pb_record_id>.<secret>` |
| Key management | `ADMIN_TOKEN` + `/admin/api/keys` | User JWT + `/api/keys` |
| DB file | `signaling.db` (custom schema) | `signaling.db` (PocketBase schema) |
| `share_url` | Stored in sessions table (unused) | **Removed** — server never needed it; agent retains it locally |
| Server framework | Gin | PocketBase (net/http router) |
| Key creation | CLI `create-key` command | `/account` web page |

---

## Out of Scope

- OAuth2 social login (can be enabled in PocketBase later with zero code changes)
- Bandwidth tracking / quota enforcement (Slice 10b)
- Stripe billing (Slice 10c)
- Agent pairing flow (Slice 11)
- Rate limiting on auth endpoints (Slice 12)
