#!/bin/bash
# Build Energy Manager for multiple OpenWRT/GL.iNet router architectures
# Creates release packages for common router platforms

set -e

VERSION="${1:-1.1.0}"
OUTPUT_DIR="dist"

echo "=== Energy Manager Release Builder v${VERSION} ==="
echo ""

# Define target architectures
# Format: "name:GOOS:GOARCH:GOMIPS"
declare -a TARGETS=(
    # GL.iNet routers
    "gl-sft1200:linux:mipsle:softfloat"    # GL-SFT1200 (Siflower SF19A28)
    "gl-mt300n-v2:linux:mipsle:softfloat"  # GL-MT300N-V2 (MediaTek MT7628AN)
    "gl-ar750:linux:mips:softfloat"        # GL-AR750 (Qualcomm QCA9531)
    "gl-ar750s:linux:mips:softfloat"       # GL-AR750S Slate (Qualcomm QCA9563)
    "gl-b1300:linux:arm:7"                 # GL-B1300 (Qualcomm IPQ4028)
    "gl-ax1800:linux:arm64:"               # GL-AX1800 Flint (Qualcomm IPQ6000)
    "gl-mt3000:linux:arm64:"               # GL-MT3000 Beryl AX (MediaTek MT7981)
    "gl-mt6000:linux:arm64:"               # GL-MT6000 Flint 2 (MediaTek MT7986)

    # Generic OpenWRT targets
    "openwrt-mipsle:linux:mipsle:softfloat"  # Generic MIPS little-endian
    "openwrt-mips:linux:mips:softfloat"      # Generic MIPS big-endian
    "openwrt-arm:linux:arm:7"                # Generic ARM (32-bit)
    "openwrt-arm64:linux:arm64:"             # Generic ARM64 (64-bit)
    "openwrt-x86:linux:386:"                 # x86 (32-bit)
    "openwrt-x86_64:linux:amd64:"            # x86_64 (64-bit)
)

mkdir -p ${OUTPUT_DIR}

# Build for each target
for target in "${TARGETS[@]}"; do
    IFS=':' read -r name goos goarch gomips <<< "$target"

    echo "Building for ${name}..."

    BINARY_NAME="${OUTPUT_DIR}/energymanager-${name}"

    export CGO_ENABLED=0
    export GOOS=${goos}
    export GOARCH=${goarch}

    if [ -n "$gomips" ]; then
        export GOMIPS=${gomips}
    else
        unset GOMIPS
    fi

    if [ "$goarch" = "arm" ]; then
        export GOARM=${gomips}
        unset GOMIPS
    fi

    go build -ldflags="-s -w -X main.Version=${VERSION}" -o "${BINARY_NAME}" ./cmd/energymanager

    # Show size
    SIZE=$(ls -lh "${BINARY_NAME}" | awk '{print $5}')
    echo "  -> ${BINARY_NAME} (${SIZE})"
done

echo ""
echo "=== Creating Release Packages ==="
echo ""

# Create release directory structure
RELEASE_DIR="${OUTPUT_DIR}/release-${VERSION}"
rm -rf ${RELEASE_DIR}
mkdir -p ${RELEASE_DIR}

# Copy common files
cp openwrt/files/energymanager.init ${RELEASE_DIR}/
cp openwrt/files/etc/config/energymanager ${RELEASE_DIR}/energymanager.config
cp README.md ${RELEASE_DIR}/

# Create a tarball for each architecture
for target in "${TARGETS[@]}"; do
    IFS=':' read -r name goos goarch gomips <<< "$target"

    BINARY_NAME="${OUTPUT_DIR}/energymanager-${name}"
    PACKAGE_DIR="${RELEASE_DIR}/energymanager-${VERSION}-${name}"

    mkdir -p ${PACKAGE_DIR}

    # Copy binary
    cp ${BINARY_NAME} ${PACKAGE_DIR}/energymanager
    chmod +x ${PACKAGE_DIR}/energymanager

    # Copy common files
    cp ${RELEASE_DIR}/energymanager.init ${PACKAGE_DIR}/
    cp ${RELEASE_DIR}/energymanager.config ${PACKAGE_DIR}/

    # Create quick install script
    cat > ${PACKAGE_DIR}/install.sh << 'EOF'
#!/bin/sh
# Quick install script for Energy Manager

echo "Installing Energy Manager..."

# Stop existing service
/etc/init.d/energymanager stop 2>/dev/null || true

# Install files
cp energymanager /usr/bin/
chmod +x /usr/bin/energymanager

cp energymanager.init /etc/init.d/energymanager
chmod +x /etc/init.d/energymanager

# Only copy config if not exists (preserve existing config)
[ ! -f /etc/config/energymanager ] && cp energymanager.config /etc/config/energymanager

# Enable and start service
/etc/init.d/energymanager enable
/etc/init.d/energymanager start

echo ""
echo "Installation complete!"
echo "Access dashboard at: http://$(uci get network.lan.ipaddr 2>/dev/null || echo 'router-ip'):8081/"
echo "Config login: admin / energy"
EOF
    chmod +x ${PACKAGE_DIR}/install.sh

    # Create tarball
    (cd ${RELEASE_DIR} && tar -czvf energymanager-${VERSION}-${name}.tar.gz energymanager-${VERSION}-${name})

    # Clean up directory
    rm -rf ${PACKAGE_DIR}

    echo "Created: energymanager-${VERSION}-${name}.tar.gz"
done

# Clean up temp files
rm -f ${RELEASE_DIR}/energymanager.init ${RELEASE_DIR}/energymanager.config ${RELEASE_DIR}/README.md

# Create checksums
echo ""
echo "Creating checksums..."
(cd ${RELEASE_DIR} && sha256sum *.tar.gz > SHA256SUMS)

echo ""
echo "=== Release Build Complete ==="
echo ""
echo "Release packages in: ${RELEASE_DIR}/"
ls -lh ${RELEASE_DIR}/*.tar.gz
echo ""
echo "SHA256 checksums:"
cat ${RELEASE_DIR}/SHA256SUMS
echo ""
echo "=== Supported Routers ==="
echo ""
echo "GL.iNet Routers:"
echo "  - GL-SFT1200 (Opal) - MIPS"
echo "  - GL-MT300N-V2 - MIPS"
echo "  - GL-AR750, GL-AR750S - MIPS"
echo "  - GL-B1300 (Convexa-B) - ARM"
echo "  - GL-AX1800 (Flint) - ARM64"
echo "  - GL-MT3000 (Beryl AX) - ARM64"
echo "  - GL-MT6000 (Flint 2) - ARM64"
echo ""
echo "Generic OpenWRT:"
echo "  - MIPS (big-endian and little-endian)"
echo "  - ARM (32-bit and 64-bit)"
echo "  - x86 (32-bit and 64-bit)"
