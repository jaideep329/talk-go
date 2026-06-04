package voicepipelinecore

import "time"

// RoomTransport is the media/signalling boundary for a PipelineTask.
// Daily and LiveKit both provide the same core operations to the
// processor chain: publish UI messages, write outbound bot PCM, clear
// already-buffered outbound audio on interruption, and disconnect.
type RoomTransport interface {
	RoomURL() string
	RoomName() string
	OutputSampleRate() int
	SendAppMessage(v interface{}) error
	WriteAudioPCM(pcm []byte) error
	ClearAudioBuffer()
	Disconnect()

	recordAudioTiming(name string, elapsed time.Duration)
	perfDiagnosticsEnabled() bool
}
