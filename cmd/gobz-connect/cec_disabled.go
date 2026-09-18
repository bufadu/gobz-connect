//go:build !cec
// +build !cec

package main

import "log"

// CECManager is a no-op stub used when the binary is built without -tags cec.
type CECManager struct{}

func NewCECManager(cfg *Config) *CECManager {
	if cfg.CEC.Enable {
		log.Printf("cec: enable=true in config but binary was built without CEC support — rebuild with: go build -tags cec")
	}
	return nil
}
func (m *CECManager) OnPlayback(_ bool)             {}
func (m *CECManager) OnVolume(_ uint32)              {}
func (m *CECManager) SetOnInitialVolume(_ func(int)) {}
func (m *CECManager) SetOnAmpWake(_ func())          {}
