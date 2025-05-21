package sfu

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/pion/interceptor"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media/oggwriter"
)

type ChannelType int

const (
	ChannelUnknown ChannelType = iota
	ChannelOne
	ChannelTwo
)

type S3Config struct {
	Secure     bool
	Endpoint   string
	AccessKey  string
	SecretKey  string
	Bucket     string
	FilePrefix string
}

type RecordingConfig struct {
	BasePath       string
	ChannelMapping map[string]ChannelType
	S3             S3Config
}

type trackBuffer struct {
	writer          *oggwriter.OggWriter
	lastPacket      *rtp.Packet
	lastPacketTime  time.Time
	mu              sync.Mutex
	ticker          *time.Ticker
	done            chan struct{}
	sequenceNumber  uint16
	timestamp       uint32
	payloadType     uint8
	sampleRate      uint32
	clientID        string
	trackID         string
	silencePackets  uint64
	regularPackets  uint64
	lastBufferState string
	debug           bool
}

type recordingSession struct {
	id      string
	cfg     RecordingConfig
	writers map[string]map[string]*oggwriter.OggWriter // clientID -> trackID -> writer
	buffers map[string]map[string]*trackBuffer         // clientID -> trackID -> buffer
	mu      sync.Mutex
	paused  bool
	debug   bool
	meta    struct {
		StartTime time.Time
		StopTime  time.Time
		Events    []Event
		Stats     map[string]map[string]BufferStats
	}
}

type BufferStats struct {
	SilencePackets uint64 `json:"silence_packets"`
	RegularPackets uint64 `json:"regular_packets"`
	Duration       string `json:"duration"`
}

// logBufferState logs the current buffer state if debug is enabled
func (b *trackBuffer) logBufferState(state string, details string) {
	if !b.debug {
		return
	}

	b.lastBufferState = state
	fmt.Printf("[Buffer %s:%s] State: %s - %s, Regular: %d, Silence: %d, SeqNum: %d, Timestamp: %d\n",
		b.clientID, b.trackID, state, details, b.regularPackets, b.silencePackets, b.sequenceNumber, b.timestamp)
}

// generateSilencePacket creates an Opus silence packet with the given sequence number and timestamp
func generateSilencePacket(sequenceNumber uint16, timestamp uint32, payloadType uint8) *rtp.Packet {
	// Opus silence frame (2 bytes for Opus silence)
	silencePayload := []byte{0xF8, 0xFF}

	return &rtp.Packet{
		Header: rtp.Header{
			Version:        2,
			Padding:        false,
			Extension:      false,
			Marker:         false,
			PayloadType:    payloadType,
			SequenceNumber: sequenceNumber,
			Timestamp:      timestamp,
			SSRC:           0, // Will be overwritten with the actual SSRC if needed
		},
		Payload: silencePayload,
	}
}

// StartRecording begins recording audio tracks in the room according to the provided config.
func (r *Room) StartRecording(cfg RecordingConfig) (string, error) {
	r.recordingMu.Lock()
	defer r.recordingMu.Unlock()
	if r.recordingSession != nil {
		return "", fmt.Errorf("recording already in progress")
	}
	id := uuid.New().String()
	session := &recordingSession{
		id:      id,
		cfg:     cfg,
		writers: make(map[string]map[string]*oggwriter.OggWriter),
		buffers: make(map[string]map[string]*trackBuffer),
		debug:   os.Getenv("RECORDING_DEBUG") == "true",
	}
	session.meta.StartTime = time.Now()
	session.meta.Stats = make(map[string]map[string]BufferStats)

	baseDir := filepath.Join(cfg.BasePath, id)
	if err := os.MkdirAll(baseDir, 0755); err != nil {
		return "", fmt.Errorf("failed to create recording directory: %w", err)
	}

	// Record client join/leave events
	r.OnClientJoined(func(c *Client) {
		session.mu.Lock()
		session.meta.Events = append(session.meta.Events, Event{
			Type: "client_join",
			Time: time.Now(),
			Data: map[string]interface{}{"client_id": c.ID()},
		})
		session.mu.Unlock()
	})
	r.OnClientLeft(func(c *Client) {
		session.mu.Lock()
		session.meta.Events = append(session.meta.Events, Event{
			Type: "client_leave",
			Time: time.Now(),
			Data: map[string]interface{}{"client_id": c.ID()},
		})
		session.mu.Unlock()
	})

	// Helper to add a track writer for a given client and track
	addWriter := func(clientID string, track ITrack) error {
		session.mu.Lock()
		defer session.mu.Unlock()
		channel := cfg.ChannelMapping[clientID]
		if channel == ChannelUnknown {
			return nil
		}
		if _, ok := session.writers[clientID]; !ok {
			session.writers[clientID] = make(map[string]*oggwriter.OggWriter)
			session.buffers[clientID] = make(map[string]*trackBuffer)
			session.meta.Stats[clientID] = make(map[string]BufferStats)
		}
		trackDir := filepath.Join(baseDir, clientID)
		if err := os.MkdirAll(trackDir, 0755); err != nil {
			return fmt.Errorf("failed to create track directory for %s: %w", clientID, err)
		}
		filePath := filepath.Join(trackDir, fmt.Sprintf("%s.ogg", track.ID()))

		sampleRate := uint32(48000) // Default for Opus
		channelCount := uint16(1)   // Default for Opus

		ow, err := oggwriter.New(filePath, sampleRate, channelCount)
		if err != nil {
			return fmt.Errorf("failed to create ogg writer: %w", err)
		}
		session.writers[clientID][track.ID()] = ow

		// Initialize track buffer
		buffer := &trackBuffer{
			writer:         ow,
			lastPacketTime: time.Now(),
			ticker:         time.NewTicker(20 * time.Millisecond), // 20ms is standard for Opus
			done:           make(chan struct{}),
			sampleRate:     sampleRate,
			payloadType:    111, // Opus payload type
			clientID:       clientID,
			trackID:        track.ID(),
			debug:          session.debug,
		}
		buffer.logBufferState("INIT", "Buffer initialized")
		session.buffers[clientID][track.ID()] = buffer

		// Start the buffer processing goroutine
		go func() {
			timeDelta := uint32(sampleRate / 50) // 20ms worth of samples at the given sample rate
			bufferStartTime := time.Now()

			for {
				select {
				case <-buffer.ticker.C:
					buffer.mu.Lock()
					now := time.Now()

					// If no packet received for more than 20ms, generate silence
					// Only generate silence if we've received at least one real packet before
					if now.Sub(buffer.lastPacketTime) >= 20*time.Millisecond && !session.paused && buffer.lastPacket != nil {
						// Generate silence packet with updated sequence number and timestamp
						buffer.sequenceNumber++
						buffer.timestamp += timeDelta
						silencePacket := generateSilencePacket(buffer.sequenceNumber, buffer.timestamp, buffer.payloadType)
						silencePacket.SSRC = buffer.lastPacket.SSRC

						err := buffer.writer.WriteRTP(silencePacket)
						if err != nil {
							fmt.Printf("ERROR [Buffer %s:%s]: Failed to write silence packet: %v\n",
								buffer.clientID, buffer.trackID, err)
						} else {
							buffer.silencePackets++
							buffer.logBufferState("SILENCE", fmt.Sprintf("Gap: %v", now.Sub(buffer.lastPacketTime)))
						}
					}
					buffer.mu.Unlock()
				case <-buffer.done:
					// Calculate final stats before shutting down
					buffer.mu.Lock()
					duration := time.Since(bufferStartTime)
					session.mu.Lock()
					if _, ok := session.meta.Stats[buffer.clientID]; !ok {
						session.meta.Stats[buffer.clientID] = make(map[string]BufferStats)
					}
					session.meta.Stats[buffer.clientID][buffer.trackID] = BufferStats{
						SilencePackets: buffer.silencePackets,
						RegularPackets: buffer.regularPackets,
						Duration:       duration.String(),
					}
					session.mu.Unlock()
					buffer.logBufferState("CLOSED", fmt.Sprintf("Duration: %v, Ratio: %.2f%%",
						duration, float64(buffer.silencePackets)/float64(buffer.silencePackets+buffer.regularPackets)*100))
					buffer.mu.Unlock()

					buffer.ticker.Stop()
					return
				}
			}
		}()

		track.OnRead(func(attrs interceptor.Attributes, pkt *rtp.Packet, q QualityLevel) {
			// Skip when paused
			if session.paused {
				return
			}

			buffer.mu.Lock()
			defer buffer.mu.Unlock()

			var packetToWrite *rtp.Packet

			// Check if this is RED packet (PT 63) and extract primary Opus payload if needed
			if pkt.PayloadType == 63 {
				// For RED packets, extract the primary payload before writing
				primaryPacket, _, err := ExtractRedPackets(pkt)
				if err != nil {
					fmt.Printf("ERROR [Buffer %s:%s]: Failed to extract primary payload from RED packet: %v\n",
						buffer.clientID, buffer.trackID, err)
					return
				}
				// Explicitly set the payload type to Opus (111) for the oggwriter
				primaryPacket.Header.PayloadType = 111
				packetToWrite = primaryPacket
			} else {
				packetToWrite = pkt
			}

			// Update buffer state
			buffer.lastPacket = packetToWrite
			buffer.lastPacketTime = time.Now()
			buffer.sequenceNumber = packetToWrite.SequenceNumber
			buffer.timestamp = packetToWrite.Timestamp
			buffer.payloadType = packetToWrite.PayloadType

			// Write the actual packet
			err := buffer.writer.WriteRTP(packetToWrite)
			if err != nil {
				fmt.Printf("ERROR [Buffer %s:%s]: Failed to write packet to OGG file: %v\n",
					buffer.clientID, buffer.trackID, err)
			} else {
				buffer.regularPackets++
				if buffer.debug && buffer.regularPackets%1000 == 0 {
					buffer.logBufferState("PACKET", fmt.Sprintf("Received %d regular packets", buffer.regularPackets))
				}
			}
		})
		return nil
	}

	// Subscribe existing clients' tracks
	for clientID, client := range r.SFU().clients.GetClients() {
		for _, track := range client.Tracks() {
			if track.Kind() == webrtc.RTPCodecTypeAudio {
				if err := addWriter(clientID, track); err != nil {
					return id, err
				}
			}
		}
	}

	// Hook future client additions
	r.OnClientJoined(func(c *Client) {
		for _, track := range c.Tracks() {
			if track.Kind() == webrtc.RTPCodecTypeAudio {
				_ = addWriter(c.ID(), track)
			}
		}
	})

	r.recordingSession = session
	return id, nil
}

// PauseRecording pauses writing RTP packets to files.
func (r *Room) PauseRecording() error {
	r.recordingMu.Lock()
	defer r.recordingMu.Unlock()
	if r.recordingSession == nil {
		return fmt.Errorf("no recording in progress")
	}
	r.recordingSession.mu.Lock()
	r.recordingSession.paused = true
	r.recordingSession.meta.Events = append(r.recordingSession.meta.Events, Event{Type: "pause", Time: time.Now(), Data: nil})
	r.recordingSession.mu.Unlock()
	return nil
}

// ResumeRecording resumes writing RTP packets to files.
func (r *Room) ResumeRecording() error {
	r.recordingMu.Lock()
	defer r.recordingMu.Unlock()
	if r.recordingSession == nil {
		return fmt.Errorf("no recording in progress")
	}
	r.recordingSession.mu.Lock()
	r.recordingSession.paused = false
	r.recordingSession.meta.Events = append(r.recordingSession.meta.Events, Event{Type: "resume", Time: time.Now(), Data: nil})
	r.recordingSession.mu.Unlock()
	return nil
}

// StopRecording stops the recording session, closes files, and writes metadata.
func (r *Room) StopRecording() error {
	r.recordingMu.Lock()
	session := r.recordingSession
	r.recordingMu.Unlock()
	if session == nil {
		return fmt.Errorf("no recording in progress")
	}
	session.mu.Lock()
	session.meta.StopTime = time.Now()

	if session.debug {
		fmt.Printf("Stopping recording session %s, duration: %v\n",
			session.id, session.meta.StopTime.Sub(session.meta.StartTime))
	}

	// Stop all buffer processing
	for clientID, clientBuffers := range session.buffers {
		for trackID, buffer := range clientBuffers {
			if session.debug {
				fmt.Printf("Closing buffer for client %s, track %s\n", clientID, trackID)
			}
			close(buffer.done)
		}
	}
	session.mu.Unlock()

	// Wait for a moment to ensure all buffer goroutines have processed their done signals
	time.Sleep(100 * time.Millisecond)

	// Close writers
	for _, m := range session.writers {
		for _, ow := range m {
			_ = ow.Close()
		}
	}
	// Write meta.json
	metaFile := filepath.Join(session.cfg.BasePath, session.id, "meta.json")
	f, err := os.Create(metaFile)
	if err != nil {
		return err
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	if err := enc.Encode(session.meta); err != nil {
		return err
	}

	// Merge channels and upload to S3
	if err := r.mergeAndUpload(session); err != nil {
		return err
	}

	r.recordingMu.Lock()
	r.recordingSession = nil
	r.recordingMu.Unlock()
	return nil
}

// mergeAndUpload mixes per-channel recordings, merges stereo, uploads to S3, and removes local files.
func (r *Room) mergeAndUpload(session *recordingSession) error {
	baseDir := filepath.Join(session.cfg.BasePath, session.id)
	// Group track files by channel
	filesByChannel := map[ChannelType][]string{}
	for clientID, writerMap := range session.writers {
		ch := session.cfg.ChannelMapping[clientID]
		for trackID := range writerMap {
			filesByChannel[ch] = append(filesByChannel[ch], filepath.Join(baseDir, clientID, fmt.Sprintf("%s.ogg", trackID)))
		}
	}
	// Create mono mixes per channel
	monoFiles := map[ChannelType]string{}
	for _, ch := range []ChannelType{ChannelOne, ChannelTwo} {
		inputs := filesByChannel[ch]
		if len(inputs) == 0 {
			continue
		}
		monoPath := filepath.Join(baseDir, fmt.Sprintf("mono_%d.ogg", ch))
		args := []string{"-y"}
		for _, in := range inputs {
			args = append(args, "-i", in)
		}
		filter := fmt.Sprintf("amix=inputs=%d:duration=longest", len(inputs))
		args = append(args, "-filter_complex", filter, "-ac", "1", monoPath)
		cmd := exec.Command("ffmpeg", args...)
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("ffmpeg mix channel %d failed: %v, output: %s", ch, err, string(out))
		}
		monoFiles[ch] = monoPath
	}
	// Merge to stereo
	finalPath := filepath.Join(baseDir, session.id+".ogg")
	left, hasLeft := monoFiles[ChannelOne]
	right, hasRight := monoFiles[ChannelTwo]
	if hasLeft && hasRight {
		cmd := exec.Command("ffmpeg", "-y", "-i", left, "-i", right, "-filter_complex", "amerge=inputs=2", "-ac", "2", finalPath)
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("ffmpeg merge stereo failed: %v, output: %s", err, string(out))
		}
	} else if hasLeft || hasRight {
		src := left
		if !hasLeft {
			src = right
		}
		if err := os.Rename(src, finalPath); err != nil {
			return err
		}
	} else {
		return fmt.Errorf("no audio to merge")
	}
	// Upload to S3
	mc, err := minio.New(session.cfg.S3.Endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(session.cfg.S3.AccessKey, session.cfg.S3.SecretKey, ""),
		Secure: session.cfg.S3.Secure,
	})
	if err != nil {
		return err
	}
	object := filepath.Join(session.cfg.S3.FilePrefix, session.id+".ogg")
	ctx := context.Background()
	_, err = mc.FPutObject(ctx, session.cfg.S3.Bucket, object, finalPath, minio.PutObjectOptions{ContentType: "audio/ogg"})
	if err != nil {
		return err
	}
	// Cleanup local files
	return os.RemoveAll(baseDir)
}
