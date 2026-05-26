package voicepipelinecore

import (
	"context"
	"strings"
	"time"
)

const minBargeInWords = 3

type ContextAggregator struct {
	*BaseProcessor
	taskCtx           *TaskContext
	messages          []map[string]string
	currentTranscript string
	spokenWords       []string
	interimTranscript string
	interimResponseID int
	interruptSent     bool
	botSpeaking       bool
	useDefaultPrompt  bool
}

func NewContextAggregator(taskCtx *TaskContext, initialMessages ...[]Message) *ContextAggregator {
	useDefaultPrompt := true
	messages := []map[string]string{}
	if len(initialMessages) > 0 {
		useDefaultPrompt = false
		messages = messagesFromInitial(initialMessages[0])
	}
	a := &ContextAggregator{
		taskCtx:          taskCtx,
		messages:         messages,
		useDefaultPrompt: useDefaultPrompt,
	}
	a.BaseProcessor = NewBaseProcessor("ContextAggregator", a, taskCtx)
	return a
}

func messagesFromInitial(initial []Message) []map[string]string {
	messages := make([]map[string]string, 0, len(initial))
	for _, msg := range initial {
		if msg.Role == "" || msg.Content == "" {
			continue
		}
		messages = append(messages, map[string]string{"role": msg.Role, "content": msg.Content})
	}
	return messages
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
	if a.taskCtx == nil || a.taskCtx.UIEvents == nil {
		return
	}
	a.taskCtx.UIEvents.Send(UIEvent{Type: LiveTranscript, Data: map[string]interface{}{"text": text}})
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
		a.messages = append(a.messages, map[string]string{"role": "assistant", "content": spoken})
		a.taskCtx.UIEvents.Send(UIEvent{Type: CommittedAssistant, Data: map[string]interface{}{"role": "assistant", "text": spoken}})
		metrics := TurnMetrics{}
		if a.taskCtx.metrics != nil {
			metrics = a.taskCtx.metrics.snapshotAndReset()
		}
		if a.taskCtx.callEvents != nil {
			a.taskCtx.callEvents.fireAssistantTurnCommitted(spoken, time.Now(), metrics)
		}
	} else if interrupted {
		a.taskCtx.Logger.Println("Barge-in interrupted bot before any assistant words were committed")
	}
}

func (a *ContextAggregator) lastMessageRole() string {
	if len(a.messages) == 0 {
		return ""
	}
	return a.messages[len(a.messages)-1]["role"]
}

func (a *ContextAggregator) addUserMessage(text string) {
	if a.lastMessageRole() == "user" {
		last := a.messages[len(a.messages)-1]
		last["content"] += " " + text
		a.taskCtx.Logger.Printf("Concatenated user message: %s\n", last["content"])
		a.taskCtx.UIEvents.Send(UIEvent{Type: UserTranscript, Data: map[string]interface{}{"role": "user", "text": last["content"], "is_final": true}})
	} else {
		a.messages = append(a.messages, map[string]string{"role": "user", "content": text})
		a.taskCtx.UIEvents.Send(UIEvent{Type: UserTranscript, Data: map[string]interface{}{"role": "user", "text": text, "is_final": true}})
	}
	if a.taskCtx.callEvents != nil {
		a.taskCtx.callEvents.fireUserTurnCommitted(text, time.Now())
	}
}

func (a *ContextAggregator) submitUserMessage(text string) {
	a.taskCtx.Logger.Printf("Final transcript received: %s\n", text)
	if len(a.messages) == 0 && a.useDefaultPrompt {
		a.messages = append(a.messages, map[string]string{"role": "system", "content": `You are an expert health coach named Disha. You have deep experience in chronic care management and behavioral change. You are a master influencer and help the users achieve their health goals with the power of conversation.
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
	a.PushFrame(NewLLMMessagesFrame(a.messages), Downstream)
}

func (a *ContextAggregator) ProcessFrame(ctx context.Context, frame Frame, dir Direction) {
	switch f := frame.(type) {
	case EndFrame:
		a.taskCtx.Logger.Printf("EndFrame at ContextAggregator: reason=%q\n", f.Reason)
		a.PushFrame(f, dir)
	case TranscriptFrame:
		interimTranscript := a.updateInterimTranscript(f)

		// Barge-in uses the latest non-final response snapshot only.
		// Turn-taking waits for final tokens ending with <end>.
		if a.botSpeaking && !a.interruptSent && !f.IsFinal {
			if len(strings.Fields(interimTranscript)) >= minBargeInWords {
				a.taskCtx.Logger.Println("Barge-in detected")
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
