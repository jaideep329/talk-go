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
	Transcript
	End
	WordTimestamp
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

type TranscriptFrame struct {
	Text    string
	IsFinal bool
}

func (f TranscriptFrame) FrameType() FrameType { return Transcript }

type EndFrame struct{}

func (f EndFrame) FrameType() FrameType { return End }

type WordTimestampFrame struct {
	Words []string
	Start []float64 // seconds from start of context when each word begins
}

func (f WordTimestampFrame) FrameType() FrameType { return WordTimestamp }
