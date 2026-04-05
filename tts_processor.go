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
	logger             *log.Logger
	currentAggregation string
	currentContextId   string
	websocketConn      *websocket.Conn
	pcmBuffer          []byte
	opusEncoder        *opus.Encoder
	audioFrames        chan Frame
	turnCtx            *TurnContext
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
	StatusCode     int    `json:"status_code"`
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
	StatusCode int    `json:"status_code"`
	Done       bool   `json:"done"`
	ContextId  string `json:"context_id"`
	Error      string `json:"error"`
}

func (t *TTSProcessor) sendTextToTTS(text string) {
	payload := map[string]interface{}{
		"model_id":       "sonic-3",
		"transcript":     text,
		"voice":          map[string]interface{}{"mode": "id", "id": "f786b574-daa5-4673-aa0c-cbe3e8534c02"},
		"output_format":  map[string]interface{}{"container": "raw", "encoding": "pcm_s16le", "sample_rate": 24000},
		"context_id":     t.currentContextId,
		"continue":       true,
		"add_timestamps": true,
	}
	if err := t.websocketConn.WriteJSON(payload); err != nil {
		t.logger.Println("failed to send TTS payload:", err)
		return
	}
}

func (t *TTSProcessor) ResetTTSContext() {
	if t.currentContextId == "" {
		return
	}
	payload := map[string]interface{}{
		"model_id":      "sonic-3",
		"transcript":    "",
		"voice":         map[string]interface{}{"mode": "id", "id": "f786b574-daa5-4673-aa0c-cbe3e8534c02"},
		"output_format": map[string]interface{}{"container": "raw", "encoding": "pcm_s16le", "sample_rate": 24000},
		"context_id":    t.currentContextId,
		"continue":      false,
	}
	if err := t.websocketConn.WriteJSON(payload); err != nil {
		t.logger.Println("failed to reset TTS context:", err)
		return
	}
}

func (t *TTSProcessor) CancelTTSContext() {
	if t.currentContextId == "" {
		return
	}
	payload := map[string]interface{}{
		"context_id": t.currentContextId,
		"cancel":     true,
	}
	if err := t.websocketConn.WriteJSON(payload); err != nil {
		t.logger.Println("failed to cancel TTS context:", err)
	}
}

func (t *TTSProcessor) handleAudioChunkMessage(audioMsg *CartesiaTTSAudioChunkMessage) {
	if t.currentContextId == "" {
		return
	}
	encodedBytes, err := base64.StdEncoding.DecodeString(audioMsg.Data)
	if err != nil {
		t.logger.Println("base64 decode error:", err)
		return
	}
	t.pcmBuffer = append(t.pcmBuffer, encodedBytes...)

	for len(t.pcmBuffer) >= 960 {
		opusFrame := t.makeOpusData()
		if opusFrame == nil {
			break
		}
		t.audioFrames <- AudioFrame{Data: opusFrame}
	}
}

func (t *TTSProcessor) pushRemainingAudioFrames() {
	if t.currentContextId == "" {
		return
	}
	if len(t.pcmBuffer) > 0 {
		for len(t.pcmBuffer) < 960 {
			t.pcmBuffer = append(t.pcmBuffer, 0, 0)
		}
		opusFrame := t.makeOpusData()
		if opusFrame != nil {
			t.audioFrames <- AudioFrame{Data: opusFrame}
		}
	}
}

func (t *TTSProcessor) makeOpusData() []byte {
	if len(t.pcmBuffer) < 960 {
		return nil
	}
	frame := make([]int16, 480)
	for i := 0; i < 480; i++ {
		frame[i] = int16(binary.LittleEndian.Uint16(t.pcmBuffer[i*2:]))
	}
	t.pcmBuffer = t.pcmBuffer[960:]
	opusData := make([]byte, 1000)
	n, err := t.opusEncoder.Encode(frame, opusData)
	if err != nil {
		t.logger.Println("opus encode error:", err)
	}
	return opusData[:n]
}

func (t *TTSProcessor) connect() {
	for {
		conn, _, err := websocket.DefaultDialer.Dial("wss://api.cartesia.ai/tts/websocket?cartesia_version=2025-04-16&api_key="+os.Getenv("CARTESIA_API_KEY"), nil)
		if err != nil {
			t.logger.Printf("TTS websocket connect failed: %v, retrying in 1s...", err)
			time.Sleep(time.Second)
			continue
		}
		t.websocketConn = conn
		t.logger.Println("TTS websocket connected")
		return
	}
}

func (t *TTSProcessor) readTTSConnectionData() {
	for {
		_, msg, err := t.websocketConn.ReadMessage()
		if err != nil {
			t.logger.Println("TTS read error, reconnecting:", err)
			t.connect()
			continue
		}
		var resp CartesiaTTSMessage
		if err := json.Unmarshal(msg, &resp); err != nil {
			t.logger.Println("TTS json unmarshal error:", err)
			continue
		}

		if resp.Error != "" {
			if !strings.Contains(resp.Error, "Invalid context ID") {
				t.logger.Println("TTS error message received:", resp.Error)
			}
			continue
		}
		switch resp.Type {
		case "chunk":
			var audioMsg CartesiaTTSAudioChunkMessage
			if err := json.Unmarshal(msg, &audioMsg); err != nil {
				t.logger.Println("TTS audio chunk unmarshal error:", err)
				continue
			}
			t.handleAudioChunkMessage(&audioMsg)
		case "timestamps":
			var tsMsg CartesiaTTSWordTimestampMessage
			if err := json.Unmarshal(msg, &tsMsg); err != nil {
				t.logger.Println("TTS word timestamp unmarshal error:", err)
				continue
			}
			if t.currentContextId != "" && len(tsMsg.WordTimestamps.Words) > 0 {
				t.audioFrames <- WordTimestampFrame{
					Words: tsMsg.WordTimestamps.Words,
					Start: tsMsg.WordTimestamps.Start,
				}
			}
		case "done":
			var doneMsg CartesiaTTSDoneMessage
			if err := json.Unmarshal(msg, &doneMsg); err != nil {
				t.logger.Println("TTS done message unmarshal error:", err)
				continue
			}
			t.logger.Printf("TTS synthesis done: context_id=%s\n", doneMsg.ContextId)
			t.pushRemainingAudioFrames()
		}
	}
}

func NewTTSProcessor(logger *log.Logger, turnCtx *TurnContext) *TTSProcessor {
	opusEncoder, err := opus.NewEncoder(24000, 1, opus.AppVoIP)
	if err != nil {
		logger.Println("opus encoder init failed:", err)
	}
	t := &TTSProcessor{
		logger:      logger,
		opusEncoder: opusEncoder,
		audioFrames: make(chan Frame, 100),
		turnCtx:     turnCtx,
	}
	t.connect()
	return t
}

func (t *TTSProcessor) Process(in <-chan Frame, out chan<- Frame) {
	go t.readTTSConnectionData()
	done := t.turnCtx.Done()
	for {
		select {
		case <-done:
			t.CancelTTSContext()
			drainChannel(t.audioFrames)
			t.currentAggregation = ""
			t.currentContextId = ""
			t.pcmBuffer = nil
			done = nil
			t.logger.Println("TTS interrupted, cleared state")
		case frame, ok := <-in:
			if !ok {
				return
			}
			switch f := frame.(type) {
			case EndFrame:
				out <- f
				return
			case LLMResponseStartFrame:
				t.currentAggregation = ""
				t.pcmBuffer = nil
				t.currentContextId = fmt.Sprintf("ctx-%d", rand.IntN(9000000))
				done = t.turnCtx.Done()
				t.logger.Println("LLM response started, resetting TTS aggregation")
				out <- f
			case TextFrame:
				t.currentAggregation += f.Text
				if endsWithPunctuation(t.currentAggregation) {
					t.sendTextToTTS(t.currentAggregation)
					t.currentAggregation = ""
				}
			case LLMResponseEndFrame:
				if strings.TrimSpace(t.currentAggregation) != "" {
					t.sendTextToTTS(t.currentAggregation)
				}
				t.ResetTTSContext()
				t.currentAggregation = ""
				t.pcmBuffer = nil
				t.logger.Println("LLM response ended, flushing TTS context")
				out <- f
			default:
				t.logger.Printf("TTS received unexpected frame: %T\n", frame)
			}
		case frame, ok := <-t.audioFrames:
			if !ok {
				return
			}
			out <- frame
		}
	}
}

func drainChannel(ch chan Frame) {
	for {
		select {
		case <-ch:
		default:
			return
		}
	}
}
