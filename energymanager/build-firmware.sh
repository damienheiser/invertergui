#!/bin/bash
# Build and package Energy Manager for GL-SFT1200
# Creates a deployment package for stock GL.iNet firmware

set -e

VERSION="1.0.0"
OUTPUT_DIR="dist"
DEPLOY_DIR="${OUTPUT_DIR}/deploy"
PACKAGE_NAME="energymanager-${VERSION}-gl-sft1200"

echo "=== GL-SFT1200 Energy Manager Builder ==="
echo ""

# Build energymanager binary if needed
if [ ! -f "${OUTPUT_DIR}/energymanager-mipsle" ]; then
    echo "Building energymanager binary..."
    mkdir -p ${OUTPUT_DIR}
    CGO_ENABLED=0 GOOS=linux GOARCH=mipsle GOMIPS=softfloat \
        go build -ldflags="-s -w" -o ${OUTPUT_DIR}/energymanager-mipsle ./cmd/energymanager
fi

echo "Creating deployment package..."

# Clean and create deploy directory
rm -rf ${DEPLOY_DIR}
mkdir -p ${DEPLOY_DIR}/usr/bin
mkdir -p ${DEPLOY_DIR}/etc/init.d
mkdir -p ${DEPLOY_DIR}/etc/config
mkdir -p ${DEPLOY_DIR}/www

# Copy energymanager binary
cp ${OUTPUT_DIR}/energymanager-mipsle ${DEPLOY_DIR}/usr/bin/energymanager
chmod +x ${DEPLOY_DIR}/usr/bin/energymanager

# Copy init script
cp openwrt/files/energymanager.init ${DEPLOY_DIR}/etc/init.d/energymanager
chmod +x ${DEPLOY_DIR}/etc/init.d/energymanager

# Copy configuration files
cp openwrt/files/etc/config/energymanager ${DEPLOY_DIR}/etc/config/
cp openwrt/files/www/index.html ${DEPLOY_DIR}/www/

# Install luci-app-zerotier if available
LUCI_ZT_DIR="../luci-app-zerotier"
if [ -d "${LUCI_ZT_DIR}" ]; then
    echo "Including luci-app-zerotier..."

    mkdir -p ${DEPLOY_DIR}/usr/lib/lua/luci/controller
    mkdir -p ${DEPLOY_DIR}/usr/lib/lua/luci/model/cbi/zerotier
    mkdir -p ${DEPLOY_DIR}/usr/lib/lua/luci/view/zerotier
    mkdir -p ${DEPLOY_DIR}/usr/share/rpcd/acl.d
    mkdir -p ${DEPLOY_DIR}/etc/uci-defaults

    cp ${LUCI_ZT_DIR}/luasrc/controller/zerotier.lua ${DEPLOY_DIR}/usr/lib/lua/luci/controller/
    cp ${LUCI_ZT_DIR}/luasrc/model/cbi/zerotier/*.lua ${DEPLOY_DIR}/usr/lib/lua/luci/model/cbi/zerotier/
    cp ${LUCI_ZT_DIR}/luasrc/view/zerotier/*.htm ${DEPLOY_DIR}/usr/lib/lua/luci/view/zerotier/
    cp ${LUCI_ZT_DIR}/root/etc/config/zerotier ${DEPLOY_DIR}/etc/config/
    cp ${LUCI_ZT_DIR}/root/etc/init.d/zerotier ${DEPLOY_DIR}/etc/init.d/
    chmod +x ${DEPLOY_DIR}/etc/init.d/zerotier
    cp ${LUCI_ZT_DIR}/root/etc/uci-defaults/luci-zerotier ${DEPLOY_DIR}/etc/uci-defaults/
    chmod +x ${DEPLOY_DIR}/etc/uci-defaults/luci-zerotier
    cp ${LUCI_ZT_DIR}/root/usr/share/rpcd/acl.d/luci-app-zerotier.json ${DEPLOY_DIR}/usr/share/rpcd/acl.d/
fi

# Create install script
cat > ${DEPLOY_DIR}/install.sh << 'INSTALL_EOF'
#!/bin/sh
# Energy Manager Installer for GL-SFT1200

INSTALL_DIR="$(cd "$(dirname "$0")" && pwd)"

echo "Installing Energy Manager..."
echo ""

# Install packages - try local packages first, fallback to online
install_package() {
    PKG_NAME="$1"
    PKG_FILE=$(ls ${INSTALL_DIR}/packages/${PKG_NAME}_*.ipk 2>/dev/null | head -1)

    if [ -f "$PKG_FILE" ]; then
        echo "Installing $PKG_NAME from local package..."
        opkg install "$PKG_FILE"
    else
        echo "Installing $PKG_NAME from repository..."
        opkg install "$PKG_NAME"
    fi
}

# Try to update package lists (non-fatal if offline)
echo "Updating package lists..."
opkg update 2>/dev/null || echo "  (offline mode - using local packages)"
echo ""

# Install packages that are likely already present
opkg install kmod-usb-serial kmod-usb-acm kmod-tun 2>/dev/null || true

# Install packages from local files if available
if [ -d "${INSTALL_DIR}/packages" ]; then
    echo "Installing from local packages..."
    # Install dependencies first
    for dep in libminiupnpc libnatpmp; do
        PKG_FILE=$(ls ${INSTALL_DIR}/packages/${dep}_*.ipk 2>/dev/null | head -1)
        [ -f "$PKG_FILE" ] && opkg install "$PKG_FILE" 2>/dev/null
    done
    # Install main packages
    install_package "kmod-usb-serial-ftdi"
    install_package "zerotier"
else
    # Fallback to online installation
    opkg install kmod-usb-serial-ftdi zerotier 2>/dev/null || \
        echo "Warning: Could not install optional packages (no network)"
fi

echo ""
echo "Copying files..."

# Copy files
cp -r ${INSTALL_DIR}/usr/* /usr/
cp -r ${INSTALL_DIR}/etc/* /etc/
cp -r ${INSTALL_DIR}/www/* /www/ 2>/dev/null || true

# Configure nginx to use our landing page instead of GL.iNet's SPA
echo ""
echo "Configuring nginx..."
if [ -f /etc/nginx/conf.d/gl.conf ]; then
    # Change index order to prioritize index.html
    sed -i 's/index gl_home.html;/index index.html gl_home.html;/' /etc/nginx/conf.d/gl.conf
    # Remove the rewrite rule that redirects index.html to / (causes loop)
    sed -i '/rewrite.*index.html.*permanent/d' /etc/nginx/conf.d/gl.conf
    # Restart nginx to apply changes
    /etc/init.d/nginx restart 2>/dev/null || true
fi

# Run UCI defaults
if [ -d /etc/uci-defaults ]; then
    for script in /etc/uci-defaults/*; do
        [ -x "$script" ] && "$script" && rm "$script"
    done
fi

echo ""
echo "Enabling services..."

# Enable services
/etc/init.d/energymanager enable
/etc/init.d/zerotier enable 2>/dev/null || true

# Start services
/etc/init.d/energymanager start
/etc/init.d/zerotier start 2>/dev/null || true

echo ""
echo "============================================"
echo "Installation complete!"
echo "============================================"
echo ""
echo "Access points:"
LAN_IP=$(uci get network.lan.ipaddr 2>/dev/null || echo "192.168.8.1")
echo "  - Landing Page: http://${LAN_IP}/"
echo "  - Energy Dashboard: http://${LAN_IP}:8080/"
echo "  - ZeroTier: http://${LAN_IP}/cgi-bin/luci/admin/vpn/zerotier/"
echo ""
echo "Configure Shelly EM3 at: http://${LAN_IP}:8080/config"
INSTALL_EOF
chmod +x ${DEPLOY_DIR}/install.sh

# Include offline packages if available
if [ -d "packages" ] && [ "$(ls -A packages/*.ipk 2>/dev/null)" ]; then
    echo "Including offline packages..."
    mkdir -p ${DEPLOY_DIR}/packages
    cp packages/*.ipk ${DEPLOY_DIR}/packages/
fi

# Create tarball
cd ${OUTPUT_DIR}
tar -czvf ${PACKAGE_NAME}.tar.gz -C deploy .
cd ..

# Show results
echo ""
echo "=== Build Complete ==="
echo ""
echo "Package: ${OUTPUT_DIR}/${PACKAGE_NAME}.tar.gz"
ls -lh ${OUTPUT_DIR}/${PACKAGE_NAME}.tar.gz
echo ""
echo "Binary size:"
ls -lh ${OUTPUT_DIR}/energymanager-mipsle
echo ""
echo "=== Installation Instructions ==="
echo ""
echo "1. Copy to router:"
echo "   scp ${OUTPUT_DIR}/${PACKAGE_NAME}.tar.gz root@<router-ip>:/tmp/"
echo ""
echo "2. SSH to router and install:"
echo "   ssh root@<router-ip>"
echo "   cd /tmp && tar -xzf ${PACKAGE_NAME}.tar.gz"
echo "   ./install.sh"
echo ""
echo "3. (Optional) Configure network settings manually:"
echo "   uci set network.lan.ipaddr='192.168.254.254'"
echo "   uci set dhcp.lan.start='200'"
echo "   uci set dhcp.lan.limit='21'"
echo "   uci commit"
echo "   /etc/init.d/network restart"
echo ""
echo "=== Alternative: Docker-based Firmware Build ==="
echo ""
echo "To build a complete firmware image, use the GL.iNet Docker builder:"
echo ""
echo "1. Clone the imagebuilder:"
echo "   git clone https://github.com/gl-inet/imagebuilder.git"
echo "   cd imagebuilder"
echo ""
echo "2. Build Docker image:"
echo "   docker build --rm -t gl_imagebuilder - < Dockerfile"
echo ""
echo "3. Run build in Docker:"
echo "   docker run -v \$(pwd):/src gl_imagebuilder python /src/gl_image -p sft1200"
echo ""
echo "See: https://github.com/gl-inet/imagebuilder"
