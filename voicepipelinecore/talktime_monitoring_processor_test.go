package voicepipelinecore

import (
	"testing"
	"time"
)

// TestTalkTime_TimerEmitsShutdownSequence verifies the timer goroutine
// emits InterruptFrame, TTSSpeakFrame, EndFrame downstream when the
// max-talk-time elapses. The timer no longer starts on Start — we
// inject an LLMMessagesFrame to simulate the first committed user turn
// and arm the countdown.
func TestTalkTime_TimerEmitsShutdownSequence(t *testing.T) {
	fix := newTestFixture(t)
	p := NewTalkTimeMonitoringProcessorWithMaxTalkTime(fix.TaskCtx, 100*time.Millisecond)

	down, _ := runProcessorTest(t, fix, runConfig{
		processor: p,
		framesToSend: []Frame{
			LLMMessagesFrame{Messages: []map[string]string{{"role": "user", "content": "hi"}}},
		},
		settleDelay:  250 * time.Millisecond,
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
	} else if endFrame.Reason != string(EndReasonTalkTimeExhausted) {
		t.Errorf("EndFrame.Reason: got %q, want %q", endFrame.Reason, EndReasonTalkTimeExhausted)
	}
}

func TestTalkTime_InitialContextDoesNotArmTimer(t *testing.T) {
	fix := newTestFixture(t)
	p := NewTalkTimeMonitoringProcessorWithMaxTalkTime(fix.TaskCtx, 50*time.Millisecond)

	down, _ := runProcessorTest(t, fix, runConfig{
		processor: p,
		framesToSend: []Frame{
			NewInitialLLMMessagesFrame([]map[string]string{{"role": "user", "content": "hello?"}}),
		},
		settleDelay:  150 * time.Millisecond,
		sendEndFrame: true,
		timeout:      3 * time.Second,
	})

	if c := countFrames[InterruptFrame](down); c != 0 {
		t.Errorf("expected no InterruptFrame for initial context, got %d: %s", c, describeFrameTypes(down))
	}
	if c := countFrames[TTSSpeakFrame](down); c != 0 {
		t.Errorf("expected no TTSSpeakFrame for initial context, got %d: %s", c, describeFrameTypes(down))
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

	source := newQueueProcessor(fix.TaskCtx, "source", Upstream)
	sink := newQueueProcessor(fix.TaskCtx, "sink", Downstream)
	source.Link(p)
	p.Link(sink)
	source.Start(fix.RootCtx)
	p.Start(fix.RootCtx)
	sink.Start(fix.RootCtx)

	// Arm the timer by simulating a committed user turn.
	source.QueueFrame(LLMMessagesFrame{Messages: []map[string]string{{"role": "user", "content": "hi"}}}, Downstream)

	// Wait for timer to fire and shutdown sequence to emit.
	time.Sleep(200 * time.Millisecond)

	// Now ending is true. New downstream frames should be dropped.
	source.QueueFrame(TextFrame{Text: "late"}, Downstream)
	time.Sleep(50 * time.Millisecond)

	// Explicit Stop: this test bypasses runProcessorTest, so taskCtx.EndTask
	// was never wired and TalkTime's call to it was a no-op (no EndFrame
	// reaches the pipeline). We still need to stop the chain explicitly,
	// mirroring what PipelineTask.completeEnd does in production via
	// pipeline.Stop().
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
