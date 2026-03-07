package main

import (
	"encoding/binary"
	"log"
	"os"
	"sync"

	"github.com/gorilla/websocket"
	"github.com/hraban/opus"
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

	audioMu.Lock()
	// Close old connections if there were any (stops old goroutines)
	if sttConn != nil {
		sttConn.Close()
	}
	if ttsConn != nil {
		ttsConn.Close()
	}
	initializeSonioxWebsocket()
	initializeTTSWebsocket()
	go readSTTWebsocketLoop()
	audioMu.Unlock()

	go readAudioTrack(track)
}

func readAudioTrack(track *webrtc.TrackRemote) {
	// Opus decoder outputting 16kHz mono — matches what Soniox expects
	decoder, err := opus.NewDecoder(16000, 1)
	if err != nil {
		log.Fatal("failed to create opus decoder:", err)
	}

	// Buffer for decoded PCM samples (960 samples per Opus frame at 48kHz = 320 at 16kHz)
	pcmBuf := make([]int16, 960)

	for {
		// Read an RTP packet from the track
		rtpPacket, _, err := track.ReadRTP()
		if err != nil {
			log.Println("track read error:", err)
			return
		}

		// Decode Opus payload → PCM samples
		n, err := decoder.Decode(rtpPacket.Payload, pcmBuf)
		if err != nil {
			log.Println("opus decode error:", err)
			continue
		}

		// Convert int16 samples to bytes (s16le) for Soniox
		pcmBytes := make([]byte, n*2)
		for i := 0; i < n; i++ {
			binary.LittleEndian.PutUint16(pcmBytes[i*2:], uint16(pcmBuf[i]))
		}

		if err := sttConn.WriteMessage(websocket.BinaryMessage, pcmBytes); err != nil {
			log.Println("stt write error:", err)
			return
		}
	}
}
