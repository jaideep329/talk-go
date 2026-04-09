package main

import "strings"

const minBargeInWords = 3

type ContextAggregator struct {
	sessionCtx        *SessionContext
	messages          []map[string]string
	currentTranscript string
	spokenWords       []string
	bargeInFinals     string // accumulated final tokens for barge-in word count
	bargeInInterim    string // latest non-final text (replaces, not accumulates)
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
	a.bargeInFinals = ""
	a.bargeInInterim = ""
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
				// Barge-in: build a live transcript snapshot (finals accumulate,
				// interims replace) and check word count — matches Pipecat's
				// per-frame full-text check. Uses interims for faster detection
				// without double-counting.
				if a.botSpeaking && !a.interruptSent {
					if f.IsFinal && f.Text != "<end>" {
						a.bargeInFinals += f.Text
						a.bargeInInterim = "" // final replaces interim
					} else if !f.IsFinal {
						a.bargeInInterim = f.Text // latest interim snapshot
					}
					liveText := a.bargeInFinals + a.bargeInInterim
					if len(strings.Fields(liveText)) >= minBargeInWords {
						a.sessionCtx.Logger.Println("Barge-in detected")
						ch.Send(InterruptFrame{}, Downstream)
						a.interruptSent = true
						a.bargeInFinals = ""
						a.bargeInInterim = ""
						a.commitSpokenText(true)
					}
				}
				// Accumulate final transcripts, trigger LLM on <end>
				if f.IsFinal {
					if f.Text == "<end>" {
						if a.currentTranscript != "" {
							if a.botSpeaking && !a.interruptSent {
								// Bot speaking, below barge-in threshold — discard.
								// Matches Pipecat: short utterances during bot speech
								// are acknowledgments, not intentional turns.
								a.sessionCtx.Logger.Printf("Discarding below-threshold transcript (bot speaking): %s\n", a.currentTranscript)
								a.currentTranscript = ""
								a.bargeInFinals = ""
								a.bargeInInterim = ""
							} else {
								a.submitUserMessage(a.currentTranscript, ch)
								a.currentTranscript = ""
							}
						}
					} else {
						a.currentTranscript += f.Text
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
