package main

import (
	"context"
	"fmt"
	"log"
	"math/rand/v2"
	"sync"
	"sync/atomic"

	lksdk "github.com/livekit/server-sdk-go/v2"
)

type Pipeline struct {
	processors     []FrameProcessor
	metricsHandler func(MetricsFrame)
}

func NewPipeline(processors []FrameProcessor, metricsHandler func(MetricsFrame)) *Pipeline {
	return &Pipeline{processors: processors, metricsHandler: metricsHandler}
}

func (p *Pipeline) Run() {
	n := len(p.processors)

	dataChs := make([]chan Frame, n)
	sysChs := make([]chan Frame, n)
	for i := range n {
		dataChs[i] = make(chan Frame, 100)
		sysChs[i] = make(chan Frame, 100)
	}

	// Build a Send function for each processor.
	// Downstream: route to next processor's data or system channel based on IsSystem().
	// Upstream: route to previous processor's data channel.
	sendFn := func(i int) func(Frame, Direction) {
		return func(frame Frame, dir Direction) {
			// Intercept MetricsFrames — route to handler, don't forward.
			if mf, ok := frame.(MetricsFrame); ok {
				if p.metricsHandler != nil {
					p.metricsHandler(mf)
				}
				return
			}
			switch dir {
			case Downstream:
				if i+1 < n {
					if frame.IsSystem() {
						sysChs[i+1] <- frame
					} else {
						dataChs[i+1] <- frame
					}
				}
			case Upstream:
				if i-1 >= 0 {
					dataChs[i-1] <- frame
				}
			}
		}
	}

	for i, processor := range p.processors {
		go processor.Process(ProcessorChannels{
			Data:   dataChs[i],
			System: sysChs[i],
			Send:   sendFn(i),
		})
	}
}

// TaskContext is the single dependency passed to all processors in a PipelineTask.
// Ctx is the root cancellation context for the task.
// Metrics is the handler invoked when a MetricsFrame is emitted by any processor.
// wg is the shared WaitGroup used by BaseProcessor.Go() to track goroutines.
type TaskContext struct {
	Ctx      context.Context
	Logger   *log.Logger
	Room     *lksdk.Room
	UIEvents *UIEventSender
	Metrics  func(MetricsFrame)
	wg       *sync.WaitGroup
}

// PipelineTask is the lifecycle owner of a single voice-call pipeline.
// It is the Go equivalent of Pipecat's PipelineTask: it manages the chain,
// queues lifecycle frames at the source, and runs cleanup when EndFrame
// reaches the sink.
type PipelineTask struct {
	TaskCtx        *TaskContext
	Cancel         context.CancelFunc
	Source         *PipelineSourceProcessor
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

	// task is created first (with a zero-value WaitGroup) so taskCtx can hold a pointer to it.
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

	pipelineSource := NewPipelineSourceProcessor(logger)
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
	pipelineSink := NewPipelineSinkProcessor(logger, task.completeEnd)

	pipeline := NewPipeline([]FrameProcessor{pipelineSource, audioSource, sttProcessor, userIdle, contextAggregator, talkTimeMonitor, llmProcessor, ttsProcessor, playbackSink, pipelineSink}, metricsHandler)
	go pipeline.Run()
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
		t.Cancel()
		if t.TaskCtx.Room != nil {
			t.TaskCtx.Room.Disconnect()
		}
		removeSession(t.RoomName)
		t.TaskCtx.Logger.Printf("PipelineTask cleanup complete after EndFrame: reason=%q\n", reason)
	})
}
