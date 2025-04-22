package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/inlivedev/sfu"
	"github.com/pion/webrtc/v4"
)

func main() {
	// Create a context that will be canceled on SIGTERM or SIGINT
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Handle OS signals
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigChan
		log.Println("Shutting down...")
		cancel()
	}()

	// Create a setting engine
	settingEngine := &webrtc.SettingEngine{}
	_ = settingEngine.SetEphemeralUDPPortRange(49152, 65535)

	// Create SFU options
	sfuOptions := sfu.Options{
		IceServers: []webrtc.ICEServer{
			{
				URLs: []string{"stun:stun.l.google.com:19302"},
			},
		},
		MinPlayoutDelay:          100,
		MaxPlayoutDelay:          300,
		SettingEngine:            settingEngine,
		EnableBandwidthEstimator: true,
	}

	// Create a manager
	manager := sfu.NewManager(ctx, "recording-example", sfuOptions)

	// Room options with recording enabled
	pli := time.Duration(0)
	roomOpts := sfu.RoomOptions{
		Bitrates:      sfu.DefaultBitrates(),
		QualityLevels: sfu.DefaultQualityLevels(),
		Codecs:        &[]string{webrtc.MimeTypeVP8, webrtc.MimeTypeOpus},
		PLIInterval:   &pli,
		Recording: &sfu.RecordingOptions{
			Enabled:        true,
			RecordingsPath: "recordings",
			FFmpegPath:     "ffmpeg",
			AutoMerge:      true,
		},
	}

	// Create a room
	roomID := "123"
	room, err := manager.NewRoom(roomID, "Recording Example Room", "conference", roomOpts)
	if err != nil {
		log.Fatalf("Failed to create room: %v", err)
	}

	// Set up event handling
	room.OnEvent = func(event sfu.Event) {
		switch event.Type {
		case sfu.EventRecordingStart:
			log.Printf("Recording started for room %s", event.Data["room_id"])
		case sfu.EventRecordingPause:
			log.Printf("Recording paused for room %s", event.Data["room_id"])
		case sfu.EventRecordingResume:
			log.Printf("Recording resumed for room %s", event.Data["room_id"])
		case sfu.EventRecordingStop:
			log.Printf("Recording stopped for room %s", event.Data["room_id"])
		}
	}

	// Wait for clients to connect
	log.Println("SFU is running with recording enabled")
	log.Println("Room ID:", roomID)
	log.Println("Press Ctrl+C to stop")

	// Example of dynamic recording control (in real usage, this would be triggered by API calls)
	go func() {
		// Pause recording after 30 seconds
		time.Sleep(30 * time.Second)
		if err := room.PauseRecording(); err != nil {
			log.Printf("Failed to pause recording: %v", err)
		} else {
			log.Println("Recording paused")
		}

		// Resume recording after 10 more seconds
		time.Sleep(10 * time.Second)
		if err := room.StartRecording(); err != nil {
			log.Printf("Failed to resume recording: %v", err)
		} else {
			log.Println("Recording resumed")
		}

		// Get the recording state
		time.Sleep(5 * time.Second)
		state := room.GetRecordingState()
		log.Printf("Current recording state: %s", state)
	}()

	// Wait for context to be done (i.e., for Ctrl+C)
	<-ctx.Done()

	// Clean up
	if err := room.Close(); err != nil {
		log.Printf("Error closing room: %v", err)
	}

	log.Println("SFU stopped")
}
