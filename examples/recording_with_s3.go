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

	// Configure a room with recording enabled and S3 upload
	roomOpts := sfu.DefaultRoomOptions()
	roomOpts.Recording = &sfu.RecordingOptions{
		Enabled:           true,
		RecordingsPath:    "recordings",
		FFmpegPath:        "ffmpeg",
		AutoMerge:         true,
		S3Upload:          true,
		S3Endpoint:        os.Getenv("S3_ENDPOINT"),          // e.g., "play.minio.io:9000"
		S3AccessKeyID:     os.Getenv("S3_ACCESS_KEY_ID"),     // Your S3 access key
		S3SecretAccessKey: os.Getenv("S3_SECRET_ACCESS_KEY"), // Your S3 secret key
		S3UseSSL:          true,
		S3BucketName:      os.Getenv("S3_BUCKET_NAME"), // Your S3 bucket name
		S3BucketPrefix:    "calls/",                    // Optional prefix
		DeleteAfterUpload: true,                        // Delete local files after upload
	}

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

	// Example of starting a new recording session with a custom identifier
	recordingID := fmt.Sprintf("call-%d", time.Now().Unix())
	log.Printf("Starting recording with ID: %s", recordingID)

	s3Config := &sfu.RecordingOptions{
		S3Upload:          true,
		S3Endpoint:        os.Getenv("S3_ENDPOINT"),
		S3AccessKeyID:     os.Getenv("S3_ACCESS_KEY_ID"),
		S3SecretAccessKey: os.Getenv("S3_SECRET_ACCESS_KEY"),
		S3UseSSL:          true,
		S3BucketName:      os.Getenv("S3_BUCKET_NAME"),
		S3BucketPrefix:    "meetings/", // Different prefix for this recording
		DeleteAfterUpload: true,
	}

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

	// Wait for signals to exit gracefully
	log.Println("Running... press Ctrl+C to exit")
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)
	<-c

	// Close the room
	log.Println("Closing room...")
	if err := room.Close(); err != nil {
		log.Printf("Error closing room: %v", err)
	}

	log.Println("Exiting...")
}
