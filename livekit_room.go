package main

import (
	"log"
	"os"
	"sync"

	lksdk "github.com/livekit/server-sdk-go/v2"
	"github.com/pion/webrtc/v4"
)

var room *lksdk.Room
var botTrack *lksdk.LocalSampleTrack
var audioMu sync.Mutex

func joinRoom() {
	var err error
	room, err = lksdk.ConnectToRoom(os.Getenv("LIVEKIT_URL"), lksdk.ConnectInfo{
		APIKey:              os.Getenv("LIVEKIT_API_KEY"),
		APISecret:           os.Getenv("LIVEKIT_API_SECRET"),
		RoomName:            "default-room",
		ParticipantIdentity: "bot",
	}, &lksdk.RoomCallback{
		ParticipantCallback: lksdk.ParticipantCallback{
			OnTrackSubscribed: onTrackSubscribed,
		},
	})
	if err != nil {
		log.Fatal("failed to join room:", err)
	}
	log.Println("Bot joined the room")
	botTrack, err = lksdk.NewLocalSampleTrack(webrtc.RTPCodecCapability{
		MimeType:  webrtc.MimeTypeOpus,
		ClockRate: 48000,
		Channels:  2,
	})
	if err != nil {
		log.Fatal("failed to create local track:", err)
	}

	_, err = room.LocalParticipant.PublishTrack(botTrack, &lksdk.TrackPublicationOptions{
		Name: "bot-audio",
	})
	if err != nil {
		log.Fatal("failed to publish track:", err)
	}
	log.Println("Published bot audio track")
}
func onTrackSubscribed(track *webrtc.TrackRemote, pub *lksdk.RemoteTrackPublication, participant *lksdk.RemoteParticipant) {
	log.Printf("Track subscribed: %s from %s (kind: %s)", track.ID(), participant.Identity(), track.Kind())

	if track.Kind() != webrtc.RTPCodecTypeAudio {
		return
	}

	// TODO: Wire up pipeline here — create AudioSourceProcessor with track and start pipeline
}
