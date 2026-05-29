package disha

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jaideep329/talk-go/voicepipelinecore"
)

const (
	SalesCallBotType        = "sales_call"
	lifetimeTalkTimeSeconds = 600

	// Prompt names in the Disha document store. Same as Python's
	// bots/sales_call/sales_call.py.
	salesPrompt21Rs    = "sales_call/main_sys-3day_21rs_v2"
	salesPrompt299Rs   = "sales_call/main_sys-30day_299rs_v2"
	salesPromptDefault = "sales_call/main_sys-3day_v2"

	// Campaign-pricing-experiment values, from Python
	// users/models.py:UserProfile.CampaignPricingExperimentFlag.
	campaignFlag21For3Days   = "21_for_3_days"
	campaignFlag299For30Days = "299_for_30_days"
)

// SalesCallBot is the Disha-specific assembly of a sales call. The
// pipeline itself is built by voicepipelinecore; this type only wires
// in the dynamic prompt, the talktime override, the phonetic dict,
// and the call-event callbacks that talk to Redis + Disha's REST API.
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

	prompt, promptName, promptVersion, err := loadSalesPrompt(ctx, deps.Documents, startup)
	if err != nil {
		return voicepipelinecore.TaskOptions{}, err
	}
	startup.Logger.Printf("disha: sales prompt selected name=%s version=%d\n", promptName, promptVersion)

	resumeMsg := buildResumeSystemMessage(startup.Data, time.Now())
	if resumeMsg != "" {
		startup.Logger.Println("disha: appending resume system message")
	}

	maxTalkTime := salesTalkTimeLimit(startup.Data.UserProfile.RemainingSalesCallTalktimeSeconds)
	callbacks := NewCallEventCallbacks(
		startup,
		deps.Redis,
		deps.API,
		NewDebugLogUploaderFromEnv(startup.Logger, startup.ConversationID),
	)

	opts := voicepipelinecore.TaskOptions{
		Logger:   startup.Logger,
		RoomName: startup.RoomName,
		InitialMessages: buildInitialMessages(
			prompt,
			startup.Data.Chunks,
			resumeMsg,
		),
		MaxTalkTime: &maxTalkTime,
		CallEvents:  callbacks.Events(),
	}
	if deps.PhoneticDict != nil {
		opts.PhoneticDict = deps.PhoneticDict.Dictionary(ctx)
	}
	return opts, nil
}

// loadSalesPrompt picks the prompt name based on
// campaign_pricing_experiment_flag (matching Python) and fetches it
// from the document store. Variables passed to the template mirror
// `sales_call_variables` in sales_call.py.
func loadSalesPrompt(ctx context.Context, store *DocumentStore, startup CallStartup) (string, string, int, error) {
	name := salesPromptDefault
	switch derefString(startup.Data.UserProfile.CampaignPricingExperimentFlag) {
	case campaignFlag21For3Days:
		name = salesPrompt21Rs
	case campaignFlag299For30Days:
		name = salesPrompt299Rs
	}

	if store == nil {
		return "", "", 0, fmt.Errorf("disha: document store is required to load %q", name)
	}

	vars := map[string]string{
		"patient_info":     startup.Data.Conversation.PatientInfo,
		"current_datetime": time.Now().In(istLocation()).Format("2 Jan 2006 15:04:05"),
		// short_term_memory mirrors Python's fetch_conversation: the
		// user-profile value concatenated with `unprocessed_chat_context`
		// when present. Today's sales prompts don't reference it, but
		// exposing it keeps us compatible with future prompt edits
		// without another code change.
		"short_term_memory": mergeShortTermMemory(
			derefString(startup.Data.UserProfile.ShortTermMemory),
			derefString(startup.Data.UnprocessedChatContext),
		),
	}
	text, version, err := store.GetDocument(ctx, name, 0, vars)
	if err != nil {
		return "", "", 0, fmt.Errorf("disha: load sales prompt %q: %w", name, err)
	}
	if strings.TrimSpace(text) == "" {
		return "", "", 0, fmt.Errorf("disha: sales prompt %q is empty", name)
	}
	return text, name, version, nil
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
