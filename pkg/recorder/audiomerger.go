package recorder

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// AudioMerger is responsible for merging audio files
type AudioMerger struct {
	ffmpegPath string
}

// NewAudioMerger creates a new audio merger
func NewAudioMerger(ffmpegPath string) *AudioMerger {
	// If ffmpeg path is not provided, assume it's in PATH
	if ffmpegPath == "" {
		ffmpegPath = "ffmpeg"
	}

	return &AudioMerger{
		ffmpegPath: ffmpegPath,
	}
}

// MergeParticipantRecordings merges all participant recordings in a room
// into a single WAV file using FFmpeg
func (m *AudioMerger) MergeParticipantRecordings(roomPath string, outputFile string) error {
	// Check if the room directory exists
	if _, err := os.Stat(roomPath); os.IsNotExist(err) {
		return fmt.Errorf("room directory does not exist: %s", roomPath)
	}

	// Find all participant directories
	dirs, err := os.ReadDir(roomPath)
	if err != nil {
		return fmt.Errorf("failed to read room directory: %w", err)
	}

	var participantDirs []string
	for _, dir := range dirs {
		if dir.IsDir() {
			// Check if this directory has a track.ogg file
			oggPath := filepath.Join(roomPath, dir.Name(), "track.ogg")
			if _, err := os.Stat(oggPath); err == nil {
				participantDirs = append(participantDirs, dir.Name())
			}
		}
	}

	if len(participantDirs) == 0 {
		return fmt.Errorf("no participant recordings found in room: %s", roomPath)
	}

	// Create a list of input files for FFmpeg
	var inputFiles []string
	var filterGraph []string

	for i, dir := range participantDirs {
		oggPath := filepath.Join(roomPath, dir, "track.ogg")
		inputFiles = append(inputFiles, "-i", oggPath)
		filterGraph = append(filterGraph, fmt.Sprintf("[%d:a]", i))
	}

	// Build the filter graph for mixing
	filterStr := strings.Join(filterGraph, "") + fmt.Sprintf("amix=inputs=%d:duration=longest", len(participantDirs))

	// Construct the FFmpeg command
	args := append([]string{"-y"}, inputFiles...)
	args = append(args, "-filter_complex", filterStr, "-c:a", "pcm_s16le", outputFile)

	// Execute FFmpeg
	cmd := exec.Command(m.ffmpegPath, args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("ffmpeg error: %w, output: %s", err, string(output))
	}

	return nil
}

// ConvertOggToWav converts a single OGG file to WAV format
func (m *AudioMerger) ConvertOggToWav(inputFile string, outputFile string) error {
	cmd := exec.Command(m.ffmpegPath, "-y", "-i", inputFile, "-c:a", "pcm_s16le", outputFile)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("ffmpeg error: %w, output: %s", err, string(output))
	}

	return nil
}
