#!/bin/bash
# Build complete OpenWRT firmware for GL-SFT1200 using Docker
# This handles the Python 2 dependency by running in a container

set -e

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
IMAGE_NAME="energymanager-builder"
OUTPUT_DIR="${SCRIPT_DIR}/firmware"

echo "=== GL-SFT1200 Energy Manager Firmware Builder (Docker) ==="
echo ""

# Check if Docker is available
if ! command -v docker &> /dev/null; then
    echo "ERROR: Docker is not installed or not in PATH"
    echo ""
    echo "Please install Docker:"
    echo "  - macOS: brew install --cask docker"
    echo "  - Linux: curl -fsSL https://get.docker.com | sh"
    echo "  - Windows: https://docs.docker.com/desktop/install/windows-install/"
    exit 1
fi

# Check if Docker daemon is running
if ! docker info &> /dev/null; then
    echo "ERROR: Docker daemon is not running"
    echo "Please start Docker Desktop or the docker service"
    exit 1
fi

# Build Docker image if needed
echo "Checking Docker image..."
if ! docker image inspect ${IMAGE_NAME} &> /dev/null; then
    echo "Building Docker image (this may take a few minutes)..."
    docker build -t ${IMAGE_NAME} -f docker/Dockerfile docker/
fi

# Create output directory
mkdir -p ${OUTPUT_DIR}

# Run the build
echo "Starting firmware build..."
echo ""

docker run --rm \
    -v "${SCRIPT_DIR}:/src" \
    -v "${OUTPUT_DIR}:/output" \
    ${IMAGE_NAME}

echo ""
echo "Firmware files saved to: ${OUTPUT_DIR}/"
ls -lh ${OUTPUT_DIR}/

echo ""
echo "To flash the firmware:"
echo ""
echo "Via Web Interface:"
echo "  1. Go to http://<router-ip>/cgi-bin/luci/admin/system/flash"
echo "  2. Upload the *sysupgrade* file"
echo ""
echo "Via Command Line:"
echo "  scp ${OUTPUT_DIR}/*sysupgrade* root@<router-ip>:/tmp/"
echo "  ssh root@<router-ip> 'sysupgrade -n /tmp/*sysupgrade*'"
