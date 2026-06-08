package voicepipelinecore

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
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
	ToolCalls   []ToolCall
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
	Stream(ctx context.Context, req LLMRequest, onToken func(string)) (LLMResult, error)
}

type ToolCallRequest struct {
	FunctionName string
	ToolCallID   string
	Arguments    map[string]any
	RawArguments string
}

type ToolCallResponse struct {
	Result any
	RunLLM bool
}

type ToolHandler func(ctx context.Context, req ToolCallRequest) (ToolCallResponse, error)

type ToolOptions struct {
	CancelOnInterruption bool
	Timeout              time.Duration
}

type registeredTool struct {
	definition ToolDefinition
	handler    ToolHandler
	options    ToolOptions
}

type LLMProcessor struct {
	*BaseProcessor
	taskCtx   *TaskContext
	client    LLMClient
	metrics   *ProcessorMetrics
	cancelMu  sync.Mutex
	cancelLLM context.CancelFunc

	tools      []ToolDefinition
	toolChoice any
	toolMap    map[string]registeredTool
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
		toolMap: make(map[string]registeredTool),
	}
	p.BaseProcessor = NewBaseProcessor("LLM", p, taskCtx)
	return p
}

func (p *LLMProcessor) RegisterTool(def ToolDefinition, handler ToolHandler, opts ToolOptions) {
	if def.Type == "" {
		def.Type = "function"
	}
	name := strings.TrimSpace(def.Function.Name)
	if name == "" {
		return
	}
	p.tools = appendOrReplaceToolDefinition(p.tools, def)
	p.toolMap[name] = registeredTool{definition: def, handler: handler, options: opts}
}

func (p *LLMProcessor) SetToolChoice(toolChoice any) {
	p.toolChoice = toolChoice
}

func appendOrReplaceToolDefinition(tools []ToolDefinition, def ToolDefinition) []ToolDefinition {
	for i, existing := range tools {
		if existing.Function.Name == def.Function.Name {
			tools[i] = def
			return tools
		}
	}
	return append(tools, def)
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

func (p *LLMProcessor) runLLM(ctx context.Context, messages []Message) {
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

	req := LLMRequest{
		Messages:   cloneMessages(messages),
		Tools:      append([]ToolDefinition(nil), p.tools...),
		ToolChoice: p.toolChoice,
	}
	result, err := p.client.Stream(ctx, req, onToken)
	if firstToken && result.TTFB > 0 {
		ttfbMs = float64(result.TTFB.Microseconds()) / 1000.0
		_ = p.metrics.Stop(MetricTTFB)
	}

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
	if len(result.ToolCalls) > 0 {
		p.executeToolCalls(ctx, result.ToolCalls)
	}
}

func (p *LLMProcessor) executeToolCalls(turnCtx context.Context, toolCalls []ToolCall) {
	toolResults := make([]FunctionCallResultFrame, 0, len(toolCalls))
	runNextLLM := false
	for _, call := range toolCalls {
		name := call.Function.Name
		tool, ok := p.toolMap[name]
		if !ok || tool.handler == nil {
			args, parseErr := parseToolArguments(call.Function.Arguments)
			if parseErr != nil {
				p.PushError(fmt.Sprintf("parse tool arguments for %q: %v", name, parseErr), false)
			}
			p.PushError(fmt.Sprintf("unregistered tool call %q", name), false)
			p.PushFrame(NewFunctionCallInProgressFrame(name, call.ID, args, call.Function.Arguments, true), Upstream)
			toolResults = append(toolResults, NewFunctionCallResultFrame(name, call.ID, args, call.Function.Arguments, toolResultString(map[string]any{"error": fmt.Sprintf("unregistered tool call %q", name)}), true))
			runNextLLM = true
			continue
		}
		args, parseErr := parseToolArguments(call.Function.Arguments)
		if parseErr != nil {
			p.PushError(fmt.Sprintf("parse tool arguments for %q: %v", name, parseErr), false)
		}
		p.PushFrame(NewFunctionCallInProgressFrame(name, call.ID, args, call.Function.Arguments, tool.options.CancelOnInterruption), Upstream)

		parent := turnCtx
		if !tool.options.CancelOnInterruption && p.taskCtx != nil && p.taskCtx.Ctx != nil {
			parent = p.taskCtx.Ctx
		}
		toolCtx := parent
		cancel := func() {}
		if tool.options.Timeout > 0 {
			toolCtx, cancel = context.WithTimeout(parent, tool.options.Timeout)
		}
		resp, err := tool.handler(toolCtx, ToolCallRequest{
			FunctionName: name,
			ToolCallID:   call.ID,
			Arguments:    args,
			RawArguments: call.Function.Arguments,
		})
		cancel()
		if err != nil {
			p.PushError(fmt.Sprintf("execute tool %q: %v", name, err), false)
			resp = ToolCallResponse{Result: map[string]any{"error": err.Error()}, RunLLM: false}
		}
		if resp.RunLLM {
			runNextLLM = true
		}
		toolResults = append(toolResults, NewFunctionCallResultFrame(name, call.ID, args, call.Function.Arguments, toolResultString(resp.Result), false))
	}
	for i, result := range toolResults {
		if i == len(toolResults)-1 && runNextLLM {
			result.RunLLM = true
		}
		p.PushFrame(result, Upstream)
	}
}

func parseToolArguments(raw string) (map[string]any, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return map[string]any{}, nil
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(raw), &args); err != nil {
		return map[string]any{}, err
	}
	if args == nil {
		args = map[string]any{}
	}
	return args, nil
}

func toolResultString(result any) string {
	switch v := result.(type) {
	case nil:
		return "COMPLETED"
	case string:
		if strings.TrimSpace(v) == "" {
			return "COMPLETED"
		}
		return v
	default:
		raw, err := json.Marshal(v)
		if err != nil {
			return fmt.Sprintf("%v", v)
		}
		return string(raw)
	}
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

func (c *openAILLMClient) Stream(ctx context.Context, llmReq LLMRequest, onToken func(string)) (LLMResult, error) {
	res := LLMResult{Model: llmModel}
	body := map[string]interface{}{
		"model":    llmModel,
		"stream":   true,
		"messages": llmReq.Messages,
	}
	if len(llmReq.Tools) > 0 {
		body["tools"] = llmReq.Tools
	}
	if llmReq.ToolChoice != nil {
		body["tool_choice"] = llmReq.ToolChoice
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
	toolCalls := newToolCallAccumulator()
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
					Content   string `json:"content"`
					ToolCalls []struct {
						Index    *int   `json:"index"`
						ID       string `json:"id"`
						Type     string `json:"type"`
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			continue
		}
		if len(chunk.Choices) == 0 {
			continue
		}
		delta := chunk.Choices[0].Delta
		if len(delta.ToolCalls) > 0 {
			if firstToken {
				firstToken = false
				res.TTFB = time.Since(start)
			}
			for _, tc := range delta.ToolCalls {
				idx := 0
				if tc.Index != nil {
					idx = *tc.Index
				}
				toolCalls.add(idx, ToolCall{
					ID:   tc.ID,
					Type: tc.Type,
					Function: ToolCallFunction{
						Name:      tc.Function.Name,
						Arguments: tc.Function.Arguments,
					},
				})
			}
		}
		content := delta.Content
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
	res.ToolCalls = toolCalls.list()
	return res, nil
}

type toolCallAccumulator struct {
	calls map[int]ToolCall
	order []int
}

func newToolCallAccumulator() *toolCallAccumulator {
	return &toolCallAccumulator{calls: make(map[int]ToolCall)}
}

func (a *toolCallAccumulator) add(index int, delta ToolCall) {
	if a == nil {
		return
	}
	call, ok := a.calls[index]
	if !ok {
		a.order = append(a.order, index)
		call.Type = "function"
	}
	if delta.ID != "" {
		call.ID = delta.ID
	}
	if delta.Type != "" {
		call.Type = delta.Type
	}
	call.Function.Name += delta.Function.Name
	call.Function.Arguments += delta.Function.Arguments
	a.calls[index] = call
}

func (a *toolCallAccumulator) list() []ToolCall {
	if a == nil || len(a.order) == 0 {
		return nil
	}
	out := make([]ToolCall, 0, len(a.order))
	for _, idx := range a.order {
		call := a.calls[idx]
		if call.Type == "" {
			call.Type = "function"
		}
		if call.Function.Name == "" && call.Function.Arguments == "" {
			continue
		}
		out = append(out, call)
	}
	return out
}
