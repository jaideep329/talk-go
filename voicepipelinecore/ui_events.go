package voicepipelinecore

import (
	"encoding/json"
	"log"
	"sync/atomic"

	lksdk "github.com/livekit/server-sdk-go/v2"
)

type UIEventType string

const (
	LiveTranscript     UIEventType = "live_transcript"
	UserTranscript     UIEventType = "user_transcript"
	CommittedAssistant UIEventType = "committed_assistant"
	AssistantSpeaking  UIEventType = "assistant_speaking"
	Metrics            UIEventType = "metrics"
	CallEnded          UIEventType = "call_ended"
)

// UIEvent is a typed message sent to the frontend over the LiveKit data
// channel. Type is an enum carried in the DataPacket topic (so clients
// can filter on it); Data is the JSON payload.
type UIEvent struct {
	Type UIEventType            `json:"type"`
	Data map[string]interface{} `json:"data"`
}

// UIEventSender publishes UIEvents to the frontend via LiveKit's
// WebRTC data channel (PublishData). The data rides the same peer
// connection as audio, so there's no separate WebSocket route to
// expose, scale, or terminate TLS for.
//
// The room is plumbed in via SetRoom after the bot has joined, because
// the sender is constructed before the room exists. Send is a no-op
// until SetRoom is called.
//
// All events are sent RELIABLE. Tempting to use LOSSY for the
// high-frequency live_transcript / assistant_speaking streams, but:
//
//   - The server SDK silently discards dc.Send errors (engine.go), so
//     a send before the lossy channel is fully open is dropped without
//     any signal back to us — and lossy channels can take a moment to
//     finish ICE/DTLS after JoinRoom returns.
//   - Lossy DataChannels are configured Ordered:false, MaxRetransmits:0,
//     so even when open, congestion drops packets with no retry.
//   - UI event traffic is tiny (a few small JSON blobs per second peak).
//     RELIABLE ordering + retransmit cost is negligible.
type UIEventSender struct {
	logger *log.Logger
	room   atomic.Pointer[lksdk.Room]
}

func NewUIEventSender(logger *log.Logger) *UIEventSender {
	return &UIEventSender{logger: logger}
}

// SetRoom wires the LiveKit room into the sender. Called by
// NewTask once the bot has joined; before that, Send is a no-op
// (any events emitted during the brief startup window are dropped).
func (s *UIEventSender) SetRoom(room *lksdk.Room) {
	s.room.Store(room)
}

// Send publishes a UIEvent to all remote participants in the room.
func (s *UIEventSender) Send(event UIEvent) {
	room := s.room.Load()
	if room == nil {
		return
	}
	payload, err := json.Marshal(event)
	if err != nil {
		s.logger.Println("UIEvent marshal error:", err)
		return
	}
	if err := room.LocalParticipant.PublishData(
		payload,
		lksdk.WithDataPublishReliable(true),
		lksdk.WithDataPublishTopic(string(event.Type)),
	); err != nil {
		s.logger.Println("UIEvent publish error:", err)
	}
}
