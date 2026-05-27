package voicepipelinecore

import (
	"log"
	"sync"
	"sync/atomic"
	"time"
)

// UIEventSender publishes RTVI events to the frontend via Daily app
// messages and stores the same entries for the end-of-call debug-log
// upload. The data rides the Daily signalling channel, so there's no
// separate WebSocket route to expose, scale, or terminate TLS for.
//
// The room is plumbed in via SetRoom after the bot has joined, because
// the sender is constructed before the room exists. Events emitted
// before SetRoom are still stored for S3, but cannot be streamed.
type UIEventSender struct {
	logger        *log.Logger
	room          atomic.Pointer[DailyRoom]
	mu            sync.Mutex
	entries       []RTVIDebugLogEntry
	meetingID     string
	botSessionID  string
	userSessionID string
}

func NewUIEventSender(logger *log.Logger) *UIEventSender {
	return &UIEventSender{logger: logger}
}

// SetRoom wires the Daily room into the sender. Called by
// NewTask once the bot has joined.
func (s *UIEventSender) SetRoom(room *DailyRoom) {
	s.room.Store(room)
}

func (s *UIEventSender) SetDailyMeeting(meetingID, botSessionID string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if meetingID != "" {
		s.meetingID = meetingID
	}
	if botSessionID != "" {
		s.botSessionID = botSessionID
	}
}

func (s *UIEventSender) SetUserSessionID(userSessionID string) {
	if s == nil || userSessionID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.userSessionID == "" {
		s.userSessionID = userSessionID
	}
}

func (s *UIEventSender) DailySession() (meetingID, botSessionID, userSessionID string) {
	if s == nil {
		return "", "", ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.meetingID, s.botSessionID, s.userSessionID
}

func (s *UIEventSender) Snapshot() []RTVIDebugLogEntry {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]RTVIDebugLogEntry, len(s.entries))
	copy(out, s.entries)
	return out
}

func (s *UIEventSender) Emit(eventType string, data any, at time.Time) RTVIDebugLogEntry {
	if s == nil || eventType == "" {
		return RTVIDebugLogEntry{}
	}
	if at.IsZero() {
		at = time.Now()
	}
	entry := RTVIDebugLogEntry{
		Label:     rtviDebugLabel,
		Type:      eventType,
		Data:      data,
		Timestamp: float64(at.UnixNano()) / float64(time.Second),
	}
	s.mu.Lock()
	s.entries = append(s.entries, entry)
	s.mu.Unlock()
	s.publish(entry)
	return entry
}

func (s *UIEventSender) BotTranscription(text string, at time.Time) {
	if text == "" {
		return
	}
	s.Emit("bot-transcription", map[string]any{"text": text}, at)
}

func (s *UIEventSender) UserTranscription(text string, final bool, at time.Time) {
	if text == "" && final {
		return
	}
	_, _, userSessionID := s.DailySession()
	s.Emit("user-transcription", map[string]any{
		"text":      text,
		"user_id":   userSessionID,
		"timestamp": rtviTimestamp(at),
		"final":     final,
	}, at)
}

func (s *UIEventSender) BotStartedSpeaking(at time.Time) {
	s.Emit("bot-started-speaking", nil, at)
}

func (s *UIEventSender) BotStoppedSpeaking(at time.Time) {
	s.Emit("bot-stopped-speaking", nil, at)
}

func (s *UIEventSender) ServerMessage(data any, at time.Time) {
	s.Emit("server-message", data, at)
}

func (s *UIEventSender) publish(event RTVIDebugLogEntry) {
	room := s.room.Load()
	if room == nil {
		return
	}
	if err := room.SendAppMessage(event); err != nil {
		if s.logger != nil {
			s.logger.Println("RTVI event publish error:", err)
		}
	}
}
