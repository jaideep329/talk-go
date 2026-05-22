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

type LLMProcessor struct {
	*BaseProcessor
	taskCtx   *TaskContext
	metrics   *ProcessorMetrics
	cancelMu  sync.Mutex
	cancelLLM context.CancelFunc
}

func NewLLMProcessor(taskCtx *TaskContext) *LLMProcessor {
	p := &LLMProcessor{
		taskCtx: taskCtx,
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
	body := map[string]interface{}{
		"model":    "gpt-4.1",
		"stream":   true,
		"messages": messages,
	}
	jsonBody, err := json.Marshal(body)
	if err != nil {
		p.taskCtx.Logger.Println("json marshal error:", err)
		return
	}

	req, err := http.NewRequestWithContext(ctx, "POST", llmEndpoint, bytes.NewReader(jsonBody))
	if err != nil {
		p.taskCtx.Logger.Println("request error:", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+os.Getenv("OPENAI_API_KEY"))

	p.metrics.Start(MetricTTFB)
	p.metrics.Start(MetricProcessing)
	p.PushFrame(NewLLMResponseStartFrame(time.Now()), Downstream)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		if ctx.Err() == nil {
			p.taskCtx.Logger.Println("LLM request failed:", err)
		}
		return
	}
	defer resp.Body.Close()

	firstToken := true
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		if ctx.Err() != nil {
			return
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
			if mf := p.metrics.Stop(MetricTTFB); mf != nil {
				p.PushFrame(*mf, Downstream)
			}
		}
		p.PushFrame(NewTextFrame(content), Downstream)
	}
	if ctx.Err() == nil {
		if mf := p.metrics.Stop(MetricProcessing); mf != nil {
			p.PushFrame(*mf, Downstream)
		}
		p.PushFrame(NewLLMResponseEndFrame(), Downstream)
	}
}
