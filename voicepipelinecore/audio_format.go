package voicepipelinecore

import "time"

const (
	defaultOutputSampleRate = 24000
	liveKitPCMUOutputRate   = 8000
	liveKitOpusMaxPacket    = 4000
	playbackFrameDuration   = 20 * time.Millisecond
	pcmBytesPerSample       = 2

	// Kept for tests and Daily's historical 24kHz frame shape.
	framePCM      = defaultOutputSampleRate * int(playbackFrameDuration/time.Millisecond) / 1000
	framePCMBytes = framePCM * pcmBytesPerSample
)

func outputSampleRateFromRoom(taskCtx *TaskContext) int {
	if taskCtx == nil || taskCtx.Room == nil {
		return defaultOutputSampleRate
	}
	rate := taskCtx.Room.OutputSampleRate()
	if rate <= 0 {
		return defaultOutputSampleRate
	}
	return rate
}

func pcmSamplesPerFrameForRate(sampleRate int) int {
	if sampleRate <= 0 {
		sampleRate = defaultOutputSampleRate
	}
	return sampleRate * int(playbackFrameDuration/time.Millisecond) / 1000
}

func pcmFrameBytesForRate(sampleRate int) int {
	return pcmSamplesPerFrameForRate(sampleRate) * pcmBytesPerSample
}
