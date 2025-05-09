package recorder

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4/pkg/media/oggwriter"
)

// RecordingState represents the current state of recording
type RecordingState string

const (
	// RecordingStateActive is when recording is active
	RecordingStateActive RecordingState = "active"
	// RecordingStatePaused is when recording is paused
	RecordingStatePaused RecordingState = "paused"
	// RecordingStateStopped is when recording is stopped
	RecordingStateStopped RecordingState = "stopped"
)

// ParticipantMetadata contains metadata about the participant recording
type ParticipantMetadata struct {
	ParticipantID string     `json:"participant_id"`
	TrackID       string     `json:"track_id"`
	StartTime     time.Time  `json:"start_time"`
	EndTime       *time.Time `json:"end_time,omitempty"`
	PausedPeriods []struct {
		Start time.Time  `json:"start"`
		End   *time.Time `json:"end,omitempty"`
	} `json:"paused_periods,omitempty"`
}

// ParticipantRecorder handles recording for a single track
type ParticipantRecorder struct {
	mu            sync.Mutex
	writer        *oggwriter.OggWriter
	metadataPath  string
	metadata      ParticipantMetadata
	state         RecordingState
	ctx           context.Context
	cancel        context.CancelFunc
	sampleRate    uint32
	channelCount  uint16
	lastTimestamp time.Time
}

// RoomRecorder handles recording for an entire room
type RoomRecorder struct {
	mu                 sync.RWMutex
	roomID             string
	recordingID        string // Unique identifier for this recording session
	recordingsPath     string
	participantRecsMap map[string]map[string]*ParticipantRecorder // map[participantID]map[trackID]*ParticipantRecorder
	state              RecordingState
	ctx                context.Context
	cancel             context.CancelFunc
	audioMerger        *AudioMerger
	ffmpegPath         string
	// S3 Upload related fields
	s3Uploader     *S3Uploader
	s3Enabled      bool
	deleteLocal    bool
	s3BucketPrefix string
}

// NewRoomRecorder creates a new room recorder
func NewRoomRecorder(ctx context.Context, recordingID, recordingsPath, ffmpegPath string) (*RoomRecorder, error) {
	// Create recordings directory if it doesn't exist
	if err := os.MkdirAll(recordingsPath, 0755); err != nil {
		return nil, fmt.Errorf("failed to create recordings directory: %w", err)
	}

	recCtx, cancel := context.WithCancel(ctx)

	return &RoomRecorder{
		mu:                 sync.RWMutex{},
		roomID:             recordingID,
		recordingID:        recordingID,
		recordingsPath:     recordingsPath,
		participantRecsMap: make(map[string]map[string]*ParticipantRecorder),
		state:              RecordingStateActive,
		ctx:                recCtx,
		cancel:             cancel,
		audioMerger:        NewAudioMerger(ffmpegPath),
		ffmpegPath:         ffmpegPath,
	}, nil
}

// SetS3Config configures S3 upload for the recording
func (r *RoomRecorder) SetS3Config(enabled bool, creds S3Credentials, deleteLocal bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.s3Enabled = enabled
	r.deleteLocal = deleteLocal
	r.s3BucketPrefix = creds.BucketPrefix

	if enabled {
		uploader, err := NewS3Uploader(r.ctx, creds)
		if err != nil {
			return fmt.Errorf("failed to configure S3 uploader: %w", err)
		}
		r.s3Uploader = uploader
	}

	return nil
}

// AddParticipant adds a new participant track to be recorded
func (r *RoomRecorder) AddParticipant(participantID, trackID string, sampleRate uint32, channelCount uint16) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	// Check if participant exists in the map, if not create a new entry
	if _, exists := r.participantRecsMap[participantID]; !exists {
		r.participantRecsMap[participantID] = make(map[string]*ParticipantRecorder)
	}

	// Check if this specific track is already being recorded
	if _, exists := r.participantRecsMap[participantID][trackID]; exists {
		return fmt.Errorf("track %s for participant %s already being recorded", trackID, participantID)
	}

	// Create participant directory
	participantDirPath := filepath.Join(r.recordingsPath, r.roomID, participantID)
	if err := os.MkdirAll(participantDirPath, 0755); err != nil {
		return fmt.Errorf("failed to create participant directory: %w", err)
	}

	// Create OGG writer with track ID in the filename
	audioFilePath := filepath.Join(participantDirPath, fmt.Sprintf("track_%s.ogg", trackID))
	oggWriter, err := oggwriter.New(audioFilePath, sampleRate, channelCount)
	if err != nil {
		return fmt.Errorf("failed to create ogg writer: %w", err)
	}

	metadataPath := filepath.Join(participantDirPath, fmt.Sprintf("meta_%s.json", trackID))

	recCtx, cancel := context.WithCancel(r.ctx)

	// Create participant recorder
	metadata := ParticipantMetadata{
		ParticipantID: participantID,
		TrackID:       trackID,
		StartTime:     time.Now(),
	}

	// Save initial metadata
	if err := saveMetadata(metadataPath, metadata); err != nil {
		cancel()
		return fmt.Errorf("failed to save metadata: %w", err)
	}

	r.participantRecsMap[participantID][trackID] = &ParticipantRecorder{
		mu:            sync.Mutex{},
		writer:        oggWriter,
		metadataPath:  metadataPath,
		metadata:      metadata,
		state:         RecordingStateActive,
		ctx:           recCtx,
		cancel:        cancel,
		sampleRate:    sampleRate,
		channelCount:  channelCount,
		lastTimestamp: time.Now(),
	}

	return nil
}

// WriteRTP writes an RTP packet for a specific participant track
func (r *RoomRecorder) WriteRTP(participantID string, packet *rtp.Packet) error {
	// Guard against nil recorder
	if r == nil {
		return fmt.Errorf("recorder is nil")
	}

	r.mu.RLock()
	// Check if recorder is stopped
	if r.state == RecordingStateStopped {
		r.mu.RUnlock()
		return nil // Silently ignore when recorder is stopped
	}

	participantMap, exists := r.participantRecsMap[participantID]
	r.mu.RUnlock()

	if !exists || len(participantMap) == 0 {
		return fmt.Errorf("participant %s not found", participantID)
	}

	// Write to all tracks for this participant
	// This assumes the packet's SSRC or another property could identify which track it belongs to
	// For now, just write to all tracks
	for trackID, recorder := range participantMap {
		recorder.mu.Lock()
		// Only write if recording is active
		if recorder.state == RecordingStateActive {
			recorder.lastTimestamp = time.Now()
			err := recorder.writer.WriteRTP(packet)
			recorder.mu.Unlock()
			if err != nil {
				// Just log errors and continue
				fmt.Printf("error writing RTP packet to track %s: %v\n", trackID, err)
			}
		} else {
			recorder.mu.Unlock()
		}
	}

	return nil
}

// PauseRecording pauses recording for the entire room
func (r *RoomRecorder) PauseRecording() error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.state == RecordingStatePaused {
		return nil // Already paused
	}

	r.state = RecordingStatePaused

	// Pause all participant recorders
	for _, participantMap := range r.participantRecsMap {
		for _, rec := range participantMap {
			rec.mu.Lock()
			if rec.state == RecordingStateActive {
				rec.state = RecordingStatePaused
				pausePeriod := struct {
					Start time.Time  `json:"start"`
					End   *time.Time `json:"end,omitempty"`
				}{
					Start: time.Now(),
				}
				rec.metadata.PausedPeriods = append(rec.metadata.PausedPeriods, pausePeriod)
				// Update metadata
				saveMetadata(rec.metadataPath, rec.metadata)
			}
			rec.mu.Unlock()
		}
	}

	return nil
}

// ResumeRecording resumes recording for the entire room
func (r *RoomRecorder) ResumeRecording() error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.state == RecordingStateActive {
		return nil // Already active
	}

	r.state = RecordingStateActive

	// Resume all participant recorders
	for _, participantMap := range r.participantRecsMap {
		for _, rec := range participantMap {
			rec.mu.Lock()
			if rec.state == RecordingStatePaused && len(rec.metadata.PausedPeriods) > 0 {
				rec.state = RecordingStateActive
				// Update the end time of the last pause period
				lastIdx := len(rec.metadata.PausedPeriods) - 1
				now := time.Now()
				rec.metadata.PausedPeriods[lastIdx].End = &now
				// Update metadata
				saveMetadata(rec.metadataPath, rec.metadata)
			}
			rec.mu.Unlock()
		}
	}

	return nil
}

// StopRecording stops recording for the entire room and merges tracks
func (r *RoomRecorder) StopRecording() error {
	r.mu.Lock()

	if r.state == RecordingStateStopped {
		r.mu.Unlock()
		return nil // Already stopped
	}

	r.state = RecordingStateStopped

	// Create local copies of all data needed for the async processing
	roomID := r.roomID
	recordingID := r.recordingID
	recordingsPath := r.recordingsPath
	ffmpegPath := r.ffmpegPath
	s3Enabled := r.s3Enabled
	s3Uploader := r.s3Uploader
	deleteLocal := r.deleteLocal
	s3BucketPrefix := r.s3BucketPrefix

	// Copy participant data to avoid race conditions
	type participantData struct {
		metadata     ParticipantMetadata
		metadataPath string
		writer       *oggwriter.OggWriter
	}

	participantsCopy := make(map[string]map[string]participantData)

	// Stop all participant recorders and gather their data
	for participantID, trackMap := range r.participantRecsMap {
		if _, exists := participantsCopy[participantID]; !exists {
			participantsCopy[participantID] = make(map[string]participantData)
		}
		for trackID, rec := range trackMap {
			rec.mu.Lock()

			// Set end time in metadata
			now := time.Now()
			rec.metadata.EndTime = &now

			// If recording was paused, also end the last pause period
			if rec.state == RecordingStatePaused && len(rec.metadata.PausedPeriods) > 0 {
				lastIdx := len(rec.metadata.PausedPeriods) - 1
				rec.metadata.PausedPeriods[lastIdx].End = &now
			}

			// Copy metadata
			participantsCopy[participantID][trackID] = participantData{
				metadata:     rec.metadata,
				metadataPath: rec.metadataPath,
				writer:       rec.writer,
			}

			// Update original metadata
			saveMetadata(rec.metadataPath, rec.metadata)

			// Mark as stopped
			rec.state = RecordingStateStopped
			rec.cancel()
			rec.mu.Unlock()
		}
	}

	// Create a background context that won't be canceled
	bgCtx := context.Background()

	// Cancel the recorder context now that we've captured all needed data
	r.cancel()
	r.mu.Unlock()

	// Process everything asynchronously
	go func() {
		// First, close all writers (outside of locks)
		for _, trackMap := range participantsCopy {
			for _, participant := range trackMap {
				if err := participant.writer.Close(); err != nil {
					fmt.Printf("Error closing writer: %v\n", err)
				}
			}
		}

		// Now perform the merging and upload
		roomPath := filepath.Join(recordingsPath, recordingID)
		outputFile := filepath.Join(recordingsPath, recordingID, "merged.wav")

		// Create a new audio merger directly
		audioMerger := NewAudioMerger(ffmpegPath)

		// Merge the participant recordings
		if err := audioMerger.MergeParticipantRecordings(roomPath, outputFile); err != nil {
			fmt.Printf("Error merging tracks: %v\n", err)
			return
		}

		// Create metadata for merged file
		mergedMetadata := struct {
			RoomID       string                `json:"room_id"`
			RecordingID  string                `json:"recording_id"`
			Participants []ParticipantMetadata `json:"participants"`
			MergedAt     time.Time             `json:"merged_at"`
			MergedFile   string                `json:"merged_file"`
			S3Uploaded   bool                  `json:"s3_uploaded,omitempty"`
			S3ObjectName string                `json:"s3_object_name,omitempty"`
		}{
			RoomID:      roomID,
			RecordingID: recordingID,
			MergedAt:    time.Now(),
			MergedFile:  outputFile,
		}

		// Add participant data to metadata
		for _, trackMap := range participantsCopy {
			for _, participant := range trackMap {
				mergedMetadata.Participants = append(mergedMetadata.Participants, participant.metadata)
			}
		}

		// Add S3 info to metadata if enabled
		if s3Enabled {
			mergedMetadata.S3Uploaded = true
			if s3BucketPrefix != "" {
				mergedMetadata.S3ObjectName = filepath.Join(s3BucketPrefix, fmt.Sprintf("%s.wav", recordingID))
			} else {
				mergedMetadata.S3ObjectName = fmt.Sprintf("%s.wav", recordingID)
			}
		}

		// Save merged metadata
		mergeMetadataPath := filepath.Join(recordingsPath, recordingID, "merged_metadata.json")
		if err := saveMetadata(mergeMetadataPath, mergedMetadata); err != nil {
			fmt.Printf("Error saving merge metadata: %v\n", err)
		}

		// Upload to S3 if enabled
		if s3Enabled && s3Uploader != nil {
			objectName := fmt.Sprintf("%s.wav", recordingID)

			// We use the background context for upload to ensure it's not cancelled
			if err := s3Uploader.UploadFile(bgCtx, outputFile, objectName); err != nil {
				fmt.Printf("Error uploading to S3: %v\n", err)
				return
			}

			// Clean up local directory if configured to do so
			if deleteLocal {
				dirPath := filepath.Join(recordingsPath, recordingID)
				if err := s3Uploader.CleanupDirectory(dirPath); err != nil {
					fmt.Printf("Error cleaning up local directory: %v\n", err)
				}
			}
		}
	}()

	return nil
}

// saveMetadata saves metadata to a JSON file
func saveMetadata(path string, data interface{}) error {
	file, err := os.Create(path)
	if err != nil {
		return err
	}
	defer file.Close()

	encoder := json.NewEncoder(file)
	encoder.SetIndent("", "  ")
	return encoder.Encode(data)
}

// RemoveParticipant removes a participant from recording
func (r *RoomRecorder) RemoveParticipant(participantID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	trackMap, exists := r.participantRecsMap[participantID]
	if !exists {
		return fmt.Errorf("participant %s not found", participantID)
	}

	// Stop all tracks for this participant
	for _, rec := range trackMap {
		rec.mu.Lock()

		// Set end time if not already set
		if rec.metadata.EndTime == nil {
			now := time.Now()
			rec.metadata.EndTime = &now

			// If paused, end the last pause period
			if rec.state == RecordingStatePaused && len(rec.metadata.PausedPeriods) > 0 {
				lastIdx := len(rec.metadata.PausedPeriods) - 1
				rec.metadata.PausedPeriods[lastIdx].End = &now
			}
		}

		// Update metadata
		if err := saveMetadata(rec.metadataPath, rec.metadata); err != nil {
			rec.mu.Unlock()
			return fmt.Errorf("failed to save metadata: %w", err)
		}

		// Close writer
		if err := rec.writer.Close(); err != nil {
			rec.mu.Unlock()
			return fmt.Errorf("failed to close writer: %w", err)
		}

		// Mark as stopped
		rec.state = RecordingStateStopped
		rec.cancel()
		rec.mu.Unlock()
	}

	delete(r.participantRecsMap, participantID)

	return nil
}

// GetState returns the current recording state
func (r *RoomRecorder) GetState() RecordingState {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.state
}
