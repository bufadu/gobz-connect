#!/usr/bin/env bash
# build.sh — cross-compile qobuz-connect for all supported platforms.
#
# CGo dependencies per platform:
#   darwin/*   → CoreAudio (macOS SDK, available on any Mac regardless of arch)
#   linux/*    → libasound (ALSA); requires musl cross-compilers on macOS:
#                  brew install FiloSottile/musl-cross/musl-cross
#                  (installs x86_64-linux-musl-gcc and aarch64-linux-musl-gcc)
#
# Outputs land in ./dist/

set -euo pipefail

BINARY=qobuz-connect
OUT_DIR=dist
MODULE=$(go list -m 2>/dev/null || echo ".")

mkdir -p "$OUT_DIR"

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

have() { command -v "$1" &>/dev/null; }

build() {
    local goos=$1 goarch=$2 cc=$3 out=$4
    local extra_ldflags=${5:-}

    echo "→ Building $out ..."
    env \
        GOOS="$goos" \
        GOARCH="$goarch" \
        CGO_ENABLED=1 \
        CC="$cc" \
        go build \
            -trimpath \
            -ldflags="-s -w $extra_ldflags" \
            -o "$OUT_DIR/$out" \
            ./cmd/gobz-connect/
    echo "  ✓ $OUT_DIR/$out ($(du -sh "$OUT_DIR/$out" | cut -f1))"
}

# ---------------------------------------------------------------------------
# macOS — Intel (amd64)
# ---------------------------------------------------------------------------
if [[ "$(go env GOOS)" == "darwin" ]]; then
    build darwin amd64 "$(xcrun --find clang) -arch x86_64 -isysroot $(xcrun --show-sdk-path)" \
        "${BINARY}-darwin-amd64" \
        "-extldflags '-arch x86_64'"
else
    echo "  skip darwin/amd64 — must be built on macOS"
fi

# ---------------------------------------------------------------------------
# macOS — Apple Silicon (arm64)
# ---------------------------------------------------------------------------
if [[ "$(go env GOOS)" == "darwin" ]]; then
    build darwin arm64 "$(xcrun --find clang) -arch arm64 -isysroot $(xcrun --show-sdk-path)" \
        "${BINARY}-darwin-arm64" \
        "-extldflags '-arch arm64'"
else
    echo "  skip darwin/arm64 — must be built on macOS"
fi

# ---------------------------------------------------------------------------
# Linux — amd64
# ---------------------------------------------------------------------------
if have x86_64-linux-musl-gcc; then
    build linux amd64 x86_64-linux-musl-gcc \
        "${BINARY}-linux-amd64" \
        "-extldflags '-static'"
elif have x86_64-linux-gnu-gcc; then
    build linux amd64 x86_64-linux-gnu-gcc \
        "${BINARY}-linux-amd64"
elif [[ "$(go env GOOS)" == "linux" && "$(go env GOARCH)" == "amd64" ]]; then
    build linux amd64 gcc \
        "${BINARY}-linux-amd64"
else
    echo "  skip linux/amd64 — no cross-compiler found"
    echo "         install: brew install FiloSottile/musl-cross/musl-cross"
fi

# ---------------------------------------------------------------------------
# Linux — arm64 (Raspberry Pi 4/5, 64-bit OS)
# ---------------------------------------------------------------------------
if have aarch64-linux-musl-gcc; then
    build linux arm64 aarch64-linux-musl-gcc \
        "${BINARY}-linux-arm64" \
        "-extldflags '-static'"
elif have aarch64-linux-gnu-gcc; then
    build linux arm64 aarch64-linux-gnu-gcc \
        "${BINARY}-linux-arm64"
elif [[ "$(go env GOOS)" == "linux" && "$(go env GOARCH)" == "arm64" ]]; then
    build linux arm64 gcc \
        "${BINARY}-linux-arm64"
else
    echo "  skip linux/arm64 — no cross-compiler found"
    echo "         install: brew install FiloSottile/musl-cross/musl-cross"
fi

# ---------------------------------------------------------------------------
echo ""
echo "Done. Artefacts in ./$OUT_DIR/:"
ls -lh "$OUT_DIR/${BINARY}-"* 2>/dev/null || true
