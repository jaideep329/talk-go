package disha

import (
	"net/http"
	"testing"
	"time"

	"github.com/jaideep329/talk-go/voicepipelinecore/llmrouter"
)

func TestNewLLMLogSinkQueuesModuleLevelWrapper(t *testing.T) {
	server, requests := captureAPIRequest(t, http.StatusOK)
	api := NewAPIClient(server.URL, time.Second, nil)
	sink := newLLMLogSink(api, nil, "sales_call_conversation", "user-1", "conv-1")
	if sink == nil {
		t.Fatal("newLLMLogSink returned nil")
	}

	sink(llmrouter.CallLog{
		Model:           "grok-4-1-fast-non-reasoning",
		Deployment:      "GROK_4_1_FNR_EASTUS",
		Messages:        []map[string]string{{"role": "user", "content": "hello"}},
		ResponseContent: "hi",
		TTFBMs:          12.5,
		TotalMs:         48.25,
		StatusCode:      http.StatusOK,
		Completed:       true,
		FinishReason:    "stop",
	})

	var got capturedAPIRequest
	select {
	case got = <-requests:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for enqueue request")
	}
	if got.Method != http.MethodPost || got.Path != "/common/enqueue_job" {
		t.Fatalf("request = %s %s, want POST /common/enqueue_job", got.Method, got.Path)
	}
	if got.Body["module_name"] != "services.llm_logging_service" ||
		got.Body["func_name"] != "log_llm_call_job" ||
		got.Body["sqs_queue"] != "p1-fast-l1" {
		t.Fatalf("enqueue target mismatch: %+v", got.Body)
	}

	kwargs, ok := got.Body["kwargs"].(map[string]any)
	if !ok {
		t.Fatalf("kwargs = %#v, want object", got.Body["kwargs"])
	}
	if kwargs["entity_id"] != "conv-1" ||
		kwargs["user_id"] != "user-1" ||
		kwargs["entity_type"] != "callconversation" ||
		kwargs["usecase_type"] != "sales_call_conversation" ||
		kwargs["deployment"] != "GROK_4_1_FNR_EASTUS" ||
		kwargs["llm_call_completed"] != true {
		t.Fatalf("kwargs mismatch: %+v", kwargs)
	}
}
