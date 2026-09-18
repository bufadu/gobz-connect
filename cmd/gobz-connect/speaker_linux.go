//go:build linux

package main

// Custom ALSA speaker backend that supports reinitialisation at a different
// sample rate between tracks (speakerReinit).  This replaces gopxl/beep/v2/speaker
// on Linux; the beep/speaker package is not imported on this platform.
//
// Architecture
// ────────────
//   • One write goroutine continuously reads from a beep.Mixer and writes
//     interleaved int16 samples to the ALSA PCM device.
//   • The PCM is opened in non-blocking mode so that snd_pcm_writei never
//     blocks indefinitely, allowing the goroutine to honour stop requests
//     within a few milliseconds even when the buffer is full (e.g. after a
//     Suspend).
//   • speakerReinit drains then closes the old device and opens a new one at
//     the requested sample rate, enabling true gapless SR changes.

/*
#cgo LDFLAGS: -lasound
#include <alsa/asoundlib.h>
#include <errno.h>
#include <stdlib.h>

// Helper predicates so Go can test errno-encoded return values without
// hardcoding numbers.  static inline functions are always inlined by the
// compiler and never become external linker symbols.
static inline int alsa_is_eagain(snd_pcm_sframes_t rc) { return rc == (snd_pcm_sframes_t)(-EAGAIN); }
static inline int alsa_is_epipe(snd_pcm_sframes_t rc)  { return rc == (snd_pcm_sframes_t)(-EPIPE); }

// alsa_open opens "default" in non-blocking playback mode and configures the
// PCM for 16-bit signed LE stereo at the requested rate.
// *period and *total are in/out: the caller proposes values, ALSA adjusts them.
// Returns 0 on success, a negative ALSA error code on failure.
static int alsa_open(snd_pcm_t **pcm, unsigned int rate,
                     snd_pcm_uframes_t *period, snd_pcm_uframes_t *total) {
    int err;
    snd_pcm_hw_params_t *hw;

    if ((err = snd_pcm_open(pcm, "default",
                            SND_PCM_STREAM_PLAYBACK,
                            SND_PCM_NONBLOCK)) < 0)
        return err;

    snd_pcm_hw_params_alloca(&hw);
    if ((err = snd_pcm_hw_params_any(*pcm, hw))                                        < 0) goto fail;
    if ((err = snd_pcm_hw_params_set_access(*pcm, hw, SND_PCM_ACCESS_RW_INTERLEAVED))  < 0) goto fail;
    if ((err = snd_pcm_hw_params_set_format(*pcm, hw, SND_PCM_FORMAT_S16_LE))          < 0) goto fail;
    if ((err = snd_pcm_hw_params_set_channels(*pcm, hw, 2))                            < 0) goto fail;
    if ((err = snd_pcm_hw_params_set_rate_near(*pcm, hw, &rate, NULL))                 < 0) goto fail;
    if ((err = snd_pcm_hw_params_set_period_size_near(*pcm, hw, period, NULL))         < 0) goto fail;
    if ((err = snd_pcm_hw_params_set_buffer_size_near(*pcm, hw, total))                < 0) goto fail;
    if ((err = snd_pcm_hw_params(*pcm, hw))                                            < 0) goto fail;
    if ((err = snd_pcm_prepare(*pcm))                                                  < 0) goto fail;
    return 0;
fail:
    snd_pcm_close(*pcm);
    *pcm = NULL;
    return err;
}

// alsa_write attempts a non-blocking write.
// Returns the number of frames written (>= 0), or:
//   -EAGAIN  buffer full, caller should wait then retry
//   -EPIPE   underrun; snd_pcm_prepare has been called, caller should retry
//   other negative: unrecoverable error
static snd_pcm_sframes_t alsa_write(snd_pcm_t *pcm,
                                    const int16_t *buf,
                                    snd_pcm_uframes_t frames) {
    snd_pcm_sframes_t n = snd_pcm_writei(pcm, buf, frames);
    if (n == -EPIPE) {
        snd_pcm_prepare(pcm);   // recover from underrun
        return (snd_pcm_sframes_t)(-EPIPE);
    }
    return n;
}
*/
import "C"

import (
	"fmt"
	"log"
	"sync"
	"unsafe"

	"github.com/gopxl/beep/v2"
)

// ---- linuxSpeaker -----------------------------------------------------------

type linuxSpeaker struct {
	// mixMu serialises mixer access between the write goroutine (reader) and
	// speakerLock/Unlock callers (modifiers).  Same contract as beep/speaker.
	mixMu sync.Mutex
	mixer beep.Mixer

	pcm          *C.snd_pcm_t
	periodFrames int

	// stopCh is closed to ask the write goroutine to exit; doneCh is closed
	// by the goroutine once it has exited.
	stopCh chan struct{}
	doneCh chan struct{}
}

var _lspk = &linuxSpeaker{}

// ---- package-level functions (same API as beep/speaker) ---------------------

func speakerInit(sr beep.SampleRate, bufSize int) error {
	return _lspk.open(sr, bufSize, false /* drop, not drain */)
}

// speakerReinit drains remaining audio, closes the device, and reopens it at
// the new sample rate.  Safe to call from runLoop (not holding mixMu).
func speakerReinit(sr beep.SampleRate, bufSize int) error {
	return _lspk.open(sr, bufSize, true /* drain */)
}

func speakerReinitSupported() bool { return true }

func speakerPlay(s ...beep.Streamer) {
	_lspk.mixMu.Lock()
	_lspk.mixer.Add(s...)
	_lspk.mixMu.Unlock()
}

func speakerClear() {
	_lspk.mixMu.Lock()
	_lspk.mixer.Clear()
	_lspk.mixMu.Unlock()
}

func speakerLock()   { _lspk.mixMu.Lock() }
func speakerUnlock() { _lspk.mixMu.Unlock() }

// speakerSuspend pauses the ALSA PCM hardware.  Returns an error (which the
// caller logs via recordSuspendFailure) if the driver does not support pause
// (e.g. some HDMI drivers return ENOSYS).
func speakerSuspend() error {
	if _lspk.pcm == nil {
		return nil
	}
	if rc := C.snd_pcm_pause(_lspk.pcm, 1); rc < 0 {
		return fmt.Errorf("snd_pcm_pause: %s", C.GoString(C.snd_strerror(rc)))
	}
	return nil
}

func speakerResume() error {
	if _lspk.pcm == nil {
		return nil
	}
	if rc := C.snd_pcm_pause(_lspk.pcm, 0); rc < 0 {
		return fmt.Errorf("snd_pcm_resume: %s", C.GoString(C.snd_strerror(rc)))
	}
	return nil
}

// ---- implementation ---------------------------------------------------------

// open stops any running write goroutine, optionally drains or drops the PCM
// buffer, closes the device, then opens a fresh PCM at the new sample rate.
func (s *linuxSpeaker) open(sr beep.SampleRate, bufSize int, drain bool) error {
	// Stop the running write goroutine (if any).  The goroutine uses a 5ms
	// poll on snd_pcm_wait so it will notice the stop within ~5 ms.
	if s.stopCh != nil {
		close(s.stopCh)
		<-s.doneCh
		s.stopCh = nil
		s.doneCh = nil
	}

	if s.pcm != nil {
		if drain {
			C.snd_pcm_drain(s.pcm) // play remaining buffered samples
		} else {
			C.snd_pcm_drop(s.pcm) // discard immediately (e.g. HDMI reinit)
		}
		C.snd_pcm_close(s.pcm)
		s.pcm = nil
	}

	// Period: 20 ms; buffer: whatever the caller requested (usually 200 ms).
	periodFrames := C.snd_pcm_uframes_t(int(sr) / 50)
	bufFrames := C.snd_pcm_uframes_t(bufSize)

	if rc := C.alsa_open(&s.pcm, C.uint(sr), &periodFrames, &bufFrames); rc < 0 {
		return fmt.Errorf("ALSA open %d Hz: %s", int(sr), C.GoString(C.snd_strerror(rc)))
	}
	s.periodFrames = int(periodFrames)

	s.stopCh = make(chan struct{})
	s.doneCh = make(chan struct{})
	go s.writeLoop()
	return nil
}

// writeLoop reads audio from the mixer and writes it to the ALSA PCM device.
// It exits when stopCh is closed.
func (s *linuxSpeaker) writeLoop() {
	defer close(s.doneCh)

	floatBuf := make([][2]float64, s.periodFrames)
	pcmBuf := make([]int16, s.periodFrames*2)

	for {
		// Check for stop before blocking on the mixer.
		select {
		case <-s.stopCh:
			return
		default:
		}

		// Read a period's worth of samples from the mixer.
		s.mixMu.Lock()
		n, _ := s.mixer.Stream(floatBuf)
		s.mixMu.Unlock()
		if n == 0 {
			continue
		}

		// Convert float64 [-1, 1] → int16.
		for i := 0; i < n; i++ {
			pcmBuf[i*2] = floatToS16(floatBuf[i][0])
			pcmBuf[i*2+1] = floatToS16(floatBuf[i][1])
		}

		// Write to ALSA.  In non-blocking mode, writei may return -EAGAIN
		// (buffer full) or -EPIPE (underrun, already recovered by alsa_write).
		// Loop until all frames are written or we are asked to stop.
		written := 0
		for written < n {
			select {
			case <-s.stopCh:
				return
			default:
			}

			rc := C.alsa_write(
				s.pcm,
				(*C.int16_t)(unsafe.Pointer(&pcmBuf[written*2])),
				C.snd_pcm_uframes_t(n-written),
			)
			switch {
			case rc > 0:
				written += int(rc)
			case C.alsa_is_eagain(rc) != 0: // buffer full in non-blocking mode
				// Wait up to 5 ms for space, then re-check stop.
				C.snd_pcm_wait(s.pcm, 5)
			case C.alsa_is_epipe(rc) != 0: // underrun; alsa_write already called snd_pcm_prepare
				// Just retry the write.
			default:
				log.Printf("speaker: ALSA write error: %s",
					C.GoString(C.snd_strerror(C.int(rc))))
				// Attempt recovery; skip remaining frames in this period.
				C.snd_pcm_prepare(s.pcm)
				written = n
			}
		}
	}
}

func floatToS16(v float64) int16 {
	if v > 1 {
		v = 1
	} else if v < -1 {
		v = -1
	}
	return int16(v * 32767)
}
