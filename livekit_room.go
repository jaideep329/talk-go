package main

import (
	"log"
	"os"

	lksdk "github.com/livekit/server-sdk-go/v2"
	"github.com/pion/webrtc/v4"
)

var room *lksdk.Room

func joinRoom(audioSource *AudioSourceProcessor) {
	var err error
	room, err = lksdk.ConnectToRoom(os.Getenv("LIVEKIT_URL"), lksdk.ConnectInfo{
		APIKey:              os.Getenv("LIVEKIT_API_KEY"),
		APISecret:           os.Getenv("LIVEKIT_API_SECRET"),
		RoomName:            "default-room",
		ParticipantIdentity: "bot",
	}, &lksdk.RoomCallback{
		ParticipantCallback: lksdk.ParticipantCallback{
			OnTrackSubscribed: func(track *webrtc.TrackRemote, pub *lksdk.RemoteTrackPublication, participant *lksdk.RemoteParticipant) {
				log.Printf("Track subscribed: %s from %s (kind: %s)", track.ID(), participant.Identity(), track.Kind())
				if track.Kind() == webrtc.RTPCodecTypeAudio {
					audioSource.SetTrack(track)
				}
			},
		},
	})
	if err != nil {
		log.Fatal("failed to join room:", err)
	}
	log.Println("Bot joined the room")
}
