package main

import (
	"context"
	"fmt"
	"log"
	"math/rand/v2"
	"sync"

	lksdk "github.com/livekit/server-sdk-go/v2"
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

// SessionContext is passed to processors that need session-level concerns
// (cancellation for cleanup, UI events for frontend updates).
type SessionContext struct {
	Ctx      context.Context
	UIEvents *UIEventSender
}

type Session struct {
	UIEvents *UIEventSender
	Cancel   context.CancelFunc
	Room     *lksdk.Room
	Logger   *log.Logger
}

var (
	sessions   = map[string]*Session{}
	sessionsMu sync.Mutex
)

func getSession(roomName string) *Session {
	sessionsMu.Lock()
	defer sessionsMu.Unlock()
	return sessions[roomName]
}

func removeSession(roomName string) {
	sessionsMu.Lock()
	defer sessionsMu.Unlock()
	delete(sessions, roomName)
}

func createSession() (string, *Session) {
	roomName := fmt.Sprintf("room-%d", rand.IntN(9000000)+1000000)
	logger := log.New(log.Writer(), fmt.Sprintf("[%s] ", roomName), log.Flags())
	ctx, cancel := context.WithCancel(context.Background())
	uiEvents := NewUIEventSender(logger)
	sessionCtx := &SessionContext{Ctx: ctx, UIEvents: uiEvents}

	turnCtx := NewTurnContext()
	audioSource := NewAudioSourceProcessor(logger)
	room := joinRoom(roomName, audioSource)

	session := &Session{UIEvents: uiEvents, Cancel: cancel, Room: room, Logger: logger}
	sessionsMu.Lock()
	sessions[roomName] = session
	sessionsMu.Unlock()

	sttProcessor := NewSTTProcessor(logger, sessionCtx)
	llmProcessor := NewLLMProcessor(logger, turnCtx, sessionCtx)
	ttsProcessor := NewTTSProcessor(logger, turnCtx, sessionCtx)
	playbackSink := NewPlaybackSinkProcessor(logger, room, turnCtx, sessionCtx)
	pipeline := NewPipeline([]FrameProcessor{audioSource, sttProcessor, llmProcessor, ttsProcessor, playbackSink})
	go pipeline.Run()
	return roomName, session
}
