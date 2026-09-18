package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/grandcat/zeroconf"
)

// AppConnectInfo holds the tokens sent by the Qobuz mobile app via connect-to-qconnect.
type AppConnectInfo struct {
	SessionID  string
	WSEndpoint string
	WSJWT      string
	WSExp      uint64
	APIToken   string
	APIExp     uint64
}

// MDNSServer advertises the device over mDNS and serves Qobuz Connect discovery endpoints.
type MDNSServer struct {
	deviceName string
	sessionID  []byte // 16-byte raw UUID
	port       int
	appID      string
	onConnect  func(*AppConnectInfo)
	mu         sync.Mutex
	server     *zeroconf.Server
	httpServer *http.Server
	stopCh     chan struct{}
}

// NewMDNSServer creates an MDNSServer.
func NewMDNSServer(deviceName string, sessionID []byte, port int, appID string, onConnect func(*AppConnectInfo)) *MDNSServer {
	return &MDNSServer{
		deviceName: deviceName,
		sessionID:  sessionID,
		port:       port,
		appID:      appID,
		onConnect:  onConnect,
	}
}

// buildTXT returns the mDNS TXT records for this device.
func (m *MDNSServer) buildTXT() []string {
	return []string{
		"path=/gobz-connect",
		"type=SPEAKER",
		"sdk_version=0.9.6",
		"Name=" + m.deviceName,
		"device_uuid=" + uuidFromBytes(m.sessionID),
	}
}

// Start registers the mDNS service and starts the HTTP server.
func (m *MDNSServer) Start() error {
	uuidStr := uuidFromBytes(m.sessionID)

	var err error
	m.server, err = zeroconf.Register(
		m.deviceName,
		"_qobuz-connect._tcp",
		"local.",
		m.port,
		m.buildTXT(),
		nil, // use all interfaces
	)
	if err != nil {
		return fmt.Errorf("mdns: register: %w", err)
	}
	log.Printf("mdns: registered %q on port %d (uuid=%s)", m.deviceName, m.port, uuidStr)

	m.stopCh = make(chan struct{})
	go m.reannounce()

	mux := http.NewServeMux()
	mux.HandleFunc("/gobz-connect/get-display-info", m.handleDisplayInfo)
	mux.HandleFunc("/gobz-connect/get-connect-info", m.handleConnectInfo)
	mux.HandleFunc("/gobz-connect/connect-to-qconnect", m.handleConnectToQConnect)

	m.httpServer = &http.Server{
		Addr:    fmt.Sprintf(":%d", m.port),
		Handler: logRequests(mux),
	}

	go func() {
		if err := m.httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("mdns: HTTP server error: %v", err)
		}
	}()
	return nil
}

// Stop shuts down the mDNS registration and HTTP server.
func (m *MDNSServer) Stop() {
	if m.stopCh != nil {
		close(m.stopCh)
	}
	m.mu.Lock()
	if m.server != nil {
		m.server.Shutdown()
	}
	m.mu.Unlock()
	if m.httpServer != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		m.httpServer.Shutdown(ctx)
	}
}

// reannounce periodically re-registers the mDNS service to work around a
// Qobuz app bug where only the first PTR record in a batched mDNS response
// is processed, causing the device to go undiscovered.
func (m *MDNSServer) reannounce() {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			m.mu.Lock()
			if m.server != nil {
				m.server.Shutdown()
			}
			s, err := zeroconf.Register(
				m.deviceName,
				"_qobuz-connect._tcp",
				"local.",
				m.port,
				m.buildTXT(),
				nil,
			)
			if err != nil {
				log.Printf("mdns: re-announce error: %v", err)
			} else {
				m.server = s
			}
			m.mu.Unlock()
		case <-m.stopCh:
			return
		}
	}
}

func (m *MDNSServer) handleDisplayInfo(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]interface{}{
		"type":               "SPEAKER",
		"friendly_name":      m.deviceName,
		"model_display_name": m.deviceName,
		"brand_display_name": "GobzConnect",
		"serial_number":      uuidFromBytes(m.sessionID),
		"max_audio_quality":  "HIRES_L3",
	})
}

func (m *MDNSServer) handleConnectInfo(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]interface{}{
		"current_session_id": "",
		"app_id":             m.appID,
	})
}

func (m *MDNSServer) handleConnectToQConnect(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil || len(body) == 0 {
		writeJSON(w, map[string]bool{"success": false})
		return
	}

	var payload struct {
		SessionID   string `json:"session_id"`
		JWTQConnect struct {
			JWT      string `json:"jwt"`
			Endpoint string `json:"endpoint"`
			Exp      uint64 `json:"exp"`
		} `json:"jwt_qconnect"`
		JWTApi struct {
			JWT string `json:"jwt"`
			Exp uint64 `json:"exp"`
		} `json:"jwt_api"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		log.Printf("mdns: connect-to-qconnect parse error: %v", err)
		writeJSON(w, map[string]bool{"success": false})
		return
	}

	log.Printf("mdns: app connected session_id=%s endpoint=%s jwt_api_exp=%d",
		payload.SessionID, payload.JWTQConnect.Endpoint, payload.JWTApi.Exp)

	if m.onConnect != nil {
		m.onConnect(&AppConnectInfo{
			SessionID:  payload.SessionID,
			WSEndpoint: payload.JWTQConnect.Endpoint,
			WSJWT:      payload.JWTQConnect.JWT,
			WSExp:      payload.JWTQConnect.Exp,
			APIToken:   payload.JWTApi.JWT,
			APIExp:     payload.JWTApi.Exp,
		})
	}
	writeJSON(w, map[string]bool{"success": true})
}

// logRequests wraps a handler and logs every incoming HTTP request with its
// method, path, query parameters, and remote address. Useful for discovering
// requests sent by the Qobuz app that are not yet handled.
func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if q := r.URL.RawQuery; q != "" {
			log.Printf("mdns: %s %s?%s (from %s)", r.Method, r.URL.Path, q, r.RemoteAddr)
		} else {
			log.Printf("mdns: %s %s (from %s)", r.Method, r.URL.Path, r.RemoteAddr)
		}
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("mdns: JSON encode error: %v", err)
	}
}
