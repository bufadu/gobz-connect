package main

// Manual protobuf encode/decode for qconnect protocol messages.
// Uses google.golang.org/protobuf/encoding/protowire for raw wire encoding.

import (
	"google.golang.org/protobuf/encoding/protowire"
)

// ---- Enum constants ----

const (
	QCloudMsgAuthenticate = 1
	QCloudMsgSubscribe    = 2
	QCloudMsgPayload      = 6

	QCloudProtoQConnect = 1

	MsgTypeUnknown                       = 0
	MsgTypeError                         = 1
	MsgTypeRndrSrvrStateUpdated              = 23
	MsgTypeRndrSrvrVolumeChanged             = 25
	MsgTypeRndrSrvrFileAudioQualityChanged   = 26
	MsgTypeRndrSrvrDeviceAudioQualityChanged = 27
	MsgTypeRndrSrvrMaxAudioQualityChanged    = 28
	MsgTypeRndrSrvrVolumeMuted               = 29
	MsgTypeSrvrRndrSetState                  = 41
	MsgTypeSrvrRndrSetMaxAudioQuality        = 44
	MsgTypeSrvrRndrSetVolume             = 42
	MsgTypeCtrlSrvrJoinSession           = 61
	MsgTypeCtrlSrvrSetActiveRenderer     = 63
	MsgTypeCtrlSrvrAskForQueueState      = 76
	MsgTypeCtrlSrvrAskForRendererState   = 77
	MsgTypeCtrlSrvrAutoplayAddTracks     = 79
	MsgTypeSrvrCtrlSessionState          = 81
	MsgTypeSrvrCtrlRendererStateUpdated  = 82
	MsgTypeSrvrCtrlAddRenderer           = 83
	MsgTypeSrvrCtrlActiveRendererChanged = 86
	MsgTypeSrvrCtrlVolumeChanged         = 87
	MsgTypeSrvrCtrlQueueErrorMessage     = 88
	MsgTypeSrvrCtrlQueueState            = 90
	MsgTypeSrvrCtrlQueueTracksLoaded     = 91
	MsgTypeSrvrCtrlQueueTracksInserted   = 92
	MsgTypeSrvrCtrlQueueTracksAdded      = 93
	MsgTypeSrvrCtrlQueueTracksRemoved    = 94
	MsgTypeSrvrRndrSetActive = 43

	MsgTypeSrvrCtrlUpdateRenderer           = 84
	MsgTypeSrvrCtrlRemoveRenderer           = 85
	MsgTypeSrvrCtrlQueueCleared             = 89
	MsgTypeSrvrCtrlQueueTracksReordered     = 95
	MsgTypeSrvrCtrlShuffleModeSet           = 96
	MsgTypeSrvrCtrlLoopModeSet              = 97
	MsgTypeSrvrCtrlVolumeMuted              = 98
	MsgTypeSrvrCtrlMaxAudioQualityChanged   = 99
	MsgTypeSrvrCtrlFileAudioQualityChanged  = 100
	MsgTypeSrvrCtrlDeviceAudioQualityChanged = 101
	MsgTypeSrvrCtrlAutoplayModeSet          = 102
	MsgTypeSrvrCtrlAutoplayTracksLoaded     = 103
	MsgTypeSrvrCtrlAutoplayTracksRemoved    = 104
	MsgTypeSrvrCtrlQueueVersionChanged      = 105

	PlayingStateUnknown = 0
	PlayingStateStopped = 1
	PlayingStatePlaying = 2
	PlayingStatePaused  = 3

	BufferStateUnknown   = 0
	BufferStateBuffering = 1
	BufferStateOK        = 2

	LoopModeOff       = 1
	LoopModeRepeatOne = 2
	LoopModeRepeatAll = 3

	DeviceTypeSpeaker = 1
)

// ---- Data structures ----

// PbAuthenticate corresponds to the Authenticate protobuf message.
type PbAuthenticate struct {
	MsgID   uint32
	MsgDate uint64
	JWT     string
}

// PbPayload corresponds to the Payload envelope protobuf message.
type PbPayload struct {
	MsgID   uint32
	MsgDate uint64
	Proto   uint32
	Src     []byte
	Dests   [][]byte
	Payload []byte
}

// PbQueueVersion corresponds to QueueVersion protobuf message.
type PbQueueVersion struct {
	Major uint64
	Minor int32
}

// PbPosition corresponds to Position protobuf message.
type PbPosition struct {
	Timestamp uint64 // fixed64 on wire
	Value     uint32
}

// PbQueueTrackRef corresponds to QueueTrackRef protobuf message.
type PbQueueTrackRef struct {
	QueueItemID uint64
	TrackID     uint32
	ContextUUID []byte
}

// PbDeviceInfo corresponds to DeviceInfo protobuf message.
type PbDeviceInfo struct {
	DeviceUUID      []byte
	FriendlyName    string
	Type            int32
	Capabilities    *PbDeviceCapabilities
	SoftwareVersion string
}

// PbDeviceCapabilities corresponds to DeviceCapabilities protobuf message.
type PbDeviceCapabilities struct {
	MinAudioQuality     int32
	MaxAudioQuality     int32
	VolumeRemoteControl int32
}

// PbQueueRendererState corresponds to QueueRendererState protobuf message.
type PbQueueRendererState struct {
	PlayingState       int32
	BufferState        int32
	CurrentPosition    *PbPosition
	Duration           uint32
	QueueVersion       *PbQueueVersion
	CurrentQueueItemID int32
	NextQueueItemID    int32
}

// PbQConnectBatch is the inner batch wrapper.
type PbQConnectBatch struct {
	MessagesTime uint64 // fixed64
	MessagesID   int32
	Messages     []*PbQConnectMessage
}

// PbQConnectMessage is the big-union message.
type PbQConnectMessage struct {
	MessageType int32

	// Renderer → Server
	RndrSrvrStateUpdated        *PbRndrSrvrStateUpdated
	RndrSrvrVolumeChanged       *PbRndrSrvrVolumeChanged
	RndrSrvrFileAudioQuality   *PbRndrSrvrFileAudioQuality
	RndrSrvrDeviceAudioQuality *PbRndrSrvrDeviceAudioQuality
	RndrSrvrMaxAudioQuality    *PbRndrSrvrMaxAudioQuality
	RndrSrvrVolumeMuted        *PbRndrSrvrVolumeMuted

	// Server → Renderer
	SrvrRndrSetState          *PbSrvrRndrSetState
	SrvrRndrSetVolume         *PbSrvrRndrSetVolume
	SrvrRndrSetActive         *PbSrvrRndrSetActive
	SrvrRndrSetMaxAudioQuality *PbSrvrRndrSetMaxAudioQuality

	// Controller → Server
	CtrlSrvrJoinSession         *PbCtrlSrvrJoinSession
	CtrlSrvrSetActiveRenderer   *PbCtrlSrvrSetActiveRenderer
	CtrlSrvrAskForQueueState    *PbCtrlSrvrAskForQueueState
	CtrlSrvrAskForRendererState *PbCtrlSrvrAskForRendererState
	CtrlSrvrAutoplayLoadTracks  *PbCtrlSrvrAutoplayLoadTracks

	// Server → Controllers
	SrvrCtrlSessionState          *PbSrvrCtrlSessionState
	SrvrCtrlRendererStateUpdated  *PbSrvrCtrlRendererStateUpdated
	SrvrCtrlAddRenderer           *PbSrvrCtrlAddRenderer
	SrvrCtrlActiveRendererChanged *PbSrvrCtrlActiveRendererChanged
	SrvrCtrlRemoveRenderer        *PbSrvrCtrlRemoveRenderer
	SrvrCtrlQueueCleared          *PbSrvrCtrlQueueCleared
	SrvrCtrlVolumeChanged         *PbSrvrCtrlVolumeChanged
	SrvrCtrlQueueErrorMessage     *PbSrvrCtrlQueueErrorMessage
	SrvrCtrlQueueState            *PbSrvrCtrlQueueState
	SrvrCtrlQueueTracksLoaded     *PbSrvrCtrlQueueTracksLoaded
	SrvrCtrlQueueTracksInserted   *PbSrvrCtrlQueueTracksInserted
	SrvrCtrlQueueTracksAdded      *PbSrvrCtrlQueueTracksAdded
	SrvrCtrlQueueTracksRemoved    *PbSrvrCtrlQueueTracksRemoved
	SrvrCtrlShuffleModeSet           *PbSrvrCtrlShuffleModeSet
	SrvrCtrlLoopModeSet              *PbSrvrCtrlLoopModeSet
	SrvrCtrlAutoplayTracksLoaded     *PbSrvrCtrlAutoplayTracksLoaded
	SrvrCtrlAutoplayTracksRemoved    *PbSrvrCtrlAutoplayTracksRemoved
	SrvrCtrlQueueVersionChanged      *PbSrvrCtrlQueueVersionChanged
	SrvrCtrlMaxAudioQualityChanged   int32 // server echo of max quality level 1–4
	SrvrCtrlFileAudioQualityChanged  int32 // server echo of file quality level 1–4
	SrvrCtrlDeviceAudioQualityChanged int32 // server echo of device quality level 1–4
	SrvrCtrlAutoplayModeSet          int32 // 0=disabled, 1=enabled
	Error                         *PbError
}

type PbError struct {
	Code    string
	Message string
}

type PbRndrSrvrStateUpdated struct{ State *PbQueueRendererState }
type PbRndrSrvrVolumeChanged struct{ Volume uint32 }

// RndrSrvrFileAudioQualityChanged (type 26): reports quality of the currently playing file.
// Proto fields: sampling_rate=1, bit_depth=2, nb_channels=3, audio_quality=4
type PbRndrSrvrFileAudioQuality struct {
	SamplingRate int32 // Hz, e.g. 44100 / 96000 / 192000
	BitDepth     int32 // e.g. 16 or 24
	NbChannels   int32 // e.g. 2
	AudioQuality int32 // protocol value 1–4 (1=MP3,2=FLAC,3=HiRes-96,4=HiRes-192)
}

// RndrSrvrDeviceAudioQualityChanged (type 27): reports device max capability.
// Proto fields: sampling_rate=1, bit_depth=2, nb_channels=3 (NO audio_quality field)
type PbRndrSrvrDeviceAudioQuality struct {
	SamplingRate int32
	BitDepth     int32
	NbChannels   int32
}

// RndrSrvrMaxAudioQualityChanged (type 28): echoes user-selected max quality.
// Proto fields: audio_quality=1, network_type=2
type PbRndrSrvrVolumeMuted struct{ Muted bool }

type PbRndrSrvrMaxAudioQuality struct {
	AudioQuality int32 // protocol value 1–4
	NetworkType  int32 // 1 = WiFi
}

type PbSrvrRndrSetState struct {
	PlayingState        int32
	CurrentPosition     uint32
	HasCurrentPosition  bool
	QueueVersion        *PbQueueVersion
	CurrentQueueItem    *PbQueueTrackRef
	NextQueueItem       *PbQueueTrackRef
}
type PbSrvrRndrSetVolume struct {
	Volume      uint32
	VolumeDelta int32
}

type PbCtrlSrvrJoinSession struct {
	SessionUUID []byte
	DeviceInfo  *PbDeviceInfo
}
type PbCtrlSrvrSetActiveRenderer struct{ RendererID int64 }
type PbCtrlSrvrAskForQueueState struct {
	QueueVersion *PbQueueVersion
	QueueUUID    []byte
}
type PbCtrlSrvrAskForRendererState struct{ SessionID uint64 }
type PbCtrlSrvrAutoplayLoadTracks struct {
	QueueVersion *PbQueueVersion
	ActionUUID   []byte
	TrackIDs     []uint32
	ContextUUID  []byte
}

type PbSrvrCtrlSessionState struct {
	SessionUUID  []byte
	SessionID    uint64
	QueueVersion *PbQueueVersion
}
type PbSrvrCtrlRendererStateUpdated struct {
	RendererID uint64
	State      *PbRendererState
}
type PbRendererState struct {
	PlayingState        int32
	BufferState         int32
	CurrentPosition     *PbPosition
	Duration            uint32
	CurrentQueueIndex   uint32
	NextQueueItemID     int32
}
type PbSrvrCtrlAddRenderer struct {
	RendererID uint64
	Renderer   *PbDeviceInfo
}
type PbSrvrCtrlActiveRendererChanged struct{ RendererID uint64 }
type PbSrvrCtrlVolumeChanged struct {
	RendererID uint64
	Volume     uint32
}
type PbSrvrCtrlQueueErrorMessage struct {
	QueueVersion *PbQueueVersion
	ActionUUID   []byte
	Error        *PbError
}
type PbSrvrCtrlQueueState struct {
	QueueVersion          *PbQueueVersion
	ActionUUID            []byte
	Tracks                []*PbQueueTrackRef
	ShuffleMode           bool
	ShuffledTrackIndexes  []uint32
	AutoplayMode          bool
	AutoplayTracks        []*PbQueueTrackRef
}
type PbSrvrCtrlQueueTracksLoaded struct {
	QueueVersion *PbQueueVersion
	ActionUUID   []byte
	Tracks       []*PbQueueTrackRef
	ContextUUID  []byte
}
type PbSrvrCtrlQueueTracksInserted struct {
	QueueVersion  *PbQueueVersion
	ActionUUID    []byte
	Tracks        []*PbQueueTrackRef
	InsertAfter   int32
	ContextUUID   []byte
	AutoplayReset bool
}
type PbSrvrCtrlQueueTracksAdded struct {
	QueueVersion  *PbQueueVersion
	ActionUUID    []byte
	Tracks        []*PbQueueTrackRef
	ContextUUID   []byte
	AutoplayReset bool
}
type PbSrvrCtrlQueueTracksRemoved struct {
	QueueVersion  *PbQueueVersion
	ActionUUID    []byte
	QueueItemIDs  []uint32
	AutoplayReset bool
}
type PbSrvrCtrlShuffleModeSet struct{ ShuffleOn bool }
type PbSrvrCtrlLoopModeSet struct{ Mode int32 }
type PbSrvrCtrlAutoplayTracksLoaded struct {
	QueueVersion *PbQueueVersion
	ActionUUID   []byte
	Tracks       []*PbQueueTrackRef
	ContextUUID  []byte
}
type PbSrvrCtrlAutoplayTracksRemoved struct {
	QueueVersion *PbQueueVersion
	QueueItemIDs []uint32
}
type PbSrvrCtrlQueueVersionChanged struct{ QueueVersion *PbQueueVersion }

type PbSrvrRndrSetActive struct{ Active bool }
type PbSrvrRndrSetMaxAudioQuality struct{ MaxAudioQuality int32 }
type PbSrvrCtrlRemoveRenderer struct{ RendererID uint64 }
type PbSrvrCtrlQueueCleared struct {
	QueueVersion *PbQueueVersion
	ActionUUID   []byte
}

// ---- Encode helpers ----

func encodeTag(b []byte, fieldNum protowire.Number, typ protowire.Type) []byte {
	return protowire.AppendTag(b, fieldNum, typ)
}

func encodeVarintField(b []byte, num protowire.Number, v uint64) []byte {
	b = encodeTag(b, num, protowire.VarintType)
	return protowire.AppendVarint(b, v)
}

func encodeFixed64Field(b []byte, num protowire.Number, v uint64) []byte {
	b = encodeTag(b, num, protowire.Fixed64Type)
	return protowire.AppendFixed64(b, v)
}

func encodeBytesField(b []byte, num protowire.Number, v []byte) []byte {
	if len(v) == 0 {
		return b
	}
	b = encodeTag(b, num, protowire.BytesType)
	return protowire.AppendBytes(b, v)
}

func encodeStringField(b []byte, num protowire.Number, v string) []byte {
	if v == "" {
		return b
	}
	b = encodeTag(b, num, protowire.BytesType)
	return protowire.AppendString(b, v)
}

func encodeMessageField(b []byte, num protowire.Number, msg []byte) []byte {
	if len(msg) == 0 {
		return b
	}
	b = encodeTag(b, num, protowire.BytesType)
	return protowire.AppendBytes(b, msg)
}

// ---- Encode functions ----

func EncodeAuthenticate(a *PbAuthenticate) []byte {
	var b []byte
	b = encodeVarintField(b, 1, uint64(a.MsgID))
	b = encodeVarintField(b, 2, a.MsgDate)
	b = encodeStringField(b, 3, a.JWT)
	return b
}

func EncodePayload(p *PbPayload) []byte {
	var b []byte
	if p.MsgID != 0 {
		b = encodeVarintField(b, 1, uint64(p.MsgID))
	}
	if p.MsgDate != 0 {
		b = encodeVarintField(b, 2, p.MsgDate)
	}
	if p.Proto != 0 {
		b = encodeVarintField(b, 3, uint64(p.Proto))
	}
	b = encodeBytesField(b, 4, p.Src)
	for _, d := range p.Dests {
		b = encodeBytesField(b, 5, d)
	}
	b = encodeBytesField(b, 7, p.Payload)
	return b
}

func EncodeQueueVersion(v *PbQueueVersion) []byte {
	if v == nil {
		return nil
	}
	var b []byte
	if v.Major != 0 {
		b = encodeVarintField(b, 1, v.Major)
	}
	if v.Minor != 0 {
		b = encodeVarintField(b, 2, uint64(v.Minor))
	}
	return b
}

func EncodePosition(p *PbPosition) []byte {
	if p == nil {
		return nil
	}
	var b []byte
	if p.Timestamp != 0 {
		b = encodeFixed64Field(b, 1, p.Timestamp)
	}
	if p.Value != 0 {
		b = encodeVarintField(b, 2, uint64(p.Value))
	}
	return b
}

func EncodeQueueRendererState(s *PbQueueRendererState) []byte {
	if s == nil {
		return nil
	}
	var b []byte
	if s.PlayingState != 0 {
		b = encodeVarintField(b, 1, uint64(s.PlayingState))
	}
	if s.BufferState != 0 {
		b = encodeVarintField(b, 2, uint64(s.BufferState))
	}
	if s.CurrentPosition != nil {
		b = encodeMessageField(b, 3, EncodePosition(s.CurrentPosition))
	}
	if s.Duration != 0 {
		b = encodeVarintField(b, 4, uint64(s.Duration))
	}
	if s.QueueVersion != nil {
		b = encodeMessageField(b, 5, EncodeQueueVersion(s.QueueVersion))
	}
	b = encodeVarintField(b, 6, uint64(s.CurrentQueueItemID))
	if s.NextQueueItemID >= 0 {
		b = encodeVarintField(b, 7, uint64(s.NextQueueItemID))
	}
	return b
}

func EncodeDeviceCapabilities(c *PbDeviceCapabilities) []byte {
	if c == nil {
		return nil
	}
	var b []byte
	if c.MinAudioQuality != 0 {
		b = encodeVarintField(b, 1, uint64(c.MinAudioQuality))
	}
	if c.MaxAudioQuality != 0 {
		b = encodeVarintField(b, 2, uint64(c.MaxAudioQuality))
	}
	if c.VolumeRemoteControl != 0 {
		b = encodeVarintField(b, 3, uint64(c.VolumeRemoteControl))
	}
	return b
}

func EncodeDeviceInfo(d *PbDeviceInfo) []byte {
	if d == nil {
		return nil
	}
	var b []byte
	b = encodeBytesField(b, 1, d.DeviceUUID)
	b = encodeStringField(b, 2, d.FriendlyName)
	if d.Type != 0 {
		b = encodeVarintField(b, 6, uint64(d.Type))
	}
	if d.Capabilities != nil {
		b = encodeMessageField(b, 7, EncodeDeviceCapabilities(d.Capabilities))
	}
	b = encodeStringField(b, 8, d.SoftwareVersion)
	return b
}

func EncodeQueueTrackRef(t *PbQueueTrackRef) []byte {
	if t == nil {
		return nil
	}
	var b []byte
	if t.QueueItemID != 0 {
		b = encodeVarintField(b, 1, t.QueueItemID)
	}
	if t.TrackID != 0 {
		b = protowire.AppendTag(b, 2, protowire.Fixed32Type)
		b = protowire.AppendFixed32(b, t.TrackID)
	}
	b = encodeBytesField(b, 3, t.ContextUUID)
	return b
}

// EncodeBatch encodes a QConnectBatch containing an array of messages.
func EncodeBatch(msgs []*PbQConnectMessage, messagesID int32, ts uint64) []byte {
	var b []byte
	b = encodeFixed64Field(b, 1, ts)
	b = encodeVarintField(b, 2, uint64(messagesID))
	for _, msg := range msgs {
		b = encodeMessageField(b, 3, EncodeQConnectMessage(msg))
	}
	return b
}

func EncodeQConnectMessage(m *PbQConnectMessage) []byte {
	var b []byte
	if m.MessageType != 0 {
		b = encodeVarintField(b, 1, uint64(m.MessageType))
	}
	if m.Error != nil {
		b = encodeMessageField(b, 2, encodeError(m.Error))
	}
	if m.RndrSrvrStateUpdated != nil {
		inner := encodeRndrSrvrStateUpdated(m.RndrSrvrStateUpdated)
		b = encodeMessageField(b, 23, inner)
	}
	if m.RndrSrvrVolumeChanged != nil {
		var inner []byte
		inner = encodeVarintField(inner, 1, uint64(m.RndrSrvrVolumeChanged.Volume))
		b = encodeMessageField(b, 25, inner)
	}
	if q := m.RndrSrvrFileAudioQuality; q != nil {
		// Fields: sampling_rate=1, bit_depth=2, nb_channels=3, audio_quality=4
		var inner []byte
		if q.SamplingRate != 0 {
			inner = encodeVarintField(inner, 1, uint64(q.SamplingRate))
		}
		if q.BitDepth != 0 {
			inner = encodeVarintField(inner, 2, uint64(q.BitDepth))
		}
		if q.NbChannels != 0 {
			inner = encodeVarintField(inner, 3, uint64(q.NbChannels))
		}
		if q.AudioQuality != 0 {
			inner = encodeVarintField(inner, 4, uint64(q.AudioQuality))
		}
		b = encodeMessageField(b, 26, inner)
	}
	if q := m.RndrSrvrDeviceAudioQuality; q != nil {
		// Fields: sampling_rate=1, bit_depth=2, nb_channels=3 (no audio_quality per proto)
		var inner []byte
		if q.SamplingRate != 0 {
			inner = encodeVarintField(inner, 1, uint64(q.SamplingRate))
		}
		if q.BitDepth != 0 {
			inner = encodeVarintField(inner, 2, uint64(q.BitDepth))
		}
		if q.NbChannels != 0 {
			inner = encodeVarintField(inner, 3, uint64(q.NbChannels))
		}
		b = encodeMessageField(b, 27, inner)
	}
	if q := m.RndrSrvrMaxAudioQuality; q != nil {
		// Fields: audio_quality=1, network_type=2
		var inner []byte
		if q.AudioQuality != 0 {
			inner = encodeVarintField(inner, 1, uint64(q.AudioQuality))
		}
		if q.NetworkType != 0 {
			inner = encodeVarintField(inner, 2, uint64(q.NetworkType))
		}
		b = encodeMessageField(b, 28, inner)
	}
	if m.RndrSrvrVolumeMuted != nil {
		var inner []byte
		if m.RndrSrvrVolumeMuted.Muted {
			inner = encodeVarintField(inner, 1, 1)
		} else {
			inner = encodeVarintField(inner, 1, 0)
		}
		b = encodeMessageField(b, 29, inner)
	}
	if m.CtrlSrvrJoinSession != nil {
		b = encodeMessageField(b, 61, encodeCtrlSrvrJoinSession(m.CtrlSrvrJoinSession))
	}
	if m.CtrlSrvrSetActiveRenderer != nil {
		var inner []byte
		inner = encodeVarintField(inner, 1, uint64(m.CtrlSrvrSetActiveRenderer.RendererID))
		b = encodeMessageField(b, 63, inner)
	}
	if m.CtrlSrvrAskForQueueState != nil {
		b = encodeMessageField(b, 76, encodeCtrlSrvrAskForQueueState(m.CtrlSrvrAskForQueueState))
	}
	if m.CtrlSrvrAskForRendererState != nil {
		var inner []byte
		inner = encodeVarintField(inner, 1, m.CtrlSrvrAskForRendererState.SessionID)
		b = encodeMessageField(b, 77, inner)
	}
	if m.CtrlSrvrAutoplayLoadTracks != nil {
		b = encodeMessageField(b, 79, encodeCtrlSrvrAutoplayLoadTracks(m.CtrlSrvrAutoplayLoadTracks))
	}
	return b
}

func encodeError(e *PbError) []byte {
	var b []byte
	b = encodeStringField(b, 1, e.Code)
	b = encodeStringField(b, 2, e.Message)
	return b
}

func encodeRndrSrvrStateUpdated(s *PbRndrSrvrStateUpdated) []byte {
	var b []byte
	if s.State != nil {
		b = encodeMessageField(b, 1, EncodeQueueRendererState(s.State))
	}
	return b
}

func encodeCtrlSrvrJoinSession(j *PbCtrlSrvrJoinSession) []byte {
	var b []byte
	b = encodeBytesField(b, 1, j.SessionUUID)
	if j.DeviceInfo != nil {
		b = encodeMessageField(b, 2, EncodeDeviceInfo(j.DeviceInfo))
	}
	return b
}

func encodeCtrlSrvrAskForQueueState(a *PbCtrlSrvrAskForQueueState) []byte {
	var b []byte
	if a.QueueVersion != nil {
		b = encodeMessageField(b, 1, EncodeQueueVersion(a.QueueVersion))
	}
	b = encodeBytesField(b, 2, a.QueueUUID)
	return b
}

func encodeCtrlSrvrAutoplayLoadTracks(a *PbCtrlSrvrAutoplayLoadTracks) []byte {
	var b []byte
	if a.QueueVersion != nil {
		b = encodeMessageField(b, 1, EncodeQueueVersion(a.QueueVersion))
	}
	b = encodeBytesField(b, 2, a.ActionUUID)
	for _, id := range a.TrackIDs {
		b = protowire.AppendTag(b, 3, protowire.Fixed32Type)
		b = protowire.AppendFixed32(b, id)
	}
	b = encodeBytesField(b, 4, a.ContextUUID)
	return b
}

// ---- Decode functions ----

func DecodePayload(data []byte) *PbPayload {
	p := &PbPayload{}
	for len(data) > 0 {
		num, typ, n := protowire.ConsumeTag(data)
		if n < 0 {
			break
		}
		data = data[n:]
		switch typ {
		case protowire.VarintType:
			v, n := protowire.ConsumeVarint(data)
			if n < 0 {
				return p
			}
			data = data[n:]
			switch num {
			case 1:
				p.MsgID = uint32(v)
			case 2:
				p.MsgDate = v
			case 3:
				p.Proto = uint32(v)
			}
		case protowire.BytesType:
			v, n := protowire.ConsumeBytes(data)
			if n < 0 {
				return p
			}
			data = data[n:]
			cp := make([]byte, len(v))
			copy(cp, v)
			switch num {
			case 4:
				p.Src = cp
			case 5:
				p.Dests = append(p.Dests, cp)
			case 7:
				p.Payload = cp
			}
		default:
			n := protowire.ConsumeFieldValue(num, typ, data)
			if n < 0 {
				return p
			}
			data = data[n:]
		}
	}
	return p
}

func DecodeBatch(data []byte) *PbQConnectBatch {
	batch := &PbQConnectBatch{}
	for len(data) > 0 {
		num, typ, n := protowire.ConsumeTag(data)
		if n < 0 {
			break
		}
		data = data[n:]
		switch typ {
		case protowire.Fixed64Type:
			v, n := protowire.ConsumeFixed64(data)
			if n < 0 {
				return batch
			}
			data = data[n:]
			if num == 1 {
				batch.MessagesTime = v
			}
		case protowire.VarintType:
			v, n := protowire.ConsumeVarint(data)
			if n < 0 {
				return batch
			}
			data = data[n:]
			if num == 2 {
				batch.MessagesID = int32(v)
			}
		case protowire.BytesType:
			v, n := protowire.ConsumeBytes(data)
			if n < 0 {
				return batch
			}
			data = data[n:]
			if num == 3 {
				msg := DecodeQConnectMessage(v)
				batch.Messages = append(batch.Messages, msg)
			}
		default:
			n := protowire.ConsumeFieldValue(num, typ, data)
			if n < 0 {
				return batch
			}
			data = data[n:]
		}
	}
	return batch
}

func DecodeQConnectMessage(data []byte) *PbQConnectMessage {
	m := &PbQConnectMessage{}
	for len(data) > 0 {
		num, typ, n := protowire.ConsumeTag(data)
		if n < 0 {
			break
		}
		data = data[n:]
		switch typ {
		case protowire.VarintType:
			v, n := protowire.ConsumeVarint(data)
			if n < 0 {
				return m
			}
			data = data[n:]
			if num == 1 {
				m.MessageType = int32(v)
			}
		case protowire.BytesType:
			v, n := protowire.ConsumeBytes(data)
			if n < 0 {
				return m
			}
			data = data[n:]
			switch num {
			case 2:
				m.Error = decodeError(v)
			case 23:
				if s := decodeQueueRendererState(v); s != nil {
					m.RndrSrvrStateUpdated = &PbRndrSrvrStateUpdated{State: s}
				}
			case 25:
				m.RndrSrvrVolumeChanged = &PbRndrSrvrVolumeChanged{Volume: decodeUint32Field1(v)}
			case 29:
				m.RndrSrvrVolumeMuted = &PbRndrSrvrVolumeMuted{Muted: decodeUint32Field1(v) != 0}
			case 41:
				m.SrvrRndrSetState = decodeSrvrRndrSetState(v)
			case 42:
				m.SrvrRndrSetVolume = decodeSrvrRndrSetVolume(v)
			case 43:
				m.SrvrRndrSetActive = decodeSrvrRndrSetActive(v)
			case 44:
				m.SrvrRndrSetMaxAudioQuality = &PbSrvrRndrSetMaxAudioQuality{MaxAudioQuality: int32(decodeUint32Field1(v))}
			case 81:
				m.SrvrCtrlSessionState = decodeSrvrCtrlSessionState(v)
			case 82:
				m.SrvrCtrlRendererStateUpdated = decodeSrvrCtrlRendererStateUpdated(v)
			case 83:
				m.SrvrCtrlAddRenderer = decodeSrvrCtrlAddRenderer(v)
			case 85:
				m.SrvrCtrlRemoveRenderer = decodeSrvrCtrlRemoveRenderer(v)
			case 86:
				m.SrvrCtrlActiveRendererChanged = &PbSrvrCtrlActiveRendererChanged{RendererID: decodeUint64Field1(v)}
			case 87:
				m.SrvrCtrlVolumeChanged = decodeSrvrCtrlVolumeChanged(v)
			case 88:
				m.SrvrCtrlQueueErrorMessage = decodeSrvrCtrlQueueErrorMessage(v)
			case 89:
				m.SrvrCtrlQueueCleared = decodeSrvrCtrlQueueCleared(v)
			case 90:
				m.SrvrCtrlQueueState = decodeSrvrCtrlQueueState(v)
			case 91:
				m.SrvrCtrlQueueTracksLoaded = decodeSrvrCtrlQueueTracksLoaded(v)
			case 92:
				m.SrvrCtrlQueueTracksInserted = decodeSrvrCtrlQueueTracksInserted(v)
			case 93:
				m.SrvrCtrlQueueTracksAdded = decodeSrvrCtrlQueueTracksAdded(v)
			case 94:
				m.SrvrCtrlQueueTracksRemoved = decodeSrvrCtrlQueueTracksRemoved(v)
			case 96:
				m.SrvrCtrlShuffleModeSet = decodeSrvrCtrlShuffleModeSet(v)
			case 97:
				m.SrvrCtrlLoopModeSet = decodeSrvrCtrlLoopModeSet(v)
			case 99:
				m.SrvrCtrlMaxAudioQualityChanged = int32(decodeUint32Field1(v))
			case 100:
				m.SrvrCtrlFileAudioQualityChanged = int32(decodeUint32Field1(v))
			case 101:
				m.SrvrCtrlDeviceAudioQualityChanged = int32(decodeUint32Field1(v))
			case 102:
				m.SrvrCtrlAutoplayModeSet = int32(decodeUint32Field1(v))
			case 103:
				m.SrvrCtrlAutoplayTracksLoaded = decodeSrvrCtrlAutoplayTracksLoaded(v)
			case 104:
				m.SrvrCtrlAutoplayTracksRemoved = decodeSrvrCtrlAutoplayTracksRemoved(v)
			case 105:
				m.SrvrCtrlQueueVersionChanged = &PbSrvrCtrlQueueVersionChanged{QueueVersion: decodeQueueVersion(v)}
			}
		default:
			n := protowire.ConsumeFieldValue(num, typ, data)
			if n < 0 {
				return m
			}
			data = data[n:]
		}
	}
	return m
}

// Generic field decoders

func decodeUint32Field1(data []byte) uint32 {
	for len(data) > 0 {
		num, typ, n := protowire.ConsumeTag(data)
		if n < 0 {
			break
		}
		data = data[n:]
		if typ == protowire.VarintType {
			v, n := protowire.ConsumeVarint(data)
			if n < 0 {
				break
			}
			data = data[n:]
			if num == 1 {
				return uint32(v)
			}
		} else {
			n := protowire.ConsumeFieldValue(num, typ, data)
			if n < 0 {
				break
			}
			data = data[n:]
		}
	}
	return 0
}

func decodeUint64Field1(data []byte) uint64 {
	for len(data) > 0 {
		num, typ, n := protowire.ConsumeTag(data)
		if n < 0 {
			break
		}
		data = data[n:]
		if typ == protowire.VarintType {
			v, n := protowire.ConsumeVarint(data)
			if n < 0 {
				break
			}
			data = data[n:]
			if num == 1 {
				return v
			}
		} else {
			n := protowire.ConsumeFieldValue(num, typ, data)
			if n < 0 {
				break
			}
			data = data[n:]
		}
	}
	return 0
}

func decodeQueueVersion(data []byte) *PbQueueVersion {
	v := &PbQueueVersion{}
	for len(data) > 0 {
		num, typ, n := protowire.ConsumeTag(data)
		if n < 0 {
			break
		}
		data = data[n:]
		if typ == protowire.VarintType {
			val, n := protowire.ConsumeVarint(data)
			if n < 0 {
				break
			}
			data = data[n:]
			switch num {
			case 1:
				v.Major = val
			case 2:
				v.Minor = int32(val)
			}
		} else {
			n := protowire.ConsumeFieldValue(num, typ, data)
			if n < 0 {
				break
			}
			data = data[n:]
		}
	}
	return v
}

func decodePosition(data []byte) *PbPosition {
	p := &PbPosition{}
	for len(data) > 0 {
		num, typ, n := protowire.ConsumeTag(data)
		if n < 0 {
			break
		}
		data = data[n:]
		switch typ {
		case protowire.Fixed64Type:
			v, n := protowire.ConsumeFixed64(data)
			if n < 0 {
				return p
			}
			data = data[n:]
			if num == 1 {
				p.Timestamp = v
			}
		case protowire.VarintType:
			v, n := protowire.ConsumeVarint(data)
			if n < 0 {
				return p
			}
			data = data[n:]
			if num == 2 {
				p.Value = uint32(v)
			}
		default:
			n := protowire.ConsumeFieldValue(num, typ, data)
			if n < 0 {
				return p
			}
			data = data[n:]
		}
	}
	return p
}

func decodeQueueTrackRef(data []byte) *PbQueueTrackRef {
	t := &PbQueueTrackRef{}
	for len(data) > 0 {
		num, typ, n := protowire.ConsumeTag(data)
		if n < 0 {
			break
		}
		data = data[n:]
		switch typ {
		case protowire.VarintType:
			v, n := protowire.ConsumeVarint(data)
			if n < 0 {
				return t
			}
			data = data[n:]
			if num == 1 {
				t.QueueItemID = v
			}
		case protowire.Fixed32Type:
			v, n := protowire.ConsumeFixed32(data)
			if n < 0 {
				return t
			}
			data = data[n:]
			if num == 2 {
				t.TrackID = v
			}
		case protowire.BytesType:
			v, n := protowire.ConsumeBytes(data)
			if n < 0 {
				return t
			}
			data = data[n:]
			if num == 3 {
				cp := make([]byte, len(v))
				copy(cp, v)
				t.ContextUUID = cp
			}
		default:
			n := protowire.ConsumeFieldValue(num, typ, data)
			if n < 0 {
				return t
			}
			data = data[n:]
		}
	}
	return t
}

func decodeQueueRendererState(data []byte) *PbQueueRendererState {
	s := &PbQueueRendererState{}
	for len(data) > 0 {
		num, typ, n := protowire.ConsumeTag(data)
		if n < 0 {
			break
		}
		data = data[n:]
		switch typ {
		case protowire.VarintType:
			v, n := protowire.ConsumeVarint(data)
			if n < 0 {
				return s
			}
			data = data[n:]
			switch num {
			case 1:
				s.PlayingState = int32(v)
			case 2:
				s.BufferState = int32(v)
			case 4:
				s.Duration = uint32(v)
			case 6:
				s.CurrentQueueItemID = int32(v)
			case 7:
				s.NextQueueItemID = int32(v)
			}
		case protowire.BytesType:
			v, n := protowire.ConsumeBytes(data)
			if n < 0 {
				return s
			}
			data = data[n:]
			switch num {
			case 3:
				s.CurrentPosition = decodePosition(v)
			case 5:
				s.QueueVersion = decodeQueueVersion(v)
			}
		default:
			n := protowire.ConsumeFieldValue(num, typ, data)
			if n < 0 {
				return s
			}
			data = data[n:]
		}
	}
	return s
}

func decodeRendererState(data []byte) *PbRendererState {
	s := &PbRendererState{}
	for len(data) > 0 {
		num, typ, n := protowire.ConsumeTag(data)
		if n < 0 {
			break
		}
		data = data[n:]
		switch typ {
		case protowire.VarintType:
			v, n := protowire.ConsumeVarint(data)
			if n < 0 {
				return s
			}
			data = data[n:]
			switch num {
			case 1:
				s.PlayingState = int32(v)
			case 2:
				s.BufferState = int32(v)
			case 4:
				s.Duration = uint32(v)
			case 5:
				s.CurrentQueueIndex = uint32(v)
			case 7:
				s.NextQueueItemID = int32(v)
			}
		case protowire.BytesType:
			v, n := protowire.ConsumeBytes(data)
			if n < 0 {
				return s
			}
			data = data[n:]
			if num == 3 {
				s.CurrentPosition = decodePosition(v)
			}
		default:
			n := protowire.ConsumeFieldValue(num, typ, data)
			if n < 0 {
				return s
			}
			data = data[n:]
		}
	}
	return s
}

func decodeDeviceInfo(data []byte) *PbDeviceInfo {
	d := &PbDeviceInfo{}
	for len(data) > 0 {
		num, typ, n := protowire.ConsumeTag(data)
		if n < 0 {
			break
		}
		data = data[n:]
		switch typ {
		case protowire.BytesType:
			v, n := protowire.ConsumeBytes(data)
			if n < 0 {
				return d
			}
			data = data[n:]
			switch num {
			case 1:
				cp := make([]byte, len(v))
				copy(cp, v)
				d.DeviceUUID = cp
			case 2:
				d.FriendlyName = string(v)
			case 8:
				d.SoftwareVersion = string(v)
			}
		case protowire.VarintType:
			v, n := protowire.ConsumeVarint(data)
			if n < 0 {
				return d
			}
			data = data[n:]
			if num == 6 {
				d.Type = int32(v)
			}
		default:
			n := protowire.ConsumeFieldValue(num, typ, data)
			if n < 0 {
				return d
			}
			data = data[n:]
		}
	}
	return d
}

func decodeError(data []byte) *PbError {
	e := &PbError{}
	for len(data) > 0 {
		num, typ, n := protowire.ConsumeTag(data)
		if n < 0 {
			break
		}
		data = data[n:]
		if typ == protowire.BytesType {
			v, n := protowire.ConsumeBytes(data)
			if n < 0 {
				break
			}
			data = data[n:]
			switch num {
			case 1:
				e.Code = string(v)
			case 2:
				e.Message = string(v)
			}
		} else {
			n := protowire.ConsumeFieldValue(num, typ, data)
			if n < 0 {
				break
			}
			data = data[n:]
		}
	}
	return e
}

func decodeSrvrRndrSetState(data []byte) *PbSrvrRndrSetState {
	s := &PbSrvrRndrSetState{}
	for len(data) > 0 {
		num, typ, n := protowire.ConsumeTag(data)
		if n < 0 {
			break
		}
		data = data[n:]
		switch typ {
		case protowire.VarintType:
			v, n := protowire.ConsumeVarint(data)
			if n < 0 {
				return s
			}
			data = data[n:]
			switch num {
			case 1:
				s.PlayingState = int32(v)
			case 2:
				s.CurrentPosition = uint32(v)
				s.HasCurrentPosition = true
			}
		case protowire.BytesType:
			v, n := protowire.ConsumeBytes(data)
			if n < 0 {
				return s
			}
			data = data[n:]
			switch num {
			case 3:
				s.QueueVersion = decodeQueueVersion(v)
			case 4:
				s.CurrentQueueItem = decodeQueueTrackRef(v)
			case 5:
				s.NextQueueItem = decodeQueueTrackRef(v)
			}
		default:
			n := protowire.ConsumeFieldValue(num, typ, data)
			if n < 0 {
				return s
			}
			data = data[n:]
		}
	}
	return s
}

func decodeSrvrRndrSetVolume(data []byte) *PbSrvrRndrSetVolume {
	s := &PbSrvrRndrSetVolume{}
	for len(data) > 0 {
		num, typ, n := protowire.ConsumeTag(data)
		if n < 0 {
			break
		}
		data = data[n:]
		if typ == protowire.VarintType {
			v, n := protowire.ConsumeVarint(data)
			if n < 0 {
				break
			}
			data = data[n:]
			switch num {
			case 1:
				s.Volume = uint32(v)
			case 2:
				s.VolumeDelta = int32(v)
			}
		} else {
			n := protowire.ConsumeFieldValue(num, typ, data)
			if n < 0 {
				break
			}
			data = data[n:]
		}
	}
	return s
}

func decodeSrvrCtrlSessionState(data []byte) *PbSrvrCtrlSessionState {
	s := &PbSrvrCtrlSessionState{}
	for len(data) > 0 {
		num, typ, n := protowire.ConsumeTag(data)
		if n < 0 {
			break
		}
		data = data[n:]
		switch typ {
		case protowire.BytesType:
			v, n := protowire.ConsumeBytes(data)
			if n < 0 {
				return s
			}
			data = data[n:]
			switch num {
			case 1:
				cp := make([]byte, len(v))
				copy(cp, v)
				s.SessionUUID = cp
			case 3:
				s.QueueVersion = decodeQueueVersion(v)
			}
		case protowire.VarintType:
			v, n := protowire.ConsumeVarint(data)
			if n < 0 {
				return s
			}
			data = data[n:]
			if num == 2 {
				s.SessionID = v
			}
		default:
			n := protowire.ConsumeFieldValue(num, typ, data)
			if n < 0 {
				return s
			}
			data = data[n:]
		}
	}
	return s
}

func decodeSrvrCtrlRendererStateUpdated(data []byte) *PbSrvrCtrlRendererStateUpdated {
	s := &PbSrvrCtrlRendererStateUpdated{}
	for len(data) > 0 {
		num, typ, n := protowire.ConsumeTag(data)
		if n < 0 {
			break
		}
		data = data[n:]
		switch typ {
		case protowire.VarintType:
			v, n := protowire.ConsumeVarint(data)
			if n < 0 {
				return s
			}
			data = data[n:]
			if num == 1 {
				s.RendererID = v
			}
		case protowire.BytesType:
			v, n := protowire.ConsumeBytes(data)
			if n < 0 {
				return s
			}
			data = data[n:]
			if num == 3 {
				s.State = decodeRendererState(v)
			}
		default:
			n := protowire.ConsumeFieldValue(num, typ, data)
			if n < 0 {
				return s
			}
			data = data[n:]
		}
	}
	return s
}

func decodeSrvrCtrlAddRenderer(data []byte) *PbSrvrCtrlAddRenderer {
	s := &PbSrvrCtrlAddRenderer{}
	for len(data) > 0 {
		num, typ, n := protowire.ConsumeTag(data)
		if n < 0 {
			break
		}
		data = data[n:]
		switch typ {
		case protowire.VarintType:
			v, n := protowire.ConsumeVarint(data)
			if n < 0 {
				return s
			}
			data = data[n:]
			if num == 1 {
				s.RendererID = v
			}
		case protowire.BytesType:
			v, n := protowire.ConsumeBytes(data)
			if n < 0 {
				return s
			}
			data = data[n:]
			if num == 2 {
				s.Renderer = decodeDeviceInfo(v)
			}
		default:
			n := protowire.ConsumeFieldValue(num, typ, data)
			if n < 0 {
				return s
			}
			data = data[n:]
		}
	}
	return s
}

func decodeSrvrCtrlVolumeChanged(data []byte) *PbSrvrCtrlVolumeChanged {
	s := &PbSrvrCtrlVolumeChanged{}
	for len(data) > 0 {
		num, typ, n := protowire.ConsumeTag(data)
		if n < 0 {
			break
		}
		data = data[n:]
		if typ == protowire.VarintType {
			v, n := protowire.ConsumeVarint(data)
			if n < 0 {
				break
			}
			data = data[n:]
			switch num {
			case 1:
				s.RendererID = v
			case 2:
				s.Volume = uint32(v)
			}
		} else {
			n := protowire.ConsumeFieldValue(num, typ, data)
			if n < 0 {
				break
			}
			data = data[n:]
		}
	}
	return s
}

func decodeSrvrCtrlQueueErrorMessage(data []byte) *PbSrvrCtrlQueueErrorMessage {
	s := &PbSrvrCtrlQueueErrorMessage{}
	for len(data) > 0 {
		num, typ, n := protowire.ConsumeTag(data)
		if n < 0 {
			break
		}
		data = data[n:]
		switch typ {
		case protowire.BytesType:
			v, n := protowire.ConsumeBytes(data)
			if n < 0 {
				return s
			}
			data = data[n:]
			switch num {
			case 1:
				s.QueueVersion = decodeQueueVersion(v)
			case 2:
				cp := make([]byte, len(v))
				copy(cp, v)
				s.ActionUUID = cp
			case 3:
				s.Error = decodeError(v)
			}
		default:
			n := protowire.ConsumeFieldValue(num, typ, data)
			if n < 0 {
				return s
			}
			data = data[n:]
		}
	}
	return s
}

func decodeSrvrCtrlQueueState(data []byte) *PbSrvrCtrlQueueState {
	s := &PbSrvrCtrlQueueState{}
	for len(data) > 0 {
		num, typ, n := protowire.ConsumeTag(data)
		if n < 0 {
			break
		}
		data = data[n:]
		switch typ {
		case protowire.VarintType:
			v, n := protowire.ConsumeVarint(data)
			if n < 0 {
				return s
			}
			data = data[n:]
			switch num {
			case 4:
				s.ShuffleMode = v != 0
			case 5:
				s.ShuffledTrackIndexes = append(s.ShuffledTrackIndexes, uint32(v))
			case 6:
				s.AutoplayMode = v != 0
			}
		case protowire.BytesType:
			v, n := protowire.ConsumeBytes(data)
			if n < 0 {
				return s
			}
			data = data[n:]
			switch num {
			case 1:
				s.QueueVersion = decodeQueueVersion(v)
			case 2:
				cp := make([]byte, len(v))
				copy(cp, v)
				s.ActionUUID = cp
			case 3:
				s.Tracks = append(s.Tracks, decodeQueueTrackRef(v))
			case 8:
				s.AutoplayTracks = append(s.AutoplayTracks, decodeQueueTrackRef(v))
			}
		default:
			n := protowire.ConsumeFieldValue(num, typ, data)
			if n < 0 {
				return s
			}
			data = data[n:]
		}
	}
	return s
}

func decodeSrvrCtrlQueueTracksLoaded(data []byte) *PbSrvrCtrlQueueTracksLoaded {
	s := &PbSrvrCtrlQueueTracksLoaded{}
	for len(data) > 0 {
		num, typ, n := protowire.ConsumeTag(data)
		if n < 0 {
			break
		}
		data = data[n:]
		switch typ {
		case protowire.BytesType:
			v, n := protowire.ConsumeBytes(data)
			if n < 0 {
				return s
			}
			data = data[n:]
			switch num {
			case 1:
				s.QueueVersion = decodeQueueVersion(v)
			case 2:
				cp := make([]byte, len(v))
				copy(cp, v)
				s.ActionUUID = cp
			case 3:
				s.Tracks = append(s.Tracks, decodeQueueTrackRef(v))
			case 8:
				cp := make([]byte, len(v))
				copy(cp, v)
				s.ContextUUID = cp
			}
		default:
			n := protowire.ConsumeFieldValue(num, typ, data)
			if n < 0 {
				return s
			}
			data = data[n:]
		}
	}
	return s
}

func decodeSrvrCtrlQueueTracksInserted(data []byte) *PbSrvrCtrlQueueTracksInserted {
	s := &PbSrvrCtrlQueueTracksInserted{}
	for len(data) > 0 {
		num, typ, n := protowire.ConsumeTag(data)
		if n < 0 {
			break
		}
		data = data[n:]
		switch typ {
		case protowire.VarintType:
			v, n := protowire.ConsumeVarint(data)
			if n < 0 {
				return s
			}
			data = data[n:]
			switch num {
			case 4:
				s.InsertAfter = int32(v)
			case 7:
				s.AutoplayReset = v != 0
			}
		case protowire.BytesType:
			v, n := protowire.ConsumeBytes(data)
			if n < 0 {
				return s
			}
			data = data[n:]
			switch num {
			case 1:
				s.QueueVersion = decodeQueueVersion(v)
			case 2:
				cp := make([]byte, len(v))
				copy(cp, v)
				s.ActionUUID = cp
			case 3:
				s.Tracks = append(s.Tracks, decodeQueueTrackRef(v))
			case 6:
				cp := make([]byte, len(v))
				copy(cp, v)
				s.ContextUUID = cp
			}
		default:
			n := protowire.ConsumeFieldValue(num, typ, data)
			if n < 0 {
				return s
			}
			data = data[n:]
		}
	}
	return s
}

func decodeSrvrCtrlQueueTracksAdded(data []byte) *PbSrvrCtrlQueueTracksAdded {
	s := &PbSrvrCtrlQueueTracksAdded{}
	for len(data) > 0 {
		num, typ, n := protowire.ConsumeTag(data)
		if n < 0 {
			break
		}
		data = data[n:]
		switch typ {
		case protowire.VarintType:
			v, n := protowire.ConsumeVarint(data)
			if n < 0 {
				return s
			}
			data = data[n:]
			if num == 6 {
				s.AutoplayReset = v != 0
			}
		case protowire.BytesType:
			v, n := protowire.ConsumeBytes(data)
			if n < 0 {
				return s
			}
			data = data[n:]
			switch num {
			case 1:
				s.QueueVersion = decodeQueueVersion(v)
			case 2:
				cp := make([]byte, len(v))
				copy(cp, v)
				s.ActionUUID = cp
			case 3:
				s.Tracks = append(s.Tracks, decodeQueueTrackRef(v))
			case 5:
				cp := make([]byte, len(v))
				copy(cp, v)
				s.ContextUUID = cp
			}
		default:
			n := protowire.ConsumeFieldValue(num, typ, data)
			if n < 0 {
				return s
			}
			data = data[n:]
		}
	}
	return s
}

func decodeSrvrCtrlQueueTracksRemoved(data []byte) *PbSrvrCtrlQueueTracksRemoved {
	s := &PbSrvrCtrlQueueTracksRemoved{}
	for len(data) > 0 {
		num, typ, n := protowire.ConsumeTag(data)
		if n < 0 {
			break
		}
		data = data[n:]
		switch typ {
		case protowire.VarintType:
			v, n := protowire.ConsumeVarint(data)
			if n < 0 {
				return s
			}
			data = data[n:]
			switch num {
			case 3:
				s.QueueItemIDs = append(s.QueueItemIDs, uint32(v))
			case 4:
				s.AutoplayReset = v != 0
			}
		case protowire.BytesType:
			v, n := protowire.ConsumeBytes(data)
			if n < 0 {
				return s
			}
			data = data[n:]
			switch num {
			case 1:
				s.QueueVersion = decodeQueueVersion(v)
			case 2:
				cp := make([]byte, len(v))
				copy(cp, v)
				s.ActionUUID = cp
			}
		default:
			n := protowire.ConsumeFieldValue(num, typ, data)
			if n < 0 {
				return s
			}
			data = data[n:]
		}
	}
	return s
}

func decodeSrvrCtrlShuffleModeSet(data []byte) *PbSrvrCtrlShuffleModeSet {
	s := &PbSrvrCtrlShuffleModeSet{}
	for len(data) > 0 {
		num, typ, n := protowire.ConsumeTag(data)
		if n < 0 {
			break
		}
		data = data[n:]
		if typ == protowire.VarintType {
			v, n := protowire.ConsumeVarint(data)
			if n < 0 {
				break
			}
			data = data[n:]
			if num == 3 {
				s.ShuffleOn = v != 0
			}
		} else {
			n := protowire.ConsumeFieldValue(num, typ, data)
			if n < 0 {
				break
			}
			data = data[n:]
		}
	}
	return s
}

func decodeSrvrCtrlLoopModeSet(data []byte) *PbSrvrCtrlLoopModeSet {
	s := &PbSrvrCtrlLoopModeSet{}
	for len(data) > 0 {
		num, typ, n := protowire.ConsumeTag(data)
		if n < 0 {
			break
		}
		data = data[n:]
		if typ == protowire.VarintType {
			v, n := protowire.ConsumeVarint(data)
			if n < 0 {
				break
			}
			data = data[n:]
			if num == 1 {
				s.Mode = int32(v)
			}
		} else {
			n := protowire.ConsumeFieldValue(num, typ, data)
			if n < 0 {
				break
			}
			data = data[n:]
		}
	}
	return s
}

func decodeSrvrCtrlAutoplayTracksLoaded(data []byte) *PbSrvrCtrlAutoplayTracksLoaded {
	s := &PbSrvrCtrlAutoplayTracksLoaded{}
	for len(data) > 0 {
		num, typ, n := protowire.ConsumeTag(data)
		if n < 0 {
			break
		}
		data = data[n:]
		switch typ {
		case protowire.BytesType:
			v, n := protowire.ConsumeBytes(data)
			if n < 0 {
				return s
			}
			data = data[n:]
			switch num {
			case 1:
				s.QueueVersion = decodeQueueVersion(v)
			case 2:
				cp := make([]byte, len(v))
				copy(cp, v)
				s.ActionUUID = cp
			case 3:
				s.Tracks = append(s.Tracks, decodeQueueTrackRef(v))
			case 4:
				cp := make([]byte, len(v))
				copy(cp, v)
				s.ContextUUID = cp
			}
		default:
			n := protowire.ConsumeFieldValue(num, typ, data)
			if n < 0 {
				return s
			}
			data = data[n:]
		}
	}
	return s
}

func decodeSrvrCtrlAutoplayTracksRemoved(data []byte) *PbSrvrCtrlAutoplayTracksRemoved {
	s := &PbSrvrCtrlAutoplayTracksRemoved{}
	for len(data) > 0 {
		num, typ, n := protowire.ConsumeTag(data)
		if n < 0 {
			break
		}
		data = data[n:]
		switch typ {
		case protowire.VarintType:
			v, n := protowire.ConsumeVarint(data)
			if n < 0 {
				return s
			}
			data = data[n:]
			if num == 3 {
				s.QueueItemIDs = append(s.QueueItemIDs, uint32(v))
			}
		case protowire.BytesType:
			v, n := protowire.ConsumeBytes(data)
			if n < 0 {
				return s
			}
			data = data[n:]
			if num == 1 {
				s.QueueVersion = decodeQueueVersion(v)
			}
		default:
			n := protowire.ConsumeFieldValue(num, typ, data)
			if n < 0 {
				return s
			}
			data = data[n:]
		}
	}
	return s
}

func decodeSrvrRndrSetActive(data []byte) *PbSrvrRndrSetActive {
	s := &PbSrvrRndrSetActive{}
	for len(data) > 0 {
		num, typ, n := protowire.ConsumeTag(data)
		if n < 0 {
			break
		}
		data = data[n:]
		if typ == protowire.VarintType {
			v, n := protowire.ConsumeVarint(data)
			if n < 0 {
				break
			}
			data = data[n:]
			if num == 1 {
				s.Active = v != 0
			}
		} else {
			n := protowire.ConsumeFieldValue(num, typ, data)
			if n < 0 {
				break
			}
			data = data[n:]
		}
	}
	return s
}

func decodeSrvrCtrlRemoveRenderer(data []byte) *PbSrvrCtrlRemoveRenderer {
	s := &PbSrvrCtrlRemoveRenderer{}
	for len(data) > 0 {
		num, typ, n := protowire.ConsumeTag(data)
		if n < 0 {
			break
		}
		data = data[n:]
		if typ == protowire.VarintType {
			v, n := protowire.ConsumeVarint(data)
			if n < 0 {
				break
			}
			data = data[n:]
			if num == 1 {
				s.RendererID = v
			}
		} else {
			n := protowire.ConsumeFieldValue(num, typ, data)
			if n < 0 {
				break
			}
			data = data[n:]
		}
	}
	return s
}

func decodeSrvrCtrlQueueCleared(data []byte) *PbSrvrCtrlQueueCleared {
	s := &PbSrvrCtrlQueueCleared{}
	for len(data) > 0 {
		num, typ, n := protowire.ConsumeTag(data)
		if n < 0 {
			break
		}
		data = data[n:]
		switch typ {
		case protowire.BytesType:
			v, n := protowire.ConsumeBytes(data)
			if n < 0 {
				return s
			}
			data = data[n:]
			switch num {
			case 1:
				s.QueueVersion = decodeQueueVersion(v)
			case 2:
				cp := make([]byte, len(v))
				copy(cp, v)
				s.ActionUUID = cp
			}
		default:
			n := protowire.ConsumeFieldValue(num, typ, data)
			if n < 0 {
				return s
			}
			data = data[n:]
		}
	}
	return s
}
