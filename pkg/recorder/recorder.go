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

// ParticipantRecorder handles recording for a single participant
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
	recordingsPath     string
	participantRecsMap map[string]*ParticipantRecorder
	state              RecordingState
	ctx                context.Context
	cancel             context.CancelFunc
	audioMerger        *AudioMerger
	ffmpegPath         string
	s3Uploader         *S3Uploader
	autoUploadToS3     bool
}

// NewRoomRecorder creates a new room recorder
func NewRoomRecorder(ctx context.Context, roomID, recordingsPath, ffmpegPath string) (*RoomRecorder, error) {
	roomDirPath := filepath.Join(recordingsPath, roomID)
	if err := os.MkdirAll(roomDirPath, 0755); err != nil {
		return nil, fmt.Errorf("failed to create room directory: %w", err)
	}

	recCtx, cancel := context.WithCancel(ctx)

	// Create audio merger
	audioMerger := NewAudioMerger(ffmpegPath)

	return &RoomRecorder{
		mu:                 sync.RWMutex{},
		roomID:             roomID,
		recordingsPath:     recordingsPath,
		participantRecsMap: make(map[string]*ParticipantRecorder),
		state:              RecordingStateActive,
		ctx:                recCtx,
		cancel:             cancel,
		audioMerger:        audioMerger,
		ffmpegPath:         ffmpegPath,
	}, nil
}

// ConfigureS3Upload sets up the S3 uploader with the given configuration
func (r *RoomRecorder) ConfigureS3Upload(config *S3UploadConfig) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	// If config is nil or not enabled, disable S3 uploads
	if config == nil || !config.Enabled {
		r.s3Uploader = nil
		r.autoUploadToS3 = false
		return nil
	}

	// Create uploader
	uploader, err := NewS3Uploader(r.ctx, *config)
	if err != nil {
		return fmt.Errorf("failed to create S3 uploader: %w", err)
	}

	r.s3Uploader = uploader
	r.autoUploadToS3 = true
	return nil
}

// AddParticipant adds a participant to the recording
func (r *RoomRecorder) AddParticipant(participantID, trackID string, sampleRate uint32, channelCount uint16) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.state == RecordingStateStopped {
		return fmt.Errorf("recording already stopped")
	}

	// Check if participant already exists
	if _, exists := r.participantRecsMap[participantID]; exists {
		return fmt.Errorf("participant already exists")
	}

	// Create participant directory
	participantDir := filepath.Join(r.recordingsPath, r.roomID, participantID)
	if err := os.MkdirAll(participantDir, 0755); err != nil {
		return fmt.Errorf("failed to create participant directory: %w", err)
	}

	// Create OGG file
	oggPath := filepath.Join(participantDir, "track.ogg")
	writer, err := oggwriter.New(oggPath, sampleRate, channelCount)
	if err != nil {
		return fmt.Errorf("failed to create OGG writer: %w", err)
	}

	// Create metadata
	metadataPath := filepath.Join(participantDir, "meta.json")
	metadata := ParticipantMetadata{
		ParticipantID: participantID,
		TrackID:       trackID,
		StartTime:     time.Now(),
	}

	// Create cancellable context
	ctx, cancel := context.WithCancel(r.ctx)

	// Create participant recorder
	recorder := &ParticipantRecorder{
		writer:        writer,
		metadataPath:  metadataPath,
		metadata:      metadata,
		state:         r.state, // inherit room state
		ctx:           ctx,
		cancel:        cancel,
		sampleRate:    sampleRate,
		channelCount:  channelCount,
		lastTimestamp: time.Now(),
	}

	// Save metadata
	if err := saveMetadata(metadataPath, metadata); err != nil {
		cancel()
		return fmt.Errorf("failed to save metadata: %w", err)
	}

	r.participantRecsMap[participantID] = recorder
	return nil
}

// WriteRTP writes an RTP packet for a participant
func (r *RoomRecorder) WriteRTP(participantID string, packet *rtp.Packet) error {
	r.mu.RLock()
	recorder, exists := r.participantRecsMap[participantID]
	r.mu.RUnlock()

	if !exists {
		return fmt.Errorf("participant not found")
	}

	recorder.mu.Lock()
	defer recorder.mu.Unlock()

	if recorder.state != RecordingStateActive {
		return nil // Silently ignore if not active
	}

	return recorder.writer.WriteRTP(packet)
}

// PauseRecording pauses recording for the entire room
func (r *RoomRecorder) PauseRecording() error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.state != RecordingStateActive {
		return nil // Already paused or stopped
	}

	r.state = RecordingStatePaused

	for _, rec := range r.participantRecsMap {
		rec.mu.Lock()
		if rec.state == RecordingStateActive {
			rec.state = RecordingStatePaused
			rec.metadata.PausedPeriods = append(rec.metadata.PausedPeriods, struct {
				Start time.Time  `json:"start"`
				End   *time.Time `json:"end,omitempty"`
			}{
				Start: time.Now(),
			})
			saveMetadata(rec.metadataPath, rec.metadata)
		}
		rec.mu.Unlock()
	}

	return nil
}

// ResumeRecording resumes recording for the entire room
func (r *RoomRecorder) ResumeRecording() error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.state != RecordingStatePaused {
		return nil // Not paused
	}

	r.state = RecordingStateActive

	now := time.Now()
	for _, rec := range r.participantRecsMap {
		rec.mu.Lock()
		if rec.state == RecordingStatePaused {
			rec.state = RecordingStateActive
			// Set end time for the last pause period
			if len(rec.metadata.PausedPeriods) > 0 {
				lastIdx := len(rec.metadata.PausedPeriods) - 1
				rec.metadata.PausedPeriods[lastIdx].End = &now
			}
			saveMetadata(rec.metadataPath, rec.metadata)
		}
		rec.mu.Unlock()
	}

	return nil
}

// StopRecording stops recording for the entire room and merges tracks
func (r *RoomRecorder) StopRecording() error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.state == RecordingStateStopped {
		return nil // Already stopped
	}

	r.state = RecordingStateStopped

	// Stop all participant recorders
	for _, rec := range r.participantRecsMap {
		rec.mu.Lock()

		// Set end time in metadata
		now := time.Now()
		rec.metadata.EndTime = &now

		// If recording was paused, also end the last pause period
		if rec.state == RecordingStatePaused && len(rec.metadata.PausedPeriods) > 0 {
			lastIdx := len(rec.metadata.PausedPeriods) - 1
			rec.metadata.PausedPeriods[lastIdx].End = &now
		}

		// Update metadata
		saveMetadata(rec.metadataPath, rec.metadata)

		// Close writer
		if err := rec.writer.Close(); err != nil {
			rec.mu.Unlock()
			return fmt.Errorf("failed to close writer for participant %s: %w", rec.metadata.ParticipantID, err)
		}

		rec.state = RecordingStateStopped
		rec.cancel()
		rec.mu.Unlock()
	}

	// Create a background context for upload operations that won't be cancelled
	// when the recorder's context is cancelled
	uploadCtx := context.Background()

	// Merge tracks (this should be done in a goroutine to not block)
	go func() {
		if err := r.mergeParticipantTracks(); err != nil {
			// Log the error but don't stop processing
			fmt.Printf("Error merging tracks: %v\n", err)
		}

		// Upload to S3 if enabled
		if r.s3Uploader != nil {
			r.autoUploadToS3 = true // Ensure auto-upload is enabled

			// Don't force DeleteAfterUpload to true, respect original configuration
			// This ensures files are only deleted after successful upload but kept if upload fails

			roomDir := filepath.Join(r.recordingsPath, r.roomID)

			// Create a new S3Uploader with the background context instead of using the one
			// that will be cancelled by r.cancel() below
			var uploader *S3Uploader
			var err error

			if uploader, err = NewS3Uploader(uploadCtx, r.s3Uploader.config); err != nil {
				fmt.Printf("Error creating S3 uploader: %v\n", err)
				return
			}
			defer uploader.Close()

			if err := uploader.UploadDirectory(roomDir, r.roomID); err != nil {
				fmt.Printf("Error uploading recordings to S3: %v\n", err)
			} else {
				fmt.Printf("Successfully uploaded recordings for room %s to S3\n", r.roomID)
			}
		}
	}()

	r.cancel()
	return nil
}

// mergeParticipantTracks merges all participant tracks into a single audio file
func (r *RoomRecorder) mergeParticipantTracks() error {
	// Make sure all recordings are stopped
	r.mu.RLock()
	for _, rec := range r.participantRecsMap {
		if rec.state != RecordingStateStopped {
			r.mu.RUnlock()
			return fmt.Errorf("cannot merge tracks while recordings are in progress")
		}
	}
	r.mu.RUnlock()

	// Create output WAV file path
	outputFile := filepath.Join(r.recordingsPath, r.roomID, "merged.wav")

	// Merge the recordings using AudioMerger
	roomPath := filepath.Join(r.recordingsPath, r.roomID)
	if err := r.audioMerger.MergeParticipantRecordings(roomPath, outputFile); err != nil {
		return fmt.Errorf("failed to merge participant recordings: %w", err)
	}

	// Create merged metadata file
	mergedMetadata := struct {
		RoomID       string                `json:"room_id"`
		Participants []ParticipantMetadata `json:"participants"`
		MergedAt     time.Time             `json:"merged_at"`
		MergedFile   string                `json:"merged_file"`
	}{
		RoomID:     r.roomID,
		MergedAt:   time.Now(),
		MergedFile: outputFile,
	}

	r.mu.RLock()
	for _, rec := range r.participantRecsMap {
		rec.mu.Lock()
		mergedMetadata.Participants = append(mergedMetadata.Participants, rec.metadata)
		rec.mu.Unlock()
	}
	r.mu.RUnlock()

	mergeMetadataPath := filepath.Join(r.recordingsPath, r.roomID, "merged_metadata.json")
	return saveMetadata(mergeMetadataPath, mergedMetadata)
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

// RemoveParticipant removes a participant recorder
func (r *RoomRecorder) RemoveParticipant(participantID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	rec, exists := r.participantRecsMap[participantID]
	if !exists {
		return fmt.Errorf("participant %s not found", participantID)
	}

	rec.mu.Lock()
	defer rec.mu.Unlock()

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
		return fmt.Errorf("failed to save metadata: %w", err)
	}

	// Close writer
	if err := rec.writer.Close(); err != nil {
		return fmt.Errorf("failed to close writer: %w", err)
	}

	rec.state = RecordingStateStopped
	rec.cancel()

	delete(r.participantRecsMap, participantID)
	return nil
}

// GetState returns the current recording state
func (r *RoomRecorder) GetState() RecordingState {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.state
}

// UploadToS3 manually uploads the recordings to S3
func (r *RoomRecorder) UploadToS3() error {
	r.mu.RLock()
	defer r.mu.RUnlock()

	if r.s3Uploader == nil {
		return fmt.Errorf("s3 uploader not configured")
	}

	if r.state != RecordingStateStopped {
		return fmt.Errorf("cannot upload recordings while recording is in progress")
	}

	roomDir := filepath.Join(r.recordingsPath, r.roomID)
	return r.s3Uploader.UploadDirectory(roomDir, r.roomID)
}

// GetS3UploadStatus returns the status of the S3 upload
func (r *RoomRecorder) GetS3UploadStatus() (bool, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	if r.s3Uploader == nil {
		return false, fmt.Errorf("s3 uploader not configured")
	}

	return r.s3Uploader.GetUploadStatus(r.roomID)
}
