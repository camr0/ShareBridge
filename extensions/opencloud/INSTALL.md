# ShareBridge OpenCloud Extension — Installation

## Prerequisites

- OpenCloud instance (v5.0+)
- ShareBridge agent running (see agent setup)
- Agent URL and API key from the agent settings page (`http://localhost:7878/settings`)

## Network Setup Options

The extension runs in your browser and connects directly to the ShareBridge agent. For this to work, **the protocols must match** — an HTTPS page cannot make requests to an HTTP endpoint (mixed content is blocked by browsers).

| OpenCloud URL | ShareBridge Agent URL | Works? | Notes |
|---------------|----------------------|--------|-------|
| `http://192.168.1.50:9200` | `http://192.168.1.100:7878` | ✅ Yes | Both HTTP — no mixed content issues |
| `https://cloud.tailnet.ts.net` | `https://agent.tailnet.ts.net:7878` | ✅ Yes | Both HTTPS — Tailscale provides certs |
| `https://cloud.tailnet.ts.net` | `http://192.168.1.100:7878` | ❌ No | Mixed content: HTTPS page → HTTP resource |

### Option A: Local Network (HTTP only)

Best for LAN-only access without HTTPS.

1. OpenCloud: `http://192.168.x.x:9200`
2. ShareBridge agent: `http://192.168.x.x:7878`
3. In extension settings, enter: `http://192.168.x.x:7878`

### Option B: Tailscale (HTTPS everywhere) — Recommended

Best for remote access with proper security.

1. Ensure your OpenCloud instance is accessible via Tailscale HTTPS:
   - OpenCloud: `https://cloud.your-tailnet.ts.net`
2. Enable HTTPS for your ShareBridge agent machine in Tailscale:
   - Go to [Tailscale Admin Console](https://login.tailscale.com/admin) → DNS → Enable HTTPS
3. The agent will be accessible at:
   - `https://agent-machine.your-tailnet.ts.net:7878`
4. In extension settings, enter the Tailscale HTTPS URL with port

> **Tip:** If running OpenCloud and the ShareBridge agent on the same machine, you can use:
> - `https://cloud.your-tailnet.ts.net:7878` (same domain, different port)

### CORS Configuration

The ShareBridge agent must allow requests from your OpenCloud origin. Set the environment variable when starting the agent:

```bash
CORS_ALLOWED_ORIGINS="https://cloud.your-tailnet.ts.net,http://192.168.1.50:9200" sharebridge agent
```

Or in your agent's `.env` file:
```
CORS_ALLOWED_ORIGINS=https://cloud.your-tailnet.ts.net
```

## Build

```bash
cd extensions/opencloud
npm install
npm run build
```

Output: `dist/web-app-sharebridge.js`

## Install in OpenCloud

1. Copy `dist/web-app-sharebridge.js` to your OpenCloud web apps directory:

   ```bash
   cp dist/web-app-sharebridge.js /path/to/opencloud/apps/web-app-sharebridge.js
   ```

   The exact path depends on your OpenCloud deployment. For Docker:
   ```bash
   docker cp dist/web-app-sharebridge.js <container>:/var/lib/opencloud/web/apps/
   ```

2. Register the app in OpenCloud's `web.yaml` (or equivalent config):

   ```yaml
   web:
     apps:
       - web-app-sharebridge
   ```

3. Restart OpenCloud web service.

## Configure the Extension

1. Open OpenCloud and select any file.
2. Click the ShareBridge panel in the right sidebar.
3. Enter your agent URL (see Network Setup Options above) and API key.
4. Click Save. The panel will load your existing shares.

## Verify

Select a file → ShareBridge panel appears → "Create ShareBridge Share" button is visible.
