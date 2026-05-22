package voicepipelinecore

import (
	"testing"
	"time"
)

// STT/TTS unit tests focus on ProcessFrame routing behaviour. The
// websocket connect goroutines run in the background but fail to dial
// (the test_setup_test.go init redirects sttDialURL/ttsDialURL to an
// unreachable URL). They exit cleanly when the test fixture cancels
// taskCtx.Ctx.
//
// Tests that need to exercise actual STT transcription or TTS
// synthesis flow are integration tests (out of scope here).

// TestSTT_ForwardsEndFrame verifies EndFrame propagates downstream and
// stops the STT processor.
func TestSTT_ForwardsEndFrame(t *testing.T) {
	fix := newTestFixture(t)
	p := NewSTTProcessor(fix.TaskCtx)

	down, _ := runProcessorTest(t, fix, runConfig{
		processor:    p,
		framesToSend: []Frame{},
		sendEndFrame: true,
		timeout:      3 * time.Second,
	})

	// EndFrame is stripped from down by the helper; we just need the
	// test to complete without timing out.
	if len(down) != 0 {
		t.Errorf("expected no downstream frames besides EndFrame, got %s", describeFrameTypes(down))
	}
}

// TestSTT_QueuesAudioFrameToWriter verifies AudioFrames are forwarded
// to the internal writer channel (which would write them to the
// Soniox websocket if connected).
func TestSTT_QueuesAudioFrameToWriter(t *testing.T) {
	fix := newTestFixture(t)
	p := NewSTTProcessor(fix.TaskCtx)

	// Send an AudioFrame; STT should consume it (not forward downstream).
	down, _ := runProcessorTest(t, fix, runConfig{
		processor: p,
		framesToSend: []Frame{
			AudioFrame{Data: []byte{0, 0, 0, 0}},
		},
		settleDelay:  50 * time.Millisecond,
		sendEndFrame: true,
	})

	// AudioFrame should NOT appear downstream — STT consumes it.
	if c := countFrames[AudioFrame](down); c != 0 {
		t.Errorf("AudioFrame should be consumed by STT, not forwarded; got %d downstream", c)
	}
}

// TestSTT_PassesThroughOtherFrames verifies frames STT doesn't
// specifically handle pass through.
func TestSTT_PassesThroughOtherFrames(t *testing.T) {
	fix := newTestFixture(t)
	p := NewSTTProcessor(fix.TaskCtx)

	down, _ := runProcessorTest(t, fix, runConfig{
		processor: p,
		framesToSend: []Frame{
			TextFrame{Text: "passthrough"},
		},
		sendEndFrame: true,
	})

	if c := countFrames[TextFrame](down); c != 1 {
		t.Errorf("expected TextFrame to pass through, got %d in %s", c, describeFrameTypes(down))
	}
}
