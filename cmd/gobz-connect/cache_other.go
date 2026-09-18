//go:build !linux

package main

import "os"

func releasePageCache(_ *os.File, _, _ int64) {}
