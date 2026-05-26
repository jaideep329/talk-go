package voicepipelinecore

import (
	"context"
	"encoding/binary"
	"time"
)

type AudioSourceProcessor struct {
	*BaseProcessor
	taskCtx *TaskContext
}

func NewAudioSourceProcessor(taskCtx *TaskContext) *AudioSourceProcessor {
	a := &AudioSourceProcessor{taskCtx: taskCtx}
	a.BaseProcessor = NewBaseProcessor("AudioSource", a, taskCtx)
	return a
}

func hasAudibleSamples(samples []int16) bool {
	const audibleThreshold = 1000
	for _, sample := range samples {
		v := int32(sample)
		if v < 0 {
			v = -v
		}
		if v > audibleThreshold {
			return true
		}
	}
	return false
}

func (a *AudioSourceProcessor) maybeMarkFirstUserAudio(samples []int16) {
	if a.taskCtx == nil || a.taskCtx.callStats == nil || !hasAudibleSamples(samples) {
		return
	}
	at := time.Now()
	if a.taskCtx.callStats.MarkFirstUserAudio(at) && a.taskCtx.callEvents != nil {
		a.taskCtx.callEvents.fireFirstUserAudio(at)
	}
}

// PushPCM is called by the Daily bridge with raw 16kHz mono PCM from
// remote participants.
func (a *AudioSourceProcessor) PushPCM(pcmBytes []byte, sampleRate, channels int) {
	if sampleRate != 16000 || channels != 1 {
		a.taskCtx.Logger.Printf("unexpected Daily audio format: sample_rate=%d channels=%d\n", sampleRate, channels)
	}
	if len(pcmBytes) == 0 {
		return
	}
	samples := make([]int16, len(pcmBytes)/2)
	for i := range samples {
		samples[i] = int16(binary.LittleEndian.Uint16(pcmBytes[i*2:]))
	}
	a.maybeMarkFirstUserAudio(samples)
	a.PushFrame(NewAudioFrame(pcmBytes), Downstream)
}

func (a *AudioSourceProcessor) ProcessFrame(ctx context.Context, frame Frame, dir Direction) {
	switch f := frame.(type) {
	case EndFrame:
		a.taskCtx.Logger.Printf("EndFrame at AudioSourceProcessor: reason=%q\n", f.Reason)
		a.PushFrame(f, dir)
	default:
		a.PushFrame(frame, dir)
	}
}
