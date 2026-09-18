#!/usr/bin/env bash
# uninstall.sh — remove gobz-connect from the system
#
# Usage:
#   sudo bash uninstall.sh [--purge]
#
# Options:
#   --purge   Also delete /etc/gobz-connect/ (config + secrets) and
#             /var/cache/gobz-connect/ (track cache).
#             Without this flag those directories are left intact.

set -euo pipefail

BINARY_PATH="/usr/local/bin/gobz-connect"
CONFIG_DIR="/etc/gobz-connect"
CACHE_DIR="/var/cache/gobz-connect"
SERVICE_USER="gobz-connect"
SERVICE_NAME="gobz-connect"
SYSTEMD_UNIT="/etc/systemd/system/${SERVICE_NAME}.service"

# ── Parse arguments ───────────────────────────────────────────────────────────
PURGE=false
for arg in "$@"; do
    case "$arg" in
        --purge) PURGE=true ;;
        *) echo "Unknown option: $arg" >&2; exit 1 ;;
    esac
done

# ── Helpers ───────────────────────────────────────────────────────────────────
info() { echo "  [+] $*"; }
warn() { echo "  [!] $*"; }
die()  { echo "  [✗] $*" >&2; exit 1; }

require_root() {
    [[ $EUID -eq 0 ]] || die "This script must be run as root (use sudo)."
}

# ── Main ──────────────────────────────────────────────────────────────────────
main() {
    require_root

    echo ""
    echo "━━━  gobz-connect uninstaller  ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
    ${PURGE} && echo "     --purge: config and cache will be deleted"
    echo ""

    # 1. Stop and disable the systemd service
    if systemctl is-active --quiet "${SERVICE_NAME}" 2>/dev/null; then
        info "Stopping service ${SERVICE_NAME}…"
        systemctl stop "${SERVICE_NAME}"
    fi
    if systemctl is-enabled --quiet "${SERVICE_NAME}" 2>/dev/null; then
        info "Disabling service ${SERVICE_NAME}…"
        systemctl disable "${SERVICE_NAME}"
    fi

    # 2. Remove systemd unit
    if [[ -f "${SYSTEMD_UNIT}" ]]; then
        info "Removing ${SYSTEMD_UNIT}…"
        rm -f "${SYSTEMD_UNIT}"
        systemctl daemon-reload
    fi

    # 3. Remove binary
    if [[ -f "${BINARY_PATH}" ]]; then
        info "Removing ${BINARY_PATH}…"
        rm -f "${BINARY_PATH}"
    fi

    # 4. Remove system user
    if id -u "${SERVICE_USER}" &>/dev/null; then
        info "Removing system user '${SERVICE_USER}'…"
        userdel "${SERVICE_USER}"
    fi

    # 5. Optionally remove config and cache
    if ${PURGE}; then
        if [[ -d "${CONFIG_DIR}" ]]; then
            info "Removing ${CONFIG_DIR}…"
            rm -rf "${CONFIG_DIR}"
        fi
        if [[ -d "${CACHE_DIR}" ]]; then
            info "Removing ${CACHE_DIR}…"
            rm -rf "${CACHE_DIR}"
        fi
    else
        warn "Config and cache were NOT deleted."
        warn "  Config : ${CONFIG_DIR}"
        warn "  Cache  : ${CACHE_DIR}"
        warn "Run with --purge to remove them too."
    fi

    echo ""
    echo "━━━  Done  ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
    echo ""
}

main "$@"
