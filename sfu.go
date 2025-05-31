package sfu

import (
	"context"
	"sync"
	"time"

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

// HoldClientTrack puts a specific track from a source client on hold for a target client.
// This means the target client will not hear/see the specified track from the source client.
// sourceClientID: The ID of the client whose track should be put on hold
// targetClientID: The ID of the client who should not receive the track
// trackID: The ID of the track to put on hold
func (s *SFU) HoldClientTrack(sourceClientID, targetClientID, trackID string) error {
	targetClient, err := s.GetClient(targetClientID)
	if err != nil {
		return err
	}

	// Check if the target client has this track from the source client
	targetTracks := targetClient.ClientTracks()
	for _, track := range targetTracks {
		// Check if this track belongs to the source client and matches the track ID
		if track.ID() == trackID {
			return targetClient.Hold(trackID)
		}
	}

	return ErrTrackIsNotExists
}

// UnholdClientTrack removes a specific track from hold for a target client.
// sourceClientID: The ID of the client whose track should be removed from hold
// targetClientID: The ID of the client who should start receiving the track again
// trackID: The ID of the track to remove from hold
func (s *SFU) UnholdClientTrack(sourceClientID, targetClientID, trackID string) error {
	targetClient, err := s.GetClient(targetClientID)
	if err != nil {
		return err
	}

	return targetClient.Unhold(trackID)
}

// HoldAllTracksFromClient puts all tracks from a source client on hold for a target client.
// This means the target client will not hear/see any tracks from the source client.
// sourceClientID: The ID of the client whose tracks should be put on hold
// targetClientID: The ID of the client who should not receive any tracks from the source
func (s *SFU) HoldAllTracksFromClient(sourceClientID, targetClientID string) error {
	targetClient, err := s.GetClient(targetClientID)
	if err != nil {
		return err
	}

	// Find all tracks from the source client in the target client's subscriptions
	targetTracks := targetClient.ClientTracks()
	var firstError error
	for _, track := range targetTracks {
		// For this implementation, we'll hold all tracks as we can't easily determine
		// which tracks came from which source client without additional metadata
		if err := targetClient.Hold(track.ID()); err != nil && firstError == nil {
			firstError = err
		}
	}

	return firstError
}

// UnholdAllTracksFromClient removes all tracks from a source client from hold for a target client.
// sourceClientID: The ID of the client whose tracks should be removed from hold
// targetClientID: The ID of the client who should start receiving tracks again from the source
func (s *SFU) UnholdAllTracksFromClient(sourceClientID, targetClientID string) error {
	targetClient, err := s.GetClient(targetClientID)
	if err != nil {
		return err
	}

	return targetClient.UnholdAllTracks()
}

// IsClientTrackOnHold checks if a specific track from a source client is on hold for a target client.
func (s *SFU) IsClientTrackOnHold(sourceClientID, targetClientID, trackID string) (bool, error) {
	targetClient, err := s.GetClient(targetClientID)
	if err != nil {
		return false, err
	}

	return targetClient.IsTrackOnHold(trackID), nil
}

// GetHeldTracksForClient returns all track IDs that are currently on hold for a specific client.
func (s *SFU) GetHeldTracksForClient(clientID string) ([]string, error) {
	client, err := s.GetClient(clientID)
	if err != nil {
		return nil, err
	}

	return client.GetHeldTracks(), nil
}

// HoldClientForAll puts a specific client on hold for all other clients in the SFU.
// This means no other client will hear/see any tracks from the specified client.
// clientID: The ID of the client to put on hold for everyone
func (s *SFU) HoldClientForAll(clientID string) error {
	clients := s.GetClients()
	var firstError error

	for _, client := range clients {
		if client.ID() != clientID {
			if err := s.HoldAllTracksFromClient(clientID, client.ID()); err != nil && firstError == nil {
				firstError = err
			}
		}
	}

	return firstError
}

// UnholdClientForAll removes a specific client from hold for all other clients in the SFU.
// clientID: The ID of the client to remove from hold for everyone
func (s *SFU) UnholdClientForAll(clientID string) error {
	clients := s.GetClients()
	var firstError error

	for _, client := range clients {
		if client.ID() != clientID {
			if err := s.UnholdAllTracksFromClient(clientID, client.ID()); err != nil && firstError == nil {
				firstError = err
			}
		}
	}

	return firstError
}
