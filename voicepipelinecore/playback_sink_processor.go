package voicepipelinecore

import (
	"context"
	"encoding/binary"
	"io"
	"os"
	"time"

	gomp3 "github.com/hajimehoshi/go-mp3"
	"github.com/hraban/opus"
)

const (
	bgVolume        = 0.5 // background audio volume
	botVolume       = 0.3 // bot speech volume when mixed
	framePCM        = 480 // 20ms at 24kHz
	playbackEndTail = 1 * time.Second
)

// PlaybackSinkProcessor is the bottom of the chain. It writes raw PCM to
// the Daily bridge and owns a 20ms ticker that paces playback. Frame
// ownership is split:
//   - ProcessFrame (on processLoop / inputLoop) forwards incoming frames
//     to runPlayback via queueCh.
//   - runPlayback owns playbackQueue, interrupted, playbackStarted, and
//     all writes to the Daily bridge.
//
// On EndFrame, ProcessFrame blocks until runPlayback has drained the
// queue, written the silence tail, and forwarded EndFrame downstream,
// so the base's auto-shutdown does not cancel ctx mid-drain.
type PlaybackSinkProcessor struct {
	*BaseProcessor
	taskCtx     *TaskContext
	metrics     *ProcessorMetrics
	opusDecoder *opus.Decoder
	bgPCM       []int16 // background audio loop buffer (24kHz mono)

	queueCh chan Frame    // ProcessFrame → runPlayback
	endDone chan struct{} // closed by runPlayback after EndFrame is forwarded

	// Owned by runPlayback only:
	bgPos           int
	interrupted     bool
	playbackStarted bool
	playbackQueue   []Frame
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

func NewPlaybackSinkProcessor(taskCtx *TaskContext) *PlaybackSinkProcessor {
	opusDec, err := opus.NewDecoder(24000, 1)
	if err != nil {
		taskCtx.Logger.Fatal("failed to create opus decoder:", err)
	}

	bgPCM := loadBackgroundPCM("background-office-sound.mp3", taskCtx.Logger)

	p := &PlaybackSinkProcessor{
		taskCtx:     taskCtx,
		metrics:     NewProcessorMetrics("playback"),
		opusDecoder: opusDec,
		bgPCM:       bgPCM,
		queueCh:     make(chan Frame, 256),
		endDone:     make(chan struct{}),
	}
	p.BaseProcessor = NewBaseProcessor("PlaybackSink", p, taskCtx)
	return p
}

func (p *PlaybackSinkProcessor) Start(ctx context.Context) {
	p.BaseProcessor.Start(ctx)
	p.Go(p.runPlayback)
}

func (p *PlaybackSinkProcessor) ProcessFrame(ctx context.Context, frame Frame, dir Direction) {
	switch frame.(type) {
	case EndFrame:
		// EndFrame must drain through runPlayback (audio tail + silence)
		// before the base auto-cancels b.ctx.
		select {
		case p.queueCh <- frame:
		case <-p.ctx.Done():
			return
		}
		select {
		case <-p.endDone:
		case <-p.ctx.Done():
		}
	case AudioFrame, WordTimestampFrame, TTSDoneFrame,
		LLMResponseStartFrame, TTSSpeakFrame, InterruptFrame:
		select {
		case p.queueCh <- frame:
		case <-p.ctx.Done():
		}
	case LLMResponseEndFrame:
		// ignore; playback doesn't care about LLM stream boundaries
	default:
		// Unknown frames terminate here.
	}
}

func (p *PlaybackSinkProcessor) runPlayback() {
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-p.ctx.Done():
			return
		case f := <-p.queueCh:
			p.handleQueueFrame(f)
		case <-ticker.C:
			if p.tick() {
				close(p.endDone)
				return
			}
		}
	}
}

func (p *PlaybackSinkProcessor) handleQueueFrame(f Frame) {
	switch v := f.(type) {
	case AudioFrame, WordTimestampFrame, TTSDoneFrame:
		if !p.interrupted {
			p.playbackQueue = append(p.playbackQueue, v)
		}
	case EndFrame:
		if shouldStopPlaybackImmediately(v.Reason) {
			p.taskCtx.Logger.Printf("EndFrame stopping PlaybackSink immediately, dropping %d pending playback frames: reason=%q\n", len(p.playbackQueue), v.Reason)
			p.playbackQueue = nil
			p.interrupted = true
		} else {
			p.taskCtx.Logger.Printf("EndFrame queued in PlaybackSink after %d pending playback frames: reason=%q\n", len(p.playbackQueue), v.Reason)
		}
		p.playbackQueue = append(p.playbackQueue, v)
	case LLMResponseStartFrame:
		p.interrupted = false
		p.playbackStarted = false
		p.playbackQueue = nil
		p.metrics.StartAt(MetricE2ELatency, v.StartedAt)
	case TTSSpeakFrame:
		p.interrupted = false
		p.playbackStarted = false
		p.playbackQueue = nil
	case InterruptFrame:
		p.interrupted = true
		// EndFrame and any other !IsInterruptible() frames survive the purge.
		kept := p.playbackQueue[:0]
		for _, qf := range p.playbackQueue {
			if !qf.IsInterruptible() {
				kept = append(kept, qf)
			}
		}
		p.playbackQueue = kept
		p.taskCtx.Logger.Printf("Playback interrupted (kept %d uninterruptible frames)\n", len(kept))
	}
}

// tick advances playback by one 20ms frame. Returns true iff an
// EndFrame was processed and runPlayback should exit.
func (p *PlaybackSinkProcessor) tick() bool {
	var botPCM []int16
	for len(p.playbackQueue) > 0 {
		switch f := p.playbackQueue[0].(type) {
		case AudioFrame:
			if !p.playbackStarted {
				p.playbackStarted = true
				at := time.Now()
				if p.taskCtx.callEvents != nil {
					p.taskCtx.callEvents.fireBotFirstSpeech(at)
				}
				p.taskCtx.UIEvents.BotStartedSpeaking(at)
				// Broadcast both ways: upstream lets UserIdleProcessor
				// cancel its idle timer; downstream is informational for
				// any future post-Playback consumer. Mirrors Pipecat's
				// BaseOutputTransport.MediaSender._bot_started_speaking.
				p.Broadcast(NewBotStartedSpeakingFrame())
				if mf := p.metrics.Stop(MetricE2ELatency); mf != nil {
					p.PushFrame(*mf, Downstream)
				}
			}
			botPCM = p.decodeBotFrame(f.Data)
			p.playbackQueue = p.playbackQueue[1:]
			goto mix
		case WordTimestampFrame:
			p.PushFrame(f, Upstream)
			p.playbackQueue = p.playbackQueue[1:]
		case TTSDoneFrame:
			p.taskCtx.Logger.Println("Playback complete")
			p.taskCtx.UIEvents.BotStoppedSpeaking(time.Now())
			p.PushFrame(f, Upstream)
			// Broadcast: upstream tells ContextAggregator/UserIdle that
			// the bot finished speaking; downstream copy is for parity
			// with Pipecat (no consumer today).
			p.Broadcast(NewBotStoppedSpeakingFrame())
			p.playbackQueue = p.playbackQueue[1:]
		case EndFrame:
			p.playbackQueue = p.playbackQueue[1:]
			if !shouldStopPlaybackImmediately(f.Reason) {
				p.writeSilenceTail()
			}
			p.taskCtx.Logger.Printf("EndFrame leaving PlaybackSink after playback handling: reason=%q\n", f.Reason)
			p.PushFrame(f, Downstream)
			return true
		default:
			p.playbackQueue = p.playbackQueue[1:]
		}
	}
mix:
	pcm := p.mixPCM(botPCM)
	_ = p.taskCtx.Room.WriteAudioPCM(pcm)
	return false
}

func shouldStopPlaybackImmediately(reason string) bool {
	switch EndReason(reason) {
	case EndReasonClientDisconnect, EndReasonError:
		return true
	default:
		return false
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

func (p *PlaybackSinkProcessor) mixPCM(botPCM []int16) []byte {
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

	pcmBytes := make([]byte, len(mixed)*2)
	for i, sample := range mixed {
		binary.LittleEndian.PutUint16(pcmBytes[i*2:], uint16(sample))
	}
	return pcmBytes
}

func (p *PlaybackSinkProcessor) decodeBotFrame(opusData []byte) []int16 {
	pcm := make([]int16, framePCM)
	_, err := p.opusDecoder.Decode(opusData, pcm)
	if err != nil {
		p.taskCtx.Logger.Println("playback opus decode error:", err)
		return nil
	}
	return pcm
}

func (p *PlaybackSinkProcessor) writeSilenceTail() {
	if !p.playbackStarted || playbackEndTail <= 0 {
		return
	}
	p.taskCtx.Logger.Printf("Writing %s playback silence before EndFrame\n", playbackEndTail)

	silence := make([]byte, framePCM*2)
	frames := int(playbackEndTail / (20 * time.Millisecond))
	for i := 0; i < frames; i++ {
		_ = p.taskCtx.Room.WriteAudioPCM(silence)
		time.Sleep(20 * time.Millisecond)
	}
}
