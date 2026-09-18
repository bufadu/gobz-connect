package main

import (
	"context"
	"encoding/binary"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

// wsFrame represents a single framed message: [kind uint8][len varint][payload].
// This mirrors the C++ WebSocketClient::pack/parse format.

func packFrame(kind uint8, payload []byte) []byte {
	// Write varint length
	lenBuf := make([]byte, binary.MaxVarintLen64)
	n := binary.PutUvarint(lenBuf, uint64(len(payload)))
	frame := make([]byte, 1+n+len(payload))
	frame[0] = kind
	copy(frame[1:], lenBuf[:n])
	copy(frame[1+n:], payload)
	return frame
}

// parseFrames parses one or more [kind][varint-len][payload] records from buf.
func parseFrames(buf []byte) ([]struct {
	Kind    uint8
	Payload []byte
}, []byte) {
	var out []struct {
		Kind    uint8
		Payload []byte
	}
	for len(buf) >= 2 {
		kind := buf[0]
		payLen, n := binary.Uvarint(buf[1:])
		if n <= 0 {
			break
		}
		start := 1 + n
		if start+int(payLen) > len(buf) {
			break
		}
		payload := make([]byte, payLen)
		copy(payload, buf[start:start+int(payLen)])
		out = append(out, struct {
			Kind    uint8
			Payload []byte
		}{kind, payload})
		buf = buf[start+int(payLen):]
	}
	return out, buf
}

// WsManager manages the Qobuz WebSocket connection with automatic reconnection.
type WsManager struct {
	tokenFn   func() (*WSToken, error) // callback to get a fresh WS token
	onAuth    func()                   // called after authentication succeeds
	onPayload func([]byte)             // called with raw payload bytes (kind==6)

	mu          sync.Mutex
	conn        *websocket.Conn
	isConnected atomic.Bool
	nextID      atomic.Uint32
	lastTxMs    atomic.Uint64
	txMu        sync.Mutex
	token       string
	tokenExpSec uint64
	endpoint    string

	// reconnectNow is signaled by Reconnect() so Run() skips the 3s retry delay.
	// This mirrors C++ which creates a brand-new WsManager (no delay) on app connect.
	reconnectNow     chan struct{}
	intentionalClose atomic.Bool // set by Reconnect()/ColdReconnect(); suppresses the "disconnected" log

	// coldReconnectUntil, when in the future, makes Run() wait this long instead
	// of the normal 3s retry delay before the next connect attempt. Set by
	// ColdReconnect() to simulate the gap a real process restart leaves in the
	// connection to the Qobuz backend — see stream.go's resetWSTokenChain.
	coldReconnectMu    sync.Mutex
	coldReconnectUntil time.Time

	ctx    context.Context
	cancel context.CancelFunc
}

// NewWsManager creates a WsManager with a token supplier.
func NewWsManager(tokenFn func() (*WSToken, error)) *WsManager {
	ctx, cancel := context.WithCancel(context.Background())
	return &WsManager{
		tokenFn:      tokenFn,
		reconnectNow: make(chan struct{}, 1),
		ctx:          ctx,
		cancel:       cancel,
	}
}

func (w *WsManager) OnAuth(f func())          { w.onAuth = f }
func (w *WsManager) OnPayload(f func([]byte)) { w.onPayload = f }

// Run connects and keeps the WebSocket alive, running in the current goroutine.
func (w *WsManager) Run() {
	for {
		select {
		case <-w.ctx.Done():
			return
		default:
		}
		// Fetch a fresh token if needed
		tok, err := w.tokenFn()
		if err != nil {
			log.Printf("ws: token fetch failed: %v; retry in 5s", err)
			time.Sleep(5 * time.Second)
			continue
		}
		w.mu.Lock()
		w.token = tok.JWT
		w.tokenExpSec = tok.ExpSec
		if tok.Endpoint != "" {
			w.endpoint = tok.Endpoint
		}
		ep := w.endpoint
		w.mu.Unlock()

		if ep == "" {
			log.Printf("ws: no endpoint; retry in 5s")
			time.Sleep(5 * time.Second)
			continue
		}
		log.Printf("ws: connecting to %s", truncate(ep, 60))
		if err := w.connectAndRun(ep, tok.JWT); err != nil {
			if !w.intentionalClose.Swap(false) {
				log.Printf("ws: disconnected (%v)", err)
			}
		}
		// If Reconnect() was called intentionally, skip the delay (C++ creates a fresh
		// WsManager immediately on app connect with no sleep). Otherwise wait 3s.
		select {
		case <-w.reconnectNow:
			// intentional reconnect — no delay
		default:
			wait := 3 * time.Second
			w.coldReconnectMu.Lock()
			if d := time.Until(w.coldReconnectUntil); d > wait {
				wait = d
			}
			w.coldReconnectMu.Unlock()
			select {
			case <-w.ctx.Done():
				return
			case <-w.reconnectNow:
				// An intentional Reconnect() (e.g. handleAppConnect got a fresh
				// token) arrived while we were in a ColdReconnect hold — stop
				// waiting and reconnect now instead of sleeping out the rest
				// of the hold with credentials we already have.
			case <-time.After(wait):
			}
		}
	}
}

// Stop shuts down the WsManager permanently.
func (w *WsManager) Stop() {
	w.cancel()
	w.mu.Lock()
	if w.conn != nil {
		w.conn.Close()
	}
	w.mu.Unlock()
}

// Reconnect closes the current connection so Run() reconnects on the next cycle,
// picking up any newly injected token. Unlike Stop(), it does not cancel the context.
// It signals reconnectNow so Run() skips the 3s delay (matching C++ which creates a
// fresh WsManager immediately when the Qobuz app connects via connect-to-qconnect).
func (w *WsManager) Reconnect() {
	w.intentionalClose.Store(true)
	select {
	case w.reconnectNow <- struct{}{}:
	default:
	}
	w.mu.Lock()
	if w.conn != nil {
		w.conn.Close()
	}
	w.mu.Unlock()
}

// ColdReconnect closes the current connection and — unlike Reconnect(), which
// reconnects immediately with a chained (refreshToken) JWT — holds off for
// `hold` before reconnecting. The caller is expected to also clear the token
// chain (stream.go's resetWSTokenChain) so the next connect presents a brand
// new, unchained JWT instead of a seamless refresh.
//
// This exists to test a hypothesis: reading the official Qobuz desktop
// client's source shows that a renderer only gets a fresh jwt_api when the
// app's Redux store no longer lists it as "registered", which only happens
// when the app receives a server-pushed RemoveRenderer message. Our normal
// Reconnect() (chained token, no delay) mirrors the documented "refresh and
// resume the same session" pattern and never triggers that. A real process
// restart does — because the old connection dies without a refresh chain and
// stays down for a beat before a new one appears. This method reproduces
// that gap without actually restarting the process.
func (w *WsManager) ColdReconnect(hold time.Duration) {
	w.intentionalClose.Store(true)
	w.coldReconnectMu.Lock()
	w.coldReconnectUntil = time.Now().Add(hold)
	w.coldReconnectMu.Unlock()
	w.mu.Lock()
	if w.conn != nil {
		w.conn.Close()
	}
	w.mu.Unlock()
}

func (w *WsManager) connectAndRun(endpoint, jwt string) error {
	dialer := websocket.Dialer{
		HandshakeTimeout: 10 * time.Second,
		Subprotocols:     []string{"qws"},
	}
	headers := map[string][]string{
		"Origin":        {"https://play.qobuz.com"},
		"User-Agent":    {"Mozilla/5.0"},
		"Pragma":        {"no-cache"},
		"Cache-Control": {"no-cache"},
	}
	conn, _, err := dialer.DialContext(w.ctx, endpoint, headers)
	if err != nil {
		// Retry without subprotocol
		dialer.Subprotocols = nil
		conn, _, err = dialer.DialContext(w.ctx, endpoint, headers)
		if err != nil {
			return err
		}
	}
	w.mu.Lock()
	w.conn = conn
	w.mu.Unlock()
	// Always clear isConnected when this function returns so that SendBatch
	// stops writing to a dead socket while Run() is blocked in tokenFn.
	defer w.isConnected.Store(false)
	w.isConnected.Store(false)

	// Authenticate and subscribe, then immediately mark connected and fire onAuth.
	// This matches the C++ WsManager onOpen callback which calls on_auth_()
	// synchronously right after sending auth+subscribe, without waiting for
	// server ACK frames.  Waiting for ACK (kind==1 or kind==2) is risky because
	// the server may not send them on every reconnect (e.g. after refreshToken).
	w.sendAuth(jwt)
	w.sendSubscribe()
	w.isConnected.Store(true)
	if w.onAuth != nil {
		go w.onAuth()
	}

	conn.SetPingHandler(func(data string) error {
		// Must hold txMu: ping handler runs in the read goroutine and can race
		// with SendBatch/sendRaw which also write to the same connection.
		w.txMu.Lock()
		defer w.txMu.Unlock()
		conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
		return conn.WriteMessage(websocket.PongMessage, []byte(data))
	})

	const pingInterval = 10 * time.Second
	const pongTimeout = 30 * time.Second
	conn.SetPongHandler(func(string) error {
		conn.SetReadDeadline(time.Now().Add(pongTimeout))
		return nil
	})

	ticker := time.NewTicker(pingInterval)
	defer ticker.Stop()
	errCh := make(chan error, 1)

	// Read loop
	go func() {
		for {
			_, msg, err := conn.ReadMessage()
			if err != nil {
				errCh <- err
				return
			}
			frames, _ := parseFrames(msg)
			for _, f := range frames {
				w.handleFrame(f.Kind, f.Payload)
			}
		}
	}()

	// Keepalive ticker
	for {
		select {
		case <-w.ctx.Done():
			conn.Close()
			return nil
		case err := <-errCh:
			conn.Close()
			w.isConnected.Store(false)
			return err
		case <-ticker.C:
			w.txMu.Lock()
			conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			pingErr := conn.WriteMessage(websocket.PingMessage, nil)
			w.txMu.Unlock()
			if pingErr != nil {
				conn.Close()
				return pingErr
			}
			// Check token expiry (reconnect 60s before expiry).
			// Do NOT call tokenFn() here — it may block in waitForValidToken
			// (e.g. when jwt_api is expired) and freeze the keepalive loop,
			// preventing pings and stalling the errCh reader.  Instead, just
			// close the connection and let Run() call tokenFn at the top of its
			// loop where blocking is safe and isConnected is already false.
			w.mu.Lock()
			exp := w.tokenExpSec
			w.mu.Unlock()
			if exp > 0 && uint64(time.Now().Unix())+60 >= exp {
				log.Printf("ws: jwt_qws expiring in ~60s — reconnecting to refresh token")
				conn.Close()
				return nil
			}
		}
	}
}

func (w *WsManager) handleFrame(kind uint8, payload []byte) {
	switch kind {
	case QCloudMsgPayload:
		if w.onPayload != nil {
			w.onPayload(payload)
		}
	// kind==1 (auth ACK) and kind==2 (subscribe ACK) are silently ignored;
	// onAuth is fired immediately after sendAuth/sendSubscribe (like C++).
	}
}

// SendBatch encodes a QConnectBatch and sends it as a PAYLOAD message.
func (w *WsManager) SendBatch(msgs []*PbQConnectMessage) {
	if !w.isConnected.Load() {
		return
	}
	id := w.nextID.Add(1)
	ts := uint64(time.Now().UnixMilli())
	batchBytes := EncodeBatch(msgs, int32(id), ts)

	payload := EncodePayload(&PbPayload{
		MsgID:   id,
		MsgDate: ts,
		Proto:   QCloudProtoQConnect,
		Dests:   [][]byte{{0x02}},
		Payload: batchBytes,
	})
	w.sendRaw(QCloudMsgPayload, payload)
	w.lastTxMs.Store(ts)
}

func (w *WsManager) sendAuth(jwt string) {
	id := w.nextID.Add(1)
	ts := uint64(time.Now().UnixMilli())
	authBytes := EncodeAuthenticate(&PbAuthenticate{
		MsgID:   id,
		MsgDate: ts,
		JWT:     jwt,
	})
	w.sendRaw(QCloudMsgAuthenticate, authBytes)
}

func (w *WsManager) sendSubscribe() {
	id := w.nextID.Add(1)
	ts := uint64(time.Now().UnixMilli())
	payload := EncodePayload(&PbPayload{
		MsgID:   id,
		MsgDate: ts,
		Proto:   QCloudProtoQConnect,
	})
	w.sendRaw(QCloudMsgSubscribe, payload)
}

func (w *WsManager) sendRaw(kind uint8, payload []byte) {
	w.txMu.Lock()
	defer w.txMu.Unlock()
	w.mu.Lock()
	conn := w.conn
	w.mu.Unlock()
	if conn == nil {
		return
	}
	frame := packFrame(kind, payload)
	conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	if err := conn.WriteMessage(websocket.BinaryMessage, frame); err != nil {
		log.Printf("ws: send error: %v", err)
	}
}
