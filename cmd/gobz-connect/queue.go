package main

import (
	"errors"
	"fmt"
	"log"
	"sync"
	"sync/atomic"
	"time"
)

// Audio format IDs matching Qobuz API format_id values.
const (
	AudioFormatMP3      = 5
	AudioFormatFLAC     = 6
	AudioFormatHiRes96  = 7
	AudioFormatHiRes192 = 27
)

// TrackState is the loading phase of a queued track.
type TrackState int

const (
	TrackQueued      TrackState = iota // initial state
	TrackPendingMeta                   // metadata being fetched
	TrackStreamable                    // metadata loaded
	TrackPendingFile                   // file URL being fetched
	TrackReady                         // ready to play
	TrackFailed                        // unrecoverable error
)

// QueueTrack holds all resolved metadata and playback info for a single track.
type QueueTrack struct {
	ID     uint32
	Index  uint64 // queueItemId from server
	Format int
	State  TrackState

	Title       string
	FileURL     string
	ContextUUID string
	Blob        string

	ArtistID   uint64
	ArtistName string

	AlbumID         string
	AlbumName       string
	AlbumLargeImage string
	AlbumGenreID    uint64
	AlbumLabelID    uint64

	DurationMs       uint64
	StartMs          uint64
	StartedPlayingAt uint64

	SamplingRate int
	BitsDepth    int
	NChannels    int

	WantSkip atomic.Bool
	SkipTo   atomic.Int64
}

// ContextJSON returns a JSON fragment with track identifiers for the suggestions API.
func (t *QueueTrack) ContextJSON() string {
	s := fmt.Sprintf(`{"track_id":%d`, t.ID)
	if t.ArtistID > 0 {
		s += fmt.Sprintf(`,"artist_id":%d`, t.ArtistID)
	}
	if t.AlbumLabelID > 0 {
		s += fmt.Sprintf(`,"label_id":%d`, t.AlbumLabelID)
	}
	if t.AlbumGenreID > 0 {
		s += fmt.Sprintf(`,"genre_id":%d`, t.AlbumGenreID)
	}
	return s + "}"
}

// uuidFromBytes formats 16 raw bytes as a UUID string.
func uuidFromBytes(b []byte) string {
	if len(b) < 16 {
		return ""
	}
	return fmt.Sprintf("%02x%02x%02x%02x-%02x%02x-%02x%02x-%02x%02x-%02x%02x%02x%02x%02x%02x",
		b[0], b[1], b[2], b[3], b[4], b[5], b[6], b[7],
		b[8], b[9], b[10], b[11], b[12], b[13], b[14], b[15])
}

// Queue manages the track queue with background preloading.
type Queue struct {
	api         *QobuzAPI
	sessionID   []byte
	audioFormat int
	sendMsg     func([]*PbQConnectMessage)
	cache       *TrackCache

	mu              sync.Mutex
	refs            []*PbQueueTrackRef
	shuffledIndexes []int
	index           int
	notifyCh        chan struct{} // closed/signalled when queue state changes

	preloadedMu     sync.Mutex
	preloaded       []*QueueTrack
	pendingStartMs  uint64 // applied to the first track added to preloaded
	expandedCache   []string // up to 5 context JSON strings for suggestions
	fetchedAutoplay bool
	autoplayMode    atomic.Bool // true when server has enabled autoplay for this queue

	QueueState *PbSrvrCtrlQueueState

	stopped atomic.Bool
	stopCh  chan struct{}
}

// NewQueue creates a Queue. audioFormat should be one of the AudioFormat* constants.
func NewQueue(sessionID []byte, api *QobuzAPI, audioFormat int) *Queue {
	return &Queue{
		api:         api,
		sessionID:   sessionID,
		audioFormat: audioFormat,
		QueueState:  &PbSrvrCtrlQueueState{},
		stopCh:      make(chan struct{}),
		notifyCh:    make(chan struct{}, 1),
	}
}

// SetSendMsg sets the callback used to send WebSocket messages.
func (q *Queue) SetSendMsg(f func([]*PbQConnectMessage)) { q.sendMsg = f }

// SetAudioFormat updates the preferred audio format for future tracks.
func (q *Queue) SetAudioFormat(format int) {
	q.mu.Lock()
	q.audioFormat = format
	q.mu.Unlock()
}

// SetCache attaches a TrackCache to the queue for background pre-downloading.
func (q *Queue) SetCache(tc *TrackCache) { q.cache = tc }

// Stop terminates the Run goroutine.
func (q *Queue) Stop() {
	if q.stopped.CompareAndSwap(false, true) {
		close(q.stopCh)
	}
}

// notify wakes Queue.Run() if it is blocked waiting for work.
func (q *Queue) notify() {
	select {
	case q.notifyCh <- struct{}{}:
	default:
	}
}

// Run is the background goroutine that fetches metadata and file URLs.
func (q *Queue) Run() {
	for {
		select {
		case <-q.stopCh:
			return
		default:
		}

		q.mu.Lock()
		hasRefs := len(q.refs) > 0
		q.mu.Unlock()

		if !hasRefs {
			select {
			case <-q.notifyCh:
			case <-q.stopCh:
				return
			}
			continue
		}

		// Fill preloaded buffer up to 3 tracks.
		q.mu.Lock()
		q.preloadedMu.Lock()
		for len(q.preloaded) < 3 {
			isFirst := len(q.preloaded) == 0
			idx := q.index + len(q.preloaded)
			if len(q.shuffledIndexes) > 0 && idx < len(q.shuffledIndexes) {
				idx = q.shuffledIndexes[idx]
			}
			if idx >= len(q.refs) {
				break
			}
			ref := q.refs[idx]
			track := &QueueTrack{
				ID:     ref.TrackID,
				Index:  ref.QueueItemID,
				Format: q.audioFormat,
				State:  TrackQueued,
			}
			if len(ref.ContextUUID) >= 16 {
				track.ContextUUID = uuidFromBytes(ref.ContextUUID)
			}
			// Apply any pending start position to the first (current) track.
			if isFirst && q.pendingStartMs > 0 {
				track.StartMs = q.pendingStartMs
				q.pendingStartMs = 0
			}
			q.preloaded = append(q.preloaded, track)
		}

		// Fetch suggestions if running low, autoplay is enabled, and we have context.
		needSuggestions := !q.fetchedAutoplay && len(q.preloaded) < 2 && len(q.expandedCache) > 0 &&
			q.autoplayMode.Load()
		q.preloadedMu.Unlock()
		q.mu.Unlock()

		if needSuggestions {
			if !q.getSuggestions() {
				return
			}
			q.preloadedMu.Lock()
			q.fetchedAutoplay = true
			q.preloadedMu.Unlock()
			continue
		}

		q.preloadedMu.Lock()
		tracks := make([]*QueueTrack, len(q.preloaded))
		copy(tracks, q.preloaded)
		q.preloadedMu.Unlock()

		processed := false
		for _, track := range tracks {
			switch track.State {
			case TrackQueued:
				track.State = TrackPendingMeta
				if q.fetchMetadata(track) {
					processed = true
				}
				// fetchMetadata sets TrackFailed or reverts to TrackQueued on failure.
			case TrackStreamable:
				track.State = TrackPendingFile
				if q.fetchFileURL(track) {
					ctx := track.ContextJSON()
					q.mu.Lock()
					q.expandedCache = append(q.expandedCache, ctx)
					if len(q.expandedCache) > 5 {
						q.expandedCache = q.expandedCache[1:]
					}
					q.mu.Unlock()
				}
				// fetchFileURL sets TrackFailed or reverts to TrackStreamable on failure.
			}
			if processed {
				break
			}
		}

		if !processed {
			select {
			case <-q.notifyCh:
			case <-q.stopCh:
				return
			}
		}
	}
}


func (q *Queue) fetchMetadata(track *QueueTrack) bool {
	meta, err := q.api.GetTrackMetadata(track.ID)
	if err != nil {
		log.Printf("queue: getMetadata id=%d: %v", track.ID, err)
		if errors.Is(err, ErrTokenUnavailable) {
			// Transient: jwt_api was unavailable, not a real content/API
			// error. Revert to TrackQueued so this track is retried once
			// the app reconnects, instead of being permanently skipped.
			track.State = TrackQueued
		} else {
			track.State = TrackFailed
		}
		return false
	}
	if streamable, _ := meta["streamable"].(bool); !streamable {
		log.Printf("queue: track %d not streamable", track.ID)
		track.State = TrackFailed
		return false
	}
	// Downgrade format if hi-res not available.
	if track.Format > AudioFormatFLAC {
		if hiRes, _ := meta["hires_streamable"].(bool); !hiRes {
			track.Format = AudioFormatFLAC
		} else if maxSR, ok := meta["maximum_sampling_rate"].(float64); ok {
			maxHz := int(maxSR * 1000)
			// Cap at HiRes96 for tracks whose max SR is ≤ 96 kHz.
			// Note: 24-bit/44.1 kHz tracks are hi-res (hires_streamable=true)
			// with maxHz=44100 — they must NOT be downgraded to AudioFormatFLAC
			// (format 6 = 16-bit); requesting format 7 returns the 24-bit FLAC.
			if maxHz <= 96000 {
				track.Format = AudioFormatHiRes96
			}
		}
	}
	if dur, ok := meta["duration"].(float64); ok {
		track.DurationMs = uint64(dur) * 1000
	}
	if ch, ok := meta["maximum_channel_count"].(float64); ok {
		track.NChannels = int(ch)
	}
	if title, ok := meta["title"].(string); ok {
		track.Title = title
	}
	if performer, ok := meta["performer"].(map[string]interface{}); ok {
		if id, ok := performer["id"].(float64); ok {
			track.ArtistID = uint64(id)
		}
		if name, ok := performer["name"].(string); ok {
			track.ArtistName = name
		}
	}
	if album, ok := meta["album"].(map[string]interface{}); ok {
		if id, ok := album["id"].(string); ok {
			track.AlbumID = id
		}
		if title, ok := album["title"].(string); ok {
			track.AlbumName = title
		}
		if image, ok := album["image"].(map[string]interface{}); ok {
			if large, ok := image["large"].(string); ok {
				track.AlbumLargeImage = large
			}
		}
		if genre, ok := album["genre"].(map[string]interface{}); ok {
			if id, ok := genre["id"].(float64); ok {
				track.AlbumGenreID = uint64(id)
			}
		}
		if label, ok := album["label"].(map[string]interface{}); ok {
			if id, ok := label["id"].(float64); ok {
				track.AlbumLabelID = uint64(id)
			}
		}
	}
	track.State = TrackStreamable
	return true
}

func (q *Queue) fetchFileURL(track *QueueTrack) bool {
	result, err := q.api.GetFileURL(track.ID, track.Format)
	if err != nil {
		log.Printf("queue: getFileUrl id=%d: %v", track.ID, err)
		if errors.Is(err, ErrTokenUnavailable) {
			track.State = TrackStreamable // retry once a token becomes available again
		} else {
			track.State = TrackFailed
		}
		return false
	}
	if status, _ := result["status"].(string); status == "error" {
		track.State = TrackFailed
		return false
	}
	url, ok := result["url"].(string)
	if !ok || url == "" {
		track.State = TrackFailed
		return false
	}
	track.FileURL = url
	if blob, ok := result["blob"].(string); ok {
		track.Blob = blob
	}
	if dur, ok := result["duration"].(float64); ok {
		track.DurationMs = uint64(dur * 1000)
	}
	if ch, ok := result["n_channels"].(float64); ok {
		track.NChannels = int(ch)
	}
	if bd, ok := result["bit_depth"].(float64); ok {
		track.BitsDepth = int(bd)
	}
	if sr, ok := result["sampling_rate"].(float64); ok {
		track.SamplingRate = int(sr * 1000)
	}
	log.Printf("queue: track %d ready: %dms %dch %dbit %dHz",
		track.ID, track.DurationMs, track.NChannels, track.BitsDepth, track.SamplingRate)
	track.State = TrackReady
	if q.cache != nil {
		q.cache.EnsureDownload(track)
	}
	return true
}

// ConsumeTrack returns the next ready track, removing prevTrack from preloaded.
// Blocks until a track is available, the queue is stopped, or done is closed.
// done should be the player's per-session stop channel so that player.Stop()
// unblocks this call immediately.
func (q *Queue) ConsumeTrack(prevTrack *QueueTrack, done <-chan struct{}) (*QueueTrack, int32) {
	q.mu.Lock()
	empty := len(q.refs) == 0
	q.mu.Unlock()
	if empty {
		return nil, 0
	}

	sleep := func() bool {
		select {
		case <-time.After(100 * time.Millisecond):
			return true
		case <-q.stopCh:
			return false
		case <-done:
			return false
		}
	}

	for {
		select {
		case <-q.stopCh:
			return nil, 0
		case <-done:
			return nil, 0
		default:
		}

		if q.fetchedAutoplay {
			if !sleep() {
				return nil, 0
			}
			continue
		}

		q.preloadedMu.Lock()

		// Remove the previous track from the preloaded list.
		if prevTrack != nil {
			for i, t := range q.preloaded {
				if t == prevTrack {
					q.mu.Lock()
					q.index++
					q.mu.Unlock()
					q.preloaded = append(q.preloaded[:i], q.preloaded[i+1:]...)
					prevTrack = nil
					q.notify() // a slot opened — wake Run() to preload the next track
					break
				}
			}
		}

		if len(q.preloaded) == 0 {
			q.preloadedMu.Unlock()
			q.mu.Lock()
			if q.index >= len(q.refs) {
				q.mu.Unlock()
				return nil, 0
			}
			q.mu.Unlock()
			if !sleep() {
				return nil, 0
			}
			continue
		}

		track := q.preloaded[0]
		nextID := int32(-1) // -1 = no next track loaded
		if len(q.preloaded) > 1 {
			nextID = int32(q.preloaded[1].Index)
		}
		q.preloadedMu.Unlock()

		if track.State != TrackReady && track.State != TrackFailed {
			if !sleep() {
				return nil, 0
			}
			continue
		}
		q.mu.Lock()
		idx := q.index
		total := len(q.refs)
		q.mu.Unlock()
		log.Printf("queue: ConsumeTrack → trackID=%d queueItemID=%d idx=%d/%d nextQueueItemID=%d",
			track.ID, track.Index, idx, total, nextID)
		return track, nextID
	}
}

// queueSnapshot returns a short human-readable summary of the first few refs.
// Must be called with q.mu held.
func (q *Queue) queueSnapshot() string {
	n := len(q.refs)
	show := n
	if show > 5 {
		show = 5
	}
	s := fmt.Sprintf("refs=%d idx=%d [", n, q.index)
	for i := 0; i < show; i++ {
		if i > 0 {
			s += " "
		}
		s += fmt.Sprintf("%d:t%d", q.refs[i].QueueItemID, q.refs[i].TrackID)
	}
	if n > show {
		s += fmt.Sprintf(" ...+%d", n-show)
	}
	s += "]"
	return s
}

// AddTracks adds queue track refs, optionally inserting after a position.
func (q *Queue) AddTracks(refs []*PbQueueTrackRef, insertAfter *int, contextUUID []byte) {
	q.mu.Lock()
	defer q.mu.Unlock()

	before := len(q.refs)
	if insertAfter != nil {
		pos := *insertAfter
		if pos < len(q.shuffledIndexes) {
			pos = q.shuffledIndexes[pos]
		}
		inserted := 0
		for _, ref := range refs {
			// Mirror C++: for insertions, deduplicate by TrackID from the
			// current index onwards.  When a duplicate is found, update the
			// existing entry's QueueItemID (the server may have re-numbered
			// it) and also refresh the matching preloaded track's Index.
			dup := false
			for j := q.index; j < len(q.refs); j++ {
				if q.refs[j].TrackID == ref.TrackID {
					dup = true
					q.refs[j].QueueItemID = ref.QueueItemID
					// Keep the preloaded track's Index in sync.
					q.preloadedMu.Lock()
					preloadedOffset := j - q.index
					if preloadedOffset >= 0 && preloadedOffset < len(q.preloaded) {
						q.preloaded[preloadedOffset].Index = ref.QueueItemID
					}
					q.preloadedMu.Unlock()
					break
				}
			}
			if dup {
				continue
			}
			if contextUUID != nil && len(ref.ContextUUID) == 0 {
				cp := make([]byte, len(contextUUID))
				copy(cp, contextUUID)
				ref.ContextUUID = cp
			}
			ins := pos + inserted + 1
			if ins > len(q.refs) {
				ins = len(q.refs)
			}
			newRefs := make([]*PbQueueTrackRef, len(q.refs)+1)
			copy(newRefs[:ins], q.refs[:ins])
			newRefs[ins] = ref
			copy(newRefs[ins+1:], q.refs[ins:])
			q.refs = newRefs
			inserted++
		}
	} else {
		for _, ref := range refs {
			if contextUUID != nil && len(ref.ContextUUID) == 0 {
				cp := make([]byte, len(contextUUID))
				copy(cp, contextUUID)
				ref.ContextUUID = cp
			}
			q.refs = append(q.refs, ref)
		}
	}
	q.growShuffleIndexes()
	added := len(q.refs) - before
	where := "append"
	if insertAfter != nil {
		where = fmt.Sprintf("insert-after=%d", *insertAfter)
	}
	log.Printf("queue: AddTracks +%d refs (%s) %s", added, where, q.queueSnapshot())
	q.notify()
}

// DeleteAllTracks clears the entire queue.
func (q *Queue) DeleteAllTracks() {
	q.mu.Lock()
	n := len(q.refs)
	q.refs = nil
	q.shuffledIndexes = nil
	q.index = 0
	q.mu.Unlock()
	q.preloadedMu.Lock()
	q.preloaded = nil
	q.pendingStartMs = 0
	q.expandedCache = nil
	q.fetchedAutoplay = false
	q.preloadedMu.Unlock()
	log.Printf("queue: DeleteAllTracks cleared %d refs", n)
}

// DeleteAutoplayTracks removes tracks beyond the regular (pre-autoplay) set.
func (q *Queue) DeleteAutoplayTracks() {
	q.mu.Lock()
	q.preloadedMu.Lock()
	regularSize := len(q.shuffledIndexes)
	if len(q.refs) > regularSize {
		q.refs = q.refs[:regularSize]
	}
	q.fetchedAutoplay = false
	q.preloadedMu.Unlock()
	q.mu.Unlock()
}

// DeleteTracksByQueueItemID removes tracks by their server-assigned queueItemId.
func (q *Queue) DeleteTracksByQueueItemID(ids []uint32) {
	q.mu.Lock()
	q.preloadedMu.Lock()
	for _, id := range ids {
		for i, ref := range q.refs {
			if ref.QueueItemID != uint64(id) {
				continue
			}
			for j, pt := range q.preloaded {
				if pt.Index == uint64(id) {
					q.preloaded = append(q.preloaded[:j], q.preloaded[j+1:]...)
					break
				}
			}
			found := -1
			for j, si := range q.shuffledIndexes {
				if si == i {
					found = j
					break
				}
			}
			if found >= 0 {
				q.shuffledIndexes = append(q.shuffledIndexes[:found], q.shuffledIndexes[found+1:]...)
				for j := range q.shuffledIndexes {
					if q.shuffledIndexes[j] > i {
						q.shuffledIndexes[j]--
					}
				}
				if q.index >= i {
					q.index--
				}
			}
			q.refs = append(q.refs[:i], q.refs[i+1:]...)
			break
		}
	}
	q.preloadedMu.Unlock()
	q.mu.Unlock()
}

// SetIndex moves the queue cursor to the track with the given queueItemId.
func (q *Queue) SetIndex(queueItemID uint64) {
	q.mu.Lock()
	q.preloadedMu.Lock()
	oldIndex := q.index
	found := false
	for i, ref := range q.refs {
		if ref.QueueItemID == queueItemID {
			q.index = i
			found = true
			break
		}
	}
	if found {
		ref := q.refs[q.index]
		log.Printf("queue: SetIndex queueItemID=%d → index=%d (was %d) trackID=%d  refs=%d",
			queueItemID, q.index, oldIndex, ref.TrackID, len(q.refs))
	} else {
		log.Printf("queue: SetIndex queueItemID=%d not found (index unchanged=%d refs=%d)",
			queueItemID, q.index, len(q.refs))
	}
	q.reorderPreloaded()
	q.preloadedMu.Unlock()
	q.mu.Unlock()
	q.notify() // preloaded was rebuilt — wake Run() to refill if needed
}

// SetStartAt sets the start-position in ms for the front preloaded track.
// If the preloaded buffer is not yet populated (track still being fetched),
// the value is saved as pendingStartMs and applied when the track appears.
func (q *Queue) SetStartAt(startMs uint64) {
	q.preloadedMu.Lock()
	q.pendingStartMs = startMs
	if len(q.preloaded) > 0 {
		q.preloaded[0].StartMs = startMs
	}
	q.preloadedMu.Unlock()
}

// RegularTracksSize returns the count of non-autoplay queue entries.
func (q *Queue) RegularTracksSize() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.shuffledIndexes)
}

// SetAutoplayMode enables or disables autoplay track fetching. Called when
// SrvrCtrlAutoplayModeSet arrives so runtime changes take effect immediately.
func (q *Queue) SetAutoplayMode(enabled bool) {
	q.autoplayMode.Store(enabled)
	log.Printf("queue: autoplay mode → %v", enabled)
}

// ConsumeQueueState applies a full queue state snapshot received from the server.
// Returns true when the server's reported track count differs from the local
// refs count, indicating a QueueTracksLoaded is expected imminently and playback
// should be deferred until the new queue arrives.
func (q *Queue) ConsumeQueueState(state *PbSrvrCtrlQueueState) (staleQueue bool) {
	q.autoplayMode.Store(state.AutoplayMode)
	q.mu.Lock()
	q.QueueState = state
	q.shuffledIndexes = nil
	for _, si := range state.ShuffledTrackIndexes {
		q.shuffledIndexes = append(q.shuffledIndexes, int(si))
	}
	if len(q.shuffledIndexes) < len(state.Tracks) {
		for i := len(q.shuffledIndexes); i < len(state.Tracks); i++ {
			q.shuffledIndexes = append(q.shuffledIndexes, i)
		}
	}
	refsUpdated := false
	// Detect stale queue before any modification: if we have existing refs but
	// the server reports a different total track count, a QueueTracksLoaded is
	// coming.  Must compare against the full refs count (manual + autoplay) —
	// comparing only state.Tracks against refs always triggers a false positive
	// when autoplay tracks are present.
	totalStateTracks := len(state.Tracks) + len(state.AutoplayTracks)
	if len(q.refs) > 0 && totalStateTracks > 0 && totalStateTracks != len(q.refs) {
		staleQueue = true
	}
	if len(q.refs) == 0 {
		q.refs = append(q.refs, state.Tracks...)
		q.refs = append(q.refs, state.AutoplayTracks...)
		refsUpdated = true
	}
	q.preloadedMu.Lock()
	if refsUpdated {
		// First population: nothing useful is preloaded yet.
		q.preloaded = nil
	} else {
		// Queue snapshot arrived while the player is running.  Preserve any
		// preloaded tracks that are still at the correct queue position so
		// that ConsumeTrack can find prevQT by pointer and increment q.index.
		// Wiping preloaded here breaks the pointer identity check in
		// ConsumeTrack: q.index is never incremented, Run() refills from the
		// same position, and the current track plays twice.
		q.reorderPreloaded()
	}
	q.preloadedMu.Unlock()
	log.Printf("queue: ConsumeQueueState stateTracks=%d autoplay=%d shuffle=%d refs=%d refsUpdated=%v stale=%v idx=%d autoplayMode=%v",
		len(state.Tracks), len(state.AutoplayTracks), len(state.ShuffledTrackIndexes),
		len(q.refs), refsUpdated, staleQueue, q.index, state.AutoplayMode)
	q.mu.Unlock()
	q.notify()
	return
}

// growShuffleIndexes extends shuffledIndexes to cover all current refs.
// Must be called with q.mu held.
func (q *Queue) growShuffleIndexes() {
	for i := len(q.shuffledIndexes); i < len(q.refs); i++ {
		q.shuffledIndexes = append(q.shuffledIndexes, i)
	}
}

// GrowShuffleIndexes is the exported version of growShuffleIndexes.
func (q *Queue) GrowShuffleIndexes() {
	q.mu.Lock()
	q.growShuffleIndexes()
	q.mu.Unlock()
}

// Position finds the shuffle-ordered logical index of the track with the given queueItemId.
func (q *Queue) Position(queueItemID uint64) (int, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for i, ref := range q.refs {
		if ref.QueueItemID == queueItemID {
			if len(q.shuffledIndexes) > 0 && i < len(q.shuffledIndexes) {
				for j, si := range q.shuffledIndexes {
					if si == i {
						return j, true
					}
				}
			}
			return i, true
		}
	}
	return 0, false
}

// getSuggestions fetches autoplay suggestions and notifies the server.
func (q *Queue) getSuggestions() bool {
	q.mu.Lock()
	if len(q.refs) == 0 {
		q.mu.Unlock()
		return false
	}
	listenedIDs := make([]uint32, len(q.refs))
	for i, ref := range q.refs {
		listenedIDs[i] = ref.TrackID
	}
	ctxCopy := make([]string, len(q.expandedCache))
	copy(ctxCopy, q.expandedCache)
	qs := q.QueueState
	q.mu.Unlock()

	trackIDs, err := q.api.GetSuggestions(listenedIDs, ctxCopy)
	if err != nil {
		log.Printf("queue: getSuggestions: %v", err)
		return false
	}
	if len(trackIDs) == 0 {
		return false
	}

	q.preloadedMu.Lock()
	var allIDs []uint32
	for _, pt := range q.preloaded {
		allIDs = append(allIDs, pt.ID)
	}
	q.preloadedMu.Unlock()
	allIDs = append(allIDs, trackIDs...)

	msg := &PbQConnectMessage{
		MessageType: MsgTypeCtrlSrvrAutoplayAddTracks,
		CtrlSrvrAutoplayLoadTracks: &PbCtrlSrvrAutoplayLoadTracks{
			TrackIDs:    allIDs,
			ContextUUID: q.sessionID,
		},
	}
	if qs != nil {
		msg.CtrlSrvrAutoplayLoadTracks.QueueVersion = qs.QueueVersion
		msg.CtrlSrvrAutoplayLoadTracks.ActionUUID = qs.ActionUUID
	}
	if q.sendMsg != nil {
		q.sendMsg([]*PbQConnectMessage{msg})
	}
	return true
}

// reorderPreloaded re-aligns the preloaded slice to start at q.index.
// Must be called with both q.mu and q.preloadedMu held.
func (q *Queue) reorderPreloaded() {
	var newPreloaded []*QueueTrack
	for i := 0; ; i++ {
		qidx := q.index + i
		if len(q.shuffledIndexes) > 0 && qidx < len(q.shuffledIndexes) {
			qidx = q.shuffledIndexes[qidx]
		}
		if qidx >= len(q.refs) {
			break
		}
		wantID := q.refs[qidx].QueueItemID
		found := false
		for _, pt := range q.preloaded {
			if pt.Index == wantID {
				newPreloaded = append(newPreloaded, pt)
				found = true
				break
			}
		}
		if !found || i >= len(q.preloaded) {
			break
		}
	}
	q.preloaded = newPreloaded
}
