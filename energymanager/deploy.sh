#!/bin/bash
# Deploy Energy Manager to GL-SFT1200 router
# Usage: ./deploy.sh [router-ip] [router-password]
#
# This script will:
# 1. Build for GL-SFT1200 (MIPS softfloat)
# 2. Create deployment package
# 3. SCP everything to the router
# 4. Run the installer remotely via SSH

set -e

# Configuration
ROUTER_IP="${1:-192.168.8.1}"
ROUTER_PASS="${2:-}"
ROUTER_USER="root"
VERSION="1.1.0"
DEPLOY_DIR="dist/deploy"
SSH_OPTS="-o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o HostKeyAlgorithms=+ssh-rsa -o PubkeyAcceptedKeyTypes=+ssh-rsa"

echo "============================================"
echo "Energy Manager Deploy Script"
echo "============================================"
echo ""
echo "Target: ${ROUTER_USER}@${ROUTER_IP}"
echo ""

# Check for sshpass if password provided
if [ -n "$ROUTER_PASS" ]; then
    if ! command -v sshpass &> /dev/null; then
        echo "sshpass not found. Install with: brew install hudochenkov/sshpass/sshpass"
        echo "Or use SSH keys for authentication."
        exit 1
    fi
    SSH_CMD="sshpass -p '$ROUTER_PASS' ssh $SSH_OPTS"
    SCP_CMD="sshpass -p '$ROUTER_PASS' scp $SSH_OPTS"
else
    SSH_CMD="ssh $SSH_OPTS"
    SCP_CMD="scp $SSH_OPTS"
fi

# Step 1: Build for GL-SFT1200
echo "=== Step 1: Building for GL-SFT1200 (MIPS softfloat) ==="
echo ""

export CGO_ENABLED=0
export GOOS=linux
export GOARCH=mipsle
export GOMIPS=softfloat

BINARY="${DEPLOY_DIR}/usr/bin/energymanager"
mkdir -p "${DEPLOY_DIR}/usr/bin"
mkdir -p "${DEPLOY_DIR}/etc/init.d"
mkdir -p "${DEPLOY_DIR}/etc/config"
mkdir -p "${DEPLOY_DIR}/www"

echo "Compiling..."
go build -ldflags="-s -w -X main.Version=${VERSION}" -o "${BINARY}" ./cmd/energymanager

SIZE=$(ls -lh "${BINARY}" | awk '{print $5}')
echo "  Binary: ${BINARY} (${SIZE})"
echo ""

# Step 2: Copy support files
echo "=== Step 2: Preparing deployment package ==="
echo ""

cp openwrt/files/energymanager.init "${DEPLOY_DIR}/etc/init.d/energymanager"
chmod +x "${DEPLOY_DIR}/etc/init.d/energymanager"

cp openwrt/files/energymanager-watchdog "${DEPLOY_DIR}/usr/bin/"
chmod +x "${DEPLOY_DIR}/usr/bin/energymanager-watchdog"

# Copy config only if it doesn't exist locally (preserve template)
cp openwrt/files/etc/config/energymanager "${DEPLOY_DIR}/etc/config/energymanager.default"

# Copy landing page
cp openwrt/files/www/index.html "${DEPLOY_DIR}/www/"

# Create remote install script
cat > "${DEPLOY_DIR}/remote-install.sh" << 'INSTALLSCRIPT'
#!/bin/sh
# Remote installer - runs on the router

set -e

DEPLOY_DIR="/tmp/energymanager-deploy"

echo ""
echo "=== Running remote installer on router ==="
echo ""

# Create /var/lock if missing (needed for opkg)
[ ! -d /var/lock ] && mkdir -p /var/lock

# Stop existing service
echo "Stopping existing service..."
/etc/init.d/energymanager stop 2>/dev/null || true
sleep 1

# Install binary
echo "Installing binary..."
cp ${DEPLOY_DIR}/usr/bin/energymanager /usr/bin/
chmod +x /usr/bin/energymanager

# Install init script
echo "Installing init script..."
cp ${DEPLOY_DIR}/etc/init.d/energymanager /etc/init.d/
chmod +x /etc/init.d/energymanager

# Install watchdog
echo "Installing watchdog..."
cp ${DEPLOY_DIR}/usr/bin/energymanager-watchdog /usr/bin/
chmod +x /usr/bin/energymanager-watchdog

# Install config (only if not exists - preserve existing config)
if [ ! -f /etc/config/energymanager ]; then
    echo "Installing default config..."
    cp ${DEPLOY_DIR}/etc/config/energymanager.default /etc/config/energymanager
else
    echo "Preserving existing config..."
fi

# Install landing page
echo "Installing landing page..."
cp ${DEPLOY_DIR}/www/index.html /www/ 2>/dev/null || true

# Setup watchdog cron job (every minute)
echo "Setting up watchdog cron..."
CRON_JOB="* * * * * /usr/bin/energymanager-watchdog"
if ! grep -q "energymanager-watchdog" /etc/crontabs/root 2>/dev/null; then
    echo "$CRON_JOB" >> /etc/crontabs/root
    /etc/init.d/cron restart 2>/dev/null || true
fi

# Configure nginx for our landing page
if [ -f /etc/nginx/conf.d/gl.conf ]; then
    echo "Configuring nginx..."
    sed -i 's/index gl_home.html;/index index.html gl_home.html;/' /etc/nginx/conf.d/gl.conf 2>/dev/null || true
    sed -i '/rewrite.*index.html.*permanent/d' /etc/nginx/conf.d/gl.conf 2>/dev/null || true
    /etc/init.d/nginx restart 2>/dev/null || true
fi

# Enable and start service
echo "Starting service..."
/etc/init.d/energymanager enable
/etc/init.d/energymanager start

# Verify
sleep 2
if /etc/init.d/energymanager status >/dev/null 2>&1; then
    echo ""
    echo "✓ Service started successfully!"
else
    echo ""
    echo "⚠ Service may not be running. Check with: /etc/init.d/energymanager status"
fi

# Cleanup
rm -rf ${DEPLOY_DIR}

echo ""
echo "============================================"
echo "Deployment complete!"
echo "============================================"
LAN_IP=$(uci get network.lan.ipaddr 2>/dev/null || echo "192.168.8.1")
echo ""
echo "Access points:"
echo "  - Landing Page:     http://${LAN_IP}/"
echo "  - Energy Dashboard: http://${LAN_IP}:8081/"
echo "  - Config:           http://${LAN_IP}:8081/config"
echo ""
INSTALLSCRIPT

chmod +x "${DEPLOY_DIR}/remote-install.sh"

echo "Package contents:"
find "${DEPLOY_DIR}" -type f | head -20
echo ""

# Step 3: Upload to router (using scp -O for legacy protocol - no sftp-server needed)
echo "=== Step 3: Uploading to router ==="
echo ""

echo "Creating remote directory..."
eval "$SSH_CMD ${ROUTER_USER}@${ROUTER_IP} 'rm -rf /tmp/energymanager-deploy; mkdir -p /tmp/energymanager-deploy'"

echo "Uploading files..."
eval "$SCP_CMD -O -r ${DEPLOY_DIR}/* ${ROUTER_USER}@${ROUTER_IP}:/tmp/energymanager-deploy/"

echo "Upload complete."
echo ""

# Step 4: Run remote installer
echo "=== Step 4: Running installer on router ==="
eval "$SSH_CMD ${ROUTER_USER}@${ROUTER_IP} 'chmod +x /tmp/energymanager-deploy/remote-install.sh && /tmp/energymanager-deploy/remote-install.sh'"

echo ""
echo "============================================"
echo "Deployment finished!"
echo "============================================"
