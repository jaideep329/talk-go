package main

import (
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

var pipelineOnce sync.Once

func initPipeline() {
	pipelineOnce.Do(func() {
		audioSource := NewAudioSourceProcessor()
		joinRoom(audioSource)
		sttProcessor := NewSTTProcessor()
		llmProcessor := NewLLMProcessor()
		ttsProcessor := NewTTSProcessor()
		playbackSink := NewPlaybackSinkProcessor(room)
		pipeline := NewPipeline([]FrameProcessor{audioSource, sttProcessor, llmProcessor, ttsProcessor, playbackSink})
		go pipeline.Run()
	})
}
