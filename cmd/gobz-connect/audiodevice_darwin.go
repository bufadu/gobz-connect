//go:build darwin

package main

/*
#cgo LDFLAGS: -framework CoreAudio
#include <CoreAudio/CoreAudio.h>
#include <stdlib.h>

static double maxOutputSampleRate() {
	AudioObjectID devID = kAudioObjectUnknown;
	UInt32 sz = sizeof(devID);
	AudioObjectPropertyAddress addr = {
		kAudioHardwarePropertyDefaultOutputDevice,
		kAudioObjectPropertyScopeGlobal,
		0 // kAudioObjectPropertyElementMain == kAudioObjectPropertyElementMaster == 0
	};
	if (AudioObjectGetPropertyData(kAudioObjectSystemObject, &addr, 0, NULL, &sz, &devID) != kAudioHardwareNoError)
		return 0;
	if (devID == kAudioObjectUnknown)
		return 0;

	addr.mSelector = kAudioDevicePropertyAvailableNominalSampleRates;
	addr.mScope    = kAudioObjectPropertyScopeOutput;
	if (AudioObjectGetPropertyDataSize(devID, &addr, 0, NULL, &sz) != kAudioHardwareNoError || sz == 0)
		return 0;

	AudioValueRange *ranges = (AudioValueRange *)malloc(sz);
	if (!ranges) return 0;
	if (AudioObjectGetPropertyData(devID, &addr, 0, NULL, &sz, ranges) != kAudioHardwareNoError) {
		free(ranges);
		return 0;
	}
	double best = 0;
	int n = (int)(sz / sizeof(AudioValueRange));
	for (int i = 0; i < n; i++) {
		if (ranges[i].mMaximum > best)
			best = ranges[i].mMaximum;
	}
	free(ranges);
	return best;
}
*/
import "C"

func detectDeviceSampleRate() int {
	rate := float64(C.maxOutputSampleRate())
	if rate < 8000 || rate > 384000 {
		return 0
	}
	return int(rate)
}
