package main

import (
	"log"
	"time"

	lksdk "github.com/livekit/server-sdk-go/v2"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"
)

type PlaybackSinkProcessor struct {
	botTrack *lksdk.LocalSampleTrack
}

func NewPlaybackSinkProcessor(room *lksdk.Room) *PlaybackSinkProcessor {
	botTrack, err := lksdk.NewLocalSampleTrack(webrtc.RTPCodecCapability{
		MimeType:  webrtc.MimeTypeOpus,
		ClockRate: 48000,
		Channels:  2,
	})
	if err != nil {
		log.Fatal("failed to create local track:", err)
	}
	_, err = room.LocalParticipant.PublishTrack(botTrack, &lksdk.TrackPublicationOptions{
		Name: "bot-audio",
	})
	if err != nil {
		return nil
	}
	log.Println("Published bot audio track")
	return &PlaybackSinkProcessor{botTrack: botTrack}
}

func (p *PlaybackSinkProcessor) Process(in <-chan Frame, _ chan<- Frame) {
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for frame := range in {
		switch f := frame.(type) {
		case AudioFrame:
			err := p.botTrack.WriteSample(media.Sample{
				Data:     f.Data,
				Duration: 20 * time.Millisecond,
			}, nil)
			if err != nil {
				log.Println("track write error:", err)
				continue
			}
			<-ticker.C
		case EndFrame:
			return
		default:
			log.Printf("PlaybackSink received frame of type %T\n", f)
		}
	}
}
