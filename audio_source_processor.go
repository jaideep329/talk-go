package main

import (
	"encoding/binary"

	"github.com/hraban/opus"
	"github.com/pion/webrtc/v4"
)

type AudioSourceProcessor struct {
	sessionCtx  *SessionContext
	audioFrames chan Frame
}

func NewAudioSourceProcessor(sessionCtx *SessionContext) *AudioSourceProcessor {
	return &AudioSourceProcessor{
		sessionCtx:  sessionCtx,
		audioFrames: make(chan Frame, 100),
	}
}

func (a *AudioSourceProcessor) readAudioTrack(track *webrtc.TrackRemote) {
	decoder, err := opus.NewDecoder(16000, 1)
	if err != nil {
		a.sessionCtx.Logger.Fatal("failed to create opus decoder:", err)
	}

	pcmBuf := make([]int16, 960)

	for {
		rtpPacket, _, err := track.ReadRTP()
		if err != nil {
			a.sessionCtx.Logger.Println("track read error:", err)
			return
		}

		n, err := decoder.Decode(rtpPacket.Payload, pcmBuf)
		if err != nil {
			a.sessionCtx.Logger.Println("opus decode error:", err)
			continue
		}

		pcmBytes := make([]byte, n*2)
		for i := 0; i < n; i++ {
			binary.LittleEndian.PutUint16(pcmBytes[i*2:], uint16(pcmBuf[i]))
		}

		a.audioFrames <- AudioFrame{Data: pcmBytes}
	}
}

func (a *AudioSourceProcessor) SetTrack(track *webrtc.TrackRemote) {
	go a.readAudioTrack(track)
}

func (a *AudioSourceProcessor) Process(ch ProcessorChannels) {
	for frame := range a.audioFrames {
		ch.Send(frame, Downstream)
	}
}
