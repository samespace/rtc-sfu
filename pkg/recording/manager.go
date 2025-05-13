package recording

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"

	"github.com/pion/logging"
	"github.com/pion/rtp"
)

// RecordingState represents the current state of a room recording
type RecordingState int

const (
	// RecordingStateStopped indicates the recording is stopped
	RecordingStateStopped RecordingState = iota
	// RecordingStateRecording indicates the recording is in progress
	RecordingStateRecording
	// RecordingStatePaused indicates the recording is paused
	RecordingStatePaused
)

// RecordingConfig contains configuration for the recording manager
type RecordingConfig struct {
	BasePath         string
	RecordingID      string
	SampleRate       uint32
	ChannelCount     uint8
	S3Config         *S3Config
	UploadToS3       bool
	UploadMergedOnly bool
}

// RecordingManager handles room recording operations
type RecordingManager struct {
	config        RecordingConfig
	recorders     map[string]map[string]Recorder // clientID -> trackID -> Recorder
	recordersLock sync.RWMutex
	state         RecordingState
	logger        logging.LeveledLogger
	context       context.Context
	cancelFunc    context.CancelFunc
	merger        *Merger
	uploader      *Uploader
	outputPath    string
}

// NewRecordingManager creates a new recording manager
func NewRecordingManager(ctx context.Context, config RecordingConfig, logger logging.LeveledLogger) (*RecordingManager, error) {
	recordingCtx, cancel := context.WithCancel(ctx)

	manager := &RecordingManager{
		config:     config,
		recorders:  make(map[string]map[string]Recorder),
		state:      RecordingStateStopped,
		logger:     logger,
		context:    recordingCtx,
		cancelFunc: cancel,
	}

	// Initialize merger
	mergerConfig := MergerConfig{
		RecordingID:      config.RecordingID,
		BasePath:         config.BasePath,
		OutputPath:       filepath.Join(config.BasePath, config.RecordingID, "merged.ogg"),
		OutputSampleRate: config.SampleRate,
		Logger:           logger,
	}

	manager.merger = NewMerger(recordingCtx, mergerConfig)
	manager.outputPath = mergerConfig.OutputPath

	// Initialize S3 uploader if needed
	if config.UploadToS3 && config.S3Config != nil {
		uploader, err := NewUploader(recordingCtx, *config.S3Config, logger)
		if err != nil {
			cancel()
			return nil, fmt.Errorf("failed to create S3 uploader: %w", err)
		}
		manager.uploader = uploader
	}

	return manager, nil
}

// Start begins the recording
func (m *RecordingManager) Start() error {
	m.recordersLock.Lock()
	defer m.recordersLock.Unlock()

	if m.state == RecordingStateRecording {
		return nil // Already recording
	}

	m.state = RecordingStateRecording

	// Start all recorders
	for _, clientRecorders := range m.recorders {
		for _, recorder := range clientRecorders {
			if err := recorder.Start(); err != nil {
				m.logger.Errorf("Failed to start recorder: %v", err)
			}
		}
	}

	return nil
}

// Pause pauses the recording
func (m *RecordingManager) Pause() error {
	m.recordersLock.Lock()
	defer m.recordersLock.Unlock()

	if m.state != RecordingStateRecording {
		return nil // Not recording
	}

	m.state = RecordingStatePaused

	// Pause all recorders
	for _, clientRecorders := range m.recorders {
		for _, recorder := range clientRecorders {
			if err := recorder.Pause(); err != nil {
				m.logger.Errorf("Failed to pause recorder: %v", err)
			}
		}
	}

	return nil
}

// Resume resumes a paused recording
func (m *RecordingManager) Resume() error {
	m.recordersLock.Lock()
	defer m.recordersLock.Unlock()

	if m.state != RecordingStatePaused {
		return nil // Not paused
	}

	m.state = RecordingStateRecording

	// Resume all recorders
	for _, clientRecorders := range m.recorders {
		for _, recorder := range clientRecorders {
			if err := recorder.Resume(); err != nil {
				m.logger.Errorf("Failed to resume recorder: %v", err)
			}
		}
	}

	return nil
}

// Stop stops the recording and optionally merges files
func (m *RecordingManager) Stop() error {
	m.recordersLock.Lock()
	defer m.recordersLock.Unlock()

	if m.state == RecordingStateStopped {
		return nil // Already stopped
	}

	prevState := m.state
	m.state = RecordingStateStopped

	// Stop all recorders
	for clientID, clientRecorders := range m.recorders {
		for trackID, recorder := range clientRecorders {
			if err := recorder.Stop(); err != nil {
				m.logger.Errorf("Failed to stop recorder: %v", err)
			}

			// Add to merger
			m.merger.AddRecording(recorder.GetMetadata(), recorder.GetFilePath())
			m.logger.Infof("Added recording for client %s, track %s to merger", clientID, trackID)
		}
	}

	// Only merge if we were recording before
	if prevState == RecordingStateRecording || prevState == RecordingStatePaused {
		// Merge files
		outputPath, err := m.merger.MergeAll(m.context)
		if err != nil {
			m.logger.Errorf("Failed to merge recordings: %v", err)
		} else {
			m.logger.Infof("Successfully merged recordings to %s", outputPath)

			// Upload to S3 if configured
			if m.config.UploadToS3 && m.uploader != nil {
				m.logger.Infof("Uploading recordings to S3...")

				urls, err := m.uploader.UploadRecording(m.context, m.config.RecordingID, m.config.BasePath, m.config.UploadMergedOnly)
				if err != nil {
					m.logger.Errorf("Failed to upload recordings: %v", err)
				} else {
					m.logger.Infof("Successfully uploaded %d files to S3", len(urls))
				}
			}
		}
	}

	// Cleanup
	m.cancelFunc()

	return nil
}

// AddTrack adds a track to be recorded
func (m *RecordingManager) AddTrack(clientID, trackID string, channelType int, sampleRate uint32) (Recorder, error) {
	m.recordersLock.Lock()
	defer m.recordersLock.Unlock()

	// Check if already recording this track
	if clientRecorders, exists := m.recorders[clientID]; exists {
		if recorder, exists := clientRecorders[trackID]; exists {
			return recorder, nil
		}
	} else {
		m.recorders[clientID] = make(map[string]Recorder)
	}

	// Don't record tracks with ChannelTypeNoRecord
	if channelType == int(ChannelTypeNoRecord) {
		return nil, fmt.Errorf("track is configured not to be recorded")
	}

	// Create recorder
	recorder, err := NewOggOpusRecorder(
		m.context,
		m.config.BasePath,
		m.config.RecordingID,
		clientID,
		trackID,
		channelType,
		sampleRate,
		m.config.ChannelCount,
		m.logger,
	)

	if err != nil {
		return nil, fmt.Errorf("failed to create recorder: %w", err)
	}

	m.recorders[clientID][trackID] = recorder

	// Start recording if already in recording state
	if m.state == RecordingStateRecording {
		if err := recorder.Start(); err != nil {
			m.logger.Errorf("Failed to start recorder: %v", err)
		}
	}

	return recorder, nil
}

// RemoveTrack stops and removes a track from recording
func (m *RecordingManager) RemoveTrack(clientID, trackID string) error {
	m.recordersLock.Lock()
	defer m.recordersLock.Unlock()

	if clientRecorders, exists := m.recorders[clientID]; exists {
		if recorder, exists := clientRecorders[trackID]; exists {
			if err := recorder.Stop(); err != nil {
				m.logger.Errorf("Failed to stop recorder: %v", err)
			}

			// Add to merger
			m.merger.AddRecording(recorder.GetMetadata(), recorder.GetFilePath())

			delete(clientRecorders, trackID)

			// Remove client if no more tracks
			if len(clientRecorders) == 0 {
				delete(m.recorders, clientID)
			}

			return nil
		}
	}

	return fmt.Errorf("track not found")
}

// WriteRTP writes an RTP packet to the track's recorder
func (m *RecordingManager) WriteRTP(clientID, trackID string, packet *rtp.Packet) error {
	m.recordersLock.RLock()
	defer m.recordersLock.RUnlock()

	if clientRecorders, exists := m.recorders[clientID]; exists {
		if recorder, exists := clientRecorders[trackID]; exists {
			return recorder.WriteRTP(packet)
		}
	}

	return fmt.Errorf("track not found")
}

// GetState returns the current recording state
func (m *RecordingManager) GetState() RecordingState {
	m.recordersLock.RLock()
	defer m.recordersLock.RUnlock()
	return m.state
}

// GetOutputPath returns the path to the merged output file
func (m *RecordingManager) GetOutputPath() string {
	return m.outputPath
}
