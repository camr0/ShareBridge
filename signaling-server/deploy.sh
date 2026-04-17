#!/bin/bash
# ShareBridge Signaling Server Deployment Script
# Run this on a fresh Debian 13 (or Ubuntu 24.04) VPS

set -e

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

echo -e "${GREEN}Starting ShareBridge Signaling Server deployment...${NC}"

# Update system
echo -e "${YELLOW}Updating system packages...${NC}"
apt update && apt upgrade -y

# Install essentials that Debian minimal might miss
echo -e "${YELLOW}Installing essential packages...${NC}"
apt install -y \
    curl \
    wget \
    git \
    vim \
    nano \
    htop \
    ufw \
    fail2ban \
    apt-transport-https \
    ca-certificates \
    gnupg \
    lsb-release \
    software-properties-common

# Configure timezone (adjust as needed)
echo -e "${YELLOW}Setting timezone to UTC...${NC}"
timedatectl set-timezone UTC

# Install Docker
echo -e "${YELLOW}Installing Docker...${NC}"
install -m 0755 -d /etc/apt/keyrings
curl -fsSL https://download.docker.com/linux/debian/gpg | gpg --dearmor -o /etc/apt/keyrings/docker.gpg
chmod a+r /etc/apt/keyrings/docker.gpg

echo "deb [arch="$(dpkg --print-architecture)" signed-by=/etc/apt/keyrings/docker.gpg] https://download.docker.com/linux/debian \
  "$(. /etc/os-release && echo "$VERSION_CODENAME")" stable" | \
  tee /etc/apt/sources.list.d/docker.list > /dev/null

apt update
apt install -y docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin

# Add user to docker group (replace 'deploy' with your username if different)
usermod -aG docker root

# Configure UFW
echo -e "${YELLOW}Configuring UFW firewall...${NC}"
ufw default deny incoming
ufw default allow outgoing

# SSH
ufw allow 22/tcp

# HTTP/HTTPS (Caddy)
ufw allow 80/tcp
ufw allow 443/tcp

# Enable UFW (will prompt for confirmation - auto-confirm with --force)
ufw --force enable

# Configure fail2ban (basic defaults are fine)
echo -e "${YELLOW}Configuring fail2ban...${NC}"
systemctl enable fail2ban
systemctl start fail2ban

# Install Caddy
echo -e "${YELLOW}Installing Caddy...${NC}"
apt install -y debian-keyring debian-archive-keyring apt-transport-https
curl -1sLf 'https://dl.cloudsmith.io/public/caddy/stable/gpg.key' | gpg --dearmor -o /usr/share/keyrings/caddy-stable-archive-keyring.gpg
curl -1sLf 'https://dl.cloudsmith.io/public/caddy/stable/debian.deb.txt' | tee /etc/apt/sources.list.d/caddy-stable.list
apt update
apt install -y caddy

# Create app directory
echo -e "${YELLOW}Creating application directory...${NC}"
mkdir -p /opt/sharebridge/signaling-server
cd /opt/sharebridge/signaling-server

# Generate secrets (save these!)
echo -e "${YELLOW}Generating secrets...${NC}"
JWT_SECRET=$(openssl rand -hex 32)

# Create .env file
cat > .env <<EOF
# ShareBridge Signaling Server Configuration
PORT=8080
DATA_DIR=/data/pb_data
RELAY_LISTEN_ADDR=/ip4/127.0.0.1/tcp/9001/ws
RELAY_ANNOUNCE_ADDR=/dns4/relay.example.com/tcp/443/wss
RELAY_PRIVATE_KEY_PATH=/data/relay.key
JWT_SECRET=${JWT_SECRET}
JWT_TTL=5m
DEFAULT_QUOTA_GB=50
QUOTA_CHECK_INTERVAL=5m
EOF

# Create docker-compose.yml
cat > docker-compose.yml <<'EOF'
services:
  sharebridge-server:
    image: sharebridge/signaling-server:latest
    container_name: sharebridge-server
    restart: unless-stopped
    network_mode: host
    environment:
      - PORT=${PORT:-8080}
      - DATA_DIR=${DATA_DIR:-/data/pb_data}
      - RELAY_LISTEN_ADDR=${RELAY_LISTEN_ADDR:-/ip4/127.0.0.1/tcp/9001/ws}
      - RELAY_ANNOUNCE_ADDR=${RELAY_ANNOUNCE_ADDR}
      - RELAY_PRIVATE_KEY_PATH=${RELAY_PRIVATE_KEY_PATH:-/data/relay.key}
      - JWT_SECRET=${JWT_SECRET}
      - JWT_TTL=${JWT_TTL:-5m}
      - DEFAULT_QUOTA_GB=${DEFAULT_QUOTA_GB:-50}
      - QUOTA_CHECK_INTERVAL=${QUOTA_CHECK_INTERVAL:-5m}
    volumes:
      - ./data:/data
EOF

# Create data directory
mkdir -p data

# Create Caddyfile (user will need to edit domain)
cat > /etc/caddy/Caddyfile <<'EOF'
# ShareBridge Signaling Server
# EDIT THIS: Replace with your real domains
share.example.com {
    reverse_proxy localhost:8080
}

relay.example.com {
    reverse_proxy localhost:9001
}
EOF

echo -e "${YELLOW}Setting correct permissions...${NC}"
chown -R root:root /opt/sharebridge
chmod 600 .env

# Create systemd service for Docker Compose
cat > /etc/systemd/system/sharebridge-signaling.service <<'EOF'
[Unit]
Description=ShareBridge Signaling Server
After=docker.service network.target
Requires=docker.service

[Service]
Type=oneshot
RemainAfterExit=yes
WorkingDirectory=/opt/sharebridge/signaling-server
Environment="COMPOSE_PROJECT_NAME=sharebridge"
ExecStart=/usr/bin/docker compose up -d
ExecStop=/usr/bin/docker compose down
TimeoutStartSec=0

[Install]
WantedBy=multi-user.target
EOF

# Create health check script
cat > /usr/local/bin/signaling-health.sh <<'EOF'
#!/bin/bash
# Quick health check script
curl -fsS http://127.0.0.1:8080/ >/dev/null || echo "HTTP app check failed"
EOF
chmod +x /usr/local/bin/signaling-health.sh

# Reload systemd
systemctl daemon-reload
systemctl enable sharebridge-signaling

# Create a setup reminder file
cat > /opt/sharebridge/SETUP.md <<EOF
# ShareBridge Signaling Server - Post-Install Steps

## 1. Configure your domains
Edit /etc/caddy/Caddyfile:
   Replace 'share.example.com' and 'relay.example.com' with your actual domains

Reload Caddy:
   systemctl reload caddy

## 2. Configure DNS
Point your domain's A record to this server's IP address.

## 3. Build and start the signaling server
\`\`\`bash
cd /opt/sharebridge/signaling-server

# Build the Docker image (or pull from registry)
# docker build -t sharebridge/signaling-server:latest /path/to/signaling-server

# Start services
systemctl start sharebridge-signaling
\`\`\`

## 4. Verify everything is running
\`\`\`bash
# Check containers
docker compose ps

# Check logs
docker compose logs -f

# Check Caddy (should show HTTPS certificate obtained)
systemctl status caddy

# Test the HTTP app
curl https://your-domain.com/
\`\`\`

## 5. Get your secrets (save these somewhere secure!)
JWT Secret: ${JWT_SECRET}

## 6. Security checklist
- [ ] SSH key-only auth (no passwords)
- [ ] UFW enabled and configured
- [ ] fail2ban running
- [ ] Docker rootless (optional but recommended)
- [ ] Automatic security updates configured
- [ ] Relay DNS record is DNS-only / not proxied through Cloudflare
- [ ] Caddy relay route is not restricted to Cloudflare IP ranges

## Useful commands

View logs:
  docker compose logs -f

Restart services:
  systemctl restart sharebridge-signaling

Check firewall:
  ufw status verbose

Check Caddy:
  systemctl status caddy

## Secrets location
Secrets are in: /opt/sharebridge/signaling-server/.env
Make sure this file is backed up securely!
EOF

echo -e "${GREEN}================================================${NC}"
echo -e "${GREEN}Deployment script completed!${NC}"
echo -e "${GREEN}================================================${NC}"
echo ""
echo -e "${YELLOW}IMPORTANT: Read /opt/sharebridge/SETUP.md for next steps${NC}"
echo ""
echo "Your secrets (SAVE THESE):"
echo "  JWT Secret: ${JWT_SECRET}"
echo ""
echo "Next steps:"
echo "  1. Configure your domains in /etc/caddy/Caddyfile"
echo "  2. Point both share and relay DNS to this server"
echo "  3. Build/deploy your signaling server container"
echo "  4. Start services: systemctl start sharebridge-signaling"
echo ""
echo -e "${GREEN}================================================${NC}"
