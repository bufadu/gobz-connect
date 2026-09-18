//go:build !linux

package main

// On non-Linux platforms (macOS, etc.) the beep/speaker package is used as-is.
// speakerReinit is not supported because oto's audio context cannot be closed
// and recreated; the caller falls back to resampling in that case.

import (
	"errors"

	"github.com/gopxl/beep/v2"
	"github.com/gopxl/beep/v2/speaker"
)

func speakerInit(sr beep.SampleRate, bufSize int) error { return speaker.Init(sr, bufSize) }

// speakerReinit is not supported on non-Linux platforms.  beep/speaker wraps
// oto whose audio context cannot be closed and reopened.
func speakerReinit(_ beep.SampleRate, _ int) error {
	return errors.New("speaker reinit not supported on this platform — resampling will be used")
}

func speakerReinitSupported() bool { return false }

func speakerPlay(s ...beep.Streamer)  { speaker.Play(s...) }
func speakerClear()                   { speaker.Clear() }
func speakerLock()                    { speaker.Lock() }
func speakerUnlock()                  { speaker.Unlock() }
func speakerSuspend() error           { return speaker.Suspend() }
func speakerResume() error            { return speaker.Resume() }
