#!/bin/bash
set -e

# BluePiCast Uninstaller
# Usage: curl -sSL https://raw.githubusercontent.com/Ilshidur/bluepicast/main/uninstall.sh | sudo bash
# Or: sudo ./uninstall.sh

# Colors
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

echo -e "${RED}╔══════════════════════════════════════════════╗${NC}"
echo -e "${RED}║         BluePiCast Uninstaller               ║${NC}"
echo -e "${RED}╚══════════════════════════════════════════════╝${NC}"
echo

# Check if running as root
if [ "$EUID" -ne 0 ]; then
    echo -e "${RED}Error: Please run as root (use sudo)${NC}"
    exit 1
fi

# Confirm uninstall
echo -e "${YELLOW}This will remove BluePiCast, BlueALSA, and Snapclient services and configurations.${NC}"
echo -e "${YELLOW}Bluetooth and ALSA base packages will NOT be removed.${NC}"
echo
read -p "Are you sure you want to uninstall? (y/N) " -n 1 -r
echo
if [[ ! $REPLY =~ ^[Yy]$ ]]; then
    echo "Uninstall cancelled."
    exit 0
fi

# Determine the actual user (not root)
if [ -n "$SUDO_USER" ] && [ "$SUDO_USER" != "root" ]; then
    REAL_USER="$SUDO_USER"
else
    REAL_USER=$(getent passwd | awk -F: '$3 >= 1000 && $3 < 65534 {print $1; exit}')
fi

if [ -n "$REAL_USER" ]; then
    REAL_USER_HOME=$(getent passwd "$REAL_USER" | cut -d: -f6)
    REAL_USER_ID=$(id -u "$REAL_USER")
fi

echo
echo -e "${YELLOW}Stopping services...${NC}"

# ============================================================================
# STOP AND DISABLE BLUEPICAST
# ============================================================================
echo -e "${YELLOW}Removing BluePiCast...${NC}"

if systemctl is-active bluepicast.service &>/dev/null; then
    systemctl stop bluepicast.service
    echo -e "  Stopped bluepicast service"
fi

if systemctl is-enabled bluepicast.service &>/dev/null; then
    systemctl disable bluepicast.service
    echo -e "  Disabled bluepicast service"
fi

rm -f /etc/systemd/system/bluepicast.service
rm -f /usr/local/bin/bluepicast
echo -e "${GREEN}BluePiCast removed${NC}"

# ============================================================================
# STOP AND DISABLE SNAPCLIENT
# ============================================================================
echo -e "${YELLOW}Removing Snapclient...${NC}"

# Stop system service if running
if systemctl is-active snapclient.service &>/dev/null; then
    systemctl stop snapclient.service
    echo -e "  Stopped snapclient system service"
fi

if systemctl is-enabled snapclient.service &>/dev/null; then
    systemctl disable snapclient.service
    echo -e "  Disabled snapclient system service"
fi

# Stop user service if running
if [ -n "$REAL_USER" ]; then
    # Try to stop user service
    sudo -u "$REAL_USER" XDG_RUNTIME_DIR="/run/user/${REAL_USER_ID}" \
        systemctl --user stop snapclient.service 2>/dev/null || true
    sudo -u "$REAL_USER" XDG_RUNTIME_DIR="/run/user/${REAL_USER_ID}" \
        systemctl --user disable snapclient.service 2>/dev/null || true
    echo -e "  Stopped snapclient user service"
    
    # Remove user service files
    rm -f "${REAL_USER_HOME}/.config/systemd/user/snapclient.service"
    rm -rf "${REAL_USER_HOME}/.config/snapclient"
    echo -e "  Removed user config files"
fi

# Remove system-wide service file
rm -f /usr/lib/systemd/user/snapclient.service
rm -f /etc/default/snapclient

# Uninstall snapclient package
if dpkg -s snapclient &>/dev/null; then
    apt-get remove -y snapclient 2>/dev/null || true
    echo -e "  Uninstalled snapclient package"
fi

# Remove fake libflac12 package
if dpkg -s libflac12 &>/dev/null; then
    dpkg --purge libflac12 2>/dev/null || true
fi

# Unmask snapserver if it was masked
systemctl unmask snapserver.service 2>/dev/null || true

echo -e "${GREEN}Snapclient removed${NC}"

# ============================================================================
# STOP AND DISABLE BLUEALSA
# ============================================================================
echo -e "${YELLOW}Removing BlueALSA...${NC}"

if systemctl is-active bluealsa.service &>/dev/null; then
    systemctl stop bluealsa.service
    echo -e "  Stopped bluealsa service"
fi

if systemctl is-enabled bluealsa.service &>/dev/null; then
    systemctl disable bluealsa.service
    echo -e "  Disabled bluealsa service"
fi

rm -f /etc/systemd/system/bluealsa.service

# Remove BlueALSA binaries (installed to /usr/bin by make install)
rm -f /usr/bin/bluealsad
rm -f /usr/bin/bluealsa-aplay
rm -f /usr/bin/bluealsa-cli
rm -f /usr/bin/bluealsa-rfcomm

# Remove BlueALSA libraries
rm -f /usr/lib/alsa-lib/libasound_module_ctl_bluealsa.so
rm -f /usr/lib/alsa-lib/libasound_module_pcm_bluealsa.so
rm -rf /usr/lib/aarch64-linux-gnu/alsa-lib/libasound_module_*bluealsa*
rm -rf /usr/lib/arm-linux-gnueabihf/alsa-lib/libasound_module_*bluealsa*

# Remove BlueALSA config
rm -f /etc/dbus-1/system.d/bluealsa.conf
rm -rf /usr/share/alsa/alsa.conf.d/*bluealsa*

# Remove ALSA user config if it was set up for bluealsa
if [ -n "$REAL_USER_HOME" ] && [ -f "${REAL_USER_HOME}/.asoundrc" ]; then
    if grep -q "bluealsa" "${REAL_USER_HOME}/.asoundrc" 2>/dev/null; then
        rm -f "${REAL_USER_HOME}/.asoundrc"
        echo -e "  Removed .asoundrc"
    fi
fi

echo -e "${GREEN}BlueALSA removed${NC}"

# ============================================================================
# CLEANUP
# ============================================================================
echo -e "${YELLOW}Cleaning up...${NC}"

# Reload systemd
systemctl daemon-reload

# Clean up any leftover symlinks
rm -f /usr/lib/aarch64-linux-gnu/pkgconfig/systemd.pc 2>/dev/null || true

# Remove libFLAC symlinks created for compatibility
rm -f /usr/lib/aarch64-linux-gnu/libFLAC.so.12 2>/dev/null || true
rm -f /usr/lib/arm-linux-gnueabihf/libFLAC.so.12 2>/dev/null || true

# Update ldconfig
ldconfig 2>/dev/null || true

echo -e "${GREEN}Cleanup complete${NC}"

# ============================================================================
# SUMMARY
# ============================================================================
echo
echo -e "${GREEN}╔══════════════════════════════════════════════╗${NC}"
echo -e "${GREEN}║  Uninstall complete!                         ║${NC}"
echo -e "${GREEN}╚══════════════════════════════════════════════╝${NC}"
echo
echo -e "The following have been removed:"
echo -e "  - BluePiCast binary and service"
echo -e "  - Snapclient package and services"
echo -e "  - BlueALSA binaries and service"
echo -e "  - Configuration files"
echo
echo -e "${YELLOW}Note: Base packages (bluez, alsa-utils, avahi-daemon) were NOT removed.${NC}"
echo -e "${YELLOW}To remove them: sudo apt remove bluez alsa-utils avahi-daemon${NC}"
echo
