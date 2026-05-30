package voicepipelinecore

import "time"

// Message is the public representation of an LLM context message.
// It maps directly to the role/content objects the LLM processor sends.
type Message struct {
	Role    string
	Content string
}

// EndReason is the typed public reason a task ended. EndFrame still
// carries the string form so existing frame logging remains simple.
type EndReason string

const (
	EndReasonUnspecified       EndReason = "unspecified"
	EndReasonTalkTimeExhausted EndReason = "talk_time_exhausted"
	EndReasonClientDisconnect  EndReason = "client_disconnected"
	EndReasonUserIdle          EndReason = "user_idle"
	EndReasonError             EndReason = "error"
)

func normalizeEndReason(reason EndReason) EndReason {
	if reason == "" {
		return EndReasonUnspecified
	}
	return reason
}

// CallStats is delivered with CallEvents.OnCallEnded.
type CallStats struct {
	TotalUserDurationSec  float64
	FirstUserAudioFrameAt time.Time
	EndedAt               time.Time
	MeetingID             string
	BotSessionID          string
	UserSessionID         string
	DebugLogs             []RTVIDebugLogEntry
}

// CallEvents are integration callbacks for call lifecycle and committed
// conversation turns. The first five are one-shot timeline events;
// committed-turn events can fire many times.
type CallEvents struct {
	OnBotJoined              func(time.Time)
	OnUserJoined             func(time.Time)
	OnUserFirstSpeech        func(time.Time)
	OnBotFirstSpeech         func(time.Time)
	OnFirstUserAudio         func(time.Time)
	OnUserTurnCommitted      func(text string, at time.Time, promptKey string)
	OnAssistantTurnCommitted func(text string, at time.Time, metrics TurnMetrics, promptKey string)
	OnCallEnded              func(reason EndReason, stats CallStats)
}

// TurnMetrics is a per-assistant-turn snapshot assembled from the
// MetricsFrame stream.
type TurnMetrics struct {
	LLMTTFBMs            float64
	LLMProcessingMs      float64
	TTSTextAggregationMs float64
	TTSTTFBMs            float64
	E2ELatencyMs         float64
}
