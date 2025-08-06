package replaygain

import (
	"context"
	"testing"
	"time"

	"github.com/navidrome/navidrome/conf"
	"github.com/navidrome/navidrome/model"
	"github.com/navidrome/navidrome/tests"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestReplayGainQueue(t *testing.T) {
	tests.Init(t, true)
	RegisterFailHandler(Fail)
	RunSpecs(t, "ReplayGain Queue Suite")
}

var _ = Describe("AsyncQueue", func() {
	var ds model.DataStore
	var queue AsyncQueue
	var ctx context.Context

	BeforeEach(func() {
		ds = &tests.MockDataStore{}
		ctx = context.Background()
		
		// Enable ReplayGain calculation for tests
		conf.Server.Scanner.CalculateReplayGain = true
		conf.Server.Scanner.ReplayGainMaxWorkers = 2
		
		queue = NewAsyncQueue(ds)
	})

	AfterEach(func() {
		if queue != nil {
			queue.Stop()
		}
	})

	Describe("NewAsyncQueue", func() {
		It("creates a new queue with default settings", func() {
			Expect(queue).ToNot(BeNil())
			stats := queue.Stats()
			Expect(stats.PendingTracks).To(Equal(int64(0)))
			Expect(stats.PendingAlbums).To(Equal(int64(0)))
		})
	})

	Describe("EnqueueTrack", func() {
		It("enqueues a track without ReplayGain", func() {
			track := &model.MediaFile{
				ID:   "track1",
				Path: "/music/track1.mp3",
			}
			
			queue.EnqueueTrack(track)
			
			stats := queue.Stats()
			Expect(stats.PendingTracks).To(Equal(int64(1)))
		})

		It("skips tracks that already have ReplayGain", func() {
			gain := -10.5
			peak := 0.8
			track := &model.MediaFile{
				ID:          "track1",
				Path:        "/music/track1.mp3",
				RGTrackGain: &gain,
				RGTrackPeak: &peak,
			}
			
			queue.EnqueueTrack(track)
			
			stats := queue.Stats()
			Expect(stats.PendingTracks).To(Equal(int64(0)))
		})

		It("skips enqueuing when ReplayGain is disabled", func() {
			conf.Server.Scanner.CalculateReplayGain = false
			track := &model.MediaFile{
				ID:   "track1",
				Path: "/music/track1.mp3",
			}
			
			queue.EnqueueTrack(track)
			
			stats := queue.Stats()
			Expect(stats.PendingTracks).To(Equal(int64(0)))
		})
	})

	Describe("EnqueueAlbum", func() {
		It("enqueues an album without ReplayGain", func() {
			album := &model.Album{
				ID:   "album1",
				Name: "Test Album",
			}
			tracks := model.MediaFiles{
				{ID: "track1", Path: "/music/track1.mp3"},
				{ID: "track2", Path: "/music/track2.mp3"},
			}
			
			queue.EnqueueAlbum(album, tracks)
			
			stats := queue.Stats()
			Expect(stats.PendingAlbums).To(Equal(int64(1)))
		})

		It("skips albums with consistent ReplayGain", func() {
			gain := -10.5
			peak := 0.8
			album := &model.Album{
				ID:   "album1",
				Name: "Test Album",
			}
			tracks := model.MediaFiles{
				{ID: "track1", Path: "/music/track1.mp3", RGAlbumGain: &gain, RGAlbumPeak: &peak},
				{ID: "track2", Path: "/music/track2.mp3", RGAlbumGain: &gain, RGAlbumPeak: &peak},
			}
			
			queue.EnqueueAlbum(album, tracks)
			
			stats := queue.Stats()
			Expect(stats.PendingAlbums).To(Equal(int64(0)))
		})
	})

	Describe("Start and Stop", func() {
		It("starts and stops the queue gracefully", func() {
			queue.Start(ctx)
			
			// Give it a moment to start
			time.Sleep(10 * time.Millisecond)
			
			queue.Stop()
			
			// Should complete without hanging
			Expect(true).To(BeTrue())
		})
	})

	Describe("Stats", func() {
		It("returns accurate statistics", func() {
			track := &model.MediaFile{
				ID:   "track1",
				Path: "/music/track1.mp3",
			}
			album := &model.Album{
				ID:   "album1",
				Name: "Test Album",
			}
			tracks := model.MediaFiles{
				{ID: "track1", Path: "/music/track1.mp3"},
			}
			
			queue.EnqueueTrack(track)
			queue.EnqueueAlbum(album, tracks)
			
			stats := queue.Stats()
			Expect(stats.PendingTracks).To(Equal(int64(1)))
			Expect(stats.PendingAlbums).To(Equal(int64(1)))
			Expect(stats.ProcessedTracks).To(Equal(int64(0)))
			Expect(stats.ProcessedAlbums).To(Equal(int64(0)))
		})
	})
})