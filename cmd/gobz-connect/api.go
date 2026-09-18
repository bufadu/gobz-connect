package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

const qobuzAPIBase = "https://www.qobuz.com/api.json/0.2"

// ErrTokenUnavailable is returned by API calls gated on WaitToken when the
// wait timed out (jwt_api still expired) or the stream is shutting down.
// Callers can check errors.Is(err, ErrTokenUnavailable) to distinguish this
// transient condition — recoverable once the app reconnects — from a genuine
// content/API error that should permanently fail the request.
var ErrTokenUnavailable = errors.New("no valid jwt_api (timed out waiting for app reconnect, or shutting down)")

// WSToken holds a WebSocket authentication token.
type WSToken struct {
	JWT      string
	ExpSec   uint64
	Endpoint string
}

// QobuzAPI wraps authenticated Qobuz API requests.
type QobuzAPI struct {
	AppID              string
	AppSecret          string
	UserAuthToken      string
	APIToken           string
	SessionToken       string
	SessionExpiresAtMs  int64 // UTC milliseconds; 0 = unknown
	APITokenExpMs       int64 // UTC milliseconds; 0 = unknown — set from jwt_api.exp
	UserID              string

	// WaitToken, when set, is called before any authenticated REST API request.
	// It should block until a valid API token is available and return false if
	// the caller should abort (e.g. on shutdown). Used in unauthenticated mode
	// to pause metadata/fileURL fetches while jwt_api is expired rather than
	// spinning through 401 errors and exhausting the queue instantly.
	WaitToken func() bool
}

// Login authenticates with email/password and stores the user_auth_token.
func (a *QobuzAPI) Login(email, password string) error {
	params := [][2]string{
		{"email", email},
		{"password", password},
		{"app_id", a.AppID},
	}
	url := qobuzAPIBase + "/user/login?" + buildQuery(params)
	headers := map[string]string{
		"User-Agent":   "Mozilla/5.0 (Windows NT 10.0; Win64; x64; rv:110.0) Gecko/20100101 Firefox/110.0",
		"X-App-Id":     a.AppID,
		"Content-Type": "application/json;charset=UTF-8",
	}
	resp, err := a.getWithHeaders(url, headers)
	if err != nil {
		return fmt.Errorf("login request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("login failed status=%d: %s", resp.StatusCode, string(body))
	}
	var data struct {
		UserAuthToken string `json:"user_auth_token"`
		User          struct {
			ID int `json:"id"`
		} `json:"user"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return fmt.Errorf("login parse: %w", err)
	}
	a.UserAuthToken = data.UserAuthToken
	a.UserID = fmt.Sprintf("%d", data.User.ID)
	log.Printf("api: login ok, user_id=%s", a.UserID)
	return nil
}

// LoginWithToken authenticates using a pre-obtained user_auth_token (e.g. from
// a browser session). Use this when email+password login is blocked by reCAPTCHA.
func (a *QobuzAPI) LoginWithToken(userID, userAuthToken string) error {
	params := [][2]string{
		{"user_id", userID},
		{"user_auth_token", userAuthToken},
		{"app_id", a.AppID},
	}
	url := qobuzAPIBase + "/user/login?" + buildQuery(params)
	headers := map[string]string{
		"User-Agent":   "Mozilla/5.0 (Windows NT 10.0; Win64; x64; rv:110.0) Gecko/20100101 Firefox/110.0",
		"X-App-Id":     a.AppID,
		"Content-Type": "application/json;charset=UTF-8",
	}
	resp, err := a.getWithHeaders(url, headers)
	if err != nil {
		return fmt.Errorf("login request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("login failed status=%d: %s", resp.StatusCode, string(body))
	}
	var data struct {
		UserAuthToken string `json:"user_auth_token"`
		User          struct {
			ID int `json:"id"`
		} `json:"user"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return fmt.Errorf("login parse: %w", err)
	}
	a.UserAuthToken = data.UserAuthToken
	a.UserID = fmt.Sprintf("%d", data.User.ID)
	log.Printf("api: token login ok, user_id=%s", a.UserID)
	return nil
}

// StartSession starts a Qobuz session and stores the session token.
func (a *QobuzAPI) StartSession() error {
	ts := nowSecText(6)
	params := [][2]string{{"profile", "qbz-1"}}
	sig := md5Sig("session", "start", params, ts, a.AppSecret)
	body := buildQuery(params) + "&request_ts=" + ts + "&request_sig=" + sig

	headers := a.commonHeaders()
	headers["Content-Type"] = "application/x-www-form-urlencoded"
	resp, err := a.postWithHeaders(qobuzAPIBase+"/session/start", headers, body)
	if err != nil {
		return fmt.Errorf("startSession request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		log.Printf("api: startSession status=%d: %s", resp.StatusCode, string(b))
		return nil // non-fatal
	}
	var data struct {
		SessionID string `json:"session_id"`
		ExpiresAt int64  `json:"expires_at"`
	}
	b, _ := io.ReadAll(resp.Body)
	if err := json.Unmarshal(b, &data); err == nil && data.SessionID != "" {
		a.SessionToken = data.SessionID
		if data.ExpiresAt > 0 {
			a.SessionExpiresAtMs = data.ExpiresAt * 1000 // API returns seconds
		}
		log.Printf("api: session started, session_id=%s...", truncate(data.SessionID, 8))
	}
	return nil
}

// CreateWSToken creates or refreshes a WebSocket token.
func (a *QobuzAPI) CreateWSToken(existing string) (*WSToken, error) {
	endpoint := "createToken"
	if existing != "" {
		endpoint = "refreshToken"
	}
	headers := a.commonHeaders()
	headers["Content-Type"] = "application/x-www-form-urlencoded"
	resp, err := a.postWithHeaders(qobuzAPIBase+"/qws/"+endpoint, headers, "jwt=jwt_qws")
	if err != nil {
		return nil, fmt.Errorf("createWSToken: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("createWSToken status=%d: %s", resp.StatusCode, string(b))
	}
	respBody, _ := io.ReadAll(resp.Body)
	var data struct {
		JWTQws struct {
			JWT      string `json:"jwt"`
			Exp      int64  `json:"exp"`
			Endpoint string `json:"endpoint"`
		} `json:"jwt_qws"`
	}
	if err := json.Unmarshal(respBody, &data); err != nil {
		return nil, fmt.Errorf("createWSToken parse: %w", err)
	}
	ep := strings.ReplaceAll(data.JWTQws.Endpoint, "%2F", "/")
	ep = strings.ReplaceAll(ep, "%3A", ":")
	return &WSToken{
		JWT:      data.JWTQws.JWT,
		ExpSec:   uint64(data.JWTQws.Exp),
		Endpoint: ep,
	}, nil
}

// GetTrackMetadata fetches track metadata by track_id.
func (a *QobuzAPI) GetTrackMetadata(trackID uint32) (map[string]interface{}, error) {
	if a.WaitToken != nil && !a.WaitToken() {
		return nil, ErrTokenUnavailable
	}
	params := [][2]string{{"track_id", fmt.Sprintf("%d", trackID)}}
	headers := a.commonHeaders()
	resp, err := a.getWithHeaders(qobuzAPIBase+"/track/get?"+buildQuery(params), headers)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("getTrackMetadata status=%d: %s", resp.StatusCode, string(b))
	}
	var result map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}
	return result, nil
}

// GetFileURL fetches the CDN stream URL for a track.
func (a *QobuzAPI) GetFileURL(trackID uint32, formatID int) (map[string]interface{}, error) {
	if a.WaitToken != nil && !a.WaitToken() {
		return nil, ErrTokenUnavailable
	}
	ts := nowSecText(6)
	params := [][2]string{
		{"format_id", fmt.Sprintf("%d", formatID)},
		{"intent", "stream"},
		{"track_id", fmt.Sprintf("%d", trackID)},
	}
	sig := md5Sig("track", "getFileUrl", params, ts, a.AppSecret)
	q := buildQuery(params) + "&request_ts=" + ts + "&request_sig=" + sig
	headers := a.commonHeaders()
	resp, err := a.getWithHeaders(qobuzAPIBase+"/track/getFileUrl?"+q, headers)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("getFileUrl status=%d: %s", resp.StatusCode, string(b))
	}
	var result map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}
	return result, nil
}

// ReportStreamingStart reports the start of a streaming event.
func (a *QobuzAPI) ReportStreamingStart(userID string, trackID uint32, formatID int) {
	body := fmt.Sprintf(`events=[{"user_id":%s,"track_id":%d,"format_id":%d,"date":%s,"duration":0,"online":true,"local":false}]`,
		userID, trackID, formatID, nowSecText(0))
	headers := a.commonHeaders()
	resp, err := a.postWithHeaders(qobuzAPIBase+"/track/reportStreamingStart", headers, body)
	if err != nil {
		log.Printf("api: reportStreamingStart: %v", err)
		return
	}
	resp.Body.Close()
}

// ReportStreamingEnd reports the end of a streaming event.
func (a *QobuzAPI) ReportStreamingEnd(userID string, trackID uint32, blob, contextUUID string, durationSec int, startedAtMs uint64) {
	body := fmt.Sprintf(`{"events":[{"blob":%q,"track_context_uuid":%q,"start_stream":%q,"online":true,"local":false,"duration":%d}],"renderer_context":{"software_version":"go-1.0.0"}}`,
		blob, contextUUID, iso8601FromEpochMs(startedAtMs), durationSec)
	headers := a.commonHeaders()
	headers["Content-Type"] = "application/json"
	resp, err := a.postWithHeaders(qobuzAPIBase+"/track/reportStreamingEndJson", headers, body)
	if err != nil {
		log.Printf("api: reportStreamingEnd: %v", err)
		return
	}
	resp.Body.Close()
}

// GetSuggestions fetches autoplay track suggestions.
func (a *QobuzAPI) GetSuggestions(listenedIDs []uint32, trackContexts []string) ([]uint32, error) {
	ids := make([]string, len(listenedIDs))
	for i, id := range listenedIDs {
		ids[i] = fmt.Sprintf("%d", id)
	}
	ctxJSON := "[]"
	if len(trackContexts) > 0 {
		ctxJSON = "[" + strings.Join(trackContexts, ",") + "]"
	}
	bodyStr := fmt.Sprintf(`{"limit":20,"listened_tracks_ids":[%s],"track_to_analysed":%s}`,
		strings.Join(ids, ","), ctxJSON)
	headers := a.commonHeaders()
	headers["Content-Type"] = "application/json"
	resp, err := a.postWithHeaders(qobuzAPIBase+"/dynamic/suggest", headers, bodyStr)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("getSuggestions status=%d: %s", resp.StatusCode, string(b))
	}
	var result struct {
		Tracks struct {
			Items []struct {
				ID uint32 `json:"id"`
			} `json:"items"`
		} `json:"tracks"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}
	var trackIDs []uint32
	for _, item := range result.Tracks.Items {
		trackIDs = append(trackIDs, item.ID)
	}
	return trackIDs, nil
}

// commonHeaders returns the standard Qobuz API headers.
func (a *QobuzAPI) commonHeaders() map[string]string {
	h := map[string]string{
		"User-Agent": "Mozilla/5.0 (Windows NT 10.0; Win64; x64; rv:110.0) Gecko/20100101 Firefox/110.0",
		"Referer":    "https://play.qobuz.com/",
		"Origin":     "https://play.qobuz.com",
		"X-App-Id":   a.AppID,
	}
	if a.SessionToken != "" {
		h["X-Session-Id"] = a.SessionToken
	}
	if a.UserAuthToken != "" {
		h["X-User-Auth-Token"] = a.UserAuthToken
	} else if a.APIToken != "" {
		h["Authorization"] = "Bearer " + a.APIToken
	}
	return h
}

func (a *QobuzAPI) getWithHeaders(url string, headers map[string]string) (*http.Response, error) {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	return http.DefaultClient.Do(req)
}

func (a *QobuzAPI) postWithHeaders(url string, headers map[string]string, body string) (*http.Response, error) {
	req, err := http.NewRequest("POST", url, strings.NewReader(body))
	if err != nil {
		return nil, err
	}
	if _, ok := headers["Content-Type"]; !ok {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	return http.DefaultClient.Do(req)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// VerifySecret tests the current AppSecret by performing a signed GetFileURL call.
// Returns true if the secret is valid (request succeeds without an error).
func (a *QobuzAPI) VerifySecret() bool {
	if a.AppSecret == "" {
		return false
	}
	result, err := a.GetFileURL(64868955, 6) // track ID used as a canary
	if err != nil {
		return false
	}
	if status, ok := result["status"].(string); ok && status == "error" {
		return false
	}
	return true
}

func iso8601FromEpochMs(ms uint64) string {
	t := time.Unix(int64(ms/1000), int64((ms%1000)*1000000)).UTC()
	return t.Format("2006-01-02T15:04:05.000Z")
}
