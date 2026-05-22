package main

import (
	"context"
	"encoding/binary"

	"github.com/hraban/opus"
	"github.com/pion/webrtc/v4"
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

func (a *AudioSourceProcessor) readAudioTrack(track *webrtc.TrackRemote) {
	decoder, err := opus.NewDecoder(16000, 1)
	if err != nil {
		a.taskCtx.Logger.Fatal("failed to create opus decoder:", err)
	}

	pcmBuf := make([]int16, 960)

	for {
		select {
		case <-a.ctx.Done():
			a.taskCtx.Logger.Println("audio source reader exiting: processor stopped")
			return
		default:
		}

		rtpPacket, _, err := track.ReadRTP()
		if err != nil {
			a.taskCtx.Logger.Println("track read error:", err)
			return
		}

		n, err := decoder.Decode(rtpPacket.Payload, pcmBuf)
		if err != nil {
			a.taskCtx.Logger.Println("opus decode error:", err)
			continue
		}

		pcmBytes := make([]byte, n*2)
		for i := 0; i < n; i++ {
			binary.LittleEndian.PutUint16(pcmBytes[i*2:], uint16(pcmBuf[i]))
		}

		a.PushFrame(NewAudioFrame(pcmBytes), Downstream)
	}
}

// SetTrack is called by the LiveKit OnTrackSubscribed callback. It spawns
// a tracked goroutine that reads RTP packets, decodes opus to PCM, and
// pushes AudioFrames downstream.
func (a *AudioSourceProcessor) SetTrack(track *webrtc.TrackRemote) {
	a.Go(func() { a.readAudioTrack(track) })
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
