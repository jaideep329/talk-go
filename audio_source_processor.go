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

		select {
		case <-a.sessionCtx.Ctx.Done():
			a.sessionCtx.Logger.Println("audio source reader exiting: session closed")
			return
		case a.audioFrames <- AudioFrame{Data: pcmBytes}:
		}
	}
}

func (a *AudioSourceProcessor) SetTrack(track *webrtc.TrackRemote) {
	go a.readAudioTrack(track)
}

func (a *AudioSourceProcessor) Process(ch ProcessorChannels) {
	for {
		select {
		case frame := <-ch.System:
			ch.Send(frame, Downstream)
		case frame, ok := <-ch.Data:
			if !ok {
				return
			}
			switch f := frame.(type) {
			case EndFrame:
				a.sessionCtx.Logger.Printf("EndFrame at AudioSourceProcessor, forwarding downstream: reason=%q\n", f.Reason)
				ch.Send(f, Downstream)
				return
			default:
				a.sessionCtx.Logger.Printf("AudioSource received unexpected frame: %T\n", f)
			}
		case frame := <-a.audioFrames:
			ch.Send(frame, Downstream)
		}
	}
}
