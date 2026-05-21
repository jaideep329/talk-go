package main

import (
	"context"
	"fmt"
	"log"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"time"

	lksdk "github.com/livekit/server-sdk-go/v2"
)

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
type TaskContext struct {
	Ctx      context.Context
	Logger   *log.Logger
	Room     *lksdk.Room
	UIEvents *UIEventSender
	Metrics  func(MetricsFrame)
	wg       *sync.WaitGroup
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
	wg             sync.WaitGroup
	endRequested   atomic.Bool
	cleanupStarted atomic.Bool
	cleanupOnce    sync.Once
}

var (
	sessions   = map[string]*PipelineTask{}
	sessionsMu sync.Mutex
)

func getSession(roomName string) *PipelineTask {
	sessionsMu.Lock()
	defer sessionsMu.Unlock()
	return sessions[roomName]
}

func removeSession(roomName string) {
	sessionsMu.Lock()
	defer sessionsMu.Unlock()
	delete(sessions, roomName)
}

func createSession() (string, *PipelineTask) {
	roomName := fmt.Sprintf("room-%d", rand.IntN(9000000)+1000000)
	logger := log.New(log.Writer(), fmt.Sprintf("[%s] ", roomName), log.Flags())
	ctx, cancel := context.WithCancel(context.Background())
	uiEvents := NewUIEventSender(logger)

	// task is created first (with zero-value WaitGroup) so taskCtx can
	// hold a pointer to it.
	task := &PipelineTask{Cancel: cancel, RoomName: roomName}

	metricsHandler := func(mf MetricsFrame) {
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
		Ctx:      ctx,
		Logger:   logger,
		UIEvents: uiEvents,
		Metrics:  metricsHandler,
		wg:       &task.wg,
	}
	task.TaskCtx = taskCtx

	pipelineSource := NewPipelineSourceProcessor(taskCtx)
	audioSource := NewAudioSourceProcessor(taskCtx)
	taskCtx.Room = joinRoom(roomName, audioSource)
	task.Source = pipelineSource

	sessionsMu.Lock()
	sessions[roomName] = task
	sessionsMu.Unlock()

	sttProcessor := NewSTTProcessor(taskCtx)
	userIdle := NewUserIdleProcessor(taskCtx)
	contextAggregator := NewContextAggregator(taskCtx)
	talkTimeMonitor := NewTalkTimeMonitoringProcessor(taskCtx)
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
	task.Pipeline.Start(ctx)
	return roomName, task
}

func (t *PipelineTask) End(reason string) {
	if reason == "" {
		reason = "unspecified"
	}
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
	t.TaskCtx.Logger.Printf("Queueing EndFrame: %s\n", reason)
	t.Source.Queue(EndFrame{Reason: reason})
}

func (t *PipelineTask) completeEnd(frame EndFrame) {
	t.cleanupOnce.Do(func() {
		t.endRequested.Store(true)
		t.cleanupStarted.Store(true)
		reason := frame.Reason
		if reason == "" {
			reason = "unspecified"
		}
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

		// Bounded wait for stragglers — 10s gives TTS's 8s pending-end timeout
		// room to fire before we hard-cancel.
		done := make(chan struct{})
		go func() { t.wg.Wait(); close(done) }()
		select {
		case <-done:
			t.TaskCtx.Logger.Println("All processor goroutines exited cleanly")
		case <-time.After(10 * time.Second):
			t.TaskCtx.Logger.Println("Timed out waiting 10s for processor goroutines")
		}

		t.Cancel()
		removeSession(t.RoomName)
		t.TaskCtx.Logger.Printf("PipelineTask cleanup complete after EndFrame: reason=%q\n", reason)
	})
}
