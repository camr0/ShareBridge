# ShareBridge TODO

## Agent container and authentication

- Add a stable unauthenticated agent health endpoint and container health check.
- Handle `SIGTERM` so Docker and Komodo stops complete graceful daemon shutdown.
- Pin the runtime container base image instead of using a moving `alpine:latest` tag.
- Run the agent as a non-root user and migrate the persistent data directory ownership safely.
- Replace the API-key/manual dashboard authentication flow with authenticated ShareBridge server login. Passwordless local UI remains allowed until that design is implemented.

## Signaling server delivery

- Add a private GHCR build and Komodo deployment workflow for the signaling server, following the agent image pattern.
