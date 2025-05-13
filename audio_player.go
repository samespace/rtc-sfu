package sfu

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media/oggreader"
)

// AudioPlayer injects Opus Ogg audio into the SFU via an internal relay track.
// It is reuseable across features such as room-wide prompts or per-participant hold music.
// Safe for concurrent usage; Stop() is idempotent.
type AudioPlayer struct {
	ctx    context.Context
	cancel context.CancelFunc

	sfu            *SFU
	trackID        string
	streamID       string
	rtpChan        chan *rtp.Packet
	holderClientID string
	finished       chan struct{}
}

// NewAudioPlayer creates the relay track and starts a goroutine that streams the given URL/path.
// If loop is true the clip restarts automatically.
func NewAudioPlayer(parent context.Context, sfu *SFU, url string, loop bool) (*AudioPlayer, error) {
	ctx, cancel := context.WithCancel(parent)

	p := &AudioPlayer{
		ctx:      ctx,
		cancel:   cancel,
		sfu:      sfu,
		trackID:  "audio_inject_" + GenerateID(8),
		streamID: "audio_inject_stream",
		rtpChan:  make(chan *rtp.Packet, 100),
		finished: make(chan struct{}),
	}

	// pick or create owner client
	var owner *Client
	for _, c := range sfu.GetClients() {
		owner = c
		break
	}
	if owner == nil {
		opts := DefaultClientOptions()
		opts.Log = sfu.log
		owner = sfu.createClient("_internal_audio_"+GenerateID(6), "system", webrtc.Configuration{}, opts)
	}

	ssrc := webrtc.SSRC(randomSSRC())
	if err := sfu.AddRelayTrack(ctx, p.trackID, p.streamID, "", owner, webrtc.RTPCodecTypeAudio, ssrc, webrtc.MimeTypeOpus, p.rtpChan); err != nil {
		cancel()
		return nil, err
	}

	p.holderClientID = owner.ID()

	go p.loop(url, loop, ssrc)

	return p, nil
}

func (p *AudioPlayer) ID() string            { return p.trackID }
func (p *AudioPlayer) ClientID() string      { return p.holderClientID }
func (p *AudioPlayer) Stop()                 { p.cancel() }
func (p *AudioPlayer) Done() <-chan struct{} { return p.finished }

// --- internal ---

func (p *AudioPlayer) loop(url string, loop bool, ssrc webrtc.SSRC) {
	defer func() {
		close(p.finished)
	}()

	var seq uint16
	var ts uint32

	for {
		if p.ctx.Err() != nil {
			return
		}

		r, err := openAudioSource(url)
		if err != nil {
			time.Sleep(time.Second)
			continue
		}

		ogg, oggHeader, err := oggreader.NewWith(r)
		if err != nil {
			r.Close()
			time.Sleep(time.Second)
			continue
		}
		if oggHeader.Channels != 1 {
			r.Close()
			return
		}

		var lastGranule uint64

		for {
			if p.ctx.Err() != nil {
				r.Close()
				return
			}

			pageData, pageHeader, err := ogg.ParseNextPage()
			switch {
			case errors.Is(err, io.EOF):
				if !loop {
					r.Close()
					return
				}
				break // restart outer loop
			case err != nil:
				break // restart outer loop on decode error
			}
			if err != nil {
				break
			}

			sampleCount := uint32(pageHeader.GranulePosition - lastGranule)
			lastGranule = pageHeader.GranulePosition
			ts += sampleCount

			pkt := &rtp.Packet{Header: rtp.Header{Version: 2, PayloadType: 111, SequenceNumber: seq, Timestamp: ts, SSRC: uint32(ssrc), Marker: true}, Payload: pageData}
			seq++

			select {
			case p.rtpChan <- pkt:
			case <-p.ctx.Done():
				r.Close()
				return
			default:
				// channel full, drop
			}

			// pacing: sleep for the duration represented by this opus frame
			if sampleCount > 0 {
				time.Sleep(time.Duration(sampleCount) * time.Second / 48000)
			}
		}
		r.Close()
		return
	}
}

// Helpers

func openAudioSource(url string) (io.ReadCloser, error) {
	if strings.HasPrefix(url, "http://") || strings.HasPrefix(url, "https://") {
		resp, err := http.Get(url)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode != http.StatusOK {
			return nil, errors.New("non-200: " + resp.Status)
		}
		return resp.Body, nil
	}
	return os.Open(url)
}

func randomSSRC() uint32 {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return binary.BigEndian.Uint32(b[:])
}
