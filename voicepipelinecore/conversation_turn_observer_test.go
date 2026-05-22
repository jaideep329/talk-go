package voicepipelinecore

import (
	"sync"
	"testing"
	"time"
)

type recordingObserver struct {
	mu         sync.Mutex
	users      []string
	assistants []string
	metrics    []TurnMetrics
}

func (o *recordingObserver) OnUserTurnCommitted(text string, at time.Time) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.users = append(o.users, text)
}

func (o *recordingObserver) OnAssistantTurnCommitted(text string, at time.Time, metrics TurnMetrics) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.assistants = append(o.assistants, text)
	o.metrics = append(o.metrics, metrics)
}

func (o *recordingObserver) userCount() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return len(o.users)
}

func TestConversationTurnObserverSet_StopAndDrainProcessesQueuedEvents(t *testing.T) {
	fix := newTestFixture(t)
	obs := &recordingObserver{}
	set := newConversationTurnObserverSet(fix.TaskCtx, []ConversationTurnObserver{obs})

	for i := 0; i < 200; i++ {
		set.emitUserTurnCommitted("hello", time.Now())
	}
	set.stopAndDrain()

	if err := waitForWG(fix.WG, 2*time.Second); err != nil {
		t.Fatalf("waitForWG: %v", err)
	}
	if got := obs.userCount(); got != 200 {
		t.Fatalf("turn observer saw %d user events, want 200", got)
	}
}

type panicOnceObserver struct {
	count int
	mu    sync.Mutex
}

func (o *panicOnceObserver) OnUserTurnCommitted(text string, at time.Time) {
	o.mu.Lock()
	o.count++
	count := o.count
	o.mu.Unlock()
	if count == 1 {
		panic("first callback panic")
	}
}

func (o *panicOnceObserver) OnAssistantTurnCommitted(text string, at time.Time, metrics TurnMetrics) {
}

func TestConversationTurnObserverSet_RecoversAndContinuesAfterCallbackPanic(t *testing.T) {
	fix := newTestFixture(t)
	obs := &panicOnceObserver{}
	set := newConversationTurnObserverSet(fix.TaskCtx, []ConversationTurnObserver{obs})

	set.emitUserTurnCommitted("first", time.Now())
	set.emitUserTurnCommitted("second", time.Now())
	set.stopAndDrain()

	if err := waitForWG(fix.WG, 2*time.Second); err != nil {
		t.Fatalf("waitForWG: %v", err)
	}
	obs.mu.Lock()
	defer obs.mu.Unlock()
	if obs.count != 2 {
		t.Fatalf("turn observer count = %d, want 2", obs.count)
	}
}
