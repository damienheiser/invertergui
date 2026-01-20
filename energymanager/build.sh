#!/bin/bash
# Build script for Energy Manager
# Cross-compiles for GL.iNet GL-SFT1200 (Opal) - MIPS Siflower

set -e

VERSION="1.0.0"
OUTPUT_DIR="dist"
BINARY_NAME="energymanager"

# GL-SFT1200 uses Siflower SF19A28 which is MIPS32 little-endian
# GOOS=linux GOARCH=mips GOMIPS=softfloat (soft float for smaller binary)

echo "Building Energy Manager v${VERSION}"

mkdir -p ${OUTPUT_DIR}

# Native build (for testing)
echo "Building native binary..."
CGO_ENABLED=0 go build -ldflags="-s -w" -o ${OUTPUT_DIR}/${BINARY_NAME}-native ./cmd/energymanager

# Cross-compile for GL-SFT1200 (MIPS little-endian, soft float)
echo "Building for GL-SFT1200 (MIPS)..."
CGO_ENABLED=0 GOOS=linux GOARCH=mipsle GOMIPS=softfloat \
    go build -ldflags="-s -w" -o ${OUTPUT_DIR}/${BINARY_NAME}-mipsle ./cmd/energymanager

# Try to compress with upx if available
if command -v upx &> /dev/null; then
    echo "Compressing with UPX..."
    upx --best ${OUTPUT_DIR}/${BINARY_NAME}-mipsle || true
fi

# Create deployment package
echo "Creating deployment package..."
DEPLOY_DIR="${OUTPUT_DIR}/deploy"
mkdir -p ${DEPLOY_DIR}/etc/init.d
mkdir -p ${DEPLOY_DIR}/etc/config
mkdir -p ${DEPLOY_DIR}/usr/bin

cp ${OUTPUT_DIR}/${BINARY_NAME}-mipsle ${DEPLOY_DIR}/usr/bin/${BINARY_NAME}
cp openwrt/files/energymanager.init ${DEPLOY_DIR}/etc/init.d/energymanager
cp openwrt/files/energymanager.config ${DEPLOY_DIR}/etc/config/energymanager
chmod +x ${DEPLOY_DIR}/etc/init.d/energymanager

# Create tarball
cd ${OUTPUT_DIR}
tar -czvf energymanager-${VERSION}-gl-sft1200.tar.gz -C deploy .
cd ..

echo ""
echo "Build complete!"
echo ""
echo "Files:"
ls -lh ${OUTPUT_DIR}/${BINARY_NAME}-*
ls -lh ${OUTPUT_DIR}/*.tar.gz
echo ""
echo "To deploy to GL-SFT1200:"
echo "  1. scp ${OUTPUT_DIR}/energymanager-${VERSION}-gl-sft1200.tar.gz root@<router-ip>:/tmp/"
echo "  2. ssh root@<router-ip>"
echo "  3. cd / && tar -xzvf /tmp/energymanager-${VERSION}-gl-sft1200.tar.gz"
echo "  4. Edit /etc/config/energymanager with your Shelly IP"
echo "  5. /etc/init.d/energymanager enable"
echo "  6. /etc/init.d/energymanager start"
echo ""
# Port 8081 to avoid conflict with GL.iNet admin panel on 8080
echo "Web UI will be available at http://<router-ip>:8081"
