package disha

import (
	"context"
	"log"

	"github.com/jaideep329/talk-go/voicepipelinecore"
)

type Deps struct {
	Logger *log.Logger
	Redis  RedisClient
	API    *APIClient
}

func BuildSession(ctx context.Context, conversationID string, deps Deps) (*voicepipelinecore.PipelineTask, error) {
	return BuildSalesCallSession(ctx, conversationID, deps)
}

func BuildOptions(ctx context.Context, conversationID string, deps Deps) (voicepipelinecore.TaskOptions, error) {
	return BuildSalesCallOptions(ctx, conversationID, deps)
}
