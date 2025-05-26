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
	stopped bool
	meta    struct {
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
	lastPayloadType  uint8
	lastSSRC         uint32
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
			writer:           ow,
			clockRate:        sampleRate, // 48000 for Opus
			lastPacketTime:   time.Time{},
			lastRTPTimestamp: 0,
			lastSeqNum:       0,
		}
		track.OnRead(func(attrs interceptor.Attributes, pkt *rtp.Packet, q QualityLevel) {
			session.mu.Lock()
			defer session.mu.Unlock()

			if session.paused || session.stopped {
				return
			}

			if pkt.SequenceNumber == 0 {
				pkt.Marker = true
			}

			tw := session.writers[clientID][track.ID()]
			const samplesPerPacket = 960 // 48000Hz * 0.02s
			currentTime := time.Now()

			// Handle time-based gaps (mute/unmute scenarios)
			if !tw.lastPacketTime.IsZero() {
				timeDiff := currentTime.Sub(tw.lastPacketTime)
				// If more than 100ms gap in real time, fill with silence
				if timeDiff > 100*time.Millisecond {
					samplesNeeded := uint64(float64(tw.clockRate) * timeDiff.Seconds())
					if samplesNeeded > 0 {
						packetsNeeded := (samplesNeeded + uint64(samplesPerPacket) - 1) / uint64(samplesPerPacket)

						// Cap at 1000 packets to prevent runaway (20s max gap)
						if packetsNeeded > 1000 {
							packetsNeeded = 1000
						}

						for i := uint64(0); i < packetsNeeded; i++ {
							silentPkt := &rtp.Packet{
								Header: rtp.Header{
									Version:        2,
									PayloadType:    tw.lastPayloadType,
									SequenceNumber: tw.lastSeqNum + 1 + uint16(i),
									Timestamp:      tw.lastRTPTimestamp + samplesPerPacket*(uint32(i)+1),
									SSRC:           tw.lastSSRC,
								},
								Payload: []byte{0xFC},
							}

							if err := writeRTPWithSamples(tw.writer, silentPkt, uint64(samplesPerPacket)); err != nil {
								fmt.Printf("error writing time-gap silence packet: %v", err)
								continue
							}

							tw.totalSamples += uint64(samplesPerPacket)
							tw.lastSeqNum = silentPkt.SequenceNumber
							tw.lastRTPTimestamp = silentPkt.Timestamp
						}
					}
				}
			}

			// Handle RTP timestamp-based gaps (network issues, packet loss)
			if tw.lastRTPTimestamp != 0 {
				// Calculate missing packets based on RTP timestamp difference
				diff := pkt.Timestamp - tw.lastRTPTimestamp
				if diff > samplesPerPacket {
					missingPackets := int((diff / samplesPerPacket) - 1)

					// Cap at 1000 packets to prevent runaway (20s max gap)
					if missingPackets > 1000 {
						missingPackets = 1000
					}

					for i := 0; i < missingPackets; i++ {
						silentPkt := &rtp.Packet{
							Header: rtp.Header{
								Version:        2,
								PayloadType:    pkt.PayloadType,
								SequenceNumber: tw.lastSeqNum + 1,
								Timestamp:      tw.lastRTPTimestamp + samplesPerPacket,
								SSRC:           pkt.SSRC,
							},
							Payload: []byte{0xFC},
						}

						// Write with explicit sample count
						if err := writeRTPWithSamples(tw.writer, silentPkt, samplesPerPacket); err != nil {
							fmt.Printf("error writing rtp-gap silence packet: %v", err)
							continue
						}

						tw.totalSamples += uint64(samplesPerPacket)
						tw.lastSeqNum++
						tw.lastRTPTimestamp += samplesPerPacket
					}
				}
			}

			// Write actual packet with its sample count
			actualSamples := uint64(pkt.Timestamp - tw.lastRTPTimestamp)
			if actualSamples > 0 {
				if err := writeRTPWithSamples(tw.writer, pkt, actualSamples); err != nil {
					fmt.Printf("error writing packet: %v", err)
					return
				}
				tw.totalSamples += actualSamples
			}

			tw.lastSeqNum = pkt.SequenceNumber
			tw.lastRTPTimestamp = pkt.Timestamp
			tw.lastPayloadType = pkt.PayloadType
			tw.lastSSRC = pkt.SSRC
			tw.lastPacketTime = currentTime
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
	// Don't set stopped = true yet, to allow final packets to be processed
	session.mu.Unlock()

	// Add final silence to fill gap between last packet and stop time
	for _, tracks := range session.writers {
		for _, tw := range tracks {
			if tw.lastPacketTime.IsZero() {
				continue
			}

			duration := session.meta.StopTime.Sub(tw.lastPacketTime)
			if duration <= 0 {
				continue
			}

			samplesNeeded := uint64(float64(tw.clockRate) * duration.Seconds())
			if samplesNeeded == 0 {
				continue
			}

			samplesPerPacket := uint32(960)
			packetsNeeded := (samplesNeeded + uint64(samplesPerPacket) - 1) / uint64(samplesPerPacket)
			if packetsNeeded > 1000 {
				packetsNeeded = 1000
			}

			for i := uint64(0); i < packetsNeeded; i++ {
				silentPkt := &rtp.Packet{
					Header: rtp.Header{
						Version:        2,
						PayloadType:    tw.lastPayloadType,
						SequenceNumber: tw.lastSeqNum + 1 + uint16(i),
						Timestamp:      tw.lastRTPTimestamp + samplesPerPacket*(uint32(i)+1),
						SSRC:           tw.lastSSRC,
					},
					Payload: []byte{0xFC},
				}

				if err := writeRTPWithSamples(tw.writer, silentPkt, uint64(samplesPerPacket)); err != nil {
					fmt.Printf("error writing final silence packet: %v", err)
					continue
				}

				tw.lastSeqNum = silentPkt.SequenceNumber
				tw.lastRTPTimestamp = silentPkt.Timestamp
				tw.totalSamples += uint64(samplesPerPacket)
			}
		}
	}

	// Now set stopped = true to prevent any further packet processing
	session.mu.Lock()
	session.stopped = true
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
