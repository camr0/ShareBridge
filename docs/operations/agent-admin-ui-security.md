# Agent admin UI — bind, authentication and exposure

Security posture for the ShareBridge agent's local admin surface
(`agent/internal/web`): the settings page, the dashboard/history pages, the
share create/revoke endpoints, the settings-mutation endpoint and the secret
reveal endpoint.

## Defaults (secure by default)

| Setting       | Default     | Effect |
|---------------|-------------|--------|
| `UI_ADDR`     | `127.0.0.1` | The admin UI listens on **loopback only**. A fresh/default deployment is not reachable from the network. |
| `UI_PORT`     | `7878`      | HTTP port for the admin UI. |
| `UI_PASSWORD` | *(empty)*   | No credential. Safe only because the default bind is loopback. |

`agent/docker-compose.yml` uses `network_mode: host`, so the container's
`127.0.0.1` is the home server's loopback: the default deployment is reachable
from that host only.

**Loopback + no password = local trust.** Any process on the home server can
drive the admin UI. That is intentional (the same user can already read
`~/.sharebridge/config.json`). It is *not* a boundary against local processes.

## Fail-closed rule for non-loopback binds

If `UI_ADDR` (from the environment or `config.json`) names a non-loopback
address and `UI_PASSWORD` is empty, the agent **refuses to start** with an
error naming both settings. There is no silent downgrade to loopback.

```
refusing to start: UI_ADDR="0.0.0.0" binds a non-loopback interface but
UI_PASSWORD is empty; set UI_PASSWORD to require authentication or set
UI_ADDR=127.0.0.1 to serve the admin UI on loopback only
```

Loopback is `127.0.0.0/8`, `::1` and `localhost` (optionally with a port).
`0.0.0.0`, `::`, LAN IPs and hostnames are **not** loopback.

### To expose the admin UI on the network

```bash
UI_ADDR=0.0.0.0
UI_PASSWORD='<strong password>'
```

Then every dynamic admin surface requires HTTP Basic auth (the username is
ignored). Unauthenticated requests — including ones that forge the
`HX-Request` / `X-Requested-With` headers — are rejected before any handler
runs. `GET /api/settings/secrets` (the reveal endpoint) is behind the same
check.

## Breaking change (1.x admin UI)

Before this change the default `UI_ADDR` was `0.0.0.0`. Any existing
deployment that **explicitly** set a non-loopback `UI_ADDR` (or kept the old
default in its `.env`/`docker-compose` override) and has **no** `UI_PASSWORD`
will now fail to start. Fix with one of:

- set `UI_PASSWORD` (recommended when remote access is required), or
- set `UI_ADDR=127.0.0.1` and reach the UI through an SSH port-forward.

A deployment that never set `UI_ADDR` and was relying on the old
all-interfaces default is *not* a breaking case — it simply stops being
network-reachable, which is the point of the change.

## TLS / plaintext warning

The admin UI speaks plaintext HTTP and Basic auth is only base64-encoded. Do
**not** expose a non-loopback `UI_ADDR` directly on an untrusted network.

- Preferred: leave `UI_ADDR=127.0.0.1` and tunnel (SSH `-L`, WireGuard,
  Tailscale) to it.
- Otherwise terminate TLS in a trusted reverse proxy in front of the agent and
  only expose the proxy. The agent's same-origin CSRF check compares the
  `Origin` host to the request `Host` (scheme-agnostic), so it keeps working
  behind a TLS-terminating proxy.

## Secret handling on the settings page

The settings page never embeds the control API key or the agent API key in its
HTML. It renders a mask (`••••••••`) and fetches the real value from
`GET /api/settings/secrets` only when the user explicitly clicks **Show** or
**Copy**. That endpoint is an admin surface: it is subject to the same auth
middleware and is never cached (`Cache-Control: no-store`). Submitting the
form with the mask untouched preserves the stored key.

## Same-origin enforcement for mutations

When no password is configured (loopback-only local trust), state-changing
requests are still protected: `POST`/`PUT`/`DELETE` require a same-origin
`Origin` header when one is present, plus a JS-set request header
(`HX-Request` or `X-Requested-With`). A cross-origin page cannot forge
`Origin`, so a forged `HX-Request` alone does not authorize a mutation.
