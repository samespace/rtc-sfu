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
}

// NewRoomRecorder creates a new room recorder
func NewRoomRecorder(ctx context.Context, roomID, recordingsPath, ffmpegPath string) (*RoomRecorder, error) {
	roomDirPath := filepath.Join(recordingsPath, roomID)
	if err := os.MkdirAll(roomDirPath, 0755); err != nil {
		return nil, fmt.Errorf("failed to create room directory: %w", err)
	}

	recCtx, cancel := context.WithCancel(ctx)

	return &RoomRecorder{
		mu:                 sync.RWMutex{},
		roomID:             roomID,
		recordingsPath:     recordingsPath,
		participantRecsMap: make(map[string]*ParticipantRecorder),
		state:              RecordingStateActive,
		ctx:                recCtx,
		cancel:             cancel,
		audioMerger:        NewAudioMerger(ffmpegPath),
		ffmpegPath:         ffmpegPath,
	}, nil
}

// AddParticipant adds a new participant to be recorded
func (r *RoomRecorder) AddParticipant(participantID, trackID string, sampleRate uint32, channelCount uint16) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	// Check if participant already exists
	if _, exists := r.participantRecsMap[participantID]; exists {
		return fmt.Errorf("participant %s already being recorded", participantID)
	}

	// Create participant directory
	participantDirPath := filepath.Join(r.recordingsPath, r.roomID, participantID)
	if err := os.MkdirAll(participantDirPath, 0755); err != nil {
		return fmt.Errorf("failed to create participant directory: %w", err)
	}

	// Create OGG writer
	audioFilePath := filepath.Join(participantDirPath, "track.ogg")
	oggWriter, err := oggwriter.New(audioFilePath, sampleRate, channelCount)
	if err != nil {
		return fmt.Errorf("failed to create ogg writer: %w", err)
	}

	metadataPath := filepath.Join(participantDirPath, "meta.json")

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

	r.participantRecsMap[participantID] = &ParticipantRecorder{
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

// WriteRTP writes an RTP packet for a specific participant
func (r *RoomRecorder) WriteRTP(participantID string, packet *rtp.Packet) error {
	r.mu.RLock()
	recorder, exists := r.participantRecsMap[participantID]
	r.mu.RUnlock()

	if !exists {
		return fmt.Errorf("participant %s not found", participantID)
	}

	recorder.mu.Lock()
	defer recorder.mu.Unlock()

	// Only write if recording is active
	if recorder.state == RecordingStateActive {
		recorder.lastTimestamp = time.Now()
		return recorder.writer.WriteRTP(packet)
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
	for _, rec := range r.participantRecsMap {
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
	for _, rec := range r.participantRecsMap {
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

	// Merge tracks (this should be done in a goroutine to not block)
	go func() {
		if err := r.mergeParticipantTracks(); err != nil {
			// Log the error but don't stop processing
			fmt.Printf("Error merging tracks: %v\n", err)
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
