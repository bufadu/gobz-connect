package main

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// activeDownload tracks a file that is currently being written by a download
// goroutine. Readers attach to it and block in Read until more bytes arrive.
type activeDownload struct {
	mu      sync.Mutex
	written int64 // bytes written so far
	done    bool
	err     error
	cond    *sync.Cond // broadcasts after each chunk and on completion
}

func newActiveDownload() *activeDownload {
	dl := &activeDownload{}
	dl.cond = sync.NewCond(&dl.mu)
	return dl
}

func (dl *activeDownload) addWritten(n int64) {
	dl.mu.Lock()
	dl.written += n
	dl.mu.Unlock()
	dl.cond.Broadcast()
}

// totalWritten returns the number of bytes written so far. Safe to call
// after the download goroutine has finished (or concurrently, if needed).
func (dl *activeDownload) totalWritten() int64 {
	dl.mu.Lock()
	defer dl.mu.Unlock()
	return dl.written
}

func (dl *activeDownload) finish(err error) {
	dl.mu.Lock()
	dl.done = true
	dl.err = err
	dl.mu.Unlock()
	dl.cond.Broadcast()
}

// downloadingReader gives the audio decoder read access to a file that is
// still being downloaded. Read blocks transparently when it reaches the
// download frontier, resuming as soon as more data is written.
type downloadingReader struct {
	f      *os.File
	dl     *activeDownload
	pos    int64
	closed atomic.Bool
}

func (r *downloadingReader) Read(p []byte) (int, error) {
	r.dl.mu.Lock()
	for r.pos >= r.dl.written && !r.dl.done && !r.closed.Load() {
		r.dl.cond.Wait()
	}
	if r.closed.Load() {
		r.dl.mu.Unlock()
		return 0, io.ErrClosedPipe
	}
	available := r.dl.written - r.pos
	dlErr := r.dl.err
	r.dl.mu.Unlock()

	if available <= 0 {
		if dlErr != nil {
			return 0, dlErr
		}
		return 0, io.EOF
	}

	toRead := int64(len(p))
	if toRead > available {
		toRead = available
	}
	// ReadAt uses pread(2): does not move the writer's file position.
	n, err := r.f.ReadAt(p[:toRead], r.pos)
	if n > 0 {
		r.pos += int64(n)
	}
	return n, err
}

func (r *downloadingReader) Seek(offset int64, whence int) (int64, error) {
	var newPos int64
	switch whence {
	case io.SeekStart:
		newPos = offset
	case io.SeekCurrent:
		newPos = r.pos + offset
	case io.SeekEnd:
		// Rare: wait for the download to finish so we know the total size.
		r.dl.mu.Lock()
		for !r.dl.done {
			r.dl.cond.Wait()
		}
		written, dlErr := r.dl.written, r.dl.err
		r.dl.mu.Unlock()
		if dlErr != nil {
			return 0, dlErr
		}
		newPos = written + offset
	default:
		return 0, fmt.Errorf("downloadingReader: unknown whence %d", whence)
	}
	if newPos < 0 {
		return 0, fmt.Errorf("downloadingReader: negative seek position")
	}
	r.pos = newPos
	return newPos, nil
}

func (r *downloadingReader) Close() error {
	r.closed.Store(true)
	// Wake any Read blocked in cond.Wait so it sees closed=true and exits.
	r.dl.mu.Lock()
	r.dl.cond.Broadcast()
	r.dl.mu.Unlock()
	return r.f.Close()
}

// pendingDownload is a download request queued because all background slots
// are occupied.
type pendingDownload struct {
	trackID uint32
	key     string
	fileURL string
	path    string
}

// TrackCache downloads tracks to a local directory and serves them during
// playback. A single download feeds both the audio player (via a streaming
// reader) and the on-disk cache. Total disk usage is kept below maxBytes via
// LRU eviction.
//
// To limit SD-card I/O pressure, at most maxConc background (EnsureDownload)
// downloads run simultaneously. Additional requests are held in a FIFO queue
// and started as slots free up. Open (called just before playback begins)
// always starts its download immediately, removing the track from the pending
// queue first if necessary.
type TrackCache struct {
	dir      string
	maxBytes int64
	maxConc  int // max concurrent background (EnsureDownload) downloads

	bgRateBytesPerSec int64 // 0 = unlimited; caps background download writes

	mu        sync.Mutex
	active    map[string]*activeDownload // all downloads currently in progress
	pending   []pendingDownload          // queued, waiting for a background slot
	concCount int                        // background slots currently in use
}

// NewTrackCache creates the cache directory and starts the eviction goroutine.
// bgRateKBps caps the write speed of background (pre-warm) downloads in KB/s;
// 0 means unlimited.
func NewTrackCache(dir string, maxMB int, bgRateKBps int) (*TrackCache, error) {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("cache: mkdir %s: %w", dir, err)
	}
	tc := &TrackCache{
		dir:               dir,
		maxBytes:          int64(maxMB) * 1024 * 1024,
		maxConc:           2, // current + next track pre-warm simultaneously
		bgRateBytesPerSec: int64(bgRateKBps) * 1024,
		active:            make(map[string]*activeDownload),
	}
	go tc.evictLoop()
	return tc, nil
}

func (tc *TrackCache) cacheKey(trackID uint32, format int) string {
	ext := "flac"
	if format == AudioFormatMP3 {
		ext = "mp3"
	}
	return fmt.Sprintf("%d_%d.%s", trackID, format, ext)
}

func (tc *TrackCache) cachePath(key string) string {
	return filepath.Join(tc.dir, key)
}

// EnsureDownload queues a background download for track if it is not already
// cached or in progress. At most maxConc downloads run simultaneously; extras
// are held in a FIFO pending queue and started as slots free up.
func (tc *TrackCache) EnsureDownload(track *QueueTrack) {
	if track.FileURL == "" {
		return
	}
	key := tc.cacheKey(track.ID, track.Format)
	path := tc.cachePath(key)
	tc.mu.Lock()
	defer tc.mu.Unlock()
	if _, exists := tc.active[key]; exists {
		return // already downloading
	}
	if _, err := os.Stat(path); err == nil {
		return // already on disk
	}
	for _, p := range tc.pending {
		if p.key == key {
			return // already queued
		}
	}
	if tc.concCount < tc.maxConc {
		tc.startDownloadLocked(track.ID, key, track.FileURL, path, true)
	} else {
		tc.pending = append(tc.pending, pendingDownload{track.ID, key, track.FileURL, path})
		log.Printf("cache: track %d queued for download (%d pending)", track.ID, len(tc.pending))
	}
}

// Open returns a ReadSeekCloser for the track's audio data:
//   - Cache hit (file fully on disk): opens the file directly.
//   - Download in progress: attaches a streaming reader; Read blocks at the
//     download frontier until more bytes are available.
//   - Not cached: starts a download immediately (bypassing the background
//     concurrency limit) so playback is never delayed by queued pre-warms.
func (tc *TrackCache) Open(track *QueueTrack) (io.ReadSeekCloser, error) {
	key := tc.cacheKey(track.ID, track.Format)
	path := tc.cachePath(key)

	tc.mu.Lock()
	dl, exists := tc.active[key]
	if !exists {
		if _, err := os.Stat(path); err == nil {
			// Fully cached — open directly.
			tc.mu.Unlock()
			f, err := os.Open(path)
			if err != nil {
				return nil, err
			}
			now := time.Now()
			os.Chtimes(path, now, now)
			log.Printf("cache: track %d from disk cache", track.ID)
			return f, nil
		}
		// Not cached, not in progress: remove from pending queue (playback
		// takes priority over background pre-warm ordering) and start now.
		tc.removePendingLocked(key)
		dl = tc.startDownloadLocked(track.ID, key, track.FileURL, path, false)
		if dl == nil {
			tc.mu.Unlock()
			return nil, fmt.Errorf("cache: could not start download for track %d", track.ID)
		}
	}
	tc.mu.Unlock()

	// Open the (possibly still-empty) file for reading.
	// Read will block until bytes are available.
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("cache: open track %d: %w", track.ID, err)
	}
	now := time.Now()
	os.Chtimes(path, now, now)
	log.Printf("cache: track %d streaming from download", track.ID)
	return &downloadingReader{f: f, dl: dl}, nil
}

// startDownloadLocked creates the cache file, registers the activeDownload,
// and spawns a download goroutine.
// If counted=true the download occupies one background concurrency slot and
// triggers startNextPendingLocked when it finishes.
// Must be called with tc.mu held.
func (tc *TrackCache) startDownloadLocked(trackID uint32, key, fileURL, path string, counted bool) *activeDownload {
	f, err := os.Create(path)
	if err != nil {
		log.Printf("cache: create %s: %v", path, err)
		return nil
	}
	dl := newActiveDownload()
	tc.active[key] = dl
	if counted {
		tc.concCount++
	}

	go func() {
		start := time.Now()
		dlErr := tc.downloadInto(f, dl, fileURL, counted)
		f.Close()
		if dlErr != nil {
			log.Printf("cache: download track %d: %v", trackID, dlErr)
			os.Remove(path)
		} else {
			elapsed := time.Since(start)
			written := dl.totalWritten()
			var speedKBps float64
			if elapsed > 0 {
				speedKBps = float64(written) / 1024 / elapsed.Seconds()
			}
			log.Printf("cache: stored track %d (%s) — %.1f MB in %.1fs (%.0f KB/s)",
				trackID, key, float64(written)/(1024*1024), elapsed.Seconds(), speedKBps)
		}
		dl.finish(dlErr)
		tc.mu.Lock()
		delete(tc.active, key)
		if counted {
			tc.concCount--
		}
		// Always try to drain the pending queue when any download completes.
		tc.startNextPendingLocked()
		tc.mu.Unlock()
	}()
	return dl
}

// startNextPendingLocked starts the next pending download if a background
// slot is available. Must be called with tc.mu held.
func (tc *TrackCache) startNextPendingLocked() {
	for len(tc.pending) > 0 && tc.concCount < tc.maxConc {
		entry := tc.pending[0]
		tc.pending = tc.pending[1:]
		if _, exists := tc.active[entry.key]; exists {
			continue // already downloading (e.g. started by Open)
		}
		if _, err := os.Stat(entry.path); err == nil {
			continue // already on disk
		}
		tc.startDownloadLocked(entry.trackID, entry.key, entry.fileURL, entry.path, true)
		return
	}
}

// removePendingLocked removes the pending queue entry for key, if present.
// Must be called with tc.mu held.
func (tc *TrackCache) removePendingLocked(key string) {
	for i, p := range tc.pending {
		if p.key == key {
			tc.pending = append(tc.pending[:i], tc.pending[i+1:]...)
			return
		}
	}
}

// fadviseFlushBytes is the write granularity at which POSIX_FADV_DONTNEED is
// applied. Large enough to align reasonably with typical flash erase-block
// sizes (avoiding extra SD-card wear from too-frequent small flushes), small
// enough to keep any single writeback burst well below the volume that was
// causing multi-second iowait stalls on the ALSA write goroutine.
const fadviseFlushBytes = 512 * 1024

// downloadInto streams url into f, notifying dl after each chunk.
// background=true throttles writes to tc.bgRateBytesPerSec (if set) to reduce
// SD-card I/O pressure during pre-warm downloads — the next track doesn't
// need to arrive at network speed, it just needs to be ready before the
// current track ends. POSIX_FADV_DONTNEED is applied every fadviseFlushBytes
// on Linux regardless of background, preventing dirty-page bursts that cause
// iowait spikes without fragmenting writes into flushes smaller than a
// typical flash erase block (which would needlessly increase SD-card wear
// for no iowait benefit).
func (tc *TrackCache) downloadInto(f *os.File, dl *activeDownload, url string, background bool) error {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "audio/*")
	req.Header.Set("Accept-Encoding", "identity")
	req.Header.Set("User-Agent", "gobz-connect/1.0")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("HTTP %s", resp.Status)
	}

	var offset int64
	var unflushed int64
	start := time.Now()
	buf := make([]byte, 32*1024)
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := f.Write(buf[:n]); werr != nil {
				return werr
			}
			offset += int64(n)
			unflushed += int64(n)
			if unflushed >= fadviseFlushBytes {
				releasePageCache(f, offset-unflushed, unflushed)
				unflushed = 0
			}
			dl.addWritten(int64(n))

			if background && tc.bgRateBytesPerSec > 0 {
				// Pace writes so they don't saturate the SD card.
				// wantNs = how long offset bytes should have taken at the cap.
				wantNs := offset * int64(time.Second) / tc.bgRateBytesPerSec
				if delay := time.Duration(wantNs) - time.Since(start); delay > 0 {
					time.Sleep(delay)
				}
			}
		}
		if err == io.EOF {
			if unflushed > 0 {
				releasePageCache(f, offset-unflushed, unflushed)
			}
			return nil
		}
		if err != nil {
			return err
		}
	}
}

func (tc *TrackCache) evictLoop() {
	ticker := time.NewTicker(60 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		if err := tc.evict(); err != nil {
			log.Printf("cache: evict: %v", err)
		}
	}
}

func (tc *TrackCache) evict() error {
	entries, err := os.ReadDir(tc.dir)
	if err != nil {
		return err
	}

	type fileEntry struct {
		path  string
		size  int64
		mtime time.Time
	}

	var files []fileEntry
	var total int64

	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if filepath.Ext(name) == ".tmp" {
			continue // skip stale temp files from previous runs
		}
		tc.mu.Lock()
		_, active := tc.active[name]
		tc.mu.Unlock()
		if active {
			continue // skip files currently being downloaded
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		p := filepath.Join(tc.dir, name)
		files = append(files, fileEntry{p, info.Size(), info.ModTime()})
		total += info.Size()
	}

	if total <= tc.maxBytes {
		return nil
	}

	// Evict oldest first (LRU).
	sort.Slice(files, func(i, j int) bool {
		return files[i].mtime.Before(files[j].mtime)
	})

	for _, f := range files {
		if total <= tc.maxBytes {
			break
		}
		if err := os.Remove(f.path); err == nil {
			log.Printf("cache: evicted %s (%d MB)", filepath.Base(f.path), f.size>>20)
			total -= f.size
		}
	}
	return nil
}
