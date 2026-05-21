package main

import (
	"testing"
	"time"
)

// TestTalkTime_TimerEmitsShutdownSequence verifies the timer goroutine
// emits InterruptFrame, TTSSpeakFrame, EndFrame downstream when the
// max-talk-time elapses.
func TestTalkTime_TimerEmitsShutdownSequence(t *testing.T) {
	fix := newTestFixture(t)
	p := NewTalkTimeMonitoringProcessorWithMaxTalkTime(fix.TaskCtx, 100*time.Millisecond)

	// We want to capture all frames the timer pushes. The timer fires
	// 100ms after Start. Use a slightly larger settleDelay before the
	// explicit EndFrame so the timer's frames flow first.
	down, _ := runProcessorTest(t, fix, runConfig{
		processor:    p,
		framesToSend: []Frame{}, // nothing — timer drives the test
		settleDelay:  200 * time.Millisecond,
		sendEndFrame: false, // timer itself will emit EndFrame
		timeout:      3 * time.Second,
	})

	if c := countFrames[InterruptFrame](down); c != 1 {
		t.Errorf("expected 1 InterruptFrame downstream, got %d: %s", c, describeFrameTypes(down))
	}
	speakFrame, ok := findFrame[TTSSpeakFrame](down)
	if !ok {
		t.Errorf("expected TTSSpeakFrame downstream, got %s", describeFrameTypes(down))
	} else if speakFrame.Text == "" {
		t.Error("TTSSpeakFrame.Text should be non-empty")
	}
	endFrame, ok := findFrame[EndFrame](down)
	if !ok {
		t.Errorf("expected EndFrame downstream, got %s", describeFrameTypes(down))
	} else if endFrame.Reason != talkTimeExceededReason {
		t.Errorf("EndFrame.Reason: got %q, want %q", endFrame.Reason, talkTimeExceededReason)
	}
}

// TestTalkTime_PassesFramesThroughBeforeTimeout verifies that ordinary
// frames pass through before the timer fires.
func TestTalkTime_PassesFramesThroughBeforeTimeout(t *testing.T) {
	fix := newTestFixture(t)
	// Long timeout so it doesn't fire during the test.
	p := NewTalkTimeMonitoringProcessorWithMaxTalkTime(fix.TaskCtx, 60*time.Second)

	down, _ := runProcessorTest(t, fix, runConfig{
		processor: p,
		framesToSend: []Frame{
			TextFrame{Text: "passthrough"},
			TranscriptFrame{Text: "hello", IsFinal: true},
		},
		sendEndFrame: true,
	})

	if c := countFrames[TextFrame](down); c != 1 {
		t.Errorf("expected TextFrame to pass through, got %d", c)
	}
	if c := countFrames[TranscriptFrame](down); c != 1 {
		t.Errorf("expected TranscriptFrame to pass through, got %d", c)
	}
}

// TestTalkTime_DropsDownstreamFramesDuringShutdown verifies the ending
// flag causes new downstream frames to be dropped after shutdown is
// initiated.
func TestTalkTime_DropsDownstreamFramesDuringShutdown(t *testing.T) {
	fix := newTestFixture(t)
	p := NewTalkTimeMonitoringProcessorWithMaxTalkTime(fix.TaskCtx, 50*time.Millisecond)

	// Wait for timer to fire, then try to push a frame — it should be
	// dropped because ending is true.
	source := newQueueProcessor(fix.TaskCtx, "source", Upstream)
	sink := newQueueProcessor(fix.TaskCtx, "sink", Downstream)
	source.Link(p)
	p.Link(sink)
	source.Start(fix.RootCtx)
	p.Start(fix.RootCtx)
	sink.Start(fix.RootCtx)

	// Wait for timer to fire and shutdown sequence to emit.
	time.Sleep(150 * time.Millisecond)

	// Now ending is true. New downstream frames should be dropped.
	source.QueueFrame(TextFrame{Text: "late"}, Downstream)
	time.Sleep(50 * time.Millisecond)

	// Explicit Stop: TalkTime emitted EndFrame from its timer goroutine,
	// so source's base goroutines never see the EndFrame and won't
	// auto-cancel. This mirrors what PipelineTask.completeEnd does in
	// production via pipeline.Stop().
	source.Stop()
	p.Stop()
	sink.Stop()

	if err := waitForWG(fix.WG, 3*time.Second); err != nil {
		t.Fatalf("waitForWG: %v", err)
	}

	got := sink.Captured()
	// Should NOT contain the late TextFrame.
	for _, f := range got {
		if tf, ok := f.(TextFrame); ok && tf.Text == "late" {
			t.Error("late TextFrame should have been dropped during shutdown")
		}
	}
}
