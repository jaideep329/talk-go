package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
)

type LLMProcessor struct {
	messages          []map[string]string
	currentTranscript string
	currentResponse   string
	isStreaming       bool
	interruptSent     bool
	cancelLLM         context.CancelFunc
	responseFrames    chan Frame
	turnCtx           *TurnContext
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
		log.Println("json marshal error:", err)
		return
	}

	req, err := http.NewRequestWithContext(ctx, "POST", "https://api.openai.com/v1/chat/completions", bytes.NewReader(jsonBody))
	if err != nil {
		log.Println("request error:", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+os.Getenv("OPENAI_API_KEY"))
	p.responseFrames <- LLMResponseStartFrame{}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Println("LLM request failed:", err)
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
			fmt.Print(content)
			p.responseFrames <- TextFrame{Text: content}
		}
	}
	// Only send EndFrame if we weren't cancelled
	if ctx.Err() == nil {
		p.responseFrames <- LLMResponseEndFrame{}
		fmt.Println()
	}
}

func NewLLMProcessor(turnCtx *TurnContext) *LLMProcessor {
	return &LLMProcessor{
		messages:       []map[string]string{},
		responseFrames: make(chan Frame, 100),
		turnCtx:        turnCtx,
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
					// BARGE-IN: trigger on any transcript while bot is speaking
					log.Println("Barge-in detected, cancelling LLM")
					if p.cancelLLM != nil {
						p.cancelLLM()
					}
					p.turnCtx.Cancel() // broadcast to TTS + PlaybackSink
					p.isStreaming = false
					p.interruptSent = true
					// Save partial response to history
					if p.currentResponse != "" {
						p.messages = append(p.messages, map[string]string{"role": "assistant", "content": p.currentResponse})
						p.currentResponse = ""
					}
				}
				// Always accumulate final tokens for transcript
				if f.IsFinal {
					if f.Text == "<end>" {
						if p.currentTranscript != "" {
							log.Printf("Final transcript received: %s\n", p.currentTranscript)
							p.isStreaming = false
							p.interruptSent = false
							p.turnCtx.Reset() // new context for new turn
							ctx, cancel := context.WithCancel(context.Background())
							p.cancelLLM = cancel
							go p.runLLM(ctx, p.currentTranscript)
							p.currentTranscript = ""
						}
					} else {
						p.currentTranscript += f.Text
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
				p.currentResponse = ""
				out <- responseFrame
			case LLMResponseEndFrame:
				p.messages = append(p.messages, map[string]string{"role": "assistant", "content": p.currentResponse})
				p.currentResponse = ""
				out <- responseFrame
			default:
				if p.isStreaming {
					if tf, ok := responseFrame.(TextFrame); ok {
						p.currentResponse += tf.Text
					}
					out <- responseFrame
				}
			}
		}
	}
}
