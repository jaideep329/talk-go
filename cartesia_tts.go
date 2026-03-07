package main

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"log"
	"math/rand/v2"
	"os"
	"strings"
	"time"

	"github.com/gorilla/websocket"
	"github.com/hraban/opus"
	"github.com/pion/webrtc/v4/pkg/media"
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

func runSentenceTTS(sentence string, contextId string, encoder *opus.Encoder) {
	log.Println("[TTS] sending sentence:", sentence[:min(50, len(sentence))])
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	var pcmBuf []byte
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
			pcmBuf = append(pcmBuf, encodedBytes...)
			for len(pcmBuf) >= 960 {
				// Convert bytes to int16 samples
				frame := make([]int16, 480)
				for i := 0; i < 480; i++ {
					frame[i] = int16(binary.LittleEndian.Uint16(pcmBuf[i*2:]))
				}
				pcmBuf = pcmBuf[960:]

				// Encode to Opus
				opusData := make([]byte, 1000)
				n, err := encoder.Encode(frame, opusData)
				if err != nil {
					log.Println("opus encode error:", err)
					continue
				}

				// Write to LiveKit track
				err = botTrack.WriteSample(media.Sample{
					Data:     opusData[:n],
					Duration: 20 * time.Millisecond,
				}, nil)
				if err != nil {
					log.Println("track write error:", err)
				}
				<-ticker.C
			}

		} else if resp.Error != "" {
			log.Println("TTS error:", resp.Error)
		} else if resp.Done {
			break
		}
	}

	// Flush remaining PCM
	if len(pcmBuf) > 0 {
		for len(pcmBuf) < 960 {
			pcmBuf = append(pcmBuf, 0, 0)
		}
		frame := make([]int16, 480)
		for i := 0; i < 480; i++ {
			frame[i] = int16(binary.LittleEndian.Uint16(pcmBuf[i*2:]))
		}
		opusData := make([]byte, 1000)
		n, err := encoder.Encode(frame, opusData)
		if err == nil {
			botTrack.WriteSample(media.Sample{
				Data:     opusData[:n],
				Duration: 20 * time.Millisecond,
			}, nil)
		}
	}
}

func runTTS(channel chan string) {
	log.Println("[TTS] runTTS started")
	initializeTTSWebsocket()
	defer ttsConn.Close()
	encoder, err := opus.NewEncoder(24000, 1, opus.AppVoIP)
	if err != nil {
		log.Println("failed to create opus encoder:", err)
		return
	}

	currentSentence := ""
	contextId := fmt.Sprintf("ctx-%d", rand.IntN(9000000))
	for msg := range channel {
		currentSentence += msg
		if i := strings.LastIndexAny(currentSentence, ".?!"); i != -1 && i < len(currentSentence)-1 {
			toSpeak := currentSentence[:i+1]
			currentSentence = currentSentence[i+1:]
			if strings.TrimSpace(toSpeak) != "" {
				runSentenceTTS(toSpeak, contextId, encoder)
			}
		}
	}
	if strings.TrimSpace(currentSentence) != "" {
		runSentenceTTS(currentSentence, contextId, encoder)
	}
}
