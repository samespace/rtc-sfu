# SFU Recording Functionality

This document describes how to use the recording functionality in the SFU.

## Overview

The SFU now supports recording of audio tracks for each participant in a room. The recordings are stored in OGG format and can be merged into a single WAV file after the room is closed. Optionally, the merged recording can be uploaded to an S3-compatible storage.

Features:
- Record audio for each participant separately
- Pause and resume recording
- Store metadata for each recording
- Merge all participant recordings into a single file
- Upload the merged recording to S3-compatible storage
- Configure recording path and other settings
- Support for multiple recording sessions per room with unique identifiers

## Requirements

- FFmpeg must be installed on the system (or path to FFmpeg executable must be provided in config)
- Opus codec must be used for audio (which is the default)
- For S3 upload: Access to an S3-compatible storage service (like AWS S3, MinIO, etc.)

## Configuration

Recording can be enabled either at room creation time or dynamically during the room's lifetime.

### Enable at Room Creation

To enable recording when creating a room, include the recording options in the room options:

```go
roomOpts := sfu.DefaultRoomOptions()
roomOpts.Recording = &sfu.RecordingOptions{
    Enabled:           true,                // Enable recording
    RecordingsPath:    "/path/to/recordings", // Path to store recordings
    FFmpegPath:        "ffmpeg",            // Path to FFmpeg executable (optional)
    AutoMerge:         true,                // Auto merge recordings when room is closed
    // S3 upload configuration (optional)
    S3Upload:          true,                // Enable S3 upload
    S3Endpoint:        "s3.amazonaws.com",  // S3 endpoint
    S3AccessKeyID:     "your-access-key",   // S3 access key
    S3SecretAccessKey: "your-secret-key",   // S3 secret key
    S3UseSSL:          true,                // Use SSL for S3 connections
    S3BucketName:      "your-bucket",       // S3 bucket name
    S3BucketPrefix:    "recordings/",       // Prefix for S3 objects (optional)
    DeleteAfterUpload: true,                // Delete local files after successful upload
}

room := sfuInstance.NewRoom("roomID", "Room Name", roomOpts)
```

### Dynamic Recording Control

You can also control recording during the room's lifetime:

```go
// Start recording with a custom identifier and S3 configuration
identifier := "session-123" // Unique identifier for this recording session
s3Config := &sfu.RecordingOptions{
    S3Upload:          true,
    S3Endpoint:        "s3.amazonaws.com",
    S3AccessKeyID:     "your-access-key",
    S3SecretAccessKey: "your-secret-key",
    S3UseSSL:          true,
    S3BucketName:      "your-bucket",
    S3BucketPrefix:    "recordings/",
    DeleteAfterUpload: true,
}
err := room.StartRecording(identifier, s3Config)
if err != nil {
    log.Printf("Failed to start recording: %v", err)
}

// You can start multiple recording sessions for the same room with different identifiers
err = room.StartRecording("session-124", s3Config)

// Pause recording
err = room.PauseRecording()
if err != nil {
    log.Printf("Failed to pause recording: %v", err)
}

// Resume recording
err = room.StartRecording("", nil) // Use empty identifier to resume the current session
if err != nil {
    log.Printf("Failed to resume recording: %v", err)
}

// Stop recording
err = room.StopRecording()
if err != nil {
    log.Printf("Failed to stop recording: %v", err)
}

// Check if recording is enabled
isEnabled := room.IsRecordingEnabled()

// Get recording state
state := room.GetRecordingState() // "active", "paused", or "stopped"
```

## Recording Events

The SFU will fire events related to recording that you can listen for using the Room's `OnEvent` callback:

```go
room.OnEvent = func(event sfu.Event) {
    switch event.Type {
    case sfu.EventRecordingStart:
        log.Printf("Recording started for room %s with identifier %s", 
            event.Data["room_id"], event.Data["identifier"])
    case sfu.EventRecordingPause:
        log.Printf("Recording paused for room %s", event.Data["room_id"])
    case sfu.EventRecordingResume:
        log.Printf("Recording resumed for room %s", event.Data["room_id"])
    case sfu.EventRecordingStop:
        log.Printf("Recording stopped for room %s", event.Data["room_id"])
    }
}
```

## Storage Structure

The recordings are stored in the following structure:

```
recordings/
├── [identifier]/
│   ├── participantId1/
│   │   ├── track.ogg     (the recorded audio)
│   │   └── meta.json     (metadata about the recording)
│   ├── participantId2/
│   │   ├── track.ogg
│   │   └── meta.json
│   ├── merged.wav        (the merged audio file)
│   └── merged_metadata.json
```

When S3 upload is enabled, the merged audio file will be uploaded to:
```
s3://[bucket]/[prefix]/[identifier].wav
```

## Metadata

Each recording has an associated `meta.json` file that contains information about the recording:

```json
{
  "participant_id": "participant123",
  "track_id": "track456",
  "start_time": "2023-06-01T12:00:00Z",
  "end_time": "2023-06-01T12:30:00Z",
  "paused_periods": [
    {
      "start": "2023-06-01T12:10:00Z",
      "end": "2023-06-01T12:15:00Z"
    }
  ]
}
```

The merged recording also has a metadata file with information about all participants:

```json
{
  "room_id": "room123",
  "recording_id": "session-123",
  "participants": [
    {
      "participant_id": "participant123",
      "track_id": "track456",
      "start_time": "2023-06-01T12:00:00Z",
      "end_time": "2023-06-01T12:30:00Z",
      "paused_periods": [
        {
          "start": "2023-06-01T12:10:00Z",
          "end": "2023-06-01T12:15:00Z"
        }
      ]
    },
    {
      "participant_id": "participant789",
      "track_id": "track101",
      "start_time": "2023-06-01T12:05:00Z",
      "end_time": "2023-06-01T12:30:00Z"
    }
  ],
  "merged_at": "2023-06-01T12:31:00Z",
  "merged_file": "/path/to/recordings/session-123/merged.wav",
  "s3_uploaded": true,
  "s3_object_name": "recordings/session-123.wav"
}
```

## Merging Recordings

When a room is closed, the SFU will automatically merge all participant recordings into a single WAV file if `AutoMerge` is set to `true`. The merged file will be stored in the recording's directory as `merged.wav`.

If S3 upload is enabled, the merged file will be uploaded to the specified S3 bucket. If `DeleteAfterUpload` is set to `true`, the local files will be deleted after a successful upload.

The merging process takes into account:
- Start and end times for each participant
- Paused periods
- Synchronization between participants

## Notes

- Recording is only supported for audio tracks. Video tracks are not recorded.
- The recordings are stored in Opus-encoded OGG format for individual participants.
- The merged recording is stored in WAV format.
- The SFU uses FFmpeg for merging recordings, so make sure FFmpeg is installed on your system.
- For large rooms or long recordings, the merging process might take time and consume resources.
- Each recording session has its own unique identifier, which can be provided when starting a recording.
- Multiple recording sessions can be created for the same room with different identifiers.
- S3 upload is performed asynchronously after the recording is stopped and merged. 