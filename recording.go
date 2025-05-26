package sfu

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
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

type recordingSession struct {
	id        string
	cfg       RecordingConfig
	writers   map[string]map[string]*trackWriter // clientID -> trackID -> writer
	mu        sync.Mutex
	paused    bool
	startTime time.Time // Add explicit start time tracking
	meta      struct {
		StartTime time.Time
		StopTime  time.Time
		Events    []Event
	}
}

type trackWriter struct {
	writer           *oggwriter.OggWriter
	lastPacketTime   time.Time
	lastRTPTimestamp uint32
	lastSeqNum       uint16
	clockRate        uint32
	totalSamples     uint64

	// New fields for proper mute handling
	ourRTPTimestamp    uint32    // Our own RTP timestamp counter
	ourSeqNum          uint16    // Our own sequence number counter
	firstPacketTime    time.Time // Wall-clock time of first packet
	isFirstPacket      bool      // Whether we're waiting for the first packet
	recordingStartTime time.Time // When recording actually started
	lastPayloadType    uint8     // Last known payload type
	lastSSRC           uint32    // Last known SSRC
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
		id:        id,
		cfg:       cfg,
		writers:   make(map[string]map[string]*trackWriter),
		startTime: time.Now(), // Initialize recording start time
	}
	session.meta.StartTime = session.startTime

	baseDir := filepath.Join(cfg.BasePath, id)
	if err := os.MkdirAll(baseDir, 0755); err != nil {
		return "", err
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

		fmt.Printf("adding writer for client %s, track %s", clientID, track.ID())

		session.mu.Lock()
		defer session.mu.Unlock()
		channel := cfg.ChannelMapping[clientID]
		if channel == ChannelUnknown {
			return nil
		}
		if _, ok := session.writers[clientID]; !ok {
			session.writers[clientID] = make(map[string]*trackWriter)
		}
		trackDir := filepath.Join(baseDir, clientID)
		if err := os.MkdirAll(trackDir, 0755); err != nil {
			return err
		}
		filePath := filepath.Join(trackDir, fmt.Sprintf("%s.ogg", track.ID()))

		sampleRate := uint32(48000) // Default for Opus
		channelCount := uint16(1)   // Default for Opus

		// Use type switch to handle both Track and AudioTrack types
		var codecParams webrtc.RTPCodecParameters
		switch t := track.(type) {
		case *Track:
			codecParams = t.base.codec
		case *AudioTrack:
			codecParams = t.Track.base.codec
		default:
			r.sfu.log.Warnf("room: unknown track type: %T", track)
			return nil
		}

		if codecParams.ClockRate > 0 {
			sampleRate = uint32(codecParams.ClockRate)
		}

		ow, err := oggwriter.New(filePath, sampleRate, channelCount)
		if err != nil {
			return err
		}
		// Create trackWriter with proper clock rate
		session.writers[clientID][track.ID()] = &trackWriter{
			writer:             ow,
			clockRate:          sampleRate, // 48000 for Opus
			lastPacketTime:     time.Time{},
			lastRTPTimestamp:   0,
			lastSeqNum:         0,
			ourRTPTimestamp:    0,
			ourSeqNum:          0,
			isFirstPacket:      true,
			recordingStartTime: session.startTime,
			lastPayloadType:    0,
			lastSSRC:           0,
		}
		track.OnRead(func(attrs interceptor.Attributes, pkt *rtp.Packet, q QualityLevel) {
			if session.paused {
				return
			}

			session.mu.Lock()
			defer session.mu.Unlock()

			tw := session.writers[clientID][track.ID()]
			if tw == nil {
				return
			}

			const samplesPerPacket = 960 // 48000Hz * 0.02s (20ms packets)
			currentTime := time.Now()

			// Handle first packet
			if tw.isFirstPacket {
				tw.isFirstPacket = false
				tw.firstPacketTime = currentTime
				tw.lastPacketTime = currentTime
				tw.lastRTPTimestamp = pkt.Timestamp
				tw.lastSeqNum = pkt.SequenceNumber
				tw.ourRTPTimestamp = pkt.Timestamp
				tw.ourSeqNum = pkt.SequenceNumber
				tw.lastPayloadType = pkt.PayloadType
				tw.lastSSRC = pkt.SSRC

				// Calculate silence needed from recording start to first packet
				timeSinceRecordingStart := currentTime.Sub(tw.recordingStartTime)
				if timeSinceRecordingStart > 100*time.Millisecond { // Only add silence if gap > 100ms
					silenceDurationMs := int64(timeSinceRecordingStart / time.Millisecond)
					silencePackets := silenceDurationMs / 20 // 20ms per packet

					fmt.Printf("Adding %d silence packets (%dms) before first packet for client %s\n",
						silencePackets, silenceDurationMs, clientID)

					for i := int64(0); i < silencePackets; i++ {
						silentPkt := createSilencePacket(
							pkt.PayloadType,
							tw.ourSeqNum,
							tw.ourRTPTimestamp,
							pkt.SSRC,
							false, // Don't mark packets in the middle of stream
						)

						if err := writeRTPWithSamples(tw.writer, silentPkt, samplesPerPacket); err != nil {
							fmt.Printf("error writing initial silence packet: %v\n", err)
							continue
						}

						tw.totalSamples += uint64(samplesPerPacket)
						tw.ourSeqNum++
						tw.ourRTPTimestamp += samplesPerPacket
					}
				}

				// Mark the actual first packet
				pkt.Marker = true
			} else {
				// Handle subsequent packets - detect mute periods
				timeSinceLastPacket := currentTime.Sub(tw.lastPacketTime)

				// If more than 40ms has passed since last packet, we likely have a mute period
				if timeSinceLastPacket > 40*time.Millisecond {
					silenceDurationMs := int64(timeSinceLastPacket / time.Millisecond)
					silencePackets := silenceDurationMs / 20 // 20ms per packet

					// Cap silence to prevent runaway (max 20 seconds)
					if silencePackets > 1000 {
						silencePackets = 1000
					}

					fmt.Printf("Detected mute period: %dms (%d packets) for client %s\n",
						silenceDurationMs, silencePackets, clientID)

					for i := int64(0); i < silencePackets; i++ {
						silentPkt := createSilencePacket(
							pkt.PayloadType,
							tw.ourSeqNum,
							tw.ourRTPTimestamp,
							pkt.SSRC,
							false, // Don't mark packets in the middle of stream
						)

						if err := writeRTPWithSamples(tw.writer, silentPkt, samplesPerPacket); err != nil {
							fmt.Printf("error writing mute silence packet: %v\n", err)
							continue
						}

						tw.totalSamples += uint64(samplesPerPacket)
						tw.ourSeqNum++
						tw.ourRTPTimestamp += samplesPerPacket
					}
				}
			}

			fmt.Printf("Client Packet (Client ID): %s (Track ID): %s\n", clientID, track.ID())
			fmt.Printf("Packet Timestamp: %d, Our Timestamp: %d\n", pkt.Timestamp, tw.ourRTPTimestamp)
			fmt.Printf("Packet Sequence Number: %d, Our Seq: %d\n", pkt.SequenceNumber, tw.ourSeqNum)
			fmt.Printf("Time since last packet: %v\n", currentTime.Sub(tw.lastPacketTime))
			fmt.Printf("--------------------------------\n")

			// Update packet to use our sequence number and timestamp
			pkt.SequenceNumber = tw.ourSeqNum
			pkt.Timestamp = tw.ourRTPTimestamp

			// Write the actual packet
			if err := writeRTPWithSamples(tw.writer, pkt, samplesPerPacket); err != nil {
				fmt.Printf("error writing actual packet: %v\n", err)
			} else {
				tw.totalSamples += uint64(samplesPerPacket)
			}

			// Update our tracking
			tw.lastPacketTime = currentTime
			tw.lastRTPTimestamp = pkt.Timestamp
			tw.lastSeqNum = pkt.SequenceNumber
			tw.lastPayloadType = pkt.PayloadType
			tw.lastSSRC = pkt.SSRC
			tw.ourSeqNum++
			tw.ourRTPTimestamp += samplesPerPacket
		})

		fmt.Printf("added writer for client %s, track %s", clientID, track.ID())

		return nil
	}

	// Subscribe existing clients' tracks
	for clientID, client := range r.SFU().clients.GetClients() {
		fmt.Printf("Client Loop: %s", clientID)
		for _, track := range client.Tracks() {
			go func() {
				if track.Kind() == webrtc.RTPCodecTypeAudio {
					if err := addWriter(clientID, track); err != nil {
						fmt.Printf("error adding writer for client %s, track %s: %v", clientID, track.ID(), err)
					}
				}
			}()
		}

		// add a hook for add track too
		client.OnTracksAdded(func(tracks []ITrack) {
			for _, track := range tracks {
				if track.Kind() == webrtc.RTPCodecTypeAudio {
					_ = addWriter(clientID, track)
				}
			}
		})
	}

	// Hook future client additions
	r.OnClientJoined(func(c *Client) {
		fmt.Printf("Client Joined: %s", c.ID())
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

	// Add final silence for tracks that were muted when recording stopped
	const samplesPerPacket = 960 // 48000Hz * 0.02s (20ms packets)
	stopTime := session.meta.StopTime

	for clientID, writerMap := range session.writers {
		for trackID, tw := range writerMap {
			if tw == nil {
				continue
			}

			// Handle tracks that were completely muted (never received any packets)
			if tw.isFirstPacket {
				// This track never received any packets, fill entire recording duration
				totalRecordingDuration := stopTime.Sub(tw.recordingStartTime)
				silenceDurationMs := int64(totalRecordingDuration / time.Millisecond)
				silencePackets := silenceDurationMs / 20 // 20ms per packet

				// Cap silence to prevent runaway (max 20 seconds)
				if silencePackets > 1000 {
					silencePackets = 1000
				}

				fmt.Printf("Track was completely muted - adding %d silence packets (%dms) for client %s, track %s\n",
					silencePackets, silenceDurationMs, clientID, trackID)

				for i := int64(0); i < silencePackets; i++ {
					silentPkt := createSilencePacket(
						111, // Default Opus payload type
						tw.ourSeqNum,
						tw.ourRTPTimestamp,
						1,      // Default SSRC
						i == 0, // Mark first packet
					)

					if err := writeRTPWithSamples(tw.writer, silentPkt, samplesPerPacket); err != nil {
						fmt.Printf("error writing complete silence packet: %v\n", err)
						continue
					}

					tw.totalSamples += uint64(samplesPerPacket)
					tw.ourSeqNum++
					tw.ourRTPTimestamp += samplesPerPacket
				}
				continue
			}

			// Calculate silence needed from last packet to recording stop
			timeSinceLastPacket := stopTime.Sub(tw.lastPacketTime)
			if timeSinceLastPacket > 40*time.Millisecond { // Only add if gap > 40ms
				silenceDurationMs := int64(timeSinceLastPacket / time.Millisecond)
				silencePackets := silenceDurationMs / 20 // 20ms per packet

				// Cap silence to prevent runaway (max 20 seconds)
				if silencePackets > 1000 {
					silencePackets = 1000
				}

				// Use default values if track never received packets
				payloadType := tw.lastPayloadType
				ssrc := tw.lastSSRC
				if payloadType == 0 {
					payloadType = 111 // Default Opus payload type
					ssrc = 1          // Default SSRC
				}

				fmt.Printf("Adding %d final silence packets (%dms) for client %s, track %s\n",
					silencePackets, silenceDurationMs, clientID, trackID)

				for i := int64(0); i < silencePackets; i++ {
					silentPkt := createSilencePacket(
						payloadType, // Use stored or default payload type
						tw.ourSeqNum,
						tw.ourRTPTimestamp,
						ssrc,  // Use stored or default SSRC
						false, // Don't mark packets at end of stream
					)

					if err := writeRTPWithSamples(tw.writer, silentPkt, samplesPerPacket); err != nil {
						fmt.Printf("error writing final silence packet: %v\n", err)
						continue
					}

					tw.totalSamples += uint64(samplesPerPacket)
					tw.ourSeqNum++
					tw.ourRTPTimestamp += samplesPerPacket
				}
			}
		}
	}

	session.mu.Unlock()

	// Close writers
	for _, m := range session.writers {
		for _, tw := range m {
			tw.writer.Close()
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
		if len(inputs) == 1 {
			src := inputs[0]
			inF, err := os.Open(src)
			if err != nil {
				return fmt.Errorf("copy file for channel %d failed: %v", ch, err)
			}
			defer inF.Close()
			outF, err := os.Create(monoPath)
			if err != nil {
				return fmt.Errorf("copy file for channel %d failed: %v", ch, err)
			}
			defer outF.Close()
			if _, err := io.Copy(outF, inF); err != nil {
				return fmt.Errorf("copy file for channel %d failed: %v", ch, err)
			}
			monoFiles[ch] = monoPath
			continue
		}
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
		fmt.Printf("error creating minio client: %v", err)
		return err
	}
	object := filepath.Join(session.cfg.S3.FilePrefix, session.id+".ogg")
	ctx := context.Background()
	_, err = mc.FPutObject(ctx, session.cfg.S3.Bucket, object, finalPath, minio.PutObjectOptions{ContentType: "audio/ogg"})
	if err != nil {
		fmt.Printf("error uploading to s3: %v", err)
		return err
	}

	fmt.Printf("uploaded to s3: %s", object)
	fmt.Printf("removing local files: %s", baseDir)
	// Cleanup local files
	os.RemoveAll(baseDir)
	return nil
}

func writeRTPWithSamples(w *oggwriter.OggWriter, p *rtp.Packet, samples uint64) error {
	// Use reflection to access private field
	writer := reflect.ValueOf(w).Elem()
	granuleField := writer.FieldByName("granule")
	if granuleField.IsValid() {
		current := granuleField.Uint()
		granuleField.SetUint(current + samples)
	}

	return w.WriteRTP(p)
}

// createSilencePacket creates an RTP packet containing silence for the given parameters
func createSilencePacket(payloadType uint8, seqNum uint16, timestamp uint32, ssrc uint32, marker bool) *rtp.Packet {
	return &rtp.Packet{
		Header: rtp.Header{
			Version:        2,
			PayloadType:    payloadType,
			SequenceNumber: seqNum,
			Timestamp:      timestamp,
			SSRC:           ssrc,
			Marker:         marker,
		},
		Payload: []byte{0xFC}, // Opus silence frame
	}
}
