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
	n := len(p.processors)

	// Create data and system channels for each processor.
	dataChs := make([]chan Frame, n)
	sysChs := make([]chan Frame, n)
	for i := range n {
		dataChs[i] = make(chan Frame, 100)
		sysChs[i] = make(chan Frame, 100)
	}

	// Build a Send function for each processor.
	// Downstream: route to next processor's data or system channel based on IsSystem().
	// Upstream: route to previous processor's data channel.
	sendFn := func(i int) func(Frame, Direction) {
		return func(frame Frame, dir Direction) {
			switch dir {
			case Downstream:
				if i+1 < n {
					if frame.IsSystem() {
						sysChs[i+1] <- frame
					} else {
						dataChs[i+1] <- frame
					}
				}
			case Upstream:
				if i-1 >= 0 {
					dataChs[i-1] <- frame
				}
			}
		}
	}

	for i, processor := range p.processors {
		go processor.Process(ProcessorChannels{
			Data:   dataChs[i],
			System: sysChs[i],
			Send:   sendFn(i),
		})
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
	audioSource := NewAudioSourceProcessor(sessionCtx)
	sessionCtx.Room = joinRoom(roomName, audioSource)
	session := &Session{SessionCtx: sessionCtx, Cancel: cancel}
	sessionsMu.Lock()
	sessions[roomName] = session
	sessionsMu.Unlock()

	sttProcessor := NewSTTProcessor(sessionCtx)
	llmProcessor := NewLLMProcessor(sessionCtx)
	ttsProcessor := NewTTSProcessor(sessionCtx)
	playbackSink := NewPlaybackSinkProcessor(sessionCtx)
	pipeline := NewPipeline([]FrameProcessor{audioSource, sttProcessor, llmProcessor, ttsProcessor, playbackSink})
	go pipeline.Run()
	return roomName, session
}
