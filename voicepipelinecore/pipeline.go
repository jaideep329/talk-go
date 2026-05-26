package voicepipelinecore

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math/rand/v2"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	lksdk "github.com/livekit/server-sdk-go/v2"
)

// captureGoroutineStacks returns a textual dump of every live
// goroutine's stack. Used as a last-resort diagnostic when
// completeEnd's wg.Wait times out — the dump tells us which
// goroutines (STT reader, TTS reader, playback, etc.) are still
// blocked and exactly which call they're parked on. The buffer is
// grown until runtime.Stack stops truncating.
func captureGoroutineStacks() string {
	buf := make([]byte, 64<<10) // 64 KiB to start
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			return string(buf[:n])
		}
		if len(buf) >= 8<<20 { // cap at 8 MiB
			return string(buf[:n])
		}
		buf = make([]byte, len(buf)*2)
	}
}

// Pipeline is a thin wrapper that links processors into a chain and
// starts each one. Routing is distributed — each processor knows its
// prev/next pointer (set by Link). There is no central Send closure.
type Pipeline struct {
	processors []Processor
}

func NewPipeline(processors []Processor) *Pipeline {
	return &Pipeline{processors: processors}
}

// Start links neighbors and starts every processor with the given
// parent context. Each processor derives its own b.ctx from this
// parent, so cancelling parent cascades to all processors.
func (p *Pipeline) Start(parent context.Context) {
	for i := 0; i+1 < len(p.processors); i++ {
		p.processors[i].Link(p.processors[i+1])
	}
	for _, proc := range p.processors {
		proc.Start(parent)
	}
}

// Stop signals cancellation to every processor. The actual wait for
// their goroutines happens at the PipelineTask level via the shared
// WaitGroup; Stop itself returns immediately.
func (p *Pipeline) Stop() {
	for _, proc := range p.processors {
		proc.Stop()
	}
}

// TaskContext is the single dependency passed to all processors in a
// PipelineTask. Ctx is the root cancellation context for the task.
// Metrics is the handler invoked when a MetricsFrame is emitted by any
// processor (intercepted in BaseProcessor.PushFrame). wg is the shared
// WaitGroup used by BaseProcessor.Go() to track goroutines.
//
// EndTask is a closure that lets any processor request graceful task
// shutdown without holding a reference to PipelineTask. It enqueues an
// EndFrame at PipelineSource so every processor (including upstream of
// the caller) sees the EndFrame in pipeline order. Wired by
// NewTask to PipelineTask.End.
type TaskContext struct {
	Ctx        context.Context
	Logger     *log.Logger
	Room       *lksdk.Room
	UIEvents   *UIEventSender
	Metrics    func(MetricsFrame)
	EndTask    func(reason EndReason)
	wg         *sync.WaitGroup
	callStats  *callStatsTracker
	callEvents *callEventDispatcher
	metrics    *perTurnMetrics
}

// TaskOptions configures one voice session.
type TaskOptions struct {
	// Logger is required.
	Logger *log.Logger

	// RoomName is the LiveKit room name. If empty, NewTask mints one
	// like "room-1234567".
	RoomName string

	// InitialMessages seeds the LLM conversation context. A nil slice
	// preserves the core demo's default prompt; an explicit empty slice
	// means the caller is intentionally providing no initial context.
	InitialMessages []Message

	// MaxTalkTime overrides the talk-time monitor default when non-nil.
	MaxTalkTime *time.Duration

	// CallEvents contains one-shot call timeline hooks.
	CallEvents CallEvents

	// OnCleanup runs after all processor goroutines have exited. Use
	// this to remove the task from binary-level registries.
	OnCleanup func()
}

// PipelineTask is the lifecycle owner of a single voice-call pipeline.
// It manages the chain, queues lifecycle frames at the source, and
// runs cleanup when EndFrame reaches the sink.
type PipelineTask struct {
	TaskCtx        *TaskContext
	Cancel         context.CancelFunc
	Source         *PipelineSourceProcessor
	Pipeline       *Pipeline
	RoomName       string
	OnCleanup      func()
	onCallEnded    func(reason EndReason, stats CallStats)
	callStats      *callStatsTracker
	wg             sync.WaitGroup
	endRequested   atomic.Bool
	cleanupStarted atomic.Bool
	cleanupOnce    sync.Once
	endReasonMu    sync.Mutex
	endReason      EndReason
}

// NewTask builds a PipelineTask but does not start its processors. The
// LiveKit room is joined during construction so callers can mint the
// browser token after receiving the room name.
func NewTask(parentCtx context.Context, opts TaskOptions) (*PipelineTask, error) {
	if opts.Logger == nil {
		return nil, errors.New("voicepipelinecore: TaskOptions.Logger is required")
	}
	if parentCtx == nil {
		parentCtx = context.Background()
	}
	roomName := opts.RoomName
	if roomName == "" {
		roomName = fmt.Sprintf("room-%d", rand.IntN(9000000)+1000000)
	}
	logger := opts.Logger
	ctx, cancel := context.WithCancel(parentCtx)
	uiEvents := NewUIEventSender(logger)
	callStats := newCallStatsTracker()
	turnMetrics := &perTurnMetrics{}

	// task is created first (with zero-value WaitGroup) so taskCtx can
	// hold a pointer to it.
	task := &PipelineTask{
		Cancel:      cancel,
		RoomName:    roomName,
		OnCleanup:   opts.OnCleanup,
		onCallEnded: opts.CallEvents.OnCallEnded,
		callStats:   callStats,
	}

	metricsHandler := func(mf MetricsFrame) {
		turnMetrics.absorb(mf)
		for _, d := range mf.Data {
			logger.Printf("Metric [%s] %s: %.1fms\n", d.Processor, d.Label, d.ValueMs)
			uiEvents.Send(UIEvent{Type: Metrics, Data: map[string]interface{}{
				"processor": d.Processor,
				"label":     string(d.Label),
				"value_ms":  d.ValueMs,
			}})
		}
	}

	taskCtx := &TaskContext{
		Ctx:       ctx,
		Logger:    logger,
		UIEvents:  uiEvents,
		Metrics:   metricsHandler,
		EndTask:   task.End,
		wg:        &task.wg,
		callStats: callStats,
		metrics:   turnMetrics,
	}
	taskCtx.callEvents = newCallEventDispatcher(logger, opts.CallEvents)
	task.TaskCtx = taskCtx

	pipelineSource := NewPipelineSourceProcessor(taskCtx)
	audioSource := NewAudioSourceProcessor(taskCtx)
	room, err := JoinRoom(roomName, taskCtx, audioSource)
	if err != nil {
		if taskCtx.callEvents != nil {
			taskCtx.callEvents.stopAndDrain()
		}
		cancel()
		return nil, err
	}
	taskCtx.Room = room
	// Hand the room to the UI sender so it can publish DataPackets.
	// Any events emitted before this point (none today, but
	// defensive) silently no-op.
	uiEvents.SetRoom(taskCtx.Room)
	task.Source = pipelineSource

	sttProcessor := NewSTTProcessor(taskCtx)
	userIdle := NewUserIdleProcessor(taskCtx)
	var contextAggregator *ContextAggregator
	if opts.InitialMessages != nil {
		contextAggregator = NewContextAggregator(taskCtx, opts.InitialMessages)
	} else {
		contextAggregator = NewContextAggregator(taskCtx)
	}
	talkTimeMonitor := NewTalkTimeMonitoringProcessor(taskCtx)
	if opts.MaxTalkTime != nil {
		talkTimeMonitor = NewTalkTimeMonitoringProcessorWithMaxTalkTime(taskCtx, *opts.MaxTalkTime)
	}
	llmProcessor := NewLLMProcessor(taskCtx)
	ttsProcessor := NewTTSProcessor(taskCtx)
	playbackSink := NewPlaybackSinkProcessor(taskCtx)
	pipelineSink := NewPipelineSinkProcessor(taskCtx, task.completeEnd)

	task.Pipeline = NewPipeline([]Processor{
		pipelineSource,
		audioSource,
		sttProcessor,
		userIdle,
		contextAggregator,
		talkTimeMonitor,
		llmProcessor,
		ttsProcessor,
		playbackSink,
		pipelineSink,
	})
	return task, nil
}

// Start links and starts every processor in the task pipeline.
func (t *PipelineTask) Start() {
	t.Pipeline.Start(t.TaskCtx.Ctx)
}

func (t *PipelineTask) End(reason EndReason) {
	reason = normalizeEndReason(reason)
	if t.cleanupStarted.Load() || t.TaskCtx.Ctx.Err() != nil {
		t.TaskCtx.Logger.Printf("Ignoring EndFrame request after cleanup started: %s\n", reason)
		return
	}
	if !t.endRequested.CompareAndSwap(false, true) {
		t.TaskCtx.Logger.Printf("Ignoring duplicate EndFrame request: %s\n", reason)
		return
	}
	if t.cleanupStarted.Load() || t.TaskCtx.Ctx.Err() != nil {
		t.TaskCtx.Logger.Printf("Ignoring EndFrame request after cleanup started: %s\n", reason)
		return
	}
	t.setEndReason(reason)
	t.TaskCtx.Logger.Printf("Queueing EndFrame: %s\n", reason)
	t.Source.Queue(NewEndFrame(string(reason)))
}

func (t *PipelineTask) setEndReason(reason EndReason) {
	reason = normalizeEndReason(reason)
	t.endReasonMu.Lock()
	defer t.endReasonMu.Unlock()
	if t.endReason == "" {
		t.endReason = reason
	}
}

func (t *PipelineTask) currentEndReason() EndReason {
	t.endReasonMu.Lock()
	defer t.endReasonMu.Unlock()
	return normalizeEndReason(t.endReason)
}

// completeEnd is the EndFrame-driven cleanup. It is called from inside
// PipelineSinkProcessor.ProcessFrame, which runs on the sink's
// processLoop goroutine — a goroutine tracked by t.wg. If we ran the
// cleanup synchronously, t.wg.Wait() below would deadlock waiting for
// the very goroutine it's running in to call Done() (which only fires
// when ProcessFrame returns).
//
// Fix: dispatch the body to a separate, untracked goroutine and return
// immediately. The sink's processLoop then sees ProcessFrame return,
// auto-cancels b.ctx (per its EndFrame handler), and its deferred
// Done() fires — allowing the wg drain to make progress.
//
// cleanupOnce still gates re-entry, so even if completeEnd is invoked
// multiple times, runCleanup runs exactly once.
func (t *PipelineTask) completeEnd(frame EndFrame) {
	t.cleanupOnce.Do(func() {
		go t.runCleanup(frame)
	})
}

func (t *PipelineTask) runCleanup(frame EndFrame) {
	t.endRequested.Store(true)
	t.cleanupStarted.Store(true)
	reason := normalizeEndReason(EndReason(frame.Reason))
	t.setEndReason(reason)
	t.TaskCtx.Logger.Printf("EndFrame reached pipeline sink, cleaning up task: %s\n", reason)
	t.TaskCtx.UIEvents.Send(UIEvent{Type: CallEnded, Data: map[string]interface{}{"reason": reason}})

	// Stop signals cancellation to all processors; the actual wait happens
	// via the shared WaitGroup below.
	t.Pipeline.Stop()

	// Disconnect the LiveKit room before waiting. AudioSource's
	// ReadRTP doesn't respect context, so its reader only exits when
	// the room is closed — disconnecting here unblocks it so the
	// WaitGroup can drain.
	if t.TaskCtx.Room != nil {
		t.TaskCtx.Room.Disconnect()
	}

	if t.TaskCtx.callEvents != nil {
		t.TaskCtx.callEvents.stopAndDrain()
	}

	// Bounded wait for stragglers — 10s gives TTS's pending-end deferral
	// room to fire before we hard-cancel. If we time out, dump every
	// live goroutine's stack so we can see which one is blocked and
	// where.
	done := make(chan struct{})
	go func() { t.wg.Wait(); close(done) }()
	select {
	case <-done:
		t.TaskCtx.Logger.Println("All processor goroutines exited cleanly")
	case <-time.After(10 * time.Second):
		t.TaskCtx.Logger.Println("Timed out waiting 10s for processor goroutines; dumping stacks:")
		t.TaskCtx.Logger.Println(captureGoroutineStacks())
	}

	endedAt := time.Now()
	if t.callStats != nil {
		t.callStats.MarkUserLeft(endedAt)
	}
	if t.onCallEnded != nil {
		stats := CallStats{EndedAt: endedAt}
		if t.callStats != nil {
			stats.TotalUserDurationSec = t.callStats.TotalDurationSec(endedAt)
			stats.FirstUserAudioFrameAt = t.callStats.FirstUserAudioFrameAt()
		}
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.TaskCtx.Logger.Printf("OnCallEnded callback panicked: %v\n", r)
				}
			}()
			t.onCallEnded(t.currentEndReason(), stats)
		}()
	}

	t.Cancel()
	if t.OnCleanup != nil {
		t.OnCleanup()
	}
	t.TaskCtx.Logger.Printf("PipelineTask cleanup complete after EndFrame: reason=%q\n", reason)
}
