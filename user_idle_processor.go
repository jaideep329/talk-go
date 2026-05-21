package main

import "time"

const (
	idleTimeout    = 7 * time.Second
	maxIdlePrompts = 7
	idlePromptText = "Hello?"
)

type UserIdleProcessor struct {
	taskCtx      *TaskContext
	idleTimer       *time.Timer
	idlePromptCount int
	idleFrames      chan Frame // idle timer pushes TTSSpeakFrame here
}

func NewUserIdleProcessor(taskCtx *TaskContext) *UserIdleProcessor {
	return &UserIdleProcessor{
		taskCtx: taskCtx,
		idleFrames: make(chan Frame, 10),
	}
}

func (p *UserIdleProcessor) cancelIdleTimer() {
	if p.idleTimer != nil {
		p.idleTimer.Stop()
		p.idleTimer = nil
	}
}

func (p *UserIdleProcessor) startIdleTimer() {
	p.cancelIdleTimer()
	if p.idlePromptCount >= maxIdlePrompts {
		return
	}
	p.idleTimer = time.AfterFunc(idleTimeout, func() {
		p.idlePromptCount++
		p.taskCtx.Logger.Printf("User idle (%d/%d), injecting prompt\n", p.idlePromptCount, maxIdlePrompts)
		p.idleFrames <- TTSSpeakFrame{Text: idlePromptText}
	})
}

func (p *UserIdleProcessor) Process(ch ProcessorChannels) {
	for {
		select {
		case frame := <-ch.System:
			ch.Send(frame, Downstream)
		case frame, ok := <-ch.Data:
			if !ok {
				return
			}
			switch f := frame.(type) {
			case EndFrame:
				p.taskCtx.Logger.Printf("EndFrame at UserIdleProcessor data path, forwarding downstream: reason=%q\n", f.Reason)
				p.cancelIdleTimer()
				ch.Send(f, Downstream)
				return
			case TranscriptFrame:
				p.cancelIdleTimer()
				p.idlePromptCount = 0
				ch.Send(frame, Downstream)
			case BotStartedSpeakingFrame:
				p.cancelIdleTimer()
				// consumed here — no need to forward to STT
			case BotStoppedSpeakingFrame:
				p.startIdleTimer()
				// consumed here — no need to forward to STT
			default:
				// Pass everything else through in its original direction.
				// Downstream frames (from STT): forward downstream.
				// Upstream frames (from LLM): forward upstream.
				ch.Send(frame, Downstream)
			}
		case frame := <-p.idleFrames:
			ch.Send(frame, Downstream)
		}
	}
}
