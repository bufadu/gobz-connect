# Building gobz-connect

## Prerequisites

All platforms require **Go 1.21+** and CGo (the audio backend uses C libraries).

```
go version   # must be ≥ 1.21
```

---

## Versioning

The binary version is injected at build time via `-ldflags`.  Binaries built
without the flag report `dev`.

```bash
gobz-connect --version   # prints the version string and exits
gobz-connect -h          # shows version in the usage header
```

To embed the version from the nearest git tag:

```bash
go build -ldflags "-X main.version=$(git describe --tags --always --dirty)" -o gobz-connect .
```

`git describe` output examples:

| Situation | Version string |
|---|---|
| On a tag `v1.2.0` | `v1.2.0` |
| 3 commits after `v1.2.0` | `v1.2.0-3-gabcdef` |
| Uncommitted changes | `v1.2.0-3-gabcdef-dirty` |
| No tags yet | short commit hash, e.g. `abcdef1` |

The `install.sh` script derives the version automatically using this mechanism.

---

## macOS (development / testing, no CEC)

macOS uses CoreAudio, which ships with Xcode Command Line Tools — no extra packages needed.

```bash
# Install Xcode CLT if not already present
xcode-select --install

# Build (dev version)
go build -o gobz-connect ./cmd/gobz-connect/

# Build with version
go build -ldflags "-X main.version=$(git describe --tags --always --dirty)" -o gobz-connect .
```

To cross-compile for both Intel and Apple Silicon and drop the binaries in `dist/`:

```bash
./build.sh
```

---

## Linux x86-64 (no CEC)

Requires ALSA development headers.

```bash
# Debian / Ubuntu
sudo apt install libasound2-dev

# Build
go build -ldflags "-X main.version=$(git describe --tags --always --dirty)" -o gobz-connect .
```

---

## Raspberry Pi — without CEC

Same as Linux above. Build directly on the Pi:

```bash
sudo apt install libasound2-dev
go build -ldflags "-X main.version=$(git describe --tags --always --dirty)" -o gobz-connect .
```

Or cross-compile from macOS using the musl toolchain:

```bash
brew install FiloSottile/musl-cross/musl-cross   # installs aarch64-linux-musl-gcc

./build.sh    # produces dist/gobz-connect-linux-arm64
```

---

## Raspberry Pi — with HDMI-CEC support (`-tags cec`)

The CEC build links against **libcec** (the Pulse Eight library). You must build
natively on the Pi; cross-compilation with libcec from macOS is not supported.

### 1. Install libcec

Raspberry Pi OS ships libcec 7:

```bash
sudo apt install libcec-dev libcec7 libudev-dev
```

### 2. Fix pkg-config stubs

`libcec.pc` still declares `p8-platform` as a dependency even though it was
folded into libcec in version 6. pkg-config fails to resolve it because no
separate package ships `p8-platform.pc` anymore. Create a dummy stub to satisfy
the check (no extra link flags are needed since p8-platform is already compiled
into `libcec.so`):

```bash
sudo sh -c 'cat > /usr/local/lib/pkgconfig/p8-platform.pc << "EOF"
Name: p8-platform
Description: p8-platform stub (bundled in libcec 7)
Version: 2.1.0
Libs:
Cflags:
EOF'
```

Verify that pkg-config can now resolve libcec without errors:

```bash
pkg-config --libs libcec
# expected: -lcec  (no error about p8-platform or libudev)
```

### 3. Build with the `cec` tag

```bash
go build -tags cec \
    -ldflags "-X main.version=$(git describe --tags --always --dirty)" \
    -o gobz-connect \
    ./cmd/gobz-connect/
```

The resulting binary includes full HDMI-CEC support: automatic amp power-on,
active source switching, standby-on-pause, and optional CEC volume control.

### 4. Verify

```bash
./gobz-connect --version
# expected: v1.0.0  (or whatever the current tag is)

./gobz-connect -h
# expected header: gobz-connect v1.0.0

# When running, CEC init is logged at startup:
# cec: ready, standby delay 15m0s, volume control false
```

---

## CEC configuration

Add a `cec:` section to your `config.yaml`:

```yaml
cec:
  enable: true
  standby_delay: 15     # minutes of inactivity before the amp is sent to standby
  volume_control: true  # route Qobuz volume slider to the CEC amp instead of beep
```

| Field | Default | Description |
|---|---|---|
| `enable` | `false` | Activate HDMI-CEC amp control |
| `standby_delay` | `15` | Minutes of inactivity (paused or renderer deselected) before standby |
| `volume_control` | `false` | If `true`, Qobuz volume changes are sent to the amp via CEC; beep output stays fixed at 90%. Initial CEC amp volume is set to 30% at startup. |

### Behaviour notes

- **Power-on**: the amp is powered on when playback transitions from stopped/paused
  to playing. If the amp is already on, the power-on command is skipped.
- **Active source**: `Active Source` is announced whenever the amp is woken, so the
  amp automatically switches its input to the HDMI port connected to the Pi.
- **Standby**: the amp is sent to standby after `standby_delay` minutes of
  inactivity. Standby is skipped if another source (e.g. the TV) has since become
  active.

---

## Build matrix summary

| Target | Host | CEC | Command |
|---|---|---|---|
| macOS arm64 | macOS | — | `go build -ldflags "-X main.version=..."` |
| macOS amd64 | macOS | — | `go build -ldflags "-X main.version=..."` |
| Linux amd64 | Linux | — | `go build -ldflags "-X main.version=..."` |
| Linux amd64 | macOS (cross) | — | `./build.sh` |
| Raspberry Pi arm64 | Pi | — | `go build -ldflags "-X main.version=..."` |
| Raspberry Pi arm64 | macOS (cross) | — | `./build.sh` |
| Raspberry Pi arm64 | Pi | **yes** | `go build -tags cec -ldflags "-X main.version=..."` |
