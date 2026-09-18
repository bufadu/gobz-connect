# Qobuz Connect Renderer Protocol

This document describes the complete protocol used by a **Qobuz Connect renderer** (a network audio playback device) to integrate with the Qobuz ecosystem. It covers credential acquisition, authentication, mDNS device discovery, the WebSocket signalling bus, the QConnect protobuf message protocol, CDN track streaming, and playback state management. A developer reading this document should be able to build a fully functional renderer from scratch.

---

## Table of Contents

1. [Architecture overview](#1-architecture-overview)
2. [App credentials — scraping AppID and AppSecret](#2-app-credentials--scraping-appid-and-appsecret)
3. [REST API authentication](#3-rest-api-authentication)
4. [Session management and heartbeat](#4-session-management-and-heartbeat)
5. [WebSocket token](#5-websocket-token)
6. [mDNS device advertisement](#6-mdns-device-advertisement)
7. [HTTP discovery endpoints](#7-http-discovery-endpoints)
8. [App-initiated connection (connect-to-qconnect)](#8-app-initiated-connection-connect-to-qconnect)
9. [WebSocket transport](#9-websocket-transport)
10. [QConnect protobuf framing](#10-qconnect-protobuf-framing)
11. [Message catalogue](#11-message-catalogue)
12. [Startup sequence](#12-startup-sequence)
13. [Playback state machine](#13-playback-state-machine)
14. [Queue management](#14-queue-management)
15. [Track streaming (CDN)](#15-track-streaming-cdn)
16. [API request signature](#16-api-request-signature)
17. [Audio quality levels](#17-audio-quality-levels)
18. [Device identity and UUID](#18-device-identity-and-uuid)

---

## 1. Architecture overview

```
┌─────────────────────────────────────────────────────────────┐
│                      Qobuz Cloud                             │
│                                                             │
│   REST API          WebSocket (QWS)      CDN                │
│   api.json/0.2      qws-eu-prod.qobuz.com  cdn.qobuz.com   │
└────────┬───────────────────┬──────────────────┬─────────────┘
         │                   │                  │
         │ HTTP/REST         │ WSS              │ HTTPS
         │                   │                  │
┌────────▼───────────────────▼──────────────────▼─────────────┐
│                      Renderer (us)                          │
│                                                             │
│  QobuzAPI       WsManager          Queue / Player           │
│  (auth/meta)    (signalling)        (preload / decode / play)│
└──────────────────────────┬──────────────────────────────────┘
                           │ mDNS  +  HTTP
                           │
              ┌────────────▼────────────┐
              │   Qobuz mobile app      │
              │   (controller/client)   │
              └─────────────────────────┘
```

Three distinct communication channels are used:

| Channel | Protocol | Purpose |
|---|---|---|
| REST API | HTTPS JSON | Authentication, metadata, CDN URLs, streaming reports |
| WebSocket (QWS) | WSS binary (protobuf) | Real-time playback control and state synchronisation |
| mDNS + HTTP | LAN HTTP | Device discovery and app pairing |

---

## 2. App credentials — scraping AppID and AppSecret

Qobuz does not publish API credentials for third-party renderers. They are embedded in the Qobuz web player JavaScript bundles and must be extracted.

### 2.1 AppID

The AppID is a 9-digit decimal string found in the production config object inside a JS bundle:

```
production:{api:{appId:"<9-digit-number>"
```

Regex: `production:\{api:\{appId:"(\d{9})"`

### 2.2 AppSecret derivation

The AppSecret is derived in three steps from values also embedded in the bundles.

**Step 1 — find seed/timezone pairs.**
Each timezone region has its own seed value:

```
.initialSeed("<seed>", window.utimezone.<timezone>)
```

Regex: `\.initialSeed\("([^"]+)",window\.utimezone\.([a-z]+)\)`

Example match: seed = `"abc123..."`, timezone = `"europe"` → key `"Europe"`.

**Step 2 — find info and extras near the timezone anchor.**
After finding the position of `/<timezone>` in the bundle:

```
info:"<info_string>"
extras:"<extras_string>"
```

**Step 3 — combine and decode.**

```
combined = seed + info + extras
encoded  = combined[0 : len(combined)-44]   # strip 44-char HMAC suffix
secret   = base64url_decode(encoded)         # try raw then padded URL-safe base64
```

The resulting `secret` is the `AppSecret` used for signed API calls.

### 2.3 Caching credentials

Store `AppID` and `AppSecret` in a local JSON file (e.g. `qobuz-secrets.json`) to avoid re-scraping on each startup. Verify the cached secret with a test signed call (`track/getFileUrl`) before use; re-scrape if the verification fails.

```json
{
  "app_id": "123456789",
  "app_secret": "the-derived-secret-string"
}
```

Entrypoints to scrape: `https://play.qobuz.com/login` and `https://play.qobuz.com/`. Scan up to 12 JS bundles referenced from the HTML.

---

## 3. REST API authentication

Base URL: `https://www.qobuz.com/api.json/0.2`

All requests must present the `X-App-Id` header with the AppID. After login, add `X-User-Auth-Token` (or `Authorization: Bearer <jwt>` when using a JWT API token).

### 3.1 User login

```
POST /user/login?email=<email>&password=<password>&app_id=<appID>
Header: extra: partner
```

Response (JSON):

```json
{
  "user_auth_token": "<token>",
  "user": { "id": 12345 }
}
```

Store `user_auth_token` and `user.id`. This token does not expire under normal circumstances but should be re-acquired after a credential change.

### 3.2 Common request headers

```
X-App-Id:          <appID>
X-User-Auth-Token: <user_auth_token>   (or omit if using Bearer JWT)
X-Session-Id:      <session_id>        (once a session is started)
Referer:           https://play.qobuz.com/
Origin:            https://play.qobuz.com
```

---

## 4. Session management and heartbeat

A Qobuz session (`session_id`) is required for some API calls.

### 4.1 Starting a session

```
POST /session/start
Body (form-encoded): profile=qbz-1&request_ts=<ts>&request_sig=<sig>
```

The request is **signed** (see §16). Object = `"session"`, method = `"start"`, params = `[["profile","qbz-1"]]`.

Response (JSON):

```json
{
  "session_id": "<uuid-like-string>",
  "expires_at": 1700000000
}
```

Store `session_id` in `X-Session-Id` for subsequent calls. `expires_at` is a Unix timestamp in **seconds**.

### 4.2 Session renewal

Poll every 30 seconds. When `now + 60s >= expires_at`, call `session/start` again to obtain a new session.

```
if now_ms + 60000 >= session_expires_at_ms:
    call StartSession()
```

---

## 5. WebSocket token

A short-lived JWT is needed to authenticate with the WebSocket server.

### 5.1 Creating a token

```
POST /qws/createToken
Body (form-encoded): jwt=jwt_qws
```

### 5.2 Refreshing an existing token

```
POST /qws/refreshToken
Body (form-encoded): jwt=jwt_qws
```

Use `refreshToken` when an existing JWT is available (e.g. one provided by the mobile app via mDNS). Use `createToken` for the very first token.

Response (JSON):

```json
{
  "jwt_qws": {
    "jwt":      "<jwt-string>",
    "exp":      1700001234,
    "endpoint": "wss://qws-eu-prod.qobuz.com/ws"
  }
}
```

The `endpoint` field may be URL-encoded (`%2F`, `%3A`); decode it before use.

### 5.3 Token lifecycle

- The `exp` field is a Unix timestamp in seconds.
- Refresh the token **60 seconds before expiry**. Check expiry on every WebSocket keepalive tick (every 10 seconds).
- When the token is refreshed, close the current WebSocket connection and reconnect with the new token — the JWT is only validated at connection time (in the `Authenticate` frame), not mid-session.

---

## 6. mDNS device advertisement

The renderer advertises itself on the local network using DNS-SD / mDNS so that Qobuz mobile apps can discover it.

### 6.1 Service type

```
_qobuz-connect._tcp.local.
```

### 6.2 TXT records

| Key | Value | Notes |
|---|---|---|
| `path` | `/streamcore` | URL prefix for HTTP discovery endpoints |
| `type` | `SPEAKER` | Device category |
| `sdk_version` | `sc32-1.0.0` | Protocol version string |
| `Name` | `<device name>` | Human-readable display name |
| `device_uuid` | `<uuid>` | Stable UUID derived from device name (see §18) |

### 6.3 Port

The renderer listens on an arbitrary TCP port (default `1984`) for both mDNS advertisement and the HTTP discovery server.

---

## 7. HTTP discovery endpoints

When a Qobuz app discovers the renderer via mDNS, it makes HTTP requests to the renderer's HTTP server using the path prefix from the `path` TXT record.

### 7.1 GET /streamcore/get-display-info

Returns human-readable device metadata.

```json
{
  "type":               "SPEAKER",
  "friendly_name":      "My Renderer",
  "model_display_name": "My Renderer",
  "brand_display_name": "QobuzConnect",
  "serial_number":      "xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx",
  "max_audio_quality":  "HIRES_L3"
}
```

`serial_number` = the device UUID in standard hyphenated lowercase hex format.
`max_audio_quality` values: `"MP3"`, `"FLAC"`, `"HIRES_L1"` (96 kHz), `"HIRES_L2"`, `"HIRES_L3"` (192 kHz).

### 7.2 GET /streamcore/get-connect-info

Returns the current session and AppID so the app can identify the Qobuz account context.

```json
{
  "current_session_id": "",
  "app_id":             "123456789"
}
```

`current_session_id` can be empty if no session is active.

### 7.3 POST /streamcore/connect-to-qconnect

The app posts its own JWT tokens to the renderer. This is how the renderer joins the app's active WebSocket session.

Request body (JSON):

```json
{
  "session_id": "<session-id>",
  "jwt_qconnect": {
    "jwt":      "<ws-jwt>",
    "endpoint": "wss://qws-eu-prod.qobuz.com/ws",
    "exp":      1700001234
  },
  "jwt_api": {
    "jwt": "<api-jwt>",
    "exp": 1700001234
  }
}
```

The renderer must:
1. Store `jwt_api.jwt` as its `Authorization: Bearer` token for subsequent REST calls.
2. Store `session_id` as the `X-Session-Id` header value.
3. Store `jwt_qconnect.jwt` as the "existing JWT" to be passed to `refreshToken`.
4. **Close and reconnect the WebSocket** immediately (no delay), calling `refreshToken` with the app's JWT so both parties land on the same server-side session.

Response: empty JSON object `{}`.

---

## 8. App-initiated connection (connect-to-qconnect)

The sequence when a user selects the renderer from the Qobuz mobile app:

```
App                          Renderer                    Qobuz WS Server
 │                               │                             │
 │── GET get-display-info ───────►│                             │
 │◄─ 200 device info ────────────│                             │
 │                               │                             │
 │── GET get-connect-info ────────►│                             │
 │◄─ 200 {app_id, session_id} ───│                             │
 │                               │                             │
 │── POST connect-to-qconnect ───►│                             │
 │   {session_id, jwt_qconnect,  │                             │
 │    jwt_api}                   │                             │
 │◄─ 200 {} ─────────────────────│                             │
 │                               │                             │
 │                               │─ close old WS connection ──►│
 │                               │                             │
 │                               │─ POST refreshToken ─────────►(REST API)
 │                               │◄─ new JWT ──────────────────│
 │                               │                             │
 │                               │─ WSS connect ───────────────►│
 │                               │─ Authenticate(new JWT) ─────►│
 │                               │─ Subscribe ─────────────────►│
 │                               │◄─ SessionState ─────────────│
 │                               │─ JoinSession ───────────────►│
 │                               │◄─ AddRenderer ──────────────│
 │                               │─ SetActiveRenderer ─────────►│
 │                               │◄─ ActiveRendererChanged ─────│
```

The key insight: the app's JWT and the renderer's JWT must be from the same refresh chain. By calling `refreshToken` with the app's JWT, the renderer enters the same server-side session.

---

## 9. WebSocket transport

### 9.1 Connection

```
URL:            wss://qws-eu-prod.qobuz.com/ws   (or endpoint from token)
Subprotocol:    qws
Headers:
  Origin:        https://play.qobuz.com
  User-Agent:    Mozilla/5.0
  Pragma:        no-cache
  Cache-Control: no-cache
```

If the connection with subprotocol `qws` fails, retry without a subprotocol.

### 9.2 Binary framing

All WebSocket messages are **binary**. A single WS message may contain **one or more frames**. Each frame is:

```
[kind: uint8][payload_length: varint][payload: bytes]
```

- `kind` determines the frame type (see below).
- `payload_length` is an unsigned LEB128 (variable-length integer).
- `payload` is a protobuf-encoded message.

**Frame kinds:**

| Kind | Name | Direction | Description |
|---|---|---|---|
| 1 | Authenticate | Client→Server | JWT authentication |
| 2 | Subscribe | Client→Server | Subscribe to the QConnect protocol |
| 6 | Payload | Both | Main QConnect message payload |

### 9.3 Authentication handshake

Immediately after connecting, without waiting for any server response, the renderer sends two frames in order:

1. **Authenticate** (kind=1): contains the JWT
2. **Subscribe** (kind=2): contains a Payload envelope with proto=QCloudProtoQConnect (1) and no inner payload

After sending both frames, the renderer is considered connected. Server ACK frames (kind=1 or kind=2) may or may not arrive; do not wait for them.

### 9.4 Keepalive

The renderer sends a WebSocket **Ping** every 10 seconds. If a Pong is not received within 30 seconds, consider the connection dead and reconnect.

Incoming server Pings (rare) must be answered with a Pong immediately.

### 9.5 Reconnection

- On unintentional disconnect: wait 3 seconds, then reconnect (fetch a new token first).
- On intentional reconnect (e.g. after `connect-to-qconnect`): reconnect immediately with no delay.
- On token expiry: close the connection, refresh the token, reconnect.

---

## 10. QConnect protobuf framing

### 10.1 Payload envelope (kind=6)

Every kind=6 frame carries a `Payload` protobuf message:

```
Payload {
  field 1  varint  msg_id        (incrementing counter)
  field 2  varint  msg_date      (Unix timestamp ms)
  field 3  varint  proto         (1 = QCloudProtoQConnect)
  field 4  bytes   src           (source address; omit when sending)
  field 5  bytes   dests[]       (destination routing; use [0x02] when sending)
  field 7  bytes   payload       (serialised QConnectBatch)
}
```

### 10.2 QConnectBatch

The inner payload is a `QConnectBatch`:

```
QConnectBatch {
  field 1  fixed64  messages_time  (Unix timestamp ms)
  field 2  int32    messages_id    (same counter as Payload.msg_id)
  field 3  bytes[]  messages[]     (one or more serialised QConnectMessage)
}
```

### 10.3 QConnectMessage

Each message in the batch is a protobuf `QConnectMessage`. All messages carry `message_type` in field 1. The remaining fields depend on the message type and are described in §11.

```
QConnectMessage {
  field 1  int32   message_type
  field N  bytes   <type-specific submessage>
}
```

Field numbers for the submessages correspond directly to the `message_type` value divided into logical groups. The exact wire field numbers are documented per-message in §11.

### 10.4 Sending a message

1. Serialise the `QConnectMessage` protobuf.
2. Wrap it in a `QConnectBatch` (can batch multiple messages).
3. Wrap the batch in a `Payload` with `proto=1`, `dests=[[0x02]]`.
4. Encode the Payload as protobuf.
5. Frame it: `[0x06][varint(len(payload))][payload]`.
6. Send as a binary WebSocket message.

---

## 11. Message catalogue

Messages are grouped by direction:

### 11.1 Renderer → Server (type 23–28)

These report the renderer's current state to the server. The server broadcasts them as `SrvrCtrl*` messages to all controllers (mobile apps).

---

#### RndrSrvrStateUpdated (type 23)
Reports the full playback state. Send after each track change, pause/resume, seek, and on the 10-second heartbeat.

QConnectMessage field: **23**

```
QueueRendererState {
  field 1  int32   playing_state        (0=unknown, 1=stopped, 2=playing, 3=paused)
  field 2  int32   buffer_state         (0=unknown, 1=buffering, 2=ok)
  field 3  bytes   current_position {
              field 1  fixed64 timestamp   (Unix timestamp ms when position was captured)
              field 2  uint32  value        (position in milliseconds)
           }
  field 4  uint32  duration             (track duration in milliseconds)
  field 5  bytes   queue_version        (current queue version)
  field 6  uint32  current_queue_item_id
  field 7  uint32  next_queue_item_id   (-1 if no next track)
}
```

The `position.timestamp` is critical: controllers use `now - timestamp + position.value` to estimate real-time position without polling.

---

#### RndrSrvrVolumeChanged (type 25)
Reports current volume (0–100).

QConnectMessage field: **25**

```
field 1  uint32  volume  (0–100)
```

---

#### RndrSrvrFileAudioQualityChanged (type 26)
Reports the quality of the currently-playing file. Send when a new track starts playing.

QConnectMessage field: **26**

```
field 1  int32  sampling_rate    (Hz, e.g. 44100)
field 2  int32  bit_depth        (e.g. 16 or 24)
field 3  int32  nb_channels      (e.g. 2)
field 4  int32  audio_quality    (1=MP3, 2=FLAC, 3=HiRes-96, 4=HiRes-192)
```

---

#### RndrSrvrDeviceAudioQualityChanged (type 27)
Reports the maximum quality the audio hardware can output. Send at startup and after reconnect.

QConnectMessage field: **27**

```
field 1  int32  sampling_rate
field 2  int32  bit_depth
field 3  int32  nb_channels
```

Note: **no `audio_quality` field** — this message describes hardware capability, not format.

---

#### RndrSrvrMaxAudioQualityChanged (type 28)
Reports the user-selected maximum streaming quality. Send at startup and after becoming active.

QConnectMessage field: **28**

```
field 1  int32  audio_quality    (1–4; use 4 for HiRes-192 capable renderers)
field 2  int32  network_type     (1 = WiFi)
```

---

### 11.2 Server → Renderer (type 41–44)

Commands from the server telling the renderer what to do.

---

#### SrvrRndrSetState (type 41)
The primary playback command. Sent by the server when the user presses Play, Pause, Skip, Seek, or switches playlists.

QConnectMessage field: **41**

```
field 1  int32           playing_state
field 2  uint32          current_position    (ms; only valid when has_current_position=true)
field 3  bool            has_current_position
field 4  bytes           queue_version
field 5  bytes           current_queue_item  (QueueTrackRef: queue_item_id, track_id, context_uuid)
field 6  bytes           next_queue_item     (QueueTrackRef)
```

**Handling logic (ordered):**

1. **Position-only seek**: `has_current_position=true` AND `current_queue_item=nil` AND (`position > 0` OR `next_queue_item=nil`) → seek the current track to `current_position`.
2. **Track change**: `current_queue_item` is set AND its `queue_item_id` differs from the currently-playing track → update the queue index and restart playback at `current_position`.
3. **Pause/resume**: neither of the above AND `playing_state` changes → call Pause or Play.

A special-case "null" message (`playing_state=0`, `current_queue_item=nil`) is sent by the server as a transitional state after queue loads. **Ignore it** — do not use it to start or stop the player.

---

#### SrvrRndrSetVolume (type 42)
Sets absolute or relative volume.

QConnectMessage field: **42**

```
field 1  uint32  volume        (absolute 0–100; use when != 0 or volume_delta == 0)
field 2  int32   volume_delta  (relative change; use when volume == 0 and delta != 0)
```

---

#### SrvrRndrSetActive (type 43)
Tells the renderer whether it is the currently-selected playback device.

QConnectMessage field: **43**

```
field 1  bool  active
```

When `active=true`: re-register volume and quality. When `active=false`: stop playback.

---

#### SrvrRndrSetMaxAudioQuality (type 44)
Overrides the maximum streaming quality. The renderer should respect this limit in subsequent `track/getFileUrl` calls.

QConnectMessage field: **44**

```
field 1  int32  max_audio_quality  (1–4)
```

---

### 11.3 Controller → Server (type 61–79)

In Qobuz Connect, a **controller** is any client that manages the session: it receives queue and state events and can issue commands. The Qobuz mobile app is a pure controller. A renderer is **both** — it plays audio AND acts as a controller simultaneously.

There is no separate "register as controller" step. Sending `CtrlSrvrJoinSession` (type 61) with a `DeviceInfo` of `type=SPEAKER` achieves both registrations at once: the server adds the device to the controller event bus (so it receives all `SrvrCtrl*` broadcasts) and simultaneously recognises it as a renderer (triggering `SrvrCtrlAddRenderer` and future `SrvrRndr*` commands). A pure controller would use a different device type and would not receive `SrvrRndr*` playback commands.

These are messages the renderer sends in its controller role (joining the session, managing the queue).

---

#### CtrlSrvrJoinSession (type 61)
Registers the renderer with the server session. Send immediately after authentication.

QConnectMessage field: **61**

```
SessionUUID:  omit   (do NOT set; setting it causes server error 10002)
DeviceInfo {
  field 1  bytes   device_uuid       (16-byte raw UUID)
  field 2  string  friendly_name     ("My Renderer")
  field 6  int32   type              (1 = SPEAKER)
  field 7  bytes   capabilities {
              field 1  int32  min_audio_quality   (1)
              field 2  int32  max_audio_quality   (4)
              field 3  int32  volume_remote_ctrl  (2 = supported)
           }
  field 10 string  software_version  ("go-1.0.0" or similar)
}
```

---

#### CtrlSrvrSetActiveRenderer (type 63)
Tells the server that this renderer is the active one. Send after receiving `SrvrCtrlActiveRendererChanged` with our ID, and whenever the user selects the renderer.

QConnectMessage field: **63**

```
field 1  int64  renderer_id   (the ID assigned by the server in SrvrCtrlAddRenderer)
```

---

#### CtrlSrvrAskForQueueState (type 76)
Requests the full queue state from the server.

QConnectMessage field: **76**

```
field 1  bytes   queue_version   (current known version; omit if unknown)
field 2  bytes   queue_uuid      (our session UUID)
```

---

#### CtrlSrvrAskForRendererState (type 77)
Requests the current renderer playback state from the server.

QConnectMessage field: **77**

```
field 1  uint64  session_id   (from SrvrCtrlSessionState)
```

---

#### CtrlSrvrAutoplayAddTracks (type 79)
Adds autoplay (radio) tracks to the queue.

QConnectMessage field: **79**

```
field 1  bytes     queue_version
field 2  bytes     action_uuid
field 3  uint32[]  track_ids
field 4  bytes     context_uuid
```

---

### 11.4 Server → Controllers (type 81–105)

The server broadcasts these to all session participants registered as controllers — including our renderer, which joined as both a renderer and a controller via a single `CtrlSrvrJoinSession` call (see §11.3).

---

#### SrvrCtrlSessionState (type 81)
Sent by the server after `JoinSession` to provide the session context.

```
field 1  bytes   session_uuid
field 2  uint64  session_id      (used in AskForRendererState)
field 3  bytes   queue_version
```

On receipt: store `session_id`, update queue version, then call `AskForQueueState` (and `AskForRendererState` if player is not running).

---

#### SrvrCtrlRendererStateUpdated (type 82)
Broadcast of the current renderer state to all controllers. The server echoes the renderer's own `RndrSrvrStateUpdated` back, including to the renderer itself.

```
field 1  uint64   renderer_id
field 2  bytes    state {
           field 1  int32   playing_state
           field 2  int32   buffer_state
           field 3  bytes   current_position {timestamp, value}
           field 4  uint32  duration
           field 5  uint32  current_queue_index
           field 6  int32   next_queue_item_id
         }
```

**When to act:** Only act when the renderer is **not** currently playing (player not running). If the player is running, ignore this message — it is a stale echo of your own state and applying it would rewind playback. If the player is stopped, use the state to seed the queue position and start playback.

---

#### SrvrCtrlAddRenderer (type 83)
Sent by the server to announce a renderer to all controllers. The renderer receives this for itself with its assigned `renderer_id`.

```
field 1  uint64      renderer_id
field 2  bytes       renderer (DeviceInfo)
```

Match using `DeviceInfo.device_uuid`. Store `renderer_id` for use in `CtrlSrvrSetActiveRenderer`.

---

#### SrvrCtrlActiveRendererChanged (type 86)
Broadcast when a different renderer becomes active.

```
field 1  uint64  renderer_id
```

If `renderer_id == our_renderer_id`: confirm active status, resend quality and volume, ask for renderer state (or resend state if already playing). Otherwise: stop playback.

---

#### SrvrCtrlVolumeChanged (type 87)
Broadcast of volume change by any controller.

```
field 1  uint64  renderer_id
field 2  uint32  volume  (0–100)
```

Apply only when `renderer_id == our_renderer_id`.

---

#### SrvrCtrlQueueErrorMessage (type 88)
Server-reported queue error.

```
field 1  bytes  queue_version
field 2  bytes  action_uuid
field 3  bytes  error {code, message}
```

On `"Queue version mismatch"`: update local queue version from the error, then call `AskForQueueState`.
On `"Current track not found in queue nor autoplay"`: call `AskForQueueState` to resync.

---

#### SrvrCtrlQueueState (type 90)
Full queue state snapshot, sent in response to `AskForQueueState`.

```
field 1  bytes             queue_version
field 2  bytes             action_uuid
field 3  bytes[]           tracks          (QueueTrackRef[])
field 4  bool              shuffle_mode
field 5  uint32[]          shuffled_track_indexes
field 6  bool              autoplay_mode
field 7  bytes[]           autoplay_tracks  (QueueTrackRef[])
```

`QueueTrackRef`:
```
field 1  uint64  queue_item_id
field 2  uint32  track_id
field 3  bytes   context_uuid  (16-byte UUID; identifies album/playlist context)
```

Calling this `ConsumeQueueState`: update the local queue refs, shuffle indexes, and autoplay state. Do **not** restart the player if it is already running — only update the queue structure.

---

#### SrvrCtrlQueueTracksLoaded (type 91)
A completely new playlist has been loaded, replacing the current queue.

```
field 1  bytes    queue_version
field 2  bytes    action_uuid
field 3  bytes[]  tracks        (QueueTrackRef[])
field 4  bytes    context_uuid
```

On receipt:
1. **Stop the player** (important: set a `awaitingRendererState` flag first so stale state echoes do not restart it at the wrong position).
2. Clear the queue (`DeleteAllTracks`).
3. Load the new tracks.
4. Call `AskForQueueState` and `AskForRendererState`.
5. The subsequent `SrvrRndrSetState` will provide the correct starting track and position. **Ignore `CurrentPosition` in that first `SrvrRndrSetState`** — it carries the old playlist's position, not a seek point for the new track. Always start at position 0.

---

#### SrvrCtrlQueueTracksInserted (type 92)
Tracks inserted at a specific position.

```
field 1  bytes    queue_version
field 2  bytes    action_uuid
field 3  bytes[]  tracks
field 4  int32    insert_after    (queue_item_id of the track to insert after)
field 5  bytes    context_uuid
field 6  bool     autoplay_reset
```

---

#### SrvrCtrlQueueTracksAdded (type 93)
Tracks appended to the end of the queue.

```
field 1  bytes    queue_version
field 2  bytes    action_uuid
field 3  bytes[]  tracks
field 4  bytes    context_uuid
field 5  bool     autoplay_reset
```

---

#### SrvrCtrlQueueTracksRemoved (type 94)
Tracks removed from the queue.

```
field 1  bytes     queue_version
field 2  bytes     action_uuid
field 3  uint32[]  queue_item_ids
field 4  bool      autoplay_reset
```

---

#### SrvrCtrlShuffleModeSet (type 96)
```
field 1  bool  shuffle_on
```

---

#### SrvrCtrlLoopModeSet (type 97)
```
field 1  int32  mode   (1=off, 2=repeat-one, 3=repeat-all)
```

---

#### SrvrCtrlMaxAudioQualityChanged (type 99)
Server echo of `RndrSrvrMaxAudioQualityChanged`. Informational; no action needed.

---

#### SrvrCtrlFileAudioQualityChanged (type 100)
Server echo of `RndrSrvrFileAudioQualityChanged`. Informational.

---

#### SrvrCtrlDeviceAudioQualityChanged (type 101)
Server echo of `RndrSrvrDeviceAudioQualityChanged`. Informational.

---

#### SrvrCtrlAutoplayTracksLoaded (type 103)
New autoplay (radio) tracks loaded by the server.

```
field 1  bytes    queue_version
field 2  bytes    action_uuid
field 3  bytes[]  tracks
field 4  bytes    context_uuid
```

If the renderer itself triggered this (by calling `CtrlSrvrAutoplayAddTracks`): update context UUIDs on existing track refs. Otherwise: replace the local autoplay track list.

---

#### SrvrCtrlAutoplayTracksRemoved (type 104)
```
field 1  bytes     queue_version
field 3  uint32[]  queue_item_ids
```

---

#### SrvrCtrlQueueVersionChanged (type 105)
```
field 1  bytes  queue_version
```

---

## 12. Startup sequence

```
1. Load or scrape AppID + AppSecret
2. Login (POST /user/login)
3. StartSession (POST /session/start)
4. CreateWSToken (POST /qws/createToken)
5. Connect WebSocket (WSS)
6. Send Authenticate(jwt) frame
7. Send Subscribe frame
8. ──────────────── onAuth callback ─────────────────────
9. Send CtrlSrvrJoinSession (with DeviceInfo)
   Server responds: SrvrCtrlSessionState → store session_id, queue_version
                    SrvrCtrlAddRenderer  → store renderer_id
10. Send CtrlSrvrAskForQueueState
    Server responds: SrvrCtrlQueueState → populate queue
11. Send CtrlSrvrAskForRendererState
    Server responds: SrvrRndrSetState → seed queue index + position → start player
12. (If active) Send SrvrRndrSetActive confirmation + CtrlSrvrSetActiveRenderer
13. Start playing
```

On every reconnect (steps 5–12 repeat). The player heartbeat (§13.3) must run continuously during reconnects — **it must not be tied to the playback goroutine lifecycle**.

---

## 13. Playback state machine

### 13.1 State values

| Constant | Value | Meaning |
|---|---|---|
| `PlayingStateUnknown` | 0 | Not set (transitional, ignore) |
| `PlayingStateStopped` | 1 | Stopped |
| `PlayingStatePlaying` | 2 | Playing |
| `PlayingStatePaused` | 3 | Paused |

| Constant | Value | Meaning |
|---|---|---|
| `BufferStateUnknown` | 0 | Not set |
| `BufferStateBuffering` | 1 | Downloading / initialising |
| `BufferStateOK` | 2 | Playing normally |

### 13.2 State reporting

Report state via `RndrSrvrStateUpdated` in these situations:

- When a new track starts playing (with `buffer_state=BUFFERING` immediately, then `OK` once playback begins).
- When playback pauses or resumes.
- When a seek completes.
- On a **10-second heartbeat** (so controllers can update their progress bars).
- Immediately before starting the player after a `SrvrRndrSetState` (send a BUFFERING state with the target queue item ID so the server sees activity before the 10-second tick).

The `current_position.timestamp` field must be the wall-clock time (Unix ms) at which `current_position.value` was measured. Controllers extrapolate: `displayed_position = value + (now - timestamp)`.

### 13.3 Heartbeat

A background goroutine (independent of the playback goroutine) sends `RndrSrvrStateUpdated` every 10 seconds. This goroutine must survive player Start/Stop cycles; if it stops when the player stops (e.g. during a playlist switch), the server will drop the WebSocket connection within ~10 seconds.

### 13.4 Gapless playback

Maintain a gapless buffer: while track N is playing, preload track N+1 so it can be seamlessly chained. Signal `next_queue_item_id` in the state reports so the server knows what is coming.

---

## 14. Queue management

### 14.1 Track references

The server sends tracks as lightweight references (`QueueTrackRef`), not full metadata:

```
queue_item_id  : uint64  — unique ID within the queue session
track_id       : uint32  — Qobuz track ID for REST API lookups
context_uuid   : bytes   — 16-byte UUID identifying the album/playlist context
                           (used for streaming reports and autoplay suggestions)
```

### 14.2 Preloading pipeline

For each track reference, the renderer runs a pipeline:

```
TrackQueued
   ↓ GET /track/get?track_id=<id>
TrackStreamable   (title, duration, artist, album, blob field, sample rate, bit depth)
   ↓ GET /track/getFileUrl?track_id=<id>&format_id=<fmt>&...  (signed)
TrackReady        (CDN URL available)
```

Maintain a **rolling preload buffer of 3 tracks** ahead of the current playing position. This hides network latency and avoids playback gaps.

### 14.3 Shuffle

When shuffle is active, the server provides `shuffled_track_indexes` — an array of indexes into the `tracks` array. Traverse the queue in that order instead of sequentially.

When new tracks are added to a shuffled queue (e.g. autoplay), extend the local shuffle index array so the new tracks are reachable.

### 14.4 Autoplay (radio mode)

When `autoplay_mode=true` and the queue is running low (fewer than 2 tracks ahead), fetch suggestions:

```
POST /dynamic/suggest
Body (JSON):
{
  "limit": 20,
  "listened_tracks_ids": [<last N track IDs played>],
  "track_to_analysed": [{"track_id": X, "artist_id": Y, "label_id": Z, "genre_id": W}, ...]
}
```

Then push the returned IDs to the server as `CtrlSrvrAutoplayAddTracks`. The server will respond with `SrvrCtrlAutoplayTracksLoaded`.

### 14.5 Queue versioning

The server maintains a `QueueVersion {major: uint64, minor: int32}`. Every queue mutation increments the version. Always include the current version in `AskForQueueState` so the server can detect divergence. On `"Queue version mismatch"` error: update the local version from the error payload and re-request the full state.

---

## 15. Track streaming (CDN)

### 15.1 Metadata fetch

```
GET /track/get?track_id=<id>
Headers: X-App-Id, X-User-Auth-Token, X-Session-Id
```

Key fields in the response:

| Field path | Type | Notes |
|---|---|---|
| `streamable` | bool | Must be true to proceed |
| `title` | string | Track title |
| `duration` | int | Duration in seconds |
| `performer.name` | string | Artist name |
| `performer.id` | int | Artist ID (for suggestions) |
| `album.title` | string | |
| `album.id` | string | |
| `album.image.large` | string | Cover art URL |
| `album.genre.id` | int | Genre ID (for suggestions) |
| `album.label.id` | int | Label ID (for suggestions) |
| `audio_info.resampling.maximum_sampling_rate` | float | Max quality available |
| `audio_info.resampling.maximum_bit_depth` | int | |
| `streaming_url` (or `blob`) | string | Used in streaming reports |

### 15.2 CDN URL fetch (signed request)

```
GET /track/getFileUrl?format_id=<fmt>&intent=stream&track_id=<id>
                     &request_ts=<ts>&request_sig=<sig>
Headers: X-App-Id, X-User-Auth-Token, X-Session-Id
```

The `request_sig` is an MD5 signature (see §16). Object = `"track"`, method = `"getFileUrl"`, params = `[["format_id","<fmt>"],["intent","stream"],["track_id","<id>"]]`.

Response:
```json
{
  "url": "https://cdn.qobuz.com/...",
  "format_id": 6,
  "sampling_rate": 44100,
  "bit_depth": 16
}
```

The CDN URL is a pre-signed time-limited HTTPS URL. Download or stream directly via HTTP GET.

### 15.3 Audio formats

| format_id | Codec | Max quality |
|---|---|---|
| 5 | MP3 320 kbps | — |
| 6 | FLAC 16-bit | CD quality |
| 7 | FLAC 24-bit | HiRes up to 96 kHz |
| 27 | FLAC 24-bit | HiRes up to 192 kHz |

### 15.4 Streaming reports

**Report stream start** (fire-and-forget, non-blocking):

```
POST /track/reportStreamingStart
Body (form-encoded):
events=[{
  "user_id":   <user_id>,
  "track_id":  <track_id>,
  "format_id": <format_id>,
  "date":      <unix_timestamp>,
  "duration":  0,
  "online":    true,
  "local":     false
}]
```

**Report stream end** (fire-and-forget):

```
POST /track/reportStreamingEndJson
Content-Type: application/json
Body:
{
  "events": [{
    "blob":                <blob from track metadata>,
    "track_context_uuid":  <context_uuid as hyphenated UUID string>,
    "start_stream":        <ISO8601 timestamp of when playback started>,
    "online":              true,
    "local":               false,
    "duration":            <seconds actually played>
  }],
  "renderer_context": {
    "software_version": "go-1.0.0"
  }
}
```

Both calls are best-effort and do not affect playback; log errors but continue.

---

## 16. API request signature

Signed calls use an MD5 HMAC-style scheme:

```
signature = MD5(
    object
  + method
  + sorted_params     # all key+value pairs concatenated after sorting by key
  + request_ts        # current Unix time as string with 6 decimal places
  + app_secret
)
```

Example for `track/getFileUrl`:
```
object  = "track"
method  = "getFileUrl"
params  = [("format_id","6"), ("intent","stream"), ("track_id","12345")]
sorted  → "format_id6intentstreamtrack_id12345"
ts      = "1700000000.123456"
input   = "trackgetFileUrlformat_id6intentstreamtrack_id12345" + ts + secret
sig     = MD5(input).hexdigest()
```

Append to request URL: `&request_ts=<ts>&request_sig=<sig>`.

The timestamp string format: `fmt.Sprintf("%d.000000", unix_seconds)` truncated to 6 decimal places.

---

## 17. Audio quality levels

The QConnect protocol uses a 4-level quality enum:

| Value | Name | format_id | Bitrate |
|---|---|---|---|
| 1 | MP3 | 5 | 320 kbps |
| 2 | FLAC CD | 6 | 16-bit/44.1–48 kHz |
| 3 | HiRes 96 | 7 | 24-bit/up to 96 kHz |
| 4 | HiRes 192 | 27 | 24-bit/up to 192 kHz |

The mDNS `max_audio_quality` uses string names: `"MP3"`, `"FLAC"`, `"HIRES_L1"`, `"HIRES_L2"`, `"HIRES_L3"`.

Announce your device capability via `RndrSrvrDeviceAudioQualityChanged` (hardware max) and `RndrSrvrMaxAudioQualityChanged` (user/config limit). The Qobuz app displays the lower of the two.

---

## 18. Device identity and UUID

The device needs a **stable 16-byte UUID** that persists across restarts. This UUID is used:
- As `DeviceInfo.device_uuid` in `JoinSession`
- As the `device_uuid` TXT record in mDNS
- As the `serial_number` in the display-info HTTP endpoint
- As the `queue_uuid` in `AskForQueueState`

**Recommended derivation:** SHA1 UUID v5 from the device name:

```
uuid = UUID_v5(DNS_NAMESPACE, "qobuz-connect.<device_name>")
```

This guarantees the same UUID for the same device name across restarts without needing to persist it.

Store and display the UUID in standard lowercase hyphenated format:
`xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx`

---

## Appendix A — Protobuf wire field map

Summary of protobuf field numbers for the most complex messages.

### QConnectMessage top-level fields

| field | type | message type |
|---|---|---|
| 1 | int32 | message_type |
| 23 | bytes | RndrSrvrStateUpdated |
| 25 | bytes | RndrSrvrVolumeChanged |
| 26 | bytes | RndrSrvrFileAudioQualityChanged |
| 27 | bytes | RndrSrvrDeviceAudioQualityChanged |
| 28 | bytes | RndrSrvrMaxAudioQualityChanged |
| 41 | bytes | SrvrRndrSetState |
| 42 | bytes | SrvrRndrSetVolume |
| 43 | bytes | SrvrRndrSetActive |
| 44 | bytes | SrvrRndrSetMaxAudioQuality |
| 61 | bytes | CtrlSrvrJoinSession |
| 63 | bytes | CtrlSrvrSetActiveRenderer |
| 76 | bytes | CtrlSrvrAskForQueueState |
| 77 | bytes | CtrlSrvrAskForRendererState |
| 79 | bytes | CtrlSrvrAutoplayLoadTracks |
| 81 | bytes | SrvrCtrlSessionState |
| 82 | bytes | SrvrCtrlRendererStateUpdated |
| 83 | bytes | SrvrCtrlAddRenderer |
| 86 | bytes | SrvrCtrlActiveRendererChanged |
| 87 | bytes | SrvrCtrlVolumeChanged |
| 88 | bytes | SrvrCtrlQueueErrorMessage |
| 90 | bytes | SrvrCtrlQueueState |
| 91 | bytes | SrvrCtrlQueueTracksLoaded |
| 92 | bytes | SrvrCtrlQueueTracksInserted |
| 93 | bytes | SrvrCtrlQueueTracksAdded |
| 94 | bytes | SrvrCtrlQueueTracksRemoved |
| 96 | bytes | SrvrCtrlShuffleModeSet |
| 97 | bytes | SrvrCtrlLoopModeSet |
| 99 | int32 | SrvrCtrlMaxAudioQualityChanged (scalar echo) |
| 100 | int32 | SrvrCtrlFileAudioQualityChanged (scalar echo) |
| 101 | int32 | SrvrCtrlDeviceAudioQualityChanged (scalar echo) |
| 103 | bytes | SrvrCtrlAutoplayTracksLoaded |
| 104 | bytes | SrvrCtrlAutoplayTracksRemoved |
| 105 | bytes | SrvrCtrlQueueVersionChanged |

---

## Appendix B — Known quirks and edge cases

1. **First SrvrRndrSetState after QueueTracksLoaded carries a stale position.** The `CurrentPosition` value is the position in the *old* playlist, not a seek point in the new one. Always start new playlists at position 0.

2. **JoinSession must not include SessionUUID.** Setting `session_uuid` in `CtrlSrvrJoinSession` causes server error code 10002. Only `DeviceInfo.device_uuid` should be set.

3. **Do not send RndrSrvrStateUpdated when inactive.** Sending player state while `SrvrRndrSetActive.active = false` triggers a server error "non active renderer". Gate all `RndrSrvr*` messages on the active flag.

4. **Token refresh requires reconnect.** The JWT is only checked at WebSocket connection time. To use a refreshed token, close the connection and reconnect.

5. **Null/transitional state.** `SrvrRndrSetState{playing_state=0, current_queue_item=nil}` is a server-internal transitional message. Ignore it — do not use it to start, stop, or update position.

6. **ACK frames are unreliable.** The server does not always send kind=1 or kind=2 ACK frames after authentication/subscription. Proceed immediately after sending both frames.

7. **SrvrCtrlRendererStateUpdated is a self-echo.** When the player is running, the server echoes our own state back to us via this message. Applying it would rewind playback to the last reported position. Only act on it when the player is stopped.

8. **Heartbeat must outlive the player.** The WS server drops the connection if no player state update arrives for ~10 seconds. The heartbeat goroutine must not be tied to the player goroutine lifecycle — it must continue during playlist switches when the player is briefly stopped.
