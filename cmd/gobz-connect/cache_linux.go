//go:build linux

package main

import (
	"os"

	"golang.org/x/sys/unix"
)

// releasePageCache advises the kernel to drop the just-written region from the
// page cache (POSIX_FADV_DONTNEED).
//
// Without this, the kernel accumulates dirty pages until the global writeback
// threshold is reached and then flushes them all at once, causing an iowait
// spike that stalls the ALSA write goroutine on Raspberry Pi SD cards.
// Calling this after every write chunk converts the burst flush into a steady
// per-chunk writeback, keeping iowait flat even during track downloads.
func releasePageCache(f *os.File, offset, length int64) {
	_ = unix.Fadvise(int(f.Fd()), offset, length, unix.FADV_DONTNEED)
}
