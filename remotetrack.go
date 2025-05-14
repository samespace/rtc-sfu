package sfu

import (
	"context"
	"fmt"
	"io"
	"sync"
	"time"

	"sync/atomic"

	"github.com/inlivedev/sfu/pkg/networkmonitor"
	"github.com/inlivedev/sfu/pkg/recording"
	"github.com/inlivedev/sfu/pkg/rtppool"
	"github.com/pion/interceptor"
	"github.com/pion/interceptor/pkg/stats"
	"github.com/pion/logging"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
)

type remoteTrack struct {
	context               context.Context
	cancel                context.CancelFunc
	mu                    sync.RWMutex
	track                 IRemoteTrack
	onRead                func(interceptor.Attributes, *rtp.Packet)
	onPLI                 func()
	bitrate               *atomic.Uint32
	previousBytesReceived *atomic.Uint64
	currentBytesReceived  *atomic.Uint64
	latestUpdatedTS       *atomic.Uint64
	lastPLIRequestTime    time.Time
	onEndedCallbacks      []func()
	statsGetter           stats.Getter
	onStatsUpdated        func(*stats.Stats)
	log                   logging.LeveledLogger
	rtppool               *rtppool.RTPPool
	// Recording related fields
	recorder         recording.Recorder
	isAudioTrack     bool
	recordingManager *recording.RecordingManager
	clientID         string
	trackID          string
}

func newRemoteTrack(ctx context.Context, log logging.LeveledLogger, useBuffer bool, track IRemoteTrack, minWait, maxWait, pliInterval time.Duration, onPLI func(), statsGetter stats.Getter, onStatsUpdated func(*stats.Stats), onRead func(interceptor.Attributes, *rtp.Packet), pool *rtppool.RTPPool, onNetworkConditionChanged func(networkmonitor.NetworkConditionType)) *remoteTrack {
	localctx, cancel := context.WithCancel(ctx)

	rt := &remoteTrack{
		context:               localctx,
		cancel:                cancel,
		mu:                    sync.RWMutex{},
		track:                 track,
		bitrate:               &atomic.Uint32{},
		previousBytesReceived: &atomic.Uint64{},
		currentBytesReceived:  &atomic.Uint64{},
		latestUpdatedTS:       &atomic.Uint64{},
		onEndedCallbacks:      make([]func(), 0),
		statsGetter:           statsGetter,
		onStatsUpdated:        onStatsUpdated,
		onPLI:                 onPLI,
		onRead:                onRead,
		log:                   log,
		rtppool:               pool,
		isAudioTrack:          track.Kind() == webrtc.RTPCodecTypeAudio,
	}

	if pliInterval > 0 {
		rt.enableIntervalPLI(pliInterval)
	}

	go rt.readRTP()

	return rt
}

func (t *remoteTrack) Context() context.Context {
	return t.context
}

func (t *remoteTrack) SetRecordingManager(manager *recording.RecordingManager, clientID, trackID string) {
	t.mu.Lock()
	defer t.mu.Unlock()

	fmt.Printf("### RECORDING DEBUG: Setting recording manager for track %s, client %s\n", trackID, clientID)

	if manager == nil {
		fmt.Printf("### RECORDING DEBUG: Received nil recording manager for track %s\n", trackID)
		return
	}

	t.recordingManager = manager
	t.clientID = clientID
	t.trackID = trackID
	fmt.Printf("### RECORDING DEBUG: Successfully set recording manager for track %s, client %s\n", trackID, clientID)
}

func (t *remoteTrack) readRTP() {
	readCtx, cancel := context.WithCancel(t.context)

	defer cancel()

	defer t.cancel()

	defer t.onEnded()

	buffer := make([]byte, 1500)
	packetCount := 0
	recordedPacketCount := 0
	errorCount := 0
	lastErrorLogTime := time.Now()

	if t.isAudioTrack {
		fmt.Printf("### RECORDING DEBUG: Starting to read RTP for audio track: %s\n", t.track.ID())
	}

	for {
		select {
		case <-readCtx.Done():
			if t.isAudioTrack {
				fmt.Printf("### RECORDING DEBUG: Stopped reading RTP for audio track: %s, processed %d packets, recorded %d packets, errors %d\n",
					t.track.ID(), packetCount, recordedPacketCount, errorCount)
			}
			return
		default:
			if err := t.track.SetReadDeadline(time.Now().Add(1 * time.Second)); err != nil {
				fmt.Printf("### RECORDING DEBUG: Set read deadline error - %s\n", err.Error())
				return
			}

			n, attrs, readErr := t.track.Read(buffer)
			if readErr != nil {
				if readErr == io.EOF {
					fmt.Printf("### RECORDING DEBUG: Track ended %s\n", t.track.ID())
					return
				}

				if t.isAudioTrack && packetCount%100 == 0 {
					fmt.Printf("### RECORDING DEBUG: Read error on audio track %s: %s\n", t.track.ID(), readErr.Error())
				}
				continue
			}

			// could be read deadline reached
			if n == 0 {
				continue
			}

			p := t.rtppool.GetPacket()

			if err := p.Unmarshal(buffer[:n]); err != nil {
				fmt.Printf("### RECORDING DEBUG: Unmarshal error on track %s: %s\n", t.track.ID(), err.Error())
				t.rtppool.PutPacket(p)
				continue
			}

			if !t.IsRelay() {
				go t.updateStats()
			}

			packetCount++

			// Log packet info every 500 packets
			if t.isAudioTrack && packetCount%500 == 0 {
				fmt.Printf("### RECORDING DEBUG: Track %s: Processed %d packets, recorded %d packets, errors %d\n",
					t.track.ID(), packetCount, recordedPacketCount, errorCount)
			}

			// Handle recording for audio tracks
			if t.isAudioTrack && t.recordingManager != nil {
				t.mu.RLock()
				clientID := t.clientID
				trackID := t.trackID
				manager := t.recordingManager
				t.mu.RUnlock()

				if clientID != "" && trackID != "" && manager != nil {
					// Log first packet to be recorded
					if recordedPacketCount == 0 {
						fmt.Printf("### RECORDING DEBUG: First packet for track %s, client %s, PT: %d, TS: %d, seq: %d, payload size: %d\n",
							trackID, clientID, p.PayloadType, p.Timestamp, p.SequenceNumber, len(p.Payload))
					}

					if err := manager.WriteRTP(clientID, trackID, p); err != nil {
						errorCount++

						// Log detailed error info but limit frequency to avoid flooding
						if time.Since(lastErrorLogTime) > 5*time.Second {
							fmt.Printf("### RECORDING DEBUG: Failed to write RTP packet to recorder: %v (packet #%d, total errors: %d)\n",
								err, packetCount, errorCount)
							lastErrorLogTime = time.Now()
						}
					} else {
						recordedPacketCount++
						if recordedPacketCount == 1 || recordedPacketCount%500 == 0 {
							fmt.Printf("### RECORDING DEBUG: Recorded %d packets for track %s, client %s, current packet PT: %d, TS: %d, seq: %d\n",
								recordedPacketCount, trackID, clientID, p.PayloadType, p.Timestamp, p.SequenceNumber)
						}
					}
				} else if t.isAudioTrack && packetCount == 1 {
					fmt.Printf("### RECORDING DEBUG: Cannot record - Missing recording info: clientID=%s, trackID=%s, manager=%v\n",
						clientID, trackID, manager != nil)
				}
			}

			t.onRead(attrs, p)
			t.rtppool.PutPacket(p)
		}
	}
}

func (t *remoteTrack) updateStats() {
	s := t.statsGetter.Get(uint32(t.track.SSRC()))
	if s == nil {
		t.log.Warnf("remotetrack: stats not found for track: ", t.track.SSRC())
		return
	}

	// update the stats if the last update equal or more than 1 second
	latestUpdated := t.latestUpdatedTS.Load()
	if time.Since(time.Unix(0, int64(latestUpdated))).Seconds() <= 1 {
		return
	}

	if latestUpdated == 0 {
		t.latestUpdatedTS.Store(uint64(s.LastPacketReceivedTimestamp.UnixNano()))
		return
	}

	t.latestUpdatedTS.Store(uint64(s.LastPacketReceivedTimestamp.UnixNano()))

	if t.onStatsUpdated != nil {
		t.onStatsUpdated(s)
	}
}

func (t *remoteTrack) Track() IRemoteTrack {
	return t.track
}

func (t *remoteTrack) SendPLI() {
	t.mu.Lock()
	defer t.mu.Unlock()

	// return if there is a pending PLI request
	maxGapSeconds := 250 * time.Millisecond
	requestGap := time.Since(t.lastPLIRequestTime)

	if requestGap < maxGapSeconds {
		return // ignore PLI request
	}

	t.lastPLIRequestTime = time.Now()

	go t.onPLI()
}

func (t *remoteTrack) enableIntervalPLI(interval time.Duration) {
	go func() {
		ctx, cancel := context.WithCancel(t.context)
		defer cancel()
		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				t.SendPLI()
			}
		}
	}()
}

func (t *remoteTrack) IsRelay() bool {
	_, ok := t.track.(*RelayTrack)
	return ok
}

func (t *remoteTrack) OnEnded(f func()) {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.onEndedCallbacks = append(t.onEndedCallbacks, f)
}

func (t *remoteTrack) onEnded() {
	t.mu.RLock()
	defer t.mu.RUnlock()

	for _, f := range t.onEndedCallbacks {
		f()
	}
}
