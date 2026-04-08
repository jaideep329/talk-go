package main

import (
	"time"

	lksdk "github.com/livekit/server-sdk-go/v2"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"
)

type PlaybackSinkProcessor struct {
	sessionCtx      *SessionContext
	botTrack        *lksdk.LocalSampleTrack
	interrupted     bool
	playbackStarted bool
	turnStarted     time.Time
}

func NewPlaybackSinkProcessor(sessionCtx *SessionContext) *PlaybackSinkProcessor {
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
		botTrack:   botTrack,
	}
}

func (p *PlaybackSinkProcessor) Process(ch ProcessorChannels) {
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		// Priority: check system channel first (InterruptionFrame, EndFrame).
		select {
		case frame := <-ch.System:
			switch frame.(type) {
			case InterruptFrame:
				p.interrupted = true
				p.sessionCtx.Logger.Println("Playback interrupted")
			case EndFrame:
				return
			}
		default:
			select {
			case frame := <-ch.System:
				switch frame.(type) {
				case InterruptFrame:
					p.interrupted = true
					p.sessionCtx.Logger.Println("Playback interrupted")
				case EndFrame:
					return
				}
			case frame, ok := <-ch.Data:
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
						e2eMs := time.Since(p.turnStarted).Milliseconds()
						p.sessionCtx.Logger.Printf("End-to-end turn latency: %dms\n", e2eMs)
						p.sessionCtx.UIEvents.Send(UIEvent{Type: Latency, Data: map[string]interface{}{"turn_e2e_ms": e2eMs}})
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
						p.sessionCtx.UIEvents.Send(UIEvent{Type: AssistantSpeaking, Data: map[string]interface{}{"text": f.Words[0]}})
						ch.Send(f, Upstream) // LLM accumulates words
					}
				case TTSDoneFrame:
					if !p.interrupted {
						p.sessionCtx.Logger.Println("Playback complete")
						ch.Send(f, Upstream) // LLM commits spoken text
					}
				case LLMResponseStartFrame:
					p.interrupted = false
					p.playbackStarted = false
					p.turnStarted = f.StartedAt
				case LLMResponseEndFrame:
					// Not used by PlaybackSink — ignore silently.
				default:
					p.sessionCtx.Logger.Printf("PlaybackSink received frame of type %T\n", f)
				}
			}
		}
	}
}
