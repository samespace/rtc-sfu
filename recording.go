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
	id      string
	cfg     RecordingConfig
	writers map[string]map[string]*trackWriter // clientID -> trackID -> writer
	mu      sync.Mutex
	paused  bool
	meta    struct {
		StartTime time.Time
		StopTime  time.Time
		Events    []Event
	}
}

type trackWriter struct {
	writer            *oggwriter.OggWriter
	lastWallClockTime time.Time // Tracks wall clock time of last processed packet
	lastRTPTimestamp  uint32
	lastSeqNum        uint16
	clockRate         uint32
	totalSamples      uint64
	ssrc              webrtc.SSRC        // SSRC of the track
	payloadType       webrtc.PayloadType // PayloadType of the track
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
		writers: make(map[string]map[string]*trackWriter),
	}
	session.meta.StartTime = time.Now()

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
		var trackSSRC webrtc.SSRC

		switch t := track.(type) {
		case *Track:
			codecParams = t.base.codec
			trackSSRC = t.SSRC()
		case *AudioTrack:
			codecParams = t.Track.base.codec
			trackSSRC = t.Track.SSRC()
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
			writer:            ow,
			clockRate:         sampleRate,
			lastWallClockTime: time.Time{}, // Explicitly initialize to zero value
			lastRTPTimestamp:  0,
			lastSeqNum:        0,
			totalSamples:      0,
			ssrc:              trackSSRC,
			payloadType:       codecParams.PayloadType,
		}
		track.OnRead(func(attrs interceptor.Attributes, pkt *rtp.Packet, q QualityLevel) {
			if session.paused {
				return
			}

			session.mu.Lock()
			defer session.mu.Unlock()

			const samplesPerPacket = 960 // 48000Hz * 0.02s (standard Opus frame size)

			// tw might be nil if the track was added *after* recording started and before this callback runs
			// or if the client left and the track is being removed.
			tw, ok := session.writers[clientID][track.ID()]
			if !ok {
				fmt.Printf("trackWriter not found for client %s, track %s. Skipping packet.", clientID, track.ID())
				return
			}

			now := time.Now()
			var samplesForPacket uint64

			// Initialize lastWallClockTime if this is the first packet for this track
			if tw.lastWallClockTime.IsZero() {
				// If it's the very first packet, we can't determine duration from wall clock yet.
				// We'll assume a standard packet duration for the first one, or base it on RTP if available.
				// For subsequent packets, wall clock will be the primary driver.
				if tw.lastRTPTimestamp != 0 && pkt.Timestamp > tw.lastRTPTimestamp {
					samplesForPacket = uint64(pkt.Timestamp - tw.lastRTPTimestamp)
				} else {
					samplesForPacket = uint64(tw.clockRate / 50) // Default to 20ms for opus
				}
				// Also, if recording started *after* this participant joined and sent packets,
				// we might need to backfill silence from session.meta.StartTime
				if session.meta.StartTime.Before(now) {
					durationFromStart := now.Sub(session.meta.StartTime)
					requiredSamplesFromStart := uint64(durationFromStart.Seconds() * float64(tw.clockRate))
					if requiredSamplesFromStart > tw.totalSamples && requiredSamplesFromStart > samplesForPacket {
						// Only do this if it's a significant gap, suggesting late start of recording for this track
						paddingNeeded := requiredSamplesFromStart - samplesForPacket // samplesForPacket is for the current actual packet
						packetsToPad := int(paddingNeeded / samplesPerPacket)
						if paddingNeeded%samplesPerPacket > 0 {
							packetsToPad++
						}
						fmt.Printf("Backfilling %d silent packets for track %s from recording start\n", packetsToPad, track.ID())
						for i := 0; i < packetsToPad; i++ {
							silentPkt := &rtp.Packet{
								Header: rtp.Header{
									Version:        2,
									PayloadType:    uint8(tw.payloadType),
									SequenceNumber: tw.lastSeqNum + 1,
									Timestamp:      tw.lastRTPTimestamp + samplesPerPacket, // Advance RTP timestamp for silent packets
									SSRC:           uint32(tw.ssrc),
								},
								Payload: []byte{0xFC}, // Opus silence frame
							}
							if err := writeRTPWithSamples(tw.writer, silentPkt, samplesPerPacket); err != nil {
								fmt.Printf("error writing initial silent packet for track %s: %v\n", track.ID(), err)
								continue
							}
							tw.totalSamples += samplesPerPacket
							tw.lastSeqNum++
							tw.lastRTPTimestamp += samplesPerPacket
						}
					}
				}
			} else {
				// Calculate duration based on wall clock time for subsequent packets
				elapsedWallClock := now.Sub(tw.lastWallClockTime)
				samplesForPacket = uint64(elapsedWallClock.Seconds() * float64(tw.clockRate))
			}

			// if samplesForPacket is 0, it means this packet arrived too close to the previous one.
			// Give it a very small nominal duration to ensure it's written and OGG advances.
			if samplesForPacket == 0 {
				samplesForPacket = 1 // Minimal advancement
			}

			// Cap max duration to prevent huge jumps if there was a long pause in receiving packets (e.g. system sleep)
			// Max 1 second of samples per packet processing step
			maxSamplesForStep := uint64(tw.clockRate)
			if samplesForPacket > maxSamplesForStep {
				samplesForPacket = maxSamplesForStep
			}

			// --- RTP Packet Loss Gap Filling (existing logic, slightly adapted) ---
			if tw.lastRTPTimestamp != 0 && pkt.Timestamp > tw.lastRTPTimestamp {
				diffRTP := pkt.Timestamp - tw.lastRTPTimestamp
				if diffRTP > samplesPerPacket { // A gap in RTP timestamps detected
					missingPackets := int((diffRTP / samplesPerPacket) - 1)
					if missingPackets > 0 {
						// Cap at 1000 packets to prevent runaway (20s max gap)
						if missingPackets > 1000 {
							missingPackets = 1000
						}
						fmt.Printf("RTP Gap: Filling %d silent packets for track %s between RTP %d and %d\n", missingPackets, track.ID(), tw.lastRTPTimestamp, pkt.Timestamp)
						for i := 0; i < missingPackets; i++ {
							silentPkt := &rtp.Packet{
								Header: rtp.Header{
									Version:        2,
									PayloadType:    uint8(tw.payloadType), // Use stored payload type
									SequenceNumber: tw.lastSeqNum + 1,
									Timestamp:      tw.lastRTPTimestamp + samplesPerPacket,
									SSRC:           uint32(tw.ssrc), // Use stored SSRC
								},
								Payload: []byte{0xFC}, // Opus silence frame
							}
							// Write with standard sample count for these gap-filling packets
							if err := writeRTPWithSamples(tw.writer, silentPkt, samplesPerPacket); err != nil {
								fmt.Printf("error writing RTP-gap silent packet for track %s: %v\n", track.ID(), err)
								continue // Or break, depending on desired error handling
							}
							tw.totalSamples += samplesPerPacket
							tw.lastSeqNum++
							tw.lastRTPTimestamp += samplesPerPacket
						}
					}
				}
			}

			// --- End RTP Packet Loss Gap Filling ---

			// Write the actual incoming packet
			// The samplesForPacket here is primarily driven by wall-clock now.
			if err := writeRTPWithSamples(tw.writer, pkt, samplesForPacket); err != nil {
				fmt.Printf("error writing packet for track %s: %v\n", track.ID(), err)
			} else {
				tw.totalSamples += samplesForPacket
			}

			// Update an internal lastRTPTimestamp, but prioritize wall-clock for ogg advancement
			// This helps the RTP Gap Filler logic work correctly if actual RTP timestamps jump.
			if pkt.Timestamp > tw.lastRTPTimestamp { // Only update if it's forward; handles out-of-order or static timestamps
				tw.lastRTPTimestamp = pkt.Timestamp
			}
			tw.lastSeqNum = pkt.SequenceNumber
			tw.lastWallClockTime = now // Update wall clock time *after* processing
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
	if session == nil {
		r.recordingMu.Unlock()
		return fmt.Errorf("no recording in progress")
	}
	r.recordingMu.Unlock() // Release room lock early, session lock will protect session data

	session.mu.Lock()
	session.meta.StopTime = time.Now()

	const samplesPerPacket = 960 // Standard Opus frame size, 48000Hz * 0.02s

	// Pad tracks with silence until StopTime and close writers
	for clientID, clientWriters := range session.writers {
		for trackID, tw := range clientWriters {
			if !tw.lastWallClockTime.IsZero() && tw.lastWallClockTime.Before(session.meta.StopTime) {
				durationToPad := session.meta.StopTime.Sub(tw.lastWallClockTime)
				samplesToPad := uint64(durationToPad.Seconds() * float64(tw.clockRate))

				if samplesToPad > 0 {
					numPacketsToPad := int(samplesToPad / samplesPerPacket)
					if samplesToPad%samplesPerPacket != 0 {
						numPacketsToPad++
					}

					fmt.Printf("Padding track %s/%s with %d silent packets for %v\n", clientID, trackID, numPacketsToPad, durationToPad)

					for i := 0; i < numPacketsToPad; i++ {
						// Determine samples for this specific padding packet
						currentPacketSamples := uint64(samplesPerPacket)
						if i == numPacketsToPad-1 { // Last packet might be partial
							if samplesToPad%samplesPerPacket != 0 {
								currentPacketSamples = samplesToPad % samplesPerPacket
							}
						}
						if currentPacketSamples == 0 { // Avoid writing zero-sample packets
							currentPacketSamples = 1
						}

						silentPkt := &rtp.Packet{
							Header: rtp.Header{
								Version:        2,
								PayloadType:    uint8(tw.payloadType),
								SequenceNumber: tw.lastSeqNum + 1,
								Timestamp:      tw.lastRTPTimestamp + uint32(samplesPerPacket), // Advance RTP timestamp consistently
								SSRC:           uint32(tw.ssrc),
							},
							Payload: []byte{0xFC}, // Opus silence frame
						}
						if err := writeRTPWithSamples(tw.writer, silentPkt, currentPacketSamples); err != nil {
							fmt.Printf("error writing end-of-recording silent packet for track %s/%s: %v\n", clientID, trackID, err)
							// Continue trying to close, etc.
						}
						tw.lastSeqNum++
						tw.lastRTPTimestamp += uint32(samplesPerPacket) // Consistent RTP advancement
						tw.totalSamples += currentPacketSamples
					}
				}
			}
			if err := tw.writer.Close(); err != nil {
				fmt.Printf("Error closing writer for track %s/%s: %v\n", clientID, trackID, err)
			}
		}
	}

	// Prepare data for mergeAndUpload under lock
	sessionID := session.id
	sessionCfg := session.cfg
	sessionMeta := session.meta // Value copy

	// Create a deep copy of the writers map for file paths because session.writers itself should not be used after unlock
	// We only need the file paths generated from clientID and trackID, and channel mapping.
	// The actual file paths are reconstructed in doMergeAndUpload based on this minimal info.
	writerInfo := make(map[string]map[string]struct{})
	for cID, tracks := range session.writers {
		writerInfo[cID] = make(map[string]struct{})
		for tID := range tracks {
			writerInfo[cID][tID] = struct{}{}
		}
	}

	session.mu.Unlock() // Unlock session before long-running tasks

	// Write meta.json (can be done after lock release as sessionMeta is a copy)
	metaFile := filepath.Join(sessionCfg.BasePath, sessionID, "meta.json")
	f, err := os.Create(metaFile)
	if err != nil {
		// Log error but proceed to attempt merge/upload if possible
		fmt.Printf("Error creating meta.json file %s: %v\n", metaFile, err)
	} else {
		enc := json.NewEncoder(f)
		enc.SetIndent("", "  ")
		if err := enc.Encode(sessionMeta); err != nil {
			// Log error but proceed
			fmt.Printf("Error encoding meta.json for session %s: %v\n", sessionID, err)
		}
		f.Close() // Close immediately after writing
	}

	// Perform merge and upload
	if err := r.doMergeAndUpload(sessionID, sessionCfg, writerInfo); err != nil {
		// Log error, but recording session is considered stopped locally.
		// Error handling for upload failures might involve retries or manual intervention.
		fmt.Printf("Error during merge and upload for session %s: %v\n", sessionID, err)
		// Even with merge/upload error, we should clear the session from the room
	}

	r.recordingMu.Lock()
	if r.recordingSession != nil && r.recordingSession.id == sessionID {
		r.recordingSession = nil
	}
	r.recordingMu.Unlock()

	return nil
}

// doMergeAndUpload mixes per-channel recordings, merges stereo, uploads to S3, and removes local files.
// It operates on data passed to it, not directly on a recordingSession, to avoid long lock holds.
func (r *Room) doMergeAndUpload(sessionID string, cfg RecordingConfig, writerInfo map[string]map[string]struct{}) error {
	baseDir := filepath.Join(cfg.BasePath, sessionID)

	// Reconstruct file paths by channel from writerInfo
	filesByChannel := make(map[ChannelType][]string)
	for clientID, tracks := range writerInfo {
		ch := cfg.ChannelMapping[clientID]
		if ch == ChannelUnknown { // Should not happen if addWriter was successful
			fmt.Printf("Warning: clientID %s found in writerInfo but not in ChannelMapping during merge/upload for session %s\n", clientID, sessionID)
			continue
		}
		for trackID := range tracks {
			// Ensure the directory for the track exists (it should, but good to be safe)
			// trackDir := filepath.Join(baseDir, clientID) // Not strictly needed here as we only form path
			filePath := filepath.Join(baseDir, clientID, fmt.Sprintf("%s.ogg", trackID))
			filesByChannel[ch] = append(filesByChannel[ch], filePath)
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
			// If only one file, check if it exists before trying to copy/rename
			src := inputs[0]
			if _, err := os.Stat(src); os.IsNotExist(err) {
				return fmt.Errorf("source file %s for channel %d does not exist for session %s", src, ch, sessionID)
			}
			// Optimization: if it's the only file, try to rename it directly to monoPath if possible, else copy.
			// For simplicity here, we'll copy, but rename is more efficient.
			// Let's stick to copy for now to avoid complexity if src is also the target of a later S3 upload.
			inF, err := os.Open(src)
			if err != nil {
				return fmt.Errorf("open source file %s for channel %d failed for session %s: %v", src, ch, sessionID, err)
			}
			defer inF.Close()
			outF, err := os.Create(monoPath)
			if err != nil {
				inF.Close() // ensure closed on error
				return fmt.Errorf("create mono file %s for channel %d failed for session %s: %v", monoPath, ch, sessionID, err)
			}
			defer outF.Close()
			if _, err := io.Copy(outF, inF); err != nil {
				// Close both on error here as well.
				return fmt.Errorf("copy file for channel %d failed for session %s: %v", ch, sessionID, err)
			}
			monoFiles[ch] = monoPath
			continue
		}
		// Check existence of all input files before calling ffmpeg
		for _, in := range inputs {
			if _, err := os.Stat(in); os.IsNotExist(err) {
				return fmt.Errorf("ffmpeg input file %s for channel %d does not exist for session %s", in, ch, sessionID)
			}
		}
		args := []string{"-y"}
		for _, in := range inputs {
			args = append(args, "-i", in)
		}
		filter := fmt.Sprintf("amix=inputs=%d:duration=longest", len(inputs))
		args = append(args, "-filter_complex", filter, "-ac", "1", monoPath)
		cmd := exec.Command("ffmpeg", args...)
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("ffmpeg mix channel %d failed for session %s: %v, output: %s", ch, sessionID, err, string(out))
		}
		monoFiles[ch] = monoPath
	}

	// Merge to stereo
	finalPath := filepath.Join(baseDir, sessionID+".ogg")
	left, hasLeft := monoFiles[ChannelOne]
	right, hasRight := monoFiles[ChannelTwo]

	if hasLeft && hasRight {
		// Ensure mono files exist before merging
		if _, err := os.Stat(left); os.IsNotExist(err) {
			return fmt.Errorf("left mono file %s does not exist for stereo merge for session %s", left, sessionID)
		}
		if _, err := os.Stat(right); os.IsNotExist(err) {
			return fmt.Errorf("right mono file %s does not exist for stereo merge for session %s", right, sessionID)
		}
		cmd := exec.Command("ffmpeg", "-y", "-i", left, "-i", right, "-filter_complex", "amerge=inputs=2", "-ac", "2", finalPath)
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("ffmpeg merge stereo failed for session %s: %v, output: %s", sessionID, err, string(out))
		}
	} else if hasLeft || hasRight {
		src := left
		if !hasLeft {
			src = right
		}
		// Ensure mono file exists before renaming
		if _, err := os.Stat(src); os.IsNotExist(err) {
			return fmt.Errorf("source mono file %s does not exist for final rename for session %s", src, sessionID)
		}
		if err := os.Rename(src, finalPath); err != nil {
			return fmt.Errorf("rename mono to final failed for session %s: %v", sessionID, err)
		}
	} else {
		// If no mono files were created (e.g. no clients ever sent audio on any mapped channel),
		// it's not an error *per se*, but there will be no final OGG.
		// We can create an empty OGG or just log and skip S3 upload.
		// For now, let's treat it as "nothing to upload".
		fmt.Printf("No audio data to merge for session %s. Skipping S3 upload.\n", sessionID)
		// Cleanup local base directory if it exists
		if _, err := os.Stat(baseDir); !os.IsNotExist(err) {
			fmt.Printf("Removing local base directory (no audio merged): %s for session %s\n", baseDir, sessionID)
			os.RemoveAll(baseDir)
		}
		return nil // Not an error, just nothing was produced.
	}

	// Ensure final file exists before uploading
	if _, err := os.Stat(finalPath); os.IsNotExist(err) {
		return fmt.Errorf("final merged file %s does not exist before S3 upload for session %s", finalPath, sessionID)
	}

	// Upload to S3
	mc, err := minio.New(cfg.S3.Endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(cfg.S3.AccessKey, cfg.S3.SecretKey, ""),
		Secure: cfg.S3.Secure,
	})
	if err != nil {
		return fmt.Errorf("error creating minio client for session %s: %v", sessionID, err)
	}

	objectName := filepath.Join(cfg.S3.FilePrefix, sessionID+".ogg")
	ctx := context.Background()
	_, err = mc.FPutObject(ctx, cfg.S3.Bucket, objectName, finalPath, minio.PutObjectOptions{ContentType: "audio/ogg"})
	if err != nil {
		return fmt.Errorf("error uploading %s to s3 (bucket: %s, object: %s) for session %s: %v", finalPath, cfg.S3.Bucket, objectName, sessionID, err)
	}

	fmt.Printf("Uploaded to s3: %s for session %s\n", objectName, sessionID)

	// Cleanup local files
	fmt.Printf("Removing local files at: %s for session %s\n", baseDir, sessionID)
	if err := os.RemoveAll(baseDir); err != nil {
		// Log this error, but the primary operation (upload) was successful.
		fmt.Printf("Error removing local directory %s for session %s: %v\n", baseDir, sessionID, err)
	}
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
