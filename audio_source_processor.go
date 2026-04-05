package main

import (
	"encoding/binary"
	"log"

	"github.com/hraban/opus"
	"github.com/pion/webrtc/v4"
)

type AudioSourceProcessor struct {
	audioFrames chan Frame
}

func NewAudioSourceProcessor() *AudioSourceProcessor {
	return &AudioSourceProcessor{
		audioFrames: make(chan Frame, 100), // Buffered channel for audio frames
	}
}

func (a *AudioSourceProcessor) readAudioTrack(track *webrtc.TrackRemote) {
	// Opus decoder outputting 16kHz mono — matches what Soniox expects
	decoder, err := opus.NewDecoder(16000, 1)
	if err != nil {
		log.Fatal("failed to create opus decoder:", err)
	}

	// Buffer for decoded PCM samples (960 samples per Opus frame at 48kHz = 320 at 16kHz)
	pcmBuf := make([]int16, 960)

	for {
		// Read an RTP packet from the track
		rtpPacket, _, err := track.ReadRTP()
		if err != nil {
			log.Println("track read error:", err)
			return
		}

		// Decode Opus payload → PCM samples
		n, err := decoder.Decode(rtpPacket.Payload, pcmBuf)
		if err != nil {
			log.Println("opus decode error:", err)
			continue
		}

		// Convert int16 samples to bytes (s16le) for Soniox
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
