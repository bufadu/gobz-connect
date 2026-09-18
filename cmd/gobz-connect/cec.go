//go:build cec
// +build cec

package main

import (
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/claes/cec"
)

// cecAmpAddr is the standard CEC logical address for an Audio System (AV receiver/amp).
const cecAmpAddr = 5

// CECManager controls the HDMI-CEC connected amplifier using the LibCEC Go
// bindings. It powers the amp on at the start of a play session, sets the
// active input source to our HDMI port, and sends the amp to standby after a
// configurable inactivity delay.
type CECManager struct {
	conn         *cec.Connection
	standbyDelay time.Duration
	volControl   bool
	fallbackLogAddr  int    // -1 = not configured
	fallbackPhysAddr string // empty = not configured

	mu           sync.Mutex
	playing      bool
	setAsSource  bool // true after we announced ourselves as active source
	standbyTimer *time.Timer
	volReset     bool // next OnVolume call should record position only, not send CEC
	ourLogAddr   int  // our CEC logical address, -1 until discovered
	ourPhysAddr  string

	volMu        sync.Mutex
	lastQobuzVol int // last Qobuz volume we applied to CEC; -1 = not yet set

	// audioStatusCh receives the <Report Audio Status> byte forwarded by
	// drainChannels so queryAmpVolume() can read it with a timeout.
	audioStatusCh chan byte
	// onInitialVolume, if set, is called with the amp's current volume (0–100)
	// once wakeAmp() has confirmed the amp is on and queried its audio status.
	onInitialVolume func(int)
	// onAmpWake, if set, is called when the amp transitions from off/standby
	// to on. Used to trigger audio device reinitialization after HDMI hotplug.
	onAmpWake func()
}

// NewCECManager opens the LibCEC adapter and initialises CEC support.
// Returns nil when CEC is disabled or the adapter cannot be opened.
func NewCECManager(cfg *Config) *CECManager {
	if !cfg.CEC.Enable {
		return nil
	}

	conn, err := cec.Open("", "gobz-cec")
	if err != nil {
		log.Printf("cec: open failed: %v — CEC support disabled", err)
		return nil
	}

	delay := time.Duration(cfg.CEC.StandbyDelay) * time.Minute
	if delay <= 0 {
		delay = 15 * time.Minute
	}

	fallbackLog := -1
	if cfg.CEC.FallbackLogAddr > 0 {
		fallbackLog = cfg.CEC.FallbackLogAddr
	}
	m := &CECManager{
		conn:             conn,
		standbyDelay:     delay,
		volControl:       cfg.CEC.VolumeControl,
		fallbackLogAddr:  fallbackLog,
		fallbackPhysAddr: cfg.CEC.FallbackPhysAddr,
		lastQobuzVol:     -1,
		ourLogAddr:       -1,
		audioStatusCh:    make(chan byte, 1),
	}

	go m.drainChannels()

	log.Printf("cec: ready, standby delay %v, volume control %v", delay, m.volControl)
	return m
}

// OnPlayback is called when playback starts (playing=true) or stops/pauses
// (playing=false). Safe to call concurrently.
func (m *CECManager) OnPlayback(playing bool) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if playing == m.playing {
		return
	}
	m.playing = playing

	if playing {
		if m.standbyTimer != nil {
			m.standbyTimer.Stop()
			m.standbyTimer = nil
			log.Printf("cec: playback resumed — standby timer cancelled")
		}
		// Reset volume reference: the first OnVolume call will record the
		// current Qobuz level without sending any CEC commands, leaving the
		// amp at whatever volume the user last set.
		m.volReset = true
		go m.wakeAmp()
	} else {
		log.Printf("cec: playback stopped — amplifier standby in %v", m.standbyDelay)
		m.standbyTimer = time.AfterFunc(m.standbyDelay, m.triggerStandby)
	}
}

// SetOnInitialVolume registers a callback that is invoked with the amp's
// current volume (0–100) once wakeAmp() has successfully queried it via CEC.
// Call before the first OnPlayback(true) to guarantee it fires on first wake.
func (m *CECManager) SetOnInitialVolume(f func(int)) {
	m.mu.Lock()
	m.onInitialVolume = f
	m.mu.Unlock()
}

// SetOnAmpWake registers a callback that is invoked when the amp transitions
// from off/standby to on. Use this to trigger audio device reinitialization
// after the HDMI link becomes active (e.g. player.ScheduleAudioReinit).
func (m *CECManager) SetOnAmpWake(f func()) {
	m.mu.Lock()
	m.onAmpWake = f
	m.mu.Unlock()
}

// OnVolume routes a Qobuz volume change (0–100) to the CEC amp.
// No-op when VolumeControl is disabled.
func (m *CECManager) OnVolume(vol uint32) {
	if !m.volControl {
		return
	}
	go m.applyVolume(int(vol))
}

// ---- internal ----

// wakeAmp checks the amplifier power state, turns it on if in standby, then
// announces ourselves as the active source.
func (m *CECManager) wakeAmp() {
	// Step 1: Power on the amp without waiting for our own logical address.
	//
	// When the amp is off/standby the downstream HDMI link carries no EDID,
	// so libcec cannot determine the Pi's physical address and emits
	// CEC_ALERT_PHYSICAL_ADDRESS_ERROR (alert_type=5).  Without a physical
	// address libcec never claims a logical address — making the "wait for
	// address" loop deadlock against the "power on the amp" step.
	//
	// Power-on commands (KeyPress 0x6D) are accepted by receivers even when
	// sent from the unregistered source address 0xF, so we issue them
	// immediately.  Only after the amp is on (HDMI link active, EDID
	// readable) do we wait for libcec to complete address negotiation and
	// send the routing announcements that require a valid source address.
	status := m.conn.GetDevicePowerStatus(cecAmpAddr)
	log.Printf("cec: amp power status: %q", status)
	ampPhys := m.conn.GetDevicePhysicalAddress(cecAmpAddr)
	log.Printf("cec: amp physical address: %q", ampPhys)

	if status != "on" {
		// KeyPress requires a valid source logical address; libcec falls back
		// to 0xF (unregistered) if it hasn't claimed one yet, and some amps
		// reject commands from unregistered sources.  Wait up to 2 s for
		// address negotiation to complete before sending the power-on press.
		// This is safe even when the amp is truly off: the CEC bus remains
		// active and address negotiation can complete without the amp.
		for i := 0; i < 2; i++ {
			if la, _ := m.findOurDevice(); la >= 0 {
				break
			}
			time.Sleep(1 * time.Second)
		}

		// Use KeyPress(0x6D = Power On Function) instead of PowerOn or the
		// Power toggle (0x40): libcec's PowerOn sends Image View On to the TV
		// first, unintentionally waking it; 0x40 is a toggle and would turn
		// the amp OFF if it is already on. 0x6D is a one-directional "turn on"
		// command and is safe to send even when the amp is already on.
		// Using status != "on" instead of status == "standby" handles VC4
		// adapters that return "" for the power status when the amp is in standby.
		log.Printf("cec: powering on amplifier (current status=%q)…", status)
		if err := m.conn.KeyPress(cecAmpAddr, 0x6D); err != nil { // 0x6D = Power On Function
			log.Printf("cec: key press power-on failed: %v", err)
		}
		m.conn.KeyRelease(cecAmpAddr)

		// Poll until the amp reports "on" (up to 10 s).
		for i := 0; i < 10; i++ {
			time.Sleep(1 * time.Second)
			if m.conn.GetDevicePowerStatus(cecAmpAddr) == "on" {
				log.Printf("cec: amplifier is on")
				break
			}
		}
		// Give the amp 1 s to finish routing initialisation before we send
		// input-selection commands; some receivers report "on" slightly
		// before their HDMI switch is ready.
		time.Sleep(1 * time.Second)

		// The amp was off/standby and is now on. The HDMI link is now active,
		// which means the ALSA audio device state may have changed (new EDID,
		// new sample-rate capabilities). Notify the player to reinitialize its
		// audio handle so it uses the live HDMI connection.
		m.mu.Lock()
		onWake := m.onAmpWake
		m.mu.Unlock()
		if onWake != nil {
			onWake()
		}
	} else {
		log.Printf("cec: amplifier already on (status=%q)", status)
	}

	// Step 2: Now that the amp is on the HDMI link is active and libcec can
	// determine the physical address and claim a logical address.  Wait up
	// to 8 s for address negotiation to complete before sending routing
	// announcements that require a valid source address.
	const maxAddrWait = 8
	for i := 0; i < maxAddrWait; i++ {
		if la, _ := m.findOurDevice(); la >= 0 {
			break
		}
		log.Printf("cec: waiting for logical address (%d/%d)…", i+1, maxAddrWait)
		time.Sleep(1 * time.Second)
	}

	m.announceActiveSource()

	// We woke (or found already-on) the amp and started a session — mark
	// ourselves as the session owner so the standby timer fires correctly
	// even if Active Source announcement failed (e.g. logical address not
	// yet discoverable on some adapters).
	m.mu.Lock()
	m.setAsSource = true
	cb := m.onInitialVolume
	m.mu.Unlock()

	// Query the amp's current volume so the Qobuz app can show the correct
	// slider position and future delta calculations start from the right baseline.
	if cb != nil {
		if logAddr, _ := m.findOurDevice(); logAddr >= 0 {
			if vol, ok := m.queryAmpVolume(logAddr); ok {
				// Pre-seed our own volume reference so the first Qobuz echo
				// (which will arrive with this same value) produces delta=0
				// and does not send spurious CEC key presses.
				m.volMu.Lock()
				m.lastQobuzVol = vol
				m.volMu.Unlock()
				cb(vol)
			}
		}
	}
}

// queryAmpVolume sends <Give Audio Status> to the amp and waits up to 2 s for
// a <Report Audio Status> reply. Returns the volume (0–100) and ok=true on
// success; returns 0, false on timeout or send error.
// Not all amps support System Audio Control; a timeout is the normal failure.
func (m *CECManager) queryAmpVolume(ourLogAddr int) (vol int, ok bool) {
	// Drain any stale byte left from a previous (timed-out) query.
	select {
	case <-m.audioStatusCh:
	default:
	}
	cmd := fmt.Sprintf("%x5:71", ourLogAddr) // Give Audio Status → amp (addr 5)
	m.conn.Transmit(cmd)
	select {
	case status := <-m.audioStatusCh:
		vol = int(status & 0x7F) // bits 0–6: volume 0–100
		muted := status&0x80 != 0
		log.Printf("cec: amp audio status: volume=%d%% muted=%v", vol, muted)
		return vol, true
	case <-time.After(2 * time.Second):
		log.Printf("cec: no audio status response from amp (amp may not support System Audio Control)")
		return 0, false
	}
}

// parseAudioStatus returns the status byte from a <Report Audio Status> CEC
// frame string (format "{src}{dst}:{opcode}:{params...}").
func parseAudioStatus(s string) (status byte, ok bool) {
	parts := strings.SplitN(s, ":", 3)
	if len(parts) < 3 {
		return
	}
	var opcode uint64
	if _, err := fmt.Sscanf(parts[1], "%x", &opcode); err != nil || byte(opcode) != 0x7A {
		return
	}
	var param uint64
	if _, err := fmt.Sscanf(strings.TrimSpace(parts[2]), "%x", &param); err != nil {
		return
	}
	return byte(param), true
}

// findOurDevice returns our CEC logical address and physical address, caching
// the result after the first successful lookup.
//
// Four discovery passes are attempted:
//
//  1. OSD name scan via GetDeviceOSDName: iterate all 16 addresses looking for
//     "gobz-cec". Works on adapters where libcec returns the configured name
//     when queried for our own address.
//
//  2. List() scan: libcec's internal device table may carry the OSD name we
//     configured even when GetDeviceOSDName() returns "" for our address
//     (adapters without CEC loopback, e.g. Raspberry Pi VC4).
//
//  3. PollDevice fallback: checks recording-device addresses [1,2,3] and
//     playback addresses [4,8,11]. PollDevice returns "1" for occupied
//     addresses; the first match is assumed to be ours. NOTE: this can
//     misidentify another device on the bus — passes 1 and 2 are preferred.
//
//  4. Hard-coded config fallback (cec.fallback_log_addr / fallback_phys_addr).
func (m *CECManager) findOurDevice() (logAddr int, physAddr string) {
	m.mu.Lock()
	if m.ourLogAddr >= 0 {
		la, pa := m.ourLogAddr, m.ourPhysAddr
		m.mu.Unlock()
		return la, pa
	}
	m.mu.Unlock()

	// Pass 1: OSD name scan.
	for addr := 0; addr < 16; addr++ {
		name := strings.TrimRight(m.conn.GetDeviceOSDName(addr), "\x00 ")
		if name == "gobz-cec" {
			phys := m.conn.GetDevicePhysicalAddress(addr)
			m.mu.Lock()
			m.ourLogAddr = addr
			m.ourPhysAddr = phys
			m.mu.Unlock()
			log.Printf("cec: our device (osd): logical=%d physical=%s", addr, phys)
			return addr, phys
		}
	}

	// Pass 2: List() scan — libcec's internal device table may include our own
	// device with the configured OSD name even when GetDeviceOSDName() returns
	// "" for our address (e.g. adapters without CEC loopback).
	for _, dev := range m.conn.List() {
		if strings.TrimRight(dev.OSDName, "\x00 ") == "gobz-cec" {
			phys := dev.PhysicalAddress
			if phys == "f.f.f.f" {
				phys = ""
			}
			m.mu.Lock()
			m.ourLogAddr = dev.LogicalAddress
			m.ourPhysAddr = phys
			m.mu.Unlock()
			log.Printf("cec: our device (list): logical=%d physical=%s", dev.LogicalAddress, phys)
			return dev.LogicalAddress, phys
		}
	}

	// Pass 3: Physical address topology scan.
	//
	// PollDevice is NOT reliable for self-discovery: on Pulse Eight adapters
	// (and others) polling our own address returns 0 (CEC NACK = "I own this
	// slot"), while polling another occupied address returns 1 (ACK from that
	// device). So PollDevice(our_addr) == 0, which is indistinguishable from
	// an unoccupied slot.
	//
	// Instead, use GetDevicePhysicalAddress across known device address pools.
	// libcec often returns our own physical address from internal EDID state
	// even when CEC loopback is unavailable. The first address in the recording
	// pool [1,2,3] or playback pool [4,8,11] that returns a valid physical
	// address in the same HDMI root-port as the amp — and is not the amp
	// itself — is our device.
	ampPhys := m.conn.GetDevicePhysicalAddress(cecAmpAddr)
	var ampRootPort int
	fmt.Sscanf(ampPhys, "%d.", &ampRootPort)
	for _, addr := range []int{1, 2, 3, 4, 8, 11} {
		phys := m.conn.GetDevicePhysicalAddress(addr)
		if phys == "" || phys == "f.f.f.f" {
			continue
		}
		if phys == ampPhys {
			continue // that's the amp, not us
		}
		var rootPort int
		fmt.Sscanf(phys, "%d.", &rootPort)
		if rootPort != ampRootPort {
			continue // different root port — not behind our amp
		}
		m.mu.Lock()
		m.ourLogAddr = addr
		m.ourPhysAddr = phys
		m.mu.Unlock()
		log.Printf("cec: our device (phys-scan): logical=%d physical=%s", addr, phys)
		return addr, phys
	}

	// Pass 4: hard-coded config fallback. Used on adapters (e.g. Raspberry Pi
	// VC4) where neither OSD name scan nor PollDevice works because libcec has
	// no loopback — our own address is invisible to discovery queries.
	// Set cec.fallback_log_addr and cec.fallback_phys_addr in config.yaml.
	m.mu.Lock()
	fb := m.fallbackLogAddr
	fbPhys := m.fallbackPhysAddr
	m.mu.Unlock()
	if fb >= 0 {
		m.mu.Lock()
		m.ourLogAddr = fb
		m.ourPhysAddr = fbPhys
		m.mu.Unlock()
		log.Printf("cec: our device (config-fallback): logical=%d physical=%s", fb, fbPhys)
		return fb, fbPhys
	}

	return -1, ""
}

// announceActiveSource sends a raw Active Source broadcast (CEC opcode 0x82),
// identical to what cec-client does with "tx XF:82:PP:PP".
//
// SetActiveSource is intentionally NOT used: libcec's implementation calls
// ActivateSource() internally, which also sends Image View On (0x04) to the
// TV, causing it to power on as a side effect. The raw Transmit only sends the
// Active Source frame with no additional commands.
func (m *CECManager) announceActiveSource() {
	// libcec allocates a logical address asynchronously after Open(); retry
	// for up to 5 s so we don't fail on fast paths (amp already on → no
	// polling loop).
	const maxAttempts = 5
	var logAddr int
	var physAddr string
	for i := 0; i < maxAttempts; i++ {
		logAddr, physAddr = m.findOurDevice()
		if logAddr >= 0 {
			break
		}
		log.Printf("cec: logical address not yet assigned (attempt %d/%d) — retrying…", i+1, maxAttempts)
		time.Sleep(1 * time.Second)
	}
	if logAddr < 0 {
		log.Printf("cec: could not find our logical address — skipping Active Source")
		return
	}
	phys := physAddrToHex(physAddr)

	// Routing Change (0x80): broadcast that the active routing path is
	// changing to our physical address. This is more explicit than Active
	// Source for amps that implement routing but not System Audio Control.
	// Original path = 00:00 (unknown/root → we don't know what was active).
	rc := fmt.Sprintf("%xf:80:00:00:%s", logAddr, phys)
	log.Printf("cec: routing change: %s", rc)
	m.conn.Transmit(rc)

	// Active Source (0x82): broadcast that we are the current source.
	cmd := fmt.Sprintf("%xf:82:%s", logAddr, phys)
	log.Printf("cec: announcing active source: %s", cmd)
	m.conn.Transmit(cmd)

	m.mu.Lock()
	m.setAsSource = true
	m.mu.Unlock()
}

// triggerStandby is fired by the standby timer. It only sends standby when we
// were the last device to announce active source AND no other device has since
// taken over.
func (m *CECManager) triggerStandby() {
	m.mu.Lock()
	wasSource := m.setAsSource
	if wasSource {
		m.setAsSource = false
	}
	m.standbyTimer = nil
	m.mu.Unlock()

	if !wasSource {
		log.Printf("cec: standby timer fired but we were not the active source — skipping")
		return
	}

	// Check if another source has become active since we last set ourselves.
	// Skip the amp (addr 5) and our own device — both are expected to show
	// ActiveSource=true and must not prevent standby.
	devices := m.conn.List()
	for _, d := range devices {
		if !d.ActiveSource || d.LogicalAddress == cecAmpAddr {
			continue
		}
		if strings.TrimRight(d.OSDName, "\x00 ") == "gobz-cec" {
			continue // that's us
		}
		log.Printf("cec: active source is now %q (logical %d) — skipping standby",
			d.OSDName, d.LogicalAddress)
		return
	}

	log.Printf("cec: source still ours — sending amplifier standby…")
	// Standby() sends the CEC Standby broadcast and the amp turns off, but
	// some libcec adapters return an error because the device stops responding
	// on the bus before libcec receives its own acknowledgement. Treat any
	// error as a warning only; the amp does go to standby in practice.
	if err := m.conn.Standby(cecAmpAddr); err != nil {
		log.Printf("cec: standby warning (amp likely off): %v", err)
	} else {
		log.Printf("cec: amplifier standby sent")
	}
}

// drainChannels consumes all libcec event channels so their goroutines never block.
func (m *CECManager) drainChannels() {
	for {
		select {
		case cmd, ok := <-m.conn.Commands:
			if !ok {
				return
			}
			log.Printf("cec: command: %s", cmd.CommandString)
			// Forward <Report Audio Status> (0x7A) to queryAmpVolume.
			if status, ok := parseAudioStatus(cmd.CommandString); ok {
				select {
				case m.audioStatusCh <- status:
				default:
				}
			}
		case msg, ok := <-m.conn.Messages:
			if !ok {
				return
			}
			_ = msg
		case sa, ok := <-m.conn.SourceActivations:
			if !ok {
				return
			}
			log.Printf("cec: source activation: %s (logical %d, active=%v)",
				sa.LogicalAddressName, sa.LogicalAddress, sa.State)
		case _, ok := <-m.conn.KeyPresses:
			if !ok {
				return
			}
		case _, ok := <-m.conn.MenuActivations:
			if !ok {
				return
			}
		}
	}
}

// applyVolume adjusts the CEC amp volume by the delta between newVol and the
// last Qobuz volume we applied. CEC has no absolute "set volume to N" command;
// the delta approach makes the amp track Qobuz changes directly.
//
// On the first call after play starts (volReset=true), the level is recorded
// without sending any CEC commands so the amp keeps the user's last volume.
//
// All key presses are sent without intermediate KeyRelease (simulating a held
// button). A single KeyRelease is sent at the end, avoiding per-step display
// flicker and letting the amp ramp at its own rate.
func (m *CECManager) applyVolume(newVol int) {
	m.mu.Lock()
	reset := m.volReset
	m.volReset = false
	m.mu.Unlock()

	m.volMu.Lock()
	defer m.volMu.Unlock()

	if reset || m.lastQobuzVol < 0 {
		log.Printf("cec: volume reference set to %d (amp keeps its current level)", newVol)
		m.lastQobuzVol = newVol
		return
	}

	delta := newVol - m.lastQobuzVol
	if delta == 0 {
		return
	}

	prevVol := m.lastQobuzVol
	m.lastQobuzVol = newVol
	log.Printf("cec: volume %d → %d (delta %+d)", prevVol, newVol, delta)

	key := 0x41 // Volume Up
	steps := delta
	if delta < 0 {
		key = 0x42 // Volume Down
		steps = -delta
	}

	for i := 0; i < steps; i++ {
		if err := m.conn.KeyPress(cecAmpAddr, key); err != nil {
			log.Printf("cec: volume key press failed at step %d/%d: %v", i+1, steps, err)
			// Correct the tracker: amp only received i out of steps presses.
			if delta > 0 {
				m.lastQobuzVol = prevVol + i
			} else {
				m.lastQobuzVol = prevVol - i
			}
			break
		}
	}
	m.conn.KeyRelease(cecAmpAddr)
}

// physAddrToHex converts a dotted CEC physical address (e.g. "1.0.0.0") to
// the two-byte hex representation used in raw CEC frames (e.g. "10:00").
// Each digit maps to one nibble: (A.B.C.D) → 0xAB 0xCD.
func physAddrToHex(addr string) string {
	var a, b, c, d int
	if _, err := fmt.Sscanf(addr, "%d.%d.%d.%d", &a, &b, &c, &d); err != nil {
		log.Printf("cec: cannot parse physical address %q: %v", addr, err)
		return "00:00"
	}
	return fmt.Sprintf("%02x:%02x", (a<<4)|b, (c<<4)|d)
}
