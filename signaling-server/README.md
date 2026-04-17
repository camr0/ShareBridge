# ShareBridge Signaling Server

PocketBase-backed signaling server with an embedded libp2p relay for ShareBridge transfers.

## Relay Deployment

Slice 13a removes Coturn and Prometheus from the Docker Compose setup. The server now runs:
- the PocketBase HTTP app on `:8080`
- an embedded libp2p relay on `127.0.0.1:9001` by default

The relay should stay bound to loopback and be exposed through Caddy or another reverse proxy at a public WebSocket endpoint such as `relay.sharebridge.app`.

### Configuration

| Environment Variable | Description | Default |
|---------------------|-------------|---------|
| `PORT` | Server port | `8080` |
| `DATA_DIR` | PocketBase data directory | `./pb_data` |
| `RELAY_LISTEN_ADDR` | Relay listen multiaddr | `/ip4/127.0.0.1/tcp/9001/ws` |
| `RELAY_ANNOUNCE_ADDR` | Public relay multiaddr advertised to clients | (required in production) |
| `RELAY_PRIVATE_KEY_PATH` | Path to persisted relay private key | (optional) |
| `JWT_SECRET` | Shared secret for relay JWT issuance/validation | (required in production) |
| `JWT_TTL` | Relay JWT lifetime | `5m` |
| `SMTP_HOST` | SMTP server hostname | (optional) |
| `SMTP_PORT` | SMTP server port | `587` |
| `SMTP_USER` | SMTP username | (optional) |
| `SMTP_PASSWORD` | SMTP password | (optional) |
| `DEFAULT_QUOTA_GB` | Default monthly relay quota per user | `50` |
| `QUOTA_CHECK_INTERVAL` | Flush interval for relay quota usage | `5m` |

### Production Notes

- `RELAY_ANNOUNCE_ADDR` must be a public, dialable multiaddr such as `/dns4/relay.sharebridge.app/tcp/443/wss`.
- `JWT_SECRET` should be a strong random secret. Generate one with `openssl rand -hex 32`.
- `RELAY_PRIVATE_KEY_PATH` should point at persistent storage so the relay peer ID survives restarts.
- The public relay endpoint should bypass Cloudflare proxying and terminate TLS at Caddy before forwarding to `127.0.0.1:9001`.

### Running with Docker Compose

```bash
# Create .env file
cp .env.example .env
# Edit .env with your values

# Start services
docker-compose up -d
```
