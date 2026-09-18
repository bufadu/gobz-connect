//go:build linux

package main

/*
#cgo LDFLAGS: -lasound
#include <alsa/asoundlib.h>

// queryDeviceMaxRate opens the named ALSA device and returns its maximum
// playback sample rate, or 0 on failure.
static unsigned int queryDeviceMaxRate(const char *name) {
	snd_pcm_t *pcm = NULL;
	if (snd_pcm_open(&pcm, name, SND_PCM_STREAM_PLAYBACK, SND_PCM_NONBLOCK) < 0)
		return 0;

	snd_pcm_hw_params_t *params;
	snd_pcm_hw_params_alloca(&params);
	if (snd_pcm_hw_params_any(pcm, params) < 0) {
		snd_pcm_close(pcm);
		return 0;
	}

	unsigned int maxRate = 0;
	int dir = 0;
	snd_pcm_hw_params_get_rate_max(params, &maxRate, &dir);
	snd_pcm_close(pcm);
	return maxRate;
}

// maxOutputSampleRate first queries the ALSA "default" device.  When that
// returns an unreasonably high value (PipeWire / PulseAudio virtual devices
// report UINT_MAX because they accept any rate and resample internally), it
// resolves which hardware card "default" maps to via ALSA's
// defaults.pcm.card config key and queries that card directly.
static unsigned int maxOutputSampleRate() {
	unsigned int r = queryDeviceMaxRate("default");
	if (r >= 8000 && r <= 384000)
		return r;

	// "default" is a virtual device — resolve the underlying hardware card
	// from ALSA's own configuration (defaults.pcm.card).
	snd_config_update();
	snd_config_t *n;
	int card = -1;
	if (snd_config_search(snd_config, "defaults.pcm.card", &n) == 0) {
		long v;
		const char *s;
		if (snd_config_get_integer(n, &v) == 0)
			card = (int)v;
		else if (snd_config_get_string(n, &s) == 0)
			card = snd_card_get_index(s);
	}
	if (card < 0)
		return 0;

	char devname[32];
	snprintf(devname, sizeof(devname), "hw:%d,0", card);
	r = queryDeviceMaxRate(devname);
	return (r >= 8000 && r <= 384000) ? r : 0;
}
*/
import "C"

func detectDeviceSampleRate() int {
	rate := int(C.maxOutputSampleRate())
	if rate < 8000 || rate > 384000 {
		return 0
	}
	return rate
}
