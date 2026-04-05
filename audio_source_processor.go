package main

import (
	"encoding/binary"
	"log"

	"github.com/hraban/opus"
	"github.com/pion/webrtc/v4"
)

type AudioSourceProcessor struct {
	logger      *log.Logger
	audioFrames chan Frame
}

func NewAudioSourceProcessor(logger *log.Logger) *AudioSourceProcessor {
	return &AudioSourceProcessor{
		logger:      logger,
		audioFrames: make(chan Frame, 100),
	}
}

func (a *AudioSourceProcessor) readAudioTrack(track *webrtc.TrackRemote) {
	decoder, err := opus.NewDecoder(16000, 1)
	if err != nil {
		a.logger.Fatal("failed to create opus decoder:", err)
	}

	pcmBuf := make([]int16, 960)

	for {
		rtpPacket, _, err := track.ReadRTP()
		if err != nil {
			a.logger.Println("track read error:", err)
			return
		}

		n, err := decoder.Decode(rtpPacket.Payload, pcmBuf)
		if err != nil {
			a.logger.Println("opus decode error:", err)
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

func (a *AudioSourceProcessor) Process(_ <-chan Frame, out chan<- Frame) {
	for frame := range a.audioFrames {
		out <- frame
	}
}
