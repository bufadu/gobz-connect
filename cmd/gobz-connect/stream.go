package main

import (
	"fmt"
	"log"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// QobuzStream is the main orchestrator: manages login, WebSocket connectivity,
// the track queue, and the audio player.
type QobuzStream struct {
	cfg       *Config
	api       *QobuzAPI
	queue     *Queue
	player    *Player
	mdns      *MDNSServer
	ws        *WsManager
	cec       *CECManager
	sessionID []byte // 16-byte raw device UUID

	rendererID    uint64
	currentSessID uint64
	isActive      atomic.Bool

	// sentLoadedTracks distinguishes a self-triggered autoplay response from a
	// server-initiated one (update vs load).
	sentLoadedTracks atomic.Bool

	// awaitingRendererState is set when QueueTracksLoaded stops the player so
	// that a stale SrvrCtrlRendererStateUpdated echo (carrying the old position)
	// does not race in and restart the player before SrvrRndrSetState arrives
	// with the correct track and position.  Cleared at the top of SrvrRndrSetState.
	awaitingRendererState atomic.Bool

	// pendingQueueLoad is set when ConsumeQueueState detects a track-count
	// mismatch (server reports N tracks but gobz has M ≠ N locally), meaning a
	// QueueTracksLoaded is imminent and the local refs are stale.  While set,
	// SrvrRndrSetState messages are dropped so the player never starts with the
	// wrong (old) queue.  Cleared by QueueTracksLoaded before it processes the
	// new tracks.
	pendingQueueLoad atomic.Bool

	// injectedWSToken holds a token provided by the Qobuz app via connect-to-qconnect
	// to be used on the next WS (re-)connection.
	injectedWSToken   *WSToken
	injectedWSTokenMu sync.Mutex

	// wsReadyCh is closed once the app has injected credentials via
	// connect-to-qconnect.  In unauthenticated mode the tokenFn blocks on this
	// channel so the WS manager never calls createWSToken before auth is available.
	wsReadyCh   chan struct{}
	wsReadyOnce sync.Once

	// tokenMu protects tokenExpiredCh, tokenExpiredAt, and the APIToken/
	// APITokenExpMs fields when they are updated by handleAppConnect and
	// read by waitForValidToken.
	tokenMu           sync.Mutex
	tokenExpiredCh    chan struct{} // non-nil when jwt_api has expired; closed by handleAppConnect
	tokenExpiredAt    time.Time     // when the current expiry episode was first detected; anchors tokenWaitTimeout
	coldReconnectDone bool          // true once a cold WS reconnect has been tried for the current expiry episode

	// wsExistingJWT is the JWT chained into the next CreateWSToken(refreshToken)
	// call. Cleared by resetWSTokenChain to force a brand-new, unchained join —
	// see WsManager.ColdReconnect.
	wsTokenChainMu sync.Mutex
	wsExistingJWT  string
}

// NewQobuzStream creates a QobuzStream.
func NewQobuzStream(cfg *Config, sessionID []byte) *QobuzStream {
	return &QobuzStream{
		cfg:       cfg,
		sessionID: sessionID,
		wsReadyCh: make(chan struct{}),
	}
}

// Start performs authentication, sets up all subsystems, and begins streaming.
func (s *QobuzStream) Start() error {
	s.api = &QobuzAPI{}
	if s.cfg.UnauthenticatedMode {
		// In unauthenticated mode, REST API calls must block while jwt_api is
		// expired rather than racing through the queue with 401 errors.
		s.api.WaitToken = s.waitForValidToken
	}

	// Load persisted app credentials if available.
	stored, err := loadSecrets(s.cfg.SecretsFile)
	if err != nil {
		log.Printf("stream: could not read secrets file %s: %v", s.cfg.SecretsFile, err)
	}
	if stored != nil {
		s.api.AppID = stored.AppID
		s.api.AppSecret = stored.AppSecret
		log.Printf("stream: loaded app credentials from %s", s.cfg.SecretsFile)
	}

	// AppID/AppSecret are always needed — even in unauthenticated mode — because
	// getFileUrl requests must be signed with AppSecret (request_sig).
	// Scrape if either is missing from the secrets file.
	if s.api.AppID == "" || s.api.AppSecret == "" {
		log.Printf("stream: no cached app credentials — scraping Qobuz web player…")
		scraped, err := FetchAppSecrets()
		if err != nil {
			return fmt.Errorf("stream: could not scrape app secrets: %w", err)
		}
		s.api.AppID = scraped.AppID
		for _, sec := range scraped.Secrets {
			s.api.AppSecret = sec
			break
		}
		if err := saveSecrets(s.cfg.SecretsFile, &StoredSecrets{
			AppID:     s.api.AppID,
			AppSecret: s.api.AppSecret,
		}); err != nil {
			log.Printf("stream: could not save secrets: %v", err)
		}
	}

	if s.cfg.UnauthenticatedMode {
		log.Printf("stream: unauthenticated mode — skipping login, exposing mDNS renderer only")
	} else {
		var loginErr error
		if s.cfg.UserAuthToken != "" && s.cfg.UserID != "" {
			// Token-based auth: bypasses reCAPTCHA-protected email/password login.
			log.Printf("stream: authenticating with user_auth_token (user_id=%s)", s.cfg.UserID)
			loginErr = s.api.LoginWithToken(s.cfg.UserID, s.cfg.UserAuthToken)
		} else {
			log.Printf("stream: logging in as %s (app_id=%s)", s.cfg.Email, s.api.AppID)
			loginErr = s.api.Login(s.cfg.Email, s.cfg.Password)
			if loginErr != nil {
				// Login failed — app_id may be stale. Re-scrape and retry once.
				log.Printf("stream: login failed (%v) — re-scraping app credentials and retrying…", loginErr)
				scraped, scrapeErr := FetchAppSecrets()
				if scrapeErr != nil {
					return fmt.Errorf("stream: login failed and re-scrape also failed: login=%v scrape=%v", loginErr, scrapeErr)
				}
				s.api.AppID = scraped.AppID
				for _, sec := range scraped.Secrets {
					s.api.AppSecret = sec
					break
				}
				log.Printf("stream: retrying login with fresh app_id=%s", s.api.AppID)
				loginErr = s.api.Login(s.cfg.Email, s.cfg.Password)
				if loginErr == nil {
					if err := saveSecrets(s.cfg.SecretsFile, &StoredSecrets{
						AppID:     s.api.AppID,
						AppSecret: s.api.AppSecret,
					}); err != nil {
						log.Printf("stream: could not save secrets: %v", err)
					}
				}
			}
		}
		if loginErr != nil {
			return fmt.Errorf("stream: authentication failed: %w", loginErr)
		}
		if err := s.api.StartSession(); err != nil {
			log.Printf("stream: startSession: %v (continuing)", err)
		}

		// Validate the app secret; re-scrape if it is stale or wrong.
		if s.api.AppSecret == "" || !s.api.VerifySecret() {
			log.Printf("stream: app secret invalid — re-scraping Qobuz web player…")
			scraped, err := FetchAppSecrets()
			if err != nil {
				return fmt.Errorf("stream: could not scrape app secrets: %w", err)
			}
			s.api.AppID = scraped.AppID
			found := false
			for tz, sec := range scraped.Secrets {
				s.api.AppSecret = sec
				if s.api.VerifySecret() {
					log.Printf("stream: using secret for timezone %s", tz)
					found = true
					break
				}
			}
			if !found {
				return fmt.Errorf("stream: no valid app secret found after scraping")
			}
			if err := saveSecrets(s.cfg.SecretsFile, &StoredSecrets{
				AppID:     s.api.AppID,
				AppSecret: s.api.AppSecret,
			}); err != nil {
				log.Printf("stream: could not save secrets: %v", err)
			}
		}
	}

	userID := s.api.UserID

	s.queue = NewQueue(s.sessionID, s.api, s.cfg.AudioFormat)
	s.player = NewPlayer(s.api, s.queue, userID)
	s.player.SetResampleQuality(s.cfg.ResampleQuality)
	s.player.SetSpeakerSampleRate(s.cfg.SpeakerSampleRate)
	s.player.SetAdaptiveSampleRate(s.cfg.IsAdaptiveSampleRate())

	s.cec = NewCECManager(s.cfg)
	if s.cec != nil {
		// When the amp wakes from off/standby, the HDMI link becomes active and
		// the ALSA device state changes. Reinitialize the audio device so the
		// player picks up the live HDMI connection (fixes silent playback when
		// gobz-connect starts while the amp is off).
		s.cec.SetOnAmpWake(func() {
			log.Printf("stream: amp woke from standby — reinitialising audio device")
			s.player.ScheduleAudioReinit()
		})
		if s.cfg.CEC.VolumeControl {
			// Beep stays at a fixed level; the CEC amp handles actual volume.
			s.player.SetVolume(90)
			// Once the amp is on and responds to <Give Audio Status>, sync its
			// current volume to the Qobuz app so the slider shows the right level
			// and future CEC delta calculations start from the correct baseline.
			s.cec.SetOnInitialVolume(func(vol int) {
				log.Printf("stream: syncing initial volume from amp via CEC: %d%%", vol)
				s.player.SetVolume(uint32(vol))
				go s.wsSetRendererVolume()
			})
		}
	}

	if s.cfg.CacheSizeMB > 0 {
		tc, err := NewTrackCache(s.cfg.CacheDir, s.cfg.CacheSizeMB, s.cfg.BackgroundDownloadRateKBps)
		if err != nil {
			log.Printf("stream: cache disabled: %v", err)
		} else {
			s.queue.SetCache(tc)
			s.player.SetCache(tc)
			log.Printf("stream: cache enabled at %s (%d MB)", s.cfg.CacheDir, s.cfg.CacheSizeMB)
		}
	}

	s.mdns = NewMDNSServer(s.cfg.DeviceName, s.sessionID, s.cfg.Port, s.api.AppID, s.handleAppConnect)
	if err := s.mdns.Start(); err != nil {
		log.Printf("stream: mdns: %v (continuing)", err)
	}

	tokenFn := func() (*WSToken, error) {
		// In unauthenticated mode, block until the Qobuz app has connected via
		// mDNS and injected its credentials — otherwise createWSToken returns 401.
		if s.cfg.UnauthenticatedMode {
			select {
			case <-s.wsReadyCh:
				// credentials available, proceed
			case <-s.ws.ctx.Done():
				return nil, fmt.Errorf("ws stopped")
			}
			// Block if jwt_api has expired; handleAppConnect will unblock us
			// when the app reconnects via mDNS with fresh credentials.
			// This avoids 401-every-5s spam from a futile createWSToken loop.
			if !s.waitForValidToken() {
				return nil, fmt.Errorf("ws: no valid jwt_api (timed out waiting for app reconnect, or shutting down)")
			}
		}
		// If the Qobuz app connected via mDNS and provided its JWT, use it as the
		// "existing" JWT so CreateWSToken calls refreshToken (matching C++ getWSToken).
		s.injectedWSTokenMu.Lock()
		inj := s.injectedWSToken
		s.injectedWSToken = nil
		s.injectedWSTokenMu.Unlock()

		s.wsTokenChainMu.Lock()
		existingJWT := s.wsExistingJWT
		s.wsTokenChainMu.Unlock()
		if inj != nil && inj.JWT != "" {
			existingJWT = inj.JWT
		}
		tok, err := s.api.CreateWSToken(existingJWT)
		if err != nil {
			return nil, err
		}
		s.wsTokenChainMu.Lock()
		s.wsExistingJWT = tok.JWT
		s.wsTokenChainMu.Unlock()
		return tok, nil
	}

	s.ws = NewWsManager(tokenFn)

	sendMsg := func(msgs []*PbQConnectMessage) { s.ws.SendBatch(msgs) }
	s.queue.SetSendMsg(sendMsg)
	// Player state updates must only be sent when we are the active renderer;
	// sending them too early causes server error "non active renderer".
	playerSendMsg := func(msgs []*PbQConnectMessage) {
		active := s.isActive.Load()
		for _, m := range msgs {
			if m.MessageType == MsgTypeRndrSrvrFileAudioQualityChanged {
				if active {
					log.Printf("stream: playerSendMsg → FileAudioQualityChanged qualityValue=%d (sending)", m.RndrSrvrFileAudioQuality)
				} else {
					log.Printf("stream: playerSendMsg → FileAudioQualityChanged qualityValue=%d DROPPED (isActive=false)", m.RndrSrvrFileAudioQuality)
				}
			}
		}
		if active {
			s.ws.SendBatch(msgs)
		}
	}
	s.player.SetSendMsg(playerSendMsg)

	s.ws.OnAuth(func() { s.wsRegisterController() })
	s.ws.OnPayload(func(data []byte) { s.wsDecodePayload(data) })

	go s.queue.Run()
	go s.ws.Run()
	go s.heartbeat()

	return nil
}

// Stop gracefully shuts down all subsystems.
func (s *QobuzStream) Stop() {
	if s.ws != nil {
		s.ws.Stop()
	}
	if s.queue != nil {
		s.queue.Stop()
	}
	if s.player != nil {
		s.player.Stop()
	}
	if s.mdns != nil {
		s.mdns.Stop()
	}
}

// tokenWaitTimeout bounds how long waitForValidToken blocks for a single
// expiry episode before giving up and returning false. Without this, any
// call gated on WaitToken (GetFileURL, GetTrackMetadata) blocks forever if
// the Qobuz app never re-sends connect-to-qconnect — per PROTOCOL.md §7.3/§8,
// nothing in the protocol guarantees it ever will on its own. Bounding the
// wait lets track loading fail and move on (existing TrackFailed handling)
// instead of freezing playback silently.
const tokenWaitTimeout = 45 * time.Second

// coldReconnectHold is a first guess at how long a real process restart
// leaves the WS connection to the Qobuz backend down. Tune based on what the
// test in waitForValidToken shows.
const coldReconnectHold = 45 * time.Second

// resetWSTokenChain clears the WS refresh-token chain so the next reconnect
// performs a brand-new, unchained CreateWSToken call instead of refreshToken
// — see WsManager.ColdReconnect.
func (s *QobuzStream) resetWSTokenChain() {
	s.wsTokenChainMu.Lock()
	s.wsExistingJWT = ""
	s.wsTokenChainMu.Unlock()
}

// giveUpOnExpiredToken logs the timeout and — once per expiry episode —
// triggers the cold-reconnect experiment. Called from both the immediate
// "already past deadline" check and the timer-fired case in
// waitForValidToken, so the experiment fires the moment the grace period
// actually elapses rather than whenever the next caller happens to check.
func (s *QobuzStream) giveUpOnExpiredToken() bool {
	log.Printf("stream: jwt_api still expired after %s — giving up on this request until the app reconnects", tokenWaitTimeout)
	s.tokenMu.Lock()
	alreadyTried := s.coldReconnectDone
	s.coldReconnectDone = true
	s.tokenMu.Unlock()
	if !alreadyTried {
		log.Printf("stream: [experiment] forcing a cold WS reconnect (unchained token, %s gap) to test whether the app evicts and re-registers us", coldReconnectHold)
		s.resetWSTokenChain()
		s.ws.ColdReconnect(coldReconnectHold)
	}
	return false
}

// waitForValidToken blocks until s.api.APIToken is non-expired, the WS is
// stopped, or tokenWaitTimeout elapses since the expiry was first detected —
// whichever comes first. Returns false if the WS context was cancelled
// (shutdown) or the app never reconnected in time.
//
// In unauthenticated mode the jwt_api comes from the Qobuz app; when it
// expires the WS token endpoint and REST calls return 401. Rather than
// spamming 401s, or hanging forever if the app never reconnects, callers use
// this to wait (bounded) until handleAppConnect provides fresh credentials.
func (s *QobuzStream) waitForValidToken() bool {
	for {
		s.tokenMu.Lock()
		exp := s.api.APITokenExpMs
		if exp == 0 || time.Now().UnixMilli() < exp {
			s.tokenMu.Unlock()
			return true
		}
		if s.tokenExpiredCh == nil {
			s.tokenExpiredCh = make(chan struct{})
			s.tokenExpiredAt = time.Now()
			s.coldReconnectDone = false
			log.Printf("stream: jwt_api expired — API calls and WS token blocked until app reconnects via mDNS (giving up after %s)", tokenWaitTimeout)
		}
		ch := s.tokenExpiredCh
		deadline := s.tokenExpiredAt.Add(tokenWaitTimeout)
		s.tokenMu.Unlock()

		remaining := time.Until(deadline)
		if remaining <= 0 {
			return s.giveUpOnExpiredToken()
		}
		timer := time.NewTimer(remaining)
		select {
		case <-ch:
			timer.Stop()
			// handleAppConnect stored fresh credentials and closed the channel;
			// loop back to re-check the expiry time.
		case <-timer.C:
			return s.giveUpOnExpiredToken()
		case <-s.ws.ctx.Done():
			timer.Stop()
			return false
		}
	}
}

// heartbeat periodically refreshes the Qobuz session token, mirroring the
// C++ token_hb_: runs every 30s but only calls startSession when the session
// expires within the next 60 seconds.
//
// In unauthenticated mode there is nothing to refresh here: jwt_api can only
// come from the Qobuz app via connect-to-qconnect, and only in response to
// the user selecting this renderer in the app's own UI. Reading the official
// desktop client's source confirms this — its QConnectRenderersFinder only
// calls connectRendererToServer from the "qconnect-register-renderer" IPC
// handler, itself wired solely to that UI action; nothing in the client
// watches jwt_api's expiry or reacts to this renderer bouncing its WS
// connection. There is no self-service refresh to attempt, so
// waitForValidToken's bounded timeout is the only correct handling for that
// case.
func (s *QobuzStream) heartbeat() {
	const refreshWindowMs = 60 * 1000
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		<-ticker.C
		if s.cfg.UnauthenticatedMode {
			continue
		}

		nowMs := time.Now().UnixMilli()
		exp := s.api.SessionExpiresAtMs
		if exp == 0 || exp <= nowMs+refreshWindowMs {
			if err := s.api.StartSession(); err != nil {
				log.Printf("stream: heartbeat: %v", err)
			}
		}
	}
}

// handleAppConnect is called when the Qobuz mobile app connects via mDNS.
// It injects the app-provided JWT tokens so the WS manager reconnects using them.
func (s *QobuzStream) handleAppConnect(info *AppConnectInfo) {
	// Update API token for subsequent signed API calls.
	// Write under tokenMu so waitForValidToken reads a consistent value and
	// we can atomically signal any blocked tokenFn.
	if info.APIToken != "" {
		s.tokenMu.Lock()
		s.api.APIToken = info.APIToken
		if info.APIExp > 0 {
			s.api.APITokenExpMs = int64(info.APIExp) * 1000
		}
		if s.tokenExpiredCh != nil {
			close(s.tokenExpiredCh)
			s.tokenExpiredCh = nil
		}
		s.tokenMu.Unlock()
	}
	// Unblock the tokenFn (was waiting in unauthenticated mode).
	s.wsReadyOnce.Do(func() { close(s.wsReadyCh) })
	// Update the session token used in X-Session-Id headers.
	// C++ strips dashes from the UUID before using it as X-Session-Id
	// (parseSessionId → hex32_from16 produces a 32-char no-dash hex string).
	if info.SessionID != "" {
		s.api.SessionToken = strings.ReplaceAll(info.SessionID, "-", "")
	}

	// In unauthenticated mode the AppSecret cannot be verified at startup
	// (VerifySecret needs a user token). Now that the app has injected its
	// jwt_api we can verify it and re-scrape if stale — do this synchronously
	// before triggering the WS reconnect so the player never starts with a
	// bad secret.
	if s.cfg.UnauthenticatedMode && info.APIToken != "" {
		if !s.api.VerifySecret() {
			log.Printf("stream: app secret invalid after app connect — re-scraping…")
			scraped, err := FetchAppSecrets()
			if err != nil {
				log.Printf("stream: secret re-scrape failed: %v", err)
			} else {
				s.api.AppID = scraped.AppID
				found := false
				for tz, sec := range scraped.Secrets {
					s.api.AppSecret = sec
					if s.api.VerifySecret() {
						log.Printf("stream: valid secret found for timezone %s", tz)
						found = true
						break
					}
				}
				if found {
					if err := saveSecrets(s.cfg.SecretsFile, &StoredSecrets{
						AppID:     s.api.AppID,
						AppSecret: s.api.AppSecret,
					}); err != nil {
						log.Printf("stream: could not save secrets: %v", err)
					}
				} else {
					log.Printf("stream: no valid secret found after re-scrape — getFileUrl calls will fail")
				}
			}
		} else {
			log.Printf("stream: app secret verified ok")
		}
	}

	// Store the app's JWT so the next CreateWSToken call uses refreshToken.
	// (C++ calls refreshToken with the app JWT as the "existing" token.)
	if info.WSJWT != "" {
		s.injectedWSTokenMu.Lock()
		s.injectedWSToken = &WSToken{JWT: info.WSJWT}
		s.injectedWSTokenMu.Unlock()
		// Close the current connection; Run() will reconnect and call refreshToken.
		s.ws.Reconnect()
	}
	s.isActive.Store(true)
}

// ---- WebSocket message production ----

func (s *QobuzStream) wsRegisterController() {
	s.ws.SendBatch([]*PbQConnectMessage{{
		MessageType: MsgTypeCtrlSrvrJoinSession,
		CtrlSrvrJoinSession: &PbCtrlSrvrJoinSession{
			// SessionUUID is intentionally omitted: C++ does not set ctrlSrvrJoinSession.sessionUuid,
			// only deviceInfo.deviceUuid. Setting it causes server error 10002.
			DeviceInfo: &PbDeviceInfo{
				DeviceUUID:   s.sessionID,
				FriendlyName: s.cfg.DeviceName,
				Type:         DeviceTypeSpeaker,
				Capabilities: &PbDeviceCapabilities{
					MinAudioQuality:     1,
					MaxAudioQuality:     formatToQualityValue(s.cfg.AudioFormat),
					VolumeRemoteControl: 2,
				},
				SoftwareVersion: "go-1.0.0",
			},
		},
	}})
}

func (s *QobuzStream) wsSetRendererActive() {
	s.ws.SendBatch([]*PbQConnectMessage{{
		MessageType: MsgTypeCtrlSrvrSetActiveRenderer,
		CtrlSrvrSetActiveRenderer: &PbCtrlSrvrSetActiveRenderer{
			RendererID: int64(s.rendererID),
		},
	}})
}

// wsSetMaxAudioQuality proactively announces our max quality (HiRes-192) to
// the server. The server broadcasts SrvrCtrlMaxAudioQualityChanged to all
// controllers; this is what drives the quality badge in the Qobuz app.
func (s *QobuzStream) wsSetMaxAudioQuality() {
	log.Printf("stream: wsSetMaxAudioQuality → sending RndrSrvrMaxAudioQualityChanged=%d", formatToQualityValue(s.cfg.AudioFormat))
	s.ws.SendBatch([]*PbQConnectMessage{{
		MessageType: MsgTypeRndrSrvrMaxAudioQualityChanged,
		RndrSrvrMaxAudioQuality: &PbRndrSrvrMaxAudioQuality{
			AudioQuality: formatToQualityValue(s.cfg.AudioFormat),
			NetworkType:  1, // WiFi
		},
	}})
}

func (s *QobuzStream) wsSetRendererVolume() {
	vol := s.player.volume.Load()
	s.ws.SendBatch([]*PbQConnectMessage{{
		MessageType: MsgTypeRndrSrvrVolumeChanged,
		RndrSrvrVolumeChanged: &PbRndrSrvrVolumeChanged{Volume: vol},
	}})
}

func (s *QobuzStream) wsSetVolumeMuted() {
	s.ws.SendBatch([]*PbQConnectMessage{{
		MessageType:        MsgTypeRndrSrvrVolumeMuted,
		RndrSrvrVolumeMuted: &PbRndrSrvrVolumeMuted{Muted: false},
	}})
}

func (s *QobuzStream) wsAskForQueueState() {
	var qv *PbQueueVersion
	s.queue.mu.Lock()
	if s.queue.QueueState != nil {
		qv = s.queue.QueueState.QueueVersion
	}
	s.queue.mu.Unlock()

	s.ws.SendBatch([]*PbQConnectMessage{{
		MessageType: MsgTypeCtrlSrvrAskForQueueState,
		CtrlSrvrAskForQueueState: &PbCtrlSrvrAskForQueueState{
			QueueVersion: qv,
			QueueUUID:    s.sessionID,
		},
	}})
}

func (s *QobuzStream) wsAskForRendererState() {
	s.ws.SendBatch([]*PbQConnectMessage{{
		MessageType: MsgTypeCtrlSrvrAskForRendererState,
		CtrlSrvrAskForRendererState: &PbCtrlSrvrAskForRendererState{
			SessionID: s.currentSessID,
		},
	}})
}

// ---- WebSocket message consumption ----

func (s *QobuzStream) wsDecodePayload(data []byte) {
	payload := DecodePayload(data)
	if payload == nil || len(payload.Payload) == 0 {
		return
	}
	batch := DecodeBatch(payload.Payload)
	if batch == nil {
		return
	}
	for _, msg := range batch.Messages {
		s.wsHandleMessage(msg)
	}
}

func (s *QobuzStream) wsHandleMessage(msg *PbQConnectMessage) {
	switch msg.MessageType {

	case MsgTypeError:
		if msg.Error != nil {
			log.Printf("stream: server error %q: %s", msg.Error.Code, msg.Error.Message)
			if msg.Error.Message == "Current track not found in queue nor autoplay" {
				// The server doesn't recognise our current track — our local
				// queue is out of sync.  Re-sync the full queue state.
				// getSuggestions() here would loop forever if the autoplay
				// context has changed, growing the queue without bound.
				s.wsAskForQueueState()
			}
		}

	case MsgTypeSrvrCtrlQueueErrorMessage:
		if msg.SrvrCtrlQueueErrorMessage == nil {
			break
		}
		e := msg.SrvrCtrlQueueErrorMessage
		log.Printf("stream: queue error %q: %s", e.Error.Code, e.Error.Message)
		if e.Error.Message == "Queue version mismatch" && e.QueueVersion != nil {
			s.queue.mu.Lock()
			if s.queue.QueueState == nil {
				s.queue.QueueState = &PbSrvrCtrlQueueState{}
			}
			s.queue.QueueState.QueueVersion = e.QueueVersion
			s.queue.mu.Unlock()
			// Re-sync the full queue state rather than retrying getSuggestions():
			// calling getSuggestions() with a stale version just produces another
			// mismatch, looping forever and growing the queue without bound.
			s.wsAskForQueueState()
		}

	case MsgTypeSrvrCtrlAddRenderer:
		if msg.SrvrCtrlAddRenderer == nil {
			break
		}
		r := msg.SrvrCtrlAddRenderer
		if r.Renderer != nil && bytesEqual(r.Renderer.DeviceUUID, s.sessionID) {
			log.Printf("stream: got renderer id=%d", r.RendererID)
			s.rendererID = r.RendererID
			if s.isActive.Load() {
				s.wsSetRendererActive()
				s.wsSetVolumeMuted()
				s.wsSetRendererVolume()
			}
		}

	case MsgTypeSrvrCtrlSessionState:
		if msg.SrvrCtrlSessionState == nil {
			break
		}
		ss := msg.SrvrCtrlSessionState
		s.currentSessID = ss.SessionID
		if ss.QueueVersion != nil {
			s.queue.mu.Lock()
			if s.queue.QueueState == nil {
				s.queue.QueueState = &PbSrvrCtrlQueueState{}
			}
			s.queue.QueueState.QueueVersion = ss.QueueVersion
			s.queue.mu.Unlock()
		}
		s.wsAskForQueueState()
		// Only ask for renderer state when the player is not already running.
		// When it is running, the server's response would reflect the last
		// position we reported — a stale echo that would rewind playback.
		if !s.player.IsRunning() {
			s.wsAskForRendererState()
		}

	case MsgTypeSrvrCtrlActiveRendererChanged:
		if msg.SrvrCtrlActiveRendererChanged == nil {
			break
		}
		newID := msg.SrvrCtrlActiveRendererChanged.RendererID
		log.Printf("stream: active renderer → %d (ours=%d)", newID, s.rendererID)
		if newID == s.rendererID {
			// Always confirm active state and report volume so the Qobuz app
			// shows the volume slider regardless of how we became active.
			s.wsSetRendererActive()
			s.wsSetVolumeMuted()
			s.wsSetRendererVolume()
			s.wsSetMaxAudioQuality()
			s.isActive.Store(true)
			if s.player.IsRunning() {
				// Player is already playing (e.g. token refresh reconnect, or
				// SrvrCtrlRendererStateUpdated arrived before this message).
				// Push our current state to the server instead of pulling from
				// it: the server's response would be a stale echo of the last
				// position we reported, which would rewind playback.
				log.Printf("stream: ActiveRendererChanged — player running, resending quality")
				go s.player.sendPlayerStateWithQuality()
			} else {
				// Ask for renderer state; the SrvrRndrSetState response will
				// call SetIndex+SetStartAt and start the player at the correct
				// track and position. Do NOT start the player here: starting
				// before state arrives causes the queue to advance from the
				// wrong position, and the subsequent SetIndex rewinds it.
				s.wsAskForRendererState()
			}
		} else {
			if s.player.IsRunning() {
				s.player.Stop()
			}
			s.isActive.Store(false)
			if s.cec != nil {
				s.cec.OnPlayback(false)
			}
		}

	case MsgTypeSrvrCtrlQueueState:
		if msg.SrvrCtrlQueueState != nil {
			if s.queue.ConsumeQueueState(msg.SrvrCtrlQueueState) {
				// Server track count differs from local refs — QueueTracksLoaded
				// is imminent. Block SrvrRndrSetState and RendererStateUpdated from
				// starting the stale queue; QueueTracksLoaded will clear the flag.
				log.Printf("stream: stale queue detected — deferring playback until QueueTracksLoaded")
				s.pendingQueueLoad.Store(true)
				s.awaitingRendererState.Store(true)
				if s.player.IsRunning() {
					s.player.Stop()
				}
			}
		}

	case MsgTypeSrvrCtrlQueueTracksLoaded:
		if msg.SrvrCtrlQueueTracksLoaded == nil {
			break
		}
		// New queue arriving — stale-queue deferral is no longer needed.
		s.pendingQueueLoad.Store(false)
		loaded := msg.SrvrCtrlQueueTracksLoaded
		log.Printf("stream: QueueTracksLoaded tracks=%d", len(loaded.Tracks))
		s.queue.DeleteAllTracks()
		if loaded.QueueVersion != nil {
			s.queue.mu.Lock()
			if s.queue.QueueState == nil {
				s.queue.QueueState = &PbSrvrCtrlQueueState{}
			}
			s.queue.QueueState.QueueVersion = loaded.QueueVersion
			s.queue.mu.Unlock()
		}
		s.wsAskForQueueState()
		s.wsAskForRendererState()
		s.queue.AddTracks(loaded.Tracks, nil, loaded.ContextUUID)
		s.queue.GrowShuffleIndexes()
		// Stop the current player so it does not bleed into the new queue.
		// Set awaitingRendererState first so that any SrvrCtrlRendererStateUpdated
		// echo (carrying the old position) that arrives while the player is
		// stopped cannot race in and restart it at the wrong position.
		// SrvrRndrSetState clears the flag and starts the player at the right
		// track/position.
		s.awaitingRendererState.Store(true)
		if s.player.IsRunning() {
			s.player.Stop()
		}

	case MsgTypeSrvrCtrlQueueTracksInserted:
		if msg.SrvrCtrlQueueTracksInserted == nil {
			break
		}
		ins := msg.SrvrCtrlQueueTracksInserted
		if ins.AutoplayReset {
			s.queue.DeleteAutoplayTracks()
		}
		pos, _ := s.queue.Position(uint64(ins.InsertAfter))
		s.queue.AddTracks(ins.Tracks, &pos, ins.ContextUUID)

	case MsgTypeSrvrCtrlQueueTracksAdded:
		if msg.SrvrCtrlQueueTracksAdded == nil {
			break
		}
		added := msg.SrvrCtrlQueueTracksAdded
		if added.AutoplayReset {
			s.queue.DeleteAutoplayTracks()
		}
		s.queue.AddTracks(added.Tracks, nil, added.ContextUUID)

	case MsgTypeSrvrCtrlQueueTracksRemoved:
		if msg.SrvrCtrlQueueTracksRemoved != nil {
			s.queue.DeleteTracksByQueueItemID(msg.SrvrCtrlQueueTracksRemoved.QueueItemIDs)
		}

	case MsgTypeSrvrCtrlAutoplayTracksLoaded:
		if msg.SrvrCtrlAutoplayTracksLoaded == nil {
			break
		}
		ap := msg.SrvrCtrlAutoplayTracksLoaded
		if s.sentLoadedTracks.Load() {
			s.sentLoadedTracks.Store(false)
			// Update context UUIDs for existing track refs.
			s.queue.mu.Lock()
			for _, ref := range ap.Tracks {
				for _, r := range s.queue.refs {
					if r.QueueItemID == ref.QueueItemID && len(ap.ContextUUID) > 0 {
						r.ContextUUID = ap.ContextUUID
					}
				}
			}
			s.queue.mu.Unlock()
		} else {
			s.queue.DeleteAutoplayTracks()
			s.queue.AddTracks(ap.Tracks, nil, ap.ContextUUID)
		}

	case MsgTypeSrvrCtrlAutoplayTracksRemoved:
		if msg.SrvrCtrlAutoplayTracksRemoved != nil {
			s.queue.DeleteTracksByQueueItemID(msg.SrvrCtrlAutoplayTracksRemoved.QueueItemIDs)
		}

	case MsgTypeSrvrCtrlVolumeChanged:
		if msg.SrvrCtrlVolumeChanged != nil && msg.SrvrCtrlVolumeChanged.RendererID == s.rendererID {
			s.applyVolume(msg.SrvrCtrlVolumeChanged.Volume)
		}

	case MsgTypeSrvrRndrSetActive:
		if msg.SrvrRndrSetActive == nil {
			break
		}
		if msg.SrvrRndrSetActive.Active {
			s.isActive.Store(true)
			if s.rendererID != 0 {
				s.wsSetRendererActive()
				s.wsSetVolumeMuted()
				s.wsSetRendererVolume()
				s.wsSetMaxAudioQuality()
			}
			s.wsAskForQueueState()
			if s.player.IsRunning() {
				// Player is already playing — push our current state to the
				// server. Pulling renderer state would return a stale echo of
				// the last position we reported, rewinding playback.
				log.Printf("stream: SetActive — player running, resending quality")
				go s.player.sendPlayerStateWithQuality()
			} else {
				s.wsAskForRendererState()
			}
		} else {
			s.isActive.Store(false)
			if s.player.IsRunning() {
				s.player.Stop()
			}
			if s.cec != nil {
				s.cec.OnPlayback(false)
			}
		}

	case MsgTypeSrvrRndrSetVolume:
		if msg.SrvrRndrSetVolume == nil {
			break
		}
		sv := msg.SrvrRndrSetVolume
		if sv.Volume != 0 || sv.VolumeDelta == 0 {
			s.applyVolume(sv.Volume)
		} else {
			cur := int32(s.player.volume.Load())
			newVol := cur + sv.VolumeDelta
			if newVol < 0 {
				newVol = 0
			} else if newVol > 100 {
				newVol = 100
			}
			s.applyVolume(uint32(newVol))
		}
		// Echo the new volume back so the Qobuz app confirms the change.
		go s.wsSetRendererVolume()

	case MsgTypeSrvrCtrlRendererStateUpdated:
		if msg.SrvrCtrlRendererStateUpdated == nil || msg.SrvrCtrlRendererStateUpdated.State == nil {
			break
		}
		rsu := msg.SrvrCtrlRendererStateUpdated
		log.Printf("stream: RendererStateUpdated rendererID=%d (ours=%d) queueIndex=%d pos=%v",
			rsu.RendererID, s.rendererID, rsu.State.CurrentQueueIndex,
			func() interface{} {
				if rsu.State.CurrentPosition != nil {
					return rsu.State.CurrentPosition.Value
				}
				return nil
			}())
		if rsu.RendererID == s.rendererID {
			if !s.player.IsRunning() {
				// Player not running: apply the server's state to seed the queue
				// at the correct position, then start playback.
				// When the player IS running, the server echoes our own state back
				// (C++ does the same: SrvrCtrlRendererStateUpdated is a no-op when
				// the player is active). Calling SetIndex while running rewinds the
				// queue to a stale position, causing large playlists to loop.
				//
				// Skip if we just stopped the player for a new queue load:
				// QueueTracksLoaded called wsAskForRendererState, and the server
				// will broadcast RendererStateUpdated with the OLD position before
				// SrvrRndrSetState arrives.  Letting it start the player here would
				// seek the first track of the new playlist to the old position.
				// SrvrRndrSetState clears awaitingRendererState and handles the
				// correct restart.  This mirrors C++ behaviour where the player is
				// never stopped in QueueTracksLoaded (so IsRunning() stays true and
				// this block is skipped).
				if s.awaitingRendererState.Load() {
					log.Printf("stream: RendererStateUpdated ignored — awaiting SrvrRndrSetState for new queue")
					break
				}
				// Skip if the player stopped because the queue ran out naturally.
				// The server echoes our own last "playing" heartbeat back, which
				// would restart the last track indefinitely.  SrvrRndrSetState or
				// a user-initiated play action will clear queueExhausted and
				// trigger the correct restart via the bottom block.
				if s.player.queueExhausted.Load() {
					log.Printf("stream: RendererStateUpdated ignored — queue exhausted naturally")
					break
				}
				s.queue.SetIndex(uint64(rsu.State.CurrentQueueIndex))
				if rsu.State.CurrentPosition != nil {
					s.queue.SetStartAt(uint64(rsu.State.CurrentPosition.Value))
				}
				s.player.Pause(rsu.State.PlayingState != PlayingStatePlaying)
				// Only start the player if we are the active renderer; starting
				// while inactive drops quality messages (playerSendMsg is gated on
				// isActive).
				if s.isActive.Load() {
					go s.player.Run()
					if s.cec != nil && rsu.State.PlayingState == PlayingStatePlaying {
						s.cec.OnPlayback(true)
					}
				}
			}
			// else: player running — server is just echoing our own state; ignore.
		} else {
			s.queue.SetIndex(uint64(rsu.State.CurrentQueueIndex))
			if rsu.State.CurrentPosition != nil {
				s.queue.SetStartAt(uint64(rsu.State.CurrentPosition.Value))
			}
		}

	case MsgTypeSrvrRndrSetState:
		if msg.SrvrRndrSetState == nil {
			break
		}
		// While a QueueTracksLoaded is pending the local refs are stale.
		// Drop this message entirely; QueueTracksLoaded will clear the flag and
		// then call wsAskForRendererState to get a fresh SrvrRndrSetState.
		if s.pendingQueueLoad.Load() {
			log.Printf("stream: SrvrRndrSetState dropped — waiting for QueueTracksLoaded (stale queue)")
			break
		}
		// Clear the flag set by QueueTracksLoaded; capture the old value so
		// the bottom block can ignore a stale position the server echoes right
		// after a queue reload (it sends the old playlist's position in the
		// first SrvrRndrSetState for the new queue).
		wasAwaitingRendererState := s.awaitingRendererState.Swap(false)
		state := msg.SrvrRndrSetState
		{
			cur := "<none>"
			if state.CurrentQueueItem != nil {
				cur = fmt.Sprintf("queueItemID=%d", state.CurrentQueueItem.QueueItemID)
			}
			nxt := "<none>"
			if state.NextQueueItem != nil {
				nxt = fmt.Sprintf("queueItemID=%d", state.NextQueueItem.QueueItemID)
			}
			log.Printf("stream: SrvrRndrSetState playing=%d pos=%dms current=%s next=%s",
				state.PlayingState, state.CurrentPosition, cur, nxt)
		}
		if state.QueueVersion != nil {
			s.queue.mu.Lock()
			if s.queue.QueueState == nil {
				s.queue.QueueState = &PbSrvrCtrlQueueState{}
			}
			s.queue.QueueState.QueueVersion = state.QueueVersion
			s.queue.mu.Unlock()
		}
		// restartScheduled is set whenever the upper block kicks off a
		// Restart() or Stop(). The bottom-check must be skipped in that
		// case: Restart() is async, so IsRunning() can momentarily be false
		// between Stop() and the new Run(), and a concurrent Run() from the
		// bottom-check would reset SetStartAt to a stale server position.
		restartScheduled := false
		if s.player.IsRunning() || s.isActive.Load() {
			// Mirror C++ order: position-only seek first, then track change,
			// then pause/play state (matching QobuzStream::WSDecodeMessage).
			if state.HasCurrentPosition && state.CurrentQueueItem == nil &&
			(state.CurrentPosition > 0 || state.NextQueueItem == nil) {
				s.player.RequestSkipTo(int64(state.CurrentPosition))
			} else if state.CurrentQueueItem != nil {
				cur := s.player.CurrentTrack()
				if cur != nil && state.NextQueueItem != nil &&
					cur.Index == state.NextQueueItem.QueueItemID &&
					s.player.CurrentPositionMs() >= 3000 {
					// "Previous" button pressed ≥3s into the track: seek to
					// beginning of the current track (mirrors C++ behaviour).
					s.player.RequestSkipTo(0)
				} else if cur != nil && cur.Index == state.CurrentQueueItem.QueueItemID &&
					state.HasCurrentPosition && state.CurrentPosition == 0 {
					// Server explicitly requests restart of the current track from
					// position 0 (e.g. "Previous" pressed ≥3s into the track sends
					// playing=2 pos=0ms current=<same> next=<next>).
					s.player.RequestSkipTo(0)
				} else if cur != nil && (cur.Index != state.CurrentQueueItem.QueueItemID || wasAwaitingRendererState) &&
				(!s.player.queueExhausted.Load() || wasAwaitingRendererState) {
					// Different track — reposition and restart the player.
					// Guard with queueExhausted: if the queue ran out naturally and
					// SrvrRndrSetState is just an echo of the old position (not a
					// user-initiated play), do not restart. wasAwaitingRendererState
					// overrides this when new content was loaded via QueueTracksLoaded.
					// wasAwaitingRendererState covers the QueueTracksLoaded race:
					// the player may still be running from the old playlist when
					// this SrvrRndrSetState arrives (Stop() is async), and the
					// new queue's first queueItemID can match the old one (both
					// start at 0), so the IDs alone don't detect the switch.
					// Forcing a restart here clears the stale position immediately
					// and prevents RendererStateUpdated from seeding pos=old later.
					s.queue.SetIndex(state.CurrentQueueItem.QueueItemID)
					if s.isActive.Load() {
						// Apply the server's playing state before PrepareRestart so
						// that PrimeState (called inside PrepareRestart) reports the
						// correct PlayingState and the new Run() starts with the right
						// pause flag.  Without this, switching playlists while paused
						// could leave the player paused even when the server says PLAYING.
						// After a new queue load (wasAwaitingRendererState), the server
						// sends PlayingStateStopped as its "auto-play" convention — treat
						// it as playing so audio starts without a second SrvrRndrSetState.
						shouldPlay := state.PlayingState == PlayingStatePlaying ||
							(wasAwaitingRendererState && state.PlayingState == PlayingStateStopped)
						s.player.Pause(!shouldPlay)
						// Immediately report the target track so the server stops
						// resending SrvrRndrSetState while the player is restarting.
						// This also clears currentTrack so any SrvrRndrSetState that
						// arrives mid-restart takes the cur==nil path (SetIndex only),
						// breaking the infinite restart loop.
						primePos := state.CurrentPosition
						if wasAwaitingRendererState {
							primePos = 0
						}
						s.player.PrepareRestart(int32(state.CurrentQueueItem.QueueItemID), primePos)
						s.player.Restart()
					} else {
						s.player.Stop()
					}
					restartScheduled = true
				} else if cur == nil && s.player.IsRunning() {
					// Player is running but hasn't consumed its first track yet
					// (e.g. still downloading). Just update the queue position;
					// stopping here would create an unrecoverable race where
					// IsRunning() is still true when the restart check below
					// fires. The player will pick up the correct track on its
					// own once ConsumeTrack returns.
					s.queue.SetIndex(state.CurrentQueueItem.QueueItemID)
				}
			} else if state.PlayingState == PlayingStatePaused {
				// Mirror C++ state-callback st==3: capture elapsed time into
				// CurrentPosition.Value so sendPlayerState reports the right position.
				nowMs := uint64(time.Now().UnixMilli())
				s.player.stateMu.Lock()
				ps := &s.player.playerState
				if ps.CurrentPosition != nil && ps.PlayingState == PlayingStatePlaying && ps.CurrentPosition.Timestamp != 0 {
					elapsed := nowMs - ps.CurrentPosition.Timestamp
					ps.CurrentPosition.Value += uint32(elapsed)
					ps.CurrentPosition.Timestamp = nowMs
				}
				ps.PlayingState = PlayingStatePaused
				s.player.stateMu.Unlock()
				s.player.Pause(true)
				go s.player.sendPlayerState()
				if s.cec != nil {
					s.cec.OnPlayback(false)
				}
			} else if state.PlayingState == PlayingStatePlaying {
				if s.player.IsRunning() {
					// Mirror C++ state-callback st==1: restart the elapsed-time clock.
					nowMs := uint64(time.Now().UnixMilli())
					s.player.stateMu.Lock()
					ps := &s.player.playerState
					if ps.CurrentPosition != nil {
						ps.CurrentPosition.Timestamp = nowMs
					}
					ps.PlayingState = PlayingStatePlaying
					s.player.stateMu.Unlock()
					s.player.Pause(false)
					go s.player.sendPlayerState()
					if s.cec != nil {
						s.cec.OnPlayback(true)
					}
				} else {
					log.Printf("stream: SrvrRndrSetState playing=2 but player not running (queue exhausted?) — not faking playback state")
				}
			}
		}
		// After a new queue load (wasAwaitingRendererState), also accept
		// PlayingStateStopped as a trigger: the server uses it as an "auto-play"
		// convention and no subsequent PlayingStatePlaying message will arrive.
		wantStart := state.PlayingState == PlayingStatePlaying ||
			state.PlayingState == PlayingStatePaused ||
			(wasAwaitingRendererState && state.PlayingState == PlayingStateStopped)
		// An explicit server play directive (playing=2 with a specific track)
		// overrides queueExhausted. This handles the case where the user appends
		// tracks while the last track is playing: ConsumeTrack sees an empty queue
		// and returns nil before AddTracks fires, so the player exhausts naturally.
		// When the server later sends playing=2 for the newly-added track, we must
		// honour it. player.Run() clears queueExhausted at the top of each run.
		isExplicitPlay := state.PlayingState == PlayingStatePlaying && state.CurrentQueueItem != nil
		if !restartScheduled && s.isActive.Load() && !s.player.IsRunning() &&
			wantStart &&
			(!s.player.queueExhausted.Load() || wasAwaitingRendererState || isExplicitPlay) {
			// Guard: when the queue ran out naturally and the server echoes the old
			// position via SrvrRndrSetState (e.g. in response to wsAskForRendererState),
			// do not restart. wasAwaitingRendererState overrides this when new content
			// was explicitly loaded via QueueTracksLoaded. isExplicitPlay overrides
			// when the server actively directs us to play a specific new track.
			if state.CurrentQueueItem != nil {
				s.queue.SetIndex(state.CurrentQueueItem.QueueItemID)
			}
			// When this SrvrRndrSetState is the first one after QueueTracksLoaded
			// (wasAwaitingRendererState), the server echoes the OLD playlist's
			// playback position in CurrentPosition.  Starting the new queue at
			// that offset is wrong; use 0 so the first track plays from the
			// beginning.
			startPos := state.CurrentPosition
			if wasAwaitingRendererState {
				startPos = 0
			}
			s.queue.SetStartAt(uint64(startPos))
			playing := state.PlayingState == PlayingStatePlaying ||
				(wasAwaitingRendererState && state.PlayingState == PlayingStateStopped)
			s.player.Pause(!playing)
			// Immediately send a buffering heartbeat so the server sees activity
			// at T+0, before the persistent heartbeat fires at T+10s.
			if state.CurrentQueueItem != nil {
				s.player.PrimeState(int32(state.CurrentQueueItem.QueueItemID), startPos)
			}
			go s.player.Run()
			if s.cec != nil && playing {
				s.cec.OnPlayback(true)
			}
		}

	case MsgTypeSrvrCtrlUpdateRenderer:
		// Informational only; no local state update needed.

	case MsgTypeSrvrCtrlRemoveRenderer:
		if msg.SrvrCtrlRemoveRenderer != nil && msg.SrvrCtrlRemoveRenderer.RendererID == s.rendererID {
			log.Printf("stream: renderer %d removed", s.rendererID)
			s.isActive.Store(false)
			if s.player.IsRunning() {
				s.player.Stop()
			}
		}

	case MsgTypeSrvrCtrlQueueCleared:
		if msg.SrvrCtrlQueueCleared != nil && msg.SrvrCtrlQueueCleared.QueueVersion != nil {
			s.queue.mu.Lock()
			if s.queue.QueueState == nil {
				s.queue.QueueState = &PbSrvrCtrlQueueState{}
			}
			s.queue.QueueState.QueueVersion = msg.SrvrCtrlQueueCleared.QueueVersion
			s.queue.mu.Unlock()
		}
		s.queue.DeleteAllTracks()
		if s.player.IsRunning() {
			s.player.Stop()
		}

	case MsgTypeSrvrCtrlQueueTracksReordered:
		// Reorder is reflected by the next QueueState snapshot; no action needed.

	case MsgTypeSrvrCtrlLoopModeSet:
		// Loop mode is informational only; queue repeat logic is server-driven.

	case MsgTypeSrvrCtrlQueueVersionChanged:
		if msg.SrvrCtrlQueueVersionChanged != nil && msg.SrvrCtrlQueueVersionChanged.QueueVersion != nil {
			s.queue.mu.Lock()
			if s.queue.QueueState == nil {
				s.queue.QueueState = &PbSrvrCtrlQueueState{}
			}
			s.queue.QueueState.QueueVersion = msg.SrvrCtrlQueueVersionChanged.QueueVersion
			s.queue.mu.Unlock()
		}

	case MsgTypeSrvrCtrlShuffleModeSet:
		if msg.SrvrCtrlShuffleModeSet != nil {
			s.wsAskForQueueState()
			s.wsAskForRendererState()
		}

	case MsgTypeSrvrRndrSetMaxAudioQuality:
		if msg.SrvrRndrSetMaxAudioQuality == nil {
			break
		}
		ql := msg.SrvrRndrSetMaxAudioQuality.MaxAudioQuality
		log.Printf("stream: SrvrRndrSetMaxAudioQuality ql=%d", ql)
		if ql == 0 {
			// Server sent an empty/zero quality — ignore rather than downgrading
			// the audio format to MP3.
			log.Printf("stream: SrvrRndrSetMaxAudioQuality ql=0 — ignoring (would downgrade to MP3)")
			break
		}
		// Cap at the configured maximum so the app cannot negotiate a higher
		// quality than audio_format allows.
		if cfgMax := formatToQualityValue(s.cfg.AudioFormat); ql > cfgMax {
			log.Printf("stream: SrvrRndrSetMaxAudioQuality ql=%d capped to %d (audio_format config)", ql, cfgMax)
			ql = cfgMax
		}
		format := qualityLevelToFormat(ql)
		log.Printf("stream: set max audio quality %d → format %d", ql, format)
		s.queue.SetAudioFormat(format)
		// Echo back the confirmed quality level.
		if ql > 4 {
			ql = 4
		}
		s.ws.SendBatch([]*PbQConnectMessage{{
			MessageType: MsgTypeRndrSrvrMaxAudioQualityChanged,
			RndrSrvrMaxAudioQuality: &PbRndrSrvrMaxAudioQuality{
				AudioQuality: ql,
				NetworkType:  1, // WiFi
			},
		}})

	case MsgTypeSrvrCtrlMaxAudioQualityChanged:
		log.Printf("stream: SrvrCtrlMaxAudioQualityChanged qualityValue=%d (server→all-clients)", msg.SrvrCtrlMaxAudioQualityChanged)

	case MsgTypeSrvrCtrlFileAudioQualityChanged:
		log.Printf("stream: SrvrCtrlFileAudioQualityChanged qualityValue=%d (server→all-clients)", msg.SrvrCtrlFileAudioQualityChanged)

	case MsgTypeSrvrCtrlDeviceAudioQualityChanged:
		log.Printf("stream: SrvrCtrlDeviceAudioQualityChanged qualityValue=%d (server→all-clients)", msg.SrvrCtrlDeviceAudioQualityChanged)

	case MsgTypeSrvrCtrlAutoplayModeSet:
		log.Printf("stream: AutoplayModeSet mode=%d", msg.SrvrCtrlAutoplayModeSet)
		s.queue.SetAutoplayMode(msg.SrvrCtrlAutoplayModeSet != 0)

	case MsgTypeSrvrCtrlVolumeMuted:
		// Informational; no local state update needed.

	default:
		log.Printf("stream: unhandled message type %d", msg.MessageType)
	}
}

// ---- Utilities ----

// qualityLevelToFormat converts a qconnect quality level (1–4, used in
// DeviceCapabilities and SrvrRndrSetMaxAudioQuality) to an AudioFormat constant.
func qualityLevelToFormat(level int32) int {
	switch level {
	case 2:
		return AudioFormatFLAC
	case 3:
		return AudioFormatHiRes96
	case 4:
		return AudioFormatHiRes192
	default: // 1 or unknown → MP3
		return AudioFormatMP3
	}
}

// applyVolume applies a Qobuz volume change (0–100). When CEC volume control
// is enabled, the amp volume is routed to CEC and the beep level stays fixed
// at 90; otherwise the beep volume is updated directly.
func (s *QobuzStream) applyVolume(vol uint32) {
	if s.cec != nil && s.cfg.CEC.VolumeControl {
		// Keep player.volume in sync so wsSetRendererVolume echoes correctly.
		s.player.volume.Store(vol)
		s.cec.OnVolume(vol)
	} else {
		s.player.SetVolume(vol)
	}
}

func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

