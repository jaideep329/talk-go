package main

import (
	"context"
	"sync"
	"time"
)

type Pipeline struct {
	processors []FrameProcessor
}

func NewPipeline(processors []FrameProcessor) *Pipeline {
	return &Pipeline{processors: processors}
}

func (p *Pipeline) Run() {
	current := make(chan Frame, 100) // first processor's input
	for _, processor := range p.processors {
		out := make(chan Frame, 100)
		go processor.Process(current, out)
		current = out
	}
}

// TurnContext is shared across processors for per-turn cancellation.
// LLM calls Reset() at the start of each turn and Cancel() on barge-in.
// TTS and PlaybackSink select on Done() to react instantly.
type TurnContext struct {
	mu     sync.Mutex
	ctx    context.Context
	cancel context.CancelFunc
}

func NewTurnContext() *TurnContext {
	ctx, cancel := context.WithCancel(context.Background())
	return &TurnContext{ctx: ctx, cancel: cancel}
}

// Reset creates a new context for a new turn.
func (t *TurnContext) Reset() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.ctx, t.cancel = context.WithCancel(context.Background())
}

// Cancel cancels the current turn (barge-in).
func (t *TurnContext) Cancel() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.cancel()
}

// Done returns the current turn's done channel to select on.
func (t *TurnContext) Done() <-chan struct{} {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.ctx.Done()
}

// SpokenText tracks words with their timestamps to determine what was
// actually spoken on barge-in. PlaybackSink records playback start time
// and appends words. LLM calls SpokenSoFar() on interrupt or FlushAll()
// on normal turn end.
type SpokenText struct {
	mu              sync.Mutex
	words           []string
	startTimes      []float64 // seconds from context start
	playbackStarted time.Time // when first audio frame was played
}

func NewSpokenText() *SpokenText {
	return &SpokenText{}
}

// StartPlayback records when audio playback began for this turn.
func (s *SpokenText) StartPlayback() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.playbackStarted = time.Now()
}

// Append adds words and their start times.
func (s *SpokenText) Append(words []string, startTimes []float64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, w := range words {
		// Add space before word unless it starts with punctuation
		if len(s.words) > 0 && len(w) > 0 && w[0] != '.' && w[0] != ',' && w[0] != '!' && w[0] != '?' && w[0] != ';' && w[0] != ':' {
			s.words = append(s.words, " "+w)
		} else {
			s.words = append(s.words, w)
		}
		if i < len(startTimes) {
			s.startTimes = append(s.startTimes, startTimes[i])
		}
	}
}

// SpokenSoFar returns only the words that were actually spoken based on
// elapsed playback time, then resets. Used on barge-in.
func (s *SpokenText) SpokenSoFar() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	elapsed := time.Since(s.playbackStarted).Seconds()
	var spoken string
	for i, w := range s.words {
		if i < len(s.startTimes) && s.startTimes[i] <= elapsed {
			spoken += w
		}
	}
	s.words = nil
	s.startTimes = nil
	return spoken
}

// FlushAll returns all words (full response completed) and resets.
func (s *SpokenText) FlushAll() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var text string
	for _, w := range s.words {
		text += w
	}
	s.words = nil
	s.startTimes = nil
	return text
}

var pipelineOnce sync.Once

func initPipeline() {
	pipelineOnce.Do(func() {
		turnCtx := NewTurnContext()
		spokenText := NewSpokenText()
		audioSource := NewAudioSourceProcessor()
		joinRoom(audioSource)
		sttProcessor := NewSTTProcessor()
		llmProcessor := NewLLMProcessor(turnCtx, spokenText)
		ttsProcessor := NewTTSProcessor(turnCtx)
		playbackSink := NewPlaybackSinkProcessor(room, turnCtx, spokenText)
		pipeline := NewPipeline([]FrameProcessor{audioSource, sttProcessor, llmProcessor, ttsProcessor, playbackSink})
		go pipeline.Run()
	})
}
