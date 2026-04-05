package main

import (
	"bufio"
	"bytes"
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
	responseFrames    chan Frame
}

func (p *LLMProcessor) runLLM(userMessage string) {
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

	req, err := http.NewRequest("POST", "https://api.openai.com/v1/chat/completions", bytes.NewReader(jsonBody))
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
	responseText := ""
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
			responseText += content
			p.responseFrames <- TextFrame{Text: content}
		}
	}
	p.responseFrames <- LLMResponseEndFrame{}
	fmt.Println()
	p.messages = append(p.messages, map[string]string{"role": "assistant", "content": responseText})
}

func NewLLMProcessor() *LLMProcessor {
	return &LLMProcessor{
		messages:          []map[string]string{},
		responseFrames:    make(chan Frame, 100),
		currentTranscript: "",
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
				if f.IsFinal {
					if f.Text == "<end>" {
						log.Printf("Final transcript received: %s\n", p.currentTranscript)
						go p.runLLM(p.currentTranscript)
						p.currentTranscript = ""
					} else {
						p.currentTranscript += f.Text
					}
				}
			case EndFrame:
				out <- f
				return
			}
		case responseFrame := <-p.responseFrames:
			out <- responseFrame
		}
	}
}
