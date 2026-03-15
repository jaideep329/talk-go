package main

type FrameType int

type Frame interface {
	FrameType() FrameType
}

const (
	Audio FrameType = iota // iota auto-increments: 0, 1, 2, 3...
	Text
	Interrupt
	LLMResponseStart
	LLMResponseEnd
)

type AudioFrame struct {
	Data []byte
}

func (f AudioFrame) FrameType() FrameType { return Audio }

type TextFrame struct {
	Text string
}

func (f TextFrame) FrameType() FrameType { return Text }

type InterruptFrame struct {
}

func (f InterruptFrame) FrameType() FrameType { return Interrupt }

type LLMResponseStartFrame struct {
}

func (f LLMResponseStartFrame) FrameType() FrameType { return LLMResponseStart }

type LLMResponseEndFrame struct {
}

func (f LLMResponseEndFrame) FrameType() FrameType { return LLMResponseEnd }
