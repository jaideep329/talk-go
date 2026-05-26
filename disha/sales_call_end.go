package disha

import (
	"context"
	"time"

	"github.com/jaideep329/talk-go/voicepipelinecore"
)

const postCallRequestTimeout = 10 * time.Second

func runSalesCallEndOperations(operations *SalesCallOperations, reason voicepipelinecore.EndReason, stats voicepipelinecore.CallStats) {
	runPostCallOperations(operations, reason, stats)
	enqueueChunkSync(operations)
}

func runPostCallOperations(operations *SalesCallOperations, reason voicepipelinecore.EndReason, stats voicepipelinecore.CallStats) {
	ctx, cancel := context.WithTimeout(context.Background(), postCallRequestTimeout)
	defer cancel()
	req := PostCallOperationsRequest{
		ConversationID:                 operations.conversationID,
		EndReason:                      mapEndReason(reason),
		TotalUserDuration:              int(stats.TotalUserDurationSec),
		FirstUserAudioFramesReceivedAt: optionalTime(stats.FirstUserAudioFrameAt),
		EndedAt:                        stats.EndedAt,
		LogDataS3Key:                   "",
		OnboardingCallDone:             false,
	}
	if req.EndedAt.IsZero() {
		req.EndedAt = time.Now()
	}
	if err := operations.api.RunPostCallOperations(ctx, req); err != nil && operations.logger != nil {
		operations.logger.Printf("disha: run_post_call_operations failed conversation=%s user=%s: %v\n", operations.conversationID, operations.userID, err)
	}
}

func enqueueChunkSync(operations *SalesCallOperations) {
	ctx, cancel := context.WithTimeout(context.Background(), postCallRequestTimeout)
	defer cancel()
	req := EnqueueJobRequest{
		ModuleName: "services.conversation_chunk_manager",
		FuncName:   "sync_conversation_chunks_to_db",
		Kwargs: map[string]any{
			"user_id":         operations.userID,
			"conversation_id": operations.conversationID,
			"bot_type":        SalesCallBotType,
		},
		SQSQueue: "p1-fast-l1",
	}
	if err := operations.api.EnqueueJob(ctx, req); err != nil && operations.logger != nil {
		operations.logger.Printf("disha: enqueue chunk sync failed conversation=%s user=%s: %v\n", operations.conversationID, operations.userID, err)
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
