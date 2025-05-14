package sfu

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/inlivedev/sfu/pkg/recording"
	"github.com/pion/interceptor"
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

// RecordingConfig contains configuration for recording a room
type RecordingConfig struct {
	BasePath    string
	RecordingID string
	S3Config    *recording.S3Config
}

// StartRecording starts recording for the current SFU
func (s *SFU) StartRecording(roomID string, roomRecordingConfig RecordingConfig) error {
	s.recordingManagerMu.Lock()
	defer s.recordingManagerMu.Unlock()

	if roomID == "" {
		return fmt.Errorf("roomID is required")
	}

	if roomRecordingConfig.BasePath == "" {
		roomRecordingConfig.BasePath = s.recordingBasePath
		fmt.Printf("### RECORDING DEBUG: Setting recording base path: %q\n", roomRecordingConfig.BasePath)
	}

	// If recording ID is not specified, use room ID
	if roomRecordingConfig.RecordingID == "" {
		roomRecordingConfig.RecordingID = roomID
	}

	fmt.Printf("### RECORDING DEBUG: Starting recording with ID: %s, basePath: %s\n",
		roomRecordingConfig.RecordingID, roomRecordingConfig.BasePath)

	// Check if already recording
	if s.recordingManager != nil {
		fmt.Printf("### RECORDING DEBUG: Already recording. Stopping current recording first.\n")
		_, err := s.StopRecording()
		if err != nil {
			fmt.Printf("### RECORDING DEBUG: Error stopping current recording: %v\n", err)
		}
	}

	// Create recording config
	config := recording.RecordingConfig{
		BasePath:    roomRecordingConfig.BasePath,
		RecordingID: roomRecordingConfig.RecordingID,
		S3Config:    roomRecordingConfig.S3Config,
		SampleRate:  48000, // Standard for WebRTC audio
	}

	// Initialize the recording manager
	mgr, err := recording.NewRecordingManager(s.context, config, s.log)
	if err != nil {
		return fmt.Errorf("failed to create recording manager: %w", err)
	}

	fmt.Printf("### RECORDING DEBUG: Recording manager created successfully\n")

	// Start recording
	if err := mgr.Start(); err != nil {
		return fmt.Errorf("failed to start recording: %w", err)
	}

	// Set recording manager
	s.recordingManager = mgr

	fmt.Printf("### RECORDING DEBUG: Recording started successfully. Adding existing tracks...\n")

	// Add all existing audio tracks from all connected clients
	clients := s.clients.GetClients()
	for clientID, client := range clients {
		// Get the client's channel type
		channelType := int(client.options.ChannelType)
		fmt.Printf("### RECORDING DEBUG: Processing client %s, channel type: %d, state: %d\n",
			clientID, channelType, client.peerConnection.PC().ConnectionState())

		// Skip clients with ChannelTypeNoRecord
		if channelType == 0 {
			fmt.Printf("### RECORDING DEBUG: Client %s has ChannelTypeNoRecord, skipping\n", clientID)
			continue
		}

		// Add all audio tracks from this client
		for _, track := range client.tracks.GetTracks() {
			if track.Kind() == webrtc.RTPCodecTypeAudio {
				fmt.Printf("### RECORDING DEBUG: Found audio track from client %s: %s\n", clientID, track.ID())
				// Add to recording with the client's channel type
				s.addTrackToRecording(clientID, track)
			}
		}
	}

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

	// Get recording ID before stopping
	recordingID := s.recordingManager.GetRecordingID()
	recordingBasePath := s.recordingManager.GetBasePath()
	recordingDir := filepath.Join(recordingBasePath, recordingID)

	fmt.Printf("### RECORDING DEBUG: Checking content of recording directory: %s\n", recordingDir)
	files, err := os.ReadDir(recordingDir)
	if err != nil {
		fmt.Printf("### RECORDING DEBUG: Error reading recording directory: %v\n", err)
	} else {
		fmt.Printf("### RECORDING DEBUG: Found %d entries in recording directory\n", len(files))
		for _, file := range files {
			info, _ := file.Info()
			if info != nil {
				fmt.Printf("### RECORDING DEBUG: File: %s, Size: %d bytes, IsDir: %v\n",
					file.Name(), info.Size(), file.IsDir())
			} else {
				fmt.Printf("### RECORDING DEBUG: File: %s, IsDir: %v\n", file.Name(), file.IsDir())
			}
		}
	}

	fmt.Printf("### RECORDING DEBUG: Stopping recording manager...\n")
	err = s.recordingManager.Stop()
	if err != nil {
		fmt.Printf("### RECORDING DEBUG: Error stopping recording: %v\n", err)
		return outputPath, err
	}

	// Check if the merged file was created
	if _, err := os.Stat(outputPath); os.IsNotExist(err) {
		fmt.Printf("### RECORDING DEBUG: Merged file not found at: %s\n", outputPath)
	} else if err != nil {
		fmt.Printf("### RECORDING DEBUG: Error checking merged file: %v\n", err)
	} else {
		fileInfo, _ := os.Stat(outputPath)
		if fileInfo != nil {
			fmt.Printf("### RECORDING DEBUG: Merged file created: %s, Size: %d bytes\n",
				outputPath, fileInfo.Size())
		} else {
			fmt.Printf("### RECORDING DEBUG: Merged file created: %s\n", outputPath)
		}
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

	if s.recordingManager == nil {
		fmt.Printf("### RECORDING DEBUG: Cannot add track %s to recording - recording manager is nil\n", track.ID())
		return
	}

	if track.Kind() != webrtc.RTPCodecTypeAudio {
		fmt.Printf("### RECORDING DEBUG: Skipping non-audio track %s\n", track.ID())
		return
	}

	fmt.Printf("### RECORDING DEBUG: Adding track %s from client %s to recording\n", track.ID(), clientID)

	// Determine channel type
	client, err := s.clients.GetClient(clientID)
	if err != nil {
		fmt.Printf("### RECORDING DEBUG: Failed to find client %s for recording: %v\n", clientID, err)
		return
	}

	// Get the channel type for this client
	channelType := int(client.options.ChannelType)
	fmt.Printf("### RECORDING DEBUG: Client %s has channel type: %d\n", clientID, channelType)

	// Don't record if channel type is NoRecord (0)
	if channelType == 0 {
		fmt.Printf("### RECORDING DEBUG: Client %s has ChannelTypeNoRecord (0), skipping\n", clientID)
		return
	}

	// Validate channel type (1 = left, 2 = right)
	if channelType != 1 && channelType != 2 {
		fmt.Printf("### RECORDING DEBUG: Invalid channel type %d for client %s, defaulting to left channel (1)\n",
			channelType, clientID)
		channelType = 1
	}

	// Find appropriate sample rate
	sampleRate := uint32(48000) // Default to 48kHz

	// Take a more direct approach to attach recording - try all possible ways

	// First try the remoteTrack approach
	var remoteTrackImpl *remoteTrack
	switch t := track.(type) {
	case *AudioTrack:
		fmt.Printf("### RECORDING DEBUG: Track %s is an AudioTrack\n", track.ID())
		remoteTrackImpl = t.RemoteTrack()
	case *SimulcastTrack:
		fmt.Printf("### RECORDING DEBUG: Track %s is a SimulcastTrack\n", track.ID())
		// For simulcast, try to get the highest quality track first
		remoteTrackImpl = t.GetRemoteTrack(QualityHigh)
		if remoteTrackImpl == nil {
			remoteTrackImpl = t.GetRemoteTrack(QualityMid)
		}
		if remoteTrackImpl == nil {
			remoteTrackImpl = t.GetRemoteTrack(QualityLow)
		}
	}

	// Add track to recording manager first to create the recorder
	fmt.Printf("### RECORDING DEBUG: Creating recorder for track %s\n", track.ID())
	recorder, err := s.recordingManager.AddTrack(clientID, track.ID(), channelType, sampleRate)
	if err != nil {
		fmt.Printf("### RECORDING DEBUG: Failed to add track %s to recording: %v\n", track.ID(), err)
		return
	}
	fmt.Printf("### RECORDING DEBUG: Successfully created recorder at %s\n", recorder.GetFilePath())

	// Try both approaches to ensure the track is properly configured

	// 1. If we found a remoteTrackImpl, set the recording manager on it
	if remoteTrackImpl != nil {
		fmt.Printf("### RECORDING DEBUG: Setting recording manager on remote track %s\n", track.ID())
		remoteTrackImpl.SetRecordingManager(s.recordingManager, clientID, track.ID())

		// Verify the recording manager is set
		remoteTrackImpl.mu.RLock()
		hasManager := remoteTrackImpl.recordingManager != nil
		hasClientID := remoteTrackImpl.clientID != ""
		hasTrackID := remoteTrackImpl.trackID != ""
		remoteTrackImpl.mu.RUnlock()

		fmt.Printf("### RECORDING DEBUG: Track %s recording setup: hasManager=%v, hasClientID=%v, hasTrackID=%v\n",
			track.ID(), hasManager, hasClientID, hasTrackID)
	}

	// 2. Also set up direct packet interception as fallback
	fmt.Printf("### RECORDING DEBUG: Setting up direct packet interception for track %s\n", track.ID())
	track.OnRead(func(_ interceptor.Attributes, packet *rtp.Packet, _ QualityLevel) {
		// Send the packet directly to the recording manager
		if s.recordingManager != nil {
			if err := s.recordingManager.WriteRTP(clientID, track.ID(), packet); err != nil {
				if rand.Intn(1000) == 0 { // Only log occasionally to avoid flooding
					fmt.Printf("### RECORDING DEBUG: Direct recording failed for track %s: %v\n", track.ID(), err)
				}
			}
		}
	})

	// 3. If it's an AudioTrack, also try EnableDirectRecording
	if audioTrack, isAudio := track.(*AudioTrack); isAudio {
		fmt.Printf("### RECORDING DEBUG: Using EnableDirectRecording for AudioTrack %s\n", track.ID())
		if innerTrack := audioTrack.RemoteTrack(); innerTrack != nil {
			innerTrack.EnableDirectRecording(s.recordingManager, clientID, track.ID(), sampleRate)
		}
	}

	fmt.Printf("### RECORDING DEBUG: Track %s setup for recording completed\n", track.ID())
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

// AddTrackToRecording explicitly adds a track to recording
// This can be called externally when normal track addition fails
func (s *SFU) AddTrackToRecording(clientID string, trackID string, track ITrack) error {
	s.recordingManagerMu.RLock()
	defer s.recordingManagerMu.RUnlock()

	fmt.Printf("### RECORDING DEBUG: Explicitly adding track %s from client %s to recording\n", trackID, clientID)

	if s.recordingManager == nil {
		return fmt.Errorf("recording manager is not initialized")
	}

	if track == nil {
		return fmt.Errorf("track is nil")
	}

	// Track must be audio
	if track.Kind() != webrtc.RTPCodecTypeAudio {
		return fmt.Errorf("only audio tracks can be recorded")
	}

	// Find the remote track implementation
	var remoteTrackImpl *remoteTrack

	switch t := track.(type) {
	case *AudioTrack:
		remoteTrackImpl = t.RemoteTrack()
	case *Track:
		remoteTrackImpl = t.RemoteTrack()
	case *SimulcastTrack:
		// Try each quality level
		remoteTrackImpl = t.GetRemoteTrack(QualityHigh)
		if remoteTrackImpl == nil {
			remoteTrackImpl = t.GetRemoteTrack(QualityMid)
		}
		if remoteTrackImpl == nil {
			remoteTrackImpl = t.GetRemoteTrack(QualityLow)
		}
	}

	client, err := s.clients.GetClient(clientID)
	if err != nil {
		return fmt.Errorf("client not found: %w", err)
	}

	channelType := int(client.options.ChannelType)

	// Don't record if channel type is NoRecord (0)
	if channelType == 0 {
		return fmt.Errorf("client is configured not to be recorded (ChannelTypeNoRecord)")
	}

	// Validate channel type (1 = left, 2 = right)
	if channelType != 1 && channelType != 2 {
		fmt.Printf("### RECORDING DEBUG: Invalid channel type %d for client %s, defaulting to left channel (1)\n",
			channelType, clientID)
		channelType = 1
	}

	sampleRate := uint32(48000) // Default

	if remoteTrackImpl != nil {
		fmt.Printf("### RECORDING DEBUG: Found remoteTrackImpl for track %s\n", trackID)

		if remoteTrackImpl.track != nil && remoteTrackImpl.track.Codec().MimeType != "" {
			sampleRate = remoteTrackImpl.track.Codec().ClockRate
			fmt.Printf("### RECORDING DEBUG: Track %s codec: %s, sample rate: %d\n",
				trackID, remoteTrackImpl.track.Codec().MimeType, sampleRate)
		}

		// Set up recording on the remote track
		remoteTrackImpl.SetRecordingManager(s.recordingManager, clientID, trackID)

		// Add to recording manager
		recorder, err := s.recordingManager.AddTrack(clientID, trackID, channelType, sampleRate)
		if err != nil {
			return fmt.Errorf("failed to add track to recording manager: %w", err)
		}

		fmt.Printf("### RECORDING DEBUG: Successfully added track %s to recording, path: %s, channel type: %d\n",
			trackID, recorder.GetFilePath(), channelType)

		return nil
	}

	// If we couldn't find a remoteTrackImpl, set up interceptor-based recording
	fmt.Printf("### RECORDING DEBUG: Setting up interceptor-based recording for track %s\n", trackID)

	// Add track to recording manager
	recorder, err := s.recordingManager.AddTrack(clientID, trackID, channelType, sampleRate)
	if err != nil {
		return fmt.Errorf("failed to add track to recording: %w", err)
	}

	// Set up packet interception
	track.OnRead(func(_ interceptor.Attributes, packet *rtp.Packet, _ QualityLevel) {
		if s.recordingManager != nil {
			err := s.recordingManager.WriteRTP(clientID, trackID, packet)
			if err != nil && rand.Intn(100) == 0 {
				fmt.Printf("### RECORDING DEBUG: Interceptor recording failed: %v\n", err)
			}
		}
	})

	fmt.Printf("### RECORDING DEBUG: Successfully set up interceptor recording for track %s, file path: %s, channel type: %d\n",
		trackID, recorder.GetFilePath(), channelType)

	return nil
}
