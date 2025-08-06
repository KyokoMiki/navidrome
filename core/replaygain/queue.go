package replaygain

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/navidrome/navidrome/conf"
	"github.com/navidrome/navidrome/log"
	"github.com/navidrome/navidrome/model"
	"golang.org/x/sync/semaphore"
)

// ProgressCallback is called when R128 analysis progress changes
type ProgressCallback func(analyzing bool, completed, total int64)

// AsyncQueue manages asynchronous ReplayGain calculation with concurrency control
type AsyncQueue interface {
	// EnqueueTrack adds a track to the ReplayGain calculation queue
	EnqueueTrack(track *model.MediaFile)
	// EnqueueAlbum adds an album to the ReplayGain calculation queue
	EnqueueAlbum(album *model.Album, tracks model.MediaFiles)
	// Start begins processing the queue
	Start(ctx context.Context)
	// Stop gracefully stops the queue processing
	Stop()
	// Stats returns current queue statistics
	Stats() QueueStats
	// SetProgressCallback sets a callback for progress updates
	SetProgressCallback(callback ProgressCallback)
}

// QueueStats provides information about the queue status
type QueueStats struct {
	PendingTracks   int64
	PendingAlbums   int64
	ProcessedTracks int64
	ProcessedAlbums int64
	ActiveWorkers   int64
	Errors          int64
	TotalTracks     int64
	TotalAlbums     int64
}

// TrackJob represents a track ReplayGain calculation job
type TrackJob struct {
	Track *model.MediaFile
	Retry int
}

// AlbumJob represents an album ReplayGain calculation job
type AlbumJob struct {
	Album  *model.Album
	Tracks model.MediaFiles
	Retry  int
}

type asyncQueue struct {
	service    Service
	trackQueue chan *TrackJob
	albumQueue chan *AlbumJob
	maxWorkers int64
	sem        *semaphore.Weighted
	wg         sync.WaitGroup
	ctx        context.Context
	cancel     context.CancelFunc

	// Progress callback
	progressCallback ProgressCallback
	progressMutex    sync.RWMutex

	// Statistics
	stats struct {
		pendingTracks   atomic.Int64
		pendingAlbums   atomic.Int64
		processedTracks atomic.Int64
		processedAlbums atomic.Int64
		activeWorkers   atomic.Int64
		errors          atomic.Int64
		totalTracks     atomic.Int64 // Fixed total count set at start
		totalAlbums     atomic.Int64 // Fixed total count set at start
	}
}

const (
	defaultMaxWorkers = 2
	maxRetries        = 3
	retryDelay        = 5 * time.Second
)

// NewAsyncQueue creates a new asynchronous ReplayGain calculation queue
func NewAsyncQueue(ds model.DataStore) AsyncQueue {
	maxWorkers := int64(defaultMaxWorkers)
	if conf.Server.Scanner.ReplayGainMaxWorkers > 0 {
		maxWorkers = int64(conf.Server.Scanner.ReplayGainMaxWorkers)
	}

	ctx, cancel := context.WithCancel(context.Background())

	return &asyncQueue{
		service:    NewService(ds),
		trackQueue: make(chan *TrackJob),
		albumQueue: make(chan *AlbumJob),
		maxWorkers: maxWorkers,
		sem:        semaphore.NewWeighted(maxWorkers),
		ctx:        ctx,
		cancel:     cancel,
	}
}

func (q *asyncQueue) EnqueueTrack(track *model.MediaFile) {
	// Skip if ReplayGain calculation is disabled
	if !conf.Server.Scanner.CalculateReplayGain {
		return
	}

	// Skip if track already has ReplayGain
	if track.RGTrackGain != nil && track.RGTrackPeak != nil {
		log.Trace(q.ctx, "Track already has ReplayGain, skipping queue", "track", track.Path)
		return
	}

	job := &TrackJob{
		Track: track,
		Retry: 0,
	}

	// Use goroutine to avoid blocking on unbuffered channel
	go func() {
		select {
		case q.trackQueue <- job:
			q.stats.pendingTracks.Add(1)
			q.stats.totalTracks.Add(1)
			log.Trace(q.ctx, "Track enqueued for ReplayGain calculation", "track", track.Path)
			q.notifyProgress()
		case <-q.ctx.Done():
			// Queue is shutting down
			return
		}
	}()
}

func (q *asyncQueue) EnqueueAlbum(album *model.Album, tracks model.MediaFiles) {
	// Skip if ReplayGain calculation is disabled
	if !conf.Server.Scanner.CalculateReplayGain {
		return
	}

	// Skip if album already has consistent ReplayGain
	if len(tracks) > 0 && q.hasConsistentAlbumReplayGain(tracks) {
		log.Trace(q.ctx, "Album already has consistent ReplayGain, skipping queue", "album", album.Name)
		return
	}

	job := &AlbumJob{
		Album:  album,
		Tracks: tracks,
		Retry:  0,
	}

	// Use goroutine to avoid blocking on unbuffered channel
	go func() {
		select {
		case q.albumQueue <- job:
			q.stats.pendingAlbums.Add(1)
			q.stats.totalAlbums.Add(1)
			log.Trace(q.ctx, "Album enqueued for ReplayGain calculation", "album", album.Name, "tracks", len(tracks))
			q.notifyProgress()
		case <-q.ctx.Done():
			// Queue is shutting down
			return
		}
	}()
}

func (q *asyncQueue) hasConsistentAlbumReplayGain(tracks model.MediaFiles) bool {
	if len(tracks) == 0 {
		return false
	}

	firstAlbumGain := tracks[0].RGAlbumGain
	firstAlbumPeak := tracks[0].RGAlbumPeak

	if firstAlbumGain == nil || firstAlbumPeak == nil {
		return false
	}

	for _, track := range tracks[1:] {
		if track.RGAlbumGain == nil || track.RGAlbumPeak == nil {
			return false
		}
		// Check for consistency (allow small floating point differences)
		if abs(*track.RGAlbumGain-*firstAlbumGain) > 0.001 ||
			abs(*track.RGAlbumPeak-*firstAlbumPeak) > 0.000001 {
			return false
		}
	}

	return true
}

func (q *asyncQueue) Start(ctx context.Context) {
	log.Info(ctx, "Starting ReplayGain async queue", "maxWorkers", q.maxWorkers)

	// Reset statistics at start
	q.stats.pendingTracks.Store(0)
	q.stats.pendingAlbums.Store(0)
	q.stats.processedTracks.Store(0)
	q.stats.processedAlbums.Store(0)
	q.stats.totalTracks.Store(0)
	q.stats.totalAlbums.Store(0)
	q.stats.errors.Store(0)

	// Start worker goroutines
	q.wg.Add(1)
	go q.processQueues(ctx)
}

func (q *asyncQueue) Stop() {
	log.Info(q.ctx, "Stopping ReplayGain async queue")
	q.cancel()
	q.wg.Wait()
	log.Info(q.ctx, "ReplayGain async queue stopped")
}

func (q *asyncQueue) Stats() QueueStats {
	return QueueStats{
		PendingTracks:   q.stats.pendingTracks.Load(),
		PendingAlbums:   q.stats.pendingAlbums.Load(),
		ProcessedTracks: q.stats.processedTracks.Load(),
		ProcessedAlbums: q.stats.processedAlbums.Load(),
		ActiveWorkers:   q.stats.activeWorkers.Load(),
		Errors:          q.stats.errors.Load(),
		TotalTracks:     q.stats.totalTracks.Load(),
		TotalAlbums:     q.stats.totalAlbums.Load(),
	}
}

func (q *asyncQueue) SetProgressCallback(callback ProgressCallback) {
	q.progressMutex.Lock()
	defer q.progressMutex.Unlock()
	q.progressCallback = callback
}

func (q *asyncQueue) notifyProgress() {
	q.progressMutex.RLock()
	callback := q.progressCallback
	q.progressMutex.RUnlock()

	if callback != nil {
		pending := q.stats.pendingTracks.Load() + q.stats.pendingAlbums.Load()
		completed := q.stats.processedTracks.Load() + q.stats.processedAlbums.Load()
		total := q.stats.totalTracks.Load() + q.stats.totalAlbums.Load()
		analyzing := pending > 0
		callback(analyzing, completed, total)
	}
}

func (q *asyncQueue) processQueues(ctx context.Context) {
	defer q.wg.Done()

	for {
		select {
		case <-ctx.Done():
			return
		case <-q.ctx.Done():
			return
		case trackJob := <-q.trackQueue:
			if trackJob != nil {
				q.stats.pendingTracks.Add(-1)
				q.processTrackJob(ctx, trackJob)
			}
		case albumJob := <-q.albumQueue:
			if albumJob != nil {
				q.stats.pendingAlbums.Add(-1)
				q.processAlbumJob(ctx, albumJob)
			}
		}
	}
}

func (q *asyncQueue) processTrackJob(ctx context.Context, job *TrackJob) {
	q.wg.Add(1)
	go func() {
		defer q.wg.Done()

		// Acquire semaphore to limit concurrent workers - THIS IS THE KEY FIX
		if err := q.sem.Acquire(ctx, 1); err != nil {
			if ctx.Err() == nil {
				log.Error(ctx, "Failed to acquire semaphore for track ReplayGain", err)
				q.stats.errors.Add(1)
			}
			return
		}
		defer q.sem.Release(1)

		q.stats.activeWorkers.Add(1)
		defer q.stats.activeWorkers.Add(-1)

		log.Debug(ctx, "Processing track ReplayGain", "track", job.Track.Path, "retry", job.Retry, "activeWorkers", q.stats.activeWorkers.Load())

		err := q.service.CalculateTrackGain(ctx, job.Track)
		if err != nil {
			log.Warn(ctx, "Failed to calculate track ReplayGain", "track", job.Track.Path, "retry", job.Retry, err)
			q.stats.errors.Add(1)

			// Retry logic
			if job.Retry < maxRetries {
				job.Retry++
				time.Sleep(retryDelay)

				select {
				case q.trackQueue <- job:
					q.stats.pendingTracks.Add(1)
					log.Debug(ctx, "Retrying track ReplayGain calculation", "track", job.Track.Path, "retry", job.Retry)
				case <-q.ctx.Done():
					log.Warn(ctx, "Failed to requeue track for retry, queue shutting down", "track", job.Track.Path)
				}
			}
		} else {
			q.stats.processedTracks.Add(1)
			log.Debug(ctx, "Successfully calculated track ReplayGain", "track", job.Track.Path)
			q.notifyProgress()
		}
	}()
}

func (q *asyncQueue) processAlbumJob(ctx context.Context, job *AlbumJob) {
	q.wg.Add(1)
	go func() {
		defer q.wg.Done()

		// Acquire semaphore to limit concurrent workers - THIS IS THE KEY FIX
		if err := q.sem.Acquire(ctx, 1); err != nil {
			if ctx.Err() == nil {
				log.Error(ctx, "Failed to acquire semaphore for album ReplayGain", err)
				q.stats.errors.Add(1)
			}
			return
		}
		defer q.sem.Release(1)

		q.stats.activeWorkers.Add(1)
		defer q.stats.activeWorkers.Add(-1)

		log.Debug(ctx, "Processing album ReplayGain", "album", job.Album.Name, "tracks", len(job.Tracks), "retry", job.Retry, "activeWorkers", q.stats.activeWorkers.Load())

		err := q.service.CalculateAlbumGain(ctx, job.Album, job.Tracks)
		if err != nil {
			log.Warn(ctx, "Failed to calculate album ReplayGain", "album", job.Album.Name, "retry", job.Retry, err)
			q.stats.errors.Add(1)

			// Retry logic
			if job.Retry < maxRetries {
				job.Retry++
				time.Sleep(retryDelay)

				select {
				case q.albumQueue <- job:
					q.stats.pendingAlbums.Add(1)
					log.Debug(ctx, "Retrying album ReplayGain calculation", "album", job.Album.Name, "retry", job.Retry)
				case <-q.ctx.Done():
					log.Warn(ctx, "Failed to requeue album for retry, queue shutting down", "album", job.Album.Name)
				}
			}
		} else {
			q.stats.processedAlbums.Add(1)
			log.Debug(ctx, "Successfully calculated album ReplayGain", "album", job.Album.Name)
			q.notifyProgress()
		}
	}()
}
