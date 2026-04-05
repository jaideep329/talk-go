package main

import (
	"fmt"
	"log"
	"math/rand/v2"
)

type Pipeline struct {
	processors []FrameProcessor
}

func NewPipeline(processors []FrameProcessor) *Pipeline {
	return &Pipeline{processors: processors}
}

func (p *Pipeline) Run() {
	current := make(chan Frame, 100)
	for _, processor := range p.processors {
		out := make(chan Frame, 100)
		go processor.Process(current, out)
		current = out
	}
}

func createSession() string {
	roomName := fmt.Sprintf("room-%d", rand.IntN(9000000)+1000000)
	logger := log.New(log.Writer(), fmt.Sprintf("[%s] ", roomName), log.Flags())
	turnCtx := NewTurnContext()
	audioSource := NewAudioSourceProcessor(logger)
	room := joinRoom(roomName, audioSource)
	sttProcessor := NewSTTProcessor(logger)
	llmProcessor := NewLLMProcessor(logger, turnCtx)
	ttsProcessor := NewTTSProcessor(logger, turnCtx)
	playbackSink := NewPlaybackSinkProcessor(logger, room, turnCtx)
	pipeline := NewPipeline([]FrameProcessor{audioSource, sttProcessor, llmProcessor, ttsProcessor, playbackSink})
	go pipeline.Run()
	return roomName
}
