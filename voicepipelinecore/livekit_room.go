package voicepipelinecore

import (
	"encoding/json"
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

// JoinRoom connects the bot to the LiveKit room and wires the room
// callbacks. taskCtx is needed so the data-channel control messages
// and human-participant disconnect can route through taskCtx.EndTask.
func JoinRoom(roomName string, taskCtx *TaskContext, audioSource *AudioSourceProcessor) (*lksdk.Room, error) {
	logger := func(format string, args ...interface{}) {
		if taskCtx != nil && taskCtx.Logger != nil {
			taskCtx.Logger.Printf(format, args...)
		}
	}
	endTask := func(reason EndReason) {
		if taskCtx != nil && taskCtx.EndTask != nil {
			taskCtx.EndTask(reason)
		}
	}
	markUserJoined := func(p *lksdk.RemoteParticipant) {
		if p == nil || p.Identity() == botIdentity {
			return
		}
		at := time.Now()
		if taskCtx != nil {
			if taskCtx.callStats != nil {
				taskCtx.callStats.MarkUserJoined(at)
			}
			if taskCtx.callEvents != nil {
				taskCtx.callEvents.fireUserJoined(at)
			}
		}
	}
	markUserLeft := func(p *lksdk.RemoteParticipant) {
		if p == nil || p.Identity() == botIdentity {
			return
		}
		if taskCtx != nil && taskCtx.callStats != nil {
			taskCtx.callStats.MarkUserLeft(time.Now())
		}
	}

	room, err := lksdk.ConnectToRoom(os.Getenv("LIVEKIT_URL"), lksdk.ConnectInfo{
		APIKey:              os.Getenv("LIVEKIT_API_KEY"),
		APISecret:           os.Getenv("LIVEKIT_API_SECRET"),
		RoomName:            roomName,
		ParticipantIdentity: botIdentity,
	}, &lksdk.RoomCallback{
		// Browser closing the tab / network dropping → bot has no one
		// to talk to. Route it through EndTask so shutdown still follows
		// the source-driven EndFrame path.
		OnParticipantConnected: func(p *lksdk.RemoteParticipant) {
			logger("[%s] Participant %q connected", roomName, p.Identity())
			markUserJoined(p)
		},
		OnParticipantDisconnected: func(p *lksdk.RemoteParticipant) {
			if p.Identity() == botIdentity {
				return
			}
			logger("[%s] Participant %q disconnected, requesting EndFrame", roomName, p.Identity())
			markUserLeft(p)
			endTask(EndReasonClientDisconnect)
		},
		ParticipantCallback: lksdk.ParticipantCallback{
			OnTrackSubscribed: func(track *webrtc.TrackRemote, pub *lksdk.RemoteTrackPublication, participant *lksdk.RemoteParticipant) {
				logger("[%s] Track subscribed: %s from %s (kind: %s)", roomName, track.ID(), participant.Identity(), track.Kind())
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
					logger("[%s] control data malformed: %v", roomName, err)
					return
				}
				if msg.Type == "end_call" {
					logger("[%s] UI requested end_call via data channel", roomName)
					endTask(EndReasonClientDisconnect)
				}
			},
		},
	})
	if err != nil {
		return nil, err
	}
	logger("[%s] Bot joined the room", roomName)
	if taskCtx != nil && taskCtx.callEvents != nil {
		taskCtx.callEvents.fireBotJoined(time.Now())
	}
	for _, p := range room.GetRemoteParticipants() {
		markUserJoined(p)
	}
	return room, nil
}

func GenerateToken(roomName, identity string) (string, error) {
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
