package sfu

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/inlivedev/sfu/pkg/rtppool"
	"github.com/pion/interceptor"
	"github.com/pion/logging"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
)

var (
	ErrPlayerAlreadyPlaying = errors.New("player: already playing")
	ErrPlayerStopped        = errors.New("player: stopped")
)

// PlayerTrack represents a track that plays audio from a URL
type PlayerTrack struct {
	context          context.Context
	cancel           context.CancelFunc
	mu               sync.RWMutex
	base             *baseTrack
	onReadCallbacks  []func(interceptor.Attributes, *rtp.Packet, QualityLevel)
	onEndedCallbacks []func()
	isPlaying        *atomic.Bool
	stopPlayingChan  chan struct{}
	log              logging.LeveledLogger
}

// PlayerOptions contains configuration for URL playback
type PlayerOptions struct {
	URL        string
	Method     string // HTTP method for fetching the URL
	Body       string // Request body if needed
	Loop       bool   // Whether to loop the audio
	TrackID    string // Optional custom track ID
	StreamID   string // Optional custom stream ID
	SampleRate uint32 // Audio sample rate (default: 48000)
	Channels   uint16 // Audio channels (default: 1)
}

// AudioUtilsInterface defines the interface for audio utilities
// This should be implemented by your utils package
type AudioUtilsInterface interface {
	GetFile(endpoint, method, body string) (io.ReadCloser, error)
	NewOggReader(file io.ReadCloser, loop bool) (OggReaderInterface, *OggHeader, error)
	ParsePacketDuration(packet []byte) (time.Duration, error)
}

// OggReaderInterface defines the interface for OGG reading
type OggReaderInterface interface {
	ReadPacket() ([]byte, error)
}

// OggHeader contains OGG file header information
type OggHeader struct {
	Channels   uint16
	SampleRate uint32
}

// newPlayerTrack creates a new player track
func newPlayerTrack(ctx context.Context, client *Client, opts PlayerOptions) *PlayerTrack {
	if opts.TrackID == "" {
		opts.TrackID = uuid.New().String()
	}
	if opts.StreamID == "" {
		opts.StreamID = "player"
	}
	if opts.SampleRate == 0 {
		opts.SampleRate = 48000
	}
	if opts.Channels == 0 {
		opts.Channels = 1
	}

	localCtx, cancel := context.WithCancel(ctx)

	codec := webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType:     webrtc.MimeTypeOpus,
			ClockRate:    opts.SampleRate,
			Channels:     opts.Channels,
			SDPFmtpLine:  "minptime=10;useinbandfec=1",
			RTCPFeedback: []webrtc.RTCPFeedback{},
		},
		PayloadType: 111, // Standard Opus payload type
	}

	baseTrack := &baseTrack{
		id:           opts.TrackID,
		msid:         opts.StreamID + " " + opts.TrackID,
		streamid:     opts.StreamID,
		client:       client,
		kind:         webrtc.RTPCodecTypeAudio,
		codec:        codec,
		isScreen:     &atomic.Bool{},
		clientTracks: newClientTrackList(),
		pool:         rtppool.New(),
	}

	baseTrack.isScreen.Store(false) // Player tracks are media, not screen

	track := &PlayerTrack{
		context:          localCtx,
		cancel:           cancel,
		mu:               sync.RWMutex{},
		base:             baseTrack,
		onReadCallbacks:  make([]func(interceptor.Attributes, *rtp.Packet, QualityLevel), 0),
		onEndedCallbacks: make([]func(), 0),
		isPlaying:        &atomic.Bool{},
		stopPlayingChan:  make(chan struct{}),
		log:              client.log,
	}

	return track
}

// Implement ITrack interface
func (t *PlayerTrack) ID() string {
	return t.base.id
}

func (t *PlayerTrack) StreamID() string {
	return t.base.streamid
}

func (t *PlayerTrack) ClientID() string {
	return t.base.client.id
}

func (t *PlayerTrack) IsSimulcast() bool {
	return false
}

func (t *PlayerTrack) IsScaleable() bool {
	return false
}

func (t *PlayerTrack) IsProcessed() bool {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.base.isProcessed
}

func (t *PlayerTrack) SetSourceType(sourceType TrackType) {
	t.base.isScreen.Store(sourceType == TrackTypeScreen)
}

func (t *PlayerTrack) SourceType() TrackType {
	if t.base.isScreen.Load() {
		return TrackTypeScreen
	}
	return TrackTypeMedia
}

func (t *PlayerTrack) SetAsProcessed() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.base.isProcessed = true
}

func (t *PlayerTrack) OnRead(callback func(interceptor.Attributes, *rtp.Packet, QualityLevel)) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.onReadCallbacks = append(t.onReadCallbacks, callback)
}

func (t *PlayerTrack) IsScreen() bool {
	return t.base.isScreen.Load()
}

func (t *PlayerTrack) IsRelay() bool {
	return true // Player tracks are considered relay tracks
}

func (t *PlayerTrack) Kind() webrtc.RTPCodecType {
	return t.base.kind
}

func (t *PlayerTrack) MimeType() string {
	return t.base.codec.MimeType
}

func (t *PlayerTrack) TotalTracks() int {
	return 1
}

func (t *PlayerTrack) Context() context.Context {
	return t.context
}

func (t *PlayerTrack) Relay(f func(webrtc.SSRC, interceptor.Attributes, *rtp.Packet)) {
	t.OnRead(func(attrs interceptor.Attributes, p *rtp.Packet, quality QualityLevel) {
		f(webrtc.SSRC(t.base.codec.PayloadType), attrs, p)
	})
}

func (t *PlayerTrack) PayloadType() webrtc.PayloadType {
	return t.base.codec.PayloadType
}

func (t *PlayerTrack) OnEnded(f func()) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.onEndedCallbacks = append(t.onEndedCallbacks, f)
}

func (t *PlayerTrack) onEnded() {
	t.mu.RLock()
	defer t.mu.RUnlock()
	for _, f := range t.onEndedCallbacks {
		f()
	}
}

func (t *PlayerTrack) onRead(attrs interceptor.Attributes, p *rtp.Packet, quality QualityLevel) {
	callbacks := make([]func(interceptor.Attributes, *rtp.Packet, QualityLevel), 0)

	t.mu.RLock()
	callbacks = append(callbacks, t.onReadCallbacks...)
	t.mu.RUnlock()

	for _, callback := range callbacks {
		copyPacket := t.base.pool.CopyPacket(p)
		callback(attrs, copyPacket, quality)
		t.base.pool.PutPacket(copyPacket)
	}
}

// IsPlaying returns whether the player is currently playing
func (t *PlayerTrack) IsPlaying() bool {
	return t.isPlaying.Load()
}

// Play starts playing audio from the URL
func (t *PlayerTrack) Play(opts PlayerOptions, audioUtils AudioUtilsInterface) error {
	if t.IsPlaying() {
		return ErrPlayerAlreadyPlaying
	}

	t.isPlaying.Store(true)
	t.log.Infof("player: starting playback from %s (loop: %v)", opts.URL, opts.Loop)

	go func() {
		defer func() {
			t.isPlaying.Store(false)
			t.log.Infof("player: playback stopped")
		}()

		for {
			select {
			case <-t.context.Done():
				return
			case <-t.stopPlayingChan:
				return
			default:
				if err := t.playOnce(opts, audioUtils); err != nil {
					if errors.Is(err, io.EOF) {
						if opts.Loop {
							t.log.Infof("player: reached EOF, looping...")
							continue
						} else {
							t.log.Infof("player: reached EOF, stopping")
							return
						}
					}
					t.log.Errorf("player: error during playback: %v", err)
					return
				}

				if !opts.Loop {
					return
				}
			}
		}
	}()

	return nil
}

// playOnce plays the audio file once
func (t *PlayerTrack) playOnce(opts PlayerOptions, audioUtils AudioUtilsInterface) error {
	file, err := audioUtils.GetFile(opts.URL, opts.Method, opts.Body)
	if err != nil {
		return fmt.Errorf("failed to get file: %w", err)
	}
	defer file.Close()

	oggReader, oggHeader, err := audioUtils.NewOggReader(file, opts.Loop)
	if err != nil {
		return fmt.Errorf("failed to create ogg reader: %w", err)
	}

	t.log.Infof("player: OGG header - Channels: %d, SampleRate: %d", oggHeader.Channels, oggHeader.SampleRate)

	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-t.context.Done():
			return ErrPlayerStopped
		case <-t.stopPlayingChan:
			return ErrPlayerStopped
		case <-ticker.C:
			packet, err := oggReader.ReadPacket()
			if errors.Is(err, io.EOF) {
				return err
			}
			if err != nil {
				return fmt.Errorf("failed to read packet: %w", err)
			}

			// Parse packet duration but don't store it as we don't need it
			_, err = audioUtils.ParsePacketDuration(packet)
			if err != nil {
				return fmt.Errorf("failed to parse packet duration: %w", err)
			}

			// Create RTP packet
			rtpPacket := &rtp.Packet{
				Header: rtp.Header{
					Version:        2,
					Padding:        false,
					Extension:      false,
					Marker:         false,
					PayloadType:    uint8(t.base.codec.PayloadType),
					SequenceNumber: 0, // Will be set by packet processing
					Timestamp:      0, // Will be set by packet processing
					SSRC:           uint32(t.base.codec.PayloadType),
				},
				Payload: packet,
			}

			// Send packet to subscribers
			tracks := t.base.clientTracks.GetTracks()
			for _, track := range tracks {
				clonedPacket := rtpPacket.Clone()
				track.push(clonedPacket, QualityAudio)
			}

			// Trigger read callbacks
			t.onRead(nil, rtpPacket, QualityAudio)
		}
	}
}

// Stop stops the audio playback
func (t *PlayerTrack) Stop() {
	if !t.IsPlaying() {
		return
	}

	select {
	case t.stopPlayingChan <- struct{}{}:
	default:
		// Channel is full or closed, player already stopping
	}
}

// createLocalTrack creates a local track for the player
func (t *PlayerTrack) createLocalTrack() *webrtc.TrackLocalStaticRTP {
	track, err := webrtc.NewTrackLocalStaticRTP(
		t.base.codec.RTPCodecCapability,
		t.base.id,
		t.base.streamid,
	)
	if err != nil {
		panic(err)
	}
	return track
}

// subscribe allows a client to subscribe to this player track
func (t *PlayerTrack) subscribe(c *Client) iClientTrack {
	localTrack := t.createLocalTrack()

	ctx, cancel := context.WithCancel(t.Context())

	ct := &clientTrack{
		id:                    localTrack.ID(),
		streamid:              localTrack.StreamID(),
		context:               ctx,
		mu:                    sync.RWMutex{},
		client:                c,
		kind:                  localTrack.Kind(),
		mimeType:              localTrack.Codec().MimeType,
		localTrack:            localTrack,
		remoteTrack:           nil, // Player tracks don't have remote tracks
		baseTrack:             t.base,
		isScreen:              false,
		ssrc:                  webrtc.SSRC(t.base.codec.PayloadType),
		onTrackEndedCallbacks: make([]func(), 0),
	}

	t.OnEnded(func() {
		ct.onEnded()
		cancel()
	})

	t.base.clientTracks.Add(ct)
	return ct
}
