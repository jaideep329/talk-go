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

// CallStats is delivered with TaskOptions.OnCallEnded.
type CallStats struct {
	TotalUserDurationSec  float64
	FirstUserAudioFrameAt time.Time
	EndedAt               time.Time
}

// CallEvents are one-shot call timeline events exposed to integrations.
// They describe transport/audio/session milestones, not conversation turns.
type CallEvents struct {
	OnBotJoined       func(time.Time)
	OnUserJoined      func(time.Time)
	OnUserFirstSpeech func(time.Time)
	OnBotFirstSpeech  func(time.Time)
	OnFirstUserAudio  func(time.Time)
	OnCallEnded       func(reason EndReason, stats CallStats)
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

// ConversationTurnObserver receives committed user/assistant turns
// without being in the audio pipeline. Implementations should do their
// own timeouts for external I/O.
type ConversationTurnObserver interface {
	OnUserTurnCommitted(text string, at time.Time)
	OnAssistantTurnCommitted(text string, at time.Time, metrics TurnMetrics)
}
