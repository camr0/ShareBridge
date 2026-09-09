# ShareBridge

ShareBridge creates browser links for privately hosted OpenCloud files and Immich albums without exposing either backend directly to recipients. It is pre-1.0 software; `v0.8.0` is the first formal [Semantic Versioning](https://semver.org/) release.

## Capabilities

- OpenCloud file and directory shares
- Immich albums, protected shares, image previews, streamed video playback and seeking
- Direct WebRTC and end-to-end encrypted secure-relay transfers
- Independent control, interactive-media, thumbnail, and bulk scheduling
- Streaming downloads with byte-count and checksum validation
- Multi-part Immich Download All archives
- Progressive 120-item gallery loading for large albums
- Accounts, API keys, session expiry, download limits, and relay quotas

## Architecture

```text
Recipient browser ↔ signaling/secure-relay server ↔ private ShareBridge agent ↔ OpenCloud or Immich
```

ShareBridge has three running components:

- The browser opens a share link, renders files or galleries, previews media, and receives downloads.
- The signaling server coordinates sessions and accounts and provides an encrypted relay when a direct connection is unavailable.
- The private agent discovers shares and reads data from OpenCloud or Immich on the owner's network.

Direct mode sends data peer-to-peer over WebRTC. Relay mode sends payloads through the signaling server, but Noise encryption remains end-to-end between the browser and agent.

## Repository layout

- `agent/` — private-network agent and backend integrations
- `control/` — signaling, accounts, secure relay, and browser application
- `control/web/` — browser client
- `docs/` — architecture, design, operations, and release documentation

The control plane has additional setup and configuration guidance in [control/README.md](control/README.md).

## Development

Run the Go and browser test suites from the repository root:

```bash
cd agent
go test ./...
go vet ./...

cd ../control
go test ./...

cd web
npm test
```

Cross-browser route-flow gate (Playwright; Chromium, Firefox, and WebKit
against the hermetic real-agent + control-surface fixture — Task 24, spec
§23.4). The control interstitial assets and the agent behavior (admission,
connect endpoint, gallery) are the real shipped code; route dispatch,
prepare-route JSON, relay-URL derivation, and CSP string construction are
fixture ports of the control implementations, with the ported CSP string
drift-pinned by the shared golden — see the disclosed boundary in
`e2e/browser/fixture.mjs`. Requires the Playwright browsers
(`npx playwright install`) on first run:

```bash
cd e2e/browser
npx playwright test --project=chromium --project=firefox --project=webkit
```

The fixture (control interstitial surface, loopback CONNECT proxy, and the
real agent data plane) is booted by `global-setup.mjs` and torn down after
the run; Safari macOS/iOS manual smoke steps live in
[e2e/browser/PHASE-B-SAFARI.md](e2e/browser/PHASE-B-SAFARI.md).

## Deployment and compatibility

Production deployment is currently operator-managed. See [docs/RELEASING.md](docs/RELEASING.md) for the complete verification, publication, deployment, smoke-test, and rollback checklist.

The ShareBridge product version is independent of its transport protocol, message envelope, configuration schema, and supported Immich API versions. Compatibility changes in those interfaces are documented separately and do not imply matching version numbers.

See the [changelog](CHANGELOG.md) for released behavior, the [roadmap](TODO.md) for unfinished work, and [design specifications](docs/superpowers/specs/) for architectural background. Design documents may include proposals that are not shipped.
