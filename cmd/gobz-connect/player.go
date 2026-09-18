package main

import (
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gopxl/beep/v2"
	"github.com/gopxl/beep/v2/flac"
	"github.com/gopxl/beep/v2/mp3"
)

// ---- Gapless queue -------------------------------------------------------

// gaplessTrack is a fully decoded, resampled track ready for gapless playback.
type gaplessTrack struct {
	queueTrack *QueueTrack
	streamer   beep.StreamSeekCloser // raw decoded frames
	format     beep.Format
	resampled  beep.Streamer // resampled to speaker SR (may equal streamer)
	nextID     int32         // NextQueueItemID at the time this track was loaded
}

// gaplessQueue implements beep.Streamer by chaining at most two tracks.
// When tracks[0] is exhausted it pops and fires onAdvance asynchronously,
// so the transition is truly gapless — no silence, no speakerPlay() call.
type gaplessQueue struct {
	tracks    []*gaplessTrack
	onAdvance func(prev, curr *gaplessTrack) // called in a new goroutine
}

// Stream fills samples from tracks[0]; when it's drained, advances to tracks[1].
// Silence is streamed when the queue is empty.
// Must be called while the speaker lock is held (beep invariant).
func (q *gaplessQueue) Stream(samples [][2]float64) (int, bool) {
	filled := 0
	for filled < len(samples) {
		if len(q.tracks) == 0 {
			for i := filled; i < len(samples); i++ {
				samples[i][0] = 0
				samples[i][1] = 0
			}
			break
		}
		n, ok := q.tracks[0].resampled.Stream(samples[filled:])
		if !ok {
			prev := q.tracks[0]
			q.tracks = q.tracks[1:]
			prev.streamer.Close() // streamer is exhausted; release the file/temp-file
			if q.onAdvance != nil {
				var curr *gaplessTrack
				if len(q.tracks) > 0 {
					curr = q.tracks[0]
				}
				go q.onAdvance(prev, curr)
			}
		}
		filled += n
	}
	return len(samples), true
}

func (q *gaplessQueue) Err() error { return nil }

// play sets or replaces the current track (slot 0).
// Must be called with speakerLock() held.
func (q *gaplessQueue) play(gt *gaplessTrack) {
	if len(q.tracks) == 0 {
		q.tracks = append(q.tracks, gt)
	} else {
		q.tracks[0].streamer.Close()
		q.tracks[0] = gt
	}
}

// enqueue sets the next track (slot 1).
// Must be called with speakerLock() held.
func (q *gaplessQueue) enqueue(gt *gaplessTrack) {
	if len(q.tracks) < 2 {
		q.tracks = append(q.tracks, gt)
	} else {
		q.tracks[1].streamer.Close()
		q.tracks[1] = gt
	}
}

// closeAll closes the streamers of all remaining tracks, releasing file handles.
// Must be called with speakerLock() held.
func (q *gaplessQueue) closeAll() {
	for _, gt := range q.tracks {
		gt.streamer.Close()
	}
	q.tracks = nil
}

// ---- Player ---------------------------------------------------------------

// Player streams audio from Qobuz CDN and plays via the default audio device.
type Player struct {
	api     *QobuzAPI
	queue   *Queue
	userID  string
	sendMsg func([]*PbQConnectMessage)
	cache   *TrackCache

	volume       atomic.Uint32 // 0–100
	pausedWant   atomic.Bool   // desired pause state; applied when each track starts
	running      atomic.Bool
	stopCh       chan struct{} // buffered(1); closed by Stop()
	stopWanted      atomic.Bool // set by Stop(); cleared at the start of each Run()
	queueExhausted  atomic.Bool // set when Run() exits because the queue ran out (not stopped)
	closeCh      chan struct{} // closed by Close() to stop the persistent heartbeat
	currentFormat atomic.Int32  // AudioFormat* constant for the currently playing file

	stateMu     sync.Mutex
	playerState PbQueueRendererState

	currentMu    sync.Mutex
	currentTrack *QueueTrack

	ctrl             *beep.Ctrl
	ctrlMu             sync.Mutex
	speakerSR          beep.SampleRate
	resampleQuality    int
	configSpeakerSR    int  // 0 = auto-detect
	adaptiveSampleRate bool // reinit speaker per track to avoid resampling
	// speakerAdaptFailed is set when speakerInit() fails inside
	// adaptSpeakerForTrack (beep/speaker v2 only allows one Init() per
	// process lifetime).  Once set, SR boundaries and reinit attempts are
	// skipped for the rest of the session; tracks are resampled instead.
	speakerAdaptFailed bool
}

// NewPlayer creates a Player and starts the persistent heartbeat goroutine.
func NewPlayer(api *QobuzAPI, queue *Queue, userID string) *Player {
	p := &Player{
		api:     api,
		queue:   queue,
		userID:  userID,
		stopCh:  make(chan struct{}, 1),
		closeCh: make(chan struct{}),
	}
	p.volume.Store(80)
	p.pausedWant.Store(true) // start paused; SrvrRndrSetState will play when ready

	// Heartbeat: send player state every 10 s regardless of playback state.
	// This must survive player Stop/Run cycles so the server never drops the
	// WS connection during a playlist switch.
	go func() {
		t := time.NewTicker(10 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				p.sendPlayerState()
			case <-p.closeCh:
				return
			}
		}
	}()

	return p
}

// Close stops the persistent heartbeat goroutine. Call when the Player is
// permanently discarded (e.g. on application shutdown).
func (p *Player) Close() {
	close(p.closeCh)
}

// SetSendMsg sets the callback used to broadcast state to the server.
func (p *Player) SetSendMsg(f func([]*PbQConnectMessage)) { p.sendMsg = f }

// SetCache attaches a TrackCache so playback prefers local files over HTTP.
func (p *Player) SetCache(tc *TrackCache) { p.cache = tc }

// SetResampleQuality sets the resampler quality (see config ResampleQuality).
func (p *Player) SetResampleQuality(q int) { p.resampleQuality = q }

// SetSpeakerSampleRate sets an explicit output sample rate. 0 means auto-detect.
func (p *Player) SetSpeakerSampleRate(sr int) { p.configSpeakerSR = sr }

// SetAdaptiveSampleRate enables or disables per-track speaker reinitialisation.
func (p *Player) SetAdaptiveSampleRate(v bool) { p.adaptiveSampleRate = v }

// SetVolume sets playback volume (0–100).
func (p *Player) SetVolume(v uint32) {
	if v > 100 {
		v = 100
	}
	p.volume.Store(v)
}

// Pause pauses or resumes playback. Safe to call before a track starts; the
// desired state is stored in pausedWant and applied when the next ctrl starts.
func (p *Player) Pause(paused bool) {
	p.pausedWant.Store(paused)

	p.ctrlMu.Lock()
	ctrl := p.ctrl
	p.ctrlMu.Unlock()

	// Only attempt ALSA suspend/resume when speakerPlay() is active (ctrl !=
	// nil). Calling Resume/Suspend before Play is called, or after speakerClear(),
	// makes ALSA return EINVAL because no PCM stream is open. Also guard with
	// speakerSuspended so we only Resume if we actually Suspended before, and
	// only Suspend if we are not already suspended — some drivers reject a
	// double-suspend or a resume-without-suspend with the same EINVAL.
	// Guard all Suspend/Resume calls: the oto context is nil until
	// speakerInit() succeeds (which may be deferred to the first track when
	// adaptive sample rate is enabled). Calling Suspend/Resume with a nil
	// context panics inside the beep/speaker package.
	speakerReady := speakerInitialized.Load()

	if !paused && ctrl != nil {
		if speakerSuspended.Load() && speakerReady {
			if err := speakerResume(); err != nil {
				log.Printf("player: speaker resume: %v", err)
			} else {
				speakerSuspended.Store(false)
			}
		}
		// Audio is resuming successfully: reset the consecutive-failure counter.
		// Suspend failures are often transient (ALSA write-goroutine error
		// surfaced through the Suspend() path, XRUN race, etc.); if playback
		// resumes correctly the device is not fundamentally incompatible with
		// suspend and the counter should not accumulate across pause/resume cycles.
		speakerSuspendFails.Store(0)
	}

	if ctrl != nil {
		speakerLock()
		ctrl.Paused = paused
		speakerUnlock()
	}

	if paused && ctrl != nil && speakerReady && !speakerSuspended.Load() && !speakerSuspendDisabled.Load() {
		if err := speakerSuspend(); err != nil {
			recordSuspendFailure("pause", err)
		} else {
			speakerSuspendFails.Store(0)
			speakerSuspended.Store(true)
		}
	}
}

// IsRunning reports whether the player goroutine is active.
func (p *Player) IsRunning() bool { return p.running.Load() }

// CurrentTrack returns the track currently playing.
func (p *Player) CurrentTrack() *QueueTrack {
	p.currentMu.Lock()
	defer p.currentMu.Unlock()
	return p.currentTrack
}

// Stop signals the player to stop. The goroutine exits asynchronously.
func (p *Player) Stop() {
	p.stopWanted.Store(true)
	select {
	case p.stopCh <- struct{}{}:
	default:
	}
}

// PrepareRestart must be called before Restart() when switching to a different
// track. It immediately clears currentTrack and primes playerState with the
// target queueItemID so that any SrvrRndrSetState messages that arrive while
// the player is restarting see cur==nil (no track yet) instead of the stale
// old track, preventing an infinite restart loop.
func (p *Player) PrepareRestart(targetQueueItemID int32, posMs uint32) {
	p.currentMu.Lock()
	p.currentTrack = nil
	p.currentMu.Unlock()
	p.PrimeState(targetQueueItemID, posMs)
}

// Restart stops the running player and starts it again once it has fully
// exited. If the player is not running it just starts it. Safe to call
// concurrently; only one Run() will win the CompareAndSwap.
func (p *Player) Restart() {
	go func() {
		if p.running.Load() {
			p.Stop()
			for p.running.Load() {
				time.Sleep(1 * time.Millisecond)
			}
		}
		p.Run()
	}()
}

// adaptSpeakerForTrack reinitialises the ALSA speaker at the track's native
// sample rate when it differs from the current rate, eliminating on-the-fly
// resampling.  Resampling 44.1 kHz → 96 kHz is too CPU-intensive for a
// Raspberry Pi; by matching the speaker SR to the track SR the resampler is
// bypassed entirely for the common case.
//
// The rate is capped at configSpeakerSR when set (respects the user's
// max-quality cap).  Must be called only when the gapless queue is empty
// (before the first track of each Run() cycle).  Returns the SR to use for
// loading the track.
func (p *Player) adaptSpeakerForTrack(trackSR beep.SampleRate) beep.SampleRate {
	wantSR := trackSR
	if p.configSpeakerSR > 0 && int(wantSR) > p.configSpeakerSR {
		wantSR = beep.SampleRate(p.configSpeakerSR)
	}
	if wantSR == p.speakerSR {
		return wantSR
	}
	log.Printf("player: track SR=%d, speaker SR=%d — reiniting speaker at %d Hz to avoid resampling",
		trackSR, p.speakerSR, wantSR)
	// The gapless queue is empty so no audio is in flight; Clear() is safe.
	speakerClear()
	if err := speakerReinit(wantSR, wantSR.N(200*time.Millisecond)); err != nil {
		// beep/speaker v2 wraps oto, whose context cannot be recreated once
		// opened ("speaker cannot be initialized more than once"). Mark the
		// failure so runLoop stops attempting SR boundaries and reinits for
		// the rest of this session; tracks will be resampled instead.
		log.Printf("player: speaker reinit at %d Hz: %v — resampling will be used for SR changes", wantSR, err)
		p.speakerAdaptFailed = true
		// Recover: re-register ctrl with the mixer at the old rate.
		p.ctrlMu.Lock()
		ctrl := p.ctrl
		p.ctrlMu.Unlock()
		if ctrl != nil {
			speakerPlay(ctrl)
		}
		return p.speakerSR
	}
	speakerMu.Lock()
	speakerInitSR = wantSR
	speakerMu.Unlock()
	speakerSuspended.Store(false) // reinit resets PCM suspend state
	p.speakerSR = wantSR
	p.ctrlMu.Lock()
	ctrl := p.ctrl
	p.ctrlMu.Unlock()
	if ctrl != nil {
		speakerLock()
		ctrl.Paused = p.pausedWant.Load()
		speakerUnlock()
		speakerPlay(ctrl)
	}
	return wantSR
}

// ScheduleAudioReinit schedules a full speaker reinitialization at the start
// of the next Run() cycle and restarts the player. Call this after an HDMI
// hotplug event (e.g. the amp powers on while gobz-connect is already running)
// so the new ALSA device state is picked up cleanly.
func (p *Player) ScheduleAudioReinit() {
	log.Printf("player: scheduling audio reinit (HDMI device change)")
	speakerReinitPending.Store(true)
	p.Restart()
}

// CurrentPositionMs returns the estimated current playback position in milliseconds.
func (p *Player) CurrentPositionMs() uint64 {
	p.stateMu.Lock()
	defer p.stateMu.Unlock()
	pos := p.playerState.CurrentPosition
	if pos == nil {
		return 0
	}
	v := uint64(pos.Value)
	if p.playerState.PlayingState == PlayingStatePlaying && pos.Timestamp != 0 {
		now := uint64(time.Now().UnixMilli())
		if now > pos.Timestamp {
			v += now - pos.Timestamp
		}
	}
	return v
}

// RequestSkipTo requests a seek to a position in milliseconds.
func (p *Player) RequestSkipTo(posMs int64) {
	p.currentMu.Lock()
	t := p.currentTrack
	p.currentMu.Unlock()
	if t != nil {
		t.SkipTo.Store(posMs)
		t.WantSkip.Store(true)
	}
}

var (
	speakerMu              sync.Mutex
	speakerInitDone        bool            // protected by speakerMu; true once speaker.Init has succeeded
	speakerReinitPending   atomic.Bool     // set by ScheduleAudioReinit; cleared at the start of Run
	speakerInitSR          beep.SampleRate // sample rate actually used when speaker was initialised
	speakerInitialized     atomic.Bool     // true once speaker.Init has succeeded
	speakerSuspended       atomic.Bool     // true when we successfully called speakerSuspend()
	speakerSuspendDisabled atomic.Bool     // set after 3 consecutive Suspend failures; skips all future calls
	speakerSuspendFails    atomic.Int32    // consecutive Suspend() failure count; resets on success or Run() start
)

// recordSuspendFailure logs a speakerSuspend() error and permanently disables
// suspend only after 3 consecutive failures. A single failure is often a
// transient ALSA XRUN race (the PCM enters XRUN state briefly during heavy
// disk I/O at track load time; snd_pcm_pause returns EINVAL from XRUN state).
// Disabling on the first failure would leave the ALSA driver spinning at ~8%
// CPU for the rest of the session on every subsequent pause.
func recordSuspendFailure(ctx string, err error) {
	n := speakerSuspendFails.Add(1)
	if n >= 3 {
		log.Printf("player: speaker suspend (%s): %v — %d consecutive failures, disabling suspend for this session", ctx, err, n)
		speakerSuspendDisabled.Store(true)
	} else {
		log.Printf("player: speaker suspend (%s): %v — transient failure (%d/3), will retry on next pause", ctx, err, n)
	}
}

// determineSpeakerSR returns the sample rate to use for speaker initialisation.
// Config override wins; otherwise the platform auto-detection is used; as a
// last resort 44100 Hz is returned.
func (p *Player) determineSpeakerSR() beep.SampleRate {
	if p.configSpeakerSR > 0 {
		log.Printf("player: audio output sample rate: %d Hz (config override)", p.configSpeakerSR)
		return beep.SampleRate(p.configSpeakerSR)
	}
	if sr := detectDeviceSampleRate(); sr > 0 {
		log.Printf("player: audio output sample rate: %d Hz (auto-detected)", sr)
		return beep.SampleRate(sr)
	}
	log.Printf("player: audio output sample rate: 44100 Hz (detection failed, using default)")
	return beep.SampleRate(44100)
}

// Run is the main player goroutine. Safe to call multiple times; extras return
// immediately. A single speaker.Play call is made; the gaplessQueue streams
// continuously, advancing tracks with no silence between them.
func (p *Player) Run() {
	if !p.running.CompareAndSwap(false, true) {
		return
	}
	defer p.running.Store(false)

	// Clear any stale stop state left by a previous Run() cycle, then drain
	// the channel token. This handles two races:
	// (a) previous Run() exited via queue exhaustion before Stop() was called —
	//     the channel has a leftover token that would kill the next runLoop.
	// (b) ConsumeTrack consumed the channel token on behalf of Stop() — the
	//     channel is empty but stopWanted is true; clearing it here prevents
	//     a false-stop detection in the new runLoop.
	p.stopWanted.Store(false)
	p.queueExhausted.Store(false)
	p.speakerAdaptFailed = false // reset per session; speaker.Init failure is sticky within a session
	select {
	case <-p.stopCh:
		log.Printf("player: drained stale stop signal at Run() start")
	default:
	}

	// runDone is closed when Run() exits, stopping background goroutines that
	// are scoped to a single playback session (none currently — the heartbeat
	// now lives in NewPlayer and survives Stop/Run cycles).
	runDone := make(chan struct{})
	defer close(runDone)

	// Reset the consecutive suspend-failure counter on every new playback session.
	// Suspend() failures are often transient (ALSA XRUN at startup); accumulating
	// them across unrelated Run() cycles would disable suspend prematurely.
	speakerSuspendFails.Store(0)

	// Check if a reinit was requested (e.g. HDMI hotplug when the amp powers on
	// after gobz-connect started with the amp off). Reset the init state so
	// speaker.Init is called again with fresh ALSA device parameters.
	speakerMu.Lock()
	if speakerReinitPending.Load() {
		speakerReinitPending.Store(false)
		speakerInitDone = false
		speakerInitialized.Store(false)
		speakerSuspended.Store(false)
		speakerSuspendDisabled.Store(false) // fresh device — allow suspend again
		log.Printf("player: audio reinit triggered — reinitialising speaker")
	}
	// Record whether the speaker was already running before this Run() call.
	// On first init (and on reinit), ALSA is not ready for an immediate
	// Suspend() right after Play() — the hw_params reconfiguration inside
	// oto/ALSA fails and leaves the PCM stream broken, silencing all
	// subsequent audio output. We skip the post-Play suspend/resume block and
	// let Pause() handle it once ALSA has had a chance to start streaming.
	wasAlreadyInitialized := speakerInitDone
	// When adaptive sample rate is enabled and this is the very first
	// speaker initialisation (fresh program start or HDMI reinit), defer
	// speakerInit() to runLoop so the sample rate can be set to the first
	// track's native rate — eliminating resampling for the most common case.
	// On subsequent Run() cycles (speakerInitDone=true), the oto context is
	// already locked to a rate that cannot be changed; fall through to the
	// normal path and let adaptSpeakerForTrack handle it gracefully.
	deferSpeakerInit := p.adaptiveSampleRate && !speakerInitDone
	var speakerErr error
	if !deferSpeakerInit && !speakerInitDone {
		wantSR := p.determineSpeakerSR()
		speakerInitSR = wantSR
		speakerErr = speakerInit(wantSR, wantSR.N(200*time.Millisecond))
		if speakerErr == nil {
			speakerInitDone = true
		}
	}
	speakerMu.Unlock()
	if speakerErr != nil {
		log.Printf("player: speaker init: %v", speakerErr)
		return
	}
	if !deferSpeakerInit {
		p.speakerSR = speakerInitSR
		speakerInitialized.Store(true)
	}

	gq := &gaplessQueue{}

	// advanceCh is signalled whenever gq pops a track, unblocking runLoop.
	advanceCh := make(chan struct{}, 2)
	gq.onAdvance = func(prev, curr *gaplessTrack) {
		p.handleTrackAdvance(prev, curr)
		select {
		case advanceCh <- struct{}{}:
		default:
		}
	}

	volStreamer := &volumeStreamer{Streamer: gq, vol: &p.volume}
	ctrl := &beep.Ctrl{Streamer: volStreamer}
	p.ctrlMu.Lock()
	p.ctrl = ctrl
	p.ctrlMu.Unlock()

	if !deferSpeakerInit {
		speakerLock()
		ctrl.Paused = p.pausedWant.Load()
		speakerUnlock()

		// Single Play call — gq never returns ok=false, so the speaker runs
		// forever until speakerClear() is called.
		speakerPlay(ctrl)

		// Suspend or resume the ALSA driver based on initial pause state.
		// Suspension stops the driver goroutine from spinning through silence,
		// cutting idle CPU from ~8% to ~0% on a Raspberry Pi 3.
		// Guard with speakerSuspended to avoid double-suspend / resume-without-
		// suspend, which cause EINVAL on some ALSA drivers.
		//
		// Skip on first init (wasAlreadyInitialized=false): ALSA is not yet
		// ready for hw_params reconfiguration right after Play(). Calling
		// Suspend() immediately after the very first Init()+Play() corrupts the
		// PCM stream and silences audio. Pause() will do it once ALSA is
		// streaming normally.
		if wasAlreadyInitialized {
			if ctrl.Paused && !speakerSuspended.Load() && !speakerSuspendDisabled.Load() {
				if err := speakerSuspend(); err != nil {
					recordSuspendFailure("start", err)
				} else {
					speakerSuspendFails.Store(0)
					speakerSuspended.Store(true)
				}
			} else if !ctrl.Paused && speakerSuspended.Load() {
				if err := speakerResume(); err != nil {
					log.Printf("player: speaker resume (start): %v", err)
				} else {
					speakerSuspended.Store(false)
				}
			}
		}
	}

	// Seek poller: apply pending seeks every 50 ms.
	go func() {
		t := time.NewTicker(50 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				p.handleSeekPoll(gq)
			case <-runDone:
				return
			}
		}
	}()

	p.runLoop(gq, advanceCh, p.speakerSR, deferSpeakerInit)

	// If runLoop exited because the queue ran out (not because Stop() was
	// called), mark the queue as exhausted and send a stopped state.  This
	// prevents RendererStateUpdated echoes of our last "playing" heartbeat
	// from triggering an unwanted restart of the last track.
	if !p.stopWanted.Load() {
		p.queueExhausted.Store(true)
		p.stateMu.Lock()
		p.playerState.PlayingState = PlayingStateStopped
		p.playerState.BufferState = BufferStateUnknown
		p.stateMu.Unlock()
		log.Printf("player: queue exhausted — sending stopped state")
		go p.sendPlayerState()
	}

	speakerClear()
	speakerLock()
	gq.closeAll()
	speakerUnlock()
	// Do NOT call speakerSuspend() here: Clear() has already torn down the
	// PCM stream, and on some ALSA drivers (e.g. Raspberry Pi) calling
	// Suspend() after Clear() fails with snd_pcm_hw_params_set_format EINVAL,
	// which would permanently disable suspend for the session and break the
	// pause-path suspend that actually saves CPU during long pauses.
	p.ctrlMu.Lock()
	p.ctrl = nil
	p.ctrlMu.Unlock()
}

// runLoop loads and enqueues tracks into gq until the queue is empty or Stop()
// is called.  It always tries to keep slot[1] filled so that the transition
// from the current track to the next is gapless.
func (p *Player) runLoop(gq *gaplessQueue, advanceCh <-chan struct{}, speakerSR beep.SampleRate, deferSpeakerInit bool) {
	var prevQT *QueueTrack

	for {
		// Non-blocking stop check before blocking on ConsumeTrack.
		select {
		case <-p.stopCh:
			return
		default:
		}

		qt, nextID := p.queue.ConsumeTrack(prevQT, p.stopCh)
		if qt == nil {
			// Was this a stop signal or queue exhaustion?
			// Check stopWanted first: ConsumeTrack selects on stopCh internally
			// (inside its sleep() helper), so it may have already consumed the
			// channel token before we reach this non-blocking check.  Without
			// the atomic flag, runLoop would fall through to the "queue
			// exhausted" drain and wait for the current track to finish
			// playing, keeping Restart() blocked for the rest of the track.
			if p.stopWanted.Load() {
				return
			}
			select {
			case <-p.stopCh:
				return
			default:
			}
			// Queue exhausted — wait for the gapless queue to finish playing
			// before returning so speakerClear() doesn't cut off buffered tracks.
			for {
				if p.stopWanted.Load() {
					return
				}
				speakerLock()
				n := len(gq.tracks)
				speakerUnlock()
				if n == 0 {
					break
				}
				select {
				case <-advanceCh:
				case <-p.stopCh:
					return
				case <-time.After(200 * time.Millisecond):
				}
			}
			return
		}

		// Non-blocking stop check after ConsumeTrack unblocks.
		if p.stopWanted.Load() {
			return
		}
		select {
		case <-p.stopCh:
			return
		default:
		}

		if qt.State == TrackFailed {
			prevQT = qt
			continue
		}

		// For slot 0 (no track currently playing): immediately tell the
		// Qobuz client we have acknowledged the new track before we spend
		// time downloading it.  C++ achieves the same effect because its
		// streaming pipeline calls state_callback(st==1) within milliseconds
		// of opening the HTTP connection (first audio bytes arrive), whereas
		// Go downloads the full file before handing it to the decoder.
		// Without this, the client spins for the entire download duration
		// and eventually shows "renderer unreachable".
		speakerLock()
		isFirst := len(gq.tracks) == 0
		speakerUnlock()

		// SR boundary: when the next track has a different effective sample
		// rate than the current speaker, wait for the gapless queue to drain
		// then reinitialise the audio device.  This eliminates resampling at
		// the cost of a brief silence (~PCM drain time, typically < 200 ms).
		// Only active on platforms where speakerReinit is supported (Linux).
		if p.adaptiveSampleRate && speakerReinitSupported() && !isFirst && qt.SamplingRate > 0 {
			effectiveSR := beep.SampleRate(qt.SamplingRate)
			if p.configSpeakerSR > 0 && int(effectiveSR) > p.configSpeakerSR {
				effectiveSR = beep.SampleRate(p.configSpeakerSR)
			}
			if effectiveSR != speakerSR {
				log.Printf("player: SR boundary %d Hz → %d Hz — waiting for current track to finish",
					speakerSR, qt.SamplingRate)
				for {
					speakerLock()
					empty := len(gq.tracks) == 0
					speakerUnlock()
					if empty {
						break
					}
					select {
					case <-advanceCh:
					case <-p.stopCh:
						return
					case <-time.After(200 * time.Millisecond):
					}
				}
				isFirst = true // treat next track as first so isFirst block reinits
			}
		}

		if isFirst {
			p.currentMu.Lock()
			p.currentTrack = qt
			p.currentMu.Unlock()
			p.sendBufferingState(qt)

			if p.adaptiveSampleRate && qt.SamplingRate > 0 {
				if deferSpeakerInit {
					// First-ever speakerInit() for this process: set the SR to
					// the track's native rate so no resampling is needed.
					sr := beep.SampleRate(qt.SamplingRate)
					if p.configSpeakerSR > 0 && int(sr) > p.configSpeakerSR {
						sr = beep.SampleRate(p.configSpeakerSR)
					}
					speakerMu.Lock()
					if err := speakerInit(sr, sr.N(200*time.Millisecond)); err != nil {
						speakerMu.Unlock()
						log.Printf("player: deferred speaker init at %d Hz: %v", sr, err)
						return
					}
					speakerInitDone = true
					speakerInitSR = sr
					speakerMu.Unlock()
					p.speakerSR = sr
					speakerSR = sr
					speakerInitialized.Store(true)
					speakerSuspended.Store(false)
					// Start the speaker now (was deferred from Run()).
					// wasAlreadyInitialized was false, so we skip initial
					// suspend/resume here — Pause() will handle it once
					// the PCM stream is running.
					p.ctrlMu.Lock()
					ctrl := p.ctrl
					p.ctrlMu.Unlock()
					if ctrl != nil {
						speakerLock()
						ctrl.Paused = p.pausedWant.Load()
						speakerUnlock()
						speakerPlay(ctrl)
					}
					deferSpeakerInit = false
				} else if !p.speakerAdaptFailed {
					// Speaker already initialized; try to reinit at the
					// track's native SR. Will fail if oto context is locked
					// (sets speakerAdaptFailed so we don't retry).
					speakerSR = p.adaptSpeakerForTrack(beep.SampleRate(qt.SamplingRate))
				}
			}
		}

		gt, err := p.loadGaplessTrack(qt, nextID, speakerSR)
		prevQT = qt
		if err != nil {
			// FLAC failure → retry as MP3.
			if qt.Format != AudioFormatMP3 {
				qt.Format = AudioFormatMP3
				if p.queue.fetchFileURL(qt) {
					// The MP3 fallback is typically 44100 Hz. If the speaker was
					// just reinited for a hi-res FLAC that failed to download
					// (e.g. 96 kHz → unexpected EOF), re-adapt now so the MP3
					// plays at its native rate instead of going through an
					// expensive upsample resampler (44100→96000 on a Pi causes
					// CPU-driven ALSA underruns → burst audio).
					if isFirst && p.adaptiveSampleRate && !p.speakerAdaptFailed &&
						qt.SamplingRate > 0 && beep.SampleRate(qt.SamplingRate) != speakerSR {
						speakerSR = p.adaptSpeakerForTrack(beep.SampleRate(qt.SamplingRate))
					}
					gt, err = p.loadGaplessTrack(qt, nextID, speakerSR)
				}
			}
			if err != nil {
				log.Printf("player: load track %d: %v", qt.ID, err)
				continue
			}
		}

		speakerLock()
		n := len(gq.tracks)
		speakerUnlock()

		if n == 0 {
			// No track is currently playing — start this one immediately.
			startMs := qt.StartMs
			// Discard a start position that is at or beyond the track's end.
			// The server sometimes echoes a stale position from the previous
			// playlist (e.g. after QueueTracksLoaded); seeking past EOF makes
			// the FLAC decoder return immediately, skipping the track entirely.
			if startMs > 0 && qt.DurationMs > 0 && startMs >= qt.DurationMs {
				log.Printf("player: ignoring startMs %dms >= duration %dms for track %d — starting from beginning",
					startMs, qt.DurationMs, qt.ID)
				startMs = 0
			}
			if startMs > 0 {
				samplePos := gt.format.SampleRate.N(time.Duration(startMs) * time.Millisecond)
				if serr := gt.streamer.Seek(samplePos); serr != nil {
					log.Printf("player: seek to startMs %dms: %v", startMs, serr)
				}
			}
			qt.StartedPlayingAt = uint64(time.Now().UnixMilli())
			go p.api.ReportStreamingStart(p.userID, qt.ID, qt.Format)

			// currentTrack was already set before the download; refresh in
			// case a Restart() race swapped it, then send the final PLAYING
			// state now that we have duration and format info.
			p.currentMu.Lock()
			p.currentTrack = qt
			p.currentMu.Unlock()

			p.setPlayerState(qt, gt, uint32(startMs))

			speakerLock()
			gq.play(gt)
			speakerUnlock()
		} else {
			// Slot[0] is occupied; wait until slot[1] is free, then enqueue.
			for {
				speakerLock()
				n = len(gq.tracks)
				speakerUnlock()
				if n < 2 {
					break
				}
				select {
				case <-advanceCh:
				case <-p.stopCh:
					return
				case <-time.After(100 * time.Millisecond):
				}
			}

			speakerLock()
			gq.enqueue(gt)
			speakerUnlock()

			// Let the server know which track is queued next.
			p.stateMu.Lock()
			p.playerState.NextQueueItemID = int32(qt.Index)
			p.stateMu.Unlock()
		}
	}
}

// handleTrackAdvance is called asynchronously when gq transitions from prev to curr.
func (p *Player) handleTrackAdvance(prev, curr *gaplessTrack) {
	if prev != nil {
		qt := prev.queueTrack
		if err := prev.streamer.Err(); err != nil {
			log.Printf("player: track %d stopped early (download/decode error): %v — advancing to next track", qt.ID, err)
		}
		playedSec := int((time.Now().UnixMilli() - int64(qt.StartedPlayingAt)) / 1000)
		go p.api.ReportStreamingEnd(p.userID, qt.ID, qt.Blob, qt.ContextUUID,
			playedSec, qt.StartedPlayingAt)
	}
	if curr == nil {
		return
	}
	qt := curr.queueTrack
	qt.StartedPlayingAt = uint64(time.Now().UnixMilli())
	go p.api.ReportStreamingStart(p.userID, qt.ID, qt.Format)

	p.currentMu.Lock()
	p.currentTrack = qt
	p.currentMu.Unlock()

	p.setPlayerState(qt, curr, 0)
}

// PrimeState seeds the player's internal state with the server-reported track
// and position, then sends one immediate heartbeat.  Call this right before
// go player.Run() so that the heartbeat goroutine (which starts inside Run()
// before speaker.Init) has a valid queue-item ID and position to report to
// the server during the audio-device initialisation phase.
func (p *Player) PrimeState(currentQueueItemID int32, posMs uint32) {
	nowMs := uint64(time.Now().UnixMilli())
	playState := int32(PlayingStatePlaying)
	if p.pausedWant.Load() {
		playState = PlayingStatePaused
	}
	p.stateMu.Lock()
	p.playerState = PbQueueRendererState{
		PlayingState:       playState,
		BufferState:        BufferStateBuffering,
		CurrentQueueItemID: currentQueueItemID,
		NextQueueItemID:    -1,
		CurrentPosition: &PbPosition{
			Timestamp: nowMs,
			Value:     posMs,
		},
	}
	p.stateMu.Unlock()
	go p.sendPlayerState()
}

// sendBufferingState immediately broadcasts a BUFFERING state for qt so the
// Qobuz client knows the renderer is alive and loading the track.  It is
// called before the download starts; setPlayerState is called again once
// the track is fully loaded and playing.
func (p *Player) sendBufferingState(qt *QueueTrack) {
	p.queue.mu.Lock()
	var qv *PbQueueVersion
	if p.queue.QueueState != nil {
		qv = p.queue.QueueState.QueueVersion
	}
	p.queue.mu.Unlock()

	nowMs := uint64(time.Now().UnixMilli())
	playState := int32(PlayingStatePlaying)
	if p.pausedWant.Load() {
		playState = PlayingStatePaused
	}

	p.stateMu.Lock()
	p.playerState = PbQueueRendererState{
		PlayingState:       playState,
		BufferState:        BufferStateBuffering,
		CurrentQueueItemID: int32(qt.Index),
		NextQueueItemID:    -1,
		Duration:           uint32(qt.DurationMs),
		QueueVersion:       qv,
		CurrentPosition: &PbPosition{
			Timestamp: nowMs,
			Value:     uint32(qt.StartMs),
		},
	}
	p.stateMu.Unlock()
	go p.sendPlayerState()
}

// formatToQualityValue converts an AudioFormat* constant to the quality level
// used by the QConnect protocol (same 1–4 scale as DeviceCapabilities.maxAudioQuality
// and SrvrRndrSetMaxAudioQuality.maxAudioQuality).
// This is the inverse of stream.go's qualityLevelToFormat.
func formatToQualityValue(format int) int32 {
	switch format {
	case AudioFormatFLAC:
		return 2
	case AudioFormatHiRes96:
		return 3
	case AudioFormatHiRes192:
		return 4
	default: // AudioFormatMP3 or unknown
		return 1
	}
}

// audioQualityProperties returns the (samplingRate, bitDepth, nbChannels) for a given
// AudioFormat constant. These values are required by RndrSrvrFileAudioQualityChanged
// and RndrSrvrDeviceAudioQualityChanged proto messages.
func audioQualityProperties(format int) (samplingRate, bitDepth, nbChannels int32) {
	switch format {
	case AudioFormatHiRes192:
		return 192000, 24, 2
	case AudioFormatHiRes96:
		return 96000, 24, 2
	case AudioFormatFLAC:
		return 44100, 16, 2
	default: // AudioFormatMP3
		return 44100, 16, 2
	}
}

// setPlayerState updates playerState for the given track and sends it.
func (p *Player) setPlayerState(qt *QueueTrack, gt *gaplessTrack, startVal uint32) {
	p.queue.mu.Lock()
	var qv *PbQueueVersion
	if p.queue.QueueState != nil {
		qv = p.queue.QueueState.QueueVersion
	}
	p.queue.mu.Unlock()

	nowMs := uint64(time.Now().UnixMilli())
	playState := int32(PlayingStatePlaying)
	if p.pausedWant.Load() {
		playState = PlayingStatePaused
	}

	p.stateMu.Lock()
	p.playerState = PbQueueRendererState{
		PlayingState:       playState,
		BufferState:        BufferStateOK,
		CurrentQueueItemID: int32(qt.Index),
		NextQueueItemID:    gt.nextID,
		Duration:           uint32(qt.DurationMs),
		QueueVersion:       qv,
		CurrentPosition: &PbPosition{
			Timestamp: nowMs,
			Value:     startVal,
		},
	}
	p.stateMu.Unlock()
	p.currentFormat.Store(int32(qt.Format))
	log.Printf("player: setPlayerState trackID=%d format=%d qualityValue=%d",
		qt.ID, qt.Format, formatToQualityValue(qt.Format))
	go p.sendPlayerStateWithQuality()
}

// handleSeekPoll applies a pending seek on the current track if WantSkip is set.
func (p *Player) handleSeekPoll(gq *gaplessQueue) {
	p.currentMu.Lock()
	qt := p.currentTrack
	p.currentMu.Unlock()

	if qt == nil || !qt.WantSkip.Load() {
		return
	}
	seekMs := uint64(qt.SkipTo.Load())
	qt.WantSkip.Store(false)

	speakerLock()
	if len(gq.tracks) > 0 {
		samplePos := gq.tracks[0].format.SampleRate.N(time.Duration(seekMs) * time.Millisecond)
		if err := gq.tracks[0].streamer.Seek(samplePos); err != nil {
			log.Printf("player: seek to %dms: %v", seekMs, err)
		}
	}
	speakerUnlock()

	nowMs := uint64(time.Now().UnixMilli())
	p.stateMu.Lock()
	// Preserve the current playing/paused state — C++ doesn't change it on seek.
	p.playerState.CurrentPosition = &PbPosition{Timestamp: nowMs, Value: uint32(seekMs)}
	p.stateMu.Unlock()
	go p.sendPlayerState()
}

// loadGaplessTrack downloads and decodes a track into a gaplessTrack.
func (p *Player) loadGaplessTrack(qt *QueueTrack, nextID int32, speakerSR beep.SampleRate) (*gaplessTrack, error) {
	rc, err := p.fetchTrackData(qt)
	if err != nil {
		return nil, err
	}

	// rc is owned by the decoder after a successful Decode call.
	// On decode error, beep closes rc automatically via defer.
	var streamer beep.StreamSeekCloser
	var format beep.Format

	if qt.Format >= AudioFormatFLAC {
		streamer, format, err = flac.Decode(rc)
	} else {
		streamer, format, err = mp3.Decode(rc)
	}
	if err != nil {
		return nil, err
	}

	var resampled beep.Streamer = streamer
	if format.SampleRate != speakerSR {
		q := p.resampleQuality
		if q <= 0 {
			q = 4
		}
		resampled = beep.Resample(q, format.SampleRate, speakerSR, streamer)
	}

	return &gaplessTrack{
		queueTrack: qt,
		streamer:   streamer,
		format:     format,
		resampled:  resampled,
		nextID:     nextID,
	}, nil
}

// fetchTrackData opens the audio file for the track as a seekable stream.
// Cache hit: returns the cached file opened for reading (caller must close).
// Cache miss: downloads to a temp file and returns it (deleted on Close).
// Memory usage is O(1) — the file is read lazily by the decoder.
func (p *Player) fetchTrackData(track *QueueTrack) (io.ReadSeekCloser, error) {
	if p.cache != nil {
		return p.cache.Open(track)
	}
	// No cache configured: download to a temp file.
	resp, err := openCDN(track.FileURL)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	f, err := os.CreateTemp("", "qobuz-track-*.tmp")
	if err != nil {
		return nil, fmt.Errorf("player: temp file: %w", err)
	}
	if _, err := io.Copy(f, resp.Body); err != nil {
		f.Close()
		os.Remove(f.Name())
		return nil, fmt.Errorf("player: download track %d: %w", track.ID, err)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		f.Close()
		os.Remove(f.Name())
		return nil, fmt.Errorf("player: seek temp file: %w", err)
	}
	return &tempTrackFile{f}, nil
}

// ---- Helpers --------------------------------------------------------------

// tempTrackFile wraps *os.File for a CDN temp download; deletes the file on Close.
type tempTrackFile struct{ *os.File }

func (t *tempTrackFile) Close() error {
	name := t.File.Name()
	err := t.File.Close()
	os.Remove(name)
	return err
}

// volumeStreamer multiplies every sample by vol/100, giving a live 0–100 gain.
type volumeStreamer struct {
	beep.Streamer
	vol *atomic.Uint32
}

func (v *volumeStreamer) Stream(samples [][2]float64) (n int, ok bool) {
	n, ok = v.Streamer.Stream(samples)
	gain := float64(v.vol.Load()) / 100.0
	for i := range samples[:n] {
		samples[i][0] *= gain
		samples[i][1] *= gain
	}
	return
}

// openCDN issues a plain HTTP GET for the full track file.
func openCDN(url string) (*http.Response, error) {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "audio/*")
	req.Header.Set("Accept-Encoding", "identity")
	req.Header.Set("User-Agent", "gobz-connect/1.0")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != 200 && resp.StatusCode != 206 {
		resp.Body.Close()
		return nil, errors.New("HTTP " + resp.Status)
	}
	return resp, nil
}

// sendPlayerState broadcasts the current player state over WebSocket.
func (p *Player) sendPlayerState() { p.sendState(false) }

// sendPlayerStateWithQuality broadcasts state plus the file audio quality
// message. Called only when the track changes so the Qobuz client updates its
// quality indicator exactly once per track, not on every heartbeat or seek.
func (p *Player) sendPlayerStateWithQuality() { p.sendState(true) }

func (p *Player) sendState(withQuality bool) {
	if p.sendMsg == nil {
		return
	}
	p.stateMu.Lock()
	state := p.playerState
	p.stateMu.Unlock()

	nowMs := uint64(time.Now().UnixMilli())
	pos := &PbPosition{Timestamp: nowMs}
	if state.CurrentPosition != nil {
		posVal := state.CurrentPosition.Value
		if state.PlayingState == PlayingStatePlaying && state.CurrentPosition.Timestamp != 0 {
			elapsed := nowMs - state.CurrentPosition.Timestamp
			posVal += uint32(elapsed)
		}
		pos.Value = posVal
	}

	msgs := []*PbQConnectMessage{{
		MessageType: MsgTypeRndrSrvrStateUpdated,
		RndrSrvrStateUpdated: &PbRndrSrvrStateUpdated{
			State: &PbQueueRendererState{
				PlayingState:       state.PlayingState,
				BufferState:        state.BufferState,
				CurrentPosition:    pos,
				Duration:           state.Duration,
				QueueVersion:       state.QueueVersion,
				CurrentQueueItemID: state.CurrentQueueItemID,
				NextQueueItemID:    state.NextQueueItemID,
			},
		},
	}}
	if withQuality {
		if format := p.currentFormat.Load(); format != 0 {
			qv := formatToQualityValue(int(format))
			sr, bd, nc := audioQualityProperties(int(format))
			log.Printf("player: sendState withQuality=true format=%d qualityValue=%d sr=%d bd=%d nc=%d",
				format, qv, sr, bd, nc)
			msgs = append(msgs,
				&PbQConnectMessage{
					MessageType: MsgTypeRndrSrvrFileAudioQualityChanged,
					RndrSrvrFileAudioQuality: &PbRndrSrvrFileAudioQuality{
						SamplingRate: sr,
						BitDepth:     bd,
						NbChannels:   nc,
						AudioQuality: qv,
					},
				},
				&PbQConnectMessage{
					MessageType: MsgTypeRndrSrvrDeviceAudioQualityChanged,
					RndrSrvrDeviceAudioQuality: &PbRndrSrvrDeviceAudioQuality{
						SamplingRate: sr,
						BitDepth:     bd,
						NbChannels:   nc,
					},
				},
			)
		} else {
			log.Printf("player: sendState withQuality=true but currentFormat=0 — quality message skipped")
		}
	}
	p.sendMsg(msgs)
}
