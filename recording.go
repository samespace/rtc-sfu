package sfu

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
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
	mu               sync.Mutex
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
			mu:               sync.Mutex{},
		}

		track.OnRead(func(attrs interceptor.Attributes, pkt *rtp.Packet, q QualityLevel) {
			go func() {
				tw := session.writers[clientID][track.ID()]
				if tw == nil {
					return
				}
				tw.mu.Lock()
				defer tw.mu.Unlock()

				if session == nil {
					return
				}
				if session.paused || session.stopped {
					return
				}

				// Current time when packet arrived
				currentPacketTime := time.Now()

				// Determine the reference time for gap detection
				timeToCompareWith := tw.lastPacketTime
				if timeToCompareWith.IsZero() {
					timeToCompareWith = session.meta.StartTime
				}

				// Initialize sequence number and timestamp for first packet
				if tw.lastSeqNum == 0 {
					tw.lastSeqNum = uint16(rand.IntN(1 << 16))
					tw.lastRTPTimestamp = rand.Uint32()
				}

				// Calculate samples per packet based on clock rate (usually 960 for 20ms at 48kHz)
				samplesPerPacket := uint32(tw.clockRate * 20 / 1000) // 20ms worth of samples

				// Check for time gap (indicating mute period)
				timeDiff := currentPacketTime.Sub(timeToCompareWith)
				if timeDiff > 100*time.Millisecond {
					// Calculate number of packets needed to fill the gap
					numPackets := int(timeDiff / (20 * time.Millisecond))

					// Insert silence packets to fill the gap
					for i := 0; i < numPackets; i++ {
						tw.lastSeqNum++
						tw.lastRTPTimestamp += samplesPerPacket

						// Proper Opus silence frame (DTX - Discontinuous Transmission)
						opusSilence := []byte{0xF8, 0xFF, 0xFE} // Opus DTX frame

						silentPkt := &rtp.Packet{
							Header: rtp.Header{
								Version:        2,
								PayloadType:    111, // Opus payload type
								SequenceNumber: tw.lastSeqNum,
								Timestamp:      tw.lastRTPTimestamp,
								SSRC:           pkt.SSRC,
							},
							Payload: opusSilence,
						}

						if err := writeRTPWithSamples(tw.writer, silentPkt, uint64(samplesPerPacket)); err != nil {
							fmt.Printf("error writing silent packet: %v", err)
							return
						}
					}
				}

				// Process the actual packet
				tw.lastSeqNum++
				tw.lastRTPTimestamp += samplesPerPacket

				// Create a copy to avoid modifying the original packet
				pkt.SequenceNumber = tw.lastSeqNum
				pkt.Timestamp = tw.lastRTPTimestamp

				if err := writeRTPWithSamples(tw.writer, pkt, uint64(samplesPerPacket)); err != nil {
					fmt.Printf("error writing audio packet: %v", err)
					return
				}

				// Update the last packet time for next gap detection
				tw.lastPacketTime = currentPacketTime
			}()
		})

		fmt.Printf("added writer for client %s, track %s", clientID, track.ID())

		return nil
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
	}

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
	session.stopped = true
	session.mu.Unlock()

	// Fill any gaps from client disconnections during mute to session end
	session.mu.Lock()
	for _, writerMap := range session.writers {
		for _, tw := range writerMap {
			tw.mu.Lock()

			// Check if there's a gap between last packet and session end
			timeToCompareWith := tw.lastPacketTime
			if timeToCompareWith.IsZero() {
				timeToCompareWith = session.meta.StartTime
			}

			timeDiff := session.meta.StopTime.Sub(timeToCompareWith)
			if timeDiff > 100*time.Millisecond {
				// Calculate number of 20ms packets needed to fill the gap to session end
				numPackets := int(timeDiff / (20 * time.Millisecond))

				// Calculate samples per packet based on clock rate
				samplesPerPacket := uint32(tw.clockRate * 20 / 1000) // 20ms worth of samples

				// Insert silence packets to fill the gap to session end
				for i := 0; i < numPackets; i++ {
					tw.lastSeqNum++
					tw.lastRTPTimestamp += samplesPerPacket

					// Proper Opus silence frame (DTX - Discontinuous Transmission)
					opusSilence := []byte{0xF8, 0xFF, 0xFE} // Opus DTX frame

					silentPkt := &rtp.Packet{
						Header: rtp.Header{
							Version:        2,
							PayloadType:    111, // Opus payload type
							SequenceNumber: tw.lastSeqNum,
							Timestamp:      tw.lastRTPTimestamp,
							SSRC:           0, // Use 0 since we don't have original SSRC
						},
						Payload: opusSilence,
					}

					if err := writeRTPWithSamples(tw.writer, silentPkt, uint64(samplesPerPacket)); err != nil {
						fmt.Printf("error writing silent packet: %v", err)
						break
					}
				}
			}

			tw.mu.Unlock()
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
	go func() {
		if err := r.mergeAndUpload(session); err != nil {
			fmt.Printf("error merging and uploading: %v", err)
		}
	}()

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
