package disha

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/jaideep329/talk-go/voicepipelinecore"
)

const (
	lifetimeTalkTimeSeconds  = 600
	roomNameConversationPref = "conv"
)

const SalesCallSystemPrompt = `You are an expert health coach named Disha. You have deep experience in chronic care management and behavioral change. You are a master influencer and help users achieve their health goals with the power of conversation.
You have been trained by master clinicians at a company called Curelink.

You are conducting your first telephonic consultation with a new client. You are talking with the user via an audio call on the Disha Health App. Always respond in exactly 2 sentences. Never respond with just 1 sentence.`

type salesCallStartup struct {
	ConversationID  string
	UserID          string
	RoomName        string
	Logger          *log.Logger
	InitialMessages []voicepipelinecore.Message
	MaxTalkTime     time.Duration
}

func collectSalesCallStartup(ctx context.Context, conversationID string, deps Deps) (salesCallStartup, error) {
	if conversationID == "" {
		return salesCallStartup{}, errors.New("disha: conversation_id is required")
	}
	if deps.Redis == nil {
		return salesCallStartup{}, errors.New("disha: Redis dependency is required")
	}
	if deps.API == nil {
		return salesCallStartup{}, errors.New("disha: API dependency is required")
	}

	data, err := deps.Redis.GetConversationData(ctx, conversationID)
	if err != nil {
		return salesCallStartup{}, fmt.Errorf("disha: load conversation_data: %w", err)
	}
	if data.Conversation.BotType != SalesCallBotType {
		return salesCallStartup{}, fmt.Errorf("disha: unsupported bot_type %q", data.Conversation.BotType)
	}

	userID := data.Conversation.UserID
	if userID == "" {
		userID = data.UserProfile.UserID
	}
	if userID == "" {
		return salesCallStartup{}, errors.New("disha: user_id is missing from conversation_data")
	}

	logger := deps.Logger
	if logger == nil {
		logger = log.Default()
	}
	callLogger := log.New(logger.Writer(), fmt.Sprintf("[conv=%s] ", conversationID), logger.Flags())

	return salesCallStartup{
		ConversationID:  conversationID,
		UserID:          userID,
		RoomName:        fmt.Sprintf("%s-%s", roomNameConversationPref, conversationID),
		Logger:          callLogger,
		InitialMessages: buildSalesCallInitialMessages(data),
		MaxTalkTime:     salesTalkTimeLimit(data.UserProfile.RemainingSalesCallTalktimeSeconds),
	}, nil
}

func buildSalesCallInitialMessages(data *ConversationData) []voicepipelinecore.Message {
	msgs := []voicepipelinecore.Message{{Role: "system", Content: SalesCallSystemPrompt}}
	for _, tuple := range data.Chunks {
		msg, ok := messageFromChunkTuple(tuple)
		if ok {
			msgs = append(msgs, msg)
		}
	}
	if len(msgs) == 1 {
		msgs = append(msgs, voicepipelinecore.Message{Role: "user", Content: "hello?"})
	}
	return msgs
}

func messageFromChunkTuple(tuple []any) (voicepipelinecore.Message, bool) {
	if len(tuple) < 4 {
		return voicepipelinecore.Message{}, false
	}
	role, _ := tuple[1].(string)
	text, _ := tuple[2].(string)
	isDebug, _ := tuple[3].(bool)
	if isDebug || text == "" || (role != "user" && role != "assistant") {
		return voicepipelinecore.Message{}, false
	}
	if len(tuple) >= 5 && tuple[4] != nil {
		return voicepipelinecore.Message{}, false
	}
	return voicepipelinecore.Message{Role: role, Content: text}, true
}

func salesTalkTimeLimit(remainingSeconds *float64) time.Duration {
	seconds := float64(lifetimeTalkTimeSeconds)
	if remainingSeconds != nil {
		seconds = *remainingSeconds
		if seconds < 0 {
			seconds = 0
		}
	}
	return time.Duration(seconds * float64(time.Second))
}
