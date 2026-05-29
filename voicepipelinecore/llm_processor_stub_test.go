package voicepipelinecore

import (
	"context"
	"testing"
	"time"
)

// stubLLMClient is a deterministic LLMClient for verifying the processor
// delegates streaming to its client and reports the client's model.
type stubLLMClient struct {
	tokens []string
	model  string
}

func (s *stubLLMClient) Stream(ctx context.Context, messages []map[string]string, onToken func(string)) (LLMResult, error) {
	for _, tok := range s.tokens {
		if ctx.Err() != nil {
			return LLMResult{Model: s.model, Interrupted: true}, ctx.Err()
		}
		onToken(tok)
	}
	return LLMResult{Model: s.model, TTFB: 10 * time.Millisecond, Total: 20 * time.Millisecond}, nil
}

// TestLLM_DelegatesToClientAndReportsModel verifies NewLLMProcessorWithClient
// streams the client's tokens downstream and emits an llm_call_result
// RTVI event carrying the real model the client reported.
func TestLLM_DelegatesToClientAndReportsModel(t *testing.T) {
	fix := newTestFixture(t)
	client := &stubLLMClient{tokens: []string{"foo ", "bar"}, model: "grok-4-1-fast-non-reasoning"}
	p := NewLLMProcessorWithClient(fix.TaskCtx, client)

	down, _ := runProcessorTest(t, fix, runConfig{
		processor: p,
		framesToSend: []Frame{
			LLMMessagesFrame{Messages: []map[string]string{{"role": "user", "content": "hi"}}},
		},
		settleDelay:  200 * time.Millisecond,
		sendEndFrame: true,
	})

	if _, ok := findFrame[LLMResponseStartFrame](down); !ok {
		t.Errorf("expected LLMResponseStartFrame, got %s", describeFrameTypes(down))
	}
	if c := countFrames[TextFrame](down); c != 2 {
		t.Errorf("expected 2 TextFrames, got %d in %s", c, describeFrameTypes(down))
	}
	if _, ok := findFrame[LLMResponseEndFrame](down); !ok {
		t.Errorf("expected LLMResponseEndFrame, got %s", describeFrameTypes(down))
	}

	var combined string
	for _, f := range down {
		if tf, ok := f.(TextFrame); ok {
			combined += tf.Text
		}
	}
	if combined != "foo bar" {
		t.Errorf("combined text: got %q, want %q", combined, "foo bar")
	}

	found := false
	for _, e := range fix.TaskCtx.UIEvents.Snapshot() {
		if e.Type != "server-message" {
			continue
		}
		data, ok := e.Data.(map[string]any)
		if ok && data["type"] == "llm_call_result" && data["model"] == "grok-4-1-fast-non-reasoning" && data["status"] == "completed" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected llm_call_result server-message with the client's model")
	}
}
