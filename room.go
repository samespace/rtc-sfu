package sfu

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/inlivedev/sfu/pkg/recorder"
	"github.com/pion/interceptor"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
)

const (
	StateRoomOpen        = "open"
	StateRoomClosed      = "closed"
	EventRoomClosed      = "room_closed"
	EventRoomClientLeft  = "room_client_left"
	EventRecordingStart  = "recording_start"
	EventRecordingPause  = "recording_pause"
	EventRecordingResume = "recording_resume"
	EventRecordingStop   = "recording_stop"
)

type Options struct {
	EnableBridging           bool
	EnableBandwidthEstimator bool
	IceServers               []webrtc.ICEServer
	MinPlayoutDelay          uint16
	MaxPlayoutDelay          uint16
	// SettingEngine is used to configure the WebRTC engine
	// Use this to configure use of enable/disable mDNS, network types, use single port mux, etc.
	SettingEngine *webrtc.SettingEngine
}

func DefaultOptions() Options {
	settingEngine := &webrtc.SettingEngine{}
	_ = settingEngine.SetEphemeralUDPPortRange(49152, 65535)

	return Options{
		EnableBandwidthEstimator: true,
		IceServers: []webrtc.ICEServer{
			{
				URLs: []string{"stun:stun.l.google.com:19302"},
			},
		},
		MinPlayoutDelay: 100,
		MaxPlayoutDelay: 100,
		SettingEngine:   settingEngine,
	}
}

type Event struct {
	Type string
	Time time.Time
	Data map[string]interface{}
}

type Room struct {
	onRoomClosedCallbacks   []func(id string)
	onClientJoinedCallbacks []func(*Client)
	onClientLeftCallbacks   []func(*Client)
	context                 context.Context
	cancel                  context.CancelFunc
	id                      string
	token                   string
	RenegotiationChan       map[string]chan bool
	name                    string
	mu                      *sync.RWMutex
	meta                    *Metadata
	sfu                     *SFU
	state                   string
	stats                   map[string]*TrackStats
	kind                    string
	extensions              []IExtension
	OnEvent                 func(event Event)
	options                 RoomOptions
	// Recording related fields
	recorder                   *recorder.RoomRecorder
	recordingConfigMgr         *recorder.ConfigManager
	isRecordingEnabled         bool
	recordingParticipantFilter []string
}

type RoomOptions struct {
	// Configures the bitrates configuration that will be used by the room
	// Make sure to use the same bitrate config when publishing video because this is used to manage the usage bandwidth in this room
	Bitrates BitrateConfigs `json:"bitrates,omitempty"`
	// Configures the codecs that will be used by the room
	Codecs *[]string `json:"codecs,omitempty" enums:"video/VP9,video/H264,video/VP8,audio/red,audio/opus" example:"video/VP9,video/H264,video/VP8,audio/red,audio/opus"`
	// Configures the interval in nanoseconds of sending PLIs to clients that will generate keyframe, default is 0 means it will use auto PLI request only when needed.
	// More often means more bandwidth usage but more stability on video quality when packet loss, but client libs supposed to request PLI automatically when needed.
	PLIInterval *time.Duration `json:"pli_interval_ns,omitempty" example:"0"`
	// Configure the mapping of spatsial and temporal layers to quality level
	// Use this to use scalable video coding (SVC) to control the bitrate level of the video
	QualityLevels []QualityLevel `json:"quality_levels,omitempty"`
	// Configure the timeout in nanonseconds when the room is empty it will close after the timeout exceeded. Default is 5 minutes
	EmptyRoomTimeout *time.Duration `json:"empty_room_timeout_ns,ompitempty" example:"300000000000" default:"300000000000"`
}

// RecordingOptions contains options for recording the room
type RecordingOptions struct {
	Enabled        bool   `json:"enabled"`
	RecordingsPath string `json:"recordings_path"`
	FFmpegPath     string `json:"ffmpeg_path"`
	AutoMerge      bool   `json:"auto_merge"`
	// S3 upload configuration
	S3Upload          bool   `json:"s3_upload"`
	S3Endpoint        string `json:"s3_endpoint"`
	S3AccessKeyID     string `json:"s3_access_key_id"`
	S3SecretAccessKey string `json:"s3_secret_access_key"`
	S3UseSSL          bool   `json:"s3_use_ssl"`
	S3BucketName      string `json:"s3_bucket_name"`
	S3BucketPrefix    string `json:"s3_bucket_prefix"`
	DeleteAfterUpload bool   `json:"delete_after_upload"`
}

func DefaultRoomOptions() RoomOptions {
	pli := time.Duration(0)
	emptyDuration := time.Duration(3) * time.Minute
	return RoomOptions{
		Bitrates:         DefaultBitrates(),
		QualityLevels:    DefaultQualityLevels(),
		Codecs:           &[]string{webrtc.MimeTypeVP9, webrtc.MimeTypeH264, webrtc.MimeTypeVP8, "audio/red", webrtc.MimeTypeOpus},
		PLIInterval:      &pli,
		EmptyRoomTimeout: &emptyDuration,
	}
}

func newRoom(id, name string, sfu *SFU, kind string, opts RoomOptions) *Room {
	localContext, cancel := context.WithCancel(sfu.context)

	room := &Room{
		id:                 id,
		context:            localContext,
		cancel:             cancel,
		sfu:                sfu,
		token:              GenerateID(21),
		stats:              make(map[string]*TrackStats),
		state:              StateRoomOpen,
		name:               name,
		mu:                 &sync.RWMutex{},
		meta:               NewMetadata(),
		extensions:         make([]IExtension, 0),
		kind:               kind,
		options:            opts,
		isRecordingEnabled: false,
	}

	sfu.OnClientRemoved(func(client *Client) {
		room.onClientLeft(client)
	})

	go room.loopRecordStats()

	return room
}

func (r *Room) ID() string {
	return r.id
}

func (r *Room) Name() string {
	return r.name
}

func (r *Room) Kind() string {
	return r.kind
}

func (r *Room) AddExtension(extension IExtension) {
	r.extensions = append(r.extensions, extension)
}

// Close the room and stop all clients. All connected clients will stopped and removed from the room.
// All clients will get `connectionstateevent` with `closed` state.
// https://developer.mozilla.org/en-US/docs/Web/API/RTCPeerConnection/connectionstatechange_event
func (r *Room) Close() error {
	if r.state == StateRoomClosed {
		return ErrRoomIsClosed
	}

	// Stop recording if enabled
	if r.isRecordingEnabled && r.recorder != nil {
		if err := r.recorder.StopRecording(); err != nil {
			r.sfu.log.Errorf("room: failed to stop recording: %v", err)
		} else {
			r.sfu.log.Infof("room: recording stopped for room %s", r.id)

			if r.OnEvent != nil {
				eventData := map[string]interface{}{
					"room_id": r.id,
				}

				// Check if S3 upload is enabled
				if r.recordingConfigMgr != nil {
					config := r.recordingConfigMgr.GetConfig()
					if config.S3Upload {
						eventData["s3_upload"] = true
						eventData["s3_bucket"] = config.S3BucketName
						if config.S3BucketPrefix != "" {
							eventData["s3_prefix"] = config.S3BucketPrefix
						}
					}
				}

				r.OnEvent(Event{
					Type: EventRecordingStop,
					Time: time.Now(),
					Data: eventData,
				})
			}
		}

		r.isRecordingEnabled = false
		r.recorder = nil
	}

	r.cancel()

	r.sfu.Stop()

	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, callback := range r.onRoomClosedCallbacks {
		callback(r.id)
	}

	r.state = StateRoomClosed

	return nil
}

// Stopping client is async, it will just stop the client and return immediately
// You should use OnClientLeft to get notified when the client is actually stopped
func (r *Room) StopClient(id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	var client *Client

	var err error

	if client, err = r.sfu.GetClient(id); err != nil {
		return err
	}

	return client.stop()
}

func (r *Room) AddClient(id, name string, opts ClientOptions) (*Client, error) {
	if r.state == StateRoomClosed {
		return nil, ErrRoomIsClosed
	}

	opts.qualityLevels = r.options.QualityLevels

	for _, ext := range r.extensions {
		if err := ext.OnBeforeClientAdded(r, id); err != nil {
			return nil, err
		}
	}

	client, _ := r.sfu.GetClient(id)
	if client != nil {
		return nil, ErrClientExists
	}

	client = r.sfu.NewClient(id, name, opts)

	// stop client if not connecting for a specific time
	initConnection := true
	go func() {
		timeout, cancel := context.WithTimeout(client.context, opts.IdleTimeout)
		defer cancel()

		mu := sync.Mutex{}

		connectingChan := make(chan bool)

		timeoutReached := false

		client.OnConnectionStateChanged(func(state webrtc.PeerConnectionState) {
			mu.Lock()
			defer mu.Unlock()

			if initConnection && state == webrtc.PeerConnectionStateConnected && !timeoutReached {
				connectingChan <- true

				// set to false so we don't send the connectingChan again because no more listener
				initConnection = false
			}
		})

		select {
		case <-timeout.Done():
			r.sfu.log.Warnf("room: client is not connected after added, stopping client...")
			_ = client.stop()
			timeoutReached = true

		case <-connectingChan:
			return
		}
	}()

	client.OnJoined(func() {
		r.onClientJoined(client)
	})

	// Set up recording for the client audio tracks if recording is enabled
	// if r.isRecordingEnabled && r.recorder != nil {
	// 	client.onTrack = func(track ITrack) {
	// 		// Skip recording for monitor clients
	// 		// if strings.HasPrefix(client.ID(), "monitor_") {
	// 		// 	r.sfu.log.Debugf("room: skipping recording for monitor client: %s", client.ID())
	// 		// 	return // Don't record monitor clients
	// 		// }

	// 		// If we have a participants filter and this client is not in the list, don't record
	// 		// if r.recordingParticipantFilter != nil && len(r.recordingParticipantFilter) > 0 {
	// 		// 	found := false
	// 		// 	for _, id := range r.recordingParticipantFilter {
	// 		// 		if client.ID() == id {
	// 		// 			found = true
	// 		// 			break
	// 		// 		}
	// 		// 	}
	// 		// 	if !found {
	// 		// 		return // Don't record this client
	// 		// 	}
	// 		// }

	// 		if track.Kind() == webrtc.RTPCodecTypeAudio {
	// 			// Use type switch to handle both Track and AudioTrack types
	// 			var codecParams webrtc.RTPCodecParameters
	// 			switch t := track.(type) {
	// 			case *Track:
	// 				codecParams = t.base.codec
	// 			case *AudioTrack:
	// 				codecParams = t.Track.base.codec
	// 			default:
	// 				r.sfu.log.Warnf("room: unknown track type: %T", track)
	// 				return
	// 			}

	// 			sampleRate := uint32(48000) // Default for Opus
	// 			channelCount := uint16(1)   // Default for Opus

	// 			if codecParams.ClockRate > 0 {
	// 				sampleRate = uint32(codecParams.ClockRate)
	// 			}

	// 			// Handle channels correctly
	// 			if codecParams.SDPFmtpLine != "" {
	// 				// For Opus, channel count is typically in the codec parameters
	// 				// but alternatively we could use a fixed value of 1 (mono) or 2 (stereo)
	// 				// since we know it's an audio track
	// 				channelCount = 1 // Mono is typical for voice calls
	// 			}

	// 			// Add participant to recording
	// 			if r.recorder != nil {
	// 				if err := r.recorder.AddParticipant(client.ID(), track.ID(), sampleRate, channelCount); err != nil {
	// 					r.sfu.log.Errorf("room: failed to add participant to recording: %v", err)
	// 					return
	// 				}

	// 				// Set up RTP packet interception for recording
	// 				track.OnRead(func(attrs interceptor.Attributes, packet *rtp.Packet, quality QualityLevel) {
	// 					if err := r.recorder.WriteRTP(client.ID(), packet); err != nil {
	// 						// Only log this at debug level to avoid flooding logs
	// 						r.sfu.log.Debugf("room: failed to write RTP packet: %v", err)
	// 					}
	// 				})
	// 			}
	// 		}
	// 	}
	// }

	return client, nil
}

// Generate a unique client ID for this room
func (r *Room) CreateClientID() string {
	return GenerateID(21)
}

// Use this to get notified when a room is closed
func (r *Room) OnRoomClosed(callback func(id string)) {
	r.onRoomClosedCallbacks = append(r.onRoomClosedCallbacks, callback)
}

// Use this to get notified when a client is stopped and completly removed from the room
func (r *Room) OnClientLeft(callback func(client *Client)) {
	r.onClientLeftCallbacks = append(r.onClientLeftCallbacks, callback)
}

func (r *Room) onClientLeft(client *Client) {
	r.mu.RLock()
	callbacks := r.onClientLeftCallbacks
	exts := r.extensions
	r.mu.RUnlock()
	for _, callback := range callbacks {
		callback(client)
	}

	for _, ext := range exts {
		ext.OnClientRemoved(r, client)
	}

	// update the latest stats from client before they left
	r.mu.Lock()
	defer r.mu.Unlock()

	r.stats[client.ID()] = client.stats.TrackStats
}

func (r *Room) onClientJoined(client *Client) {
	for _, callback := range r.onClientJoinedCallbacks {
		callback(client)
	}

	for _, ext := range r.extensions {
		ext.OnClientAdded(r, client)
	}
}

func (r *Room) OnClientJoined(callback func(client *Client)) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.onClientJoinedCallbacks = append(r.onClientJoinedCallbacks, callback)
}

func (r *Room) SFU() *SFU {
	return r.sfu
}

// Get the room real time stats. This will return the current room stats.
// The client stats and it's tracks will be removed from the stats if the client or track is removed.
// But the aggregated stats will still be there and included in the room stats even if they're removed.
func (r *Room) Stats() RoomStats {
	var (
		bytesReceived    uint64
		bytesSent        uint64
		bitratesSent     uint64
		bitratesReceived uint64
	)

	clientStats := make(map[string]ClientTrackStats)

	r.mu.RLock()

	defer r.mu.RUnlock()

	for _, cstats := range r.stats {
		for _, stat := range cstats.receivers {
			bytesReceived += stat.BytesReceived
		}

		for _, stat := range cstats.receiverBitrates {
			bitratesReceived += uint64(stat)
		}

		for _, stat := range cstats.senderBitrates {
			bitratesSent += uint64(stat)
		}

		for _, stat := range cstats.senders {
			bytesSent += stat.OutboundRTPStreamStats.BytesSent
		}

	}

	roomStats := RoomStats{
		ActiveSessions: r.sfu.TotalActiveSessions(),
		ClientsCount:   0,
		BytesIngress:   bytesReceived,
		BytesEgress:    bytesSent,
		Timestamp:      time.Now(),
		ClientStats:    clientStats,
	}

	for id, c := range r.sfu.clients.GetClients() {
		roomStats.ClientStats[id] = c.Stats()

		roomStats.ClientsCount++

		for _, track := range roomStats.ClientStats[id].Receives {
			if track.Kind == webrtc.RTPCodecTypeAudio {
				roomStats.ReceivedTracks.Audio++
			} else {
				roomStats.ReceivedTracks.Video++
			}

			roomStats.BitrateReceived += uint64(track.CurrentBitrate)
		}

		for _, track := range roomStats.ClientStats[id].Sents {
			if track.Kind == webrtc.RTPCodecTypeAudio {
				roomStats.SentTracks.Audio++
			} else {
				roomStats.SentTracks.Video++
			}

			roomStats.BitrateSent += uint64(track.CurrentBitrate)
		}
	}

	return roomStats
}

func (r *Room) updateStats() {
	r.mu.Lock()
	defer r.mu.Unlock()

	for _, client := range r.sfu.clients.GetClients() {
		r.stats[client.ID()] = client.stats.TrackStats
	}
}

func (r *Room) CreateDataChannel(label string, opts DataChannelOptions) error {
	return r.sfu.CreateDataChannel(label, opts)
}

// BitrateConfigs return the current bitrate configuration that used in bitrate controller
// Client should use this to configure the bitrate when publishing media tracks
// Inconsistent bitrate configuration between client and server will result missed bitrate calculation and
// could affecting packet loss and media quality
func (r *Room) BitrateConfigs() BitrateConfigs {
	return r.sfu.bitrateConfigs
}

// CodecPreferences return the current codec preferences that used in SFU
// Client should use this to configure the used codecs when publishing media tracks
// Inconsistent codec preferences between client and server can make the SFU cannot handle the codec properly
func (r *Room) CodecPreferences() []string {
	return r.sfu.codecs
}

func (r *Room) Context() context.Context {
	return r.context
}

func (r *Room) Meta() *Metadata {
	return r.meta
}

func (r *Room) Options() RoomOptions {
	return r.options
}

func (r *Room) loopRecordStats() {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	ctx, cancel := context.WithCancel(r.context)
	defer cancel()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.updateStats()
		}
	}
}

// StartRecording starts recording for the room
func (r *Room) StartRecording(identifier string, s3Config *RecordingOptions, participantIDs []string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.state == StateRoomClosed {
		return ErrRoomIsClosed
	}

	// If identifier is empty, use the room ID
	if identifier == "" {
		identifier = r.id
	}

	if !r.isRecordingEnabled {
		// Initialize recording config
		r.recordingConfigMgr = recorder.NewConfigManager()
		config := r.recordingConfigMgr.GetConfig()

		// Store the participant filter for future clients that join during recording
		r.recordingParticipantFilter = participantIDs

		// Set default configuration
		config.RecordingsPath = "recordings"
		config.FFmpegPath = "ffmpeg"
		config.AutoMerge = true

		// Override with provided S3 configuration if available
		if s3Config != nil {
			// Update recording path if provided
			if s3Config.RecordingsPath != "" {
				config.RecordingsPath = s3Config.RecordingsPath
			}

			// Update FFmpeg path if provided
			if s3Config.FFmpegPath != "" {
				config.FFmpegPath = s3Config.FFmpegPath
			}

			// Update auto merge if provided
			if !s3Config.AutoMerge {
				config.AutoMerge = s3Config.AutoMerge
			}

			// Set S3 configuration
			config.S3Upload = s3Config.S3Upload
			config.S3Endpoint = s3Config.S3Endpoint
			config.S3AccessKey = s3Config.S3AccessKeyID
			config.S3SecretKey = s3Config.S3SecretAccessKey
			config.S3UseSSL = s3Config.S3UseSSL
			config.S3BucketName = s3Config.S3BucketName
			config.S3BucketPrefix = s3Config.S3BucketPrefix
			config.DeleteAfterUpload = s3Config.DeleteAfterUpload
		}

		_ = r.recordingConfigMgr.UpdateConfig(config)

		// Create room recorder
		rec, err := recorder.NewRoomRecorder(r.context, identifier, config.RecordingsPath, config.FFmpegPath)
		if err != nil {
			return err
		}

		// Configure S3 upload if enabled
		if config.S3Upload {
			s3Creds := r.recordingConfigMgr.GetS3Credentials()
			if err := rec.SetS3Config(true, s3Creds, config.DeleteAfterUpload); err != nil {
				r.sfu.log.Warnf("room: failed to configure S3: %v", err)
			}
		}

		r.recorder = rec
		r.isRecordingEnabled = true

		// Add all existing clients to the recorder
		clients := r.sfu.GetClients()
		for _, client := range clients {
			// Skip monitor clients
			if strings.HasPrefix(client.ID(), "monitor_") {
				r.sfu.log.Debugf("room: skipping recording for monitor client: %s", client.ID())
				continue // Don't record monitor clients
			}

			// Skip clients not in the participantIDs list if specific participants are provided
			if len(participantIDs) > 0 {
				found := false
				for _, id := range participantIDs {
					if client.ID() == id {
						found = true
						break
					}
				}
				if !found {
					continue // Skip this client as it's not in the list
				}
			}

			for _, track := range client.Tracks() {
				if track.Kind() == webrtc.RTPCodecTypeAudio {
					// Use type switch to handle both Track and AudioTrack types
					var codecParams webrtc.RTPCodecParameters
					switch t := track.(type) {
					case *Track:
						codecParams = t.base.codec
					case *AudioTrack:
						codecParams = t.Track.base.codec
					default:
						r.sfu.log.Warnf("room: unknown track type: %T", track)
						continue
					}

					sampleRate := uint32(48000) // Default for Opus
					channelCount := uint16(1)   // Default for Opus

					if codecParams.ClockRate > 0 {
						sampleRate = uint32(codecParams.ClockRate)
					}

					// Handle channels correctly
					if codecParams.SDPFmtpLine != "" {
						// For Opus, channel count is typically in the codec parameters
						// but alternatively we could use a fixed value of 1 (mono) or 2 (stereo)
						// since we know it's an audio track
						channelCount = 1 // Mono is typical for voice calls
					}

					// Add participant to recording
					if err := r.recorder.AddParticipant(client.ID(), track.ID(), sampleRate, channelCount); err != nil {
						r.sfu.log.Errorf("room: failed to add participant to recording: %v", err)
						continue
					}

					// Set up RTP packet interception for recording
					track.OnRead(func(attrs interceptor.Attributes, packet *rtp.Packet, quality QualityLevel) {
						if err := r.recorder.WriteRTP(client.ID(), packet); err != nil {
							// Only log this at debug level to avoid flooding logs
							r.sfu.log.Debugf("room: failed to write RTP packet: %v", err)
						}
					})
				}
			}
		}

		if r.OnEvent != nil {
			eventData := map[string]interface{}{
				"room_id":    r.id,
				"identifier": identifier,
			}

			// Add information about specific participants if applicable
			if participantIDs != nil && len(participantIDs) > 0 {
				eventData["specific_participants"] = true
				eventData["participant_ids"] = participantIDs
			}

			r.OnEvent(Event{
				Type: EventRecordingStart,
				Time: time.Now(),
				Data: eventData,
			})
		}
	} else if r.recorder != nil {
		// Recording is already enabled, just resume if paused
		if r.recorder.GetState() == recorder.RecordingStatePaused {
			if err := r.recorder.ResumeRecording(); err != nil {
				return err
			}

			if r.OnEvent != nil {
				r.OnEvent(Event{
					Type: EventRecordingResume,
					Time: time.Now(),
					Data: map[string]interface{}{
						"room_id": r.id,
					},
				})
			}
		}
	}

	return nil
}

// PauseRecording pauses recording for the room
func (r *Room) PauseRecording() error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if !r.isRecordingEnabled || r.recorder == nil {
		return nil // Nothing to pause
	}

	if err := r.recorder.PauseRecording(); err != nil {
		return err
	}

	if r.OnEvent != nil {
		r.OnEvent(Event{
			Type: EventRecordingPause,
			Time: time.Now(),
			Data: map[string]interface{}{
				"room_id": r.id,
			},
		})
	}

	return nil
}

// ResumeRecording resumes recording for the room if it was paused
func (r *Room) ResumeRecording() error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if !r.isRecordingEnabled || r.recorder == nil {
		return nil // Nothing to resume
	}

	if r.recorder.GetState() != recorder.RecordingStatePaused {
		return nil // Not paused, nothing to resume
	}

	if err := r.recorder.ResumeRecording(); err != nil {
		return err
	}

	if r.OnEvent != nil {
		r.OnEvent(Event{
			Type: EventRecordingResume,
			Time: time.Now(),
			Data: map[string]interface{}{
				"room_id": r.id,
			},
		})
	}

	return nil
}

// StopRecording stops recording for the room
func (r *Room) StopRecording() error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if !r.isRecordingEnabled || r.recorder == nil {
		return nil // Nothing to stop
	}

	// First stop the recorder (this sets its state to stopped)
	if err := r.recorder.StopRecording(); err != nil {
		return err
	}

	// We delay nullifying the recorder to allow any in-flight RTP packets to be processed
	// The WriteRTP method will now check for stopped state and ignore packets

	// Set recording as disabled
	r.isRecordingEnabled = false

	// Clear participant filter
	r.recordingParticipantFilter = nil

	if r.OnEvent != nil {
		eventData := map[string]interface{}{
			"room_id": r.id,
		}

		// Check if S3 upload is enabled
		if r.recordingConfigMgr != nil {
			config := r.recordingConfigMgr.GetConfig()
			if config.S3Upload {
				eventData["s3_upload"] = true
				eventData["s3_bucket"] = config.S3BucketName
				if config.S3BucketPrefix != "" {
					eventData["s3_prefix"] = config.S3BucketPrefix
				}
			}
		}

		r.OnEvent(Event{
			Type: EventRecordingStop,
			Time: time.Now(),
			Data: eventData,
		})
	}

	// Schedule a cleanup after a short delay to allow in-flight packets to be processed
	// This is done in a goroutine to not block the current thread
	recorderId := r.recorder
	go func() {
		// Wait a short period to allow in-flight packets to be processed
		time.Sleep(2 * time.Second)

		// Acquire lock again for the delayed recorder nullification
		r.mu.Lock()
		defer r.mu.Unlock()

		// Only nullify if this is still the same recorder
		if r.recorder == recorderId {
			r.recorder = nil
		}
	}()

	return nil
}

// IsRecordingEnabled returns whether recording is enabled for the room
func (r *Room) IsRecordingEnabled() bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.isRecordingEnabled
}

// GetRecordingState returns the current state of recording
func (r *Room) GetRecordingState() string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	if !r.isRecordingEnabled || r.recorder == nil {
		return string(recorder.RecordingStateStopped)
	}

	return string(r.recorder.GetState())
}
