package disha

import (
	"time"

	"github.com/jaideep329/talk-go/voicepipelinecore"
)

type CallOperations interface {
	OnBotJoined(at time.Time)
	OnUserJoined(at time.Time)
	OnUserFirstSpeech(at time.Time)
	OnBotFirstSpeech(at time.Time)
	OnFirstUserAudio(at time.Time)
	OnUserTurnCommitted(text string, at time.Time)
	OnAssistantTurnCommitted(text string, at time.Time, metrics voicepipelinecore.TurnMetrics)
	OnCallEnded(reason voicepipelinecore.EndReason, stats voicepipelinecore.CallStats)
}

func CallEventsForOperations(ops CallOperations) voicepipelinecore.CallEvents {
	if ops == nil {
		return voicepipelinecore.CallEvents{}
	}
	return voicepipelinecore.CallEvents{
		OnBotJoined:              ops.OnBotJoined,
		OnUserJoined:             ops.OnUserJoined,
		OnUserFirstSpeech:        ops.OnUserFirstSpeech,
		OnBotFirstSpeech:         ops.OnBotFirstSpeech,
		OnFirstUserAudio:         ops.OnFirstUserAudio,
		OnUserTurnCommitted:      ops.OnUserTurnCommitted,
		OnAssistantTurnCommitted: ops.OnAssistantTurnCommitted,
		OnCallEnded:              ops.OnCallEnded,
	}
}
