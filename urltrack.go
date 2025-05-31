package sfu

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"github.com/inlivedev/sfu/pkg/packetmap"
	"github.com/inlivedev/sfu/pkg/rtppool"
	"github.com/pion/interceptor"
	"github.com/pion/logging"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
)

// URLTrackOptions contains configuration for URL audio playback
type URLTrackOptions struct {
	URL    string // HTTP endpoint to fetch audio
	Method string // HTTP method (e.g., GET, POST)
	Body   string // Request body if needed
	Loop   bool   // Whether to loop playback
}

// URLTrack plays Opus audio from a remote URL, integrated as an SFU track
type URLTrack struct {
	context          context.Context
	cancel           context.CancelFunc
	mu               sync.RWMutex
	base             *baseTrack
	onReadCallbacks  []func(interceptor.Attributes, *rtp.Packet, QualityLevel)
	onEndedCallbacks []func()
	isPlaying        *atomic.Bool
	stopCh           chan struct{}
	nextTimestamp    uint32 // cumulative RTP timestamp
	log              logging.LeveledLogger
}

// NewURLTrack constructs a URLTrack attached to the given client
func NewURLTrack(ctx context.Context, client *Client) *URLTrack {
	localCtx, cancel := context.WithCancel(ctx)

	// Opus codec parameters
	codec := webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType:    webrtc.MimeTypeOpus,
			ClockRate:   48000,
			Channels:    1,
			SDPFmtpLine: "minptime=10;useinbandfec=1",
		},
		PayloadType: 111,
	}

	base := &baseTrack{
		id:           fmt.Sprintf("url-%s", client.ID()),
		msid:         fmt.Sprintf("url-stream-%s url-%s", client.ID(), client.ID()),
		streamid:     fmt.Sprintf("url-stream-%s", client.ID()),
		client:       client,
		kind:         webrtc.RTPCodecTypeAudio,
		codec:        codec,
		isScreen:     &atomic.Bool{},
		clientTracks: newClientTrackList(),
		pool:         rtppool.New(),
	}
	base.isScreen.Store(false)

	ut := &URLTrack{
		context:          localCtx,
		cancel:           cancel,
		mu:               sync.RWMutex{},
		base:             base,
		onReadCallbacks:  make([]func(interceptor.Attributes, *rtp.Packet, QualityLevel), 0),
		onEndedCallbacks: make([]func(), 0),
		isPlaying:        &atomic.Bool{},
		stopCh:           make(chan struct{}, 1),
		nextTimestamp:    0,
		log:              client.log,
	}
	return ut
}

// ID returns the unique track ID
func (t *URLTrack) ID() string { return t.base.id }

// StreamID returns the stream identifier
func (t *URLTrack) StreamID() string { return t.base.streamid }

// ClientID returns the owning client ID
func (t *URLTrack) ClientID() string { return t.base.client.ID() }

// IsSimulcast indicates no simulcast
func (t *URLTrack) IsSimulcast() bool { return false }

// IsScaleable indicates no scalability
func (t *URLTrack) IsScaleable() bool { return false }

// IsProcessed flags this track as already published
func (t *URLTrack) IsProcessed() bool {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.base.isProcessed
}

// SetSourceType sets the media/screen source type
func (t *URLTrack) SetSourceType(st TrackType) { t.base.isScreen.Store(st == TrackTypeScreen) }

// SourceType returns media or screen
func (t *URLTrack) SourceType() TrackType {
	if t.base.isScreen.Load() {
		return TrackTypeScreen
	}
	return TrackTypeMedia
}

// SetAsProcessed marks the track as processed
func (t *URLTrack) SetAsProcessed() { t.mu.Lock(); t.base.isProcessed = true; t.mu.Unlock() }

// OnRead registers a callback for each outgoing RTP packet
func (t *URLTrack) OnRead(f func(interceptor.Attributes, *rtp.Packet, QualityLevel)) {
	t.mu.Lock()
	t.onReadCallbacks = append(t.onReadCallbacks, f)
	t.mu.Unlock()
}

// IsScreen always false
func (t *URLTrack) IsScreen() bool { return false }

// IsRelay indicates this is a relay track
func (t *URLTrack) IsRelay() bool { return true }

// Kind returns audio kind
func (t *URLTrack) Kind() webrtc.RTPCodecType { return t.base.kind }

// MimeType returns the Opus MIME
func (t *URLTrack) MimeType() string { return t.base.codec.MimeType }

// TotalTracks always 1
func (t *URLTrack) TotalTracks() int { return 1 }

// Context returns the playback context
func (t *URLTrack) Context() context.Context { return t.context }

// Relay pushes packets to downstream
func (t *URLTrack) Relay(f func(webrtc.SSRC, interceptor.Attributes, *rtp.Packet)) {
	t.OnRead(func(attrs interceptor.Attributes, p *rtp.Packet, q QualityLevel) {
		f(webrtc.SSRC(t.base.codec.PayloadType), attrs, p)
	})
}

// PayloadType returns the RTP payload type
func (t *URLTrack) PayloadType() webrtc.PayloadType { return t.base.codec.PayloadType }

// OnEnded registers end-of-stream callbacks
func (t *URLTrack) OnEnded(f func()) {
	t.mu.Lock()
	t.onEndedCallbacks = append(t.onEndedCallbacks, f)
	t.mu.Unlock()
}

func (t *URLTrack) onEnded() {
	t.mu.RLock()
	for _, f := range t.onEndedCallbacks {
		f()
	}
	t.mu.RUnlock()
}

func (t *URLTrack) triggerRead(attrs interceptor.Attributes, p *rtp.Packet, q QualityLevel) {
	for _, cb := range t.onReadCallbacks {
		pkt := t.base.pool.CopyPacket(p)
		cb(attrs, pkt, q)
		t.base.pool.PutPacket(pkt)
	}
}

// IsPlaying reports if playback is active
func (t *URLTrack) IsPlaying() bool { return t.isPlaying.Load() }

// Play begins URL audio streaming
func (t *URLTrack) Play(opts URLTrackOptions) error {
	if t.IsPlaying() {
		return errors.New("already playing")
	}
	// reset timestamp at start of playback
	t.nextTimestamp = 0
	t.isPlaying.Store(true)
	fmt.Println("urltrack: start", opts.URL, opts.Loop)
	go func() {
		defer func() { t.isPlaying.Store(false) }()
		for {
			select {
			case <-t.context.Done():
				return
			case <-t.stopCh:
				return
			default:
				err := t.playOnce(opts)
				if err != nil {
					if errors.Is(err, io.EOF) && opts.Loop {
						continue
					}
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

func (t *URLTrack) playOnce(opts URLTrackOptions) error {
	file, err := GetFile(opts.URL, opts.Method, opts.Body)
	if err != nil {
		return err
	}
	ogg, hdr, err := NewOggReader(file, opts.Loop)
	if err != nil {
		return err
	}
	t.log.Infof("urltrack: OGG hdr ch=%d sr=%d", hdr.Channels, hdr.SampleRate)
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-t.context.Done():
			fmt.Println("urltrack: context done")
			return io.EOF
		case <-t.stopCh:
			fmt.Println("urltrack: stop")
			return io.EOF
		case <-tick.C:
			pkt, err := ogg.ReadPacket()
			if err != nil {
				fmt.Println("urltrack: read packet error", err)
				return err
			}
			// determine duration for RTP timestamp
			dur, err := ParsePacketDuration(pkt)
			if err != nil {
				fmt.Println("urltrack: parse packet duration error", err)
				return err
			}
			// build RTP packet with advancing timestamp
			r := &rtp.Packet{
				Header: rtp.Header{
					Version:     2,
					PayloadType: uint8(t.base.codec.PayloadType),
					Timestamp:   t.nextTimestamp,
					SSRC:        uint32(t.base.codec.PayloadType),
				},
				Payload: pkt,
			}
			// advance timestamp for next packet (convert play time to sample count)
			// dur is in time.Duration (e.g., 20ms), ClockRate is samples/sec (e.g., 48000)
			ms := dur.Milliseconds()
			delta := uint32(ms * int64(t.base.codec.ClockRate) / 1000)
			t.nextTimestamp += delta

			for _, ct := range t.base.clientTracks.GetTracks() {
				fmt.Println("urltrack: push packet to client track", ct.ID())
				x := r.Clone()
				ct.push(x, QualityAudio)
			}
			fmt.Println("urltrack: trigger read")
			t.triggerRead(nil, r, QualityAudio)
		}
	}
}

// Stop halts playback
func (t *URLTrack) Stop() {
	select {
	case t.stopCh <- struct{}{}:
	default:
	}
}

// subscribe allows a client to subscribe to this URLTrack and get RTP pushed
func (t *URLTrack) subscribe(c *Client) iClientTrack {
	// create a local track for playback
	localTrack, err := webrtc.NewTrackLocalStaticRTP(t.base.codec.RTPCodecCapability, t.ID(), t.StreamID())
	if err != nil {
		panic(err)
	}

	ctx, cancel := context.WithCancel(t.Context())

	// build the base clientTrack
	baseCT := &clientTrack{
		id:                    localTrack.ID(),
		streamid:              localTrack.StreamID(),
		context:               ctx,
		mu:                    sync.RWMutex{},
		client:                c,
		kind:                  t.base.kind,
		mimeType:              t.base.codec.MimeType,
		localTrack:            localTrack,
		remoteTrack:           nil,
		baseTrack:             t.base,
		packetmap:             &packetmap.Map{},
		isScreen:              false,
		onTrackEndedCallbacks: make([]func(), 0),
	}
	// wrap it so we override Kind/push
	urlCT := &urlClientTrack{clientTrack: baseCT}
	// register track in base list so playOnce will use urlClientTrack
	t.base.clientTracks.Add(urlCT)
	// cleanup on end
	t.OnEnded(func() {
		urlCT.onEnded()
		cancel()
	})
	return urlCT
}

// urlClientTrack wraps clientTrack to override Kind, RequestPLI, and push for URL playback
type urlClientTrack struct {
	*clientTrack
}

// Kind returns the audio kind
func (t *urlClientTrack) Kind() webrtc.RTPCodecType {
	return t.kind
}

// RequestPLI is a no-op for URL playback
func (t *urlClientTrack) RequestPLI() {}

// push writes RTP packets to the local track
func (t *urlClientTrack) push(p *rtp.Packet, _ QualityLevel) {
	if t.client.peerConnection.PC().ConnectionState() != webrtc.PeerConnectionStateConnected {
		return
	}
	ok, newSeq, _ := t.packetmap.Map(p.SequenceNumber, 0)
	if !ok {
		return
	}
	p.SequenceNumber = newSeq
	if err := t.localTrack.WriteRTP(p); err != nil {
		t.client.log.Errorf("urlclienttrack: error writing RTP packet %v", err)
	}
}

// unsubscribe logic placeholder removed
