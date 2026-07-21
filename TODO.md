# ShareBridge TODO

## Agent container and authentication

- Add a stable unauthenticated agent health endpoint and container health check.
- Handle `SIGTERM` so Docker and Komodo stops complete graceful daemon shutdown.
- Pin the runtime container base image instead of using a moving `alpine:latest` tag.
- Run the agent as a non-root user and migrate the persistent data directory ownership safely.
- Replace the API-key/manual dashboard authentication flow with authenticated ShareBridge server login. Passwordless local UI remains allowed until that design is implemented.

## Signaling server delivery

- Add a private GHCR build and Komodo deployment workflow for the signaling server, following the agent image pattern.

## Website

- Reuse the revised [three-lane transport diagram](docs/diagrams/multilane-transport.html) on the ShareBridge website when documenting direct and relay media/download concurrency.

## Transfer roadmap

- Completed: Immich `Download All` implements Immich's ordered `/api/download/info` and `/api/download/archive` ZIP flow and streams each ZIP through the serial bulk lane. A successful multi-part album batch counts as one ShareBridge download.
- Add a serial browser download-job queue so user requests made during an active download wait in order instead of being rejected.
- Design parallel bulk downloads later using transfer IDs and a higher bulk concurrency limit.
- Revisit WebTransport as a relay adapter behind the three-lane API once its browser and deployment ecosystem is mature enough; retain WebRTC for direct peer-to-peer sessions.
