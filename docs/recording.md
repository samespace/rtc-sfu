# SFU Recording Functionality

This document describes how to use the recording functionality in the SFU.

## Overview

The SFU now supports recording of audio tracks for each participant in a room. The recordings are stored in OGG format and can be merged into a single WAV file after the room is closed.

Features:
- Record audio for each participant separately
- Pause and resume recording
- Store metadata for each recording
- Merge all participant recordings into a single file
- Configure recording path and other settings

## Requirements

- FFmpeg must be installed on the system (or path to FFmpeg executable must be provided in config)
- Opus codec must be used for audio (which is the default)

## Configuration

Recording can be enabled either at room creation time or dynamically during the room's lifetime.

### Enable at Room Creation

To enable recording when creating a room, include the recording options in the room options:

```go
roomOpts := sfu.DefaultRoomOptions()
roomOpts.Recording = &sfu.RecordingOptions{
    Enabled:        true,                // Enable recording
    RecordingsPath: "/path/to/recordings", // Path to store recordings
    FFmpegPath:     "ffmpeg",            // Path to FFmpeg executable (optional)
    AutoMerge:      true,                // Auto merge recordings when room is closed
}

room := sfuInstance.NewRoom("roomID", "Room Name", roomOpts)
```

### Dynamic Recording Control

You can also control recording during the room's lifetime:

```go
// Start recording
err := room.StartRecording()
if err != nil {
    log.Printf("Failed to start recording: %v", err)
}

// Pause recording
err = room.PauseRecording()
if err != nil {
    log.Printf("Failed to pause recording: %v", err)
}

// Resume recording
err = room.StartRecording() // also used for resuming
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
        log.Printf("Recording started for room %s", event.Data["room_id"])
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
├── roomId/
│   ├── participantId1/
│   │   ├── track.ogg     (the recorded audio)
│   │   └── meta.json     (metadata about the recording)
│   ├── participantId2/
│   │   ├── track.ogg
│   │   └── meta.json
│   ├── merged.wav        (the merged audio file)
│   └── merged_metadata.json
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
  "merged_file": "/path/to/recordings/room123/merged.wav"
}
```

## Merging Recordings

When a room is closed, the SFU will automatically merge all participant recordings into a single WAV file if `AutoMerge` is set to `true`. The merged file will be stored in the room's directory as `merged.wav`.

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
- Each participant's recording is handled independently, so if a participant leaves and rejoins, they will have multiple recordings. 