package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/rand/v2"
	"os"
	"strings"

	"github.com/gorilla/websocket"
)

var ttsConn *websocket.Conn

type CartesiaTTSResponseMessage struct {
	Type      string `json:"type"`
	Data      string `json:"data"`
	ContextId string `json:"context_id"`
	Done      bool   `json:"done"`
	Error     string `json:"error"`
}

func initializeTTSWebsocket() {
	conn, _, err := websocket.DefaultDialer.Dial("wss://api.cartesia.ai/tts/websocket?cartesia_version=2025-04-16&api_key="+os.Getenv("CARTESIA_API_KEY"), nil)
	if err != nil {
		log.Println("TTS websocket connect failed:", err)
	}
	ttsConn = conn
}

func runSentenceTTS(sentence string, contextId string, playerIn io.WriteCloser) {
	log.Println("[TTS] sending sentence:", sentence[:min(50, len(sentence))])
	payload := map[string]interface{}{
		"model_id":      "sonic-3",
		"transcript":    sentence,
		"voice":         map[string]interface{}{"mode": "id", "id": "f786b574-daa5-4673-aa0c-cbe3e8534c02"},
		"output_format": map[string]interface{}{"container": "raw", "encoding": "pcm_s16le", "sample_rate": 24000},
		"context_id":    contextId,
		"continue":      true,
	}
	if err := ttsConn.WriteJSON(payload); err != nil {
		log.Println("failed to send TTS payload:", err)
		return
	}
	for {
		_, msg, err := ttsConn.ReadMessage()
		if err != nil {
			log.Println("TTS read error:", err)
			return
		}

		var resp CartesiaTTSResponseMessage
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
			n, err := playerIn.Write(encodedBytes)
			if err != nil {
				log.Println("[TTS] write to player FAILED:", err)
			} else {
				log.Printf("[TTS] wrote %d bytes to player", n)
			}

		} else if resp.Error != "" {
			println("TTS error:", resp.Error)
		} else if resp.Done {
			break
		}
	}
}

func runTTS(channel chan string) {
	log.Println("[TTS] runTTS started")

	audioOutputStream, audioOutputProcessRef := initializeAudioOutputStream()

	currentSentence := ""
	contextId := fmt.Sprintf("ctx-%d", rand.IntN(9000000))
	for msg := range channel {
		currentSentence += msg
		if i := strings.LastIndexAny(currentSentence, ".?!"); i != -1 && i < len(currentSentence)-1 {
			toSpeak := currentSentence[:i+1]
			currentSentence = currentSentence[i+1:]
			if strings.TrimSpace(toSpeak) != "" {
				runSentenceTTS(toSpeak, contextId, audioOutputStream)
			}
		}
	}
	if strings.TrimSpace(currentSentence) != "" {
		runSentenceTTS(currentSentence, contextId, audioOutputStream)
	}

	audioOutputStream.Close()
	audioOutputProcessRef.Wait()
}
