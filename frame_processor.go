package main

type Direction int

const (
	Downstream Direction = iota
	Upstream
)

// ProcessorChannels is the single argument passed to every processor's Process method.
// Data receives normal-priority frames (from upstream or downstream neighbors).
// System receives high-priority frames (InterruptionFrame, EndFrame) that bypass buffered data.
// Send routes a frame to the next (Downstream) or previous (Upstream) processor.
type ProcessorChannels struct {
	Data   <-chan Frame
	System <-chan Frame
	Send   func(frame Frame, dir Direction)
}

type FrameProcessor interface {
	Process(ch ProcessorChannels)
}
