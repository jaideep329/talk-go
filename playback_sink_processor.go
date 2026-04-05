package main

import (
	"context"
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

func (p *PlaybackSinkProcessor) Process(ctx context.Context, in <-chan Frame, out chan<- Frame) {
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case frame, ok := <-in:
			if !ok {
				return // channel closed
			}
			switch f := frame.(type) {
			case AudioFrame:
				//log.Printf("Received audio frame with %d bytes\n", len(f.Data))
				err := p.botTrack.WriteSample(media.Sample{
					Data:     f.Data,
					Duration: 20 * time.Millisecond,
				}, nil)
				if err != nil {
					log.Println("track write error:", err)
					continue
				}
				<-ticker.C
			default:
				log.Printf("Received non-audio frame of type %T\n", frame)
			}
		}

	}
}
