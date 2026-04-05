package main

import (
	"time"

	lksdk "github.com/livekit/server-sdk-go/v2"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"
)

type PlaybackSinkProcessor struct {
	sessionCtx      *SessionContext
	turnCtx         *TurnContext
	botTrack        *lksdk.LocalSampleTrack
	interrupted     bool
	playbackStarted bool
}

func NewPlaybackSinkProcessor(sessionCtx *SessionContext, turnCtx *TurnContext) *PlaybackSinkProcessor {
	botTrack, err := lksdk.NewLocalSampleTrack(webrtc.RTPCodecCapability{
		MimeType:  webrtc.MimeTypeOpus,
		ClockRate: 48000,
		Channels:  2,
	})
	if err != nil {
		sessionCtx.Logger.Fatal("failed to create local track:", err)
	}
	_, err = sessionCtx.Room.LocalParticipant.PublishTrack(botTrack, &lksdk.TrackPublicationOptions{
		Name: "bot-audio",
	})
	if err != nil {
		sessionCtx.Logger.Fatal("failed to publish track:", err)
	}
	sessionCtx.Logger.Println("Published bot audio track")
	return &PlaybackSinkProcessor{
		sessionCtx: sessionCtx,
		turnCtx:    turnCtx,
		botTrack:   botTrack,
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
			p.sessionCtx.Logger.Println("Playback interrupted")
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
					p.playbackStarted = true
					e2eMs := time.Since(p.turnCtx.TurnStarted()).Milliseconds()
					p.sessionCtx.Logger.Printf("End-to-end turn latency: %dms\n", e2eMs)
					p.sessionCtx.UIEvents.Send(UIEvent{Type: "latency", TurnE2EMs: e2eMs})
				}
				err := p.botTrack.WriteSample(media.Sample{
					Data:     f.Data,
					Duration: 20 * time.Millisecond,
				}, nil)
				if err != nil {
					p.sessionCtx.Logger.Println("track write error:", err)
					continue
				}
				<-ticker.C
			case WordTimestampFrame:
				if !p.interrupted {
					p.turnCtx.AppendWords(f.Words)
					p.sessionCtx.UIEvents.Send(UIEvent{Type: "assistant_speaking", Text: f.Words[0]})
				}
			case TTSDoneFrame:
				if !p.interrupted {
					p.turnCtx.MarkPlaybackDone()
					p.sessionCtx.Logger.Println("Playback complete, marked turn done")
				}
			case LLMResponseStartFrame:
				p.interrupted = false
				p.playbackStarted = false
				done = p.turnCtx.Done()
			case EndFrame:
				return
			default:
				p.sessionCtx.Logger.Printf("PlaybackSink received frame of type %T\n", f)
			}
		}
	}
}
