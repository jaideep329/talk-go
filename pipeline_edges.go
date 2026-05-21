package main

import "log"

type PipelineSourceProcessor struct {
	frames chan Frame
	logger *log.Logger
}

func NewPipelineSourceProcessor(logger *log.Logger) *PipelineSourceProcessor {
	return &PipelineSourceProcessor{
		frames: make(chan Frame, 100),
		logger: logger,
	}
}

func (p *PipelineSourceProcessor) Queue(frame Frame) {
	if f, ok := frame.(EndFrame); ok && p.logger != nil {
		p.logger.Printf("EndFrame queued at pipeline source: reason=%q\n", f.Reason)
	}
	p.frames <- frame
}

func (p *PipelineSourceProcessor) Process(ch ProcessorChannels) {
	for {
		select {
		case frame := <-ch.System:
			ch.Send(frame, Downstream)
		case frame, ok := <-ch.Data:
			if !ok {
				return
			}
			ch.Send(frame, Downstream)
		case frame := <-p.frames:
			if f, ok := frame.(EndFrame); ok && p.logger != nil {
				p.logger.Printf("EndFrame entering pipeline from source: reason=%q\n", f.Reason)
			}
			ch.Send(frame, Downstream)
			if _, ok := frame.(EndFrame); ok {
				return
			}
		}
	}
}

type PipelineSinkProcessor struct {
	logger *log.Logger
	onEnd  func(EndFrame)
}

func NewPipelineSinkProcessor(logger *log.Logger, onEnd func(EndFrame)) *PipelineSinkProcessor {
	return &PipelineSinkProcessor{logger: logger, onEnd: onEnd}
}

func (p *PipelineSinkProcessor) Process(ch ProcessorChannels) {
	for {
		select {
		case <-ch.System:
		case frame, ok := <-ch.Data:
			if !ok {
				return
			}
			if f, ok := frame.(EndFrame); ok {
				p.handleEnd(f)
				return
			}
		}
	}
}

func (p *PipelineSinkProcessor) handleEnd(frame EndFrame) {
	if p.logger != nil {
		p.logger.Printf("EndFrame reached pipeline sink: reason=%q\n", frame.Reason)
	}
	if p.onEnd != nil {
		p.onEnd(frame)
	}
}
