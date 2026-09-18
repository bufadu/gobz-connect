//go:build !darwin && !linux

package main

func detectDeviceSampleRate() int { return 0 }
