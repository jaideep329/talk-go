package main

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"os"
	"strings"
	"sync"
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

type ttsEventType int

const (
	ttsEventAudioChunk ttsEventType = iota
	ttsEventWordTimestamps
	ttsEventDone
)

const ttsPendingEndTimeout = 8 * time.Second

type ttsEvent struct {
	eventType ttsEventType
	contextID string
	audioData []byte
	words     []pendingWord
}

type TTSProcessor struct {
	taskCtx         *TaskContext
	metrics            *ProcessorMetrics
	currentAggregation string
	currentContextId   string
	activeContextId    atomic.Value // string — safe for reader goroutine to check
	websocketConn      *websocket.Conn
	pcmBuffer          []byte
	opusEncoder        *opus.Encoder
	ttsEvents          chan ttsEvent
	pendingWords       []pendingWord
	audioTimePushed    float64 // seconds of audio pushed downstream
	firstTextReceived  bool
	firstSentenceSent  bool
	firstAudioReceived bool
	done               chan struct{}
	stopOnce           sync.Once
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

func (t *TTSProcessor) pushTTSEvent(event ttsEvent) bool {
	select {
	case <-t.done:
		return false
	case t.ttsEvents <- event:
		return true
	}
}

func (t *TTSProcessor) sendTextToTTS(text string) bool {
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
		t.taskCtx.Logger.Println("failed to send TTS payload:", err)
		return false
	}
	return true
}

func (t *TTSProcessor) ResetTTSContext() bool {
	if t.currentContextId == "" {
		return false
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
		t.taskCtx.Logger.Println("failed to reset TTS context:", err)
		return false
	}
	return true
}

func (t *TTSProcessor) CancelTTSContext() bool {
	if t.currentContextId == "" {
		return false
	}
	payload := map[string]interface{}{
		"context_id": t.currentContextId,
		"cancel":     true,
	}
	if err := t.websocketConn.WriteJSON(payload); err != nil {
		t.taskCtx.Logger.Println("failed to cancel TTS context:", err)
		return false
	}
	return true
}

func (t *TTSProcessor) isCurrentContext(contextID string) bool {
	return t.currentContextId != "" && t.currentContextId == contextID
}

func (t *TTSProcessor) handleAudioChunkData(audioData []byte, ch ProcessorChannels) {
	if t.currentContextId == "" {
		return
	}
	if !t.firstAudioReceived {
		t.firstAudioReceived = true
		if mf := t.metrics.Stop(MetricTTFB); mf != nil {
			ch.Send(*mf, Downstream)
		}
	}
	t.pcmBuffer = append(t.pcmBuffer, audioData...)

	for len(t.pcmBuffer) >= 960 {
		opusFrame := t.makeOpusData()
		if opusFrame == nil {
			break
		}
		ch.Send(AudioFrame{Data: opusFrame}, Downstream)
		t.audioTimePushed += 0.02 // 20ms per opus frame
		t.emitPendingWords(ch)
	}
}

// emitPendingWords sends WordTimestampFrames for words whose start time
// has been reached by the audio pushed so far.
func (t *TTSProcessor) emitPendingWords(ch ProcessorChannels) {
	for len(t.pendingWords) > 0 && t.pendingWords[0].start <= t.audioTimePushed {
		w := t.pendingWords[0]
		t.pendingWords = t.pendingWords[1:]
		ch.Send(WordTimestampFrame{Words: []string{w.word}}, Downstream)
	}
}

func (t *TTSProcessor) pushRemainingAudioFrames(ch ProcessorChannels) {
	if t.currentContextId == "" {
		return
	}
	if len(t.pcmBuffer) > 0 {
		for len(t.pcmBuffer) < 960 {
			t.pcmBuffer = append(t.pcmBuffer, 0, 0)
		}
		opusFrame := t.makeOpusData()
		if opusFrame != nil {
			ch.Send(AudioFrame{Data: opusFrame}, Downstream)
			t.audioTimePushed += 0.02
		}
	}
	// Flush any remaining pending words
	for _, w := range t.pendingWords {
		ch.Send(WordTimestampFrame{Words: []string{w.word}}, Downstream)
	}
	t.pendingWords = nil
}

func (t *TTSProcessor) handleTTSEvent(event ttsEvent, ch ProcessorChannels) bool {
	if !t.isCurrentContext(event.contextID) {
		if event.eventType == ttsEventDone {
			t.taskCtx.Logger.Printf("TTS ignoring stale done: context_id=%s\n", event.contextID)
		}
		return false
	}

	switch event.eventType {
	case ttsEventAudioChunk:
		t.handleAudioChunkData(event.audioData, ch)
	case ttsEventWordTimestamps:
		t.pendingWords = append(t.pendingWords, event.words...)
		t.emitPendingWords(ch)
	case ttsEventDone:
		t.taskCtx.Logger.Printf("TTS synthesis done: context_id=%s\n", event.contextID)
		t.pushRemainingAudioFrames(ch)
		ch.Send(TTSDoneFrame{}, Downstream)
		return true
	}
	return false
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
		t.taskCtx.Logger.Println("opus encode error:", err)
	}
	return opusData[:n]
}

func (t *TTSProcessor) connect() bool {
	for {
		select {
		case <-t.done:
			return false
		case <-t.taskCtx.Ctx.Done():
			return false
		default:
		}
		conn, _, err := websocket.DefaultDialer.Dial("wss://api.cartesia.ai/tts/websocket?cartesia_version=2025-04-16&api_key="+os.Getenv("CARTESIA_API_KEY"), nil)
		if err != nil {
			t.taskCtx.Logger.Printf("TTS websocket connect failed: %v, retrying in 1s...", err)
			time.Sleep(time.Second)
			continue
		}
		t.websocketConn = conn
		t.taskCtx.Logger.Println("TTS websocket connected")
		return true
	}
}

func (t *TTSProcessor) stop() {
	t.stopOnce.Do(func() {
		t.taskCtx.Logger.Println("TTSProcessor stopping websocket reader")
		close(t.done)
		t.activeContextId.Store("")
		if t.websocketConn != nil {
			t.websocketConn.Close()
		}
	})
}

func (t *TTSProcessor) readTTSConnectionData() {
	for {
		select {
		case <-t.done:
			t.taskCtx.Logger.Println("TTS reader exiting: processor ended")
			return
		default:
		}
		if t.taskCtx.Ctx.Err() != nil {
			t.taskCtx.Logger.Println("TTS reader exiting: session closed")
			t.websocketConn.Close()
			return
		}
		_, msg, err := t.websocketConn.ReadMessage()
		if err != nil {
			select {
			case <-t.done:
				t.taskCtx.Logger.Println("TTS reader exiting: processor ended")
				return
			default:
			}
			if t.taskCtx.Ctx.Err() != nil {
				t.taskCtx.Logger.Println("TTS reader exiting: session closed")
				return
			}
			t.taskCtx.Logger.Println("TTS read error, reconnecting:", err)
			if !t.connect() {
				return
			}
			continue
		}
		var resp CartesiaTTSMessage
		if err := json.Unmarshal(msg, &resp); err != nil {
			t.taskCtx.Logger.Println("TTS json unmarshal error:", err)
			continue
		}

		if resp.Error != "" {
			if !strings.Contains(resp.Error, "Invalid context ID") {
				t.taskCtx.Logger.Println("TTS error message received:", resp.Error)
			}
			continue
		}
		switch resp.Type {
		case "chunk":
			var audioMsg CartesiaTTSAudioChunkMessage
			if err := json.Unmarshal(msg, &audioMsg); err != nil {
				t.taskCtx.Logger.Println("TTS audio chunk unmarshal error:", err)
				continue
			}
			if !t.isActiveContext(audioMsg.ContextId) {
				continue
			}
			audioData, err := base64.StdEncoding.DecodeString(audioMsg.Data)
			if err != nil {
				t.taskCtx.Logger.Println("base64 decode error:", err)
				continue
			}
			if !t.pushTTSEvent(ttsEvent{
				eventType: ttsEventAudioChunk,
				contextID: audioMsg.ContextId,
				audioData: audioData,
			}) {
				return
			}
		case "timestamps":
			var tsMsg CartesiaTTSWordTimestampMessage
			if err := json.Unmarshal(msg, &tsMsg); err != nil {
				t.taskCtx.Logger.Println("TTS word timestamp unmarshal error:", err)
				continue
			}
			if !t.isActiveContext(tsMsg.ContextId) {
				continue
			}
			words := make([]pendingWord, 0, len(tsMsg.WordTimestamps.Words))
			for i, w := range tsMsg.WordTimestamps.Words {
				if i < len(tsMsg.WordTimestamps.Start) {
					words = append(words, pendingWord{word: w, start: tsMsg.WordTimestamps.Start[i]})
				}
			}
			if len(words) > 0 {
				if !t.pushTTSEvent(ttsEvent{
					eventType: ttsEventWordTimestamps,
					contextID: tsMsg.ContextId,
					words:     words,
				}) {
					return
				}
			}
		case "done":
			var doneMsg CartesiaTTSDoneMessage
			if err := json.Unmarshal(msg, &doneMsg); err != nil {
				t.taskCtx.Logger.Println("TTS done message unmarshal error:", err)
				continue
			}
			if !t.isActiveContext(doneMsg.ContextId) {
				t.taskCtx.Logger.Printf("TTS ignoring stale done: context_id=%s\n", doneMsg.ContextId)
				continue
			}
			if !t.pushTTSEvent(ttsEvent{
				eventType: ttsEventDone,
				contextID: doneMsg.ContextId,
			}) {
				return
			}
		}
	}
}

func NewTTSProcessor(taskCtx *TaskContext) *TTSProcessor {
	opusEncoder, err := opus.NewEncoder(24000, 1, opus.AppVoIP)
	if err != nil {
		taskCtx.Logger.Println("opus encoder init failed:", err)
	}
	t := &TTSProcessor{
		taskCtx:  taskCtx,
		metrics:     NewProcessorMetrics("tts"),
		opusEncoder: opusEncoder,
		ttsEvents:   make(chan ttsEvent, 100),
		done:        make(chan struct{}),
	}
	t.connect()
	return t
}

func (t *TTSProcessor) Process(ch ProcessorChannels) {
	go t.readTTSConnectionData()

	var pendingEnd *EndFrame
	awaitingTTSDone := false
	ttsFlushSent := false
	var pendingEndTimer *time.Timer
	var pendingEndTimerC <-chan time.Time

	clearTTSState := func() {
		t.activeContextId.Store("")
		t.currentAggregation = ""
		t.currentContextId = ""
		t.pcmBuffer = nil
		t.pendingWords = nil
		t.audioTimePushed = 0
		t.firstTextReceived = false
		t.firstSentenceSent = false
		t.firstAudioReceived = false
		awaitingTTSDone = false
		ttsFlushSent = false
	}

	stopPendingEndTimer := func() {
		if pendingEndTimer == nil {
			return
		}
		if !pendingEndTimer.Stop() {
			select {
			case <-pendingEndTimer.C:
			default:
			}
		}
		pendingEndTimer = nil
		pendingEndTimerC = nil
	}
	defer stopPendingEndTimer()

	startPendingEndTimer := func() {
		stopPendingEndTimer()
		pendingEndTimer = time.NewTimer(ttsPendingEndTimeout)
		pendingEndTimerC = pendingEndTimer.C
	}

	forwardPendingEnd := func(reason string) bool {
		if pendingEnd == nil {
			return false
		}
		stopPendingEndTimer()
		t.taskCtx.Logger.Printf("TTS forwarding pending EndFrame %s: reason=%q\n", reason, pendingEnd.Reason)
		ch.Send(*pendingEnd, Downstream)
		pendingEnd = nil
		t.stop()
		return true
	}

	flushForEnd := func() {
		// currentContextId remains the active turn context until the next
		// LLMResponseStartFrame/TTSSpeakFrame, so a shutdown flush can still send
		// any aggregated assistant text waiting for punctuation.
		if strings.TrimSpace(t.currentAggregation) != "" {
			if t.sendTextToTTS(t.currentAggregation) {
				awaitingTTSDone = true
				ttsFlushSent = false
			}
			t.currentAggregation = ""
		}
		if awaitingTTSDone && !ttsFlushSent {
			if t.ResetTTSContext() {
				ttsFlushSent = true
			} else {
				awaitingTTSDone = false
			}
		}
	}

	handleEnd := func(frame EndFrame, path string) bool {
		if pendingEnd != nil {
			t.taskCtx.Logger.Printf("EndFrame at TTSProcessor %s ignored because shutdown is already pending: reason=%q\n", path, frame.Reason)
			return false
		}
		flushForEnd()
		if awaitingTTSDone {
			pending := frame
			pendingEnd = &pending
			startPendingEndTimer()
			t.taskCtx.Logger.Printf("EndFrame at TTSProcessor %s deferred until TTS done: reason=%q\n", path, frame.Reason)
			return false
		}
		t.taskCtx.Logger.Printf("EndFrame at TTSProcessor %s forwarding downstream and stopping TTS: reason=%q\n", path, frame.Reason)
		ch.Send(frame, Downstream)
		t.stop()
		return true
	}

	for {
		// Priority: check system channel first.
		select {
		case frame := <-ch.System:
			switch frame.(type) {
			case InterruptFrame:
				if pendingEnd != nil {
					t.taskCtx.Logger.Println("TTS shutdown is pending, dropping interrupt")
					continue
				}
				t.CancelTTSContext()
				clearTTSState()
				drainTTSEvents(t.ttsEvents)
				t.metrics.Reset()
				t.taskCtx.Logger.Println("TTS interrupted, cleared state")
				ch.Send(frame, Downstream) // propagate to PlaybackSink
			}
		default:
			select {
			case frame := <-ch.System:
				switch frame.(type) {
				case InterruptFrame:
					if pendingEnd != nil {
						t.taskCtx.Logger.Println("TTS shutdown is pending, dropping interrupt")
						continue
					}
					t.CancelTTSContext()
					clearTTSState()
					drainTTSEvents(t.ttsEvents)
					t.metrics.Reset()
					t.taskCtx.Logger.Println("TTS interrupted, cleared state")
					ch.Send(frame, Downstream)
				}
			case frame, ok := <-ch.Data:
				if !ok {
					return
				}
				if pendingEnd != nil {
					switch f := frame.(type) {
					case WordTimestampFrame:
						ch.Send(f, Upstream)
					case TTSDoneFrame:
						ch.Send(f, Upstream)
					case BotStartedSpeakingFrame:
						ch.Send(f, Upstream)
					case BotStoppedSpeakingFrame:
						ch.Send(f, Upstream)
					case EndFrame:
						t.taskCtx.Logger.Printf("EndFrame at TTSProcessor data path ignored because shutdown is already pending: reason=%q\n", f.Reason)
					default:
						t.taskCtx.Logger.Printf("TTS shutdown pending, dropping frame: %T\n", f)
					}
					continue
				}
				switch f := frame.(type) {
				case EndFrame:
					if handleEnd(f, "data path") {
						return
					}
				case LLMResponseStartFrame:
					clearTTSState()
					drainTTSEvents(t.ttsEvents) // flush stale events from previous context
					t.metrics.Reset()
					t.currentContextId = fmt.Sprintf("ctx-%d", rand.IntN(9000000))
					t.activeContextId.Store(t.currentContextId)
					t.taskCtx.Logger.Println("LLM response started, resetting TTS aggregation")
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
						if t.sendTextToTTS(t.currentAggregation) {
							awaitingTTSDone = true
							ttsFlushSent = false
						}
						t.currentAggregation = ""
					}
				case LLMResponseEndFrame:
					if strings.TrimSpace(t.currentAggregation) != "" {
						if t.sendTextToTTS(t.currentAggregation) {
							awaitingTTSDone = true
							ttsFlushSent = false
						}
					}
					if awaitingTTSDone {
						if t.ResetTTSContext() {
							ttsFlushSent = true
						} else {
							awaitingTTSDone = false
						}
					}
					t.currentAggregation = ""
					t.pcmBuffer = nil
					t.taskCtx.Logger.Println("LLM response ended, flushing TTS context")
					ch.Send(f, Downstream)
				case TTSSpeakFrame:
					// Standalone synthesis — send full text immediately
					clearTTSState()
					drainTTSEvents(t.ttsEvents)
					t.firstSentenceSent = true // skip aggregation metrics for idle prompts
					t.metrics.Reset()
					t.currentContextId = fmt.Sprintf("ctx-%d", rand.IntN(9000000))
					t.activeContextId.Store(t.currentContextId)
					t.metrics.Start(MetricTTFB)
					awaitingTTSDone = false
					ttsFlushSent = false
					if t.sendTextToTTS(f.Text) {
						awaitingTTSDone = true
						if t.ResetTTSContext() {
							ttsFlushSent = true
						} else {
							awaitingTTSDone = false
						}
					}
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
					t.taskCtx.Logger.Printf("TTS received unexpected frame: %T\n", f)
				}
			case event := <-t.ttsEvents:
				if t.handleTTSEvent(event, ch) {
					awaitingTTSDone = false
					ttsFlushSent = false
					if forwardPendingEnd("after TTS done") {
						return
					}
				}
			case <-pendingEndTimerC:
				if pendingEnd != nil {
					t.taskCtx.Logger.Printf("Timed out waiting %s for TTS done before EndFrame: reason=%q\n", ttsPendingEndTimeout, pendingEnd.Reason)
					if forwardPendingEnd("after TTS done timeout") {
						return
					}
				}
			}
		}
	}
}

func drainTTSEvents(ch chan ttsEvent) {
	for {
		select {
		case <-ch:
		default:
			return
		}
	}
}
