package main

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"os"
	"strings"
	"sync/atomic"
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

type pendingWord struct {
	word  string
	start float64 // seconds from context start
}

type TTSProcessor struct {
	sessionCtx         *SessionContext
	metrics            *ProcessorMetrics
	currentAggregation string
	currentContextId   string
	activeContextId    atomic.Value // string — safe for reader goroutine to check
	websocketConn      *websocket.Conn
	pcmBuffer          []byte
	opusEncoder        *opus.Encoder
	audioFrames        chan Frame
	pendingWords       []pendingWord
	audioTimePushed    float64 // seconds of audio pushed to audioFrames
	firstTextReceived  bool
	firstSentenceSent  bool
	firstAudioReceived bool
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

func (t *TTSProcessor) isActiveContext(contextId string) bool {
	active, _ := t.activeContextId.Load().(string)
	return active != "" && active == contextId
}

func (t *TTSProcessor) sendTextToTTS(text string) {
	payload := map[string]interface{}{
		"model_id":       "sonic-3",
		"transcript":     text,
		"voice":          map[string]interface{}{"mode": "id", "id": "95d51f79-c397-46f9-b49a-23763d3eaa2d"},
		"output_format":  map[string]interface{}{"container": "raw", "encoding": "pcm_s16le", "sample_rate": 24000},
		"context_id":     t.currentContextId,
		"continue":       true,
		"add_timestamps": true,
	}
	if err := t.websocketConn.WriteJSON(payload); err != nil {
		t.sessionCtx.Logger.Println("failed to send TTS payload:", err)
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
		"voice":         map[string]interface{}{"mode": "id", "id": "95d51f79-c397-46f9-b49a-23763d3eaa2d"},
		"output_format": map[string]interface{}{"container": "raw", "encoding": "pcm_s16le", "sample_rate": 24000},
		"context_id":    t.currentContextId,
		"continue":      false,
	}
	if err := t.websocketConn.WriteJSON(payload); err != nil {
		t.sessionCtx.Logger.Println("failed to reset TTS context:", err)
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
		t.sessionCtx.Logger.Println("failed to cancel TTS context:", err)
	}
}

func (t *TTSProcessor) handleAudioChunkMessage(audioMsg *CartesiaTTSAudioChunkMessage) {
	if t.currentContextId == "" {
		return
	}
	if !t.firstAudioReceived {
		t.firstAudioReceived = true
		if mf := t.metrics.Stop(MetricTTFB); mf != nil {
			t.audioFrames <- *mf
		}
	}
	encodedBytes, err := base64.StdEncoding.DecodeString(audioMsg.Data)
	if err != nil {
		t.sessionCtx.Logger.Println("base64 decode error:", err)
		return
	}
	t.pcmBuffer = append(t.pcmBuffer, encodedBytes...)

	for len(t.pcmBuffer) >= 960 {
		opusFrame := t.makeOpusData()
		if opusFrame == nil {
			break
		}
		t.audioFrames <- AudioFrame{Data: opusFrame}
		t.audioTimePushed += 0.02 // 20ms per opus frame
		t.emitPendingWords()
	}
}

// emitPendingWords sends WordTimestampFrames for words whose start time
// has been reached by the audio pushed so far.
func (t *TTSProcessor) emitPendingWords() {
	for len(t.pendingWords) > 0 && t.pendingWords[0].start <= t.audioTimePushed {
		w := t.pendingWords[0]
		t.pendingWords = t.pendingWords[1:]
		t.audioFrames <- WordTimestampFrame{Words: []string{w.word}}
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
			t.audioTimePushed += 0.02
		}
	}
	// Flush any remaining pending words
	for _, w := range t.pendingWords {
		t.audioFrames <- WordTimestampFrame{Words: []string{w.word}}
	}
	t.pendingWords = nil
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
		t.sessionCtx.Logger.Println("opus encode error:", err)
	}
	return opusData[:n]
}

func (t *TTSProcessor) connect() {
	for {
		conn, _, err := websocket.DefaultDialer.Dial("wss://api.cartesia.ai/tts/websocket?cartesia_version=2025-04-16&api_key="+os.Getenv("CARTESIA_API_KEY"), nil)
		if err != nil {
			t.sessionCtx.Logger.Printf("TTS websocket connect failed: %v, retrying in 1s...", err)
			time.Sleep(time.Second)
			continue
		}
		t.websocketConn = conn
		t.sessionCtx.Logger.Println("TTS websocket connected")
		return
	}
}

func (t *TTSProcessor) readTTSConnectionData() {
	for {
		if t.sessionCtx.Ctx.Err() != nil {
			t.sessionCtx.Logger.Println("TTS reader exiting: session closed")
			t.websocketConn.Close()
			return
		}
		_, msg, err := t.websocketConn.ReadMessage()
		if err != nil {
			if t.sessionCtx.Ctx.Err() != nil {
				t.sessionCtx.Logger.Println("TTS reader exiting: session closed")
				return
			}
			t.sessionCtx.Logger.Println("TTS read error, reconnecting:", err)
			t.connect()
			continue
		}
		var resp CartesiaTTSMessage
		if err := json.Unmarshal(msg, &resp); err != nil {
			t.sessionCtx.Logger.Println("TTS json unmarshal error:", err)
			continue
		}

		if resp.Error != "" {
			if !strings.Contains(resp.Error, "Invalid context ID") {
				t.sessionCtx.Logger.Println("TTS error message received:", resp.Error)
			}
			continue
		}
		switch resp.Type {
		case "chunk":
			var audioMsg CartesiaTTSAudioChunkMessage
			if err := json.Unmarshal(msg, &audioMsg); err != nil {
				t.sessionCtx.Logger.Println("TTS audio chunk unmarshal error:", err)
				continue
			}
			if !t.isActiveContext(audioMsg.ContextId) {
				continue
			}
			t.handleAudioChunkMessage(&audioMsg)
		case "timestamps":
			var tsMsg CartesiaTTSWordTimestampMessage
			if err := json.Unmarshal(msg, &tsMsg); err != nil {
				t.sessionCtx.Logger.Println("TTS word timestamp unmarshal error:", err)
				continue
			}
			if !t.isActiveContext(tsMsg.ContextId) {
				continue
			}
			for i, w := range tsMsg.WordTimestamps.Words {
				if i < len(tsMsg.WordTimestamps.Start) {
					t.pendingWords = append(t.pendingWords, pendingWord{word: w, start: tsMsg.WordTimestamps.Start[i]})
				}
			}
		case "done":
			var doneMsg CartesiaTTSDoneMessage
			if err := json.Unmarshal(msg, &doneMsg); err != nil {
				t.sessionCtx.Logger.Println("TTS done message unmarshal error:", err)
				continue
			}
			if !t.isActiveContext(doneMsg.ContextId) {
				t.sessionCtx.Logger.Printf("TTS ignoring stale done: context_id=%s\n", doneMsg.ContextId)
				continue
			}
			t.sessionCtx.Logger.Printf("TTS synthesis done: context_id=%s\n", doneMsg.ContextId)
			t.pushRemainingAudioFrames()
			t.audioFrames <- TTSDoneFrame{}
		}
	}
}

func NewTTSProcessor(sessionCtx *SessionContext) *TTSProcessor {
	opusEncoder, err := opus.NewEncoder(24000, 1, opus.AppVoIP)
	if err != nil {
		sessionCtx.Logger.Println("opus encoder init failed:", err)
	}
	t := &TTSProcessor{
		sessionCtx:  sessionCtx,
		metrics:     NewProcessorMetrics("tts"),
		opusEncoder: opusEncoder,
		audioFrames: make(chan Frame, 100),
	}
	t.connect()
	return t
}

func (t *TTSProcessor) Process(ch ProcessorChannels) {
	go t.readTTSConnectionData()
	for {
		// Priority: check system channel first.
		select {
		case frame := <-ch.System:
			switch frame.(type) {
			case InterruptFrame:
				t.CancelTTSContext()
				drainChannel(t.audioFrames)
				t.currentAggregation = ""
				t.currentContextId = ""
				t.activeContextId.Store("")
				t.pcmBuffer = nil
				t.pendingWords = nil
				t.audioTimePushed = 0
				t.firstTextReceived = false
				t.firstSentenceSent = false
				t.firstAudioReceived = false
				t.metrics.Reset()
				t.sessionCtx.Logger.Println("TTS interrupted, cleared state")
				ch.Send(frame, Downstream) // propagate to PlaybackSink
			case EndFrame:
				ch.Send(frame, Downstream)
				return
			}
		default:
			select {
			case frame := <-ch.System:
				switch frame.(type) {
				case InterruptFrame:
					t.CancelTTSContext()
					drainChannel(t.audioFrames)
					t.currentAggregation = ""
					t.currentContextId = ""
					t.pcmBuffer = nil
					t.pendingWords = nil
					t.audioTimePushed = 0
					t.firstTextReceived = false
					t.firstSentenceSent = false
					t.firstAudioReceived = false
					t.metrics.Reset()
					t.sessionCtx.Logger.Println("TTS interrupted, cleared state")
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
				case LLMResponseStartFrame:
					drainChannel(t.audioFrames) // flush stale frames from previous context
					t.currentAggregation = ""
					t.pcmBuffer = nil
					t.pendingWords = nil
					t.audioTimePushed = 0
					t.firstTextReceived = false
					t.firstSentenceSent = false
					t.firstAudioReceived = false
					t.metrics.Reset()
					t.currentContextId = fmt.Sprintf("ctx-%d", rand.IntN(9000000))
					t.activeContextId.Store(t.currentContextId)
					t.sessionCtx.Logger.Println("LLM response started, resetting TTS aggregation")
					ch.Send(f, Downstream)
				case TextFrame:
					if !t.firstTextReceived {
						t.firstTextReceived = true
						t.metrics.Start(MetricTextAggregation)
					}
					t.currentAggregation += f.Text
					if endsWithPunctuation(t.currentAggregation) {
						if !t.firstSentenceSent {
							t.firstSentenceSent = true
							if mf := t.metrics.Stop(MetricTextAggregation); mf != nil {
								ch.Send(*mf, Downstream)
							}
							t.metrics.Start(MetricTTFB) // TTS TTFB: text sent → first audio
						}
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
					t.sessionCtx.Logger.Println("LLM response ended, flushing TTS context")
					ch.Send(f, Downstream)
				case TTSSpeakFrame:
					// Standalone synthesis — send full text immediately
					t.currentAggregation = ""
					t.pcmBuffer = nil
					t.pendingWords = nil
					t.audioTimePushed = 0
					t.firstTextReceived = false
					t.firstSentenceSent = true // skip aggregation metrics for idle prompts
					t.firstAudioReceived = false
					t.metrics.Reset()
					t.currentContextId = fmt.Sprintf("ctx-%d", rand.IntN(9000000))
					t.activeContextId.Store(t.currentContextId)
					t.metrics.Start(MetricTTFB)
					t.sendTextToTTS(f.Text)
					t.ResetTTSContext()
					ch.Send(f, Downstream) // tell PlaybackSink to reset interrupted state
				// Upstream frames from PlaybackSink — forward to LLM/UserIdleProcessor
				case WordTimestampFrame:
					ch.Send(f, Upstream)
				case TTSDoneFrame:
					ch.Send(f, Upstream)
				case BotStartedSpeakingFrame:
					ch.Send(f, Upstream)
				case BotStoppedSpeakingFrame:
					ch.Send(f, Upstream)
				default:
					t.sessionCtx.Logger.Printf("TTS received unexpected frame: %T\n", f)
				}
			case frame, ok := <-t.audioFrames:
				if !ok {
					return
				}
				ch.Send(frame, Downstream) // audio/word/done frames to PlaybackSink
			}
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
