package recording

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/pion/logging"
	"github.com/pion/rtp"
)

// RecorderState represents the current state of a recorder
type RecorderState int

const (
	// RecorderStateStopped indicates the recorder is stopped
	RecorderStateStopped RecorderState = iota
	// RecorderStateRecording indicates the recorder is currently recording
	RecorderStateRecording
	// RecorderStatePaused indicates the recorder is paused
	RecorderStatePaused
)

// TrackMetadata stores metadata about a recorded track
type TrackMetadata struct {
	TrackID      string    `json:"trackId"`
	ClientID     string    `json:"clientId"`
	StartTime    time.Time `json:"startTime"`
	StopTime     time.Time `json:"stopTime,omitempty"`
	ChannelType  int       `json:"channelType"`
	SampleRate   uint32    `json:"sampleRate"`
	ChannelCount uint8     `json:"channelCount"`
}

// Recorder interface defines methods for recording audio
type Recorder interface {
	// Start begins recording
	Start() error
	// Pause pauses the recording
	Pause() error
	// Resume resumes a paused recording
	Resume() error
	// Stop stops the recording and finalizes the file
	Stop() error
	// WriteRTP writes an RTP packet to the recording
	WriteRTP(packet *rtp.Packet) error
	// GetState returns the current recorder state
	GetState() RecorderState
	// GetFilePath returns the path to the recording file
	GetFilePath() string
	// GetMetadata returns the track metadata
	GetMetadata() TrackMetadata
}

// OggOpusRecorder implements Recorder for OggOpus encoding
type OggOpusRecorder struct {
	mu           sync.Mutex
	state        RecorderState
	file         *os.File
	sampleRate   uint32
	channelCount uint8
	serialNumber uint32
	pageIndex    uint32
	granulePos   uint64
	lastPacketTS uint32
	metadata     TrackMetadata
	filePath     string
	logger       logging.LeveledLogger
	// Added new fields for debugging
	packetCount uint32
}

// NewOggOpusRecorder creates a new OggOpusRecorder
func NewOggOpusRecorder(ctx context.Context, basePath, recordingID, clientID, trackID string, channelType int, sampleRate uint32, channels uint8, logger logging.LeveledLogger) (*OggOpusRecorder, error) {
	// Create path for the recording file
	dirPath := filepath.Join(basePath, recordingID, clientID)
	if err := os.MkdirAll(dirPath, 0755); err != nil {
		fmt.Printf("### RECORDING DEBUG: Failed to create directory for recorder: %v\n", err)
		return nil, fmt.Errorf("failed to create directory: %w", err)
	}

	filePath := filepath.Join(dirPath, trackID+".opus")
	fmt.Printf("### RECORDING DEBUG: Creating OggOpusRecorder for file: %s\n", filePath)

	// Check if directory is writeable
	if info, err := os.Stat(dirPath); err != nil {
		fmt.Printf("### RECORDING DEBUG: Failed to stat recording directory: %v\n", err)
	} else {
		fmt.Printf("### RECORDING DEBUG: Recording directory (%s) mode: %v\n", dirPath, info.Mode())
	}

	// Create metadata for this track
	metadata := TrackMetadata{
		ClientID:     clientID,
		TrackID:      trackID,
		StartTime:    time.Now(),
		SampleRate:   sampleRate,
		ChannelCount: channels,
		ChannelType:  channelType,
	}

	recorder := &OggOpusRecorder{
		filePath:     filePath,
		channelCount: channels,
		sampleRate:   sampleRate,
		state:        RecorderStateStopped,
		logger:       logger,
		metadata:     metadata,
		packetCount:  0,
		serialNumber: uint32(time.Now().UnixNano() % 1000000),
	}

	fmt.Printf("### RECORDING DEBUG: OggOpusRecorder created successfully for track %s, client %s\n", trackID, clientID)
	return recorder, nil
}

// Start begins recording
func (r *OggOpusRecorder) Start() error {
	r.mu.Lock()
	defer r.mu.Unlock()

	fmt.Printf("### RECORDING DEBUG: Starting recorder for file: %s, current state: %d\n", r.filePath, r.state)

	if r.state != RecorderStateStopped {
		fmt.Printf("### RECORDING DEBUG: Recorder already started or paused\n")
		return nil // Already started or paused
	}

	// Open the file for writing
	file, err := os.Create(r.filePath)
	if err != nil {
		fmt.Printf("### RECORDING DEBUG: Failed to create file for recording: %v, path: %s\n", err, r.filePath)
		return fmt.Errorf("failed to create file: %w", err)
	}

	fmt.Printf("### RECORDING DEBUG: Created file for recording: %s\n", r.filePath)

	r.file = file
	r.state = RecorderStateRecording
	r.metadata.StartTime = time.Now()
	r.packetCount = 0

	// Write file headers
	if err := r.writeHeaders(); err != nil {
		fmt.Printf("### RECORDING DEBUG: Failed to write headers: %v\n", err)
		file.Close()
		r.file = nil
		return fmt.Errorf("failed to write headers: %w", err)
	}

	fmt.Printf("### RECORDING DEBUG: Recorder started successfully for file: %s\n", r.filePath)
	return nil
}

// Pause pauses the recording
func (r *OggOpusRecorder) Pause() error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.state != RecorderStateRecording {
		return nil // Not recording
	}

	r.state = RecorderStatePaused
	return nil
}

// Resume resumes a paused recording
func (r *OggOpusRecorder) Resume() error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.state != RecorderStatePaused {
		return nil // Not paused
	}

	r.state = RecorderStateRecording
	return nil
}

// Stop stops recording and closes the file
func (r *OggOpusRecorder) Stop() error {
	r.mu.Lock()
	defer r.mu.Unlock()

	fmt.Printf("### RECORDING DEBUG: Stopping recorder for file: %s, current state: %d, packets recorded: %d\n",
		r.filePath, r.state, r.packetCount)

	if r.state == RecorderStateStopped {
		fmt.Printf("### RECORDING DEBUG: Recorder already stopped\n")
		return nil // Already stopped
	}

	r.state = RecorderStateStopped

	// Close the file
	var err error
	if r.file != nil {
		// Write final page
		if finalErr := r.writeOggPage(nil, false); finalErr != nil {
			fmt.Printf("### RECORDING DEBUG: Failed to write final page: %v\n", finalErr)
			err = finalErr
		}

		if closeErr := r.file.Close(); closeErr != nil && err == nil {
			fmt.Printf("### RECORDING DEBUG: Failed to close file: %v\n", closeErr)
			err = closeErr
		} else {
			fmt.Printf("### RECORDING DEBUG: Successfully closed recording file: %s\n", r.filePath)
		}
		r.file = nil
	}

	// Write metadata file
	metadataPath := r.filePath + ".json"
	r.metadata.StopTime = time.Now()
	if metaErr := r.writeMetadata(metadataPath); metaErr != nil {
		fmt.Printf("### RECORDING DEBUG: Failed to write metadata: %v\n", metaErr)
		if err == nil {
			err = metaErr
		}
	} else {
		fmt.Printf("### RECORDING DEBUG: Successfully wrote metadata file: %s\n", metadataPath)
	}

	fmt.Printf("### RECORDING DEBUG: Recorder stopped successfully for file: %s, duration: %v, packets: %d\n",
		r.filePath, r.metadata.StopTime.Sub(r.metadata.StartTime), r.packetCount)

	return err
}

// WriteRTP writes an RTP packet to the recorder
func (r *OggOpusRecorder) WriteRTP(packet *rtp.Packet) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.state != RecorderStateRecording {
		// Silently drop packets when not recording
		return nil
	}

	if r.file == nil {
		if r.packetCount%100 == 0 { // Don't log too frequently
			fmt.Printf("### RECORDING DEBUG: Cannot write RTP packet - file is nil for file: %s\n", r.filePath)
		}
		return fmt.Errorf("file is nil")
	}

	// Extract Opus packet from RTP payload
	opusPacket := packet.Payload

	// Calculate granule position based on timestamp
	if r.lastPacketTS == 0 {
		r.lastPacketTS = packet.Timestamp
	} else {
		// Convert RTP timestamp diff to samples
		tsDiff := packet.Timestamp - r.lastPacketTS
		r.lastPacketTS = packet.Timestamp

		// 48000 is the reference sample rate for Opus
		samples := (tsDiff * 48000) / r.sampleRate
		r.granulePos += uint64(samples)
	}

	// Write the packet
	if err := r.writeOggPage(opusPacket, false); err != nil {
		// Only log errors occasionally to avoid flooding
		if r.packetCount%100 == 0 {
			fmt.Printf("### RECORDING DEBUG: Failed to write RTP packet to file %s: %v\n", r.filePath, err)
		}
		return fmt.Errorf("failed to write RTP packet: %w", err)
	}

	r.packetCount++

	// Log progress periodically
	if r.packetCount == 1 || r.packetCount%500 == 0 {
		fmt.Printf("### RECORDING DEBUG: Written %d packets to file: %s, PT: %d, file size: %d bytes\n",
			r.packetCount, r.filePath, packet.PayloadType, r.getFileSize())
	}

	return nil
}

// GetState returns the current recorder state
func (r *OggOpusRecorder) GetState() RecorderState {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.state
}

// GetFilePath returns the path to the recording file
func (r *OggOpusRecorder) GetFilePath() string {
	return r.filePath
}

// GetMetadata returns the track metadata
func (r *OggOpusRecorder) GetMetadata() TrackMetadata {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.metadata
}

// Write Ogg Opus headers
func (r *OggOpusRecorder) writeHeaders() error {
	// Write OpusHead header
	opusHead := &OggOpusHeader{
		Version:         1,
		ChannelCount:    r.channelCount,
		PreSkip:         0,
		InputSampleRate: r.sampleRate,
		OutputGain:      0,
		MappingFamily:   0,
	}

	headData := opusHead.Marshal()

	// Write ID Header page
	headPacket := append([]byte("OpusHead"), headData...)
	if err := r.writeOggPage(headPacket, true); err != nil {
		return err
	}

	// Write OpusTags header
	opusTags := &OggOpusTags{
		Vendor: "github.com/inlivedev/sfu",
		Comments: []string{
			"ENCODER=inlivedev/sfu",
		},
	}

	tagsData := opusTags.Marshal()
	tagsPacket := append([]byte("OpusTags"), tagsData...)

	return r.writeOggPage(tagsPacket, false)
}

// Write an Ogg page
func (r *OggOpusRecorder) writeOggPage(data []byte, bos bool) error {
	// Ogg page header
	header := make([]byte, 27)
	copy(header[0:4], []byte("OggS")) // Capture pattern
	header[4] = 0                     // Stream structure version
	header[5] = 0                     // Header type flag

	if bos {
		header[5] |= 0x02 // Beginning of stream
	}

	// Granule position (8 bytes)
	binary.LittleEndian.PutUint64(header[6:14], r.granulePos)

	// Stream serial number
	binary.LittleEndian.PutUint32(header[14:18], r.serialNumber)

	// Page sequence number
	binary.LittleEndian.PutUint32(header[18:22], r.pageIndex)
	r.pageIndex++

	// CRC checksum (calculate later)
	binary.LittleEndian.PutUint32(header[22:26], 0)

	// Number of segments
	header[26] = 1

	// Segment table
	segmentTable := []byte{byte(len(data))}

	// Write header, segment table, and data
	if _, err := r.file.Write(header); err != nil {
		return err
	}

	if _, err := r.file.Write(segmentTable); err != nil {
		return err
	}

	if _, err := r.file.Write(data); err != nil {
		return err
	}

	// Flush to disk
	return r.file.Sync()
}

// Write metadata to JSON file
func (r *OggOpusRecorder) writeMetadata(path string) error {
	metadataJSON, err := json.Marshal(r.metadata)
	if err != nil {
		return err
	}

	file, err := os.Create(path)
	if err != nil {
		return err
	}
	defer file.Close()

	_, err = file.Write(metadataJSON)
	return err
}

// OggOpusHeader represents the OpusHead header structure
type OggOpusHeader struct {
	Version         uint8
	ChannelCount    uint8
	PreSkip         uint16
	InputSampleRate uint32
	OutputGain      int16
	MappingFamily   uint8
}

// Marshal serializes the header to bytes
func (h *OggOpusHeader) Marshal() []byte {
	data := make([]byte, 19)

	data[0] = h.Version
	data[1] = h.ChannelCount
	binary.LittleEndian.PutUint16(data[2:4], h.PreSkip)
	binary.LittleEndian.PutUint32(data[4:8], h.InputSampleRate)
	binary.LittleEndian.PutUint16(data[8:10], uint16(h.OutputGain))
	data[10] = h.MappingFamily

	return data
}

// OggOpusTags represents the OpusTags header structure
type OggOpusTags struct {
	Vendor   string
	Comments []string
}

// Marshal serializes the tags to bytes
func (t *OggOpusTags) Marshal() []byte {
	// Calculate size
	vendorLen := len(t.Vendor)
	size := 8 + vendorLen // 4 bytes for vendor length + vendor string + 4 bytes for comment count

	for _, comment := range t.Comments {
		size += 4 + len(comment) // 4 bytes for comment length + comment string
	}

	data := make([]byte, size)

	// Vendor length
	binary.LittleEndian.PutUint32(data[0:4], uint32(vendorLen))

	// Vendor string
	copy(data[4:4+vendorLen], t.Vendor)

	// Comment count
	binary.LittleEndian.PutUint32(data[4+vendorLen:8+vendorLen], uint32(len(t.Comments)))

	// Comments
	offset := 8 + vendorLen
	for _, comment := range t.Comments {
		commentLen := len(comment)
		binary.LittleEndian.PutUint32(data[offset:offset+4], uint32(commentLen))
		copy(data[offset+4:offset+4+commentLen], comment)
		offset += 4 + commentLen
	}

	return data
}

// Helper to get file size
func (r *OggOpusRecorder) getFileSize() int64 {
	if r.file == nil {
		return 0
	}

	info, err := r.file.Stat()
	if err != nil {
		return 0
	}
	return info.Size()
}
