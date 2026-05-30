package disha

import (
	"context"
	"encoding/json"
	"log"
	"time"

	"github.com/jaideep329/talk-go/voicepipelinecore/llmrouter"
)

const (
	// llmLogModule/llmLogFunc target Disha's existing llm_logging_service
	// singleton via the worker dispatch (module.instance.method), so no
	// new Python code is needed. The service uploads the payload to S3
	// and enqueues the DB save itself.
	llmLogModule = "services.llm_logging_service"
	llmLogFunc   = "llm_logging_service.log_llm_call_async"
	llmLogQueue  = "p1-fast-l1"

	// Matches services.llm_logging_service.EntityType.CALL_CONVERSATION.
	// The usecase_type is supplied per call so this sink is not bot-specific.
	callConversationEntityType = "callconversation"

	// maxLLMLogPayloadBytes guards SQS's 256KB message limit; if the
	// assembled kwargs exceed this the log is dropped (best-effort).
	maxLLMLogPayloadBytes = 200 * 1024
)

// newLLMLogSink returns an llmrouter.LogSink that enqueues each LLM call
// to Disha's llm_logging_service (S3 + DB) through the existing
// /common/enqueue_job worker. usecaseType differentiates call types
// (e.g. sales_call_conversation), so this sink is reusable across bots.
// It is best-effort: failures are logged and swallowed so they never
// affect the live call. Returns nil when no API client is available.
func newLLMLogSink(api *APIClient, logger *log.Logger, usecaseType, userID, conversationID string) func(llmrouter.CallLog) {
	if api == nil {
		return nil
	}
	return func(c llmrouter.CallLog) {
		requestPayload := map[string]any{
			"model":    c.Model,
			"messages": c.Messages,
			"stream":   true,
		}
		responsePayload := map[string]any{
			"content":       c.ResponseContent,
			"completed":     c.Completed,
			"status_code":   c.StatusCode,
			"finish_reason": c.FinishReason,
		}
		if c.ErrorMessage != "" {
			responsePayload["error"] = map[string]any{
				"type":        c.ErrorType,
				"message":     c.ErrorMessage,
				"status_code": c.StatusCode,
			}
		}
		kwargs := map[string]any{
			"model":              c.Model,
			"request_payload":    requestPayload,
			"response_payload":   responsePayload,
			"prompt_tokens":      0,
			"completion_tokens":  0,
			"ttfb_ms":            c.TTFBMs,
			"total_time_ms":      c.TotalMs,
			"user_id":            userID,
			"entity_type":        callConversationEntityType,
			"entity_id":          conversationID,
			"usecase_type":       usecaseType,
			"deployment":         c.Deployment,
			"llm_call_completed": c.Completed,
			"status_code":        statusCodeOrNil(c.StatusCode),
		}

		raw, err := json.Marshal(kwargs)
		if err != nil || len(raw) > maxLLMLogPayloadBytes {
			if logger != nil {
				logger.Printf("disha: skipping LLM call log (bytes=%d err=%v)\n", len(raw), err)
			}
			return
		}

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := api.EnqueueJob(ctx, EnqueueJobRequest{
			ModuleName: llmLogModule,
			FuncName:   llmLogFunc,
			Kwargs:     kwargs,
			SQSQueue:   llmLogQueue,
		}); err != nil && logger != nil {
			logger.Printf("disha: LLM call log enqueue failed: %v\n", err)
		}
	}
}

func statusCodeOrNil(code int) any {
	if code == 0 {
		return nil
	}
	return code
}
