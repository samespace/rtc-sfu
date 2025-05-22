package sfu

import (
	"context"
	"encoding/binary"
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
	"gopkg.in/hraban/opus.v2"
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
	writers map[string]map[string]*pcmWriter // clientID -> trackID -> PCM writer
	mu      sync.Mutex
	paused  bool
	meta    struct {
		StartTime time.Time
		StopTime  time.Time
		Events    []Event
	}
}

// pcmWriter handles decoding Opus RTP to PCM and writing raw samples with silence insertion
type pcmWriter struct {
	file         *os.File
	decoder      *opus.Decoder
	pcmCh        chan []int16
	done         chan struct{}
	sampleRate   uint32
	channelCount uint16
}

// newPcmWriter creates a PCM writer for raw PCM output and starts its processing goroutine.
func newPcmWriter(filePath string, sampleRate uint32, channelCount uint16) (*pcmWriter, error) {
	f, err := os.Create(filePath)
	if err != nil {
		return nil, err
	}
	d, err := opus.NewDecoder(int(sampleRate), int(channelCount))
	if err != nil {
		f.Close()
		return nil, err
	}
	pw := &pcmWriter{
		file:         f,
		decoder:      d,
		pcmCh:        make(chan []int16, 128),
		done:         make(chan struct{}),
		sampleRate:   sampleRate,
		channelCount: channelCount,
	}
	go pw.start()
	return pw, nil
}

// start runs the writing loop for pcmWriter, inserting silence as needed.
func (pw *pcmWriter) start() {
	frameSamples := int(pw.sampleRate) * 20 / 1000 * int(pw.channelCount)
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	bufSamples := make([]int16, 0)
	for {
		select {
		case samples := <-pw.pcmCh:
			bufSamples = append(bufSamples, samples...)
		case <-ticker.C:
			var toWrite []int16
			if len(bufSamples) >= frameSamples {
				toWrite = bufSamples[:frameSamples]
				bufSamples = bufSamples[frameSamples:]
			} else {
				silence := make([]int16, frameSamples-len(bufSamples))
				toWrite = append(bufSamples, silence...)
				bufSamples = bufSamples[:0]
			}
			buf := make([]byte, len(toWrite)*2)
			for i, s := range toWrite {
				binary.LittleEndian.PutUint16(buf[i*2:], uint16(s))
			}
			if _, err := pw.file.Write(buf); err != nil {
				fmt.Printf("error writing PCM: %v\n", err)
			}
		case <-pw.done:
			return
		}
	}
}

// Close signals the writer to stop and closes the underlying file.
func (pw *pcmWriter) Close() error {
	close(pw.done)
	if err := pw.file.Sync(); err != nil {
		return err
	}
	return pw.file.Close()
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
		writers: make(map[string]map[string]*pcmWriter),
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
			session.writers[clientID] = make(map[string]*pcmWriter)
		}
		trackDir := filepath.Join(baseDir, clientID)
		if err := os.MkdirAll(trackDir, 0755); err != nil {
			return err
		}
		filePath := filepath.Join(trackDir, fmt.Sprintf("%s.raw", track.ID()))

		sampleRate := uint32(48000)
		channelCount := uint16(1)
		pw, err := newPcmWriter(filePath, sampleRate, channelCount)
		if err != nil {
			return err
		}
		session.writers[clientID][track.ID()] = pw

		// Decode Opus RTP packets to PCM
		track.OnRead(func(attrs interceptor.Attributes, pkt *rtp.Packet, q QualityLevel) {
			if session.paused {
				return
			}
			// Handle RED (PT 63)
			if pkt.PayloadType == 63 {
				primaryPacket, _, err := ExtractRedPackets(pkt)
				if err != nil {
					fmt.Printf("error extracting RED payload: %v\n", err)
					return
				}
				pkt = primaryPacket
			}
			// Decode to PCM and send to writer
			frameSamples := int(pw.sampleRate) * 20 / 1000 * int(pw.channelCount)
			pcmBuf := make([]int16, frameSamples)
			n, err := pw.decoder.Decode(pkt.Payload, pcmBuf)
			if err != nil {
				fmt.Printf("opus decode error: %v\n", err)
				return
			}
			pw.pcmCh <- pcmBuf[:n*int(pw.channelCount)]
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
			filesByChannel[ch] = append(filesByChannel[ch], filepath.Join(baseDir, clientID, fmt.Sprintf("%s.raw", trackID)))
		}
	}
	// Create mono mixes per channel
	monoFiles := map[ChannelType]string{}
	for _, ch := range []ChannelType{ChannelOne, ChannelTwo} {
		inputs := filesByChannel[ch]
		if len(inputs) == 0 {
			continue
		}
		// mix raw PCM inputs into a mono WAV file
		monoPath := filepath.Join(baseDir, fmt.Sprintf("mono_%d.wav", ch))
		args := []string{"-y"}
		for _, in := range inputs {
			args = append(args,
				"-f", "s16le", // raw PCM
				"-ar", "48000",
				"-ac", "1",
				"-i", in)
		}
		filter := fmt.Sprintf("amix=inputs=%d:duration=longest", len(inputs))
		args = append(args, "-filter_complex", filter, "-c:a", "pcm_s16le", "-ac", "1", monoPath)
		cmd := exec.Command("ffmpeg", args...)
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("ffmpeg mix channel %d failed: %v, output: %s", ch, err, string(out))
		}
		monoFiles[ch] = monoPath
	}
	// Merge to stereo
	finalPath := filepath.Join(baseDir, session.id+".wav")
	left, hasLeft := monoFiles[ChannelOne]
	right, hasRight := monoFiles[ChannelTwo]
	if hasLeft && hasRight {
		cmd := exec.Command("ffmpeg", "-y", "-i", left, "-i", right, "-filter_complex", "amerge=inputs=2", "-c:a", "pcm_s16le", "-ac", "2", finalPath)
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("ffmpeg merge stereo failed: %v, output: %s", err, string(out))
		}
	} else if hasLeft || hasRight {
		src := left
		if !hasLeft {
			src = right
		}
		// single channel only, rename mono WAV
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
	object := filepath.Join(session.cfg.S3.FilePrefix, session.id+".wav")
	ctx := context.Background()
	_, err = mc.FPutObject(ctx, session.cfg.S3.Bucket, object, finalPath, minio.PutObjectOptions{ContentType: "audio/wav"})
	if err != nil {
		return err
	}
	// Cleanup local files
	return os.RemoveAll(baseDir)
}
