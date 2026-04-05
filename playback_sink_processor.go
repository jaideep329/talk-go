package main

import (
	"log"
	"time"

	lksdk "github.com/livekit/server-sdk-go/v2"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"
)

type PlaybackSinkProcessor struct {
	botTrack    *lksdk.LocalSampleTrack
	interrupted bool
	turnCtx     *TurnContext
}

func NewPlaybackSinkProcessor(room *lksdk.Room, turnCtx *TurnContext) *PlaybackSinkProcessor {
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
	return &PlaybackSinkProcessor{
		botTrack: botTrack,
		turnCtx:  turnCtx,
	}
}

func (p *PlaybackSinkProcessor) Process(in <-chan Frame, _ chan<- Frame) {
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	done := p.turnCtx.Done()
	for {
		select {
		case <-done:
			p.interrupted = true
			done = nil // stop selecting on closed channel until next turn
			log.Println("Playback interrupted")
		case frame, ok := <-in:
			if !ok {
				return
			}
			switch f := frame.(type) {
			case AudioFrame:
				if p.interrupted {
					continue
				}
				err := p.botTrack.WriteSample(media.Sample{
					Data:     f.Data,
					Duration: 20 * time.Millisecond,
				}, nil)
				if err != nil {
					log.Println("track write error:", err)
					continue
				}
				<-ticker.C
			case LLMResponseStartFrame:
				log.Println("LLM response started, resetting playback state")
				p.interrupted = false
				done = p.turnCtx.Done() // re-enable interrupt for new turn
			case EndFrame:
				return
			default:
				log.Printf("PlaybackSink received frame of type %T\n", f)
			}
		}
	}
}
