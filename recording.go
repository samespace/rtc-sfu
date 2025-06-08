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

const (
	silencePacketDetectionThreshold = 500 * time.Millisecond
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

// bufferedPacket holds an RTP packet along with its arrival time
type bufferedPacket struct {
	packet      *rtp.Packet
	arrivalTime time.Time
}

type trackWriter struct {
	writer                       *oggwriter.OggWriter
	lastRTPTimestamp             uint32
	actualPacketLastRTPTimestamp uint32
	lastSeqNum                   uint16
	clockRate                    uint32
	lastPacketTime               time.Time
	mu                           sync.Mutex

	// Buffering fields
	packetBuffer       chan bufferedPacket
	stopChan           chan struct{}
	wg                 sync.WaitGroup
	ssrc               uint32
	firstPacket        bool
	recordingStartTime time.Time
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

		// Create trackWriter with buffering
		tw := &trackWriter{
			writer:             ow,
			clockRate:          sampleRate,
			lastRTPTimestamp:   0,
			lastSeqNum:         0,
			firstPacket:        true,
			packetBuffer:       make(chan bufferedPacket, 1000), // Buffer up to 1000 packets
			stopChan:           make(chan struct{}),
			recordingStartTime: session.meta.StartTime,
		}

		session.writers[clientID][track.ID()] = tw

		// Start the packet processor goroutine
		tw.wg.Add(1)
		go tw.processPackets()

		track.OnRead(func(attrs interceptor.Attributes, pkt *rtp.Packet, q QualityLevel) {
			if session.paused || session.stopped {
				return
			}

			// Buffer the packet with its arrival time
			select {
			case tw.packetBuffer <- bufferedPacket{
				packet:      pkt.Clone(),
				arrivalTime: time.Now(),
			}:
			default:
				// Buffer full, drop packet
				fmt.Printf("packet buffer full for client %s, track %s, dropping packet", clientID, track.ID())
			}
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
		client.OnTracksReady(func(tracks []ITrack) {
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

		// add a hook for add track too
		c.OnTracksReady(func(tracks []ITrack) {
			for _, track := range tracks {
				if track.Kind() == webrtc.RTPCodecTypeAudio {
					_ = addWriter(c.ID(), track)
				}
			}
		})
	})

	r.recordingSession = session
	return id, nil
}

// processPackets processes buffered packets in a separate goroutine
func (tw *trackWriter) processPackets() {
	defer tw.wg.Done()

	ticker := time.NewTicker(100 * time.Millisecond) // Process buffer every 100ms
	defer ticker.Stop()

	packetBatch := make([]bufferedPacket, 0, 100)

	for {
		select {
		case <-tw.stopChan:
			// Process any remaining packets
			tw.drainBuffer(packetBatch)
			return

		case <-ticker.C:
			// Collect packets from buffer
			packetBatch = packetBatch[:0]
		collectLoop:
			for {
				select {
				case pkt := <-tw.packetBuffer:
					packetBatch = append(packetBatch, pkt)
					if len(packetBatch) >= 100 {
						break collectLoop
					}
				default:
					break collectLoop
				}
			}

			// Process collected packets
			if len(packetBatch) > 0 {
				tw.processBatch(packetBatch)
			}
		}
	}
}

// drainBuffer processes all remaining packets in the buffer
func (tw *trackWriter) drainBuffer(batch []bufferedPacket) {
	for {
		select {
		case pkt := <-tw.packetBuffer:
			batch = append(batch, pkt)
		default:
			if len(batch) > 0 {
				tw.processBatch(batch)
			}
			return
		}
	}
}

// processBatch processes a batch of packets
func (tw *trackWriter) processBatch(batch []bufferedPacket) {
	tw.mu.Lock()
	defer tw.mu.Unlock()

	for _, bp := range batch {
		pkt := bp.packet

		// Calculate samples per packet (20ms worth)
		samplesPerPacket := uint32(tw.clockRate * 20 / 1000)

		// Initialize on first packet
		if tw.firstPacket {
			tw.lastSeqNum = uint16(rand.IntN(1 << 16))
			tw.lastRTPTimestamp = pkt.Timestamp
			tw.actualPacketLastRTPTimestamp = pkt.Timestamp
			tw.ssrc = pkt.SSRC
			tw.firstPacket = false

			// Check if client started muted (gap between recording start and first packet)
			recordingStartTime := tw.recordingStartTime
			if !recordingStartTime.IsZero() {
				startGap := bp.arrivalTime.Sub(recordingStartTime)

				// If the first packet arrives significantly after recording started
				if startGap > silencePacketDetectionThreshold {
					numSilentPackets := int(startGap.Milliseconds() / 20)

					fmt.Printf("Client started muted - filling %d silence packets at start (gap: %v)\n",
						numSilentPackets, startGap)

					// Adjust starting timestamp to account for silence
					tw.lastRTPTimestamp = pkt.Timestamp - uint32(numSilentPackets)*samplesPerPacket

					// Insert silence packets from recording start
					for i := 0; i < numSilentPackets; i++ {
						opusSilence := []byte{0xF8, 0xFF, 0xFE} // Opus DTX frame
						silentPkt := &rtp.Packet{
							Header: rtp.Header{
								Version:        2,
								PayloadType:    111,
								SequenceNumber: tw.lastSeqNum,
								Timestamp:      tw.lastRTPTimestamp,
								SSRC:           tw.ssrc,
							},
							Payload: opusSilence,
						}

						if err := writeRTPWithSamples(tw.writer, silentPkt, uint64(samplesPerPacket)); err != nil {
							fmt.Printf("error writing start silence packet: %v", err)
							continue
						}

						tw.lastSeqNum++
						tw.lastRTPTimestamp += samplesPerPacket
					}
				}
			}

			tw.lastPacketTime = bp.arrivalTime

			// Write the first actual packet
			actualPkt := *pkt
			actualPkt.SequenceNumber = tw.lastSeqNum
			actualPkt.Timestamp = tw.lastRTPTimestamp

			if err := writeRTPWithSamples(tw.writer, &actualPkt, uint64(samplesPerPacket)); err != nil {
				fmt.Printf("error writing first packet: %v", err)
			}

			tw.lastSeqNum++
			continue
		}

		if !tw.lastPacketTime.IsZero() {
			// Use wall-clock time to determine actual mute duration
			muteDuration := bp.arrivalTime.Sub(tw.lastPacketTime)

			// Only insert silence if the gap is significant (> 100ms)
			// This avoids inserting silence for small processing delays
			if muteDuration > silencePacketDetectionThreshold {
				// Calculate number of silent packets needed
				numSilentPackets := int(muteDuration.Milliseconds() / 20)

				// Insert silence packets
				for i := 0; i < numSilentPackets; i++ {
					tw.lastSeqNum++
					tw.lastRTPTimestamp += samplesPerPacket

					opusSilence := []byte{0xF8, 0xFF, 0xFE} // Opus DTX frame
					silentPkt := &rtp.Packet{
						Header: rtp.Header{
							Version:        2,
							PayloadType:    111,
							SequenceNumber: tw.lastSeqNum,
							Timestamp:      tw.lastRTPTimestamp,
							SSRC:           tw.ssrc,
						},
						Payload: opusSilence,
					}

					if err := writeRTPWithSamples(tw.writer, silentPkt, uint64(samplesPerPacket)); err != nil {
						fmt.Printf("error writing silent packet: %v", err)
						continue
					}
				}
			}
		}

		// Write the actual packet
		tw.lastSeqNum++
		tw.lastRTPTimestamp += samplesPerPacket

		actualPkt := *pkt
		actualPkt.SequenceNumber = tw.lastSeqNum
		actualPkt.Timestamp = tw.lastRTPTimestamp

		if err := writeRTPWithSamples(tw.writer, &actualPkt, uint64(samplesPerPacket)); err != nil {
			fmt.Printf("error writing packet: %v", err)
		}

		// Update tracking variables
		tw.actualPacketLastRTPTimestamp = pkt.Timestamp
		tw.lastPacketTime = bp.arrivalTime
	}
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

	fmt.Printf("stopping recording: %s", r.recordingSession.id)

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

	// Stop all packet processors and wait for them to finish
	session.mu.Lock()
	for _, writerMap := range session.writers {
		for _, tw := range writerMap {
			close(tw.stopChan)
		}
	}

	// Wait for all processors to finish
	for _, writerMap := range session.writers {
		for _, tw := range writerMap {
			tw.wg.Wait()
		}
	}

	// Fill silence for any tracks that were muted when recording stopped
	for clientID, writerMap := range session.writers {
		for trackID, tw := range writerMap {
			tw.mu.Lock()

			// Check if there's a gap between last packet and recording stop time
			if !tw.lastPacketTime.IsZero() {
				gapDuration := session.meta.StopTime.Sub(tw.lastPacketTime)

				// If gap is significant (> 100ms), fill with silence
				if gapDuration > silencePacketDetectionThreshold {
					samplesPerPacket := uint32(tw.clockRate * 20 / 1000)
					numSilentPackets := int(gapDuration.Milliseconds() / 20)

					fmt.Printf("Filling %d silence packets at end for client %s track %s (gap: %v)",
						numSilentPackets, clientID, trackID, gapDuration)

					// Insert silence packets to fill the gap to recording end
					for i := 0; i < numSilentPackets; i++ {
						tw.lastSeqNum++
						tw.lastRTPTimestamp += samplesPerPacket

						opusSilence := []byte{0xF8, 0xFF, 0xFE} // Opus DTX frame
						silentPkt := &rtp.Packet{
							Header: rtp.Header{
								Version:        2,
								PayloadType:    111,
								SequenceNumber: tw.lastSeqNum,
								Timestamp:      tw.lastRTPTimestamp,
								SSRC:           tw.ssrc,
							},
							Payload: opusSilence,
						}

						if err := writeRTPWithSamples(tw.writer, silentPkt, uint64(samplesPerPacket)); err != nil {
							fmt.Printf("error writing end silence for client %s track %s: %v",
								clientID, trackID, err)
							break
						}
					}
				}
			}

			tw.mu.Unlock()
		}
	}
	session.mu.Unlock()

	fmt.Printf("closing writers: %s", session.id)

	// Close writers
	for _, m := range session.writers {
		for _, tw := range m {
			tw.writer.Close()
		}
	}

	fmt.Printf("writing meta.json: %s", session.id)

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

	fmt.Printf("merging and uploading: %s", session.id)

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
	// Convert merged .ogg to .m4a
	m4aPath := filepath.Join(baseDir, session.id+".m4a")
	cmd := exec.Command("ffmpeg", "-y", "-i", finalPath, "-c:a", "aac", "-b:a", "32k", m4aPath)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("ffmpeg convert to m4a failed: %v, output: %s", err, string(out))
	}
	if err := os.Remove(finalPath); err != nil {
		fmt.Printf("warning: failed to remove merged ogg: %v", err)
	}
	finalPath = m4aPath

	// Upload to S3
	mc, err := minio.New(session.cfg.S3.Endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(session.cfg.S3.AccessKey, session.cfg.S3.SecretKey, ""),
		Secure: session.cfg.S3.Secure,
	})
	if err != nil {
		fmt.Printf("error creating minio client: %v", err)
		return err
	}
	object := filepath.Join(session.cfg.S3.FilePrefix, session.id+".m4a")
	ctx := context.Background()
	_, err = mc.FPutObject(ctx, session.cfg.S3.Bucket, object, finalPath, minio.PutObjectOptions{ContentType: "audio/mp4"})
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
