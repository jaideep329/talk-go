package disha

import (
	"context"
	"log"
	"time"

	"github.com/google/uuid"
	"github.com/jaideep329/talk-go/voicepipelinecore"
)

const chunkWriteTimeout = 5 * time.Second

type SalesCallOperations struct {
	redis  RedisClient
	api    *APIClient
	logger *log.Logger

	conversationID string
	userID         string
}

func NewSalesCallOperations(startup salesCallStartup, redis RedisClient, api *APIClient) *SalesCallOperations {
	return &SalesCallOperations{
		redis:          redis,
		api:            api,
		logger:         startup.Logger,
		conversationID: startup.ConversationID,
		userID:         startup.UserID,
	}
}

func (o *SalesCallOperations) OnBotJoined(at time.Time) {
	o.updateConversation(UpdateConversationRequest{ConversationID: o.conversationID, BotJoinedAt: &at})
}

func (o *SalesCallOperations) OnUserJoined(at time.Time) {
	o.updateConversation(UpdateConversationRequest{ConversationID: o.conversationID, UserJoinedAt: &at})
}

func (o *SalesCallOperations) OnUserFirstSpeech(at time.Time) {
	o.updateConversation(UpdateConversationRequest{ConversationID: o.conversationID, UserFirstSpeechAt: &at})
}

func (o *SalesCallOperations) OnBotFirstSpeech(at time.Time) {
	o.updateConversation(UpdateConversationRequest{ConversationID: o.conversationID, BotFirstSpeechAt: &at})
}

func (o *SalesCallOperations) OnFirstUserAudio(time.Time) {}

func (o *SalesCallOperations) OnUserTurnCommitted(text string, at time.Time) {
	o.appendConversationChunk(text, "user", at, voicepipelinecore.TurnMetrics{})
}

func (o *SalesCallOperations) OnAssistantTurnCommitted(text string, at time.Time, metrics voicepipelinecore.TurnMetrics) {
	o.appendConversationChunk(text, "assistant", at, metrics)
}

func (o *SalesCallOperations) OnCallEnded(reason voicepipelinecore.EndReason, stats voicepipelinecore.CallStats) {
	runSalesCallEndOperations(o, reason, stats)
}

func (o *SalesCallOperations) updateConversation(req UpdateConversationRequest) {
	ctx, cancel := context.WithTimeout(context.Background(), callEventRequestTimeout)
	defer cancel()
	if err := o.api.UpdateConversation(ctx, req); err != nil && o.logger != nil {
		o.logger.Printf("disha: update_conversation failed: %v\n", err)
	}
}

func (o *SalesCallOperations) appendConversationChunk(text, role string, at time.Time, metrics voicepipelinecore.TurnMetrics) {
	if o == nil || o.redis == nil {
		return
	}
	chunk := ConversationChunk{
		ID:                uuid.NewString(),
		Text:              text,
		Role:              role,
		BotType:           SalesCallBotType,
		ConversationID:    o.conversationID,
		UserID:            o.userID,
		LLMTTFBMs:         assistantMetric(role, metrics.LLMTTFBMs),
		TTSTTFBMs:         assistantMetric(role, metrics.TTSTTFBMs),
		V2VLatencyMs:      assistantMetric(role, metrics.E2ELatencyMs),
		TextAggregationMs: assistantMetric(role, metrics.TTSTextAggregationMs),
		Created:           at.Format(time.RFC3339Nano),
		IsDebugLog:        false,
	}
	ctx, cancel := context.WithTimeout(context.Background(), chunkWriteTimeout)
	defer cancel()
	if err := o.redis.AppendChunk(ctx, o.userID, o.conversationID, chunk); err != nil && o.logger != nil {
		o.logger.Printf("disha: chunk persist failed conversation=%s role=%s: %v\n", o.conversationID, role, err)
	}
}

func assistantMetric(role string, value float64) *float64 {
	if role != "assistant" || value == 0 {
		return nil
	}
	return &value
}
