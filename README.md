# gobz-connect

🇫🇷 [Lire ce document en français](README.fr.md)

A Qobuz Connect renderer written in Go, purpose-built to run on a **Raspberry
Pi** — streaming audio over HDMI straight into an amplifier or AV receiver,
with HDMI-CEC control so the amp powers on/off and switches input
automatically. It also runs well on **macOS** for everyday use — a good way
to repurpose an old Mac mini as a dedicated Qobuz Connect endpoint, for
example. Advertises itself over mDNS so the Qobuz mobile app can discover and
stream audio to it, just like a Chromecast or Sonos device.

## Features

- **Raspberry Pi first**: designed to run headless on a Pi, with the amp/AV
  receiver connected directly over HDMI — no separate DAC or sound card
  needed.
- **HDMI-CEC** *(experimental)*: automatically powers the amp on when playback
  starts, switches its input to the Pi, and sends it to standby after
  inactivity. CEC implementations vary a lot between amp brands and models —
  it may not work with every amplifier. See
  [Raspberry Pi — HDMI & CEC setup](#raspberry-pi--hdmi--cec-setup) below.
- **Gapless playback** across tracks that share the same sample rate — the
  common case for most albums and playlists.
- **Full audio format support up to Hi-Res 192 kHz/24-bit, even on a
  Raspberry Pi** — the adaptive sample-rate engine reinitialises the ALSA
  device at each track's native rate instead of resampling, keeping CPU usage
  low enough for a Pi to handle Hi-Res without choppy playback. See
  [Audio output behaviour](#audio-output-behaviour) below.
- Also runs well on **macOS** (CoreAudio) for everyday use, not just
  development.

---

## Installing on a Raspberry Pi

`install.sh` automates the whole setup: system dependencies, the Go
toolchain, the build itself, a dedicated service user, and a systemd service.
Run it directly on the Pi.

### 1. Install git and clone the repository

```bash
sudo apt update
sudo apt install -y git
git clone https://github.com/bufadu/gobz-connect.git
cd gobz-connect
```

### 2. Run the installer

```bash
sudo bash install.sh
# or, for HDMI-CEC support (experimental — see the HDMI & CEC section below):
sudo bash install.sh --cec
```

This installs the system dependencies (`build-essential`, `pkg-config`,
`libasound2-dev`, plus the CEC libraries when `--cec` is used), installs Go if
it's missing or too old, builds the binary, installs it to
`/usr/local/bin/gobz-connect`, creates a dedicated `gobz-connect` system user,
and installs — but does not start — a systemd service.

Re-running `install.sh` later performs an in-place upgrade: it rebuilds from
the latest source and restarts the service for you.

### 3. Edit the configuration

```bash
sudo nano /etc/gobz-connect/config.yaml
```

A commented example is written there on first install. Fill in at least
`user_id` and `user_auth_token` (see [AUTHENTICATION.md](AUTHENTICATION.md)),
or set `unauthenticated_mode: true`. See [Full reference](#full-reference)
below for every available setting.

### 4. Start the service

```bash
sudo systemctl start gobz-connect
```

`install.sh` already enables the service to start automatically on boot.

### 5. Useful commands

```bash
# Follow the logs live
sudo journalctl -u gobz-connect -f

# Restart after editing the config
sudo systemctl restart gobz-connect

# Check whether it's running
sudo systemctl status gobz-connect

# Stop it
sudo systemctl stop gobz-connect
```

### Uninstalling

```bash
sudo bash uninstall.sh          # keeps config and cache
sudo bash uninstall.sh --purge  # also removes config, secrets, and cache
```

---

## Configuration

All settings live in a single `config.yaml` file.

### Authentication

gobz-connect needs Qobuz credentials to fetch track metadata and stream URLs.
See **[AUTHENTICATION.md](AUTHENTICATION.md)** for how to obtain a token and
for the full list of supported authentication modes, including
`unauthenticated_mode`, where credentials are supplied by the Qobuz app itself
instead of being configured locally.

### Minimal example

```yaml
# Token-based auth — see AUTHENTICATION.md for how to obtain these
user_id: "123456789"
user_auth_token: "your_auth_token_here"

device_name: My Renderer
port: 1984
```

### Full reference

```yaml
# ── Authentication ──────────────────────────────────────────────────────────
# See AUTHENTICATION.md for how to obtain these.
user_id: ""             # User ID from the Qobuz web player
user_auth_token: ""     # Auth token from the Qobuz web player

unauthenticated_mode: false
# Experimental. Alternative to user_id/user_auth_token: skip local
# authentication entirely and rely on the Qobuz app to supply credentials
# over mDNS (connect-to-qconnect) when it connects to this renderer. Those
# credentials are short-lived and only the app can renew them — nothing
# guarantees it will do so automatically, so long unattended sessions can
# eventually lose the ability to load new tracks until the app reconnects.
# Prefer user_id/user_auth_token above for unattended, long-running use.

# ── Device ──────────────────────────────────────────────────────────────────
device_name: QobuzConnect   # Name shown in the Qobuz app's renderer list
port: 1984                  # HTTP port for mDNS discovery endpoints

# ── Audio quality ────────────────────────────────────────────────────────────
audio_format: 6   # Requested Qobuz stream format
                  #   5  = MP3
                  #   6  = FLAC CD (16-bit 44.1 kHz)  ← default
                  #   7  = Hi-Res 96 kHz
                  #  27  = Hi-Res 192 kHz

# ── Audio output ─────────────────────────────────────────────────────────────
speaker_sample_rate: 0
# Output sample rate in Hz (e.g. 44100, 48000, 96000, 192000).
# 0 (default): auto-detect the maximum rate supported by the default ALSA/
# CoreAudio device.
#
# With adaptive_sample_rate enabled (the default on Linux), this acts as a
# hardware ceiling: the speaker is reinitialised per track at the track's
# native rate, but never above this value.  Useful when the audio device
# physically does not support certain rates (e.g. a USB DAC limited to 96 kHz).
#
# With adaptive_sample_rate disabled, this fixes the speaker at a single rate
# and all tracks that differ are resampled.

adaptive_sample_rate: true
# Linux only.  When true (default), gobz-connect reinitialises the ALSA audio
# device at the native sample rate of each track.  Benefits:
#   • No resampling for the common case → zero CPU overhead on a Raspberry Pi.
#   • Every track plays at its true quality (44.1 kHz CD tracks at 44.1 kHz,
#     96 kHz Hi-Res at 96 kHz, etc.).
#
# Trade-off: when two consecutive tracks have different sample rates, the
# device is reinitialised between them, producing a brief silence (~100–200 ms)
# instead of a gapless transition.  Within a single-SR playlist (all CD or
# all Hi-Res) playback remains fully gapless.
#
# On macOS, adaptive_sample_rate has no effect: CoreAudio's audio context
# cannot be closed and reopened, so the device is always locked at a fixed
# rate (auto-detected or speaker_sample_rate) and tracks are resampled as
# needed.  Resampling on macOS uses CoreAudio's hardware SRC and is
# essentially free; no choppy audio occurs.
#
# Set to false to lock the device at a fixed rate and resample all tracks,
# which may cause choppy audio on a Raspberry Pi when upsampling
# (e.g. 44.1 → 96 kHz).

resample_quality: 4
# Resampler quality when a track's sample rate must be converted.
# With adaptive_sample_rate enabled the resampler is only invoked when
# speaker_sample_rate caps a track's native rate (e.g. a 192 kHz track capped
# at 96 kHz), so this setting rarely matters in that mode.
# With adaptive_sample_rate disabled this is critical on low-power devices.
#
# value | trade-off
# ------|-----------
#  1    | fastest, lowest quality — use on very slow hardware
#  4    | good balance (default)
#  6    | higher quality, higher CPU — borderline for on-the-fly use
# >6    | offline/archival use only; too slow for real-time playback

# ── Track cache ──────────────────────────────────────────────────────────────
cache_dir: /tmp/qobuz-cache   # Directory for the local track cache
cache_size_mb: 1024           # Maximum cache size in MB, 0 to disable

background_download_rate_kbps: 0
# Rate-limit background pre-warm downloads (the next track, fetched ahead of
# when it's needed) in KB/s, to reduce SD-card I/O contention with the track
# currently playing. The currently playing track's own download is never
# throttled. 0 (default) = unlimited.

# ── Secrets ──────────────────────────────────────────────────────────────────
secrets_file: qobuz-secrets.json
# Path to the file where gobz-connect stores the scraped Qobuz app credentials
# (AppID and AppSecret).  Generated automatically on first run.

# ── HDMI-CEC ─────────────────────────────────────────────────────────────────
cec:
  enable: false
  standby_delay: 15     # minutes of inactivity before sending amp to standby
  volume_control: false # route Qobuz volume slider to the amp via CEC
  # Advanced, only needed if amp auto-discovery fails on your adapter
  # (common on the Raspberry Pi's built-in VC4 CEC adapter) — see the
  # HDMI & CEC setup section below.
  fallback_log_addr: 0    # 0 = not set; use auto-discovery
  fallback_phys_addr: ""  # e.g. "3.0.0.0"; empty = not set
# Requires the binary built with -tags cec.  See BUILD.md for build steps and
# the Raspberry Pi — HDMI & CEC setup section below for the one-time OS setup.
```

---

## Audio output behaviour

### Platform overview

gobz-connect uses different audio backends depending on the platform:

| Platform | Audio backend | Adaptive SR | Resampling when SR differs |
|----------|--------------|-------------|---------------------------|
| **Linux** | Custom ALSA (CGo) | Yes — PCM reopened per track | Only when `speaker_sample_rate` caps the track's native rate |
| **macOS** | CoreAudio via beep/speaker + oto | No — oto context is permanent | Always (free: CoreAudio SRC) |

### Linux — adaptive sample rate (default)

On Linux, gobz-connect uses a custom ALSA backend that can close and reopen the
PCM device at any sample rate between tracks.

**First track:** speaker initialisation is deferred until the first track is
known, so the device always opens at that track's native rate — no resampling
occurs even on the very first track.

**Same SR playlist** (e.g. all CD 44.1 kHz or all Hi-Res 96 kHz): the device is
opened once and playback is fully gapless with zero CPU overhead.

**Mixed SR playlist** (e.g. a CD track followed by a Hi-Res track): gobz-connect
waits for the current track to finish, drains the PCM buffer, closes the device,
and reopens it at the new rate.  This produces a brief silence (~100–200 ms) at
the sample-rate boundary instead of a gapless transition.  No resampling is
performed.

| Track | Native SR | Speaker inited at | Resampling |
|-------|-----------|-------------------|------------|
| CD FLAC | 44.1 kHz | 44.1 kHz | none |
| Hi-Res | 96 kHz | 96 kHz | none |
| Hi-Res (capped) | 192 kHz | 96 kHz (`speaker_sample_rate: 96000`) | 192 → 96 kHz |

### Linux — fixed sample rate (`adaptive_sample_rate: false`)

The device is initialised once at startup (auto-detected or `speaker_sample_rate`)
and all tracks are resampled to that rate.  Upsampling (e.g. 44.1 → 96 kHz) is
CPU-intensive and may cause choppy audio on a Raspberry Pi.  Use
`resample_quality: 1` to reduce CPU load at the cost of quality, or leave
`adaptive_sample_rate` at its default.

### macOS

The oto audio context is created once and cannot be recreated.  The device is
locked at the rate detected at startup (or `speaker_sample_rate`).  All tracks
with a different native SR are resampled by beep using the quality set by
`resample_quality`.  CoreAudio performs sample-rate conversion efficiently in
hardware, so resampling is not a CPU concern on macOS.  `adaptive_sample_rate`
has no effect on macOS.

---

## Raspberry Pi setup

### Power supply

Choppy audio and seek stalls are often caused by CPU throttling due to
undervoltage.  Check `dmesg | grep -i volt` and use an official 5V/3A power
supply.  A bad power supply will cause issues regardless of software settings.

### SD card

gobz-connect caches downloaded tracks to disk continuously while playing,
which means small, mixed reads and writes on the same card that holds the OS —
not the big sequential transfers most speed ratings are marketed around.

- Prefer a card rated **A1 or A2** (Application Performance Class). These
  ratings guarantee a minimum random IOPS figure, which is what this workload
  actually stresses — plain sequential MB/s numbers on the packaging don't
  tell you much here.
- **U3 or V30** (minimum sustained sequential write) is a reasonable secondary
  signal of a card with a decent controller.
- Avoid bargain/no-name cards. Inconsistent performance under mixed
  read/write load is a common cause of audible stutters during Hi-Res
  downloads. If you see this on a card that otherwise meets the ratings
  above, try lowering `background_download_rate_kbps` (see the config
  reference) to reduce write pressure while a track is pre-fetched in the
  background.

### Raspberry Pi — HDMI & CEC setup

This section is Raspberry Pi OS (Raspbian) specific. It's a one-time setup
done on the Pi's operating system itself, separate from building gobz-connect
with CEC support — see [BUILD.md](BUILD.md) for that. Follow it if you want
audio over HDMI and, optionally, HDMI-CEC control of your amp.

**CEC support is experimental.** HDMI-CEC is notoriously inconsistent across
manufacturers — behaviour that works well on one amp may not work at all on
another, even when both claim CEC compliance. Treat it as a bonus, not a
guarantee, and always keep a way to power the amp on manually.

**1. Make HDMI the default audio output**

```bash
sudo raspi-config
# System Options → Audio → select the HDMI output
```

On a Pi 4/5 with two HDMI ports, pick the port your amp is actually connected
to. Reboot, then confirm the device is visible:

```bash
aplay -l
# expect a card named something like "vc4-hdmi" in the list
```

**2. Keep HDMI active even if the amp is slow to assert hotplug**

Some amps/receivers only assert the HDMI hotplug signal once they're already
powered on, which can prevent the Pi from finding the audio (and CEC) device
at boot. Force it in `/boot/firmware/config.txt` (`/boot/config.txt` on older
Raspberry Pi OS releases):

```ini
hdmi_force_hotplug=1
```

**3. Confirm the KMS driver is active (required for CEC)**

Current Raspberry Pi OS images default to this already, but it's worth
confirming — HDMI-CEC on the Pi is exposed through the VC4 KMS display driver:

```ini
dtoverlay=vc4-kms-v3d
```

Do **not** set `hdmi_ignore_cec_init=1` anywhere in `config.txt` — that
disables CEC entirely.

**4. Grant access to the CEC device**

gobz-connect talks to `/dev/cec0`, which on Raspberry Pi OS is owned by the
`video` group. If gobz-connect runs as a dedicated service user (as
`install.sh` sets up), add that user to the group:

```bash
sudo usermod -aG video gobz-connect
```

**5. Sanity-check CEC independently of gobz-connect**

Before troubleshooting gobz-connect itself, confirm the Pi can see your amp
over CEC at all:

```bash
sudo apt install cec-utils
echo 'scan' | cec-client -s -d 1
```

Your amplifier should show up as a device on the bus. If it doesn't, the issue
is in the HDMI/CEC wiring or the amp's own CEC setting (often labelled
"Anynet+", "Bravia Sync", "SimpLink", etc. depending on the brand) — not in
gobz-connect.

Once this all checks out, build with `-tags cec` and set `cec.enable: true` in
`config.yaml` — see [BUILD.md](BUILD.md#cec-configuration) for the build steps
and the full list of `cec:` options.

---

## License & disclaimer

Licensed under the [MIT License](LICENSE).

This project was written with the assistance of Anthropic's Claude, building
on protocol knowledge and patterns from other open-source Qobuz Connect
implementations already available on GitHub. It is an independent,
unofficial project, not affiliated with, endorsed by, or supported by Qobuz
in any way. "Qobuz" is referenced solely to describe interoperability with
its service.

---

## Further reading

- [AUTHENTICATION.md](AUTHENTICATION.md) — authentication modes in detail
- [BUILD.md](BUILD.md) — how to compile, cross-compile, and enable CEC support
- [PROTOCOL.md](PROTOCOL.md) — QConnect WebSocket protocol notes
