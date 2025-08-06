package ffmpeg

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"github.com/navidrome/navidrome/conf"
	"github.com/navidrome/navidrome/log"
)

type FFmpeg interface {
	Transcode(ctx context.Context, command, path string, maxBitRate, offset int) (io.ReadCloser, error)
	ExtractImage(ctx context.Context, path string) (io.ReadCloser, error)
	Probe(ctx context.Context, files []string) (string, error)
	AnalyzeR128(ctx context.Context, files []string) (map[string]R128Analysis, error)
	AnalyzeR128Concat(ctx context.Context, files []string) (R128Analysis, error)
	CmdPath() (string, error)
	IsAvailable() bool
	Version() string
}

// R128Analysis contains EBU R128 loudness analysis results
type R128Analysis struct {
	IntegratedLoudness float64 // LUFS
	LoudnessRange      float64 // LU
	TruePeak           float64 // dBTP
	FilePath           string
}

func New() FFmpeg {
	return &ffmpeg{}
}

const (
	extractImageCmd = "ffmpeg -i %s -map 0:v -map -0:V -vcodec copy -f image2pipe -"
	probeCmd        = "ffmpeg %s -f ffmetadata"
	r128Cmd         = "ffmpeg %s -af ebur128=framelog=verbose:peak=true -f null -"
)

type ffmpeg struct{}

func (e *ffmpeg) Transcode(ctx context.Context, command, path string, maxBitRate, offset int) (io.ReadCloser, error) {
	if _, err := ffmpegCmd(); err != nil {
		return nil, err
	}
	// First make sure the file exists
	if err := fileExists(path); err != nil {
		return nil, err
	}
	args := createFFmpegCommand(command, path, maxBitRate, offset)
	return e.start(ctx, args)
}

func (e *ffmpeg) ExtractImage(ctx context.Context, path string) (io.ReadCloser, error) {
	if _, err := ffmpegCmd(); err != nil {
		return nil, err
	}
	// First make sure the file exists
	if err := fileExists(path); err != nil {
		return nil, err
	}
	args := createFFmpegCommand(extractImageCmd, path, 0, 0)
	return e.start(ctx, args)
}

func fileExists(path string) error {
	s, err := os.Stat(path)
	if err != nil {
		return err
	}
	if s.IsDir() {
		return fmt.Errorf("'%s' is a directory", path)
	}
	return nil
}

func (e *ffmpeg) Probe(ctx context.Context, files []string) (string, error) {
	if _, err := ffmpegCmd(); err != nil {
		return "", err
	}
	args := createProbeCommand(probeCmd, files)
	log.Trace(ctx, "Executing ffmpeg command", "args", args)
	cmd := exec.CommandContext(ctx, args[0], args[1:]...) // #nosec
	output, _ := cmd.CombinedOutput()
	return string(output), nil
}

func (e *ffmpeg) AnalyzeR128(ctx context.Context, files []string) (map[string]R128Analysis, error) {
	if _, err := ffmpegCmd(); err != nil {
		return nil, err
	}

	results := make(map[string]R128Analysis)

	// Process files individually to get per-file analysis
	for _, file := range files {
		if err := fileExists(file); err != nil {
			log.Warn(ctx, "Skipping R128 analysis for non-existent file", "file", file, err)
			continue
		}

		args := createR128Command(r128Cmd, []string{file})
		log.Trace(ctx, "Executing R128 analysis", "file", file, "args", args)

		cmd := exec.CommandContext(ctx, args[0], args[1:]...) // #nosec
		output, err := cmd.CombinedOutput()
		if err != nil {
			log.Warn(ctx, "R128 analysis failed", "file", file, err)
			continue
		}

		outputStr := string(output)

		analysis, err := parseR128Output(outputStr, file)
		if err != nil {
			log.Warn(ctx, "Failed to parse R128 output", "file", file, err)
			continue
		}

		results[file] = analysis
	}

	return results, nil
}

func (e *ffmpeg) AnalyzeR128Concat(ctx context.Context, files []string) (R128Analysis, error) {
	if _, err := ffmpegCmd(); err != nil {
		return R128Analysis{}, err
	}

	if len(files) == 0 {
		return R128Analysis{}, fmt.Errorf("no files provided for analysis")
	}

	// For a single file, just use the regular analysis
	if len(files) == 1 {
		results, err := e.AnalyzeR128(ctx, files)
		if err != nil {
			return R128Analysis{}, err
		}
		if analysis, ok := results[files[0]]; ok {
			return analysis, nil
		}
		return R128Analysis{}, fmt.Errorf("no analysis result for file %s", files[0])
	}

	// For multiple files, we need to concatenate them and analyze as one stream
	// Build FFmpeg command to concatenate files and analyze
	args := []string{}
	cmd, _ := ffmpegCmd()
	args = append(args, cmd)

	// Add standard FFmpeg options
	args = append(args, "-nostats", "-nostdin", "-hide_banner", "-vn", "-loglevel", "info")

	// Add input files
	validFiles := 0
	for _, file := range files {
		if err := fileExists(file); err != nil {
			log.Warn(ctx, "Skipping non-existent file in concat analysis", "file", file, err)
			continue
		}
		args = append(args, "-i", file)
		validFiles++
	}

	if validFiles == 0 {
		return R128Analysis{}, fmt.Errorf("no valid files for concat analysis")
	}

	// Add concat filter and ebur128 analysis
	filterComplex := fmt.Sprintf("concat=n=%d:v=0:a=1,ebur128=framelog=verbose:peak=none[out]", validFiles)
	args = append(args, "-filter_complex", filterComplex, "-map", "[out]", "-f", "null", "-")

	log.Trace(ctx, "Executing R128 concat analysis", "files", files, "args", args)

	cmd2 := exec.CommandContext(ctx, args[0], args[1:]...) // #nosec
	output, err := cmd2.CombinedOutput()
	if err != nil {
		return R128Analysis{}, fmt.Errorf("R128 concat analysis failed: %w", err)
	}

	outputStr := string(output)

	analysis, err := parseR128Output(outputStr, "album_concat")
	if err != nil {
		return R128Analysis{}, fmt.Errorf("failed to parse R128 concat output: %w", err)
	}

	log.Debug(ctx, "R128 concat analysis completed", "fileCount", len(files), "loudness", analysis.IntegratedLoudness)

	return analysis, nil
}

func (e *ffmpeg) CmdPath() (string, error) {
	return ffmpegCmd()
}

func (e *ffmpeg) IsAvailable() bool {
	_, err := ffmpegCmd()
	return err == nil
}

// Version executes ffmpeg -version and extracts the version from the output.
// Sample output: ffmpeg version 6.0 Copyright (c) 2000-2023 the FFmpeg developers
func (e *ffmpeg) Version() string {
	cmd, err := ffmpegCmd()
	if err != nil {
		return "N/A"
	}
	out, err := exec.Command(cmd, "-version").CombinedOutput() // #nosec
	if err != nil {
		return "N/A"
	}
	parts := strings.Split(string(out), " ")
	if len(parts) < 3 {
		return "N/A"
	}
	return parts[2]
}

func (e *ffmpeg) start(ctx context.Context, args []string) (io.ReadCloser, error) {
	log.Trace(ctx, "Executing ffmpeg command", "cmd", args)
	j := &ffCmd{args: args}
	j.PipeReader, j.out = io.Pipe()
	err := j.start()
	if err != nil {
		return nil, err
	}
	go j.wait()
	return j, nil
}

type ffCmd struct {
	*io.PipeReader
	out  *io.PipeWriter
	args []string
	cmd  *exec.Cmd
}

func (j *ffCmd) start() error {
	cmd := exec.Command(j.args[0], j.args[1:]...) // #nosec
	cmd.Stdout = j.out
	if log.IsGreaterOrEqualTo(log.LevelTrace) {
		cmd.Stderr = os.Stderr
	} else {
		cmd.Stderr = io.Discard
	}
	j.cmd = cmd

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("starting cmd: %w", err)
	}
	return nil
}

func (j *ffCmd) wait() {
	if err := j.cmd.Wait(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			_ = j.out.CloseWithError(fmt.Errorf("%s exited with non-zero status code: %d", j.args[0], exitErr.ExitCode()))
		} else {
			_ = j.out.CloseWithError(fmt.Errorf("waiting %s cmd: %w", j.args[0], err))
		}
		return
	}
	_ = j.out.Close()
}

// Path will always be an absolute path
func createFFmpegCommand(cmd, path string, maxBitRate, offset int) []string {
	var args []string
	for _, s := range fixCmd(cmd) {
		if strings.Contains(s, "%s") {
			s = strings.ReplaceAll(s, "%s", path)
			args = append(args, s)
			if offset > 0 && !strings.Contains(cmd, "%t") {
				args = append(args, "-ss", strconv.Itoa(offset))
			}
		} else {
			s = strings.ReplaceAll(s, "%t", strconv.Itoa(offset))
			s = strings.ReplaceAll(s, "%b", strconv.Itoa(maxBitRate))
			args = append(args, s)
		}
	}
	return args
}

func createProbeCommand(cmd string, inputs []string) []string {
	var args []string
	for _, s := range fixCmd(cmd) {
		if s == "%s" {
			for _, inp := range inputs {
				args = append(args, "-i", inp)
			}
		} else {
			args = append(args, s)
		}
	}
	return args
}

func createR128Command(cmd string, inputs []string) []string {
	var args []string
	for _, s := range fixCmd(cmd) {
		if s == "%s" {
			for _, inp := range inputs {
				args = append(args, "-i", inp)
			}
		} else {
			args = append(args, s)
		}
	}
	return args
}

func fixCmd(cmd string) []string {
	split := strings.Fields(cmd)
	cmdPath, _ := ffmpegCmd()
	for i, s := range split {
		if s == "ffmpeg" || s == "ffmpeg.exe" {
			split[i] = cmdPath
		}
	}
	return split
}

func ffmpegCmd() (string, error) {
	ffOnce.Do(func() {
		if conf.Server.FFmpegPath != "" {
			ffmpegPath = conf.Server.FFmpegPath
			ffmpegPath, ffmpegErr = exec.LookPath(ffmpegPath)
		} else {
			ffmpegPath, ffmpegErr = exec.LookPath("ffmpeg")
			if errors.Is(ffmpegErr, exec.ErrDot) {
				log.Trace("ffmpeg found in current folder '.'")
				ffmpegPath, ffmpegErr = exec.LookPath("./ffmpeg")
			}
		}
		if ffmpegErr == nil {
			log.Info("Found ffmpeg", "path", ffmpegPath)
			return
		}
	})
	return ffmpegPath, ffmpegErr
}

// parseR128Output parses FFmpeg EBU R128 analysis output
func parseR128Output(output, filePath string) (R128Analysis, error) {
	analysis := R128Analysis{FilePath: filePath}

	// Parse integrated loudness: [Parsed_ebur128_0 @ 0x...] I: -23.0 LUFS
	// Look for the final summary line, not intermediate values
	integratedRegex := regexp.MustCompile(`I:\s*(-?\d+\.?\d*)\s*LUFS`)
	matches := integratedRegex.FindAllStringSubmatch(output, -1)
	if len(matches) > 0 {
		// Use the last match (final summary)
		lastMatch := matches[len(matches)-1]
		if len(lastMatch) > 1 {
			if val, err := strconv.ParseFloat(lastMatch[1], 64); err == nil {
				analysis.IntegratedLoudness = val
			}
		}
	}

	// Parse loudness range: [Parsed_ebur128_0 @ 0x...] LRA: 7.3 LU
	rangeRegex := regexp.MustCompile(`LRA:\s*(-?\d+\.?\d*)\s*LU`)
	rangeMatches := rangeRegex.FindAllStringSubmatch(output, -1)
	if len(rangeMatches) > 0 {
		lastMatch := rangeMatches[len(rangeMatches)-1]
		if len(lastMatch) > 1 {
			if val, err := strconv.ParseFloat(lastMatch[1], 64); err == nil {
				analysis.LoudnessRange = val
			}
		}
	}

	// Parse true peak: [Parsed_ebur128_0 @ 0x...] Peak: -1.2 dBFS
	peakRegex := regexp.MustCompile(`Peak:\s*(-?\d+\.?\d*)\s*dBFS`)
	peakMatches := peakRegex.FindAllStringSubmatch(output, -1)
	if len(peakMatches) > 0 {
		lastMatch := peakMatches[len(peakMatches)-1]
		if len(lastMatch) > 1 {
			if val, err := strconv.ParseFloat(lastMatch[1], 64); err == nil {
				analysis.TruePeak = val
			}
		}
	}

	// Validate that we got at least the integrated loudness
	if analysis.IntegratedLoudness == 0 {
		return analysis, fmt.Errorf("failed to parse integrated loudness from R128 output")
	}

	return analysis, nil
}

// These variables are accessible here for tests. Do not use them directly in production code. Use ffmpegCmd() instead.
var (
	ffOnce     sync.Once
	ffmpegPath string
	ffmpegErr  error
)
