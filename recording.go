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
	ow          *oggwriter.OggWriter
	packetChan  chan *rtp.Packet
	done        chan struct{}
	initialTS   uint32
	firstPacket time.Time
	lastTS      uint32
	lastSeq     uint16
	ssrc        uint32
	mu          sync.Mutex
}

func (tw *trackWriter) process() {
	defer close(tw.done)
	// defer tw.ow.Close()

	for pkt := range tw.packetChan {
		tw.mu.Lock()
		if tw.initialTS == 0 {
			tw.initialTS = pkt.Timestamp
			tw.firstPacket = time.Now()
			tw.ssrc = pkt.SSRC
			tw.lastTS = pkt.Timestamp
			tw.lastSeq = pkt.SequenceNumber
			tw.ow.WriteRTP(pkt)
			tw.mu.Unlock()
			continue
		}

		currentTS := pkt.Timestamp
		delta := currentTS - tw.lastTS
		numFrames := delta / 960

		if numFrames > 1 {
			numSilence := numFrames - 1
			for i := uint32(0); i < numSilence; i++ {
				silenceTS := tw.lastTS + 960*(i+1)
				silenceSeq := tw.lastSeq + uint16(i+1)
				silencePkt := &rtp.Packet{
					Header: rtp.Header{
						Version:        2,
						PayloadType:    111,
						Timestamp:      silenceTS,
						SequenceNumber: silenceSeq,
						SSRC:           tw.ssrc,
					},
					Payload: []byte{0xFC}, // Opus silence payload
				}
				if err := tw.ow.WriteRTP(silencePkt); err != nil {
					fmt.Printf("error writing silence packet: %v", err)
				}
			}
		}

		if err := tw.ow.WriteRTP(pkt); err != nil {
			fmt.Printf("error writing packet: %v", err)
		}

		tw.lastTS = currentTS
		tw.lastSeq = pkt.SequenceNumber
		tw.mu.Unlock()
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
		ow, err := oggwriter.New(filePath, 48000, 1)
		if err != nil {
			return err
		}

		tw := &trackWriter{
			ow:         ow,
			packetChan: make(chan *rtp.Packet, 100),
			done:       make(chan struct{}),
		}

		// Start processing goroutine
		go tw.process()

		session.writers[clientID][track.ID()] = tw

		track.OnRead(func(attrs interceptor.Attributes, pkt *rtp.Packet, q QualityLevel) {
			if session.paused {
				return
			}

			select {
			case tw.packetChan <- pkt:
			default:
				fmt.Println("packet buffer full, dropping packet")
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
	session.mu.Unlock()

	// Close writers and process remaining silence
	for _, trackMap := range session.writers {
		for _, tw := range trackMap {
			close(tw.packetChan)
			<-tw.done // Wait for processing to finish

			// Fill remaining silence up to stop time
			duration := session.meta.StopTime.Sub(tw.firstPacket).Seconds()
			expectedSamples := uint32(duration * 48000)
			generatedSamples := tw.lastTS - tw.initialTS

			if expectedSamples > generatedSamples {
				remaining := expectedSamples - generatedSamples
				numFrames := remaining / 960
				tw.mu.Lock()
				for i := uint32(0); i < numFrames; i++ {
					silenceTS := tw.lastTS + 960*(i+1)
					silenceSeq := tw.lastSeq + uint16(i+1)
					silencePkt := &rtp.Packet{
						Header: rtp.Header{
							Version:        2,
							PayloadType:    111,
							Timestamp:      silenceTS,
							SequenceNumber: silenceSeq,
							SSRC:           tw.ssrc,
						},
						Payload: []byte{0xFC},
					}
					if err := tw.ow.WriteRTP(silencePkt); err != nil {
						fmt.Printf("error writing final silence: %v", err)
					}
				}
				tw.ow.Close()
				tw.mu.Unlock()
			}
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
