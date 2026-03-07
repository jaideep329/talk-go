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

var messages []map[string]string

func callLLM(userMessage string) {
	if len(messages) == 0 {
		messages = append(messages, map[string]string{"role": "system", "content": "You are a helpful assistant. Always respond in exactly 2 sentences. Never respond with just 1 sentence."})
	}
	messages = append(messages, map[string]string{"role": "user", "content": userMessage})
	body := map[string]interface{}{
		"model":    "gpt-4.1",
		"stream":   true,
		"messages": messages,
	}
	jsonBody, err := json.Marshal(body)
	if err != nil {
		log.Println("json marshal error:", err)
		return
	}
	channel := make(chan string, 100)
	go runTTS(channel) // start TTS in parallel so it can play chunks as they arrive
	req, err := http.NewRequest("POST", "https://api.openai.com/v1/chat/completions", bytes.NewReader(jsonBody))
	if err != nil {
		log.Println("request error:", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+os.Getenv("OPENAI_API_KEY"))

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
			channel <- content
		}
	}
	close(channel) // signal TTS that response is complete
	fmt.Println()
	messages = append(messages, map[string]string{"role": "assistant", "content": responseText})
}
