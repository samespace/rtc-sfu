package audiorecorder

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4/pkg/media/oggwriter"
)

// ChannelType represents which audio channel a client should be recorded to
type ChannelType int

const (
	// ChannelUnknown means this client should not be recorded
	ChannelUnknown ChannelType = 0
	// ChannelOne takes the first channel in stereo output
	ChannelOne ChannelType = 1
	// ChannelTwo takes the second channel in stereo output
	ChannelTwo ChannelType = 2
)

// RecordingState represents the current state of recording
type RecordingState int

const (
	// RecordingStopped indicates recording is not active
	RecordingStopped RecordingState = 0
	// RecordingActive indicates recording is ongoing
	RecordingActive RecordingState = 1
	// RecordingPaused indicates recording is paused
	RecordingPaused RecordingState = 2
)

// S3Config contains configuration for S3 bucket upload
type S3Config struct {
	Secure     bool   `json:"secure"`
	Endpoint   string `json:"endpoint"`
	AccessKey  string `json:"accessKey"`
	SecretKey  string `json:"secretKey"`
	Bucket     string `json:"bucket"`
	FilePrefix string `json:"filePrefix"`
}

// Track represents a recording track for a client
type Track struct {
	ClientID    string
	TrackID     string
	ChannelType ChannelType
	Writer      *oggwriter.OggWriter
	FilePath    string
}

// Event represents a recording event for metadata
type Event struct {
	Type      string    `json:"type"`
	Timestamp time.Time `json:"timestamp"`
	ClientID  string    `json:"client_id,omitempty"`
	TrackID   string    `json:"track_id,omitempty"`
	Data      string    `json:"data,omitempty"`
}

// Recorder manages audio recording for a room
type Recorder struct {
	mu           sync.Mutex
	basePath     string
	recordingID  string
	roomID       string
	tracks       map[string]*Track
	state        RecordingState
	s3Config     *S3Config
	metadataFile *os.File
	events       []Event
	context      context.Context
	cancel       context.CancelFunc
}

// NewRecorder creates a new audio recorder
func NewRecorder(basePath, roomID, recordingID string) *Recorder {
	ctx, cancel := context.WithCancel(context.Background())
	return &Recorder{
		basePath:    basePath,
		roomID:      roomID,
		recordingID: recordingID,
		tracks:      make(map[string]*Track),
		state:       RecordingStopped,
		events:      make([]Event, 0),
		context:     ctx,
		cancel:      cancel,
	}
}

// Write a log message to a debug file in the recording directory
func (r *Recorder) writeDebugLog(message string) {
	if r.recordingID == "" {
		return
	}

	debugPath := filepath.Join(r.basePath, r.recordingID, "debug.log")
	f, err := os.OpenFile(debugPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return
	}
	defer f.Close()

	timestamp := time.Now().Format(time.RFC3339)
	_, _ = f.WriteString(fmt.Sprintf("[%s] %s\n", timestamp, message))
}

// Start begins a new recording session
func (r *Recorder) Start(s3Config *S3Config) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.state == RecordingActive {
		return fmt.Errorf("recording already in progress")
	}

	// We don't generate recordingID anymore as it's passed in constructor
	r.s3Config = s3Config
	r.state = RecordingActive

	// Create directory structure
	recordingPath := filepath.Join(r.basePath, r.recordingID)
	if err := os.MkdirAll(recordingPath, 0755); err != nil {
		return fmt.Errorf("failed to create recording directory: %w", err)
	}

	r.writeDebugLog(fmt.Sprintf("Starting recording for room %s with recording ID %s", r.roomID, r.recordingID))

	// Initialize metadata file
	metadataPath := filepath.Join(recordingPath, "metadata.json")
	file, err := os.Create(metadataPath)
	if err != nil {
		r.writeDebugLog(fmt.Sprintf("Failed to create metadata file: %v", err))
		return fmt.Errorf("failed to create metadata file: %w", err)
	}
	r.metadataFile = file

	// Log start event
	r.logEvent(Event{
		Type:      "recording_started",
		Timestamp: time.Now(),
		Data:      r.roomID,
	})

	r.writeDebugLog("Recording started successfully")
	return nil
}

// Stop ends the recording and processes files
func (r *Recorder) Stop() error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.state == RecordingStopped {
		return fmt.Errorf("recording is not active")
	}

	r.writeDebugLog("Stopping recording")
	r.writeDebugLog(fmt.Sprintf("Number of tracks: %d", len(r.tracks)))

	// Close all track writers
	for trackKey, track := range r.tracks {
		r.writeDebugLog(fmt.Sprintf("Closing track: %s", trackKey))
		if track.Writer != nil {
			track.Writer.Close()
		}
	}

	// Log stop event
	r.logEvent(Event{
		Type:      "recording_stopped",
		Timestamp: time.Now(),
		Data:      r.roomID,
	})

	// Save metadata file
	if err := r.saveMetadata(); err != nil {
		r.writeDebugLog(fmt.Sprintf("Failed to save metadata: %v", err))
		return fmt.Errorf("failed to save metadata: %w", err)
	}

	r.writeDebugLog("Metadata saved successfully")

	// Process the recordings (merge and upload)
	r.writeDebugLog("Starting to process recordings")
	if err := r.processRecordings(); err != nil {
		r.writeDebugLog(fmt.Sprintf("Failed to process recordings: %v", err))
		return fmt.Errorf("failed to process recordings: %w", err)
	}

	r.writeDebugLog("Recording processed successfully")
	r.state = RecordingStopped
	r.cancel()
	return nil
}

// Pause pauses the current recording
func (r *Recorder) Pause() error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.state != RecordingActive {
		return fmt.Errorf("recording is not active")
	}

	r.state = RecordingPaused

	// Log pause event
	r.logEvent(Event{
		Type:      "recording_paused",
		Timestamp: time.Now(),
		Data:      r.roomID,
	})

	return nil
}

// Resume resumes a paused recording
func (r *Recorder) Resume() error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.state != RecordingPaused {
		return fmt.Errorf("recording is not paused")
	}

	r.state = RecordingActive

	// Log resume event
	r.logEvent(Event{
		Type:      "recording_resumed",
		Timestamp: time.Now(),
		Data:      r.roomID,
	})

	return nil
}

// AddTrack creates a new track for recording
func (r *Recorder) AddTrack(clientID, trackID string, sampleRate uint32, channelType ChannelType) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.state == RecordingStopped {
		return fmt.Errorf("recording is not active")
	}

	if channelType == ChannelUnknown {
		// Skip recording for this track
		return nil
	}

	// Create client directory if it doesn't exist
	clientPath := filepath.Join(r.basePath, r.recordingID, clientID)
	if err := os.MkdirAll(clientPath, 0755); err != nil {
		return fmt.Errorf("failed to create client directory: %w", err)
	}

	// Create OggWriter for this track
	filePath := filepath.Join(clientPath, fmt.Sprintf("%s.ogg", trackID))
	writer, err := oggwriter.New(filePath, sampleRate, 1)
	if err != nil {
		return fmt.Errorf("failed to create ogg writer: %w", err)
	}

	track := &Track{
		ClientID:    clientID,
		TrackID:     trackID,
		ChannelType: channelType,
		Writer:      writer,
		FilePath:    filePath,
	}

	trackKey := fmt.Sprintf("%s-%s", clientID, trackID)
	r.tracks[trackKey] = track

	// Log track added event
	r.logEvent(Event{
		Type:      "track_added",
		Timestamp: time.Now(),
		ClientID:  clientID,
		TrackID:   trackID,
	})

	return nil
}

// RemoveTrack stops recording for a specific track
func (r *Recorder) RemoveTrack(clientID, trackID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	trackKey := fmt.Sprintf("%s-%s", clientID, trackID)
	track, exists := r.tracks[trackKey]
	if !exists {
		return fmt.Errorf("track not found")
	}

	if track.Writer != nil {
		track.Writer.Close()
	}

	delete(r.tracks, trackKey)

	// Log track removed event
	r.logEvent(Event{
		Type:      "track_removed",
		Timestamp: time.Now(),
		ClientID:  clientID,
		TrackID:   trackID,
	})

	return nil
}

// WriteRTPPacket writes an RTP packet to the corresponding track's file
func (r *Recorder) WriteRTPPacket(clientID, trackID string, packet *rtp.Packet) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.state != RecordingActive {
		return nil // Silently ignore if recording is not active or paused
	}

	trackKey := fmt.Sprintf("%s-%s", clientID, trackID)
	track, exists := r.tracks[trackKey]
	if !exists {
		return fmt.Errorf("track not found")
	}

	if track.Writer != nil {
		err := track.Writer.WriteRTP(packet)
		if err != nil {
			r.writeDebugLog(fmt.Sprintf("Error writing RTP packet to track %s: %v", trackKey, err))
		}
		return err
	}

	return nil
}

// LogClientJoined logs when a client joins the room
func (r *Recorder) LogClientJoined(clientID string) {
	r.logEvent(Event{
		Type:      "client_joined",
		Timestamp: time.Now(),
		ClientID:  clientID,
	})
}

// LogClientLeft logs when a client leaves the room
func (r *Recorder) LogClientLeft(clientID string) {
	r.logEvent(Event{
		Type:      "client_left",
		Timestamp: time.Now(),
		ClientID:  clientID,
	})
}

// IsRecording returns whether recording is currently active
func (r *Recorder) IsRecording() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.state == RecordingActive
}

// logEvent adds an event to the metadata
func (r *Recorder) logEvent(event Event) {
	r.events = append(r.events, event)
}

// saveMetadata writes all events to the metadata file
func (r *Recorder) saveMetadata() error {
	if r.metadataFile == nil {
		r.writeDebugLog("Failed to save metadata: metadata file not initialized")
		return fmt.Errorf("metadata file not initialized")
	}

	r.writeDebugLog(fmt.Sprintf("Saving %d events to metadata file", len(r.events)))

	// Seek to beginning of file
	if _, err := r.metadataFile.Seek(0, io.SeekStart); err != nil {
		r.writeDebugLog(fmt.Sprintf("Failed to seek in metadata file: %v", err))
		return err
	}

	// Truncate file
	if err := r.metadataFile.Truncate(0); err != nil {
		r.writeDebugLog(fmt.Sprintf("Failed to truncate metadata file: %v", err))
		return err
	}

	// Write events as JSON
	encoder := json.NewEncoder(r.metadataFile)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(r.events); err != nil {
		r.writeDebugLog(fmt.Sprintf("Failed to encode events to JSON: %v", err))
		return err
	}

	if err := r.metadataFile.Sync(); err != nil {
		r.writeDebugLog(fmt.Sprintf("Failed to sync metadata file: %v", err))
		return err
	}

	r.writeDebugLog("Metadata saved successfully")
	return nil
}
