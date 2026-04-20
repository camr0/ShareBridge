# ShareBridge Signaling Server

WebSocket-based signaling server for WebRTC peer connection establishment.

## Secure Relay

ShareBridge uses a secure relay architecture that replaces traditional TURN servers:

- **Cloudflare STUN**: Uses Cloudflare's free public STUN server (`stun:stun.cloudflare.com:3478`) for NAT traversal
- **Secure Relay**: WebRTC traffic is relayed through an encrypted tunnel when direct connection fails
- **No TURN server required**: Eliminates the complexity of deploying and managing Coturn

Users behind symmetric NAT (typically corporate/ISP firewalls) will automatically use relay mode. Connection status in the browser shows "Connected (Relay)" when relay is used.

### Configuration

| Environment Variable | Description | Default |
|---------------------|-------------|---------|
| `PORT` | Server port | `8080` |
| `DATA_DIR` | PocketBase data directory | `./pb_data` |
| `STUN_URL` | STUN server URL | `stun:stun.cloudflare.com:3478` |
| `RELAY_JWT_SECRET` | JWT secret for relay authentication | (required) |
| `RELAY_PENDING_WAIT_WINDOW` | How long relay waits for peer | `30s` |
| `SMTP_HOST` | SMTP server hostname | (optional) |
| `SMTP_PORT` | SMTP server port | `587` |
| `SMTP_USER` | SMTP username | (optional) |
| `SMTP_PASSWORD` | SMTP password | (optional) |

**Important:** Generate a secure `RELAY_JWT_SECRET` for production:

```bash
openssl rand -base64 32
```

### Running with Docker Compose

```bash
# Create .env file
cp .env.example .env
# Edit .env with your values, especially RELAY_JWT_SECRET

# Start services
docker-compose up -d
```

### Architecture

The secure relay mode uses the signaling server as a relay when direct WebRTC connection fails:

1. Browser and agent attempt direct WebRTC connection via STUN
2. If direct connection fails (symmetric NAT), both sides open a relay tunnel
3. Traffic flows: Browser <-> Signaling Server <-> Agent
4. All relay traffic is encrypted end-to-end with WebRTC DataChannel encryption
5. The agent's IP address remains hidden from recipients
