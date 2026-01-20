#!/bin/bash
# Docker entrypoint for building GL-SFT1200 firmware with Energy Manager

DEVICE="sft1200"
SRC_DIR="/src"
BUILD_DIR="/build"
OUTPUT_DIR="/output"

echo "=== GL-SFT1200 Energy Manager Firmware Builder (Docker) ==="
echo ""

# Check if source is mounted
if [ ! -d "$SRC_DIR" ]; then
    echo "ERROR: Source directory not mounted. Use: docker run -v \$(pwd):/src ..."
    exit 1
fi

# Build energymanager binary
echo "Building energymanager for MIPS..."
cd $SRC_DIR
CGO_ENABLED=0 GOOS=linux GOARCH=mipsle GOMIPS=softfloat \
    go build -ldflags="-s -w" -o /tmp/energymanager ./cmd/energymanager || {
    echo "ERROR: Failed to build energymanager binary"
    exit 1
}

echo "Binary built successfully: $(ls -lh /tmp/energymanager)"

# Create files directory for imagebuilder
echo "Preparing custom files..."
FILES_DIR="${BUILD_DIR}/imagebuilder/files"
mkdir -p ${FILES_DIR}/usr/bin
mkdir -p ${FILES_DIR}/etc/init.d
mkdir -p ${FILES_DIR}/etc/config
mkdir -p ${FILES_DIR}/www

# Copy energymanager binary
cp /tmp/energymanager ${FILES_DIR}/usr/bin/
chmod +x ${FILES_DIR}/usr/bin/energymanager

# Copy OpenWRT files from source
cp ${SRC_DIR}/openwrt/files/energymanager.init ${FILES_DIR}/etc/init.d/energymanager
chmod +x ${FILES_DIR}/etc/init.d/energymanager
cp ${SRC_DIR}/openwrt/files/etc/config/energymanager ${FILES_DIR}/etc/config/
cp ${SRC_DIR}/openwrt/files/etc/config/network ${FILES_DIR}/etc/config/ 2>/dev/null || true
cp ${SRC_DIR}/openwrt/files/etc/config/dhcp ${FILES_DIR}/etc/config/ 2>/dev/null || true
cp ${SRC_DIR}/openwrt/files/etc/config/uhttpd ${FILES_DIR}/etc/config/ 2>/dev/null || true
cp ${SRC_DIR}/openwrt/files/www/index.html ${FILES_DIR}/www/ 2>/dev/null || true

# Copy luci-app-zerotier if present
LUCI_ZT_DIR="${SRC_DIR}/../luci-app-zerotier"
if [ -d "${LUCI_ZT_DIR}" ]; then
    echo "Including luci-app-zerotier..."
    mkdir -p ${FILES_DIR}/usr/lib/lua/luci/controller
    mkdir -p ${FILES_DIR}/usr/lib/lua/luci/model/cbi/zerotier
    mkdir -p ${FILES_DIR}/usr/lib/lua/luci/view/zerotier
    mkdir -p ${FILES_DIR}/usr/share/rpcd/acl.d
    mkdir -p ${FILES_DIR}/etc/uci-defaults

    cp ${LUCI_ZT_DIR}/luasrc/controller/zerotier.lua ${FILES_DIR}/usr/lib/lua/luci/controller/ 2>/dev/null || true
    cp ${LUCI_ZT_DIR}/luasrc/model/cbi/zerotier/*.lua ${FILES_DIR}/usr/lib/lua/luci/model/cbi/zerotier/ 2>/dev/null || true
    cp ${LUCI_ZT_DIR}/luasrc/view/zerotier/*.htm ${FILES_DIR}/usr/lib/lua/luci/view/zerotier/ 2>/dev/null || true
    cp ${LUCI_ZT_DIR}/root/etc/config/zerotier ${FILES_DIR}/etc/config/ 2>/dev/null || true
    cp ${LUCI_ZT_DIR}/root/etc/init.d/zerotier ${FILES_DIR}/etc/init.d/ 2>/dev/null || true
    chmod +x ${FILES_DIR}/etc/init.d/zerotier 2>/dev/null || true
    cp ${LUCI_ZT_DIR}/root/etc/uci-defaults/luci-zerotier ${FILES_DIR}/etc/uci-defaults/ 2>/dev/null || true
    chmod +x ${FILES_DIR}/etc/uci-defaults/luci-zerotier 2>/dev/null || true
    cp ${LUCI_ZT_DIR}/root/usr/share/rpcd/acl.d/luci-app-zerotier.json ${FILES_DIR}/usr/share/rpcd/acl.d/ 2>/dev/null || true
fi

# Initialize GL.iNet imagebuilder
echo "Initializing GL.iNet imagebuilder..."
cd ${BUILD_DIR}/imagebuilder

# List available profiles
echo "Available profiles:"
python ./gl_image -l 2>/dev/null || echo "(Could not list profiles)"

# Build firmware using GL.iNet imagebuilder
echo ""
echo "Building firmware image..."

# Use minimal extra packages - GL-SFT1200 has limited space
# The base GL.iNet image already includes LuCI and essential packages
EXTRA_PACKAGES="kmod-usb-serial kmod-usb-serial-ftdi kmod-usb-acm"

echo "Extra packages: ${EXTRA_PACKAGES}"
echo ""

# Run GL.iNet image builder (don't fail on error - we'll check output)
if python ./gl_image -p ${DEVICE} -e "${EXTRA_PACKAGES}" -f files; then
    echo "Build completed successfully"
else
    echo ""
    echo "WARNING: GL.iNet imagebuilder failed."
    echo "This is often due to package availability issues."
    echo ""
    echo "Trying minimal build without extra packages..."
    python ./gl_image -p ${DEVICE} -f files || {
        echo ""
        echo "ERROR: Minimal build also failed."
        echo "The GL.iNet imagebuilder may need to be updated or the device profile may be unavailable."
        echo ""
        echo "Alternative: Use the deployment package instead:"
        echo "  ./build-firmware.sh creates dist/energymanager-*.tar.gz"
        echo "  This can be installed on an existing GL-SFT1200 with stock firmware."
    }
fi

# Copy output
echo ""
echo "Copying output files..."
mkdir -p ${OUTPUT_DIR}
find ${BUILD_DIR}/imagebuilder/bin -name "*${DEVICE}*" -type f -exec cp {} ${OUTPUT_DIR}/ \; 2>/dev/null || true
find ${BUILD_DIR}/imagebuilder/bin -name "*.bin" -type f -exec cp {} ${OUTPUT_DIR}/ \; 2>/dev/null || true

echo ""
echo "=== Build Complete ==="
echo ""
echo "Firmware files in ${OUTPUT_DIR}:"
ls -lh ${OUTPUT_DIR}/

echo ""
echo "Default Configuration:"
echo "  - Router IP: 192.168.254.254"
echo "  - DHCP Range: 192.168.254.200-220"
echo "  - WAN: DHCP client"
echo "  - Auto-discover Shelly Pro EM3"
echo "  - PID mode enabled by default"
echo ""
echo "Access Points:"
echo "  - Landing Page: http://192.168.254.254/"
echo "  - Energy Dashboard: http://192.168.254.254:8080/"
echo "  - LuCI: http://192.168.254.254/cgi-bin/luci/"
echo "  - ZeroTier: http://192.168.254.254/cgi-bin/luci/admin/vpn/zerotier/"
