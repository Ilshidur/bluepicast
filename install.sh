#!/bin/bash
set -e

# BluePiCast Installer
# Usage: curl -sSL https://raw.githubusercontent.com/Ilshidur/bluepicast/main/install.sh | sudo bash

REPO="Ilshidur/bluepicast"
INSTALL_DIR="/usr/local/bin"
BLUEPICAST_SERVICE_FILE="/etc/systemd/system/bluepicast.service"
BLUEALSA_SERVICE_FILE="/etc/systemd/system/bluealsa.service"
SNAPCLIENT_SERVICE_FILE="/usr/lib/systemd/user/snapclient.service"
SNAPCLIENT_DEFAULT_FILE="/etc/default/snapclient"

# Versions
BLUEALSA_VERSION="441311552fd0119ac29b6757e0f483dde5f42945"  # Latest commit (post v4.3.1)
SNAPCLIENT_VERSION="v0.34.0"

# Colors
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

echo -e "${GREEN}╔══════════════════════════════════════════════╗${NC}"
echo -e "${GREEN}║         BluePiCast Installer                 ║${NC}"
echo -e "${GREEN}╚══════════════════════════════════════════════╝${NC}"
echo

# Check if running as root
if [ "$EUID" -ne 0 ]; then
    echo -e "${RED}Error: Please run as root (use sudo)${NC}"
    exit 1
fi

# Detect architecture
detect_arch() {
    local arch=$(uname -m)
    case "$arch" in
        aarch64|arm64)
            echo "linux-arm64"
            ;;
        armv7l)
            echo "linux-armv7"
            ;;
        armv6l)
            echo "linux-armv6"
            ;;
        *)
            echo -e "${RED}Unsupported architecture: $arch${NC}"
            exit 1
            ;;
    esac
}

ARCH=$(detect_arch)
echo -e "${YELLOW}Detected architecture: ${ARCH}${NC}"

# Ensure Bluetooth is not soft-blocked via rfkill and warn if disabled in Pi config
ensure_bluetooth_unblocked() {
    if command -v rfkill >/dev/null 2>&1; then
        if rfkill list bluetooth 2>/dev/null | grep -qi "Soft blocked: yes"; then
            echo -e "${YELLOW}Bluetooth is soft-blocked via rfkill. Unblocking...${NC}"
            if ! rfkill unblock bluetooth 2>/dev/null; then
                echo -e "${YELLOW}Warning: Failed to rfkill unblock bluetooth. You may need to run 'sudo rfkill unblock bluetooth' manually.${NC}"
            fi
        fi
    fi

    # Warn if Bluetooth is disabled in Raspberry Pi config
    for cfg in /boot/config.txt /boot/firmware/config.txt; do
        if [ -f "$cfg" ] && grep -Eq "^dtoverlay=disable-bt" "$cfg"; then
            echo -e "${YELLOW}Warning: Bluetooth appears disabled in ${cfg} (dtoverlay=disable-bt).${NC}"
            echo -e "${YELLOW}Please remove or comment this line and reboot to fully enable Bluetooth.${NC}"
        fi
    done
}

ensure_bluetooth_unblocked

# ============================================================================
# DISK SPACE MANAGEMENT
# ============================================================================

# Get available disk space in MB for root filesystem
get_root_disk_space() {
    df -m / | awk 'NR==2 {print $4}'
}

# Check and free disk space if needed
ensure_disk_space() {
    local min_space_mb=${1:-2048}  # Default: need at least 2GB
    local available=$(get_root_disk_space)
    
    echo -e "${YELLOW}Available disk space: ${available}MB${NC}"
    
    # Always clean /tmp to avoid apt corruption issues
    echo -e "${YELLOW}Cleaning /tmp directory...${NC}"
    rm -rf /tmp/* 2>/dev/null || true
    
    if [ "$available" -lt "$min_space_mb" ]; then
        echo -e "${YELLOW}Low disk space detected. Cleaning up...${NC}"
        
        # Clean apt cache
        apt-get clean 2>/dev/null || true
        
        # Remove old apt lists
        rm -rf /var/lib/apt/lists/* 2>/dev/null || true
        mkdir -p /var/lib/apt/lists/partial
        
        # Clean journal logs (keep only 50MB)
        journalctl --vacuum-size=50M 2>/dev/null || true
        
        # Remove old kernels (keep current)
        apt-get autoremove -y 2>/dev/null || true
        
        # Clean thumbnail cache
        rm -rf /home/*/.cache/thumbnails/* 2>/dev/null || true
        
        # Refresh available space
        available=$(get_root_disk_space)
        echo -e "${YELLOW}Available disk space after cleanup: ${available}MB${NC}"
        
        if [ "$available" -lt 500 ]; then
            echo -e "${RED}ERROR: Not enough disk space (${available}MB available, need at least 500MB)${NC}"
            echo -e "${YELLOW}Please free up disk space manually and try again.${NC}"
            echo -e "${YELLOW}Suggestions:${NC}"
            echo -e "  - Remove unused packages: sudo apt autoremove"
            echo -e "  - Clear apt cache: sudo apt clean"
            echo -e "  - Check large files: sudo du -sh /* | sort -h"
            exit 1
        elif [ "$available" -lt "$min_space_mb" ]; then
            echo -e "${YELLOW}Warning: Limited disk space. Installation may use most available space.${NC}"
        fi
    fi
}

# ============================================================================
# MEMORY / SWAP MANAGEMENT (for low-memory devices like Pi Zero 2 W)
# ============================================================================
SWAPFILE="/var/bluepicast_swap"
SWAP_CREATED=0

# Get available memory in MB
get_available_memory() {
    free -m | awk '/^Mem:/{print $7}'
}

# Get available disk space in MB for a given path
get_available_disk_space() {
    df -m "$1" 2>/dev/null | awk 'NR==2 {print $4}'
}

# Create temporary swap if memory is low
ensure_swap() {
    local available_mem=$(get_available_memory)
    echo -e "${YELLOW}Available memory: ${available_mem}MB${NC}"
    
    # If less than 1GB available, create swap
    if [ "$available_mem" -lt 1024 ]; then
        # Check if swap already exists and is active
        if swapon --show | grep -q "$SWAPFILE"; then
            echo -e "${YELLOW}Temporary swap already active${NC}"
            return 0
        fi
        
        # Check available disk space on /var
        local available_disk=$(get_available_disk_space /var)
        
        # Determine swap size based on available disk (leave 500MB free)
        local swap_size=1024
        if [ -n "$available_disk" ] && [ "$available_disk" -lt 1524 ]; then
            swap_size=$((available_disk - 500))
            if [ "$swap_size" -lt 256 ]; then
                echo -e "${YELLOW}Warning: Not enough disk space for swap. Compilation may fail.${NC}"
                echo -e "${YELLOW}Available: ${available_disk}MB, need at least 756MB${NC}"
                return 1
            fi
            echo -e "${YELLOW}Limited disk space. Creating ${swap_size}MB swap instead of 1GB...${NC}"
        fi
        
        echo -e "${YELLOW}Creating temporary ${swap_size}MB swap file...${NC}"
        
        # Remove old swap file if exists
        if [ -f "$SWAPFILE" ]; then
            swapoff "$SWAPFILE" 2>/dev/null || true
            rm -f "$SWAPFILE"
        fi
        
        # Create swap file
        if ! dd if=/dev/zero of="$SWAPFILE" bs=1M count="$swap_size" status=progress 2>&1; then
            echo -e "${RED}Failed to create swap file${NC}"
            rm -f "$SWAPFILE" 2>/dev/null
            return 1
        fi
        
        chmod 600 "$SWAPFILE"
        mkswap "$SWAPFILE"
        swapon "$SWAPFILE"
        SWAP_CREATED=1
        echo -e "${GREEN}Temporary swap enabled (${swap_size}MB)${NC}"
    fi
    return 0
}

# Remove temporary swap
cleanup_swap() {
    if [ "$SWAP_CREATED" -eq 1 ] && [ -f "$SWAPFILE" ]; then
        echo -e "${YELLOW}Removing temporary swap...${NC}"
        swapoff "$SWAPFILE" || true
        rm -f "$SWAPFILE"
        echo -e "${GREEN}Temporary swap removed${NC}"
    fi
}

# Determine safe number of parallel jobs based on available memory
get_make_jobs() {
    local available_mem=$(get_available_memory)
    local total_swap=$(free -m | awk '/^Swap:/{print $2}')
    local total_available=$((available_mem + total_swap))
    
    # Need roughly 512MB per compilation job
    local safe_jobs=$((total_available / 512))
    
    # At least 1 job, at most nproc
    local max_jobs=$(nproc)
    if [ "$safe_jobs" -lt 1 ]; then
        safe_jobs=1
    elif [ "$safe_jobs" -gt "$max_jobs" ]; then
        safe_jobs=$max_jobs
    fi
    
    echo "$safe_jobs"
}

# ============================================================================
# BLUEALSA INSTALLATION
# ============================================================================
install_bluealsa() {
    echo
    echo -e "${GREEN}╔══════════════════════════════════════════════╗${NC}"
    echo -e "${GREEN}║         Installing BlueALSA                  ║${NC}"
    echo -e "${GREEN}╚══════════════════════════════════════════════╝${NC}"
    echo

    # Set locale to avoid perl warnings
    export LC_ALL=C
    export LANG=C

    # Install build dependencies
    echo -e "${YELLOW}Installing build dependencies for bluez-alsa...${NC}"
    apt-get update
    apt-get install -y \
        git \
        build-essential \
        autoconf \
        automake \
        libtool \
        pkg-config \
        libbluetooth-dev \
        libsystemd-dev \
        libasound2-dev \
        libdbus-1-dev \
        libglib2.0-dev \
        libsbc-dev \
        libfdk-aac-dev \
        libldacbt-abr-dev \
        libldacbt-enc-dev \
        libopenaptx-dev \
        libmp3lame-dev \
        libspandsp-dev

    # Add common pkg-config paths for arm64
    export PKG_CONFIG_PATH="/usr/lib/aarch64-linux-gnu/pkgconfig:/usr/lib/arm-linux-gnueabihf/pkgconfig:/usr/lib/pkgconfig:/usr/share/pkgconfig:$PKG_CONFIG_PATH"

    # Create symlink for systemd.pc since bluez-alsa v4.2.0 looks for systemd not libsystemd
    SYSTEMD_PC="/usr/lib/aarch64-linux-gnu/pkgconfig/systemd.pc"
    if [ ! -f "$SYSTEMD_PC" ] && [ -f "/usr/lib/aarch64-linux-gnu/pkgconfig/libsystemd.pc" ]; then
        echo -e "${YELLOW}Creating symlink for systemd.pc...${NC}"
        ln -s /usr/lib/aarch64-linux-gnu/pkgconfig/libsystemd.pc "$SYSTEMD_PC" || true
    fi

    # Check for libsystemd
    pkg-config --exists --print-errors "libsystemd >= 200" || {
        echo -e "${RED}ERROR: libsystemd development package not found${NC}"
        exit 1
    }

    # Clone and build bluez-alsa
    cd /tmp
    rm -rf bluez-alsa
    git clone https://github.com/arkq/bluez-alsa.git
    cd bluez-alsa
    git checkout ${BLUEALSA_VERSION}

    autoreconf --install --force

    # Set systemd environment variables for configure since pkg-config name mismatch
    export SYSTEMD_CFLAGS="$(pkg-config --cflags libsystemd)"
    export SYSTEMD_LIBS="$(pkg-config --libs libsystemd)"

    ./configure \
        --prefix=/usr \
        --enable-aac \
        --enable-aptx \
        --with-libopenaptx \
        --enable-msbc \
        --enable-systemd

    # Use safe number of parallel jobs to avoid OOM
    local jobs=$(get_make_jobs)
    echo -e "${YELLOW}Compiling with ${jobs} parallel job(s)...${NC}"
    make -j${jobs}
    make install

    # Clean up
    cd /tmp
    rm -rf bluez-alsa

    echo -e "${GREEN}BlueALSA installed successfully${NC}"
}

# Create BlueALSA systemd service
create_bluealsa_service() {
    echo -e "${YELLOW}Creating BlueALSA systemd service...${NC}"
    cat > "$BLUEALSA_SERVICE_FILE" << 'EOF'
[Unit]
Description=BlueALSA service
After=bluetooth.target
Requires=bluetooth.target

[Service]
Type=simple
ExecStart=/usr/bin/bluealsad -S -p a2dp-source -p a2dp-sink --all-codecs --keep-alive 10
Restart=on-failure
RestartSec=5

[Install]
WantedBy=multi-user.target
EOF

    systemctl daemon-reload
    systemctl enable bluealsa.service
    systemctl start bluealsa.service || true
    echo -e "${GREEN}BlueALSA service created and enabled${NC}"
}

# Check if BlueALSA is installed
check_bluealsa() {
    # Check for bluealsad (the daemon executable)
    if command -v bluealsad &> /dev/null; then
        echo -e "${GREEN}BlueALSA daemon (bluealsad) is already installed${NC}"
        # Also check if service exists and is enabled
        if systemctl is-enabled bluealsa.service &> /dev/null; then
            echo -e "${GREEN}BlueALSA service is enabled${NC}"
            return 0
        else
            echo -e "${YELLOW}BlueALSA installed but service not enabled, will create service${NC}"
            return 1
        fi
    fi
    return 1
}

# ============================================================================
# SNAPCLIENT INSTALLATION
# ============================================================================

# Detect Snapcast architecture suffix for download
get_snapcast_arch() {
    local arch=$(uname -m)
    case "$arch" in
        aarch64|arm64)
            echo "arm64"
            ;;
        armv7l)
            echo "armhf"
            ;;
        *)
            echo ""
            ;;
    esac
}

install_snapclient() {
    echo
    echo -e "${GREEN}╔══════════════════════════════════════════════╗${NC}"
    echo -e "${GREEN}║         Installing Snapclient                ║${NC}"
    echo -e "${GREEN}╚══════════════════════════════════════════════╝${NC}"
    echo

    local snap_arch=$(get_snapcast_arch)
    
    if [ -z "$snap_arch" ]; then
        echo -e "${RED}Unsupported architecture for Snapclient pre-built package${NC}"
        exit 1
    fi

    # Update package lists
    echo -e "${YELLOW}Updating package lists...${NC}"
    apt-get update

    # Install essential dependencies that we know we need
    echo -e "${YELLOW}Installing base dependencies...${NC}"
    apt-get install -y alsa-utils avahi-daemon || true

    # Install the actual library package FIRST (before creating symlinks)
    # On Trixie it's libflac-dev, on Bookworm it's libflac12
    echo -e "${YELLOW}Installing FLAC library...${NC}"
    apt-get install -y libflac-dev 2>/dev/null || apt-get install -y libflac12t64 2>/dev/null || apt-get install -y libflac12 2>/dev/null || true

    # Workaround for Debian Trixie: Snapcast expects libflac12 but Trixie has libflac14 (libFLAC.so.14)
    # Create compatibility symlinks if needed
    echo -e "${YELLOW}Setting up library compatibility for Debian Trixie...${NC}"
    
    # For arm64
    if [ ! -e /usr/lib/aarch64-linux-gnu/libFLAC.so.12 ]; then
        # Try to find any libFLAC.so.*.*.* and create symlink
        local flac_lib=$(ls /usr/lib/aarch64-linux-gnu/libFLAC.so.*.*.* 2>/dev/null | head -1)
        if [ -z "$flac_lib" ]; then
            # Try pattern libFLAC.so.[0-9]*
            flac_lib=$(ls /usr/lib/aarch64-linux-gnu/libFLAC.so.[0-9]* 2>/dev/null | grep -v ".so.12" | head -1)
        fi
        if [ -n "$flac_lib" ] && [ -e "$flac_lib" ]; then
            echo -e "${YELLOW}Creating libFLAC.so.12 symlink -> ${flac_lib}${NC}"
            ln -sf "$flac_lib" /usr/lib/aarch64-linux-gnu/libFLAC.so.12
        else
            echo -e "${RED}Warning: Could not find libFLAC library to symlink${NC}"
            ls -la /usr/lib/aarch64-linux-gnu/libFLAC* 2>/dev/null || echo "No libFLAC files found"
        fi
    else
        echo -e "${GREEN}libFLAC.so.12 already exists${NC}"
    fi
    
    # For armhf
    if [ ! -e /usr/lib/arm-linux-gnueabihf/libFLAC.so.12 ]; then
        local flac_lib=$(ls /usr/lib/arm-linux-gnueabihf/libFLAC.so.*.*.* 2>/dev/null | head -1)
        if [ -z "$flac_lib" ]; then
            flac_lib=$(ls /usr/lib/arm-linux-gnueabihf/libFLAC.so.[0-9]* 2>/dev/null | grep -v ".so.12" | head -1)
        fi
        if [ -n "$flac_lib" ] && [ -e "$flac_lib" ]; then
            echo -e "${YELLOW}Creating libFLAC.so.12 symlink -> ${flac_lib}${NC}"
            ln -sf "$flac_lib" /usr/lib/arm-linux-gnueabihf/libFLAC.so.12
        fi
    fi
    
    # Update library cache
    ldconfig

    # Create a fake libflac12 package entry to satisfy dpkg
    # This is needed because dpkg checks package names, not just library files
    echo -e "${YELLOW}Creating compatibility package entry...${NC}"
    if ! dpkg -s libflac12 &>/dev/null; then
        mkdir -p /tmp/libflac12-compat/DEBIAN
        cat > /tmp/libflac12-compat/DEBIAN/control << 'CTRL'
Package: libflac12
Version: 1.4.3-1~compat
Architecture: all
Maintainer: BluePiCast Installer
Description: Compatibility package for libflac12 (provided by libflac12t64)
 This is a dummy package to satisfy Snapclient dependencies on Debian Trixie.
Provides: libflac12
CTRL
        dpkg-deb --build /tmp/libflac12-compat /tmp/libflac12-compat.deb
        dpkg -i /tmp/libflac12-compat.deb
        rm -rf /tmp/libflac12-compat /tmp/libflac12-compat.deb
    fi

    # Download pre-built .deb package from GitHub releases
    # Version without 'v' prefix for download URL
    local version_number="${SNAPCLIENT_VERSION#v}"
    local deb_url="https://github.com/badaix/snapcast/releases/download/${SNAPCLIENT_VERSION}/snapclient_${version_number}-1_${snap_arch}_bookworm.deb"
    local temp_deb=$(mktemp --suffix=.deb)

    echo -e "${YELLOW}Downloading Snapclient ${SNAPCLIENT_VERSION} for ${snap_arch}...${NC}"
    echo -e "${YELLOW}URL: ${deb_url}${NC}"
    
    if ! curl -sSL -o "$temp_deb" "$deb_url"; then
        # Try without bookworm suffix
        deb_url="https://github.com/badaix/snapcast/releases/download/${SNAPCLIENT_VERSION}/snapclient_${version_number}-1_${snap_arch}.deb"
        echo -e "${YELLOW}Trying alternate URL: ${deb_url}${NC}"
        if ! curl -sSL -o "$temp_deb" "$deb_url"; then
            echo -e "${RED}Failed to download Snapclient package${NC}"
            rm -f "$temp_deb"
            exit 1
        fi
    fi

    # Install the .deb package
    echo -e "${YELLOW}Installing Snapclient package...${NC}"
    dpkg -i "$temp_deb"
    
    # Fix any missing dependencies automatically
    echo -e "${YELLOW}Resolving dependencies...${NC}"
    apt-get install -f -y

    # Clean up
    rm -f "$temp_deb"

    # Disable the system snapclient service (we use user service)
    systemctl stop snapclient.service 2>/dev/null || true
    systemctl disable snapclient.service 2>/dev/null || true

    # Mask snapserver to prevent it from starting
    systemctl mask snapserver.service 2>/dev/null || true

    echo -e "${GREEN}Snapclient installed successfully${NC}"
}

# Create Snapclient systemd user service
create_snapclient_service() {
    echo -e "${YELLOW}Creating Snapclient systemd user service...${NC}"

    # Create user service directory
    mkdir -p /usr/lib/systemd/user

    cat > "$SNAPCLIENT_SERVICE_FILE" << 'EOF'
[Unit]
Description=Snapcast client
Documentation=man:snapclient(1)
After=network-online.target sound.target
Wants=network-online.target

[Service]
EnvironmentFile=-/etc/default/snapclient
ExecStart=/usr/bin/snapclient $SNAPCLIENT_OPTS
Restart=on-failure
RestartSec=3

[Install]
WantedBy=default.target
EOF

    # Create default configuration
    mkdir -p /etc/default
    cat > "$SNAPCLIENT_DEFAULT_FILE" << 'EOF'
# Snapclient configuration
# This file is managed by BluePiCast
SNAPCLIENT_OPTS="--hostID bluepicast"
EOF

    # Enable avahi-daemon (required for Snapclient)
    systemctl enable avahi-daemon.service || true
    systemctl start avahi-daemon.service || true

    echo -e "${GREEN}Snapclient service created${NC}"
    echo -e "${YELLOW}Note: Snapclient runs as a user service. Enable with: systemctl --user enable snapclient${NC}"
}

# Check if Snapclient is installed
check_snapclient() {
    if command -v snapclient &> /dev/null; then
        echo -e "${GREEN}Snapclient is already installed${NC}"
        return 0
    fi
    return 1
}

# ============================================================================
# BLUEPICAST INSTALLATION
# ============================================================================
install_bluepicast() {
    echo
    echo -e "${GREEN}╔══════════════════════════════════════════════╗${NC}"
    echo -e "${GREEN}║         Installing BluePiCast                ║${NC}"
    echo -e "${GREEN}╚══════════════════════════════════════════════╝${NC}"
    echo

    # Get latest release version
    echo -e "${YELLOW}Fetching latest release...${NC}"
    API_RESPONSE=$(curl -sSL "https://api.github.com/repos/${REPO}/releases/latest" 2>&1)
    CURL_EXIT_CODE=$?

    # Check if curl failed
    if [ $CURL_EXIT_CODE -ne 0 ]; then
        echo -e "${RED}Error: Failed to connect to GitHub API${NC}"
        echo -e "${YELLOW}Please check your internet connection.${NC}"
        exit 1
    fi

    # Extract tag_name from response
    LATEST_RELEASE=$(echo "$API_RESPONSE" | grep '"tag_name":' | sed -E 's/.*"([^"]+)".*/\1/')

    if [ -z "$LATEST_RELEASE" ]; then
        # Check if response contains an error message
        if echo "$API_RESPONSE" | grep -q '"message":'; then
            ERROR_MSG=$(echo "$API_RESPONSE" | grep '"message":' | sed -E 's/.*"message": *"([^"]+)".*/\1/')
            echo -e "${RED}Error: $ERROR_MSG${NC}"
        else
            echo -e "${RED}Error: Could not fetch latest release${NC}"
            echo -e "${YELLOW}Make sure the repository has at least one release.${NC}"
        fi
        exit 1
    fi

    echo -e "${GREEN}Latest version: ${LATEST_RELEASE}${NC}"

    # Download binary
    DOWNLOAD_URL="https://github.com/${REPO}/releases/download/${LATEST_RELEASE}/bluepicast-${ARCH}"
    TEMP_FILE=$(mktemp)

    echo -e "${YELLOW}Downloading from: ${DOWNLOAD_URL}${NC}"
    curl -sSL -o "$TEMP_FILE" "$DOWNLOAD_URL"

    # Make executable and install
    chmod +x "$TEMP_FILE"
    mv "$TEMP_FILE" "${INSTALL_DIR}/bluepicast"

    echo -e "${GREEN}BluePiCast binary installed to ${INSTALL_DIR}/bluepicast${NC}"
}

# Create BluePiCast systemd service
create_bluepicast_service() {
    echo -e "${YELLOW}Creating BluePiCast systemd service...${NC}"
    cat > "$BLUEPICAST_SERVICE_FILE" << 'EOF'
[Unit]
Description=BluePiCast - Bluetooth Manager and Audio Streamer
Documentation=https://github.com/Ilshidur/raspberry-bluetooth-web-ui
After=network-online.target bluetooth.target bluealsa.service
Wants=network-online.target
Requires=bluetooth.target bluealsa.service

[Service]
Type=simple
ExecStart=/usr/local/bin/bluepicast --port 8080 --enable-systemd-snapclient
Restart=on-failure
RestartSec=5
User=root

[Install]
WantedBy=multi-user.target
EOF

    systemctl daemon-reload
    systemctl enable bluepicast
    echo -e "${GREEN}BluePiCast service created and enabled${NC}"
}

# ============================================================================
# MAIN INSTALLATION FLOW
# ============================================================================

# Check and free disk space first
ensure_disk_space 2048

# Ensure we have enough memory for compilation
ensure_swap

# Step 1: BlueALSA
echo -e "${YELLOW}Checking BlueALSA installation...${NC}"
if ! check_bluealsa; then
    install_bluealsa
    create_bluealsa_service
else
    # Ensure service is started
    systemctl start bluealsa.service || true
fi

# Step 2: Snapclient
echo -e "${YELLOW}Checking Snapclient installation...${NC}"
if ! check_snapclient; then
    install_snapclient
    create_snapclient_service
else
    # Ensure service file exists
    if [ ! -f "$SNAPCLIENT_SERVICE_FILE" ]; then
        create_snapclient_service
    fi
fi

# Step 3: BluePiCast
install_bluepicast
create_bluepicast_service

# Cleanup temporary swap if we created one
cleanup_swap

# ============================================================================
# START SERVICES
# ============================================================================
echo
echo -e "${YELLOW}Starting services...${NC}"

# Start BlueALSA
echo -e "${YELLOW}Starting BlueALSA...${NC}"
systemctl start bluealsa.service || echo -e "${RED}Failed to start BlueALSA${NC}"

# Start BluePiCast
echo -e "${YELLOW}Starting BluePiCast...${NC}"
systemctl start bluepicast.service || echo -e "${RED}Failed to start BluePiCast${NC}"

# Snapclient: Create user service file and config directory
# The actual enable/start will be done via the web UI after configuration
if [ -n "$SUDO_USER" ] && [ "$SUDO_USER" != "root" ]; then
    REAL_USER="$SUDO_USER"
elif [ -n "$USER" ] && [ "$USER" != "root" ]; then
    REAL_USER="$USER"
else
    # Fallback: find the first non-root user with a home directory
    REAL_USER=$(getent passwd | awk -F: '$3 >= 1000 && $3 < 65534 {print $1; exit}')
fi

if [ -n "$REAL_USER" ]; then
    REAL_USER_HOME=$(getent passwd "$REAL_USER" | cut -d: -f6)
    echo -e "${YELLOW}Setting up Snapclient user service for user: ${REAL_USER}${NC}"
    
    # Enable lingering so user services can start at boot
    loginctl enable-linger "$REAL_USER" 2>/dev/null || true
    
    # Create user systemd directory
    SYSTEMD_USER_DIR="${REAL_USER_HOME}/.config/systemd/user"
    sudo -u "$REAL_USER" mkdir -p "$SYSTEMD_USER_DIR"
    
    # Create user service file
    cat > "${SYSTEMD_USER_DIR}/snapclient.service" << 'SVCEOF'
[Unit]
Description=Snapcast client (user)
Documentation=man:snapclient(1)
Wants=network-online.target
After=network-online.target sound.target

[Service]
EnvironmentFile=-%h/.config/snapclient/options
ExecStart=/usr/bin/snapclient --logsink=system $SNAPCLIENT_OPTS
Restart=on-failure

[Install]
WantedBy=default.target
SVCEOF
    chown "$REAL_USER:$REAL_USER" "${SYSTEMD_USER_DIR}/snapclient.service"
    
    # Create snapclient config directory
    SNAPCLIENT_CONFIG_DIR="${REAL_USER_HOME}/.config/snapclient"
    sudo -u "$REAL_USER" mkdir -p "$SNAPCLIENT_CONFIG_DIR"
    
    echo -e "${GREEN}Snapclient user service files created${NC}"
    echo -e "${YELLOW}Configure Snapclient via the web UI and enable the service there${NC}"
else
    echo -e "${YELLOW}Note: Snapclient is a user service. Configure it via the web UI.${NC}"
fi

# Final summary
echo
echo -e "${GREEN}╔══════════════════════════════════════════════╗${NC}"
echo -e "${GREEN}║  Installation complete!                      ║${NC}"
echo -e "${GREEN}╚══════════════════════════════════════════════╝${NC}"
echo
echo -e "Installed components:"
echo -e "  - BlueALSA ${BLUEALSA_VERSION}"
echo -e "  - Snapclient ${SNAPCLIENT_VERSION}"
echo -e "  - BluePiCast (latest)"
echo
echo -e "Service status:"
systemctl is-active bluealsa.service &>/dev/null && echo -e "  - BlueALSA: ${GREEN}running${NC}" || echo -e "  - BlueALSA: ${RED}not running${NC}"
systemctl is-active bluepicast.service &>/dev/null && echo -e "  - BluePiCast: ${GREEN}running${NC}" || echo -e "  - BluePiCast: ${RED}not running${NC}"
echo -e "  - Snapclient: ${YELLOW}configure via web UI, then enable${NC}"
echo
echo -e "Access the web interface at:"
echo -e "  ${YELLOW}http://<your-raspberry-pi-ip>:8080${NC}"
echo
echo -e "${YELLOW}Next steps:${NC}"
echo -e "  1. Open the web interface"
echo -e "  2. Configure Snapclient (set server host, etc.)"
echo -e "  3. Click 'Enable User Service' to start Snapclient"
echo
