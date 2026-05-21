package main

import (
	"context"
	"sync/atomic"
	"time"
)

const (
	idleTimeout    = 7 * time.Second
	maxIdlePrompts = 7
	idlePromptText = "Hello?"
)

type UserIdleProcessor struct {
	*BaseProcessor
	taskCtx         *TaskContext
	idleTimer       *time.Timer
	idlePromptCount atomic.Int32
}

func NewUserIdleProcessor(taskCtx *TaskContext) *UserIdleProcessor {
	p := &UserIdleProcessor{taskCtx: taskCtx}
	p.BaseProcessor = NewBaseProcessor("UserIdle", p, taskCtx)
	return p
}

func (p *UserIdleProcessor) cancelIdleTimer() {
	if p.idleTimer != nil {
		p.idleTimer.Stop()
		p.idleTimer = nil
	}
}

func (p *UserIdleProcessor) startIdleTimer() {
	p.cancelIdleTimer()
	if p.idlePromptCount.Load() >= maxIdlePrompts {
		return
	}
	p.idleTimer = time.AfterFunc(idleTimeout, func() {
		count := p.idlePromptCount.Add(1)
		p.taskCtx.Logger.Printf("User idle (%d/%d), injecting prompt\n", count, maxIdlePrompts)
		p.PushFrame(TTSSpeakFrame{Text: idlePromptText}, Downstream)
	})
}

func (p *UserIdleProcessor) ProcessFrame(ctx context.Context, frame Frame, dir Direction) {
	switch f := frame.(type) {
	case EndFrame:
		p.taskCtx.Logger.Printf("EndFrame at UserIdleProcessor: reason=%q\n", f.Reason)
		p.cancelIdleTimer()
		p.PushFrame(f, dir)
	case TranscriptFrame:
		p.cancelIdleTimer()
		p.idlePromptCount.Store(0)
		p.PushFrame(frame, dir)
	case BotStartedSpeakingFrame:
		p.cancelIdleTimer()
		// consumed upstream here — UserIdle is the terminal upstream consumer
	case BotStoppedSpeakingFrame:
		p.startIdleTimer()
		// consumed upstream here
	default:
		p.PushFrame(frame, dir)
	}
}
