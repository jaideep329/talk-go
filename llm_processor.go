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
	sessionCtx     *SessionContext
	metrics        *ProcessorMetrics
	isStreaming    bool
	cancelLLM      context.CancelFunc
	responseFrames chan Frame
}

func NewLLMProcessor(sessionCtx *SessionContext) *LLMProcessor {
	return &LLMProcessor{
		sessionCtx:     sessionCtx,
		metrics:        NewProcessorMetrics("llm"),
		responseFrames: make(chan Frame, 100),
	}
}

func (p *LLMProcessor) runLLM(ctx context.Context, messages []map[string]string) {
	body := map[string]interface{}{
		"model":    "gpt-4.1",
		"stream":   true,
		"messages": messages,
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

func (p *LLMProcessor) Process(ch ProcessorChannels) {
	for {
		select {
		case frame := <-ch.System:
			switch frame.(type) {
			case InterruptFrame:
				if p.cancelLLM != nil {
					p.cancelLLM()
				}
				p.metrics.Reset()
				p.isStreaming = false
				ch.Send(frame, Downstream) // propagate to TTS/PlaybackSink
			case EndFrame:
				ch.Send(frame, Downstream)
				return
			}
		default:
			select {
			case frame := <-ch.System:
				switch frame.(type) {
				case InterruptFrame:
					if p.cancelLLM != nil {
						p.cancelLLM()
					}
					p.metrics.Reset()
					p.isStreaming = false
					ch.Send(frame, Downstream)
				case EndFrame:
					ch.Send(frame, Downstream)
					return
				}
			case frame, ok := <-ch.Data:
				if !ok {
					return
				}
				switch f := frame.(type) {
				case LLMMessagesFrame:
					ctx, cancel := context.WithCancel(context.Background())
					p.cancelLLM = cancel
					go p.runLLM(ctx, f.Messages)
				// Pass-through downstream
				case TTSSpeakFrame:
					ch.Send(f, Downstream)
				// Upstream frames — forward to ContextAggregator
				case WordTimestampFrame:
					ch.Send(f, Upstream)
				case TTSDoneFrame:
					p.isStreaming = false
					ch.Send(f, Upstream)
				case BotStartedSpeakingFrame:
					ch.Send(f, Upstream)
				case BotStoppedSpeakingFrame:
					ch.Send(f, Upstream)
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
