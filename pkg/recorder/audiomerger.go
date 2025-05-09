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

	// Find all audio files in participant directories
	var audioFiles []string

	// Find all participant directories
	participantDirs, err := os.ReadDir(roomPath)
	if err != nil {
		return fmt.Errorf("failed to read room directory: %w", err)
	}

	for _, dir := range participantDirs {
		if dir.IsDir() {
			// Read each participant directory and find all track_*.ogg files
			participantPath := filepath.Join(roomPath, dir.Name())
			files, err := os.ReadDir(participantPath)
			if err != nil {
				fmt.Printf("Warning: Failed to read participant directory %s: %v\n", participantPath, err)
				continue
			}

			for _, file := range files {
				if !file.IsDir() && strings.HasPrefix(file.Name(), "track_") && strings.HasSuffix(file.Name(), ".ogg") {
					audioPath := filepath.Join(participantPath, file.Name())
					audioFiles = append(audioFiles, audioPath)
				}
			}
		}
	}

	if len(audioFiles) == 0 {
		return fmt.Errorf("no participant recordings found in room: %s", roomPath)
	}

	// Create a list of input files for FFmpeg
	var inputFiles []string
	var filterGraph []string

	for i, filePath := range audioFiles {
		inputFiles = append(inputFiles, "-i", filePath)
		filterGraph = append(filterGraph, fmt.Sprintf("[%d:a]", i))
	}

	// Build the filter graph for mixing
	filterStr := strings.Join(filterGraph, "") + fmt.Sprintf("amix=inputs=%d:duration=longest", len(audioFiles))

	// Construct the FFmpeg command
	args := append([]string{"-y"}, inputFiles...)
	args = append(args, "-filter_complex", filterStr, "-ac", "1", "-ar", "48000", outputFile)

	cmd := exec.Command(m.ffmpegPath, args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("failed to merge audio: %s\n%w", string(output), err)
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
