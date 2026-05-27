package voicepipelinecore

import (
	"context"
	"sync/atomic"
	"time"
)

const (
	defaultMaxTalkTime     = 120 * time.Second
	talkTimeExceededPrompt = "Your talk time is exhausted now. Ending the call."
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
	p.taskCtx.Logger.Printf("Talk time exceeded after %s, sending closing prompt then ending task\n", p.maxTalkTime)
	p.taskCtx.UIEvents.ServerMessage(map[string]any{"type": "talktime_exhausted"}, time.Now())
	p.PushFrame(NewInterruptFrame(), Downstream)
	p.PushFrame(NewTTSSpeakFrame(talkTimeExceededPrompt), Downstream)
	// Route EndFrame through the task source instead of pushing it
	// downstream directly. This way upstream processors (STT,
	// AudioSource, UserIdle, ContextAggregator) also see the EndFrame
	// in pipeline order, matching Pipecat's source-driven lifecycle.
	if p.taskCtx.EndTask != nil {
		p.taskCtx.EndTask(EndReasonTalkTimeExhausted)
	}
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
