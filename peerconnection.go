package sfu

import (
	"sync"

	"github.com/pion/webrtc/v4"
)

type PeerConnection struct {
	mu             sync.Mutex
	pc             *webrtc.PeerConnection
	silenceTracks  map[string]*webrtc.TrackLocalStaticSample // Map of silence tracks by track ID
	heldTracks     map[string]*webrtc.RTPSender              // Map of held track senders by track ID
	originalTracks map[string]*webrtc.TrackLocalStaticRTP    // Map of original tracks by track ID
	senders        map[string]*webrtc.RTPSender              // Map of all track senders by track ID
}

func newPeerConnection(pc *webrtc.PeerConnection) *PeerConnection {
	return &PeerConnection{
		mu:             sync.Mutex{},
		pc:             pc,
		silenceTracks:  make(map[string]*webrtc.TrackLocalStaticSample),
		heldTracks:     make(map[string]*webrtc.RTPSender),
		originalTracks: make(map[string]*webrtc.TrackLocalStaticRTP),
		senders:        make(map[string]*webrtc.RTPSender),
	}
}

func (p *PeerConnection) PC() *webrtc.PeerConnection {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.pc
}

func (p *PeerConnection) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()

	return p.pc.Close()
}

func (p *PeerConnection) AddTrack(track *webrtc.TrackLocalStaticRTP) (*webrtc.RTPSender, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	sender, err := p.pc.AddTrack(track)
	if err == nil {
		// Store the original track for potential hold/unhold operations
		p.originalTracks[track.ID()] = track
		p.senders[track.ID()] = sender
	}
	return sender, err
}

// StoreSender stores a sender for a given track ID. This is used when tracks are added via AddTransceiver
func (p *PeerConnection) StoreSender(trackID string, sender *webrtc.RTPSender, track *webrtc.TrackLocalStaticRTP) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.senders[trackID] = sender
	p.originalTracks[trackID] = track
}

// GetSender retrieves the sender for a given track ID
func (p *PeerConnection) GetSender(trackID string) (*webrtc.RTPSender, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()

	sender, exists := p.senders[trackID]
	return sender, exists
}

func (p *PeerConnection) RemoveTrack(sender *webrtc.RTPSender) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	// Clean up track references when removing
	if sender.Track() != nil {
		trackID := sender.Track().ID()
		delete(p.silenceTracks, trackID)
		delete(p.heldTracks, trackID)
		delete(p.originalTracks, trackID)
		delete(p.senders, trackID)
	}

	return p.pc.RemoveTrack(sender)
}

// Hold puts a specific track on hold by replacing it with a silence track
// This mutes the track for the receiving peer
func (p *PeerConnection) Hold(trackID string, sender *webrtc.RTPSender) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	// Check if track is already on hold
	if _, exists := p.heldTracks[trackID]; exists {
		return nil // Already on hold
	}

	// Get the original track
	originalTrack, exists := p.originalTracks[trackID]
	if !exists {
		return ErrTrackIsNotExists
	}

	// Create silence track if it doesn't exist
	silenceTrack, exists := p.silenceTracks[trackID]
	if !exists {
		var err error

		// Create silence track based on the original track's codec capability
		if originalTrack.Kind() == webrtc.RTPCodecTypeAudio {
			silenceTrack, err = webrtc.NewTrackLocalStaticSample(
				webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus},
				originalTrack.ID(),
				originalTrack.StreamID(),
			)
		} else {
			// For video, we could create a blank video track or handle differently
			silenceTrack, err = webrtc.NewTrackLocalStaticSample(
				webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeVP8},
				originalTrack.ID(),
				originalTrack.StreamID(),
			)
		}

		if err != nil {
			return err
		}

		p.silenceTracks[trackID] = silenceTrack
	}

	// Replace the track with silence track
	err := sender.ReplaceTrack(silenceTrack)
	if err != nil {
		return err
	}

	// Mark this track as held
	p.heldTracks[trackID] = sender

	return nil
}

// Unhold removes a track from hold by restoring the original track
func (p *PeerConnection) Unhold(trackID string) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	// Check if track is actually on hold
	sender, exists := p.heldTracks[trackID]
	if !exists {
		return nil // Not on hold
	}

	// Get the original track
	originalTrack, exists := p.originalTracks[trackID]
	if !exists {
		return ErrTrackIsNotExists
	}

	// Replace the silence track with the original track
	err := sender.ReplaceTrack(originalTrack)
	if err != nil {
		return err
	}

	// Remove from held tracks
	delete(p.heldTracks, trackID)

	return nil
}

// IsOnHold checks if a track is currently on hold
func (p *PeerConnection) IsOnHold(trackID string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()

	_, exists := p.heldTracks[trackID]
	return exists
}

// GetHeldTracks returns a list of track IDs that are currently on hold
func (p *PeerConnection) GetHeldTracks() []string {
	p.mu.Lock()
	defer p.mu.Unlock()

	heldTrackIDs := make([]string, 0, len(p.heldTracks))
	for trackID := range p.heldTracks {
		heldTrackIDs = append(heldTrackIDs, trackID)
	}
	return heldTrackIDs
}
