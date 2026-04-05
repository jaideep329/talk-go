package main

import (
	"context"
	"encoding/binary"
	"log"

	"github.com/hraban/opus"
	"github.com/pion/webrtc/v4"
)

type AudioSourceProcessor struct {
	track       *webrtc.TrackRemote
	audioFrames chan Frame
}

func NewAudioSourceProcessor(readTrack *webrtc.TrackRemote) *AudioSourceProcessor {
	track := readTrack
	if track == nil {
		panic("AudioSourceProcessor requires a non-nil track")
	}
	return &AudioSourceProcessor{
		track:       track,
		audioFrames: make(chan Frame, 100), // Buffered channel for audio frames
	}
}

func (a *AudioSourceProcessor) readAudioTrack() {
	// Opus decoder outputting 16kHz mono — matches what Soniox expects
	decoder, err := opus.NewDecoder(16000, 1)
	if err != nil {
		log.Fatal("failed to create opus decoder:", err)
	}

	// Buffer for decoded PCM samples (960 samples per Opus frame at 48kHz = 320 at 16kHz)
	pcmBuf := make([]int16, 960)

	for {
		// Read an RTP packet from the track
		rtpPacket, _, err := a.track.ReadRTP()
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

func (a *AudioSourceProcessor) Process(ctx context.Context, _ <-chan Frame, out chan<- Frame) {
	go a.readAudioTrack()

	for {
		select {
		case frame := <-a.audioFrames:
			out <- frame
		case <-ctx.Done():
			return
		}
	}
}
