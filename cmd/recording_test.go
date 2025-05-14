package main

import (
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
)

func main() {
	// Start a test HTTP server to test the recording functionality
	fmt.Println("Starting recording test...")

	// Set recording path
	recordingPath := "/opt/wave/recordings"
	if _, err := os.Stat(recordingPath); os.IsNotExist(err) {
		fmt.Printf("Creating recording directory: %s\n", recordingPath)
		if err := os.MkdirAll(recordingPath, 0755); err != nil {
			fmt.Printf("Failed to create recording directory: %v\n", err)
			return
		}
	}

	// Create a room ID for testing
	roomID := "test-recording-room-" + fmt.Sprintf("%d", time.Now().Unix())
	recordingDir := filepath.Join(recordingPath, roomID)

	// Create the recording directory
	if err := os.MkdirAll(recordingDir, 0755); err != nil {
		fmt.Printf("Failed to create test recording directory: %v\n", err)
		return
	}

	// Create some test files to simulate recording
	testFiles := []struct {
		name     string
		content  string
		channels int // 1=left, 2=right
	}{
		{
			name:     "client1_track1.ogg",
			content:  "test audio data for client 1",
			channels: 1, // left
		},
		{
			name:     "client2_track1.ogg",
			content:  "test audio data for client 2",
			channels: 2, // right
		},
	}

	// Create the test files
	fmt.Println("Creating test recording files...")
	for _, file := range testFiles {
		filePath := filepath.Join(recordingDir, file.name)
		err := os.WriteFile(filePath, []byte(file.content), 0644)
		if err != nil {
			fmt.Printf("Failed to write test file %s: %v\n", file.name, err)
			continue
		}
		fmt.Printf("Created test file: %s (channel type: %d)\n", filePath, file.channels)

		// Create metadata file
		metadataContent := fmt.Sprintf(`{
			"clientID": "%s",
			"trackID": "%s",
			"channelType": %d,
			"startTime": "%s",
			"sampleRate": 48000
		}`,
			file.name[:len(file.name)-9],                 // client1, client2
			file.name[len(file.name)-9:len(file.name)-4], // track1
			file.channels,
			time.Now().Format(time.RFC3339))

		metadataPath := filePath + ".json"
		err = os.WriteFile(metadataPath, []byte(metadataContent), 0644)
		if err != nil {
			fmt.Printf("Failed to write metadata file %s: %v\n", metadataPath, err)
		}
		fmt.Printf("Created metadata file: %s\n", metadataPath)
	}

	fmt.Println("Test files created successfully!")
	fmt.Println("Recording test setup complete at:", recordingDir)
	fmt.Println("Now you should run:")
	fmt.Printf("1. Start recording: curl http://localhost:8080/room/%s/recording/start\n", roomID)
	fmt.Printf("2. Wait for a few seconds\n")
	fmt.Printf("3. Stop recording: curl http://localhost:8080/room/%s/recording/stop\n", roomID)
	fmt.Printf("4. Check for files at: %s\n", recordingDir)

	// Wait for termination signal
	fmt.Println("Press Ctrl+C to clean up and exit.")
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)
	<-c

	// Clean up test files
	fmt.Println("Cleaning up test files...")
	if err := os.RemoveAll(recordingDir); err != nil {
		fmt.Printf("Failed to clean up test directory: %v\n", err)
	} else {
		fmt.Println("Test directory cleaned up successfully")
	}
}
