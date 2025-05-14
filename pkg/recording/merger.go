package recording

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/pion/logging"
)

// ChannelType represents the stereo channel position
type ChannelType int

const (
	// ChannelTypeNoRecord indicates participant should not be recorded
	ChannelTypeNoRecord ChannelType = 0
	// ChannelTypeLeft indicates participant should be in left channel
	ChannelTypeLeft ChannelType = 1
	// ChannelTypeRight indicates participant should be in right channel
	ChannelTypeRight ChannelType = 2
)

// MergerConfig contains configuration for merging audio files
type MergerConfig struct {
	RecordingID      string
	BasePath         string
	OutputPath       string
	OutputSampleRate uint32
	Logger           logging.LeveledLogger
}

// Merger handles combining multiple audio files into one
type Merger struct {
	config      MergerConfig
	recordsLock sync.RWMutex
	records     map[string][]TrackMetadata
	logger      logging.LeveledLogger
}

// NewMerger creates a new audio merger
func NewMerger(ctx context.Context, config MergerConfig) *Merger {
	return &Merger{
		config:  config,
		records: make(map[string][]TrackMetadata),
		logger:  config.Logger,
	}
}

// AddRecording adds a recording to be merged
func (m *Merger) AddRecording(metadata TrackMetadata, filePath string) {
	m.recordsLock.Lock()
	defer m.recordsLock.Unlock()

	m.records[metadata.ClientID] = append(m.records[metadata.ClientID], metadata)
	m.logger.Infof("Added recording for client %s, track %s", metadata.ClientID, metadata.TrackID)
}

// MergeAll merges all recordings into a single stereo file
func (m *Merger) MergeAll(ctx context.Context) (string, error) {
	m.recordsLock.RLock()
	defer m.recordsLock.RUnlock()

	if len(m.records) == 0 {
		m.logger.Warnf("No recordings to merge")
		return "", fmt.Errorf("no recordings to merge")
	}

	m.logger.Infof("Merging recordings from %d clients", len(m.records))

	// Group files by ChannelType (left/right)
	leftFiles := make([]TrackFile, 0)
	rightFiles := make([]TrackFile, 0)

	fileCount := 0
	missingCount := 0
	for clientID, metadataList := range m.records {
		clientFileCount := 0
		m.logger.Infof("Processing client %s with %d tracks", clientID, len(metadataList))

		for _, metadata := range metadataList {
			// Construct file path from client ID and track ID
			filePath := filepath.Join(m.config.BasePath, m.config.RecordingID, clientID, metadata.TrackID+".ogg")

			// Check if file exists
			if _, err := os.Stat(filePath); os.IsNotExist(err) {
				m.logger.Warnf("Recording file does not exist: %s", filePath)
				missingCount++
				continue
			}

			// Add to appropriate channel list based on ChannelType
			switch metadata.ChannelType {
			case 1: // Left channel
				m.logger.Infof("Adding file %s to left channel", filePath)
				leftFiles = append(leftFiles, TrackFile{
					FilePath: filePath,
					Metadata: metadata,
				})
				clientFileCount++
			case 2: // Right channel
				m.logger.Infof("Adding file %s to right channel", filePath)
				rightFiles = append(rightFiles, TrackFile{
					FilePath: filePath,
					Metadata: metadata,
				})
				clientFileCount++
			default:
				m.logger.Warnf("Unsupported channel type %d for file %s",
					metadata.ChannelType, filePath)
			}
			fileCount++
		}

		m.logger.Infof("Added %d files from client %s", clientFileCount, clientID)
	}

	m.logger.Infof("Grouped %d files for merging (%d left, %d right), %d files missing",
		fileCount, len(leftFiles), len(rightFiles), missingCount)

	if len(leftFiles) == 0 && len(rightFiles) == 0 {
		m.logger.Errorf("No valid files to merge")
		return "", fmt.Errorf("no valid files to merge")
	}

	// Create merged file
	outputPath := m.config.OutputPath
	os.MkdirAll(filepath.Dir(outputPath), 0755)

	// Generate and execute FFmpeg command
	cmd, err := m.generateFFmpegCommand(leftFiles, rightFiles, outputPath)
	if err != nil {
		m.logger.Errorf("Failed to generate FFmpeg command: %v", err)
		return "", fmt.Errorf("failed to generate FFmpeg command: %w", err)
	}

	// Execute the command
	m.logger.Infof("Executing FFmpeg command: %s", strings.Join(cmd, " "))
	err = executeFFmpegCommand(ctx, cmd)
	if err != nil {
		m.logger.Errorf("Failed to execute FFmpeg command: %v", err)
		return "", fmt.Errorf("failed to execute FFmpeg command: %w", err)
	}

	m.logger.Infof("Successfully merged recordings to %s", outputPath)

	// Save metadata for the merged file
	type MergedMetadata struct {
		LeftTracks  []TrackMetadata `json:"leftTracks"`
		RightTracks []TrackMetadata `json:"rightTracks"`
		MergedAt    time.Time       `json:"mergedAt"`
		OutputPath  string          `json:"outputPath"`
	}

	mergedMeta := MergedMetadata{
		MergedAt:   time.Now(),
		OutputPath: outputPath,
	}

	for _, file := range leftFiles {
		mergedMeta.LeftTracks = append(mergedMeta.LeftTracks, file.Metadata)
	}

	for _, file := range rightFiles {
		mergedMeta.RightTracks = append(mergedMeta.RightTracks, file.Metadata)
	}

	metadataJSON, err := json.Marshal(mergedMeta)
	if err != nil {
		m.logger.Warnf("Failed to marshal merged metadata: %v", err)
	} else {
		metadataPath := outputPath + ".json"
		err = os.WriteFile(metadataPath, metadataJSON, 0644)
		if err != nil {
			m.logger.Warnf("Failed to write merged metadata: %v", err)
		} else {
			m.logger.Infof("Saved merged metadata to %s", metadataPath)
		}
	}

	return outputPath, nil
}

type TrackFile struct {
	Metadata TrackMetadata
	FilePath string
}

// generateFFmpegCommand creates an FFmpeg command to merge files
func (m *Merger) generateFFmpegCommand(leftFiles, rightFiles []TrackFile, outputPath string) ([]string, error) {
	if len(leftFiles) == 0 && len(rightFiles) == 0 {
		return nil, fmt.Errorf("no files to merge")
	}

	m.logger.Infof("Generating FFmpeg command with %d left channel files and %d right channel files",
		len(leftFiles), len(rightFiles))

	for i, file := range leftFiles {
		m.logger.Infof("Left channel file %d: %s (client: %s, track: %s, start: %s)",
			i, file.FilePath, file.Metadata.ClientID, file.Metadata.TrackID, file.Metadata.StartTime)
	}

	for i, file := range rightFiles {
		m.logger.Infof("Right channel file %d: %s (client: %s, track: %s, start: %s)",
			i, file.FilePath, file.Metadata.ClientID, file.Metadata.TrackID, file.Metadata.StartTime)
	}

	// Create a complex FFmpeg command that:
	// 1. Aligns audio files based on timestamps
	// 2. Mixes multiple files per channel if needed
	// 3. Creates a stereo output with left and right channels

	cmd := []string{
		"ffmpeg",
		"-y", // Overwrite output file if it exists
	}

	// Add input files
	for _, file := range leftFiles {
		cmd = append(cmd, "-i", file.FilePath)
	}

	for _, file := range rightFiles {
		cmd = append(cmd, "-i", file.FilePath)
	}

	// Create filter complex for alignment and mixing
	filterComplex := ""

	// Handle left channel
	if len(leftFiles) > 0 {
		// Calculate time offsets relative to the earliest file
		earliestStart := leftFiles[0].Metadata.StartTime
		for _, file := range leftFiles {
			if file.Metadata.StartTime.Before(earliestStart) {
				earliestStart = file.Metadata.StartTime
			}
		}

		m.logger.Infof("Earliest start time for left channel: %s", earliestStart)

		for i, file := range leftFiles {
			delay := file.Metadata.StartTime.Sub(earliestStart).Milliseconds()
			if delay < 0 {
				delay = 0
			}

			m.logger.Infof("Left file %d delay: %d ms", i, delay)

			if i == 0 {
				filterComplex += fmt.Sprintf("[0:a]adelay=%d|%d[l0];", delay, delay)
			} else {
				filterComplex += fmt.Sprintf("[%d:a]adelay=%d|%d[l%d];", i, delay, delay, i)
			}
		}

		// Mix all left channels if there are multiple
		if len(leftFiles) > 1 {
			filterComplex += "["
			for i := 0; i < len(leftFiles); i++ {
				if i > 0 {
					filterComplex += "]["
				}
				filterComplex += fmt.Sprintf("l%d", i)
			}
			filterComplex += fmt.Sprintf("]amix=inputs=%d:normalize=0[left];", len(leftFiles))
		} else {
			filterComplex += "[l0]channelsplit=channel_layout=mono:channels=c0[left];"
		}
	} else {
		// If no left files, create silent left channel
		filterComplex += "anullsrc=r=48000:cl=mono[left];"
	}

	// Handle right channel
	if len(rightFiles) > 0 {
		// Calculate time offsets relative to the earliest file
		earliestStart := rightFiles[0].Metadata.StartTime
		for _, file := range rightFiles {
			if file.Metadata.StartTime.Before(earliestStart) {
				earliestStart = file.Metadata.StartTime
			}
		}

		m.logger.Infof("Earliest start time for right channel: %s", earliestStart)

		for i, file := range rightFiles {
			delay := file.Metadata.StartTime.Sub(earliestStart).Milliseconds()
			if delay < 0 {
				delay = 0
			}

			m.logger.Infof("Right file %d delay: %d ms", i, delay)

			offset := len(leftFiles) + i
			if i == 0 {
				filterComplex += fmt.Sprintf("[%d:a]adelay=%d|%d[r0];", offset, delay, delay)
			} else {
				filterComplex += fmt.Sprintf("[%d:a]adelay=%d|%d[r%d];", offset, delay, delay, i)
			}
		}

		// Mix all right channels if there are multiple
		if len(rightFiles) > 1 {
			filterComplex += "["
			for i := 0; i < len(rightFiles); i++ {
				if i > 0 {
					filterComplex += "]["
				}
				filterComplex += fmt.Sprintf("r%d", i)
			}
			filterComplex += fmt.Sprintf("]amix=inputs=%d:normalize=0[right];", len(rightFiles))
		} else {
			filterComplex += "[r0]channelsplit=channel_layout=mono:channels=c0[right];"
		}
	} else {
		// If no right files, create silent right channel
		filterComplex += "anullsrc=r=48000:cl=mono[right];"
	}

	// Merge left and right channels to stereo
	filterComplex += "[left][right]join=inputs=2:channel_layout=stereo[a]"

	cmd = append(cmd, "-filter_complex", filterComplex)
	cmd = append(cmd, "-map", "[a]")
	cmd = append(cmd, "-c:a", "libopus")
	cmd = append(cmd, "-b:a", "128k")
	cmd = append(cmd, outputPath)

	m.logger.Infof("Generated FFmpeg command with %d arguments", len(cmd))
	m.logger.Infof("FFmpeg filter complex: %s", filterComplex)

	return cmd, nil
}

// executeFFmpegCommand runs the FFmpeg command to merge files
func executeFFmpegCommand(ctx context.Context, cmd []string) error {
	// Print the command for debugging
	fmt.Printf("### RECORDING DEBUG: Executing FFmpeg command: %s\n", strings.Join(cmd, " "))

	// Execute FFmpeg command using os/exec
	execCmd := exec.CommandContext(ctx, cmd[0], cmd[1:]...)

	// Create pipes for stdout and stderr
	stdout, err := execCmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("failed to create stdout pipe: %w", err)
	}

	stderr, err := execCmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("failed to create stderr pipe: %w", err)
	}

	// Create a wait group to wait for the goroutines to finish
	var wg sync.WaitGroup
	wg.Add(2)

	// Start the command
	if err := execCmd.Start(); err != nil {
		return fmt.Errorf("failed to start FFmpeg command: %w", err)
	}

	// Read stdout
	go func() {
		defer wg.Done()
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			fmt.Printf("### FFMPEG STDOUT: %s\n", scanner.Text())
		}
	}()

	// Read stderr
	go func() {
		defer wg.Done()
		scanner := bufio.NewScanner(stderr)
		for scanner.Scan() {
			fmt.Printf("### FFMPEG STDERR: %s\n", scanner.Text())
		}
	}()

	// Wait for the command to finish
	err = execCmd.Wait()
	wg.Wait()

	if err != nil {
		fmt.Printf("### RECORDING DEBUG: FFmpeg command failed with error: %v\n", err)
		return fmt.Errorf("FFmpeg command failed: %w", err)
	}

	fmt.Printf("### RECORDING DEBUG: FFmpeg command completed successfully\n")
	return nil
}
