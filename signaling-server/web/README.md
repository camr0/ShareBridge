# ShareBridge web frontend

Single-page app. Bundled with esbuild because we ship js-libp2p which is ESM-only.

## Build

    cd signaling-server/web
    npm install
    npm run build       # produces app.bundle.js
    npm run watch       # rebuild on change

The generated `app.bundle.js` is not checked in — it is produced by `make web-build` during deploy and consumed by the Go embed / static-file handler.