package voicepipelinecore

import (
	"context"
	"encoding/json"
	"strings"
	"time"
)

const minBargeInWords = 3

type ContextAggregator struct {
	*BaseProcessor
	taskCtx                          *TaskContext
	messages                         []Message
	currentTranscript                string
	spokenWords                      []string
	interimTranscript                string
	interimResponseID                int
	interruptSent                    bool
	botSpeaking                      bool
	useDefaultPrompt                 bool
	mainAgentSystemPromptLangfuseKey string
}

func NewContextAggregator(taskCtx *TaskContext, initialMessages []Message, mainAgentSystemPromptLangfuseKey string) *ContextAggregator {
	useDefaultPrompt := initialMessages == nil
	messages := []Message{}
	if !useDefaultPrompt {
		messages = messagesFromInitial(initialMessages)
	}
	a := &ContextAggregator{
		taskCtx:                          taskCtx,
		messages:                         messages,
		useDefaultPrompt:                 useDefaultPrompt,
		mainAgentSystemPromptLangfuseKey: mainAgentSystemPromptLangfuseKey,
	}
	a.BaseProcessor = NewBaseProcessor("ContextAggregator", a, taskCtx)
	return a
}

func messagesFromInitial(initial []Message) []Message {
	out := make([]Message, 0, len(initial))
	for _, msg := range initial {
		if msg.Role == "" {
			continue
		}
		if msg.Content == "" && len(msg.ToolCalls) == 0 && msg.ToolCallID == "" {
			continue
		}
		out = append(out, msg)
	}
	return out
}

func cloneMessages(messages []Message) []Message {
	out := make([]Message, len(messages))
	for i, msg := range messages {
		out[i] = msg
		if len(msg.ToolCalls) > 0 {
			out[i].ToolCalls = append([]ToolCall(nil), msg.ToolCalls...)
		}
	}
	return out
}

func (a *ContextAggregator) appendWords(words []string) {
	for _, w := range words {
		if len(a.spokenWords) > 0 && len(w) > 0 && w[0] != '.' && w[0] != ',' && w[0] != '!' && w[0] != '?' && w[0] != ';' && w[0] != ':' {
			a.spokenWords = append(a.spokenWords, " "+w)
		} else {
			a.spokenWords = append(a.spokenWords, w)
		}
	}
}

func (a *ContextAggregator) spokenSoFar() string {
	var spoken string
	for _, w := range a.spokenWords {
		spoken += w
	}
	a.spokenWords = nil
	return spoken
}

func (a *ContextAggregator) resetFinalTranscript() {
	a.currentTranscript = ""
}

func (a *ContextAggregator) resetInterimTranscript() {
	a.interimTranscript = ""
	a.interimResponseID = 0
}

func (a *ContextAggregator) sendLiveTranscript(text string) {
	// Interim user transcription events are intentionally suppressed.
	// They are high-frequency diagnostics/UI traffic and final RTVI
	// user-transcription events are still emitted from addUserMessage.
}

func (a *ContextAggregator) updateInterimTranscript(f TranscriptFrame) string {
	if f.IsFinal && f.Text == "<end>" {
		a.sendLiveTranscript("")
		a.resetInterimTranscript()
		return ""
	}
	if f.IsFinal {
		return a.interimTranscript
	}

	if f.ResponseID != 0 && f.ResponseID != a.interimResponseID {
		a.interimResponseID = f.ResponseID
		a.interimTranscript = ""
	}

	a.interimTranscript += f.Text
	if a.interimTranscript != "" {
		a.sendLiveTranscript(a.interimTranscript)
	}
	return a.interimTranscript
}

func (a *ContextAggregator) updateFinalTranscript(f TranscriptFrame) (string, bool) {
	if !f.IsFinal {
		return "", false
	}
	if f.Text == "<end>" {
		text := a.currentTranscript
		a.resetFinalTranscript()
		return text, true
	}

	a.currentTranscript += f.Text
	return "", false
}

func (a *ContextAggregator) commitSpokenText(interrupted bool) {
	spoken := a.spokenSoFar()
	if spoken != "" {
		a.taskCtx.Logger.Printf("Committing to history (interrupted=%v): %s\n", interrupted, spoken)
		a.messages = append(a.messages, Message{Role: "assistant", Content: spoken})
		metrics := TurnMetrics{}
		if a.taskCtx.metrics != nil {
			metrics = a.taskCtx.metrics.snapshotAndReset()
		}
		if a.taskCtx.callEvents != nil {
			a.taskCtx.callEvents.fireAssistantTurnCommitted(spoken, time.Now(), metrics, a.mainAgentSystemPromptLangfuseKey)
		}
		if interrupted {
			a.taskCtx.UIEvents.BotStoppedSpeaking(time.Now())
		}
	} else if interrupted {
		a.taskCtx.Logger.Println("Barge-in interrupted bot before any assistant words were committed")
		a.taskCtx.UIEvents.BotStoppedSpeaking(time.Now())
	}
}

func (a *ContextAggregator) lastMessageRole() string {
	if len(a.messages) == 0 {
		return ""
	}
	return a.messages[len(a.messages)-1].Role
}

func (a *ContextAggregator) addUserMessage(text string) {
	at := time.Now()
	if a.lastMessageRole() == "user" {
		last := &a.messages[len(a.messages)-1]
		last.Content += " " + text
		a.taskCtx.Logger.Printf("Concatenated user message: %s\n", last.Content)
	} else {
		a.messages = append(a.messages, Message{Role: "user", Content: text})
	}
	a.taskCtx.UIEvents.UserTranscription(text, true, at)
	if a.taskCtx.callEvents != nil {
		a.taskCtx.callEvents.fireUserTurnCommitted(text, at, a.mainAgentSystemPromptLangfuseKey)
	}
}

func (a *ContextAggregator) addFunctionCallInProgress(f FunctionCallInProgressFrame) {
	rawArgs := f.RawArguments
	if rawArgs == "" {
		rawArgs = "{}"
		if len(f.Arguments) > 0 {
			if encoded, err := json.Marshal(f.Arguments); err == nil {
				rawArgs = string(encoded)
			}
		}
	}
	toolCall := ToolCall{
		ID:   f.ToolCallID,
		Type: "function",
		Function: ToolCallFunction{
			Name:      f.FunctionName,
			Arguments: rawArgs,
		},
	}
	toolMessage := Message{
		Role:       "tool",
		Content:    "IN_PROGRESS",
		ToolCallID: f.ToolCallID,
	}
	if len(a.messages) >= 2 {
		last := a.messages[len(a.messages)-1]
		assistant := &a.messages[len(a.messages)-2]
		if last.Role == "tool" && last.Content == "IN_PROGRESS" && assistant.Role == "assistant" && len(assistant.ToolCalls) > 0 {
			assistant.ToolCalls = append(assistant.ToolCalls, toolCall)
			a.messages = append(a.messages, toolMessage)
			return
		}
	}
	a.messages = append(a.messages,
		Message{
			Role:      "assistant",
			ToolCalls: []ToolCall{toolCall},
		},
		toolMessage,
	)
}

func (a *ContextAggregator) applyFunctionCallResult(f FunctionCallResultFrame) {
	result := f.Result
	if result == "" {
		result = "COMPLETED"
	}
	for i := len(a.messages) - 1; i >= 0; i-- {
		msg := &a.messages[i]
		if msg.Role == "tool" && msg.ToolCallID == f.ToolCallID {
			msg.Content = result
			return
		}
	}
	a.messages = append(a.messages, Message{
		Role:       "tool",
		Content:    result,
		ToolCallID: f.ToolCallID,
	})
}

func (a *ContextAggregator) submitUserMessage(text string) {
	a.taskCtx.Logger.Printf("Final transcript received: %s\n", text)
	if len(a.messages) == 0 && a.useDefaultPrompt {
		a.messages = append(a.messages, Message{Role: "system", Content: `You are an expert health coach named Disha. You have deep experience in chronic care management and behavioral change. You are a master influencer and help the users achieve their health goals with the power of conversation.
You have been trained by master clinicians at a company called Curelink.

You are conducting your first telephonic consultation with a new client. You are talking with the user via an audio call on the Disha Health App. Always respond in exactly 2 sentences. Never respond with just 1 sentence.`})
	}
	if a.taskCtx.callEvents != nil {
		a.taskCtx.callEvents.fireUserFirstSpeech(time.Now())
	}
	a.addUserMessage(text)
	a.interruptSent = false
	a.spokenWords = nil
	a.resetInterimTranscript()
	a.resetFinalTranscript()
	a.PushFrame(NewLLMMessagesFrame(cloneMessages(a.messages)), Downstream)
}

func (a *ContextAggregator) ProcessFrame(ctx context.Context, frame Frame, dir Direction) {
	switch f := frame.(type) {
	case EndFrame:
		a.taskCtx.Logger.Printf("EndFrame at ContextAggregator: reason=%q\n", f.Reason)
		a.PushFrame(f, dir)
	case LLMMessagesAppendFrame:
		// Append any provided messages to the context, then (if RunLLM)
		// run a turn on the current context. Pushed on user-join with no
		// messages + RunLLM to make the bot greet first from the initial
		// context (system prompt + "hello?" for a fresh call, or prior
		// chunks + resume note). Consumed here, not forwarded.
		if len(f.Messages) > 0 {
			a.messages = append(a.messages, messagesFromInitial(f.Messages)...)
		}
		if f.RunLLM {
			if len(a.messages) == 0 {
				a.taskCtx.Logger.Println("LLMMessagesAppend run skipped: empty context")
				return
			}
			a.taskCtx.Logger.Println("Running LLM turn from appended context (greet-first / injected)")
			a.PushFrame(NewLLMMessagesFrame(cloneMessages(a.messages)), Downstream)
		}
	case FunctionCallInProgressFrame:
		a.taskCtx.Logger.Printf("Function call in progress: %s tool_call_id=%s\n", f.FunctionName, f.ToolCallID)
		a.addFunctionCallInProgress(f)
		a.PushFrame(f, Upstream)
	case FunctionCallResultFrame:
		a.taskCtx.Logger.Printf("Function call result: %s tool_call_id=%s run_llm=%v\n", f.FunctionName, f.ToolCallID, f.RunLLM)
		a.applyFunctionCallResult(f)
		a.PushFrame(f, Upstream)
		if f.RunLLM {
			a.PushFrame(NewLLMMessagesFrame(cloneMessages(a.messages)), Downstream)
		}
	case TranscriptFrame:
		interimTranscript := a.updateInterimTranscript(f)

		// Barge-in uses the latest non-final response snapshot only.
		// Turn-taking waits for final tokens ending with <end>.
		if a.botSpeaking && !a.interruptSent && !f.IsFinal {
			if len(strings.Fields(interimTranscript)) >= minBargeInWords {
				a.taskCtx.Logger.Println("Barge-in detected")
				a.taskCtx.UIEvents.ServerMessage("Interruption received while bot is speaking", time.Now())
				a.PushFrame(NewInterruptFrame(), Downstream)
				a.interruptSent = true
				a.botSpeaking = false
				a.commitSpokenText(true)
			}
		}
		if text, finished := a.updateFinalTranscript(f); finished {
			if text != "" {
				if a.botSpeaking && !a.interruptSent {
					// Bot speaking, below barge-in threshold — discard.
					// Matches Pipecat: short utterances during bot speech are
					// acknowledgments, not intentional turns.
					a.taskCtx.Logger.Printf("Discarding below-threshold transcript (bot speaking): %s\n", text)
					a.resetInterimTranscript()
				} else {
					a.submitUserMessage(text)
				}
			}
		}
	case WordTimestampFrame:
		// Upstream — record what the bot has actually spoken.
		a.appendWords(f.Words)
	case TTSDoneFrame:
		// Upstream — turn finished cleanly, commit assistant message.
		a.commitSpokenText(false)
		a.botSpeaking = false
		// Mirror Pipecat's reset_aggregation behavior at the bot-turn
		// boundary: any user speech that didn't trigger barge-in
		// during this turn was back-channeling and must not become a
		// user message. Without this, a Soniox <end> arriving a few
		// hundred ms AFTER TTSDone (bot already silent) would fall
		// into the submitUserMessage branch — the LLM would then
		// "acknowledge" 2 words of unrelated speech. The
		// in-progress-discard branch on TranscriptFrame only catches
		// the case where <end> arrives WHILE botSpeaking is still
		// true; this handles the racing case after.
		if !a.interruptSent {
			if a.interimTranscript != "" || a.currentTranscript != "" {
				a.taskCtx.Logger.Printf("Discarding back-channel speech after bot turn: interim=%q final=%q\n", a.interimTranscript, a.currentTranscript)
				a.resetInterimTranscript()
				a.resetFinalTranscript()
				a.sendLiveTranscript("")
			}
		}
	case BotStartedSpeakingFrame:
		a.botSpeaking = true
		a.PushFrame(f, dir) // continue upstream to UserIdle
	case BotStoppedSpeakingFrame:
		a.botSpeaking = false
		a.PushFrame(f, dir) // continue upstream to UserIdle
	default:
		a.PushFrame(frame, dir)
	}
}
