package voicepipelinecore

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// llmEndpoint is the OpenAI Chat Completions URL. Exposed as a package
// variable so tests can override it to point at an httptest server.
var llmEndpoint = "https://api.openai.com/v1/chat/completions"

const llmModel = "gpt-4.1"

// LLMResult summarizes a single streaming completion. Implementations
// fill Model with the model that actually served the request (which may
// differ per call when a router selects among endpoints). TTFB/Total
// are best-effort timings and Interrupted reports whether the stream
// ended because its context was cancelled.
type LLMResult struct {
	Model       string
	TTFB        time.Duration
	Total       time.Duration
	Interrupted bool
}

// LLMClient performs one streaming chat completion. It must invoke
// onToken for every non-empty text delta as it arrives, and return the
// model that served the request. The implementation owns provider and
// transport concerns (endpoint selection, auth, health write-backs);
// the LLMProcessor owns pipeline framing, metrics, and cancellation.
//
// Inputs are plain stdlib types; the only core type in the contract is
// the small LLMResult return value, so an implementer (e.g. the
// llmrouter package) imports voicepipelinecore just for that. The
// dependency runs one way — the core never imports the implementer — so
// the pipeline package stays free of any business/transport dependency.
type LLMClient interface {
	Stream(ctx context.Context, messages []map[string]string, onToken func(string)) (LLMResult, error)
}

type LLMProcessor struct {
	*BaseProcessor
	taskCtx   *TaskContext
	client    LLMClient
	metrics   *ProcessorMetrics
	cancelMu  sync.Mutex
	cancelLLM context.CancelFunc
}

// NewLLMProcessor builds an LLM processor backed by the default OpenAI
// gpt-4.1 client. Used by the local /connect demo path.
func NewLLMProcessor(taskCtx *TaskContext) *LLMProcessor {
	return NewLLMProcessorWithClient(taskCtx, &openAILLMClient{})
}

// NewLLMProcessorWithClient builds an LLM processor backed by a custom
// client (e.g. the health-based llmrouter for sales calls).
func NewLLMProcessorWithClient(taskCtx *TaskContext, client LLMClient) *LLMProcessor {
	if client == nil {
		client = &openAILLMClient{}
	}
	p := &LLMProcessor{
		taskCtx: taskCtx,
		client:  client,
		metrics: NewProcessorMetrics("llm"),
	}
	p.BaseProcessor = NewBaseProcessor("LLM", p, taskCtx)
	return p
}

func (p *LLMProcessor) ProcessFrame(ctx context.Context, frame Frame, dir Direction) {
	switch f := frame.(type) {
	case EndFrame:
		p.taskCtx.Logger.Printf("EndFrame at LLMProcessor, cancelling LLM: reason=%q\n", f.Reason)
		p.cancelInFlight()
		p.metrics.Reset()
		p.PushFrame(f, dir)
	case LLMMessagesFrame:
		runCtx, cancel := context.WithCancel(ctx)
		p.cancelMu.Lock()
		if p.cancelLLM != nil {
			p.cancelLLM()
		}
		p.cancelLLM = cancel
		p.cancelMu.Unlock()
		p.Go(func() { p.runLLM(runCtx, f.Messages) })
	case InterruptFrame:
		// Base has already cancelled the previous procCtx, which cancels any
		// in-flight runLLM transitively. Clearing the stored cancel func is
		// just bookkeeping.
		p.cancelInFlight()
		p.metrics.Reset()
		p.PushFrame(frame, dir)
	default:
		p.PushFrame(frame, dir)
	}
}

func (p *LLMProcessor) cancelInFlight() {
	p.cancelMu.Lock()
	defer p.cancelMu.Unlock()
	if p.cancelLLM != nil {
		p.cancelLLM()
		p.cancelLLM = nil
	}
}

func (p *LLMProcessor) runLLM(ctx context.Context, messages []map[string]string) {
	p.metrics.Start(MetricTTFB)
	p.metrics.Start(MetricProcessing)
	p.PushFrame(NewLLMResponseStartFrame(time.Now()), Downstream)

	var ttfbMs float64
	firstToken := true
	onToken := func(content string) {
		if content == "" {
			return
		}
		if firstToken {
			firstToken = false
			if mf := p.metrics.Stop(MetricTTFB); mf != nil {
				ttfbMs = mf.Data[0].ValueMs
				p.PushFrame(*mf, Downstream)
			}
		}
		p.PushFrame(NewTextFrame(content), Downstream)
	}

	result, err := p.client.Stream(ctx, messages, onToken)

	// Cancellation (barge-in / EndFrame) takes precedence: the interrupt/
	// end path already reset TTS + playback, so we must NOT emit terminal
	// frames here (they'd be processed after the reset).
	if ctx.Err() != nil {
		p.emitLLMCallResult(result.Model, ttfbMs, 0, "interrupted")
		return
	}
	if err != nil {
		// Live endpoint error (the router has already blacklisted +
		// triggered a re-poll). The turn fails but the call continues
		// (Python parity: the next turn re-selects). Close the turn so
		// the pipeline returns to a terminal state — LLMResponseEndFrame
		// makes TTS flush any partial text and emit TTSDoneFrame, which
		// PlaybackSink turns into BotStoppedSpeaking so UserIdleProcessor
		// arms its prompt loop. Otherwise the caller sits in silence
		// until the 120s watchdog.
		p.taskCtx.Logger.Println("LLM stream failed:", err)
		p.PushFrame(NewLLMResponseEndFrame(), Downstream)
		p.emitLLMCallResult(result.Model, ttfbMs, 0, "interrupted")
		return
	}

	var totalMs float64
	if mf := p.metrics.Stop(MetricProcessing); mf != nil {
		totalMs = mf.Data[0].ValueMs
		p.PushFrame(*mf, Downstream)
	}
	p.PushFrame(NewLLMResponseEndFrame(), Downstream)
	p.emitLLMCallResult(result.Model, ttfbMs, totalMs, "completed")
}

// emitLLMCallResult publishes the Python-compatible RTVI server-message
// reporting which model served the turn and its latency. Mirrors
// CustomOpenAILLMService.on_llm_call_complete -> rtvi llm_call_result.
func (p *LLMProcessor) emitLLMCallResult(model string, ttfbMs, totalMs float64, status string) {
	if p.taskCtx == nil || p.taskCtx.UIEvents == nil {
		return
	}
	data := map[string]any{
		"type":   "llm_call_result",
		"status": status,
		"model":  model,
	}
	if ttfbMs > 0 {
		data["ttfb_ms"] = ttfbMs
	}
	if totalMs > 0 {
		data["total_ms"] = totalMs
	}
	p.taskCtx.UIEvents.ServerMessage(data, time.Now())
}

// openAILLMClient is the default LLMClient: a single OpenAI gpt-4.1
// streaming call using OPENAI_API_KEY. It preserves the original
// pre-router behavior for the local /connect demo.
type openAILLMClient struct{}

func (c *openAILLMClient) Stream(ctx context.Context, messages []map[string]string, onToken func(string)) (LLMResult, error) {
	res := LLMResult{Model: llmModel}
	body := map[string]interface{}{
		"model":    llmModel,
		"stream":   true,
		"messages": messages,
	}
	jsonBody, err := json.Marshal(body)
	if err != nil {
		return res, err
	}

	req, err := http.NewRequestWithContext(ctx, "POST", llmEndpoint, bytes.NewReader(jsonBody))
	if err != nil {
		return res, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+os.Getenv("OPENAI_API_KEY"))

	start := time.Now()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		res.Total = time.Since(start)
		res.Interrupted = ctx.Err() != nil
		return res, err
	}
	defer resp.Body.Close()

	firstToken := true
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		if ctx.Err() != nil {
			res.Total = time.Since(start)
			res.Interrupted = true
			return res, ctx.Err()
		}
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			break
		}

		var chunk struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			continue
		}
		if len(chunk.Choices) == 0 {
			continue
		}
		content := chunk.Choices[0].Delta.Content
		if content == "" {
			continue
		}
		if firstToken {
			firstToken = false
			res.TTFB = time.Since(start)
		}
		onToken(content)
	}
	res.Total = time.Since(start)
	res.Interrupted = ctx.Err() != nil
	return res, nil
}
