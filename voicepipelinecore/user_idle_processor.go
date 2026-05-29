package voicepipelinecore

import (
	"context"
	"sync/atomic"
	"time"
)

const (
	idleTimeout    = 7 * time.Second
	maxIdlePrompts = 7
	idlePromptText = "Hello?"

	// cancelOnIdleTimeout ends the call if there is no activity at all
	// (user speech / interim transcript / bot speaking) for this long.
	// Mirrors Pipecat's PipelineTask(cancel_on_idle_timeout=True,
	// idle_timeout_secs=120). It's a backstop: the idle-prompt logic
	// above normally ends a quiet call first (its prompts count as bot
	// activity, which resets this timer).
	cancelOnIdleTimeout = 120 * time.Second
)

type UserIdleProcessor struct {
	*BaseProcessor
	taskCtx         *TaskContext
	idleTimer       *time.Timer
	idlePromptCount atomic.Int32
	cancelResetCh   chan struct{}
}

func NewUserIdleProcessor(taskCtx *TaskContext) *UserIdleProcessor {
	p := &UserIdleProcessor{taskCtx: taskCtx, cancelResetCh: make(chan struct{}, 1)}
	p.BaseProcessor = NewBaseProcessor("UserIdle", p, taskCtx)
	return p
}

// Start launches the base loops plus the inactivity watchdog goroutine.
func (p *UserIdleProcessor) Start(ctx context.Context) {
	p.BaseProcessor.Start(ctx)
	p.Go(p.runCancelWatchdog)
}

// markActivity resets the inactivity watchdog. Non-blocking so it's cheap
// to call on every activity frame.
func (p *UserIdleProcessor) markActivity() {
	select {
	case p.cancelResetCh <- struct{}{}:
	default:
	}
}

// runCancelWatchdog ends the call when no activity is seen for
// cancelOnIdleTimeout. Reset by any user speech / interim transcript /
// bot speaking (see markActivity). Exits when the processor's context is
// cancelled (EndFrame / Stop).
func (p *UserIdleProcessor) runCancelWatchdog() {
	timer := time.NewTimer(cancelOnIdleTimeout)
	defer timer.Stop()
	for {
		select {
		case <-p.ctx.Done():
			return
		case <-p.cancelResetCh:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(cancelOnIdleTimeout)
		case <-timer.C:
			p.taskCtx.Logger.Printf("No activity for %s; ending call (idle timeout)\n", cancelOnIdleTimeout)
			if p.taskCtx.EndTask != nil {
				p.taskCtx.EndTask(EndReasonUserIdle)
			}
			return
		}
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
	if p.idlePromptCount.Load() >= maxIdlePrompts {
		return
	}
	p.idleTimer = time.AfterFunc(idleTimeout, func() {
		count := p.idlePromptCount.Add(1)
		if count > maxIdlePrompts {
			// We've already issued the final prompt and asked the
			// pipeline to end; ignore any straggling fires.
			return
		}
		if count == maxIdlePrompts {
			// Final attempt: speak the prompt one last time, then ask
			// the pipeline source to inject EndFrame so every processor
			// shuts down in order. Matches Python sales_call's
			// handle_idle returning False after retry == 6.
			p.taskCtx.Logger.Printf("User idle (%d/%d), final prompt; ending task\n", count, maxIdlePrompts)
			p.PushFrame(NewTTSSpeakFrame(idlePromptText), Downstream)
			if p.taskCtx.EndTask != nil {
				p.taskCtx.EndTask(EndReasonUserIdle)
			}
			return
		}
		p.taskCtx.Logger.Printf("User idle (%d/%d), injecting prompt\n", count, maxIdlePrompts)
		p.PushFrame(NewTTSSpeakFrame(idlePromptText), Downstream)
	})
}

func (p *UserIdleProcessor) ProcessFrame(ctx context.Context, frame Frame, dir Direction) {
	switch f := frame.(type) {
	case EndFrame:
		p.taskCtx.Logger.Printf("EndFrame at UserIdleProcessor: reason=%q\n", f.Reason)
		p.cancelIdleTimer()
		p.PushFrame(f, dir)
	case TranscriptFrame:
		// User speech / interim transcript: activity for the task idle
		// watchdog (Pipecat's UserSpeaking/InterimTranscription frames).
		p.markActivity()
		p.cancelIdleTimer()
		p.idlePromptCount.Store(0)
		p.PushFrame(frame, dir)
	case BotStartedSpeakingFrame:
		// Bot speaking: activity for the task idle watchdog.
		p.markActivity()
		p.cancelIdleTimer()
		// consumed upstream here — UserIdle is the terminal upstream consumer
	case BotStoppedSpeakingFrame:
		p.markActivity()
		p.startIdleTimer()
		// consumed upstream here
	default:
		p.PushFrame(frame, dir)
	}
}
