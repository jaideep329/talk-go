package voicepipelinecore

import (
	"context"
	"io"
	"log"
	"testing"
	"time"
)

func TestPipelineTaskRunCleanupCallsOnCallEndedWithStats(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	logger := log.New(io.Discard, "", 0)
	callStats := newCallStatsTracker()
	start := time.Now().Add(-3 * time.Second)
	callStats.MarkUserJoined(start)
	callStats.MarkFirstUserAudio(start.Add(time.Second))

	got := make(chan struct {
		reason EndReason
		stats  CallStats
	}, 1)
	task := &PipelineTask{
		TaskCtx: &TaskContext{
			Ctx:      ctx,
			Logger:   logger,
			UIEvents: NewUIEventSender(logger),
		},
		Cancel:    cancel,
		Pipeline:  NewPipeline(nil),
		callStats: callStats,
		onCallEnded: func(reason EndReason, stats CallStats) {
			got <- struct {
				reason EndReason
				stats  CallStats
			}{reason: reason, stats: stats}
		},
	}

	task.runCleanup(NewEndFrame(string(EndReasonClientDisconnect)))

	select {
	case call := <-got:
		if call.reason != EndReasonClientDisconnect {
			t.Fatalf("reason = %q, want %q", call.reason, EndReasonClientDisconnect)
		}
		if call.stats.TotalUserDurationSec <= 0 {
			t.Fatalf("total user duration should be positive, got %.3f", call.stats.TotalUserDurationSec)
		}
		if call.stats.FirstUserAudioFrameAt.IsZero() {
			t.Fatal("first user audio timestamp should be present")
		}
		if call.stats.EndedAt.IsZero() {
			t.Fatal("ended_at should be present")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("OnCallEnded was not called")
	}
}
