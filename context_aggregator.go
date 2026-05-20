package main

import "strings"

const minBargeInWords = 3

type ContextAggregator struct {
	sessionCtx        *SessionContext
	messages          []map[string]string
	currentTranscript string
	finalResponseID   int
	spokenWords       []string
	interimTranscript string
	interimResponseID int
	interruptSent     bool
	botSpeaking       bool
}

func NewContextAggregator(sessionCtx *SessionContext) *ContextAggregator {
	return &ContextAggregator{
		sessionCtx: sessionCtx,
		messages:   []map[string]string{},
	}
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
	a.finalResponseID = 0
}

func (a *ContextAggregator) resetInterimTranscript() {
	a.interimTranscript = ""
	a.interimResponseID = 0
}

func (a *ContextAggregator) sendLiveTranscript(text string) {
	a.sessionCtx.UIEvents.Send(UIEvent{Type: LiveTranscript, Data: map[string]interface{}{"text": text}})
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
	if f.ResponseID != 0 && f.ResponseID != a.finalResponseID {
		a.finalResponseID = f.ResponseID
		a.currentTranscript = ""
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
		a.sessionCtx.Logger.Printf("Committing to history (interrupted=%v): %s\n", interrupted, spoken)
		a.messages = append(a.messages, map[string]string{"role": "assistant", "content": spoken})
		a.sessionCtx.UIEvents.Send(UIEvent{Type: CommittedAssistant, Data: map[string]interface{}{"role": "assistant", "text": spoken}})
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
		a.sessionCtx.Logger.Printf("Concatenated user message: %s\n", last["content"])
		a.sessionCtx.UIEvents.Send(UIEvent{Type: UserTranscript, Data: map[string]interface{}{"role": "user", "text": last["content"], "is_final": true}})
	} else {
		a.messages = append(a.messages, map[string]string{"role": "user", "content": text})
		a.sessionCtx.UIEvents.Send(UIEvent{Type: UserTranscript, Data: map[string]interface{}{"role": "user", "text": text, "is_final": true}})
	}
}

func (a *ContextAggregator) submitUserMessage(text string, ch ProcessorChannels) {
	a.sessionCtx.Logger.Printf("Final transcript received: %s\n", text)
	if len(a.messages) == 0 {
		a.messages = append(a.messages, map[string]string{"role": "system", "content": `You are an expert health coach named Disha. You have deep experience in chronic care management and behavioral change. You are a master influencer and help the users achieve their health goals with the power of conversation.
You have been trained by master clinicians at a company called Curelink.

You are conducting your first telephonic consultation with a new client. You are talking with the user via an audio call on the Disha Health App. Always respond in exactly 2 sentences. Never respond with just 1 sentence.`})
	}
	a.addUserMessage(text)
	a.interruptSent = false
	a.spokenWords = nil
	a.resetInterimTranscript()
	a.resetFinalTranscript()
	ch.Send(LLMMessagesFrame{Messages: a.messages}, Downstream)
}

func (a *ContextAggregator) Process(ch ProcessorChannels) {
	for {
		select {
		case frame := <-ch.System:
			switch frame.(type) {
			case EndFrame:
				ch.Send(frame, Downstream)
				return
			default:
				ch.Send(frame, Downstream)
			}
		case frame, ok := <-ch.Data:
			if !ok {
				return
			}
			switch f := frame.(type) {
			case TranscriptFrame:
				interimTranscript := a.updateInterimTranscript(f)

				// Barge-in uses the latest non-final response snapshot only.
				// Turn-taking waits for final tokens ending with <end>.
				if a.botSpeaking && !a.interruptSent && !f.IsFinal {
					if len(strings.Fields(interimTranscript)) >= minBargeInWords {
						a.sessionCtx.Logger.Println("Barge-in detected")
						ch.Send(InterruptFrame{}, Downstream)
						a.interruptSent = true
						a.botSpeaking = false
						a.commitSpokenText(true)
					}
				}
				if text, finished := a.updateFinalTranscript(f); finished {
					if text != "" {
						if a.botSpeaking && !a.interruptSent {
							// Bot speaking, below barge-in threshold — discard.
							// Matches Pipecat: short utterances during bot speech
							// are acknowledgments, not intentional turns.
							a.sessionCtx.Logger.Printf("Discarding below-threshold transcript (bot speaking): %s\n", text)
							a.resetInterimTranscript()
						} else {
							a.submitUserMessage(text, ch)
						}
					}
				}
				// Upstream frames from LLM (originally from PlaybackSink via TTS)
			case WordTimestampFrame:
				a.appendWords(f.Words)
			case TTSDoneFrame:
				a.commitSpokenText(false)
				a.botSpeaking = false
			case BotStartedSpeakingFrame:
				a.botSpeaking = true
				ch.Send(f, Upstream) // to UserIdleProcessor
			case BotStoppedSpeakingFrame:
				a.botSpeaking = false
				ch.Send(f, Upstream) // to UserIdleProcessor
			// Pass-through
			case TTSSpeakFrame:
				ch.Send(f, Downstream)
			default:
				ch.Send(f, Downstream)
			}
		}
	}
}
