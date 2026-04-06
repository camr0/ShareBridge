# ShareBridge Signaling Server

WebSocket-based signaling server for WebRTC peer connection establishment.

## TURN Server

The docker-compose setup includes Coturn for TURN relay. Set the TURN_SECRET environment variable to a secure random value:

```bash
openssl rand -hex 32
```

Users behind symmetric NAT (typically corporate/ISP firewalls) will automatically use TURN relay. Connection status in the browser shows "Connected (Relay)" when TURN is used.

### Configuration

| Environment Variable | Description | Default |
|---------------------|-------------|---------|
| `PORT` | Server port | `8080` |
| `DATABASE_PATH` | SQLite database path | `./signaling.db` |
| `TURN_HOST` | Coturn public IP/domain | (optional) |
| `TURN_PORT` | Coturn port | `3478` |
| `TURN_SECRET` | HMAC shared secret for TURN | (optional) |
| `SMTP_HOST` | SMTP server hostname | (optional) |
| `SMTP_PORT` | SMTP server port | `587` |
| `SMTP_USER` | SMTP username | (optional) |
| `SMTP_PASSWORD` | SMTP password | (optional) |

**Important:** `TURN_HOST` must be your VPS's public IP or domain name. Browsers receive this in ICE config and connect directly to it. Do NOT use Docker hostnames like "coturn" - browsers cannot resolve them.

### Running with Docker Compose

```bash
# Create .env file
cp .env.example .env
# Edit .env with your values

# Start services
docker-compose up -d
```