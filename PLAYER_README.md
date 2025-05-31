# Player Implementation for WebRTC SFU

This implementation adds audio playback functionality to the WebRTC SFU, allowing you to play audio files from URLs to specific clients. This is perfect for contact center solutions where you need features like hold music, announcements, or background audio.

## Features

- **Play audio from URLs to specific clients**
- **Loop support** for continuous playback
- **HTTP method support** (GET, POST, etc.) with custom body
- **Start/Stop control** for individual clients
- **Track active players** for each client
- **Integrates seamlessly** with existing SFU track system
- **Supports Opus audio codec** in OGG container format
- **Single reusable track** per client to avoid renegotiation

## Core Components

### 1. PlayerTrack (`player.go`)
A new track type that implements the `ITrack` interface and handles audio playback from URLs.

### 2. Client Methods (`client.go`)
- `Play()` - Play audio from URL to this client
- `StopPlay()` - Stop playback for this client
- `IsPlaying()` - Check if client has active playback
- `GetPlayers()` - Get active players for this client
- `GetPlayerTrack()` - Get the player track (may be nil)

## Usage Examples

### Prerequisites

You need to implement the `AudioUtilsInterface` for your audio processing needs:

```go
type AudioUtilsInterface interface {
    GetFile(endpoint, method, body string) (io.ReadCloser, error)
    NewOggReader(file io.ReadCloser, loop bool) (OggReaderInterface, *sfu.OggHeader, error)
    ParsePacketDuration(packet []byte) (time.Duration, error)
}
```

### Basic Usage

```go
package main

import (
    "context"
    "github.com/inlivedev/sfu"
    "github.com/pion/webrtc/v4"
)

func main() {
    ctx := context.Background()
    
    // Create SFU
    sfuOpts := sfu.SFUOptions{
        IceServers: []webrtc.ICEServer{
            {URLs: []string{"stun:stun.l.google.com:19302"}},
        },
    }
    sfuInstance := sfu.New(ctx, sfuOpts)
    
    // Create clients
    agent := sfuInstance.NewClient("agent1", "Agent 1", sfu.DefaultClientOptions())
    customer := sfuInstance.NewClient("customer1", "Customer 1", sfu.DefaultClientOptions())
    
    // Implement your audio utils
    audioUtils := &YourAudioUtilsImplementation{}
    
    // Play hold music to customer
    opts := sfu.PlayerOptions{
        URL:        "https://example.com/hold-music.ogg",
        Method:     "GET",
        Loop:       true,
        SampleRate: 48000,
        Channels:   1,
    }
    
    playerTrack, err := customer.Play(opts, audioUtils)
    if err != nil {
        log.Fatal(err)
    }
    
    // Later, stop the music
    err = customer.StopPlay()
    if err != nil {
        log.Fatal(err)
    }
}
```

### Contact Center Scenarios

#### 1. Hold Music
```go
// Put customer on hold with music
holdMusicOpts := sfu.PlayerOptions{
    URL:        "https://yourcdn.com/hold-music.ogg",
    Method:     "GET",
    Loop:       true, // Loop continuously
    SampleRate: 48000,
    Channels:   1,
}

// Play to customer
playerTrack, err := customer.Play(holdMusicOpts, audioUtils)
if err != nil {
    log.Printf("Failed to start hold music: %v", err)
}

// When agent is ready, stop hold music
err = customer.StopPlay()
if err != nil {
    log.Printf("Failed to stop hold music: %v", err)
}
```

#### 2. System Announcements to All Clients
```go
// Get all clients from SFU
clients := sfu.GetClients()

// Play announcement to all clients
announcementOpts := sfu.PlayerOptions{
    URL:        "https://yourcdn.com/queue-announcement.ogg",
    Method:     "GET",
    Loop:       false, // Play once
    SampleRate: 48000,
    Channels:   1,
}

for _, client := range clients {
    playerTrack, err := client.Play(announcementOpts, audioUtils)
    if err != nil {
        log.Printf("Failed to play announcement to client %s: %v", client.ID(), err)
    }
}
```

#### 3. Background Music for Agents
```go
// Play background music to specific agent
backgroundOpts := sfu.PlayerOptions{
    URL:        "https://yourcdn.com/ambient-music.ogg",
    Method:     "GET",
    Loop:       true,
    SampleRate: 48000,
    Channels:   1,
}

musicTrack, err := agent.Play(backgroundOpts, audioUtils)
if err != nil {
    log.Printf("Failed to start background music: %v", err)
}

// Check if playing
if agent.IsPlaying() {
    log.Println("Agent has active audio playback")
}
```

#### 4. Dynamic Audio Content
```go
// Play audio with custom HTTP headers/body
customOpts := sfu.PlayerOptions{
    URL:        "https://api.yourservice.com/generate-audio",
    Method:     "POST",
    Body:       `{"customer_id": "12345", "language": "en"}`,
    Loop:       false,
    SampleRate: 48000,
    Channels:   1,
}

playerTrack, err := customer.Play(customOpts, audioUtils)
```

### Monitoring and Control

```go
// Get players for specific client
players := client.GetPlayers()
log.Printf("Client has %d active players", len(players))
for _, player := range players {
    log.Printf("  Track: %s, Playing: %t", player.ID(), player.IsPlaying())
}

// Stop playback for a client
err := client.StopPlay()
if err != nil {
    log.Printf("Error stopping playback: %v", err)
}

// Check all clients for active players
for _, client := range sfu.GetClients() {
    if client.IsPlaying() {
        log.Printf("Client %s has active playback", client.ID())
        
        // Stop playback for this client
        client.StopPlay()
    }
}
```

### Integration with Existing Hold/Unhold System

```go
// Put client on hold AND play hold music
err = sfu.HoldAllTracksFromClient(agentID, customerID)
if err != nil {
    log.Printf("Failed to hold tracks: %v", err)
}

// Start hold music
holdOpts := sfu.PlayerOptions{
    URL:  "https://yourcdn.com/hold-music.ogg",
    Loop: true,
}
_, err = customer.Play(holdOpts, audioUtils)
if err != nil {
    log.Printf("Failed to start hold music: %v", err)
}

// Later, remove from hold AND stop music
err = sfu.UnholdAllTracksFromClient(agentID, customerID)
if err != nil {
    log.Printf("Failed to unhold tracks: %v", err)
}

err = customer.StopPlay()
if err != nil {
    log.Printf("Failed to stop hold music: %v", err)
}
```

### Switching Between Different Audio Sources

```go
// First audio
opts1 := sfu.PlayerOptions{
    URL: "https://example.com/welcome.ogg",
    Loop: false,
}
client.Play(opts1, audioUtils)

// Switch to different audio (no renegotiation!)
opts2 := sfu.PlayerOptions{
    URL: "https://example.com/hold-music.ogg", 
    Loop: true,
}
client.Play(opts2, audioUtils)

// Switch again (still no renegotiation!)
opts3 := sfu.PlayerOptions{
    URL: "https://example.com/goodbye.ogg",
    Loop: false,
}
client.Play(opts3, audioUtils)
```

## Implementation Notes

### Single Track Per Client
- Each client has one reusable player track
- First `Play()` call creates the track (triggers renegotiation)
- Subsequent `Play()` calls reuse the same track (no renegotiation)
- Perfect for switching between hold music, announcements, etc.

### Audio Format Support
- **Primary**: Opus audio in OGG container
- **Sample Rate**: 48kHz recommended for WebRTC compatibility
- **Channels**: Mono (1 channel) recommended for efficiency

### Performance Considerations
- Each player runs in its own goroutine
- Audio packets are sent every 20ms (configurable)
- Memory usage scales with number of active players
- Network bandwidth depends on audio quality and client count
- **No renegotiation overhead** when switching audio sources

### Error Handling
- Failed URL requests are logged and stop playback
- Network timeouts are handled gracefully
- Invalid audio formats cause playback to fail safely
- Client disconnections automatically clean up player resources

### Thread Safety
- All player operations are thread-safe
- Concurrent access to client tracks is protected
- Player state changes are atomic

## Audio Utils Implementation Guide

You need to implement the `AudioUtilsInterface` for your specific audio processing needs. Here's a basic structure:

```go
type YourAudioUtils struct {
    httpClient *http.Client
}

func (a *YourAudioUtils) GetFile(endpoint, method, body string) (io.ReadCloser, error) {
    // Implement HTTP request logic
    // Handle authentication, headers, etc.
    // Return ReadCloser for audio file
}

func (a *YourAudioUtils) NewOggReader(file io.ReadCloser, loop bool) (sfu.OggReaderInterface, *sfu.OggHeader, error) {
    // Parse OGG file header
    // Create reader that can iterate through packets
    // Handle looping logic if needed
}

func (a *YourAudioUtils) ParsePacketDuration(packet []byte) (time.Duration, error) {
    // Parse Opus packet to determine duration
    // Typically 20ms for Opus
    // Used for timing calculations
}
```

## Error Codes

- `ErrPlayerAlreadyPlaying`: Attempted to start player that's already playing
- `ErrPlayerStopped`: Player was stopped or context cancelled
- `ErrTrackIsNotExists`: Referenced track doesn't exist
- `ErrClientNotFound`: Referenced client doesn't exist

## Best Practices

1. **Always implement proper error handling** for URL requests
2. **Use appropriate sample rates** (48kHz for WebRTC)
3. **Monitor active players** to prevent resource leaks
4. **Implement timeouts** for URL requests
5. **Use looping sparingly** to avoid excessive bandwidth
6. **Test with various audio formats** and durations
7. **Handle client disconnections** gracefully
8. **Implement audio caching** for frequently played content
9. **Take advantage of no-renegotiation switching** for better UX

## License

This implementation follows the same license as the parent SFU project. 