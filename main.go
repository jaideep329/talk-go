package main

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
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

type TTSPlayer struct {
	conn     *websocket.Conn
	playerIn io.WriteCloser
	player   *exec.Cmd
}

func initializeAudioPlayer() (*TTSPlayer, error) {
	conn, _, err := websocket.DefaultDialer.Dial("wss://api.cartesia.ai/tts/websocket?cartesia_version=2025-04-16&api_key="+os.Getenv("CARTESIA_API_KEY"), nil)
	if err != nil {
		log.Println("TTS websocket connect failed:", err)
		return nil, err
	}

	// Create a buffered writer
	player := exec.Command("ffplay", "-f", "s16le", "-ar", "24000", "-ch_layout", "mono", "-nodisp", "-autoexit", "-")
	playerIn, err := player.StdinPipe()
	if err != nil {
		log.Println("failed to create player stdin pipe:", err)
		return nil, err
	}
	if err := player.Start(); err != nil {
		log.Println("failed to start player:", err)
		return nil, err
	}
	return &TTSPlayer{conn: conn, playerIn: playerIn, player: player}, nil
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
	ttsPlayer, err := initializeAudioPlayer()
	if err != nil {
		log.Fatal("failed to initialize TTS player:", err)
	}
	defer conn.Close()

	done := make(chan struct{})
	go readWebsocketLoop(conn, done, ttsPlayer)

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
	err = ttsPlayer.playerIn.Close()
	if err != nil {
		return
	}
	err = ttsPlayer.player.Wait()
	if err != nil {
		return
	}
	err = ttsPlayer.conn.Close()
	if err != nil {
		return
	}
	fmt.Println("audio stream ended")
}

func (t *TTSPlayer) callLLM(userMessage string) {
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
	channel := make(chan string)
	go t.runTTS(channel) // start TTS in parallel so it can play chunks as they arrive
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
}

func (t *TTSPlayer) runSentenceTTS(sentence string, contextId string) {
	println("\nRunning TTS for sentence:", sentence)
	payload := map[string]interface{}{
		"model_id":      "sonic-3",
		"transcript":    sentence,
		"voice":         map[string]interface{}{"mode": "id", "id": "f786b574-daa5-4673-aa0c-cbe3e8534c02"},
		"output_format": map[string]interface{}{"container": "raw", "encoding": "pcm_s16le", "sample_rate": 24000},
		"context_id":    contextId,
		"continue":      true,
	}
	if err := t.conn.WriteJSON(payload); err != nil {
		log.Println("failed to send TTS payload:", err)
		return
	}
	for {
		_, msg, err := t.conn.ReadMessage()
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
			_, err = t.playerIn.Write(encodedBytes)
			if err != nil {
				log.Println("failed to write to player stdin:", err)
			}

		} else if resp.Error != "" {
			println("TTS error:", resp.Error)
		} else if resp.Done {
			break
		}
	}
}

func (t *TTSPlayer) runTTS(channel chan string) {
	println("\nRunning TTS")

	currentSentence := ""
	contextId := fmt.Sprintf("ctx-%d", rand.IntN(9000000))
	for msg := range channel {
		currentSentence += msg
		if i := strings.LastIndexAny(currentSentence, ".?!"); i != -1 && i < len(currentSentence)-1 {
			toSpeak := currentSentence[:i+1]
			currentSentence = currentSentence[i+1:]
			t.runSentenceTTS(toSpeak, contextId)
		}
	}
	if currentSentence != "" {
		t.runSentenceTTS(currentSentence, contextId)
	}
}

func readWebsocketLoop(conn *websocket.Conn, done chan struct{}, ttsPlayer *TTSPlayer) {
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
					go ttsPlayer.callLLM(transcript)
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
