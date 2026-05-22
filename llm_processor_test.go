package main

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// stubOpenAIServer returns an httptest.Server that responds to a
// chat-completions POST with an SSE stream of the given content
// chunks. blockUntil (if non-nil) blocks the response until it's
// closed/sent on, simulating a slow LLM.
func stubOpenAIServer(t *testing.T, chunks []string, blockUntil <-chan struct{}) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)

		writeChunk := func(content string) {
			payload := fmt.Sprintf(`{"choices":[{"delta":{"content":%q}}]}`, content)
			fmt.Fprintf(w, "data: %s\n\n", payload)
			if flusher != nil {
				flusher.Flush()
			}
		}

		for i, c := range chunks {
			if i == 1 && blockUntil != nil {
				select {
				case <-blockUntil:
				case <-r.Context().Done():
					return
				}
			}
			writeChunk(c)
			select {
			case <-r.Context().Done():
				return
			default:
			}
		}
		io.WriteString(w, "data: [DONE]\n\n")
		if flusher != nil {
			flusher.Flush()
		}
	}))
	return s
}

// withLLMEndpoint sets llmEndpoint for the duration of the test.
func withLLMEndpoint(t *testing.T, url string) {
	t.Helper()
	old := llmEndpoint
	llmEndpoint = url
	t.Cleanup(func() { llmEndpoint = old })
}

// TestLLM_StreamsTextFramesDownstream verifies a complete LLM run
// produces LLMResponseStart → TextFrames → LLMResponseEnd downstream.
func TestLLM_StreamsTextFramesDownstream(t *testing.T) {
	server := stubOpenAIServer(t, []string{"hello ", "world"}, nil)
	defer server.Close()
	withLLMEndpoint(t, server.URL)

	fix := newTestFixture(t)
	p := NewLLMProcessor(fix.TaskCtx)

	down, _ := runProcessorTest(t, fix, runConfig{
		processor: p,
		framesToSend: []Frame{
			LLMMessagesFrame{Messages: []map[string]string{
				{"role": "user", "content": "hi"},
			}},
		},
		settleDelay:  200 * time.Millisecond,
		sendEndFrame: true,
		timeout:      3 * time.Second,
	})

	if _, ok := findFrame[LLMResponseStartFrame](down); !ok {
		t.Errorf("expected LLMResponseStartFrame, got %s", describeFrameTypes(down))
	}
	if c := countFrames[TextFrame](down); c != 2 {
		t.Errorf("expected 2 TextFrames (one per chunk), got %d in %s", c, describeFrameTypes(down))
	}
	if _, ok := findFrame[LLMResponseEndFrame](down); !ok {
		t.Errorf("expected LLMResponseEndFrame, got %s", describeFrameTypes(down))
	}

	// Verify TextFrame contents.
	var combined string
	for _, f := range down {
		if tf, ok := f.(TextFrame); ok {
			combined += tf.Text
		}
	}
	if combined != "hello world" {
		t.Errorf("combined text: got %q, want %q", combined, "hello world")
	}
}

// TestLLM_InterruptCancelsHTTP verifies an InterruptFrame cancels the
// in-flight HTTP request and stops further TextFrames from being
// emitted.
func TestLLM_InterruptCancelsHTTP(t *testing.T) {
	// Server emits one chunk, then waits for blockUntil before emitting
	// the second. The test interrupts in between.
	blockUntil := make(chan struct{})
	var serverClosed sync.Once
	defer serverClosed.Do(func() { close(blockUntil) })

	server := stubOpenAIServer(t, []string{"first ", "second"}, blockUntil)
	defer server.Close()
	withLLMEndpoint(t, server.URL)

	fix := newTestFixture(t)
	p := NewLLMProcessor(fix.TaskCtx)

	source := newQueueProcessor(fix.TaskCtx, "source", Upstream)
	sink := newQueueProcessor(fix.TaskCtx, "sink", Downstream)
	source.Link(p)
	p.Link(sink)
	source.Start(fix.RootCtx)
	p.Start(fix.RootCtx)
	sink.Start(fix.RootCtx)

	source.QueueFrame(LLMMessagesFrame{Messages: []map[string]string{{"role": "user", "content": "hi"}}}, Downstream)

	// Wait for the first chunk to arrive.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		hasText := false
		for _, f := range sink.Captured() {
			if _, ok := f.(TextFrame); ok {
				hasText = true
				break
			}
		}
		if hasText {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Send InterruptFrame; it should cancel the in-flight HTTP request.
	source.QueueFrame(InterruptFrame{}, Downstream)
	time.Sleep(100 * time.Millisecond)

	// Now unblock the server. Any second chunk it emits should NOT reach
	// the sink because the HTTP request context was cancelled.
	serverClosed.Do(func() { close(blockUntil) })
	time.Sleep(100 * time.Millisecond)

	// Clean shutdown.
	source.QueueFrame(EndFrame{}, Downstream)
	if err := waitForWG(fix.WG, 3*time.Second); err != nil {
		t.Fatalf("waitForWG: %v", err)
	}

	got := sink.Captured()
	textCount := countFrames[TextFrame](got)
	if textCount > 1 {
		t.Errorf("expected at most 1 TextFrame (the first chunk before interrupt), got %d in %s", textCount, describeFrameTypes(got))
	}
	if c := countFrames[InterruptFrame](got); c != 1 {
		t.Errorf("expected InterruptFrame forwarded, got %d", c)
	}
}

// TestLLM_EndFrameCancelsInFlight verifies EndFrame cancels the
// in-flight LLM request via the stored cancel func.
func TestLLM_EndFrameCancelsInFlight(t *testing.T) {
	blockUntil := make(chan struct{})
	server := stubOpenAIServer(t, []string{"first ", "second"}, blockUntil)
	defer server.Close()
	defer close(blockUntil)
	withLLMEndpoint(t, server.URL)

	fix := newTestFixture(t)
	p := NewLLMProcessor(fix.TaskCtx)

	source := newQueueProcessor(fix.TaskCtx, "source", Upstream)
	sink := newQueueProcessor(fix.TaskCtx, "sink", Downstream)
	source.Link(p)
	p.Link(sink)
	source.Start(fix.RootCtx)
	p.Start(fix.RootCtx)
	sink.Start(fix.RootCtx)

	source.QueueFrame(LLMMessagesFrame{Messages: []map[string]string{{"role": "user", "content": "hi"}}}, Downstream)

	// Wait for the first chunk.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		found := false
		for _, f := range sink.Captured() {
			if _, ok := f.(TextFrame); ok {
				found = true
				break
			}
		}
		if found {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	// EndFrame should cancel the in-flight request.
	source.QueueFrame(EndFrame{Reason: "test"}, Downstream)

	if err := waitForWG(fix.WG, 3*time.Second); err != nil {
		t.Fatalf("waitForWG: %v", err)
	}

	// Verify EndFrame reached the sink.
	got := sink.Captured()
	if c := countFrames[EndFrame](got); c == 0 {
		t.Errorf("expected EndFrame forwarded, got %s", describeFrameTypes(got))
	}
}

// TestLLM_PassesThroughOtherFrames verifies the default-forward
// behaviour for frames LLM doesn't specifically handle (e.g., upstream
// frames going back to ContextAggregator).
func TestLLM_PassesThroughOtherFrames(t *testing.T) {
	withLLMEndpoint(t, "http://127.0.0.1:0/never-called")

	fix := newTestFixture(t)
	p := NewLLMProcessor(fix.TaskCtx)

	// Push an upstream frame; should reach the source.
	source := newQueueProcessor(fix.TaskCtx, "source", Upstream)
	sink := newQueueProcessor(fix.TaskCtx, "sink", Downstream)
	source.Link(p)
	p.Link(sink)
	source.Start(fix.RootCtx)
	p.Start(fix.RootCtx)
	sink.Start(fix.RootCtx)

	// Simulate downstream-of-LLM pushing TTSDoneFrame upstream.
	sink.QueueFrame(TTSDoneFrame{}, Upstream)
	time.Sleep(50 * time.Millisecond)

	source.QueueFrame(EndFrame{}, Downstream)
	if err := waitForWG(fix.WG, 3*time.Second); err != nil {
		t.Fatalf("waitForWG: %v", err)
	}

	if c := countFrames[TTSDoneFrame](source.Captured()); c != 1 {
		t.Errorf("expected TTSDoneFrame to pass through upstream, got %d", c)
	}
}

// stubLogger satisfies the io.Writer interface to silence LLM logs
// during the error-path test.
type stubLogger struct{}

func (stubLogger) Write(p []byte) (int, error) { return len(p), nil }

// TestLLM_HandlesServerError verifies LLM handles a failing HTTP
// response gracefully (no panic, just no downstream tokens).
func TestLLM_HandlesServerError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		io.WriteString(w, "{\"error\":\"boom\"}")
	}))
	defer server.Close()
	withLLMEndpoint(t, server.URL)

	fix := newTestFixture(t)
	p := NewLLMProcessor(fix.TaskCtx)

	down, _ := runProcessorTest(t, fix, runConfig{
		processor: p,
		framesToSend: []Frame{
			LLMMessagesFrame{Messages: []map[string]string{{"role": "user", "content": "hi"}}},
		},
		settleDelay:  200 * time.Millisecond,
		sendEndFrame: true,
	})

	// LLMResponseStart still goes out before the request fails.
	if _, ok := findFrame[LLMResponseStartFrame](down); !ok {
		t.Errorf("expected LLMResponseStartFrame even on error, got %s", describeFrameTypes(down))
	}
	if c := countFrames[TextFrame](down); c != 0 {
		t.Errorf("expected no TextFrames on error response, got %d", c)
	}

	// Verify TextFrame logic doesn't leak: response shouldn't contain
	// parsed content from the error body.
	for _, f := range down {
		if tf, ok := f.(TextFrame); ok {
			if strings.Contains(tf.Text, "boom") {
				t.Errorf("error body leaked into TextFrame: %q", tf.Text)
			}
		}
	}
}

// Suppress unused-warning for stubLogger when not invoked.
var _ = stubLogger{}
