package main

import (
	"context"
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

// ttsCommand is the relay envelope used by ProcessFrame to hand frames
// off to the orchestrator goroutine. done (if non-nil) is closed by the
// orchestrator after the frame has been fully processed and (for
// EndFrame) forwarded downstream.
type ttsCommand struct {
	frame Frame
	dir   Direction
	done  chan struct{}
}

// TTSProcessor wraps Cartesia. After migration its shape is:
//
//   - readTTSConnectionData goroutine: reads from the websocket, parses
//     messages, and pushes typed events to ttsEvents.
//   - orchestrator goroutine: owns ALL TTS state (aggregation, synthesis,
//     shutdown). It drives Cartesia (send text, reset, cancel) and
//     emits downstream frames (AudioFrame, WordTimestampFrame,
//     TTSDoneFrame, deferred EndFrame).
//   - ProcessFrame: thin relay. For EndFrame it blocks until the
//     orchestrator confirms the EndFrame has been forwarded; for
//     InterruptFrame it forwards downstream immediately and signals the
//     orchestrator to clean up; for other downstream frames it queues a
//     command; for upstream pass-through frames it forwards directly.
//
// State ownership: only the orchestrator mutates state, so there is no
// concurrent access to currentAggregation, pcmBuffer, etc.
type TTSProcessor struct {
	*BaseProcessor
	taskCtx *TaskContext
	metrics *ProcessorMetrics

	// Aggregation state (orchestrator only)
	currentAggregation string
	currentContextId   string
	firstTextReceived  bool
	firstSentenceSent  bool

	// Synthesis state (orchestrator only — modified inside handleTTSEvent
	// which is called only from orchestrator goroutine)
	pcmBuffer          []byte
	pendingWords       []pendingWord
	audioTimePushed    float64
	firstAudioReceived bool

	// Shared (atomic) — read by reader goroutine, written by orchestrator.
	activeContextId atomic.Value // string

	// Channels
	commands  chan ttsCommand
	ttsEvents chan ttsEvent
	done      chan struct{}

	websocketConn *websocket.Conn
	opusEncoder   *opus.Encoder
	stopOnce      sync.Once
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

func NewTTSProcessor(taskCtx *TaskContext) *TTSProcessor {
	opusEncoder, err := opus.NewEncoder(24000, 1, opus.AppVoIP)
	if err != nil {
		taskCtx.Logger.Println("opus encoder init failed:", err)
	}
	t := &TTSProcessor{
		taskCtx:     taskCtx,
		metrics:     NewProcessorMetrics("tts"),
		opusEncoder: opusEncoder,
		commands:    make(chan ttsCommand, 100),
		ttsEvents:   make(chan ttsEvent, 100),
		done:        make(chan struct{}),
	}
	t.BaseProcessor = NewBaseProcessor("TTS", t, taskCtx)
	t.connect()
	return t
}

func (t *TTSProcessor) Start(ctx context.Context) {
	t.BaseProcessor.Start(ctx)
	t.Go(t.readTTSConnectionData)
	t.Go(t.orchestrator)
}

// ProcessFrame relays frames to the orchestrator. EndFrame blocks until
// the orchestrator has fully processed it (waited for Cartesia done +
// forwarded the EndFrame downstream); InterruptFrame is forwarded
// downstream immediately, then the orchestrator is signalled to clean
// up; other frames are queued non-blocking.
func (t *TTSProcessor) ProcessFrame(ctx context.Context, frame Frame, dir Direction) {
	switch frame.(type) {
	case EndFrame:
		done := make(chan struct{})
		select {
		case t.commands <- ttsCommand{frame: frame, dir: dir, done: done}:
		case <-t.ctx.Done():
			return
		}
		select {
		case <-done:
		case <-ctx.Done():
		}
	case InterruptFrame:
		// Forward downstream right away so PlaybackSink/playback goroutine
		// can stop in parallel with orchestrator cleanup.
		t.PushFrame(frame, dir)
		select {
		case t.commands <- ttsCommand{frame: frame, dir: dir}:
		case <-t.ctx.Done():
		}
	case TTSDoneFrame, BotStartedSpeakingFrame, BotStoppedSpeakingFrame, WordTimestampFrame:
		// Upstream pass-through; orchestrator doesn't react to these.
		t.PushFrame(frame, dir)
	default:
		// All other downstream frames (LLMResponseStart/End, TextFrame,
		// TTSSpeakFrame, LLMMessagesFrame, etc.) go through the orchestrator.
		select {
		case t.commands <- ttsCommand{frame: frame, dir: dir}:
		case <-t.ctx.Done():
		}
	}
}

// orchestrator owns all TTS state and drives Cartesia. It mirrors the
// original Process loop's switch structure, but reads commands from a
// dedicated channel (relayed by ProcessFrame) instead of from
// ProcessorChannels.
func (t *TTSProcessor) orchestrator() {
	var pendingEnd *EndFrame
	var pendingEndDir Direction
	var pendingEndDone chan struct{}
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
		t.PushFrame(*pendingEnd, pendingEndDir)
		if pendingEndDone != nil {
			close(pendingEndDone)
			pendingEndDone = nil
		}
		pendingEnd = nil
		t.stop()
		return true
	}

	flushForEnd := func() {
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

	handleEnd := func(frame EndFrame, dir Direction, doneCh chan struct{}) bool {
		if pendingEnd != nil {
			t.taskCtx.Logger.Printf("EndFrame at TTSProcessor ignored: shutdown already pending: reason=%q\n", frame.Reason)
			if doneCh != nil {
				close(doneCh)
			}
			return false
		}
		flushForEnd()
		if awaitingTTSDone {
			pending := frame
			pendingEnd = &pending
			pendingEndDir = dir
			pendingEndDone = doneCh
			startPendingEndTimer()
			t.taskCtx.Logger.Printf("EndFrame at TTSProcessor deferred until TTS done: reason=%q\n", frame.Reason)
			return false
		}
		t.taskCtx.Logger.Printf("EndFrame at TTSProcessor forwarding immediately: reason=%q\n", frame.Reason)
		t.PushFrame(frame, dir)
		if doneCh != nil {
			close(doneCh)
		}
		t.stop()
		return true
	}

	handleCommand := func(cmd ttsCommand) (exit bool) {
		switch f := cmd.frame.(type) {
		case InterruptFrame:
			if pendingEnd != nil {
				t.taskCtx.Logger.Println("TTS shutdown is pending, dropping interrupt")
				return false
			}
			t.CancelTTSContext()
			clearTTSState()
			drainTTSEvents(t.ttsEvents)
			t.metrics.Reset()
			t.taskCtx.Logger.Println("TTS interrupted, cleared state")
			// (ProcessFrame already forwarded the InterruptFrame downstream)
			return false

		case EndFrame:
			return handleEnd(f, cmd.dir, cmd.done)

		case LLMResponseStartFrame:
			if pendingEnd != nil {
				return false
			}
			clearTTSState()
			drainTTSEvents(t.ttsEvents)
			t.metrics.Reset()
			t.currentContextId = fmt.Sprintf("ctx-%d", rand.IntN(9000000))
			t.activeContextId.Store(t.currentContextId)
			t.taskCtx.Logger.Println("LLM response started, resetting TTS aggregation")
			t.PushFrame(f, cmd.dir)
			return false

		case TextFrame:
			if pendingEnd != nil {
				return false
			}
			if !t.firstTextReceived {
				t.firstTextReceived = true
				t.metrics.Start(MetricTextAggregation)
			}
			t.currentAggregation += f.Text
			if endsWithPunctuation(t.currentAggregation) {
				if !t.firstSentenceSent {
					t.firstSentenceSent = true
					if mf := t.metrics.Stop(MetricTextAggregation); mf != nil {
						t.PushFrame(*mf, Downstream)
					}
					t.metrics.Start(MetricTTFB)
				}
				if t.sendTextToTTS(t.currentAggregation) {
					awaitingTTSDone = true
					ttsFlushSent = false
				}
				t.currentAggregation = ""
			}
			return false

		case LLMResponseEndFrame:
			if pendingEnd != nil {
				return false
			}
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
			t.PushFrame(f, cmd.dir)
			return false

		case TTSSpeakFrame:
			if pendingEnd != nil {
				return false
			}
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
			t.PushFrame(f, cmd.dir)
			return false

		default:
			if pendingEnd != nil {
				t.taskCtx.Logger.Printf("TTS shutdown pending, dropping frame: %T\n", f)
				return false
			}
			t.PushFrame(cmd.frame, cmd.dir)
			return false
		}
	}

	for {
		select {
		case <-t.ctx.Done():
			return
		case cmd := <-t.commands:
			if handleCommand(cmd) {
				return
			}
		case event := <-t.ttsEvents:
			if t.handleTTSEvent(event) {
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

// --- Cartesia interactions (called only from orchestrator goroutine) ---

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

func (t *TTSProcessor) isActiveContext(contextId string) bool {
	active, _ := t.activeContextId.Load().(string)
	return active != "" && active == contextId
}

func (t *TTSProcessor) isCurrentContext(contextID string) bool {
	return t.currentContextId != "" && t.currentContextId == contextID
}

// handleTTSEvent processes a Cartesia event. Called only from the
// orchestrator goroutine. Returns true if a done event was processed
// (which the orchestrator uses to clear awaitingTTSDone and potentially
// forward a pendingEnd).
func (t *TTSProcessor) handleTTSEvent(event ttsEvent) bool {
	if !t.isCurrentContext(event.contextID) {
		if event.eventType == ttsEventDone {
			t.taskCtx.Logger.Printf("TTS ignoring stale done: context_id=%s\n", event.contextID)
		}
		return false
	}

	switch event.eventType {
	case ttsEventAudioChunk:
		t.handleAudioChunkData(event.audioData)
	case ttsEventWordTimestamps:
		t.pendingWords = append(t.pendingWords, event.words...)
		t.emitPendingWords()
	case ttsEventDone:
		t.taskCtx.Logger.Printf("TTS synthesis done: context_id=%s\n", event.contextID)
		t.pushRemainingAudioFrames()
		t.PushFrame(TTSDoneFrame{}, Downstream)
		return true
	}
	return false
}

func (t *TTSProcessor) handleAudioChunkData(audioData []byte) {
	if t.currentContextId == "" {
		return
	}
	if !t.firstAudioReceived {
		t.firstAudioReceived = true
		if mf := t.metrics.Stop(MetricTTFB); mf != nil {
			t.PushFrame(*mf, Downstream)
		}
	}
	t.pcmBuffer = append(t.pcmBuffer, audioData...)

	for len(t.pcmBuffer) >= 960 {
		opusFrame := t.makeOpusData()
		if opusFrame == nil {
			break
		}
		t.PushFrame(AudioFrame{Data: opusFrame}, Downstream)
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
		t.PushFrame(WordTimestampFrame{Words: []string{w.word}}, Downstream)
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
			t.PushFrame(AudioFrame{Data: opusFrame}, Downstream)
			t.audioTimePushed += 0.02
		}
	}
	for _, w := range t.pendingWords {
		t.PushFrame(WordTimestampFrame{Words: []string{w.word}}, Downstream)
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
		t.taskCtx.Logger.Println("opus encode error:", err)
	}
	return opusData[:n]
}

// --- Cartesia websocket reader ---

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

func (t *TTSProcessor) pushTTSEvent(event ttsEvent) bool {
	select {
	case <-t.done:
		return false
	case t.ttsEvents <- event:
		return true
	}
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

func drainTTSEvents(ch chan ttsEvent) {
	for {
		select {
		case <-ch:
		default:
			return
		}
	}
}
