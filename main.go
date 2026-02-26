package main

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"math/rand/v2"
	"net/http"
	"os"
	"os/exec"
	"strings"

	"github.com/gorilla/websocket"
)

type SonioxToken struct {
	Text    string `json:"text"`
	IsFinal bool   `json:"is_final"`
}

type SonioxResponseMessage struct {
	Tokens   []SonioxToken `json:"tokens"`
	Finished bool          `json:"finished"`
}

func main() {
	loadEnv(".env")
	cmd := exec.Command("ffmpeg",
		"-f", "avfoundation",
		"-i", ":0",
		"-ac", "1",
		"-ar", "16000",
		"-f", "s16le",
		"-flush_packets", "1",
		"-",
	)
	f, _ := os.Create("ffmpeg.log")
	cmd.Stderr = f
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		log.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		log.Fatal(err)
	}

	conn := initializeSonioxWebsocket()
	defer conn.Close()

	done := make(chan struct{})
	go readWebsocketLoop(conn, done)

	buf := make([]byte, 3200) // 100ms of audio at 16kHz, 16-bit mono
	for {
		n, err := stdout.Read(buf)
		if err != nil {
			log.Println("audio stream ended:", err)
			break
		}
		chunk := buf[:n]
		if err := conn.WriteMessage(websocket.BinaryMessage, chunk); err != nil {
			log.Println("write error:", err)
			break
		}
	}
	conn.WriteMessage(websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
	<-done
	fmt.Println("audio stream ended")
}

func callLLM(userMessage string) {
	body := map[string]interface{}{
		"model":  "gpt-4.1",
		"stream": true,
		"messages": []map[string]string{
			{"role": "system", "content": "You are a helpful voice assistant. Keep responses concise. "},
			{"role": "user", "content": userMessage},
		},
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
		}
	}
	runTTS(responseText)
	fmt.Println()
}

func runTTS(text string) {
	println("\nRunning TTS")

	conn, _, err := websocket.DefaultDialer.Dial("wss://api.cartesia.ai/tts/websocket?cartesia_version=2025-04-16&api_key="+os.Getenv("CARTESIA_API_KEY"), nil)
	if err != nil {
		log.Println("TTS websocket connect failed:", err)
		return
	}
	defer conn.Close()
	payload := map[string]interface{}{
		"model_id":      "sonic-3",
		"transcript":    text,
		"voice":         map[string]interface{}{"mode": "id", "id": "f786b574-daa5-4673-aa0c-cbe3e8534c02"},
		"output_format": map[string]interface{}{"container": "raw", "encoding": "pcm_s16le", "sample_rate": 24000},
		"context_id":    fmt.Sprintf("ctx-%d", rand.IntN(9000000)),
		"continue":      false,
	}
	if err := conn.WriteJSON(payload); err != nil {
		log.Println("failed to send TTS payload:", err)
		return
	}

	// Create a buffered writer
	player := exec.Command("ffplay", "-f", "s16le", "-ar", "24000", "-ch_layout", "mono", "-nodisp", "-autoexit", "-")
	playerIn, err := player.StdinPipe()
	if err != nil {
		log.Println("failed to create player stdin pipe:", err)
		return
	}
	if err := player.Start(); err != nil {
		log.Println("failed to start player:", err)
		return
	}
	for {
		_, msg, err := conn.ReadMessage()
		if err != nil {
			log.Println("TTS read error:", err)
			return
		}
		type responseMessage struct {
			Type      string `json:"type"`
			Data      string `json:"data"`
			ContextId string `json:"context_id"`
			Done      bool   `json:"done"`
			Error     string `json:"error"`
		}
		var resp responseMessage
		if err := json.Unmarshal(msg, &resp); err != nil {
			log.Println("TTS json unmarshal error:", err)
			continue
		}
		if resp.Type == "chunk" {
			encodedBytes, err := base64.StdEncoding.DecodeString(resp.Data)
			if err != nil {
				log.Println("base64 decode error:", err)
				continue
			}
			_, err = playerIn.Write(encodedBytes)
			if err != nil {
				log.Println("failed to write to player stdin:", err)
			}

		} else if resp.Error != "" {
			println("TTS error:", resp.Error)
		} else if resp.Done {
			break
		}
	}
	err = playerIn.Close()
	if err != nil {
		return
	} // signals EOF to ffplay — it plays remaining audio and exits
	err = player.Wait()
	if err != nil {
		return
	}
	println("TTS synthesis complete")

}

func readWebsocketLoop(conn *websocket.Conn, done chan struct{}) {
	defer close(done)
	var transcript string
	for {
		_, msg, err := conn.ReadMessage()
		if err != nil {
			log.Println("read error:", err)
			return
		}
		var resp SonioxResponseMessage
		if err := json.Unmarshal(msg, &resp); err != nil {
			log.Println("json unmarshal error:", err)
			continue
		}
		for _, token := range resp.Tokens {
			if token.IsFinal {
				if token.Text == "<end>" {
					println("[You]: " + transcript)
					go callLLM(transcript)
					transcript = ""
				} else {
					transcript += token.Text
				}

			}
		}
	}
}

func initializeSonioxWebsocket() *websocket.Conn {
	conn, _, err := websocket.DefaultDialer.Dial("wss://stt-rt.soniox.com/transcribe-websocket", nil)
	if err != nil {
		log.Fatal("websocket connect failed:", err)
	}
	config := map[string]interface{}{
		"api_key":                   os.Getenv("SONIOX_API_KEY"),
		"model":                     "stt-rt-preview",
		"audio_format":              "s16le",
		"sample_rate":               16000,
		"num_channels":              1,
		"language_hints":            []string{"en"},
		"enable_endpoint_detection": true,
	}
	if err := conn.WriteJSON(config); err != nil {
		log.Fatal("failed to send config:", err)
	}

	return conn
}

func loadEnv(path string) {
	data, err := os.ReadFile(path)
	if err != nil {
		log.Fatal("failed to read .env:", err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) == 2 {
			os.Setenv(parts[0], parts[1])
		}
	}
}
