package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/inlivedev/sfu"
)

func main() {
	// Create context with cancellation
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Create an SFU manager
	sfuOpts := sfu.DefaultOptions()
	manager := sfu.NewManager(ctx, "s3-recording-demo", sfuOpts)

	// Configure a room with standard options (no recording configuration)
	roomOpts := sfu.DefaultRoomOptions()

	// Create a new room
	roomID := manager.CreateRoomID()
	room, err := manager.NewRoom(roomID, "Test Room", sfu.RoomTypeLocal, roomOpts)
	if err != nil {
		log.Fatalf("Failed to create room: %v", err)
	}

	// Set up event handler to monitor recording events
	room.OnEvent = func(event sfu.Event) {
		switch event.Type {
		case sfu.EventRecordingStart:
			log.Printf("Recording started: room=%s identifier=%v",
				event.Data["room_id"], event.Data["identifier"])
		case sfu.EventRecordingPause:
			log.Printf("Recording paused: room=%s", event.Data["room_id"])
		case sfu.EventRecordingResume:
			log.Printf("Recording resumed: room=%s", event.Data["room_id"])
		case sfu.EventRecordingStop:
			log.Printf("Recording stopped: room=%s", event.Data["room_id"])
			if s3Upload, ok := event.Data["s3_upload"].(bool); ok && s3Upload {
				log.Printf("Recording will be uploaded to S3: bucket=%s prefix=%s",
					event.Data["s3_bucket"], event.Data["s3_prefix"])
			}
		}
	}

	// Generate a unique recording ID
	recordingID := fmt.Sprintf("call-%d", time.Now().Unix())
	log.Printf("Starting recording with ID: %s", recordingID)

	// Configure recording with S3 upload - this is the ONLY place where recording is configured
	s3Config := &sfu.RecordingOptions{
		// Basic recording configuration
		Enabled:        true,
		RecordingsPath: "recordings",
		FFmpegPath:     "ffmpeg",
		AutoMerge:      true,

		// S3 upload configuration
		S3Upload:          true,
		S3Endpoint:        os.Getenv("S3_ENDPOINT"),
		S3AccessKeyID:     os.Getenv("S3_ACCESS_KEY_ID"),
		S3SecretAccessKey: os.Getenv("S3_SECRET_ACCESS_KEY"),
		S3UseSSL:          true,
		S3BucketName:      os.Getenv("S3_BUCKET_NAME"),
		S3BucketPrefix:    "meetings/",
		DeleteAfterUpload: true,
	}

	// Start recording with the configuration
	if err := room.StartRecording(recordingID, s3Config); err != nil {
		log.Fatalf("Failed to start recording: %v", err)
	}

	// Wait for 10 seconds
	log.Println("Recording... (waiting 10 seconds)")
	time.Sleep(10 * time.Second)

	// Stop the recording
	log.Println("Stopping recording...")
	if err := room.StopRecording(); err != nil {
		log.Fatalf("Failed to stop recording: %v", err)
	}

	// Wait a bit to give the S3 upload time to start and make some progress
	log.Println("Waiting for S3 upload to complete (15 seconds)...")
	time.Sleep(15 * time.Second)

	// Wait for signals to exit gracefully
	log.Println("Running... press Ctrl+C to exit")
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)

	// Set up signal handling to gracefully shut down
	go func() {
		sig := <-c
		log.Printf("Received signal %v, shutting down...", sig)

		// Give any in-progress uploads time to complete
		log.Println("Waiting 10 seconds for uploads to complete before closing room...")
		time.Sleep(10 * time.Second)

		// Close the room
		log.Println("Closing room...")
		if err := room.Close(); err != nil {
			log.Printf("Error closing room: %v", err)
		}

		log.Println("Exiting...")
		os.Exit(0)
	}()

	// Wait indefinitely
	select {}
}
