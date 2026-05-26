package voicepipelinecore

import (
	"encoding/json"
	"log"
	"sync/atomic"
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

// UIEvent is a typed message sent to the frontend over Daily app messages.
// Type is the event discriminator; Data is the JSON payload.
type UIEvent struct {
	Type UIEventType            `json:"type"`
	Data map[string]interface{} `json:"data"`
}

// UIEventSender publishes UIEvents to the frontend via Daily app messages.
// The data rides the Daily signalling channel, so there's no separate
// WebSocket route to expose, scale, or terminate TLS for.
//
// The room is plumbed in via SetRoom after the bot has joined, because
// the sender is constructed before the room exists. Send is a no-op
// until SetRoom is called.
type UIEventSender struct {
	logger *log.Logger
	room   atomic.Pointer[DailyRoom]
}

func NewUIEventSender(logger *log.Logger) *UIEventSender {
	return &UIEventSender{logger: logger}
}

// SetRoom wires the Daily room into the sender. Called by
// NewTask once the bot has joined; before that, Send is a no-op
// (any events emitted during the brief startup window are dropped).
func (s *UIEventSender) SetRoom(room *DailyRoom) {
	s.room.Store(room)
}

// Send publishes a UIEvent to all participants in the Daily room.
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
	var msg UIEvent
	if err := json.Unmarshal(payload, &msg); err != nil {
		s.logger.Println("UIEvent unmarshal error:", err)
		return
	}
	if err := room.SendAppMessage(msg); err != nil {
		s.logger.Println("UIEvent publish error:", err)
	}
}
