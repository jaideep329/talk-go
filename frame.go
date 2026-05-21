package main

import "time"

type FrameType int

// Frame is implemented by every frame flowing through the pipeline.
//
// IsSystem returns true for frames that should be processed with priority
// (they bypass the per-processor data queue). System frames are handled
// inline by BaseProcessor's input loop, before any pending data frames.
//
// IsInterruptible returns false for frames that must survive the queue
// purge that happens when InterruptFrame is processed. Only EndFrame
// needs this today; other data frames return true so they get dropped on
// interrupt. System frames are never enqueued in the purgeable queue, so
// their value is conventional (false).
type Frame interface {
	FrameType() FrameType
	IsSystem() bool
	IsInterruptible() bool
}

// BroadcastableFrame is implemented by frames that BaseProcessor.Broadcast
// can emit in both directions simultaneously. Clone must return a fresh,
// independent copy of the frame so the upstream and downstream copies
// don't share mutable state. Most broadcast frames are zero-field structs
// (BotStartedSpeakingFrame, BotStoppedSpeakingFrame) where Clone is a
// one-liner returning the zero value.
type BroadcastableFrame interface {
	Frame
	Clone() Frame
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
	TTSSpeak
	BotStartedSpeaking
	BotStoppedSpeaking
	LLMMessages
)

type AudioFrame struct {
	Data []byte
}

func (f AudioFrame) FrameType() FrameType { return Audio }
func (f AudioFrame) IsSystem() bool       { return false }
func (f AudioFrame) IsInterruptible() bool { return true }

type TextFrame struct {
	Text string
}

func (f TextFrame) FrameType() FrameType  { return Text }
func (f TextFrame) IsSystem() bool        { return false }
func (f TextFrame) IsInterruptible() bool { return true }

type InterruptFrame struct {
}

func (f InterruptFrame) FrameType() FrameType  { return Interrupt }
func (f InterruptFrame) IsSystem() bool        { return true }
func (f InterruptFrame) IsInterruptible() bool { return false }

type LLMResponseStartFrame struct {
	StartedAt time.Time
}

func (f LLMResponseStartFrame) FrameType() FrameType  { return LLMResponseStart }
func (f LLMResponseStartFrame) IsSystem() bool        { return false }
func (f LLMResponseStartFrame) IsInterruptible() bool { return true }

type LLMResponseEndFrame struct {
}

func (f LLMResponseEndFrame) FrameType() FrameType  { return LLMResponseEnd }
func (f LLMResponseEndFrame) IsSystem() bool        { return false }
func (f LLMResponseEndFrame) IsInterruptible() bool { return true }

type TranscriptFrame struct {
	Text       string
	IsFinal    bool
	ResponseID int  // Groups tokens from the same STT websocket message.
	Finished   bool // Stream-level Soniox flag; not an utterance boundary.
}

func (f TranscriptFrame) FrameType() FrameType  { return Transcript }
func (f TranscriptFrame) IsSystem() bool        { return false }
func (f TranscriptFrame) IsInterruptible() bool { return true }

// EndFrame travels downstream through the pipeline as a regular data
// frame (IsSystem returns false) so processors observe it in pipeline
// order. It returns IsInterruptible=false so that if an InterruptFrame
// is processed while an EndFrame is queued behind it, the EndFrame
// survives the purge. This mirrors Pipecat's UninterruptibleFrame mixin
// on EndFrame.
type EndFrame struct {
	Reason string
}

func (f EndFrame) FrameType() FrameType  { return End }
func (f EndFrame) IsSystem() bool        { return false }
func (f EndFrame) IsInterruptible() bool { return false }

type WordTimestampFrame struct {
	Words []string
}

func (f WordTimestampFrame) FrameType() FrameType  { return WordTimestamp }
func (f WordTimestampFrame) IsSystem() bool        { return false }
func (f WordTimestampFrame) IsInterruptible() bool { return true }

type TTSDoneFrame struct{}

func (f TTSDoneFrame) FrameType() FrameType  { return TTSDone }
func (f TTSDoneFrame) IsSystem() bool        { return false }
func (f TTSDoneFrame) IsInterruptible() bool { return true }

type TTSSpeakFrame struct {
	Text string
}

func (f TTSSpeakFrame) FrameType() FrameType  { return TTSSpeak }
func (f TTSSpeakFrame) IsSystem() bool        { return false }
func (f TTSSpeakFrame) IsInterruptible() bool { return true }

type BotStartedSpeakingFrame struct{}

func (f BotStartedSpeakingFrame) FrameType() FrameType  { return BotStartedSpeaking }
func (f BotStartedSpeakingFrame) IsSystem() bool        { return false }
func (f BotStartedSpeakingFrame) IsInterruptible() bool { return true }
func (f BotStartedSpeakingFrame) Clone() Frame          { return BotStartedSpeakingFrame{} }

type BotStoppedSpeakingFrame struct{}

func (f BotStoppedSpeakingFrame) FrameType() FrameType  { return BotStoppedSpeaking }
func (f BotStoppedSpeakingFrame) IsSystem() bool        { return false }
func (f BotStoppedSpeakingFrame) IsInterruptible() bool { return true }
func (f BotStoppedSpeakingFrame) Clone() Frame          { return BotStoppedSpeakingFrame{} }

type LLMMessagesFrame struct {
	Messages []map[string]string
}

func (f LLMMessagesFrame) FrameType() FrameType  { return LLMMessages }
func (f LLMMessagesFrame) IsSystem() bool        { return false }
func (f LLMMessagesFrame) IsInterruptible() bool { return true }
