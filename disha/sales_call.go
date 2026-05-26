package disha

import (
	"context"

	"github.com/jaideep329/talk-go/voicepipelinecore"
)

const SalesCallBotType = "sales_call"

func BuildSalesCallSession(ctx context.Context, conversationID string, deps Deps) (*voicepipelinecore.PipelineTask, error) {
	opts, err := BuildSalesCallOptions(ctx, conversationID, deps)
	if err != nil {
		return nil, err
	}
	return voicepipelinecore.NewTask(ctx, opts)
}

func BuildSalesCallOptions(ctx context.Context, conversationID string, deps Deps) (voicepipelinecore.TaskOptions, error) {
	startup, err := collectSalesCallStartup(ctx, conversationID, deps)
	if err != nil {
		return voicepipelinecore.TaskOptions{}, err
	}

	operations := NewSalesCallOperations(startup, deps.Redis, deps.API)

	return voicepipelinecore.TaskOptions{
		Logger:          startup.Logger,
		RoomName:        startup.RoomName,
		InitialMessages: startup.InitialMessages,
		MaxTalkTime:     &startup.MaxTalkTime,
		CallEvents:      registerSalesCallEvents(operations),
	}, nil
}
