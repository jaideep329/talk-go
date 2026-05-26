package disha

import (
	"context"
	"errors"
	"fmt"
	"log"

	"github.com/jaideep329/talk-go/voicepipelinecore"
)

const roomNameConversationPref = "conv"

type CallStartup struct {
	ConversationID string
	UserID         string
	BotType        string
	RoomName       string
	Logger         *log.Logger
	Data           *ConversationData
}

func collectCallStartup(ctx context.Context, conversationID, expectedBotType string, deps Deps) (CallStartup, error) {
	if conversationID == "" {
		return CallStartup{}, errors.New("disha: conversation_id is required")
	}
	if deps.Redis == nil {
		return CallStartup{}, errors.New("disha: Redis dependency is required")
	}
	if deps.API == nil {
		return CallStartup{}, errors.New("disha: API dependency is required")
	}

	data, err := deps.Redis.GetConversationData(ctx, conversationID)
	if err != nil {
		return CallStartup{}, fmt.Errorf("disha: load conversation_data: %w", err)
	}
	if expectedBotType != "" && data.Conversation.BotType != expectedBotType {
		return CallStartup{}, fmt.Errorf("disha: unsupported bot_type %q", data.Conversation.BotType)
	}

	userID := data.Conversation.UserID
	if userID == "" {
		userID = data.UserProfile.UserID
	}
	if userID == "" {
		return CallStartup{}, errors.New("disha: user_id is missing from conversation_data")
	}

	logger := deps.Logger
	if logger == nil {
		logger = log.Default()
	}
	callLogger := log.New(logger.Writer(), fmt.Sprintf("[conv=%s] ", conversationID), logger.Flags())

	return CallStartup{
		ConversationID: conversationID,
		UserID:         userID,
		BotType:        data.Conversation.BotType,
		RoomName:       fmt.Sprintf("%s-%s", roomNameConversationPref, conversationID),
		Logger:         callLogger,
		Data:           data,
	}, nil
}

func initialMessagesFromChunks(systemPrompt string, chunks [][]any, freshSeed voicepipelinecore.Message) []voicepipelinecore.Message {
	msgs := []voicepipelinecore.Message{{Role: "system", Content: systemPrompt}}
	for _, tuple := range chunks {
		msg, ok := messageFromChunkTuple(tuple)
		if ok {
			msgs = append(msgs, msg)
		}
	}
	if len(msgs) == 1 && freshSeed != (voicepipelinecore.Message{}) {
		msgs = append(msgs, freshSeed)
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
