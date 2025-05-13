package recording

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"sync"

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
		return "", fmt.Errorf("no recordings to merge")
	}

	// Group files by ChannelType (left/right)
	leftFiles := make([]TrackFile, 0)
	rightFiles := make([]TrackFile, 0)

	for clientID, metadataList := range m.records {
		for _, metadata := range metadataList {
			filePath := filepath.Join(m.config.BasePath, m.config.RecordingID, clientID, metadata.TrackID+".ogg")

			if _, err := os.Stat(filePath); os.IsNotExist(err) {
				m.logger.Warnf("File not found: %s", filePath)
				continue
			}

			trackFile := TrackFile{
				Metadata: metadata,
				FilePath: filePath,
			}

			if metadata.ChannelType == int(ChannelTypeLeft) {
				leftFiles = append(leftFiles, trackFile)
			} else if metadata.ChannelType == int(ChannelTypeRight) {
				rightFiles = append(rightFiles, trackFile)
			}
		}
	}

	// Sort files by start time
	sort.Slice(leftFiles, func(i, j int) bool {
		return leftFiles[i].Metadata.StartTime.Before(leftFiles[j].Metadata.StartTime)
	})

	sort.Slice(rightFiles, func(i, j int) bool {
		return rightFiles[i].Metadata.StartTime.Before(rightFiles[j].Metadata.StartTime)
	})

	// Ensure output directory exists
	outputDir := filepath.Dir(m.config.OutputPath)
	if err := os.MkdirAll(outputDir, 0755); err != nil {
		return "", fmt.Errorf("failed to create output directory: %w", err)
	}

	// Generate FFmpeg command to merge files
	ffmpegCmd, err := m.generateFFmpegCommand(leftFiles, rightFiles, m.config.OutputPath)
	if err != nil {
		return "", fmt.Errorf("failed to generate FFmpeg command: %w", err)
	}

	// Execute FFmpeg command to merge files
	if err := executeFFmpegCommand(ctx, ffmpegCmd); err != nil {
		return "", fmt.Errorf("failed to merge files: %w", err)
	}

	return m.config.OutputPath, nil
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
		for i, file := range leftFiles {
			delay := file.Metadata.StartTime.Sub(earliestStart).Seconds()
			if delay < 0 {
				delay = 0
			}

			if i == 0 {
				filterComplex += fmt.Sprintf("[0:a]adelay=%d|%d[l0];", int(delay*1000), int(delay*1000))
			} else {
				filterComplex += fmt.Sprintf("[%d:a]adelay=%d|%d[l%d];", i, int(delay*1000), int(delay*1000), i)
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
		for i, file := range rightFiles {
			delay := file.Metadata.StartTime.Sub(earliestStart).Seconds()
			if delay < 0 {
				delay = 0
			}

			offset := len(leftFiles) + i
			if i == 0 {
				filterComplex += fmt.Sprintf("[%d:a]adelay=%d|%d[r0];", offset, int(delay*1000), int(delay*1000))
			} else {
				filterComplex += fmt.Sprintf("[%d:a]adelay=%d|%d[r%d];", offset, int(delay*1000), int(delay*1000), i)
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

	return cmd, nil
}

// executeFFmpegCommand runs the FFmpeg command to merge files
func executeFFmpegCommand(ctx context.Context, cmd []string) error {
	// Execute FFmpeg command using os/exec
	execCmd := exec.CommandContext(ctx, cmd[0], cmd[1:]...)
	execCmd.Stdout = os.Stdout
	execCmd.Stderr = os.Stderr

	return execCmd.Run()
}
