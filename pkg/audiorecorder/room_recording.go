package audiorecorder

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// RoomRecorder manages recording for an entire room
type RoomRecorder struct {
	mu             sync.Mutex
	basePath       string
	roomID         string
	recordingID    string
	recorder       *Recorder
	cleanupOnError bool
}

// NewRoomRecorder creates a new room recorder
func NewRoomRecorder(basePath, recordingID string, cleanupOnError bool) *RoomRecorder {
	return &RoomRecorder{
		basePath:       basePath,
		recordingID:    recordingID,
		cleanupOnError: cleanupOnError,
	}
}

// StartRecording starts recording for the room
func (r *RoomRecorder) StartRecording(s3Config *S3Config) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.recorder != nil {
		return fmt.Errorf("recording already in progress")
	}

	r.recorder = NewRecorder(r.basePath, r.roomID, r.recordingID)
	if err := r.recorder.Start(s3Config); err != nil {
		if r.cleanupOnError {
			r.cleanup()
		}
		return err
	}

	return nil
}

// StopRecording stops the recording and processes the files
func (r *RoomRecorder) StopRecording() error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.recorder == nil {
		return fmt.Errorf("recording not in progress")
	}

	err := r.recorder.Stop()

	// Clean up after successful upload
	if err == nil && r.cleanupOnError {
		r.cleanup()
	}

	r.recorder = nil
	return err
}

// PauseRecording pauses the current recording
func (r *RoomRecorder) PauseRecording() error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.recorder == nil {
		return fmt.Errorf("recording not in progress")
	}

	return r.recorder.Pause()
}

// ResumeRecording resumes a paused recording
func (r *RoomRecorder) ResumeRecording() error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.recorder == nil {
		return fmt.Errorf("recording not in progress")
	}

	return r.recorder.Resume()
}

// IsRecording returns whether recording is currently active
func (r *RoomRecorder) IsRecording() bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.recorder == nil {
		return false
	}

	return r.recorder.IsRecording()
}

// GetRecorder returns the current recorder instance
func (r *RoomRecorder) GetRecorder() *Recorder {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.recorder
}

// cleanup removes the recording directory after successful processing
func (r *RoomRecorder) cleanup() {
	if r.recorder == nil || r.recorder.recordingID == "" {
		return
	}

	recordingPath := filepath.Join(r.basePath, r.recorder.recordingID)
	if err := os.RemoveAll(recordingPath); err != nil {
		// Just log the error, don't fail
		fmt.Printf("Failed to clean up recording directory: %v\n", err)
	}
}

// AddTrack adds a track to the recorder
func (r *RoomRecorder) AddTrack(clientID, trackID string, sampleRate uint32, channelType ChannelType) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.recorder == nil {
		return fmt.Errorf("recording not in progress")
	}

	return r.recorder.AddTrack(clientID, trackID, sampleRate, channelType)
}

// RemoveTrack removes a track from the recorder
func (r *RoomRecorder) RemoveTrack(clientID, trackID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.recorder == nil {
		return fmt.Errorf("recording not in progress")
	}

	return r.recorder.RemoveTrack(clientID, trackID)
}

// LogClientJoined logs when a client joins the room
func (r *RoomRecorder) LogClientJoined(clientID string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.recorder == nil {
		return
	}

	r.recorder.LogClientJoined(clientID)
}

// LogClientLeft logs when a client leaves the room
func (r *RoomRecorder) LogClientLeft(clientID string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.recorder == nil {
		return
	}

	r.recorder.LogClientLeft(clientID)
}

// SetRoomID sets the room ID for this recorder
func (r *RoomRecorder) SetRoomID(roomID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.roomID = roomID
}
