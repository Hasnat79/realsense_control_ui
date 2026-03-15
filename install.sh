#!/usr/bin/env bash
# =============================================================================
# install.sh — RealSense Capture Studio: Ubuntu dependency installer
# =============================================================================
# Installs everything needed to build and run the Go server on a fresh Ubuntu
# machine (20.04, 22.04, 24.04).
#
# What this script installs:
#   1. Build essentials (gcc, g++, make, git, curl, wget)
#   2. Go 1.22.x (from official tarball — apt version is usually too old)
#   3. librealsense2 SDK (from Intel's official apt repository)
#   4. OpenCV development headers (libopencv-dev)
#   5. udev rules for RealSense cameras (camera usable without root)
#
# Usage:
#   chmod +x install.sh
#   ./install.sh
#
# After the script completes:
#   source ~/.bashrc          # load Go into current shell
#   cd /path/to/realsense-go
#   make run                  # real hardware
#   make run-demo             # no hardware needed
# =============================================================================

set -euo pipefail

# ── Colours ───────────────────────────────────────────────────────────────────
RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[1;33m'
CYAN='\033[0;36m'; BOLD='\033[1m'; RESET='\033[0m'

info()    { echo -e "${CYAN}[•]${RESET} $*"; }
success() { echo -e "${GREEN}[✓]${RESET} $*"; }
warn()    { echo -e "${YELLOW}[!]${RESET} $*"; }
error()   { echo -e "${RED}[✗]${RESET} $*"; exit 1; }
section() { echo -e "\n${BOLD}${CYAN}═══ $* ${RESET}"; }

# ── Root check ────────────────────────────────────────────────────────────────
if [[ $EUID -eq 0 ]]; then
  error "Do not run this script as root. Run as a normal user — sudo will be called as needed."
fi

# ── Detect Ubuntu version ─────────────────────────────────────────────────────
if ! command -v lsb_release &>/dev/null; then
  error "lsb_release not found. This script requires Ubuntu 20.04, 22.04, or 24.04."
fi

UBUNTU_CODENAME=$(lsb_release -cs)
UBUNTU_VERSION=$(lsb_release -rs)
info "Detected Ubuntu ${UBUNTU_VERSION} (${UBUNTU_CODENAME})"

case "$UBUNTU_CODENAME" in
  focal|jammy|noble) ;;
  *) warn "Ubuntu ${UBUNTU_CODENAME} is untested. Proceeding anyway." ;;
esac

# ── Detect architecture ───────────────────────────────────────────────────────
ARCH=$(uname -m)
case "$ARCH" in
  x86_64)  GO_ARCH="amd64" ;;
  aarch64) GO_ARCH="arm64" ;;
  *) error "Unsupported architecture: $ARCH. Only x86_64 and aarch64 are supported." ;;
esac
info "Architecture: ${ARCH} → Go arch: ${GO_ARCH}"

# =============================================================================
# STEP 1 — Build essentials
# =============================================================================
section "Step 1 — Build essentials"

info "Updating apt package index…"
# sudo apt-get update -qq

info "Installing build tools…"
sudo apt-get install -y -qq \
  build-essential \
  gcc \
  g++ \
  make \
  git \
  curl \
  wget \
  ca-certificates \
  gnupg \
  lsb-release \
  pkg-config \
  software-properties-common

success "Build essentials installed."

# =============================================================================
# STEP 2 — Go
# =============================================================================
section "Step 2 — Go 1.22"

GO_VERSION="1.22.3"
GO_TARBALL="go${GO_VERSION}.linux-${GO_ARCH}.tar.gz"
GO_URL="https://go.dev/dl/${GO_TARBALL}"
GO_INSTALL_DIR="/usr/local"
GO_BIN="${GO_INSTALL_DIR}/go/bin/go"

# Check if a sufficient version is already installed
if command -v go &>/dev/null; then
  CURRENT_GO=$(go version | awk '{print $3}' | sed 's/go//')
  REQUIRED="1.22.0"
  # Simple version comparison (works for x.y.z format)
  if [[ "$(printf '%s\n' "$REQUIRED" "$CURRENT_GO" | sort -V | head -1)" == "$REQUIRED" ]]; then
    success "Go ${CURRENT_GO} already installed and satisfies >= 1.22."
    GO_BIN=$(command -v go)
  else
    warn "Go ${CURRENT_GO} is too old (need >= 1.22). Upgrading…"
    INSTALL_GO=1
  fi
else
  info "Go not found. Installing Go ${GO_VERSION}…"
  INSTALL_GO=1
fi

if [[ "${INSTALL_GO:-0}" == "1" ]]; then
  TMP_DIR=$(mktemp -d)
  info "Downloading ${GO_URL}…"
  wget -q --show-progress -O "${TMP_DIR}/${GO_TARBALL}" "${GO_URL}"

  info "Installing to ${GO_INSTALL_DIR}…"
  sudo rm -rf "${GO_INSTALL_DIR}/go"
  sudo tar -C "${GO_INSTALL_DIR}" -xzf "${TMP_DIR}/${GO_TARBALL}"
  rm -rf "${TMP_DIR}"

  # Add Go to PATH in .bashrc if not already there
  BASHRC="$HOME/.bashrc"
  GO_PATH_LINE='export PATH=$PATH:/usr/local/go/bin'
  if ! grep -qF "$GO_PATH_LINE" "$BASHRC"; then
    echo "" >> "$BASHRC"
    echo "# Go" >> "$BASHRC"
    echo "$GO_PATH_LINE" >> "$BASHRC"
    info "Added Go to PATH in ~/.bashrc"
  fi

  export PATH="$PATH:/usr/local/go/bin"
  success "Go ${GO_VERSION} installed."
fi

# Verify
GO_ACTUAL=$("${GO_BIN:-go}" version 2>/dev/null || /usr/local/go/bin/go version)
success "Go version: ${GO_ACTUAL}"

# =============================================================================
# STEP 3 — librealsense2
# =============================================================================
section "Step 3 — librealsense2 SDK"

# Check if already installed
if dpkg -l librealsense2-dev &>/dev/null 2>&1; then
  RS_VER=$(dpkg -l librealsense2-dev | awk '/librealsense2-dev/{print $3}')
  success "librealsense2-dev already installed (${RS_VER}). Skipping."
else
  info "Adding Intel RealSense apt repository…"

  sudo mkdir -p /etc/apt/keyrings

  # Download and install the signing key
  curl -sSf https://librealsense.intel.com/Debian/librealsense.pgp \
    | sudo tee /etc/apt/keyrings/librealsense.pgp > /dev/null

  # Add the repository
  echo "deb [signed-by=/etc/apt/keyrings/librealsense.pgp] \
https://librealsense.intel.com/Debian/apt-repo \
${UBUNTU_CODENAME} main" \
    | sudo tee /etc/apt/sources.list.d/librealsense.list > /dev/null

  info "Updating apt and installing librealsense2…"
  sudo apt-get update -qq
  sudo apt-get install -y \
    librealsense2-dkms \
    librealsense2-dev \
    librealsense2-utils

  success "librealsense2 installed."
fi

# =============================================================================
# STEP 4 — OpenCV
# =============================================================================
section "Step 4 — OpenCV"

if dpkg -l libopencv-dev &>/dev/null 2>&1; then
  CV_VER=$(dpkg -l libopencv-dev | awk '/libopencv-dev/{print $3}')
  success "libopencv-dev already installed (${CV_VER}). Skipping."
else
  info "Installing OpenCV development libraries…"
  sudo apt-get install -y libopencv-dev
  success "OpenCV installed."
fi

# =============================================================================
# STEP 5 — udev rules for RealSense cameras
# =============================================================================
section "Step 5 — udev rules"

UDEV_FILE="/etc/udev/rules.d/99-realsense-libusb.rules"
RS_UDEV_SRC="/etc/udev/rules.d/99-realsense-libusb.rules"

# librealsense2-dkms installs the rules file automatically
if [[ -f "$UDEV_FILE" ]]; then
  success "udev rules already in place (${UDEV_FILE})."
else
  warn "udev rules file not found. Writing minimal rules for Intel RealSense…"
  echo 'SUBSYSTEMS=="usb", ATTRS{idVendor}=="8086", MODE="0666", GROUP="plugdev"' \
    | sudo tee /etc/udev/rules.d/99-realsense.rules > /dev/null
fi

info "Reloading udev rules…"
sudo udevadm control --reload-rules
sudo udevadm trigger

# Add user to plugdev group
if ! groups "$USER" | grep -q plugdev; then
  info "Adding ${USER} to plugdev group…"
  sudo usermod -aG plugdev "$USER"
  warn "Group change requires a logout/login to take effect."
else
  success "User ${USER} is already in plugdev group."
fi

success "udev rules configured."

# =============================================================================
# STEP 6 — Verify installations
# =============================================================================
section "Step 6 — Verification"

FAIL=0

check() {
  local label="$1"; local cmd="$2"
  if eval "$cmd" &>/dev/null; then
    success "$label"
  else
    warn "FAILED: $label"
    FAIL=1
  fi
}

check "Go binary"             "command -v go || /usr/local/go/bin/go version"
check "gcc"                   "command -v gcc"
check "g++"                   "command -v g++"
check "make"                  "command -v make"
check "librealsense2 headers" "test -f /usr/local/include/librealsense2/rs.h || test -f /usr/include/librealsense2/rs.h"
check "OpenCV headers"        "test -f /usr/include/opencv4/opencv2/imgcodecs.hpp"
check "rs-enumerate-devices"  "command -v rs-enumerate-devices"

if [[ $FAIL -eq 0 ]]; then
  success "All checks passed."
else
  warn "Some checks failed — review the output above."
fi

# =============================================================================
# Summary
# =============================================================================
echo ""
echo -e "${BOLD}${GREEN}══════════════════════════════════════════${RESET}"
echo -e "${BOLD}${GREEN}  Installation complete!${RESET}"
echo -e "${BOLD}${GREEN}══════════════════════════════════════════${RESET}"
echo ""
echo -e "  ${BOLD}Next steps:${RESET}"
echo ""
echo -e "  1. Reload your shell (or open a new terminal):"
echo -e "     ${CYAN}source ~/.bashrc${RESET}"
echo ""
echo -e "  2. Go to the project directory:"
echo -e "     ${CYAN}cd /path/to/realsense-go${RESET}"
echo ""
echo -e "  3. Run without hardware (demo mode):"
echo -e "     ${CYAN}make run-demo${RESET}"
echo ""
echo -e "  4. Run with RealSense cameras:"
echo -e "     ${CYAN}make run${RESET}"
echo ""
echo -e "  5. Open in browser:"
echo -e "     ${CYAN}http://localhost:5050${RESET}"
echo ""

if groups "$USER" | grep -q plugdev; then
  : # already in group, no warning needed
else
  echo -e "  ${YELLOW}⚠ Log out and back in for camera USB permissions to take effect.${RESET}"
  echo ""
fi