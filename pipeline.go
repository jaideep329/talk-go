package main

import (
	"context"
	"sync"
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

var pipelineOnce sync.Once

func initPipeline() {
	pipelineOnce.Do(func() {
		turnCtx := NewTurnContext()
		audioSource := NewAudioSourceProcessor()
		joinRoom(audioSource)
		sttProcessor := NewSTTProcessor()
		llmProcessor := NewLLMProcessor(turnCtx)
		ttsProcessor := NewTTSProcessor(turnCtx)
		playbackSink := NewPlaybackSinkProcessor(room, turnCtx)
		pipeline := NewPipeline([]FrameProcessor{audioSource, sttProcessor, llmProcessor, ttsProcessor, playbackSink})
		go pipeline.Run()
	})
}
