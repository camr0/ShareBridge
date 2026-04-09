# ShareBridge OpenCloud Extension — Installation

## Prerequisites

- OpenCloud instance (v5.0+)
- ShareBridge agent running (see agent setup)
- Agent URL and API key from the agent settings page (`http://localhost:7878/settings`)

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
3. Enter your agent URL (e.g. `http://localhost:7878`) and API key.
4. Click Save. The panel will load your existing shares.

## Verify

Select a file → ShareBridge panel appears → "Create ShareBridge Share" button is visible.