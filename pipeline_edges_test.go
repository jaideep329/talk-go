package main

import (
	"testing"
	"time"
)

// TestPipelineSource_QueueForwardsDownstream verifies an external frame
// queued via Queue() is forwarded downstream.
func TestPipelineSource_QueueForwardsDownstream(t *testing.T) {
	fix := newTestFixture(t)
	p := NewPipelineSourceProcessor(fix.TaskCtx)

	sink := newQueueProcessor(fix.TaskCtx, "sink", Downstream)
	p.Link(sink)
	p.Start(fix.RootCtx)
	sink.Start(fix.RootCtx)

	p.Queue(EndFrame{Reason: "test"})

	if err := waitForWG(fix.WG, 3*time.Second); err != nil {
		t.Fatalf("waitForWG: %v", err)
	}

	got := sink.Captured()
	endFrame, ok := findFrame[EndFrame](got)
	if !ok {
		t.Fatalf("expected EndFrame downstream, got %s", describeFrameTypes(got))
	}
	if endFrame.Reason != "test" {
		t.Errorf("EndFrame.Reason: got %q, want 'test'", endFrame.Reason)
	}
}

// TestPipelineSink_CallsOnEndCallback verifies the onEnd callback fires
// when an EndFrame arrives.
func TestPipelineSink_CallsOnEndCallback(t *testing.T) {
	fix := newTestFixture(t)

	called := make(chan EndFrame, 1)
	p := NewPipelineSinkProcessor(fix.TaskCtx, func(f EndFrame) {
		called <- f
	})

	p.Start(fix.RootCtx)
	p.QueueFrame(EndFrame{Reason: "test-reason"}, Downstream)

	select {
	case f := <-called:
		if f.Reason != "test-reason" {
			t.Errorf("EndFrame.Reason: got %q, want 'test-reason'", f.Reason)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("onEnd was not called within 2s")
	}

	if err := waitForWG(fix.WG, 2*time.Second); err != nil {
		t.Fatalf("waitForWG: %v", err)
	}
}

// TestPipelineSink_IgnoresNonEndFrames verifies non-EndFrame frames are
// silently terminated (do not fire onEnd).
func TestPipelineSink_IgnoresNonEndFrames(t *testing.T) {
	fix := newTestFixture(t)

	called := make(chan EndFrame, 1)
	p := NewPipelineSinkProcessor(fix.TaskCtx, func(f EndFrame) {
		called <- f
	})

	p.Start(fix.RootCtx)
	p.QueueFrame(TextFrame{Text: "ignored"}, Downstream)
	p.QueueFrame(TranscriptFrame{Text: "also ignored"}, Downstream)
	time.Sleep(50 * time.Millisecond)

	select {
	case <-called:
		t.Error("onEnd should not have fired for non-EndFrame frames")
	default:
	}

	// Now actually shut down with an EndFrame.
	p.QueueFrame(EndFrame{Reason: "shutdown"}, Downstream)
	if err := waitForWG(fix.WG, 2*time.Second); err != nil {
		t.Fatalf("waitForWG: %v", err)
	}
}
