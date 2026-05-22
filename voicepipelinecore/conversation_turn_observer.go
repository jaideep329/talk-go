package voicepipelinecore

import (
	"log"
	"sync"
	"time"
)

const conversationTurnObserverQueueSize = 256

type conversationTurnObserverTask func(ConversationTurnObserver)

type conversationTurnObserverWorker struct {
	observer ConversationTurnObserver
	queue    chan conversationTurnObserverTask
	done     chan struct{}
	logger   *log.Logger
}

type conversationTurnObserverSet struct {
	mu       sync.Mutex
	workers  []*conversationTurnObserverWorker
	closing  bool
	stopOnce sync.Once
	stopped  chan struct{}
	logger   *log.Logger
}

func newConversationTurnObserverSet(taskCtx *TaskContext, turnObservers []ConversationTurnObserver) *conversationTurnObserverSet {
	if len(turnObservers) == 0 {
		return nil
	}
	s := &conversationTurnObserverSet{
		workers: make([]*conversationTurnObserverWorker, 0, len(turnObservers)),
		stopped: make(chan struct{}),
	}
	if taskCtx != nil {
		s.logger = taskCtx.Logger
	}
	for _, obs := range turnObservers {
		if obs == nil {
			continue
		}
		w := &conversationTurnObserverWorker{
			observer: obs,
			queue:    make(chan conversationTurnObserverTask, conversationTurnObserverQueueSize),
			done:     make(chan struct{}),
			logger:   s.logger,
		}
		s.workers = append(s.workers, w)
		startConversationTurnObserverWorker(taskCtx, w)
	}
	if len(s.workers) == 0 {
		close(s.stopped)
		return nil
	}
	return s
}

func startConversationTurnObserverWorker(taskCtx *TaskContext, w *conversationTurnObserverWorker) {
	run := func() {
		defer close(w.done)
		for task := range w.queue {
			func() {
				defer func() {
					if r := recover(); r != nil && w.logger != nil {
						w.logger.Printf("turn observer callback panicked: %v\n", r)
					}
				}()
				task(w.observer)
			}()
		}
	}
	if taskCtx != nil && taskCtx.wg != nil {
		taskCtx.wg.Add(1)
		go func() {
			defer taskCtx.wg.Done()
			run()
		}()
		return
	}
	go run()
}

func (s *conversationTurnObserverSet) emitUserTurnCommitted(text string, at time.Time) {
	if s == nil {
		return
	}
	s.dispatch(func(obs ConversationTurnObserver) {
		obs.OnUserTurnCommitted(text, at)
	})
}

func (s *conversationTurnObserverSet) emitAssistantTurnCommitted(text string, at time.Time, metrics TurnMetrics) {
	if s == nil {
		return
	}
	s.dispatch(func(obs ConversationTurnObserver) {
		obs.OnAssistantTurnCommitted(text, at, metrics)
	})
}

func (s *conversationTurnObserverSet) dispatch(task conversationTurnObserverTask) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closing {
		return
	}
	for _, w := range s.workers {
		select {
		case w.queue <- task:
		default:
			if s.logger != nil {
				s.logger.Println("turn observer queue full; dropping turn observer event")
			}
		}
	}
}

func (s *conversationTurnObserverSet) stopAndDrain() {
	if s == nil {
		return
	}
	s.stopOnce.Do(func() {
		s.mu.Lock()
		s.closing = true
		workers := append([]*conversationTurnObserverWorker(nil), s.workers...)
		for _, w := range workers {
			close(w.queue)
		}
		s.mu.Unlock()

		for _, w := range workers {
			<-w.done
		}
		close(s.stopped)
	})
	<-s.stopped
}
