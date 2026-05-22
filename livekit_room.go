package main

import (
	"encoding/json"
	"log"
	"os"
	"time"

	"github.com/livekit/protocol/auth"
	lksdk "github.com/livekit/server-sdk-go/v2"
	"github.com/pion/webrtc/v4"
)

// controlTopic is the DataPacket topic browsers use to send control
// messages to the bot (e.g. {"type":"end_call"}). Distinct from the
// UIEventType topics the bot publishes outbound.
const controlTopic = "control"

// botIdentity is the participant identity the bot joins with. Used to
// distinguish bot vs user when handling participant lifecycle events.
const botIdentity = "bot"

// joinRoom connects the bot to the LiveKit room and wires the room
// callbacks. taskCtx is needed so the data-channel control messages
// and human-participant disconnect can route through taskCtx.EndTask.
func joinRoom(roomName string, taskCtx *TaskContext, audioSource *AudioSourceProcessor) *lksdk.Room {
	endTask := func(reason string) {
		if taskCtx != nil && taskCtx.EndTask != nil {
			taskCtx.EndTask(reason)
		}
	}

	room, err := lksdk.ConnectToRoom(os.Getenv("LIVEKIT_URL"), lksdk.ConnectInfo{
		APIKey:              os.Getenv("LIVEKIT_API_KEY"),
		APISecret:           os.Getenv("LIVEKIT_API_SECRET"),
		RoomName:            roomName,
		ParticipantIdentity: botIdentity,
	}, &lksdk.RoomCallback{
		// Browser closing the tab / network dropping → bot has no one
		// to talk to. Mirrors the pre-DataPacket behavior where a UI
		// WebSocket close triggered EndTask.
		OnParticipantDisconnected: func(p *lksdk.RemoteParticipant) {
			if p.Identity() == botIdentity {
				return
			}
			log.Printf("[%s] Participant %q disconnected, requesting EndFrame", roomName, p.Identity())
			endTask("ui participant disconnected")
		},
		ParticipantCallback: lksdk.ParticipantCallback{
			OnTrackSubscribed: func(track *webrtc.TrackRemote, pub *lksdk.RemoteTrackPublication, participant *lksdk.RemoteParticipant) {
				log.Printf("[%s] Track subscribed: %s from %s (kind: %s)", roomName, track.ID(), participant.Identity(), track.Kind())
				if track.Kind() == webrtc.RTPCodecTypeAudio {
					audioSource.SetTrack(track)
				}
			},
			OnDataPacket: func(data lksdk.DataPacket, params lksdk.DataReceiveParams) {
				ud, ok := data.(*lksdk.UserDataPacket)
				if !ok {
					return
				}
				if ud.Topic != controlTopic {
					return
				}
				var msg struct {
					Type string `json:"type"`
				}
				if err := json.Unmarshal(ud.Payload, &msg); err != nil {
					log.Printf("[%s] control data malformed: %v", roomName, err)
					return
				}
				if msg.Type == "end_call" {
					log.Printf("[%s] UI requested end_call via data channel", roomName)
					endTask("ui requested end call")
				}
			},
		},
	})
	if err != nil {
		log.Fatalf("[%s] failed to join room: %v", roomName, err)
	}
	log.Printf("[%s] Bot joined the room", roomName)
	return room
}

func generateToken(roomName, identity string) (string, error) {
	at := auth.NewAccessToken(os.Getenv("LIVEKIT_API_KEY"), os.Getenv("LIVEKIT_API_SECRET"))
	grant := &auth.VideoGrant{
		RoomJoin: true,
		Room:     roomName,
	}
	at.SetVideoGrant(grant).
		SetIdentity(identity).
		SetValidFor(10 * time.Minute)
	return at.ToJWT()
}
