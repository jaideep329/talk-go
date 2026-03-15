package main

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"log"
	"math/rand/v2"
	"os"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/gorilla/websocket"
	"github.com/hraban/opus"
)

func endsWithPunctuation(s string) bool {
	if s == "" {
		return false
	}
	lastRune, _ := utf8.DecodeLastRuneInString(s)
	return unicode.IsPunct(lastRune)
}

type TTSProcessor struct {
	currentAggregation string
	currentContextId   string
	websocketConn      *websocket.Conn
	pcmBuffer          []byte
	opusEncoder        *opus.Encoder
	audioFrames        chan Frame
}

type CartesiaTTSMessage struct {
	Type  string `json:"type"`
	Error string `json:"error"`
}

type CartesiaTTSAudioChunkMessage struct {
	Type      string `json:"type"`
	Data      string `json:"data"`
	ContextId string `json:"context_id"`
	Done      bool   `json:"done"`
	Error     string `json:"error"`
}

type CartesiaTTSWordTimestampMessage struct {
	Type           string `json:"type"`
	StatusCode     string `json:"status_code"`
	ContextId      string `json:"context_id"`
	Done           bool   `json:"done"`
	Error          string `json:"error"`
	WordTimestamps struct {
		Words []string  `json:"words"`
		Start []float64 `json:"start"`
		End   []float64 `json:"end"`
	} `json:"word_timestamps"`
}

type CartesiaTTSDoneMessage struct {
	Type       string `json:"type"`
	StatusCode string `json:"status_code"`
	Done       bool   `json:"done"`
	ContextId  string `json:"context_id"`
	Error      string `json:"error"`
}

func (t *TTSProcessor) sendTextToTTS(text string) {
	payload := map[string]interface{}{
		"model_id":      "sonic-3",
		"transcript":    text,
		"voice":         map[string]interface{}{"mode": "id", "id": "f786b574-daa5-4673-aa0c-cbe3e8534c02"},
		"output_format": map[string]interface{}{"container": "raw", "encoding": "pcm_s16le", "sample_rate": 24000},
		"context_id":    t.currentContextId,
		"continue":      true,
	}
	if err := t.websocketConn.WriteJSON(payload); err != nil {
		log.Println("failed to send TTS payload:", err)
		return
	}
}

func (t *TTSProcessor) ResetTTSContext() {
	payload := map[string]interface{}{
		"model_id":      "sonic-3",
		"transcript":    "",
		"voice":         map[string]interface{}{"mode": "id", "id": "f786b574-daa5-4673-aa0c-cbe3e8534c02"},
		"output_format": map[string]interface{}{"container": "raw", "encoding": "pcm_s16le", "sample_rate": 24000},
		"context_id":    t.currentContextId,
		"continue":      false,
	}
	if err := t.websocketConn.WriteJSON(payload); err != nil {
		log.Println("failed to send TTS payload:", err)
		return
	}
}

func (t *TTSProcessor) handleAudioChunkMessage(audioMsg *CartesiaTTSAudioChunkMessage) {
	encodedBytes, err := base64.StdEncoding.DecodeString(audioMsg.Data)
	if err != nil {
		log.Println("base64 decode error:", err)
		return
	}
	t.pcmBuffer = append(t.pcmBuffer, encodedBytes...)

	for len(t.pcmBuffer) >= 960 {
		opusFrame := t.makeOpusData()
		t.audioFrames <- AudioFrame{Data: opusFrame}
	}

}

func (t *TTSProcessor) pushRemainingAudioFrames() {

	if len(t.pcmBuffer) > 0 {
		for len(t.pcmBuffer) < 960 {
			t.pcmBuffer = append(t.pcmBuffer, 0, 0)
		}
		opusFrame := t.makeOpusData()
		t.audioFrames <- AudioFrame{Data: opusFrame}

	}

}

func (t *TTSProcessor) makeOpusData() []byte {
	frame := make([]int16, 480)
	for i := 0; i < 480; i++ {
		frame[i] = int16(binary.LittleEndian.Uint16(t.pcmBuffer[i*2:]))
	}
	t.pcmBuffer = t.pcmBuffer[960:]
	opusData := make([]byte, 1000)
	n, err := t.opusEncoder.Encode(frame, opusData)
	if err != nil {
		log.Println("opus encode error:", err)
	}
	return opusData[:n]
}

func (t *TTSProcessor) readTTSConnectionData() {
	for {
		_, msg, err := t.websocketConn.ReadMessage()
		if err != nil {
			log.Println("TTS read error:", err)
			return
		}
		var resp CartesiaTTSMessage
		if err := json.Unmarshal(msg, &resp); err != nil {
			log.Println("TTS json unmarshal error:", err)
			continue
		}

		if resp.Error != "" {
			log.Println("TTS error message received:", resp.Error)
			continue
		} else {
			switch resp.Type {
			case "chunk":
				var audioMsg CartesiaTTSAudioChunkMessage
				if err := json.Unmarshal(msg, &audioMsg); err != nil {
					log.Println("TTS audio chunk unmarshal error:", err)
					continue
				}
				log.Printf("Received audio chunk: context_id=%s, done=%v\n", audioMsg.ContextId, audioMsg.Done)
				t.handleAudioChunkMessage(&audioMsg)
			case "word_timestamps":
				var tsMsg CartesiaTTSWordTimestampMessage
				if err := json.Unmarshal(msg, &tsMsg); err != nil {
					log.Println("TTS word timestamp unmarshal error:", err)
					continue
				}
				log.Printf("Received word timestamps: context_id=%s, words=%v\n", tsMsg.ContextId, tsMsg.WordTimestamps.Words)
			case "done":
				var doneMsg CartesiaTTSDoneMessage
				if err := json.Unmarshal(msg, &doneMsg); err != nil {
					log.Println("TTS done message unmarshal error:", err)
					continue
				}
				log.Printf("TTS synthesis done: context_id=%s\n", doneMsg.ContextId)
				t.pushRemainingAudioFrames()
			}
		}

	}
}

func NewTTSProcessor() *TTSProcessor {
	conn, _, err := websocket.DefaultDialer.Dial("wss://api.cartesia.ai/tts/websocket?cartesia_version=2025-04-16&api_key="+os.Getenv("CARTESIA_API_KEY"), nil)
	if err != nil {
		log.Println("TTS websocket connect failed:", err)
	}
	opusEncoder, err := opus.NewEncoder(24000, 1, opus.AppVoIP)
	if err != nil {
		log.Println("opus encoder int failed:", err)
	}
	audioFrames := make(chan Frame, 100)
	return &TTSProcessor{currentAggregation: "", websocketConn: conn, opusEncoder: opusEncoder, audioFrames: audioFrames}
}

func (t *TTSProcessor) Process(ctx context.Context, in <-chan Frame, out chan<- Frame) {
	go t.readTTSConnectionData()
	for {
		select {
		case <-ctx.Done():
			return
		case frame, ok := <-in:
			if !ok {
				return
			}
			switch f := frame.(type) {
			case LLMResponseStartFrame:
				t.currentAggregation = ""
				t.pcmBuffer = nil
				t.currentContextId = fmt.Sprintf("ctx-%d", rand.IntN(9000000))
				log.Println("LLM response started, resetting TTS aggregation")
				out <- f
			case TextFrame:
				t.currentAggregation += f.Text
				if endsWithPunctuation(t.currentAggregation) {
					t.sendTextToTTS(t.currentAggregation)
					t.currentAggregation = ""
				}
				log.Printf("Received text frame: %s\n", f.Text)
			case LLMResponseEndFrame:

				if strings.TrimSpace(t.currentAggregation) != "" {
					t.sendTextToTTS(t.currentAggregation)
				}
				t.ResetTTSContext()
				t.currentAggregation = ""
				t.currentContextId = ""
				t.pcmBuffer = nil
				log.Println("LLM response ended, clearing TTS aggregation and context")
				out <- f
			default:
				log.Printf("Received non-text frame of type %T\n", frame)
			}
		case frame, ok := <-t.audioFrames:
			if !ok {
				return
			}
			out <- frame
		}
	}
}
