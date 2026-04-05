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

// SessionContext is the single dependency passed to all processors.
type SessionContext struct {
	Ctx      context.Context
	Logger   *log.Logger
	Room     *lksdk.Room
	UIEvents *UIEventSender
}

type Session struct {
	SessionCtx *SessionContext
	Cancel     context.CancelFunc
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

	sessionCtx := &SessionContext{Ctx: ctx, Logger: logger, UIEvents: uiEvents}
	turnCtx := NewTurnContext()
	audioSource := NewAudioSourceProcessor(sessionCtx)
	sessionCtx.Room = joinRoom(roomName, audioSource)
	session := &Session{SessionCtx: sessionCtx, Cancel: cancel}
	sessionsMu.Lock()
	sessions[roomName] = session
	sessionsMu.Unlock()

	sttProcessor := NewSTTProcessor(sessionCtx)
	llmProcessor := NewLLMProcessor(sessionCtx, turnCtx)
	ttsProcessor := NewTTSProcessor(sessionCtx, turnCtx)
	playbackSink := NewPlaybackSinkProcessor(sessionCtx, turnCtx)
	pipeline := NewPipeline([]FrameProcessor{audioSource, sttProcessor, llmProcessor, ttsProcessor, playbackSink})
	go pipeline.Run()
	return roomName, session
}
