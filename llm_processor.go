package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"time"
)

type LLMProcessor struct {
	sessionCtx        *SessionContext
	metrics           *ProcessorMetrics
	messages          []map[string]string
	currentTranscript string
	isStreaming       bool
	interruptSent     bool
	cancelLLM         context.CancelFunc
	responseFrames    chan Frame
	spokenWords       []string // words accumulated from upstream WordTimestampFrames
}

func (p *LLMProcessor) appendWords(words []string) {
	for _, w := range words {
		if len(p.spokenWords) > 0 && len(w) > 0 && w[0] != '.' && w[0] != ',' && w[0] != '!' && w[0] != '?' && w[0] != ';' && w[0] != ':' {
			p.spokenWords = append(p.spokenWords, " "+w)
		} else {
			p.spokenWords = append(p.spokenWords, w)
		}
	}
}

func (p *LLMProcessor) spokenSoFar() string {
	var spoken string
	for _, w := range p.spokenWords {
		spoken += w
	}
	p.spokenWords = nil
	return spoken
}

func (p *LLMProcessor) runLLM(ctx context.Context) {
	body := map[string]interface{}{
		"model":    "gpt-4.1",
		"stream":   true,
		"messages": p.messages,
	}
	jsonBody, err := json.Marshal(body)
	if err != nil {
		p.sessionCtx.Logger.Println("json marshal error:", err)
		return
	}

	req, err := http.NewRequestWithContext(ctx, "POST", "https://api.openai.com/v1/chat/completions", bytes.NewReader(jsonBody))
	if err != nil {
		p.sessionCtx.Logger.Println("request error:", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+os.Getenv("OPENAI_API_KEY"))
	p.metrics.Start(MetricTTFB)
	p.metrics.Start(MetricProcessing)
	p.responseFrames <- LLMResponseStartFrame{StartedAt: time.Now()}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		p.sessionCtx.Logger.Println("LLM request failed:", err)
		return
	}
	defer resp.Body.Close()

	firstToken := true
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			break
		}

		var chunk struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			continue
		}
		if len(chunk.Choices) > 0 {
			content := chunk.Choices[0].Delta.Content
			if content == "" {
				continue
			}
			if firstToken {
				firstToken = false
				if mf := p.metrics.Stop(MetricTTFB); mf != nil {
					p.responseFrames <- *mf
				}
			}
			p.responseFrames <- TextFrame{Text: content}
		}
	}
	if ctx.Err() == nil {
		if mf := p.metrics.Stop(MetricProcessing); mf != nil {
			p.responseFrames <- *mf
		}
		p.responseFrames <- LLMResponseEndFrame{}
	}
}

func NewLLMProcessor(sessionCtx *SessionContext) *LLMProcessor {
	return &LLMProcessor{
		sessionCtx:     sessionCtx,
		metrics:        NewProcessorMetrics("llm"),
		messages:       []map[string]string{},
		responseFrames: make(chan Frame, 100),
	}
}

func (p *LLMProcessor) commitSpokenText(interrupted bool) {
	spoken := p.spokenSoFar()
	if spoken != "" {
		p.sessionCtx.Logger.Printf("Committing to history (interrupted=%v): %s\n", interrupted, spoken)
		p.messages = append(p.messages, map[string]string{"role": "assistant", "content": spoken})
		p.sessionCtx.UIEvents.Send(UIEvent{Type: CommittedAssistant, Data: map[string]interface{}{"role": "assistant", "text": spoken}})
	}
}

// lastMessageRole returns the role of the last message in the conversation history.
func (p *LLMProcessor) lastMessageRole() string {
	if len(p.messages) == 0 {
		return ""
	}
	return p.messages[len(p.messages)-1]["role"]
}

// addUserMessage either appends to the last user message (if no assistant
// response came between) or creates a new user message.
func (p *LLMProcessor) addUserMessage(text string) {
	if p.lastMessageRole() == "user" {
		// Concatenate — previous turn got no assistant response (barge-in with nothing spoken)
		last := p.messages[len(p.messages)-1]
		last["content"] += " " + text
		p.sessionCtx.Logger.Printf("Concatenated user message: %s\n", last["content"])
		p.sessionCtx.UIEvents.Send(UIEvent{Type: UserTranscript, Data: map[string]interface{}{"role": "user", "text": last["content"], "is_final": true}})
	} else {
		p.messages = append(p.messages, map[string]string{"role": "user", "content": text})
		p.sessionCtx.UIEvents.Send(UIEvent{Type: UserTranscript, Data: map[string]interface{}{"role": "user", "text": text, "is_final": true}})
	}
}

func (p *LLMProcessor) Process(ch ProcessorChannels) {
	for {
		// Priority: check system channel first.
		select {
		case frame := <-ch.System:
			switch frame.(type) {
			case EndFrame:
				ch.Send(frame, Downstream)
				return
			}
		default:
			select {
			case frame := <-ch.System:
				switch frame.(type) {
				case EndFrame:
					ch.Send(frame, Downstream)
					return
				}
			case frame, ok := <-ch.Data:
				if !ok {
					return
				}
				switch f := frame.(type) {
				case TranscriptFrame:
					if p.isStreaming && !p.interruptSent {
						p.sessionCtx.Logger.Println("Barge-in detected, cancelling LLM")
						if p.cancelLLM != nil {
							p.cancelLLM()
						}
						p.metrics.Reset()
						ch.Send(InterruptFrame{}, Downstream)
						p.isStreaming = false
						p.interruptSent = true
						p.commitSpokenText(true)
					}
					if f.IsFinal {
						if f.Text == "<end>" {
							if p.currentTranscript != "" {
								p.sessionCtx.Logger.Printf("Final transcript received: %s\n", p.currentTranscript)
								if len(p.messages) == 0 {
									p.messages = append(p.messages, map[string]string{"role": "system", "content": "You are a helpful assistant. Always respond in exactly 2 sentences. Never respond with just 1 sentence."})
								}
								p.addUserMessage(p.currentTranscript)
								p.interruptSent = false
								p.spokenWords = nil
								ctx, cancel := context.WithCancel(context.Background())
								p.cancelLLM = cancel
								go p.runLLM(ctx)
								p.currentTranscript = ""
							}
						} else {
							p.currentTranscript += f.Text
						}
					}
				// Upstream frames from PlaybackSink (via TTS)
				case WordTimestampFrame:
					p.appendWords(f.Words)
				case TTSDoneFrame:
					p.commitSpokenText(false)
					p.isStreaming = false
				default:
					p.sessionCtx.Logger.Printf("LLM received unexpected frame: %T\n", f)
				}
			case responseFrame := <-p.responseFrames:
				switch responseFrame.(type) {
				case LLMResponseStartFrame:
					p.isStreaming = true
					ch.Send(responseFrame, Downstream)
				case LLMResponseEndFrame:
					ch.Send(responseFrame, Downstream)
				default:
					if p.isStreaming {
						ch.Send(responseFrame, Downstream)
					}
				}
			}
		}
	}
}
