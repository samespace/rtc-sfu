package sfu

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/inlivedev/sfu/pkg/recording"
	"github.com/pion/logging"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
	"golang.org/x/exp/slices"
)

// BitrateConfigs is the configuration for the bitrate that will be used for adaptive bitrates controller
// The paramenter is in bps (bit per second) for non pixels parameters.
// For pixels parameters, it is total pixels (width * height) of the video.
// High, Mid, and Low are the references for bitrate controller to decide the max bitrate to send to the client.
type BitrateConfigs struct {
	AudioRed         uint32 `json:"audio_red" example:"75000"`
	Audio            uint32 `json:"audio" example:"48000"`
	Video            uint32 `json:"video" example:"1200000"`
	VideoHigh        uint32 `json:"video_high" example:"1200000"`
	VideoHighPixels  uint32 `json:"video_high_pixels" example:"921600"`
	VideoMid         uint32 `json:"video_mid" example:"500000"`
	VideoMidPixels   uint32 `json:"video_mid_pixels" example:"259200"`
	VideoLow         uint32 `json:"video_low" example:"150000"`
	VideoLowPixels   uint32 `json:"video_low_pixels" example:"64800"`
	InitialBandwidth uint32 `json:"initial_bandwidth" example:"1000000"`
}

func DefaultBitrates() BitrateConfigs {
	return BitrateConfigs{
		AudioRed:         75_000,
		Audio:            48_000,
		Video:            700_000,
		VideoHigh:        700_000,
		VideoHighPixels:  720 * 360,
		VideoMid:         300_000,
		VideoMidPixels:   360 * 180,
		VideoLow:         90_000,
		VideoLowPixels:   180 * 90,
		InitialBandwidth: 1_000_000,
	}
}

type SFUClients struct {
	clients map[string]*Client
	mu      sync.Mutex
}

func (s *SFUClients) GetClients() map[string]*Client {
	s.mu.Lock()
	defer s.mu.Unlock()

	clients := make(map[string]*Client)
	for k, v := range s.clients {
		clients[k] = v
	}

	return clients
}

func (s *SFUClients) GetClient(id string) (*Client, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if client, ok := s.clients[id]; ok {
		return client, nil
	}

	return nil, ErrClientNotFound
}

func (s *SFUClients) Length() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	return len(s.clients)
}

func (s *SFUClients) Add(client *Client) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.clients[client.ID()]; ok {
		return ErrClientExists
	}

	s.clients[client.ID()] = client

	return nil
}

func (s *SFUClients) Remove(client *Client) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.clients[client.ID()]; !ok {
		return ErrClientNotFound
	}

	delete(s.clients, client.ID())

	return nil
}

type SFU struct {
	bitrateConfigs            BitrateConfigs
	clients                   *SFUClients
	context                   context.Context
	cancel                    context.CancelFunc
	codecs                    []string
	dataChannels              *SFUDataChannelList
	iceServers                []webrtc.ICEServer
	mu                        sync.Mutex
	onStop                    func()
	pliInterval               time.Duration
	onTrackAvailableCallbacks []func(tracks []ITrack)
	onClientRemovedCallbacks  []func(*Client)
	onClientAddedCallbacks    []func(*Client)
	relayTracks               map[string]ITrack
	clientStats               map[string]*ClientStats
	log                       logging.LeveledLogger
	defaultSettingEngine      *webrtc.SettingEngine
	// Recording related fields
	recordingManager   *recording.RecordingManager
	recordingManagerMu sync.RWMutex
	recordingBasePath  string
}

type PublishedTrack struct {
	ClientID string
	Track    webrtc.TrackLocal
}

type sfuOptions struct {
	IceServers    []webrtc.ICEServer
	Bitrates      BitrateConfigs
	QualityLevels []QualityLevel
	Codecs        []string
	PLIInterval   time.Duration
	Log           logging.LeveledLogger
	SettingEngine *webrtc.SettingEngine
}

// @Param muxPort: port for udp mux
func New(ctx context.Context, opts sfuOptions) *SFU {
	localCtx, cancel := context.WithCancel(ctx)

	sfu := &SFU{
		clients:                   &SFUClients{clients: make(map[string]*Client), mu: sync.Mutex{}},
		context:                   localCtx,
		cancel:                    cancel,
		codecs:                    opts.Codecs,
		dataChannels:              NewSFUDataChannelList(),
		mu:                        sync.Mutex{},
		iceServers:                opts.IceServers,
		bitrateConfigs:            opts.Bitrates,
		pliInterval:               opts.PLIInterval,
		relayTracks:               make(map[string]ITrack),
		onTrackAvailableCallbacks: make([]func(tracks []ITrack), 0),
		onClientRemovedCallbacks:  make([]func(*Client), 0),
		onClientAddedCallbacks:    make([]func(*Client), 0),
		log:                       opts.Log,
		defaultSettingEngine:      opts.SettingEngine,
	}

	return sfu
}

func (s *SFU) addClient(client *Client) {
	if err := s.clients.Add(client); err != nil {
		s.log.Errorf("sfu: failed to add client ", err)
		return
	}

	s.onClientAdded(client)
}

func (s *SFU) createClient(id string, name string, peerConnectionConfig webrtc.Configuration, opts ClientOptions) *Client {
	opts.settingEngine = *s.defaultSettingEngine

	client := NewClient(s, id, name, peerConnectionConfig, opts)

	// Get the LocalDescription and take it to base64 so we can paste in browser
	return client
}

func (s *SFU) NewClient(id, name string, opts ClientOptions) *Client {
	peerConnectionConfig := webrtc.Configuration{}

	if len(s.iceServers) > 0 {
		peerConnectionConfig.ICEServers = s.iceServers
	}

	opts.Log = s.log

	client := s.createClient(id, name, peerConnectionConfig, opts)

	// Set up the onTracksAdded callback to handle recording
	client.OnTracksAdded(func(tracks []ITrack) {
		s.onTracksAdded(id, tracks)
	})

	s.addClient(client)

	return client
}

func (s *SFU) AvailableTracks() []ITrack {
	tracks := make([]ITrack, 0)

	for _, client := range s.clients.GetClients() {
		tracks = append(tracks, client.publishedTracks.GetTracks()...)
	}

	return tracks
}

// Syncs track from connected client to other clients
func (s *SFU) syncTrack(client *Client) {
	publishedTrackIDs := make([]string, 0)
	for _, track := range client.publishedTracks.GetTracks() {
		publishedTrackIDs = append(publishedTrackIDs, track.ID())
	}

	subscribes := make([]SubscribeTrackRequest, 0)

	for _, clientPeer := range s.clients.GetClients() {
		for _, track := range clientPeer.tracks.GetTracks() {
			if client.ID() != clientPeer.ID() {
				if !slices.Contains(publishedTrackIDs, track.ID()) {
					subscribes = append(subscribes, SubscribeTrackRequest{
						ClientID: clientPeer.ID(),
						TrackID:  track.ID(),
					})
				}
			}
		}
	}

	if len(subscribes) > 0 {
		err := client.SubscribeTracks(subscribes)
		if err != nil {
			s.log.Errorf("client: failed to subscribe tracks ", err)
		}
	}
}

func (s *SFU) Stop() {
	for _, client := range s.clients.GetClients() {
		client.PeerConnection().Close()
	}

	if s.onStop != nil {
		s.onStop()
	}

	s.cancel()

}

func (s *SFU) OnStopped(callback func()) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.onStop = callback
}

func (s *SFU) OnClientAdded(callback func(*Client)) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.onClientAddedCallbacks = append(s.onClientAddedCallbacks, callback)
}

func (s *SFU) OnClientRemoved(callback func(*Client)) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.onClientRemovedCallbacks = append(s.onClientRemovedCallbacks, callback)
}

func (s *SFU) onAfterClientStopped(client *Client) {
	if err := s.removeClient(client); err != nil {
		s.log.Errorf("sfu: failed to remove client ", err)
	}
}

func (s *SFU) onClientAdded(client *Client) {
	for _, callback := range s.onClientAddedCallbacks {
		callback(client)
	}
}

func (s *SFU) onClientRemoved(client *Client) {
	for _, callback := range s.onClientRemovedCallbacks {
		callback(client)
	}
}

func (s *SFU) onTracksAvailable(clientId string, tracks []ITrack) {
	for _, client := range s.clients.GetClients() {
		if client.ID() != clientId {
			client.onTracksAvailable(tracks)
			s.log.Infof("sfu: client %s have %d tracks available ", client.ID(), len(tracks))
		}
	}

	for _, callback := range s.onTrackAvailableCallbacks {
		if callback != nil {
			callback(tracks)
		}
	}
}

func (s *SFU) GetClient(id string) (*Client, error) {
	return s.clients.GetClient(id)
}

func (s *SFU) GetClients() map[string]*Client {
	return s.clients.GetClients()
}

func (s *SFU) removeClient(client *Client) error {
	if err := s.clients.Remove(client); err != nil {
		s.log.Errorf("sfu: failed to remove client ", err)
		return err
	}

	s.onClientRemoved(client)

	return nil
}

func (s *SFU) CreateDataChannel(label string, opts DataChannelOptions) error {
	dc := s.dataChannels.Get(label)
	if dc != nil {
		return ErrDataChannelExists
	}

	s.dataChannels.Add(label, opts)

	errors := []error{}
	initOpts := &webrtc.DataChannelInit{
		Ordered: &opts.Ordered,
	}

	for _, client := range s.clients.GetClients() {
		if len(opts.ClientIDs) > 0 {
			if !slices.Contains(opts.ClientIDs, client.ID()) {
				continue
			}
		}

		err := client.createDataChannel(label, initOpts)
		if err != nil {
			errors = append(errors, err)
		}
	}

	return FlattenErrors(errors)
}

func (s *SFU) setupMessageForwarder(clientID string, d *webrtc.DataChannel) {
	d.OnMessage(func(msg webrtc.DataChannelMessage) {
		// broadcast to all clients
		s.mu.Lock()
		defer s.mu.Unlock()

		for _, client := range s.clients.GetClients() {
			// skip the sender
			if client.id == clientID {
				continue
			}

			dc := client.dataChannels.Get(d.Label())
			if dc == nil {
				continue
			}

			if dc.ReadyState() != webrtc.DataChannelStateOpen {
				dc.OnOpen(func() {
					dc.Send(msg.Data)
				})
			} else {
				dc.Send(msg.Data)
			}
		}
	})
}

func (s *SFU) createExistingDataChannels(c *Client) {
	for _, dc := range s.dataChannels.dataChannels {
		initOpts := &webrtc.DataChannelInit{
			Ordered: &dc.isOrdered,
		}
		if len(dc.clientIDs) > 0 {
			if !slices.Contains(dc.clientIDs, c.id) {
				continue
			}
		}

		if err := c.createDataChannel(dc.label, initOpts); err != nil {
			s.log.Errorf("datachanel: error on create existing data channel %s, error %s", dc.label, err.Error())
		}
	}
}

func (s *SFU) TotalActiveSessions() int {
	count := 0
	for _, c := range s.clients.GetClients() {
		if c.PeerConnection().PC().ConnectionState() == webrtc.PeerConnectionStateConnected {
			count++
		}
	}

	return count
}

func (s *SFU) PLIInterval() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.pliInterval
}

func (s *SFU) OnTracksAvailable(callback func(tracks []ITrack)) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.onTrackAvailableCallbacks = append(s.onTrackAvailableCallbacks, callback)
}

func (s *SFU) AddRelayTrack(ctx context.Context, id, streamid, rid string, client *Client, kind webrtc.RTPCodecType, ssrc webrtc.SSRC, mimeType string, rtpChan chan *rtp.Packet) error {
	var track ITrack

	relayTrack := NewTrackRelay(id, streamid, rid, kind, ssrc, mimeType, rtpChan)

	onPLI := func() {}

	if rid == "" {
		// not simulcast
		track = newTrack(ctx, client, relayTrack, 0, 0, s.pliInterval, onPLI, nil, nil)
		s.mu.Lock()
		s.relayTracks[relayTrack.ID()] = track
		s.mu.Unlock()
	} else {
		// simulcast
		var simulcast *SimulcastTrack
		var ok bool

		s.mu.Lock()
		track, ok := s.relayTracks[relayTrack.ID()]
		if !ok {
			// if track not found, add it
			track = newSimulcastTrack(client, relayTrack, 0, 0, s.pliInterval, onPLI, nil, nil)
			s.relayTracks[relayTrack.ID()] = track

		} else if simulcast, ok = track.(*SimulcastTrack); ok {
			simulcast.AddRemoteTrack(relayTrack, 0, 0, nil, nil, onPLI)
		}
		s.mu.Unlock()
	}

	// TODO: replace to with subscribe to all available tracks
	// s.broadcastTracksToAutoSubscribeClients(client.ID(), []ITrack{track})

	// notify the local clients that a relay track is available
	s.onTracksAvailable(client.ID(), []ITrack{track})

	return nil
}

// StartRecording starts recording for the room
func (s *SFU) StartRecording(recordingID string, s3Config *recording.S3Config) error {
	s.recordingManagerMu.Lock()
	defer s.recordingManagerMu.Unlock()

	fmt.Printf("### RECORDING DEBUG: Starting recording with ID: %s, basePath: %s\n", recordingID, s.recordingBasePath)

	// Check if already recording
	if s.recordingManager != nil {
		fmt.Printf("### RECORDING DEBUG: Cannot start recording - already recording with existing manager\n")
		return fmt.Errorf("already recording")
	}

	// Check if base path is set
	if s.recordingBasePath == "" {
		fmt.Printf("### RECORDING DEBUG: Cannot start recording - recording base path is not set\n")
		return fmt.Errorf("recording base path is not set")
	}

	// Check if directory is accessible and create if needed
	recordingPath := filepath.Join(s.recordingBasePath, recordingID)
	fmt.Printf("### RECORDING DEBUG: Checking/creating recording directory at: %s\n", recordingPath)
	if err := os.MkdirAll(recordingPath, 0755); err != nil {
		fmt.Printf("### RECORDING DEBUG: Failed to create recording directory: %v\n", err)
		return fmt.Errorf("failed to create recording directory: %w", err)
	}

	// Verify directory permissions
	if info, err := os.Stat(recordingPath); err != nil {
		fmt.Printf("### RECORDING DEBUG: Failed to stat recording directory: %v\n", err)
	} else {
		fmt.Printf("### RECORDING DEBUG: Recording directory (%s) mode: %v\n", recordingPath, info.Mode())
	}

	// Create recording config
	config := recording.RecordingConfig{
		BasePath:         s.recordingBasePath,
		RecordingID:      recordingID,
		SampleRate:       48000, // Standard for WebRTC audio
		ChannelCount:     1,     // Mono for individual recordings
		UploadToS3:       s3Config != nil,
		S3Config:         s3Config,
		UploadMergedOnly: true, // Only upload merged file by default
	}

	fmt.Printf("### RECORDING DEBUG: Creating recording manager with config BasePath: %s, RecordingID: %s\n",
		config.BasePath, config.RecordingID)

	// Create recording manager
	manager, err := recording.NewRecordingManager(s.context, config, s.log)
	if err != nil {
		fmt.Printf("### RECORDING DEBUG: Failed to create recording manager: %v\n", err)
		return fmt.Errorf("failed to create recording manager: %w", err)
	}

	s.recordingManager = manager
	fmt.Printf("### RECORDING DEBUG: Recording manager created successfully\n")

	// Start recording
	if err := manager.Start(); err != nil {
		fmt.Printf("### RECORDING DEBUG: Failed to start recording: %v\n", err)
		s.recordingManager = nil
		return fmt.Errorf("failed to start recording: %w", err)
	}

	fmt.Printf("### RECORDING DEBUG: Recording started successfully. Adding existing tracks...\n")

	// Add existing audio tracks to recording
	trackCount := 0
	clientCount := 0
	for _, client := range s.clients.GetClients() {
		clientCount++
		fmt.Printf("### RECORDING DEBUG: Processing client %s, channel type: %d, state: %d\n",
			client.ID(), client.options.ChannelType, client.peerConnection.PC().ConnectionState())

		for _, track := range client.tracks.GetTracks() {
			if track.Kind() == webrtc.RTPCodecTypeAudio {
				fmt.Printf("### RECORDING DEBUG: Found audio track from client %s: %s\n",
					client.ID(), track.ID())
				trackCount++
				s.addTrackToRecording(client.ID(), track)
			}
		}
	}

	fmt.Printf("### RECORDING DEBUG: Added %d audio tracks from %d clients to recording\n", trackCount, clientCount)

	return nil
}

// PauseRecording pauses the current recording
func (s *SFU) PauseRecording() error {
	s.recordingManagerMu.RLock()
	defer s.recordingManagerMu.RUnlock()

	if s.recordingManager == nil {
		return fmt.Errorf("not recording")
	}

	return s.recordingManager.Pause()
}

// ResumeRecording resumes a paused recording
func (s *SFU) ResumeRecording() error {
	s.recordingManagerMu.RLock()
	defer s.recordingManagerMu.RUnlock()

	if s.recordingManager == nil {
		return fmt.Errorf("not recording")
	}

	return s.recordingManager.Resume()
}

// StopRecording stops the current recording
func (s *SFU) StopRecording() (string, error) {
	s.recordingManagerMu.Lock()
	defer s.recordingManagerMu.Unlock()

	fmt.Printf("### RECORDING DEBUG: Stopping recording\n")

	if s.recordingManager == nil {
		fmt.Printf("### RECORDING DEBUG: StopRecording called but not recording (manager is nil)\n")
		return "", fmt.Errorf("not recording")
	}

	outputPath := s.recordingManager.GetOutputPath()
	fmt.Printf("### RECORDING DEBUG: Stopping recording with output path: %s\n", outputPath)

	err := s.recordingManager.Stop()
	if err != nil {
		fmt.Printf("### RECORDING DEBUG: Error stopping recording: %v\n", err)
		return outputPath, err
	}

	fmt.Printf("### RECORDING DEBUG: Recording stopped successfully\n")
	s.recordingManager = nil
	return outputPath, nil
}

// SetRecordingBasePath sets the base path for recordings
func (s *SFU) SetRecordingBasePath(path string) {
	s.recordingManagerMu.Lock()
	defer s.recordingManagerMu.Unlock()

	fmt.Printf("### RECORDING DEBUG: Setting recording base path: %q\n", path)
	s.recordingBasePath = path
}

// IsRecording checks if the room is currently recording
func (s *SFU) IsRecording() bool {
	s.recordingManagerMu.RLock()
	defer s.recordingManagerMu.RUnlock()

	return s.recordingManager != nil
}

// Internal method to add a track to recording
func (s *SFU) addTrackToRecording(clientID string, track ITrack) {
	s.recordingManagerMu.RLock()
	defer s.recordingManagerMu.RUnlock()

	if s.recordingManager == nil || track.Kind() != webrtc.RTPCodecTypeAudio {
		if s.recordingManager == nil {
			fmt.Printf("### RECORDING DEBUG: Cannot add track - recording manager is nil\n")
		}
		return
	}

	fmt.Printf("### RECORDING DEBUG: Adding track %s from client %s to recording\n", track.ID(), clientID)

	// Find the remote track implementation
	var remoteTrackImpl *remoteTrack

	switch t := track.(type) {
	case *AudioTrack:
		fmt.Printf("### RECORDING DEBUG: Track %s is an AudioTrack\n", track.ID())
		remoteTrackImpl = t.RemoteTrack()
		if remoteTrackImpl == nil {
			fmt.Printf("### RECORDING DEBUG: Failed to get remoteTrack from AudioTrack %s\n", track.ID())
		} else {
			fmt.Printf("### RECORDING DEBUG: Successfully obtained remoteTrackImpl for track %s\n", track.ID())
		}
	case *Track:
		// Skip video tracks
		fmt.Printf("### RECORDING DEBUG: Track %s is a video Track, skipping\n", track.ID())
		return
	case *SimulcastTrack:
		// Skip simulcast tracks (these are video)
		fmt.Printf("### RECORDING DEBUG: Track %s is a SimulcastTrack, skipping\n", track.ID())
		return
	default:
		fmt.Printf("### RECORDING DEBUG: Unknown track type for recording: %T\n", track)
		return
	}

	client, err := s.clients.GetClient(clientID)
	if err != nil {
		fmt.Printf("### RECORDING DEBUG: Failed to find client %s for recording: %v\n", clientID, err)
		return
	}

	// Get the channel type for this client
	channelType := int(client.options.ChannelType)
	fmt.Printf("### RECORDING DEBUG: Client %s has channel type: %d\n", clientID, channelType)

	// Don't record if channel type is NoRecord
	if channelType == int(recording.ChannelTypeNoRecord) {
		fmt.Printf("### RECORDING DEBUG: Client %s has ChannelTypeNoRecord, skipping\n", clientID)
		return
	}

	// Find the correct sample rate from the codec
	sampleRate := uint32(48000) // Default to 48kHz
	if remoteTrackImpl != nil && remoteTrackImpl.track != nil {
		codec := remoteTrackImpl.track.Codec()
		if codec.MimeType != "" {
			sampleRate = codec.ClockRate
			fmt.Printf("### RECORDING DEBUG: Track %s codec: %s, sample rate: %d\n", track.ID(), codec.MimeType, sampleRate)
		}
	}

	// Add track to recording manager
	recorder, err := s.recordingManager.AddTrack(clientID, track.ID(), channelType, sampleRate)
	if err != nil {
		fmt.Printf("### RECORDING DEBUG: Failed to add track %s to recording: %v\n", track.ID(), err)
		return
	} else {
		fmt.Printf("### RECORDING DEBUG: Successfully added track %s to recording manager, path: %s\n",
			track.ID(), recorder.GetFilePath())
	}

	// Set recording manager on remote track for packet routing
	if remoteTrackImpl != nil {
		fmt.Printf("### RECORDING DEBUG: Setting recording manager on remote track %s\n", track.ID())
		remoteTrackImpl.SetRecordingManager(s.recordingManager, clientID, track.ID())
		fmt.Printf("### RECORDING DEBUG: Successfully configured track %s from client %s for recording\n", track.ID(), clientID)
	} else {
		fmt.Printf("### RECORDING DEBUG: Cannot set recording manager - remoteTrackImpl is nil for track %s\n", track.ID())
	}
}

// Handle track added
func (s *SFU) onTracksAdded(clientID string, tracks []ITrack) {
	// Call the onTracksAvailable method to notify other clients
	s.onTracksAvailable(clientID, tracks)

	// Add audio tracks to recording if needed
	for _, track := range tracks {
		if track.Kind() == webrtc.RTPCodecTypeAudio {
			s.addTrackToRecording(clientID, track)
		}
	}
}
