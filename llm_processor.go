package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"strings"
	"time"
)

type LLMProcessor struct {
	logger            *log.Logger
	messages          []map[string]string
	currentTranscript string
	isStreaming       bool
	interruptSent     bool
	cancelLLM         context.CancelFunc
	responseFrames    chan Frame
	turnCtx           *TurnContext
	sessionCtx        *SessionContext
	turnStartTime     time.Time
	firstTokenSent    bool
}

func (p *LLMProcessor) runLLM(ctx context.Context, userMessage string) {
	if len(p.messages) == 0 {
		p.messages = append(p.messages, map[string]string{"role": "system", "content": "You are a helpful assistant. Always respond in exactly 2 sentences. Never respond with just 1 sentence."})
	}
	p.messages = append(p.messages, map[string]string{"role": "user", "content": userMessage})
	body := map[string]interface{}{
		"model":    "gpt-4.1",
		"stream":   true,
		"messages": p.messages,
	}
	jsonBody, err := json.Marshal(body)
	if err != nil {
		p.logger.Println("json marshal error:", err)
		return
	}

	req, err := http.NewRequestWithContext(ctx, "POST", "https://api.openai.com/v1/chat/completions", bytes.NewReader(jsonBody))
	if err != nil {
		p.logger.Println("request error:", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+os.Getenv("OPENAI_API_KEY"))
	p.responseFrames <- LLMResponseStartFrame{}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		p.logger.Println("LLM request failed:", err)
		return
	}
	defer resp.Body.Close()

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
			if !p.firstTokenSent {
				p.firstTokenSent = true
				llmLatency := time.Since(p.turnStartTime).Milliseconds()
				p.logger.Printf("LLM first token latency: %dms\n", llmLatency)
			}
			p.sessionCtx.UIEvents.Send(UIEvent{Type: "assistant_transcript", Role: "assistant", Text: content})
			p.responseFrames <- TextFrame{Text: content}
		}
	}
	if ctx.Err() == nil {
		p.sessionCtx.UIEvents.Send(UIEvent{Type: "assistant_done"})
		p.responseFrames <- LLMResponseEndFrame{}
	}
}

func NewLLMProcessor(logger *log.Logger, turnCtx *TurnContext, sessionCtx *SessionContext) *LLMProcessor {
	return &LLMProcessor{
		logger:         logger,
		messages:       []map[string]string{},
		responseFrames: make(chan Frame, 100),
		turnCtx:        turnCtx,
		sessionCtx:     sessionCtx,
	}
}

func (p *LLMProcessor) commitSpokenText(interrupted bool) {
	var spoken string
	if interrupted {
		spoken = p.turnCtx.SpokenSoFar()
	} else {
		spoken = p.turnCtx.FlushAll()
	}
	if spoken != "" {
		p.logger.Printf("Committing to history (interrupted=%v): %s\n", interrupted, spoken)
		p.messages = append(p.messages, map[string]string{"role": "assistant", "content": spoken})
		p.sessionCtx.UIEvents.Send(UIEvent{Type: "committed_assistant", Role: "assistant", Text: spoken})
	} else if interrupted {
		// Nothing was spoken — tell frontend to remove the assistant turn
		p.sessionCtx.UIEvents.Send(UIEvent{Type: "committed_assistant", Role: "assistant", Text: ""})
	}
}

func (p *LLMProcessor) Process(in <-chan Frame, out chan<- Frame) {
	for {
		select {
		case frame, ok := <-in:
			if !ok {
				return
			}
			switch f := frame.(type) {
			case TranscriptFrame:
				if p.isStreaming && !p.interruptSent {
					p.logger.Println("Barge-in detected, cancelling LLM")
					if p.cancelLLM != nil {
						p.cancelLLM()
					}
					p.turnCtx.Cancel()
					p.isStreaming = false
					p.interruptSent = true
					p.commitSpokenText(true)
				}
				if f.IsFinal {
					if f.Text == "<end>" {
						if p.currentTranscript != "" {
							p.logger.Printf("Final transcript received: %s\n", p.currentTranscript)
							p.sessionCtx.UIEvents.Send(UIEvent{Type: "user_transcript", Role: "user", Text: p.currentTranscript, IsFinal: true})
							p.commitSpokenText(false)
							p.isStreaming = false
							p.interruptSent = false
							p.turnCtx.Reset()
							p.turnStartTime = time.Now()
							p.firstTokenSent = false
							ctx, cancel := context.WithCancel(context.Background())
							p.cancelLLM = cancel
							go p.runLLM(ctx, p.currentTranscript)
							p.currentTranscript = ""
						}
					} else {
						p.currentTranscript += f.Text
						p.sessionCtx.UIEvents.Send(UIEvent{Type: "user_transcript", Role: "user", Text: f.Text})
					}
				}
			case EndFrame:
				out <- f
				return
			}
		case responseFrame := <-p.responseFrames:
			switch responseFrame.(type) {
			case LLMResponseStartFrame:
				p.isStreaming = true
				out <- responseFrame
			case LLMResponseEndFrame:
				out <- responseFrame
			default:
				if p.isStreaming {
					out <- responseFrame
				}
			}
		}
	}
}
