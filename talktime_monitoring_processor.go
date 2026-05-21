package main

import "time"

const (
	defaultMaxTalkTime     = 120 * time.Second
	talkTimeExceededPrompt = "Your talk time is exhausted now. Ending the call."
	talkTimeExceededReason = "talk time exhausted"
)

type TalkTimeMonitoringProcessor struct {
	taskCtx  *TaskContext
	maxTalkTime time.Duration
}

func NewTalkTimeMonitoringProcessor(taskCtx *TaskContext) *TalkTimeMonitoringProcessor {
	return NewTalkTimeMonitoringProcessorWithMaxTalkTime(taskCtx, defaultMaxTalkTime)
}

func NewTalkTimeMonitoringProcessorWithMaxTalkTime(taskCtx *TaskContext, maxTalkTime time.Duration) *TalkTimeMonitoringProcessor {
	return &TalkTimeMonitoringProcessor{
		taskCtx:  taskCtx,
		maxTalkTime: maxTalkTime,
	}
}

func (p *TalkTimeMonitoringProcessor) Process(ch ProcessorChannels) {
	timer := time.NewTimer(p.maxTalkTime)
	defer timer.Stop()

	p.taskCtx.Logger.Printf("Talk time monitor started: max_talk_time=%s\n", p.maxTalkTime)

	var timerC <-chan time.Time = timer.C
	ending := false

	for {
		select {
		case frame := <-ch.System:
			switch f := frame.(type) {
			case InterruptFrame:
				if ending {
					p.taskCtx.Logger.Println("Talk time shutdown in progress, dropping interrupt")
					continue
				}
				ch.Send(f, Downstream)
			default:
				ch.Send(frame, Downstream)
			}

		case frame, ok := <-ch.Data:
			if !ok {
				return
			}
			switch f := frame.(type) {
			case EndFrame:
				p.taskCtx.Logger.Printf("EndFrame at TalkTimeMonitoringProcessor data path, forwarding downstream: reason=%q\n", f.Reason)
				ch.Send(f, Downstream)
				return
			case WordTimestampFrame:
				ch.Send(f, Upstream)
			case TTSDoneFrame:
				ch.Send(f, Upstream)
			case BotStartedSpeakingFrame:
				ch.Send(f, Upstream)
			case BotStoppedSpeakingFrame:
				ch.Send(f, Upstream)
			default:
				if ending {
					p.taskCtx.Logger.Printf("Talk time shutdown in progress, dropping downstream frame: %T\n", f)
					continue
				}
				ch.Send(frame, Downstream)
			}

		case <-timerC:
			timerC = nil
			if ending {
				continue
			}
			ending = true
			p.taskCtx.Logger.Printf("Talk time exceeded after %s, sending closing prompt then EndFrame\n", p.maxTalkTime)
			ch.Send(InterruptFrame{}, Downstream)
			ch.Send(TTSSpeakFrame{Text: talkTimeExceededPrompt}, Downstream)
			ch.Send(EndFrame{Reason: talkTimeExceededReason}, Downstream)
		}
	}
}
