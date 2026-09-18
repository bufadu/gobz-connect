#!/usr/bin/env bash
# install.sh — build and install gobz-connect on Debian / Raspberry Pi OS
#
# Usage:
#   sudo bash install.sh [--cec]
#
# Options:
#   --cec   Build with HDMI-CEC support (requires libcec-dev).
#
# What it does:
#   1. Install system build dependencies
#   2. Install Go if absent or outdated
#   3. Clone / update the repository
#   4. Build the binary
#   5. Install the binary to /usr/local/bin/
#   6. Create a 'gobz-connect' system user
#   7. Create /etc/gobz-connect/ with an example config (if absent)
#   8. Create /var/cache/gobz-connect/
#   9. Install and enable the systemd service
#
# Re-running the script performs an in-place upgrade: the service is briefly
# stopped while the binary is replaced, then restarted automatically.

set -euo pipefail

# ── Settings ──────────────────────────────────────────────────────────────────
REPO_URL="https://github.com/bufadu/gobz-connect"
BINARY_NAME="gobz-connect"
BINARY_PATH="/usr/local/bin/${BINARY_NAME}"
CONFIG_DIR="/etc/gobz-connect"
CACHE_DIR="/var/cache/gobz-connect"
SERVICE_USER="gobz-connect"
SERVICE_NAME="gobz-connect"
BUILD_DIR="/tmp/gobz-connect-build"
GO_MIN_VERSION="1.21"
GO_INSTALL_VERSION="1.24.2"

# ── Parse arguments ───────────────────────────────────────────────────────────
BUILD_TAGS=""
for arg in "$@"; do
    case "$arg" in
        --cec) BUILD_TAGS="cec" ;;
        *) echo "Unknown option: $arg" >&2; exit 1 ;;
    esac
done

# ── Helpers ───────────────────────────────────────────────────────────────────
info() { echo "  [+] $*"; }
warn() { echo "  [!] $*" >&2; }
die()  { echo "  [✗] $*" >&2; exit 1; }

require_root() {
    [[ $EUID -eq 0 ]] || die "This script must be run as root (use sudo)."
}

version_ge() {
    printf '%s\n%s\n' "$2" "$1" | sort -V -C
}

# ── 1. System dependencies ────────────────────────────────────────────────────
install_deps() {
    info "Installing system dependencies…"
    apt-get update -qq
    apt-get install -y --no-install-recommends \
        git curl build-essential pkg-config \
        libasound2-dev

    if [[ "${BUILD_TAGS}" == "cec" ]]; then
        info "Installing CEC dependencies…"
        apt-get install -y --no-install-recommends \
            libcec-dev libp8-platform-dev libudev-dev

        # libcec.pc lists p8-platform as a dependency but it was folded into
        # libcec 6+. Create a dummy stub to satisfy pkg-config.
        local pc_dir="/usr/local/lib/pkgconfig"
        if ! pkg-config --exists p8-platform 2>/dev/null; then
            info "Creating p8-platform.pc stub for pkg-config…"
            mkdir -p "${pc_dir}"
            cat > "${pc_dir}/p8-platform.pc" <<'EOF'
Name: p8-platform
Description: p8-platform stub (bundled in libcec 6+)
Version: 2.1.0
Libs:
Cflags:
EOF
        fi
    fi
}

# ── 2. Go toolchain ───────────────────────────────────────────────────────────
install_go() {
    local arch
    case "$(uname -m)" in
        aarch64)       arch="arm64"  ;;
        armv7l|armv6l) arch="armv6l" ;;
        x86_64)        arch="amd64"  ;;
        *) die "Unsupported architecture: $(uname -m)" ;;
    esac

    local tarball="go${GO_INSTALL_VERSION}.linux-${arch}.tar.gz"
    local url="https://go.dev/dl/${tarball}"
    local tmp
    tmp=$(mktemp -d)
    trap 'rm -rf "$tmp"' EXIT

    info "Downloading Go ${GO_INSTALL_VERSION} (${arch})…"
    curl -fsSL "${url}" -o "${tmp}/${tarball}"
    info "Installing Go to /usr/local/go…"
    rm -rf /usr/local/go
    tar -C /usr/local -xzf "${tmp}/${tarball}"
    export PATH="/usr/local/go/bin:${PATH}"
}

ensure_go() {
    export PATH="/usr/local/go/bin:${HOME}/go/bin:${PATH}"
    if command -v go &>/dev/null; then
        local current
        current=$(go version | awk '{print $3}' | sed 's/go//')
        if version_ge "${current}" "${GO_MIN_VERSION}"; then
            info "Found Go ${current} — OK"
            return
        fi
        warn "Go ${current} is too old (need >= ${GO_MIN_VERSION}); upgrading…"
    else
        info "Go not found; installing ${GO_INSTALL_VERSION}…"
    fi
    install_go
}

# ── 3. Clone / update repository ─────────────────────────────────────────────
fetch_source() {
    if [[ -d "${BUILD_DIR}/.git" ]]; then
        info "Updating existing clone in ${BUILD_DIR}…"
        git -C "${BUILD_DIR}" fetch --quiet origin
        git -C "${BUILD_DIR}" reset --hard origin/main
    else
        info "Cloning ${REPO_URL}…"
        rm -rf "${BUILD_DIR}"
        git clone --depth 1 "${REPO_URL}" "${BUILD_DIR}"
    fi
}

# ── 4. Build ──────────────────────────────────────────────────────────────────
build_binary() {
    cd "${BUILD_DIR}"
    local ver
    ver=$(git describe --tags --always --dirty 2>/dev/null || echo "dev")

    local tags_flag=""
    [[ -n "${BUILD_TAGS}" ]] && tags_flag="-tags ${BUILD_TAGS}"

    local label="${ver}"
    [[ "${BUILD_TAGS}" == "cec" ]] && label="${ver} (with CEC)"
    info "Building gobz-connect ${label}…"

    # shellcheck disable=SC2086
    go build ${tags_flag} -trimpath \
        -ldflags="-s -w -X main.version=${ver}" \
        -o gobz-connect \
        ./cmd/gobz-connect/

    info "Build succeeded ($(du -sh gobz-connect | cut -f1))"
}

# ── 5. Install binary ─────────────────────────────────────────────────────────
install_binary() {
    info "Installing binary to ${BINARY_PATH}…"

    if systemctl is-active --quiet "${SERVICE_NAME}" 2>/dev/null; then
        info "Stopping ${SERVICE_NAME} for upgrade…"
        systemctl stop "${SERVICE_NAME}"
    fi

    install -m 755 "${BUILD_DIR}/gobz-connect" "${BINARY_PATH}"
}

# ── 6. System user and directories ───────────────────────────────────────────
setup_user_and_dirs() {
    if ! id -u "${SERVICE_USER}" &>/dev/null; then
        info "Creating system user '${SERVICE_USER}'…"
        useradd --system --no-create-home \
            --home-dir /nonexistent \
            --shell /usr/sbin/nologin \
            --comment "gobz-connect renderer" \
            "${SERVICE_USER}"
    fi

    # audio: ALSA playback  |  video: /dev/vchiq (Raspberry Pi CEC)
    for grp in audio video; do
        if getent group "${grp}" &>/dev/null; then
            usermod -aG "${grp}" "${SERVICE_USER}"
        fi
    done

    info "Creating directories…"
    mkdir -p "${CONFIG_DIR}" "${CACHE_DIR}"
    chown "${SERVICE_USER}:${SERVICE_USER}" "${CACHE_DIR}"
    chmod 750 "${CONFIG_DIR}"   # may contain auth tokens
    chown root:${SERVICE_USER} "${CONFIG_DIR}"

    # Pre-create the secrets file so the service user can write to it.
    # The directory is root-owned (no write for service user), so the file
    # must exist before the service starts — the service can overwrite an
    # existing file it owns, but cannot create a new file in a dir it doesn't
    # own with write permission.
    local secrets="${CONFIG_DIR}/qobuz-secrets.json"
    if [[ ! -f "${secrets}" ]]; then
        touch "${secrets}"
    fi
    chown "${SERVICE_USER}:${SERVICE_USER}" "${secrets}"
    chmod 600 "${secrets}"
}

# ── 7. Example config ─────────────────────────────────────────────────────────
write_example_config() {
    local cfg="${CONFIG_DIR}/config.yaml"
    if [[ -f "${cfg}" ]]; then
        info "Config already exists at ${cfg} — skipping"
        return
    fi

    info "Writing example config to ${cfg}…"
    cat > "${cfg}" <<'EOF'
# gobz-connect configuration — edit before starting the service.
# Full reference: README.md  |  Authentication: AUTHENTICATION.md

# ── Authentication ──────────────────────────────────────────────────────────
# See AUTHENTICATION.md for how to obtain these.
user_id: ""
user_auth_token: ""

unauthenticated_mode: false
# Experimental. Alternative to user_id/user_auth_token: skip local
# authentication entirely and rely on the Qobuz app to supply credentials
# over mDNS (connect-to-qconnect) when it connects to this renderer. Those
# credentials are short-lived and only the app can renew them — nothing
# guarantees it will do so automatically, so long unattended sessions can
# eventually lose the ability to load new tracks until the app reconnects.
# Prefer user_id/user_auth_token above for unattended, long-running use.

# ── Device ──────────────────────────────────────────────────────────────────
device_name: "GobzConnect"
port: 1984

# ── Audio quality ────────────────────────────────────────────────────────────
audio_format: 27   # Requested Qobuz stream format
                   #   5  = MP3
                   #   6  = FLAC CD (16-bit 44.1 kHz)
                   #   7  = Hi-Res 96 kHz
                   #  27  = Hi-Res 192 kHz  ← default here

# ── Audio output ─────────────────────────────────────────────────────────────
speaker_sample_rate: 0
# Output sample rate in Hz (e.g. 44100, 48000, 96000, 192000).
# 0 (default): auto-detect the maximum rate supported by the default ALSA
# device.
#
# With adaptive_sample_rate enabled (the default), this acts as a hardware
# ceiling: the speaker is reinitialised per track at the track's native rate,
# but never above this value.  Useful when the audio device physically does
# not support certain rates (e.g. a USB DAC limited to 96 kHz).
#
# With adaptive_sample_rate disabled, this fixes the speaker at a single rate
# and all tracks that differ are resampled.

adaptive_sample_rate: true
# When true (default), gobz-connect reinitialises the ALSA audio device at
# the native sample rate of each track. Benefits:
#   • No resampling for the common case → zero CPU overhead on a Raspberry Pi.
#   • Every track plays at its true quality (44.1 kHz CD tracks at 44.1 kHz,
#     96 kHz Hi-Res at 96 kHz, etc.).
#
# Trade-off: when two consecutive tracks have different sample rates, the
# device is reinitialised between them, producing a brief silence (~100–200 ms)
# instead of a gapless transition.  Within a single-SR playlist (all CD or
# all Hi-Res) playback remains fully gapless.
#
# Set to false to lock the device at a fixed rate and resample all tracks,
# which may cause choppy audio on a Raspberry Pi when upsampling
# (e.g. 44.1 → 96 kHz).

resample_quality: 4
# Resampler quality when a track's sample rate must be converted.
# With adaptive_sample_rate enabled the resampler is only invoked when
# speaker_sample_rate caps a track's native rate, so this setting rarely
# matters in that mode. With adaptive_sample_rate disabled this is critical
# on low-power devices.
#
# value | trade-off
# ------|-----------
#  1    | fastest, lowest quality — use on very slow hardware
#  4    | good balance (default)
#  6    | higher quality, higher CPU — borderline for on-the-fly use
# >6    | offline/archival use only; too slow for real-time playback

# ── Cache ────────────────────────────────────────────────────────────────────
cache_dir: "/var/cache/gobz-connect"
cache_size_mb: 10240   # Maximum cache size in MB, 0 to disable

background_download_rate_kbps: 4000
# Rate-limit background pre-warm downloads (the next track, fetched ahead of
# when it's needed) in KB/s, to reduce SD-card I/O contention with the track
# currently playing. The currently playing track's own download is never
# throttled. 0 (default) = unlimited.

# ── Credentials cache (auto-generated at startup) ───────────────────────────
secrets_file: "/etc/gobz-connect/qobuz-secrets.json"
# Path to the file where gobz-connect stores the scraped Qobuz app credentials
# (AppID and AppSecret).  Generated automatically on first run.

# ── HDMI-CEC (requires binary built with --cec flag, experimental) ─────────
cec:
  enable: false
  standby_delay: 15     # minutes of inactivity before sending amp to standby
  volume_control: false # route Qobuz volume slider to the amp via CEC
  # Advanced, only needed if amp auto-discovery fails on your adapter
  # (common on the Raspberry Pi's built-in VC4 CEC adapter) — see README.md's
  # HDMI & CEC setup section.
  fallback_log_addr: 0    # 0 = not set; use auto-discovery
  fallback_phys_addr: ""  # e.g. "3.0.0.0"; empty = not set
EOF

    chown "root:${SERVICE_USER}" "${cfg}"
    chmod 640 "${cfg}"
    warn "Edit ${cfg} — fill in user_id and user_auth_token before starting the service."
}

# ── 8. systemd service ────────────────────────────────────────────────────────
install_service() {
    local unit="/etc/systemd/system/${SERVICE_NAME}.service"
    info "Installing systemd unit ${unit}…"

    cat > "${unit}" <<EOF
[Unit]
Description=gobz-connect — Qobuz Connect renderer
Documentation=https://github.com/bufadu/gobz-connect
After=network-online.target sound.target
Wants=network-online.target

[Service]
Type=simple
User=${SERVICE_USER}
Group=${SERVICE_USER}
ExecStart=${BINARY_PATH} -config ${CONFIG_DIR}/config.yaml
Restart=on-failure
RestartSec=10s
TimeoutStopSec=10s

StandardOutput=journal
StandardError=journal
SyslogIdentifier=${SERVICE_NAME}

# Hardening
NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=true
ReadWritePaths=${CACHE_DIR} ${CONFIG_DIR}

[Install]
WantedBy=multi-user.target
EOF

    systemctl daemon-reload
    systemctl enable "${SERVICE_NAME}"
}

# ── Main ──────────────────────────────────────────────────────────────────────
main() {
    require_root

    echo ""
    echo "━━━  gobz-connect installer  ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
    [[ "${BUILD_TAGS}" == "cec" ]] && echo "     CEC support: enabled"
    echo ""

    install_deps
    ensure_go
    fetch_source
    build_binary
    install_binary
    setup_user_and_dirs
    write_example_config
    install_service

    echo ""
    echo "━━━  Done  ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
    echo ""
    echo "  Binary  : ${BINARY_PATH}"
    echo "  Config  : ${CONFIG_DIR}/config.yaml"
    echo "  Cache   : ${CACHE_DIR}"
    echo "  Logs    : journalctl -u ${SERVICE_NAME} -f"
    echo ""
    echo "  Next steps:"
    echo "    1. Edit ${CONFIG_DIR}/config.yaml"
    echo "       (fill in user_id and user_auth_token)"
    echo "    2. sudo systemctl start ${SERVICE_NAME}"
    echo ""
}

main "$@"
