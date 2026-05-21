package main

import (
	"testing"
)

// TestAudioSource_ForwardsEndFrame verifies AudioSourceProcessor's
// ProcessFrame forwards EndFrame downstream. We don't test the actual
// RTP reading path here (would require a webrtc.TrackRemote stub) —
// that's covered by integration testing.
func TestAudioSource_ForwardsEndFrame(t *testing.T) {
	fix := newTestFixture(t)
	a := NewAudioSourceProcessor(fix.TaskCtx)

	down, _ := runProcessorTest(t, fix, runConfig{
		processor:    a,
		framesToSend: []Frame{},
		sendEndFrame: true,
	})

	// EndFrame is stripped from `down` by runProcessorTest, so down
	// should be empty. The fact that the test reached this point without
	// timing out means the EndFrame propagated correctly.
	if len(down) != 0 {
		t.Errorf("expected no downstream frames besides EndFrame, got %s", describeFrameTypes(down))
	}
}

// TestAudioSource_PassesThroughUnknownFrames verifies the default-forward
// behaviour for unhandled frame types.
func TestAudioSource_PassesThroughUnknownFrames(t *testing.T) {
	fix := newTestFixture(t)
	a := NewAudioSourceProcessor(fix.TaskCtx)

	down, _ := runProcessorTest(t, fix, runConfig{
		processor: a,
		framesToSend: []Frame{
			TextFrame{Text: "pass through"},
		},
		sendEndFrame: true,
	})

	if c := countFrames[TextFrame](down); c != 1 {
		t.Errorf("expected TextFrame to pass through, got %d in: %s", c, describeFrameTypes(down))
	}
}
