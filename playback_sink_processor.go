package main

import (
	"log"
	"time"

	lksdk "github.com/livekit/server-sdk-go/v2"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"
)

type PlaybackSinkProcessor struct {
	logger          *log.Logger
	botTrack        *lksdk.LocalSampleTrack
	interrupted     bool
	playbackStarted bool
	turnCtx         *TurnContext
	sessionCtx      *SessionContext
}

func NewPlaybackSinkProcessor(logger *log.Logger, room *lksdk.Room, turnCtx *TurnContext, sessionCtx *SessionContext) *PlaybackSinkProcessor {
	botTrack, err := lksdk.NewLocalSampleTrack(webrtc.RTPCodecCapability{
		MimeType:  webrtc.MimeTypeOpus,
		ClockRate: 48000,
		Channels:  2,
	})
	if err != nil {
		logger.Fatal("failed to create local track:", err)
	}
	_, err = room.LocalParticipant.PublishTrack(botTrack, &lksdk.TrackPublicationOptions{
		Name: "bot-audio",
	})
	if err != nil {
		logger.Fatal("failed to publish track:", err)
	}
	logger.Println("Published bot audio track")
	return &PlaybackSinkProcessor{
		logger:     logger,
		botTrack:   botTrack,
		turnCtx:    turnCtx,
		sessionCtx: sessionCtx,
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
			done = nil
			p.logger.Println("Playback interrupted")
		case frame, ok := <-in:
			if !ok {
				return
			}
			switch f := frame.(type) {
			case AudioFrame:
				if p.interrupted {
					continue
				}
				if !p.playbackStarted {
					p.turnCtx.StartPlayback()
					p.playbackStarted = true
					e2eMs := time.Since(p.turnCtx.TurnStarted()).Milliseconds()
					p.logger.Printf("End-to-end turn latency: %dms\n", e2eMs)
					p.sessionCtx.UIEvents.Send(UIEvent{Type: "latency", TurnE2EMs: e2eMs})
				}
				err := p.botTrack.WriteSample(media.Sample{
					Data:     f.Data,
					Duration: 20 * time.Millisecond,
				}, nil)
				if err != nil {
					p.logger.Println("track write error:", err)
					continue
				}
				<-ticker.C
			case WordTimestampFrame:
				if !p.interrupted {
					p.turnCtx.AppendWords(f.Words, f.Start)
				}
			case LLMResponseStartFrame:
				p.interrupted = false
				p.playbackStarted = false
				done = p.turnCtx.Done()
			case EndFrame:
				return
			default:
				p.logger.Printf("PlaybackSink received frame of type %T\n", f)
			}
		}
	}
}
