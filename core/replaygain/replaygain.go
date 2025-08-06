package replaygain

import (
	"context"
	"fmt"
	"math"

	"github.com/navidrome/navidrome/core/ffmpeg"
	"github.com/navidrome/navidrome/log"
	"github.com/navidrome/navidrome/model"
)

// Service handles ReplayGain calculation using FFmpeg EBU R128 analysis
type Service interface {
	CalculateTrackGain(ctx context.Context, mf *model.MediaFile) error
	CalculateAlbumGain(ctx context.Context, album *model.Album, tracks model.MediaFiles) error
}

type service struct {
	ffmpeg ffmpeg.FFmpeg
	ds     model.DataStore
}

// NewService creates a new ReplayGain service
func NewService(ds model.DataStore) Service {
	return &service{
		ffmpeg: ffmpeg.New(),
		ds:     ds,
	}
}

const (
	// Target loudness for ReplayGain calculation (-18 LUFS)
	targetLoudness = -18.0
)

// CalculateTrackGain calculates ReplayGain for a single track using EBU R128
func (s *service) CalculateTrackGain(ctx context.Context, mf *model.MediaFile) error {
	// Skip if track already has ReplayGain information
	if mf.RGTrackGain != nil && mf.RGTrackPeak != nil {
		log.Trace(ctx, "Track already has ReplayGain info, skipping", "track", mf.Path)
		return nil
	}

	// Check if the track already has ReplayGain in the database
	// This handles the case where file metadata doesn't have ReplayGain but database does
	dbTrack, err := s.ds.MediaFile(ctx).Get(mf.ID)
	if err == nil && dbTrack.RGTrackGain != nil && dbTrack.RGTrackPeak != nil {
		log.Trace(ctx, "Track already has ReplayGain in database, updating current object", "track", mf.Path)
		mf.RGTrackGain = dbTrack.RGTrackGain
		mf.RGTrackPeak = dbTrack.RGTrackPeak
		return nil
	}

	// Check if FFmpeg is available
	if !s.ffmpeg.IsAvailable() {
		return fmt.Errorf("FFmpeg not available for ReplayGain calculation")
	}

	results, err := s.ffmpeg.AnalyzeR128(ctx, []string{mf.AbsolutePath()})
	if err != nil {
		return fmt.Errorf("R128 analysis failed for %s: %w", mf.Path, err)
	}

	analysis, ok := results[mf.AbsolutePath()]
	if !ok {
		return fmt.Errorf("no R128 analysis result for %s", mf.Path)
	}

	// Calculate ReplayGain values
	trackGain := targetLoudness - analysis.IntegratedLoudness
	trackPeak := dbfsToLinear(analysis.TruePeak)

	// Update the MediaFile
	mf.RGTrackGain = &trackGain
	mf.RGTrackPeak = &trackPeak

	// Save to database
	err = s.ds.MediaFile(ctx).Put(mf)
	if err != nil {
		return fmt.Errorf("failed to save ReplayGain data for %s: %w", mf.Path, err)
	}

	log.Info(ctx, "Calculated ReplayGain for track",
		"track", mf.Path,
		"gain", fmt.Sprintf("%.2f dB", trackGain),
		"peak", fmt.Sprintf("%.6f", trackPeak),
		"loudness", fmt.Sprintf("%.1f LUFS", analysis.IntegratedLoudness))

	return nil
}

// CalculateAlbumGain calculates album-level ReplayGain for a collection of tracks
func (s *service) CalculateAlbumGain(ctx context.Context, album *model.Album, tracks model.MediaFiles) error {
	// First, ensure all tracks have their track-level ReplayGain
	for i := range tracks {
		if err := s.CalculateTrackGain(ctx, &tracks[i]); err != nil {
			log.Warn(ctx, "Failed to calculate track ReplayGain for album processing", "track", tracks[i].Path, err)
		}
	}

	// Check if all tracks have consistent album ReplayGain
	if len(tracks) > 0 {
		firstAlbumGain := tracks[0].RGAlbumGain
		firstAlbumPeak := tracks[0].RGAlbumPeak

		if firstAlbumGain != nil && firstAlbumPeak != nil {
			allConsistent := true
			for _, track := range tracks[1:] {
				if track.RGAlbumGain == nil || track.RGAlbumPeak == nil ||
					abs(*track.RGAlbumGain-*firstAlbumGain) > 0.001 ||
					abs(*track.RGAlbumPeak-*firstAlbumPeak) > 0.000001 {
					allConsistent = false
					break
				}
			}

			if allConsistent {
				log.Trace(ctx, "Album already has consistent ReplayGain for all tracks, skipping", "album", album.Name)
				return nil
			}
		}
	}

	// Check if FFmpeg is available
	if !s.ffmpeg.IsAvailable() {
		return fmt.Errorf("FFmpeg not available for ReplayGain calculation")
	}

	log.Debug(ctx, "Calculating album ReplayGain", "album", album.Name, "tracks", len(tracks))

	// Collect file paths
	filePaths := make([]string, len(tracks))
	for i, track := range tracks {
		filePaths[i] = track.AbsolutePath()
	}

	// Calculate album loudness using concatenated analysis (following regainer.py logic)
	albumLoudness, err := s.calculateAlbumLoudness(ctx, filePaths)
	if err != nil {
		return fmt.Errorf("failed to calculate album loudness for %s: %w", album.Name, err)
	}

	// Calculate album peak as the maximum of all track peaks (following regainer.py logic)
	var albumPeak float64
	for _, track := range tracks {
		if track.RGTrackPeak != nil && *track.RGTrackPeak > albumPeak {
			albumPeak = *track.RGTrackPeak
		}
	}

	albumGain := targetLoudness - albumLoudness

	// Update all tracks with album ReplayGain
	for i := range tracks {
		tracks[i].RGAlbumGain = &albumGain
		tracks[i].RGAlbumPeak = &albumPeak

		err = s.ds.MediaFile(ctx).Put(&tracks[i])
		if err != nil {
			log.Error(ctx, "Failed to save album ReplayGain data", "track", tracks[i].Path, err)
			continue
		}
	}

	log.Info(ctx, "Calculated album ReplayGain",
		"album", album.Name,
		"tracks", len(tracks),
		"gain", fmt.Sprintf("%.2f dB", albumGain),
		"peak", fmt.Sprintf("%.6f", albumPeak),
		"loudness", fmt.Sprintf("%.1f LUFS", albumLoudness))

	return nil
}

// calculateAlbumLoudness calculates the integrated loudness across all album tracks
func (s *service) calculateAlbumLoudness(ctx context.Context, filePaths []string) (float64, error) {
	// For proper album ReplayGain, we need to analyze all files together as one continuous stream
	// This gives us the true integrated loudness across the entire album

	log.Debug(ctx, "Analyzing album loudness by concatenating files", "fileCount", len(filePaths))

	// Try to use the concat method for proper album analysis
	analysis, err := s.ffmpeg.AnalyzeR128Concat(ctx, filePaths)
	if err != nil {
		// Fallback to the simple average method if concat fails
		log.Warn(ctx, "Failed to analyze concatenated album, falling back to simple average", err)
		return s.calculateAlbumLoudnessFallback(ctx, filePaths)
	}

	// The concatenated analysis gives us the true album loudness
	return analysis.IntegratedLoudness, nil
}

// calculateAlbumLoudnessFallback provides a fallback method using simple average
func (s *service) calculateAlbumLoudnessFallback(ctx context.Context, filePaths []string) (float64, error) {
	// Analyze each file individually and calculate simple average
	results, err := s.ffmpeg.AnalyzeR128(ctx, filePaths)
	if err != nil {
		return 0, fmt.Errorf("failed to analyze album files: %w", err)
	}

	var totalLoudness float64
	var validFiles int

	for _, filePath := range filePaths {
		analysis, ok := results[filePath]
		if !ok {
			log.Warn(ctx, "No analysis result for file", "file", filePath)
			continue
		}

		totalLoudness += analysis.IntegratedLoudness
		validFiles++
	}

	if validFiles == 0 {
		return 0, fmt.Errorf("no valid files for album loudness calculation")
	}

	// Simple arithmetic average of LUFS values
	avgLoudness := totalLoudness / float64(validFiles)

	log.Debug(ctx, "Album loudness fallback calculation", "avgLoudness", avgLoudness, "fileCount", validFiles)

	return avgLoudness, nil
}

// dbfsToLinear converts dBFS to linear scale
func dbfsToLinear(dbfs float64) float64 {
	return math.Pow(10, dbfs/20)
}

// abs returns the absolute value of a float64
func abs(x float64) float64 {
	if x < 0 {
		return -x
	}
	return x
}
