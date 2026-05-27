package disha

import (
	"context"
	"log"
	"time"

	"github.com/google/uuid"
	"github.com/jaideep329/talk-go/voicepipelinecore"
)

const (
	callEventRequestTimeout = 10 * time.Second
	postCallRequestTimeout  = 10 * time.Second
	chunkWriteTimeout       = 5 * time.Second
)

type CallEventCallbacks struct {
	redis            RedisClient
	api              *APIClient
	logger           *log.Logger
	debugLogUploader DebugLogUploader

	conversationID string
	userID         string
	botType        string
}

func NewCallEventCallbacks(startup CallStartup, redis RedisClient, api *APIClient, debugLogUploader DebugLogUploader) *CallEventCallbacks {
	return &CallEventCallbacks{
		redis:            redis,
		api:              api,
		logger:           startup.Logger,
		debugLogUploader: debugLogUploader,
		conversationID:   startup.ConversationID,
		userID:           startup.UserID,
		botType:          startup.BotType,
	}
}

func (c *CallEventCallbacks) Events() voicepipelinecore.CallEvents {
	if c == nil {
		return voicepipelinecore.CallEvents{}
	}
	return voicepipelinecore.CallEvents{
		OnBotJoined:              c.OnBotJoined,
		OnUserJoined:             c.OnUserJoined,
		OnUserFirstSpeech:        c.OnUserFirstSpeech,
		OnBotFirstSpeech:         c.OnBotFirstSpeech,
		OnFirstUserAudio:         c.OnFirstUserAudio,
		OnUserTurnCommitted:      c.OnUserTurnCommitted,
		OnAssistantTurnCommitted: c.OnAssistantTurnCommitted,
		OnCallEnded:              c.OnCallEnded,
	}
}

func (c *CallEventCallbacks) OnBotJoined(at time.Time) {
	c.updateConversation(UpdateConversationRequest{ConversationID: c.conversationID, BotJoinedAt: &at})
}

func (c *CallEventCallbacks) OnUserJoined(at time.Time) {
	c.updateConversation(UpdateConversationRequest{ConversationID: c.conversationID, UserJoinedAt: &at})
}

func (c *CallEventCallbacks) OnUserFirstSpeech(at time.Time) {
	c.updateConversation(UpdateConversationRequest{ConversationID: c.conversationID, UserFirstSpeechAt: &at})
}

func (c *CallEventCallbacks) OnBotFirstSpeech(at time.Time) {
	c.updateConversation(UpdateConversationRequest{ConversationID: c.conversationID, BotFirstSpeechAt: &at})
}

func (c *CallEventCallbacks) OnFirstUserAudio(time.Time) {}

func (c *CallEventCallbacks) OnUserTurnCommitted(text string, at time.Time) {
	c.appendConversationChunk(text, "user", at, voicepipelinecore.TurnMetrics{})
}

func (c *CallEventCallbacks) OnAssistantTurnCommitted(text string, at time.Time, metrics voicepipelinecore.TurnMetrics) {
	c.appendConversationChunk(text, "assistant", at, metrics)
}

func (c *CallEventCallbacks) OnCallEnded(reason voicepipelinecore.EndReason, stats voicepipelinecore.CallStats) {
	logDataS3Key := uploadDebugLogs(c.logger, c.debugLogUploader, stats.DebugLogs)
	c.runPostCallOperations(reason, stats, logDataS3Key)
	c.enqueueDailyMetrics(stats)
	c.enqueueChunkSync()
}

func (c *CallEventCallbacks) updateConversation(req UpdateConversationRequest) {
	if c == nil || c.api == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), callEventRequestTimeout)
	defer cancel()
	if err := c.api.UpdateConversation(ctx, req); err != nil && c.logger != nil {
		c.logger.Printf("disha: update_conversation failed: %v\n", err)
	}
}

func (c *CallEventCallbacks) appendConversationChunk(text, role string, at time.Time, metrics voicepipelinecore.TurnMetrics) {
	if c == nil || c.redis == nil {
		return
	}
	chunk := ConversationChunk{
		ID:                uuid.NewString(),
		Text:              text,
		Role:              role,
		BotType:           c.botType,
		ConversationID:    c.conversationID,
		UserID:            c.userID,
		LLMTTFBMs:         assistantMetric(role, metrics.LLMTTFBMs),
		TTSTTFBMs:         assistantMetric(role, metrics.TTSTTFBMs),
		V2VLatencyMs:      assistantMetric(role, metrics.E2ELatencyMs),
		TextAggregationMs: assistantMetric(role, metrics.TTSTextAggregationMs),
		Created:           at.Format(time.RFC3339Nano),
		IsDebugLog:        false,
	}
	ctx, cancel := context.WithTimeout(context.Background(), chunkWriteTimeout)
	defer cancel()
	if err := c.redis.AppendChunk(ctx, c.userID, c.conversationID, chunk); err != nil && c.logger != nil {
		c.logger.Printf("disha: chunk persist failed conversation=%s role=%s: %v\n", c.conversationID, role, err)
	}
}

func (c *CallEventCallbacks) runPostCallOperations(reason voicepipelinecore.EndReason, stats voicepipelinecore.CallStats, logDataS3Key string) {
	if c == nil || c.api == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), postCallRequestTimeout)
	defer cancel()
	req := PostCallOperationsRequest{
		ConversationID:                 c.conversationID,
		EndReason:                      mapEndReason(reason),
		TotalUserDuration:              int(stats.TotalUserDurationSec),
		FirstUserAudioFramesReceivedAt: optionalTime(stats.FirstUserAudioFrameAt),
		EndedAt:                        stats.EndedAt,
		LogDataS3Key:                   logDataS3Key,
		OnboardingCallDone:             false,
	}
	if req.EndedAt.IsZero() {
		req.EndedAt = time.Now()
	}
	if err := c.api.RunPostCallOperations(ctx, req); err != nil && c.logger != nil {
		c.logger.Printf("disha: run_post_call_operations failed conversation=%s user=%s: %v\n", c.conversationID, c.userID, err)
	}
}

func (c *CallEventCallbacks) enqueueDailyMetrics(stats voicepipelinecore.CallStats) {
	if c == nil || c.api == nil || stats.MeetingID == "" || stats.UserSessionID == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), postCallRequestTimeout)
	defer cancel()
	req := EnqueueJobRequest{
		ModuleName: "bots.webhooks",
		FuncName:   "fetch_and_store_daily_metrics",
		Kwargs: map[string]any{
			"conversation_id": c.conversationID,
			"meeting_id":      stats.MeetingID,
			"bot_session_id":  stats.BotSessionID,
			"user_session_id": stats.UserSessionID,
		},
		SQSQueue: "p1-fast-l1",
	}
	if err := c.api.EnqueueJob(ctx, req); err != nil && c.logger != nil {
		c.logger.Printf("disha: enqueue Daily metrics failed conversation=%s user=%s: %v\n", c.conversationID, c.userID, err)
	}
}

func (c *CallEventCallbacks) enqueueChunkSync() {
	if c == nil || c.api == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), postCallRequestTimeout)
	defer cancel()
	req := EnqueueJobRequest{
		ModuleName: "services.conversation_chunk_manager",
		FuncName:   "sync_conversation_chunks_to_db",
		Kwargs: map[string]any{
			"user_id":         c.userID,
			"conversation_id": c.conversationID,
			"bot_type":        c.botType,
		},
		SQSQueue: "p1-fast-l1",
	}
	if err := c.api.EnqueueJob(ctx, req); err != nil && c.logger != nil {
		c.logger.Printf("disha: enqueue chunk sync failed conversation=%s user=%s: %v\n", c.conversationID, c.userID, err)
	}
}

func optionalTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

func mapEndReason(reason voicepipelinecore.EndReason) *string {
	switch reason {
	case voicepipelinecore.EndReasonTalkTimeExhausted:
		value := "talktime_exhausted"
		return &value
	case voicepipelinecore.EndReasonUserIdle:
		value := "user_idle"
		return &value
	default:
		return nil
	}
}

func assistantMetric(role string, value float64) *float64 {
	if role != "assistant" || value == 0 {
		return nil
	}
	return &value
}
