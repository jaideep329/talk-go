package main

import "time"

type FrameType int

type Frame interface {
	FrameType() FrameType
	IsSystem() bool
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
	TTSDone
	MetricsType
)

type AudioFrame struct {
	Data []byte
}

func (f AudioFrame) FrameType() FrameType { return Audio }
func (f AudioFrame) IsSystem() bool       { return false }

type TextFrame struct {
	Text string
}

func (f TextFrame) FrameType() FrameType { return Text }
func (f TextFrame) IsSystem() bool       { return false }

type InterruptFrame struct {
}

func (f InterruptFrame) FrameType() FrameType { return Interrupt }
func (f InterruptFrame) IsSystem() bool       { return true }

type LLMResponseStartFrame struct {
	StartedAt time.Time
}

func (f LLMResponseStartFrame) FrameType() FrameType { return LLMResponseStart }
func (f LLMResponseStartFrame) IsSystem() bool       { return false }

type LLMResponseEndFrame struct {
}

func (f LLMResponseEndFrame) FrameType() FrameType { return LLMResponseEnd }
func (f LLMResponseEndFrame) IsSystem() bool       { return false }

type TranscriptFrame struct {
	Text    string
	IsFinal bool
}

func (f TranscriptFrame) FrameType() FrameType { return Transcript }
func (f TranscriptFrame) IsSystem() bool       { return false }

type EndFrame struct{}

func (f EndFrame) FrameType() FrameType { return End }
func (f EndFrame) IsSystem() bool       { return true }

type WordTimestampFrame struct {
	Words []string
}

func (f WordTimestampFrame) FrameType() FrameType { return WordTimestamp }
func (f WordTimestampFrame) IsSystem() bool       { return false }

type TTSDoneFrame struct{}

func (f TTSDoneFrame) FrameType() FrameType { return TTSDone }
func (f TTSDoneFrame) IsSystem() bool       { return false }
