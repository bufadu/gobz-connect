package main

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// CECConfig holds HDMI-CEC settings for amplifier control.
type CECConfig struct {
	Enable        bool   `yaml:"enable"`
	StandbyDelay  int    `yaml:"standby_delay"`  // minutes; default 15
	VolumeControl bool   `yaml:"volume_control"` // route Qobuz volume to CEC amp
	// FallbackLogAddr and FallbackPhysAddr are used on adapters (e.g. Raspberry
	// Pi VC4 built-in CEC) where libcec cannot discover our own logical address
	// via OSD name scan or PollDevice. Set FallbackLogAddr to the playback-device
	// logical address allocated by libcec (usually 4) and FallbackPhysAddr to the
	// physical address of the HDMI port we are connected to (e.g. "3.0.0.0").
	// Leave both at zero/empty to rely on auto-discovery.
	FallbackLogAddr int    `yaml:"fallback_log_addr"`  // 0 means "not set"
	FallbackPhysAddr string `yaml:"fallback_phys_addr"` // e.g. "3.0.0.0"; empty = use "0.0.0.0"
}

// Config holds all user-configurable settings loaded from config.yaml.
type Config struct {
	Email       string `yaml:"email"`
	Password    string `yaml:"password"`
	// Token-based auth: alternative to email+password.
	// Since April 2026 Qobuz added reCAPTCHA to the login endpoint, blocking
	// direct API logins. Use these instead:
	//   1. Open https://play.qobuz.com/login in a browser and log in.
	//   2. Open DevTools → Network tab → filter "user/login".
	//   3. Copy "id" (UserID) and "user_auth_token" (UserAuthToken) from the response.
	UserID        string `yaml:"user_id"`
	UserAuthToken string `yaml:"user_auth_token"`
	DeviceName  string `yaml:"device_name"`
	Port        int    `yaml:"port"`
	SecretsFile string `yaml:"secrets_file"`
	AudioFormat     int    `yaml:"audio_format"`
	CacheDir        string `yaml:"cache_dir"`
	CacheSizeMB     int    `yaml:"cache_size_mb"`
	// BackgroundDownloadRateKBps caps the write speed (in KB/s) of background
	// pre-warm downloads (the next track, fetched ahead of when it's needed)
	// to reduce SD-card I/O contention with the currently playing track's own
	// download, which is never throttled.
	// 0 (the default, or unset) means unlimited — no throttling.
	BackgroundDownloadRateKBps int `yaml:"background_download_rate_kbps"`
	// SpeakerSampleRate sets the output sample rate in Hz (e.g. 44100, 48000,
	// 96000, 192000). When 0 (the default), the program auto-detects the
	// maximum sample rate supported by the default audio device and uses that.
	// Set this to override auto-detection for manual tuning.
	SpeakerSampleRate int `yaml:"speaker_sample_rate"`
	// CEC configures HDMI-CEC control of the connected amplifier.
	// Requires the binary to be built with -tags cec and libcec installed.
	CEC CECConfig `yaml:"cec"`
	// UnauthenticatedMode skips all Qobuz API authentication (login, session,
	// secret verification) and starts only the mDNS advertisement and
	// WebSocket listener.  The renderer is still discoverable by the Qobuz
	// app; when the app connects via mDNS it injects its own JWT which is then
	// used for the WebSocket session.  Useful for exploring the protocol
	// without valid API credentials.
	UnauthenticatedMode bool `yaml:"unauthenticated_mode"`

	// ResampleQuality controls the quality of the resampler used when the audio
	// track sample rate differs from the output device sample rate.  With
	// adaptive_sample_rate enabled (the default) the resampler is only invoked
	// when speaker_sample_rate caps a track's native rate (e.g. a 192 kHz track
	// capped at 96 kHz).  Without adaptive_sample_rate every track that differs
	// from the speaker rate is resampled, making this setting critical for
	// low-power devices.
	//
	// quality | use case
	// --------|---------
	// 1       | very high performance, on-the-fly resampling, low quality
	// 3-4     | good performance, on-the-fly resampling, good quality
	// 6       | higher CPU usage, usually not suitable for on-the-fly resampling, very good quality
	// >6      | even higher CPU usage, for offline resampling, very good quality
	//
	// Sane quality values are usually below 16. Higher values will consume too much CPU,
	// giving negligible quality improvements.
	ResampleQuality int `yaml:"resample_quality"`

	// AdaptiveSampleRate makes gobz-connect reinitialise the audio output at
	// the native sample rate of each track, eliminating on-the-fly resampling.
	// When a playlist contains tracks with different sample rates, the current
	// track is allowed to finish and then the audio device is reinitialised
	// before the next track starts (a brief silence of ~100 ms replaces the
	// gapless transition at the sample-rate boundary).
	//
	// Defaults to true.  Set to false to keep the audio device locked at a
	// fixed sample rate (speaker_sample_rate or the auto-detected device rate)
	// and resample all tracks that differ.
	AdaptiveSampleRate *bool `yaml:"adaptive_sample_rate"`
}

// IsAdaptiveSampleRate returns true when adaptive sample rate is enabled
// (the default). Returns false only when explicitly set to false in config.
func (c *Config) IsAdaptiveSampleRate() bool {
	return c.AdaptiveSampleRate == nil || *c.AdaptiveSampleRate
}

func loadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config file: %w", err)
	}
	defaultTrue := true
	cfg := &Config{
		DeviceName:  "QobuzConnect",
		Port:        1984,
		SecretsFile: "qobuz-secrets.json",
		AudioFormat:     6,
		CacheDir:        "/tmp/qobuz-cache",
		CacheSizeMB:     1024,
		CEC:             CECConfig{StandbyDelay: 15},
		ResampleQuality:    4,
		AdaptiveSampleRate: &defaultTrue,
	}
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parsing config file: %w", err)
	}
	hasEmailAuth := cfg.Email != "" && cfg.Password != ""
	hasTokenAuth := cfg.UserID != "" && cfg.UserAuthToken != ""
	if !hasEmailAuth && !hasTokenAuth && !cfg.UnauthenticatedMode {
		return nil, fmt.Errorf("config.yaml: provide either (email+password) or (user_id+user_auth_token), or set unauthenticated_mode: true")
	}
	return cfg, nil
}
