package main

import "context"

type Pipeline struct {
	processors []FrameProcessor
}

func NewPipeline(processors []FrameProcessor) *Pipeline {
	return &Pipeline{processors: processors}
}

func (p *Pipeline) Run(ctx context.Context) {
	current := make(chan Frame, 100) // first processor's input
	for _, processor := range p.processors {
		out := make(chan Frame, 100)
		go processor.Process(ctx, current, out)
		current = out
	}
}
