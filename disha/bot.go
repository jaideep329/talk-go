package disha

import (
	"context"
	"fmt"
	"log"

	"github.com/jaideep329/talk-go/voicepipelinecore"
)

type Deps struct {
	Logger       *log.Logger
	Redis        RedisClient
	API          *APIClient
	Documents    *DocumentStore
	PhoneticDict *PhoneticDict
	GKEPatcher   *GKEPodPatcher
}

type Bot interface {
	BotType() string
	BuildOptions(ctx context.Context, conversationID string, deps Deps) (voicepipelinecore.TaskOptions, error)
}

func NewBot(botType string) (Bot, error) {
	switch botType {
	case SalesCallBotType:
		return SalesCallBot{}, nil
	default:
		return nil, fmt.Errorf("disha: unsupported bot_type %q", botType)
	}
}

func NewBotTask(ctx context.Context, bot Bot, conversationID string, deps Deps) (*voicepipelinecore.PipelineTask, error) {
	if bot == nil {
		return nil, fmt.Errorf("disha: bot is required")
	}
	opts, err := bot.BuildOptions(ctx, conversationID, deps)
	if err != nil {
		return nil, err
	}
	return voicepipelinecore.NewTask(ctx, opts)
}
