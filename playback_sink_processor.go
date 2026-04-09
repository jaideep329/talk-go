package main

import (
	"encoding/binary"
	"io"
	"os"
	"time"

	gomp3 "github.com/hajimehoshi/go-mp3"
	"github.com/hraban/opus"
	lksdk "github.com/livekit/server-sdk-go/v2"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"
)

const (
	bgVolume  = 0.5 // background audio volume
	botVolume = 0.3 // bot speech volume when mixed
	framePCM  = 480 // 20ms at 24kHz
)

type PlaybackSinkProcessor struct {
	sessionCtx      *SessionContext
	botTrack        *lksdk.LocalSampleTrack
	metrics         *ProcessorMetrics
	interrupted     bool
	playbackStarted bool
	opusDecoder     *opus.Decoder
	opusEncoder     *opus.Encoder
	bgPCM           []int16 // background audio loop buffer (24kHz mono)
	bgPos           int     // current position in background loop
	playbackQueue   []Frame // ordered queue: AudioFrame, WordTimestampFrame, TTSDoneFrame
}

func loadBackgroundPCM(path string, logger interface{ Printf(string, ...interface{}) }) []int16 {
	f, err := os.Open(path)
	if err != nil {
		logger.Printf("Background audio not found (%s), mixing disabled\n", path)
		return nil
	}
	defer f.Close()

	dec, err := gomp3.NewDecoder(f)
	if err != nil {
		logger.Printf("Background MP3 decode error: %v\n", err)
		return nil
	}

	raw, err := io.ReadAll(dec)
	if err != nil {
		logger.Printf("Background MP3 read error: %v\n", err)
		return nil
	}

	// Convert stereo s16le bytes → mono int16
	numSamples := len(raw) / 4
	mono := make([]int16, numSamples)
	for i := 0; i < numSamples; i++ {
		left := int16(binary.LittleEndian.Uint16(raw[i*4:]))
		right := int16(binary.LittleEndian.Uint16(raw[i*4+2:]))
		mono[i] = int16((int32(left) + int32(right)) / 2)
	}
	logger.Printf("Background audio loaded: %d samples (%.1fs at 24kHz)\n", len(mono), float64(len(mono))/24000)
	return mono
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

	opusDec, err := opus.NewDecoder(24000, 1)
	if err != nil {
		sessionCtx.Logger.Fatal("failed to create opus decoder:", err)
	}
	opusEnc, err := opus.NewEncoder(24000, 1, opus.AppVoIP)
	if err != nil {
		sessionCtx.Logger.Fatal("failed to create opus encoder:", err)
	}

	bgPCM := loadBackgroundPCM("background-office-sound.mp3", sessionCtx.Logger)

	return &PlaybackSinkProcessor{
		sessionCtx:  sessionCtx,
		botTrack:    botTrack,
		metrics:     NewProcessorMetrics("playback"),
		opusDecoder: opusDec,
		opusEncoder: opusEnc,
		bgPCM:       bgPCM,
	}
}

func (p *PlaybackSinkProcessor) getBgFrame() []int16 {
	if len(p.bgPCM) == 0 {
		return nil
	}
	frame := make([]int16, framePCM)
	for i := 0; i < framePCM; i++ {
		frame[i] = p.bgPCM[p.bgPos]
		p.bgPos++
		if p.bgPos >= len(p.bgPCM) {
			p.bgPos = 0
		}
	}
	return frame
}

func (p *PlaybackSinkProcessor) mixAndEncode(botPCM []int16) []byte {
	bgFrame := p.getBgFrame()
	mixed := make([]int16, framePCM)

	for i := 0; i < framePCM; i++ {
		var sample float64
		if bgFrame != nil {
			sample += float64(bgFrame[i]) * bgVolume
		}
		if botPCM != nil {
			sample += float64(botPCM[i]) * botVolume
		}
		if sample > 32767 {
			sample = 32767
		} else if sample < -32768 {
			sample = -32768
		}
		mixed[i] = int16(sample)
	}

	opusData := make([]byte, 1000)
	n, err := p.opusEncoder.Encode(mixed, opusData)
	if err != nil {
		p.sessionCtx.Logger.Println("playback opus encode error:", err)
		return nil
	}
	return opusData[:n]
}

func (p *PlaybackSinkProcessor) decodeBotFrame(opusData []byte) []int16 {
	pcm := make([]int16, framePCM)
	_, err := p.opusDecoder.Decode(opusData, pcm)
	if err != nil {
		p.sessionCtx.Logger.Println("playback opus decode error:", err)
		return nil
	}
	return pcm
}

func (p *PlaybackSinkProcessor) Process(ch ProcessorChannels) {
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case frame := <-ch.System:
			switch frame.(type) {
			case InterruptFrame:
				p.interrupted = true
				p.playbackQueue = nil
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
					p.playbackQueue = nil
					p.sessionCtx.Logger.Println("Playback interrupted")
				case EndFrame:
					return
				}
			case frame, ok := <-ch.Data:
				if !ok {
					return
				}
				switch f := frame.(type) {
				case AudioFrame, WordTimestampFrame, TTSDoneFrame:
					if !p.interrupted {
						p.playbackQueue = append(p.playbackQueue, f)
					}
				case LLMResponseStartFrame:
					p.interrupted = false
					p.playbackStarted = false
					p.playbackQueue = nil
					p.metrics.StartAt(MetricE2ELatency, f.StartedAt)
				case TTSSpeakFrame:
					p.interrupted = false
					p.playbackStarted = false
					p.playbackQueue = nil
				case LLMResponseEndFrame:
					// ignore
				default:
					p.sessionCtx.Logger.Printf("PlaybackSink received frame of type %T\n", f)
				}
			case <-ticker.C:
				// Drain non-audio frames from front of queue, then play one audio frame.
				var botPCM []int16
				for len(p.playbackQueue) > 0 {
					switch f := p.playbackQueue[0].(type) {
					case AudioFrame:
						if !p.playbackStarted {
							p.playbackStarted = true
							ch.Send(BotStartedSpeakingFrame{}, Upstream)
							if mf := p.metrics.Stop(MetricE2ELatency); mf != nil {
								ch.Send(*mf, Downstream)
							}
						}
						botPCM = p.decodeBotFrame(f.Data)
						p.playbackQueue = p.playbackQueue[1:]
						goto mix // one audio frame per tick
					case WordTimestampFrame:
						p.sessionCtx.UIEvents.Send(UIEvent{Type: AssistantSpeaking, Data: map[string]interface{}{"text": f.Words[0]}})
						ch.Send(f, Upstream)
						p.playbackQueue = p.playbackQueue[1:]
					case TTSDoneFrame:
						p.sessionCtx.Logger.Println("Playback complete")
						ch.Send(f, Upstream)
						ch.Send(BotStoppedSpeakingFrame{}, Upstream)
						p.playbackQueue = p.playbackQueue[1:]
					default:
						p.playbackQueue = p.playbackQueue[1:]
					}
				}
			mix:
				opusData := p.mixAndEncode(botPCM)
				if opusData == nil {
					continue
				}
				p.botTrack.WriteSample(media.Sample{
					Data:     opusData,
					Duration: 20 * time.Millisecond,
				}, nil)
			}
		}
	}
}
