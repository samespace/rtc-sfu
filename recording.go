package sfu

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/pion/interceptor"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
)

// RecordingOptions configures a recording session.
type RecordingOptions struct {
	RecordingID string    // Unique identifier for this recording
	BasePath    string    // Local base path to store recordings
	S3Config    *S3Config // Optional S3 configuration for upload
}

// S3Config holds AWS S3 upload parameters.
type S3Config struct {
	Secure     bool   // true for https, false for http
	URL        string // S3 endpoint URL
	AccessKey  string
	SecretKey  string
	Bucket     string // S3 bucket name
	PrefixPath string // Key prefix in bucket
}

// Recorder manages per-room audio recording sessions.
type Recorder struct {
	options      RecordingOptions
	ctx          context.Context
	cancel       context.CancelFunc
	mu           sync.RWMutex
	participants map[string]*participantRecorder
	paused       bool
}

// participantRecorder stores per-participant recording data.
type participantRecorder struct {
	id     string
	meta   ParticipantMeta
	tracks map[string]*trackRecorder
}

// ParticipantMeta records join/leave times for a participant.
type ParticipantMeta struct {
	ID        string
	JoinedAt  time.Time
	StoppedAt time.Time
}

// trackRecorder writes RTP packet payloads for a single track to a raw Opus file.
type trackRecorder struct {
	id       string
	file     *os.File
	joinedAt time.Time
}

// newRecorder initializes a Recorder for a given Room and options.
func newRecorder(room *Room, opts RecordingOptions) *Recorder {
	ctx, cancel := context.WithCancel(room.context)
	rec := &Recorder{
		options:      opts,
		ctx:          ctx,
		cancel:       cancel,
		participants: make(map[string]*participantRecorder),
	}
	// subscribe to future client join/leave events
	room.OnClientJoined(func(client *Client) {
		rec.addParticipant(client)
	})
	room.OnClientLeft(func(client *Client) {
		rec.removeParticipant(client)
	})
	// add existing participants
	for _, client := range room.SFU().GetClients() {
		rec.addParticipant(client)
	}
	return rec
}

// start is a no-op placeholder; recording begins automatically on track add.
func (r *Recorder) start() { /* no-op */ }

// Pause temporarily stops writing packets to files.
func (r *Recorder) Pause() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.paused = true
}

// Resume re-enables writing packets to files.
func (r *Recorder) Resume() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.paused = false
}

// Stop finalizes all files, merges and optionally uploads to S3.
func (r *Recorder) Stop() error {
	r.cancel()
	// close all track files
	r.mu.RLock()
	for _, p := range r.participants {
		for _, tr := range p.tracks {
			tr.file.Close()
		}
	}
	r.mu.RUnlock()
	// merge into single file
	if err := r.merge(); err != nil {
		return err
	}
	// upload if configured
	if r.options.S3Config != nil {
		return r.uploadToMinio()
	}
	return nil
}

// addParticipant sets up recording for all audio tracks of a new participant.
func (r *Recorder) addParticipant(client *Client) {
	id := client.ID()
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.participants[id]; exists {
		return
	}
	p := &participantRecorder{
		id:     id,
		meta:   ParticipantMeta{ID: id, JoinedAt: time.Now()},
		tracks: make(map[string]*trackRecorder),
	}
	r.participants[id] = p
	// record any existing audio tracks
	for _, t := range client.PublishedTracks() {
		if t.Kind() == webrtc.RTPCodecTypeAudio {
			r.addTrack(p, t)
		}
	}
	// listen for future tracks
	client.OnTracksAdded(func(tracks []ITrack) {
		for _, t := range tracks {
			if t.Kind() == webrtc.RTPCodecTypeAudio {
				r.addTrack(p, t)
			}
		}
	})
}

// removeParticipant finalizes files when a participant leaves.
func (r *Recorder) removeParticipant(client *Client) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if p, exists := r.participants[client.ID()]; exists {
		p.meta.StoppedAt = time.Now()
		for _, tr := range p.tracks {
			tr.file.Close()
		}
		// write participant metadata
		metaPath := filepath.Join(r.options.BasePath, r.options.RecordingID, p.id, "meta.json")
		m, _ := os.Create(metaPath)
		json.NewEncoder(m).Encode(p.meta)
		m.Close()
	}
}

// addTrack creates an Opus file for a track and hooks packet writes.
func (r *Recorder) addTrack(p *participantRecorder, track ITrack) {
	trackID := track.ID()
	baseDir := filepath.Join(r.options.BasePath, r.options.RecordingID, p.id)
	os.MkdirAll(baseDir, os.ModePerm)
	filePath := filepath.Join(baseDir, fmt.Sprintf("%s.opus", trackID))
	f, err := os.Create(filePath)
	if err != nil {
		return
	}
	tr := &trackRecorder{id: trackID, file: f, joinedAt: time.Now()}
	p.tracks[trackID] = tr
	track.OnRead(func(attrs interceptor.Attributes, pkt *rtp.Packet, _ QualityLevel) {
		r.mu.RLock()
		paused := r.paused
		r.mu.RUnlock()
		if paused {
			return
		}
		tr.file.Write(pkt.Payload)
	})
}

// merge combines all participant Opus files into a single file using ffmpeg.
func (r *Recorder) merge() error {
	output := filepath.Join(r.options.BasePath, r.options.RecordingID, fmt.Sprintf("%s_merged.ogg", r.options.RecordingID))
	// collect input files
	inputs := []string{}
	for _, p := range r.participants {
		for _, tr := range p.tracks {
			inputs = append(inputs, tr.file.Name())
		}
	}
	if len(inputs) == 0 {
		return fmt.Errorf("no tracks to merge")
	}
	// build ffmpeg command
	args := []string{"-y"}
	for _, in := range inputs {
		args = append(args, "-f", "opus", "-i", in)
	}
	if len(inputs) == 1 {
		args = append(args, "-c:a", "libopus", output)
	} else {
		var filter strings.Builder
		for i := range inputs {
			filter.WriteString(fmt.Sprintf("[%d:a]", i))
		}
		filter.WriteString(fmt.Sprintf("amerge=inputs=%d[a]", len(inputs)))
		args = append(args, "-filter_complex", filter.String(), "-map", "[a]", "-c:a", "libopus", output)
	}
	cmd := exec.Command("ffmpeg", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// uploadToMinio sends the merged file to a MinIO/S3-compatible storage.
func (r *Recorder) uploadToMinio() error {
	client, err := minio.New(r.options.S3Config.URL, &minio.Options{
		Creds:  credentials.NewStaticV4(r.options.S3Config.AccessKey, r.options.S3Config.SecretKey, ""),
		Secure: r.options.S3Config.Secure,
	})
	if err != nil {
		return err
	}
	mergedPath := filepath.Join(r.options.BasePath, r.options.RecordingID, fmt.Sprintf("%s_merged.ogg", r.options.RecordingID))
	objectName := filepath.Join(r.options.S3Config.PrefixPath, fmt.Sprintf("%s_merged.ogg", r.options.RecordingID))
	_, err = client.FPutObject(context.Background(), r.options.S3Config.Bucket, objectName, mergedPath, minio.PutObjectOptions{ContentType: "audio/ogg"})
	return err
}
