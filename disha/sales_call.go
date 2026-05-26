package disha

import (
	"context"
	"time"

	"github.com/jaideep329/talk-go/voicepipelinecore"
)

const (
	SalesCallBotType        = "sales_call"
	lifetimeTalkTimeSeconds = 600
)

const SalesCallSystemPrompt = `You are an expert health coach named Disha. You have deep experience in chronic care management and behavioral change. You are a master influencer and help users achieve their health goals with the power of conversation.
You have been trained by master clinicians at a company called Curelink.

You are conducting your first telephonic consultation with a new client. You are talking with the user via an audio call on the Disha Health App. Always respond in exactly 2 sentences. Never respond with just 1 sentence.`

type SalesCallBot struct{}

var _ Bot = SalesCallBot{}

func (SalesCallBot) BotType() string {
	return SalesCallBotType
}

func (b SalesCallBot) BuildOptions(ctx context.Context, conversationID string, deps Deps) (voicepipelinecore.TaskOptions, error) {
	startup, err := collectCallStartup(ctx, conversationID, SalesCallBotType, deps)
	if err != nil {
		return voicepipelinecore.TaskOptions{}, err
	}

	maxTalkTime := salesTalkTimeLimit(startup.Data.UserProfile.RemainingSalesCallTalktimeSeconds)
	callbacks := NewCallEventCallbacks(startup, deps.Redis, deps.API)

	return voicepipelinecore.TaskOptions{
		Logger:   startup.Logger,
		RoomName: startup.RoomName,
		InitialMessages: initialMessagesFromChunks(
			SalesCallSystemPrompt,
			startup.Data.Chunks,
			voicepipelinecore.Message{Role: "user", Content: "hello?"},
		),
		MaxTalkTime: &maxTalkTime,
		CallEvents:  callbacks.Events(),
	}, nil
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
