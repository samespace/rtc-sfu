package main

import (
	"context"
	"fmt"
	"log"

	"github.com/inlivedev/sfu"
	"github.com/pion/webrtc/v4"
)

func main() {
	// Example usage of the hold/unhold functionality
	ctx := context.Background()

	// Create SFU manager with default options
	opts := sfu.DefaultOptions()
	opts.IceServers = []webrtc.ICEServer{
		{URLs: []string{"stun:stun.l.google.com:19302"}},
	}

	manager := sfu.NewManager(ctx, "hold-unhold-example", opts)
	defer manager.Close()

	// Create a room
	roomOpts := sfu.DefaultRoomOptions()
	roomOpts.Codecs = &[]string{"video/VP8", "audio/opus"}

	room, err := manager.NewRoom("room1", "Hold/Unhold Demo Room", sfu.RoomTypeLocal, roomOpts)
	if err != nil {
		log.Fatalf("Failed to create room: %v", err)
	}

	// Create three clients: A, B, and C
	clientA, err := room.AddClient("clientA", "User A", sfu.DefaultClientOptions())
	if err != nil {
		log.Fatalf("Failed to create client A: %v", err)
	}

	clientB, err := room.AddClient("clientB", "User B", sfu.DefaultClientOptions())
	if err != nil {
		log.Fatalf("Failed to create client B: %v", err)
	}

	clientC, err := room.AddClient("clientC", "User C", sfu.DefaultClientOptions())
	if err != nil {
		log.Fatalf("Failed to create client C: %v", err)
	}

	fmt.Println("Created clients A, B, and C")

	// Simulate scenario: User A wants to put User B on hold
	// This means User B will not hear User A and User C

	// Method 1: Put specific track on hold
	// Example: Put User A's audio track on hold for User B
	trackID := "audio-track-from-A"
	err = room.SFU().HoldClientTrack("clientA", "clientB", trackID)
	if err != nil {
		log.Printf("Error holding track: %v", err)
	} else {
		fmt.Printf("Put track %s from User A on hold for User B\n", trackID)
	}

	// Method 2: Put all tracks from User A on hold for User B
	err = room.SFU().HoldAllTracksFromClient("clientA", "clientB")
	if err != nil {
		log.Printf("Error holding all tracks: %v", err)
	} else {
		fmt.Println("Put all tracks from User A on hold for User B")
	}

	// Method 3: Put all tracks from User C on hold for User B
	err = room.SFU().HoldAllTracksFromClient("clientC", "clientB")
	if err != nil {
		log.Printf("Error holding all tracks: %v", err)
	} else {
		fmt.Println("Put all tracks from User C on hold for User B")
	}

	// Check which tracks are on hold for User B
	heldTracks, err := room.SFU().GetHeldTracksForClient("clientB")
	if err != nil {
		log.Printf("Error getting held tracks: %v", err)
	} else {
		fmt.Printf("User B has %d tracks on hold: %v\n", len(heldTracks), heldTracks)
	}

	// Method 4: Using client-level methods directly
	// Put a specific track on hold using the client directly
	err = clientB.Hold(trackID)
	if err != nil {
		log.Printf("Error holding track at client level: %v", err)
	} else {
		fmt.Printf("User B put track %s on hold using client method\n", trackID)
	}

	// Check if a specific track is on hold
	isOnHold := clientB.IsTrackOnHold(trackID)
	fmt.Printf("Track %s is on hold for User B: %t\n", trackID, isOnHold)

	// Remove track from hold
	err = clientB.Unhold(trackID)
	if err != nil {
		log.Printf("Error unholding track: %v", err)
	} else {
		fmt.Printf("Removed track %s from hold for User B\n", trackID)
	}

	// Remove all tracks from hold for User B
	err = clientB.UnholdAllTracks()
	if err != nil {
		log.Printf("Error unholding all tracks: %v", err)
	} else {
		fmt.Println("Removed all tracks from hold for User B")
	}

	// Method 5: Room-wide hold functionality
	// Put User B on hold for everyone in the room
	err = room.SFU().HoldClientForAll("clientB")
	if err != nil {
		log.Printf("Error holding client for all: %v", err)
	} else {
		fmt.Println("Put User B on hold for all other users")
	}

	// Remove User B from hold for everyone
	err = room.SFU().UnholdClientForAll("clientB")
	if err != nil {
		log.Printf("Error unholding client for all: %v", err)
	} else {
		fmt.Println("Removed User B from hold for all other users")
	}

	fmt.Println("Hold/Unhold example completed")

	// Use the variables to avoid "declared and not used" errors
	_ = clientA
	_ = clientC

	// Run the contact center example
	contactCenterExample()
}

// Example in a contact center scenario:
func contactCenterExample() {
	ctx := context.Background()

	// Create SFU manager with default options
	opts := sfu.DefaultOptions()
	opts.IceServers = []webrtc.ICEServer{
		{URLs: []string{"stun:stun.l.google.com:19302"}},
	}

	manager := sfu.NewManager(ctx, "contact-center-example", opts)
	defer manager.Close()

	// Create a room
	roomOpts := sfu.DefaultRoomOptions()
	roomOpts.Codecs = &[]string{"audio/opus"}

	room, err := manager.NewRoom("room2", "Contact Center Room", sfu.RoomTypeLocal, roomOpts)
	if err != nil {
		log.Fatalf("Failed to create room: %v", err)
	}

	// Create participants
	agent, err := room.AddClient("agent", "Agent Smith", sfu.DefaultClientOptions())
	if err != nil {
		log.Fatalf("Failed to create agent: %v", err)
	}

	customer, err := room.AddClient("customer", "Customer John", sfu.DefaultClientOptions())
	if err != nil {
		log.Fatalf("Failed to create customer: %v", err)
	}

	supervisor, err := room.AddClient("supervisor", "Supervisor Jane", sfu.DefaultClientOptions())
	if err != nil {
		log.Fatalf("Failed to create supervisor: %v", err)
	}

	fmt.Println("=== Contact Center Example ===")
	fmt.Println("Participants: Agent, Customer, Supervisor")

	// Scenario 1: Agent puts customer on hold
	fmt.Println("\n1. Agent puts customer on hold")
	err = room.SFU().HoldAllTracksFromClient("customer", "agent")
	if err != nil {
		log.Printf("Error: %v", err)
	} else {
		fmt.Println("   ✓ Customer is now on hold for the agent")
		fmt.Println("   ✓ Agent can still hear supervisor")
		fmt.Println("   ✓ Customer cannot hear agent or supervisor")
	}

	// Scenario 2: Agent removes customer from hold
	fmt.Println("\n2. Agent removes customer from hold")
	err = room.SFU().UnholdAllTracksFromClient("customer", "agent")
	if err != nil {
		log.Printf("Error: %v", err)
	} else {
		fmt.Println("   ✓ Customer is no longer on hold")
		fmt.Println("   ✓ Normal conversation can resume")
	}

	// Scenario 3: Private conversation between agent and supervisor
	fmt.Println("\n3. Agent has private conversation with supervisor")
	err = room.SFU().HoldAllTracksFromClient("agent", "customer")
	if err != nil {
		log.Printf("Error: %v", err)
	}
	err = room.SFU().HoldAllTracksFromClient("supervisor", "customer")
	if err != nil {
		log.Printf("Error: %v", err)
	} else {
		fmt.Println("   ✓ Customer cannot hear agent or supervisor")
		fmt.Println("   ✓ Agent and supervisor can communicate privately")
	}

	// End private conversation
	fmt.Println("\n4. End private conversation")
	err = room.SFU().UnholdAllTracksFromClient("agent", "customer")
	if err != nil {
		log.Printf("Error: %v", err)
	}
	err = room.SFU().UnholdAllTracksFromClient("supervisor", "customer")
	if err != nil {
		log.Printf("Error: %v", err)
	} else {
		fmt.Println("   ✓ All participants can hear each other again")
	}

	fmt.Println("\n=== Contact Center Example Completed ===")

	// Use the variables to avoid "declared and not used" errors
	_ = agent
	_ = customer
	_ = supervisor
}
