package main

import (
	"context"
	"sync/atomic"
	"time"
)

const (
	defaultMaxTalkTime     = 120 * time.Second
	talkTimeExceededPrompt = "Your talk time is exhausted now. Ending the call."
	talkTimeExceededReason = "talk time exhausted"
)

type TalkTimeMonitoringProcessor struct {
	*BaseProcessor
	taskCtx     *TaskContext
	maxTalkTime time.Duration
	ending      atomic.Bool
}

func NewTalkTimeMonitoringProcessor(taskCtx *TaskContext) *TalkTimeMonitoringProcessor {
	return NewTalkTimeMonitoringProcessorWithMaxTalkTime(taskCtx, defaultMaxTalkTime)
}

func NewTalkTimeMonitoringProcessorWithMaxTalkTime(taskCtx *TaskContext, maxTalkTime time.Duration) *TalkTimeMonitoringProcessor {
	p := &TalkTimeMonitoringProcessor{
		taskCtx:     taskCtx,
		maxTalkTime: maxTalkTime,
	}
	p.BaseProcessor = NewBaseProcessor("TalkTimeMonitor", p, taskCtx)
	return p
}

func (p *TalkTimeMonitoringProcessor) Start(ctx context.Context) {
	p.BaseProcessor.Start(ctx)
	p.Go(p.runTimer)
}

func (p *TalkTimeMonitoringProcessor) runTimer() {
	p.taskCtx.Logger.Printf("Talk time monitor started: max_talk_time=%s\n", p.maxTalkTime)
	select {
	case <-p.ctx.Done():
		return
	case <-time.After(p.maxTalkTime):
	}
	if !p.ending.CompareAndSwap(false, true) {
		return
	}
	p.taskCtx.Logger.Printf("Talk time exceeded after %s, sending closing prompt then EndFrame\n", p.maxTalkTime)
	p.PushFrame(InterruptFrame{}, Downstream)
	p.PushFrame(TTSSpeakFrame{Text: talkTimeExceededPrompt}, Downstream)
	p.PushFrame(EndFrame{Reason: talkTimeExceededReason}, Downstream)
}

func (p *TalkTimeMonitoringProcessor) ProcessFrame(ctx context.Context, frame Frame, dir Direction) {
	switch f := frame.(type) {
	case EndFrame:
		p.taskCtx.Logger.Printf("EndFrame at TalkTimeMonitoringProcessor: reason=%q\n", f.Reason)
		p.PushFrame(f, dir)
	case InterruptFrame:
		if p.ending.Load() {
			p.taskCtx.Logger.Println("Talk time shutdown in progress, dropping interrupt")
			return
		}
		p.PushFrame(f, dir)
	default:
		if p.ending.Load() && dir == Downstream {
			p.taskCtx.Logger.Printf("Talk time shutdown in progress, dropping downstream frame: %T\n", frame)
			return
		}
		p.PushFrame(frame, dir)
	}
}
