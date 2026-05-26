package disha

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/jaideep329/talk-go/voicepipelinecore"
)

type sessionAPIRequest struct {
	Method string
	Path   string
	Body   map[string]any
}

type sessionAPIRecorder struct {
	mu       sync.Mutex
	requests []sessionAPIRequest
}

func newSessionAPIServer(t *testing.T) (*httptest.Server, *sessionAPIRecorder) {
	t.Helper()
	recorder := &sessionAPIRecorder{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("ReadAll: %v", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		var body map[string]any
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &body); err != nil {
				t.Errorf("Unmarshal request: %v", err)
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
		}
		if got := r.Header.Get("Authorization"); got != "" {
			t.Errorf("Authorization = %q, want absent", got)
		}
		recorder.mu.Lock()
		recorder.requests = append(recorder.requests, sessionAPIRequest{
			Method: r.Method,
			Path:   r.URL.Path,
			Body:   body,
		})
		recorder.mu.Unlock()
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"success":true}`))
	}))
	t.Cleanup(server.Close)
	return server, recorder
}

func (r *sessionAPIRecorder) snapshot() []sessionAPIRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]sessionAPIRequest, len(r.requests))
	copy(out, r.requests)
	return out
}

func seedConversationData(t *testing.T, server *miniredis.Miniredis, conversationID string, data ConversationData) {
	t.Helper()
	raw, err := json.Marshal(data)
	if err != nil {
		t.Fatalf("Marshal conversation data: %v", err)
	}
	server.Set(conversationDataKey(conversationID), string(raw))
}

func testDeps(redis RedisClient, api *APIClient) Deps {
	return Deps{
		Logger: log.New(io.Discard, "", 0),
		Redis:  redis,
		API:    api,
	}
}

func TestBuildSalesCallOptionsAssemblesDishaSession(t *testing.T) {
	redisServer, redisClient := newRedisTestClient(t)
	apiServer, apiRecorder := newSessionAPIServer(t)
	api := NewAPIClient(apiServer.URL, 10*time.Second, nil)
	remainingTalkTime := 2.5
	conversationID := "conv-1"
	userID := "user-1"
	seedConversationData(t, redisServer, conversationID, ConversationData{
		Conversation: ConversationRow{
			ID:      conversationID,
			UserID:  userID,
			BotType: SalesCallBotType,
		},
		Chunks: [][]any{
			{"chunk-1", "user", "hello", false, nil},
			{"chunk-2", "assistant", "hi", false, nil},
			{"debug", "assistant", "debug", true, nil},
			{"tool", "tool", "skip role", false, nil},
			{"metadata", "assistant", "skip metadata", false, map[string]any{"x": "y"}},
		},
		UserProfile: UserProfileData{
			UserID:                            userID,
			Phone:                             "+15551234567",
			RemainingSalesCallTalktimeSeconds: &remainingTalkTime,
		},
	})

	opts, err := BuildSalesCallOptions(context.Background(), conversationID, testDeps(redisClient, api))
	if err != nil {
		t.Fatalf("BuildSalesCallOptions: %v", err)
	}
	if opts.RoomName != "conv-conv-1" {
		t.Fatalf("RoomName = %q, want conv-conv-1", opts.RoomName)
	}
	if opts.MaxTalkTime == nil || *opts.MaxTalkTime != 2500*time.Millisecond {
		t.Fatalf("MaxTalkTime = %v, want 2.5s", opts.MaxTalkTime)
	}
	wantMessages := []voicepipelinecore.Message{
		{Role: "system", Content: SalesCallSystemPrompt},
		{Role: "user", Content: "hello"},
		{Role: "assistant", Content: "hi"},
	}
	if len(opts.InitialMessages) != len(wantMessages) {
		t.Fatalf("InitialMessages len = %d, want %d: %+v", len(opts.InitialMessages), len(wantMessages), opts.InitialMessages)
	}
	for i, want := range wantMessages {
		if opts.InitialMessages[i] != want {
			t.Fatalf("InitialMessages[%d] = %+v, want %+v", i, opts.InitialMessages[i], want)
		}
	}
	turnAt := time.Date(2026, 5, 22, 1, 2, 3, 0, time.UTC)
	opts.CallEvents.OnUserTurnCommitted("new user", turnAt)
	opts.CallEvents.OnAssistantTurnCommitted("new assistant", turnAt.Add(time.Second), voicepipelinecore.TurnMetrics{
		LLMTTFBMs:            11.1,
		TTSTTFBMs:            22.2,
		E2ELatencyMs:         33.3,
		TTSTextAggregationMs: 44.4,
	})
	chunkItems, err := redisServer.List(conversationChunksKey(userID, conversationID))
	if err != nil {
		t.Fatalf("List chunks: %v", err)
	}
	if len(chunkItems) != 2 {
		t.Fatalf("chunk count = %d, want 2", len(chunkItems))
	}
	var userChunk, assistantChunk ConversationChunk
	if err := json.Unmarshal([]byte(chunkItems[0]), &userChunk); err != nil {
		t.Fatalf("Unmarshal user chunk: %v", err)
	}
	if err := json.Unmarshal([]byte(chunkItems[1]), &assistantChunk); err != nil {
		t.Fatalf("Unmarshal assistant chunk: %v", err)
	}
	if userChunk.Role != "user" || userChunk.Text != "new user" || userChunk.LLMTTFBMs != nil {
		t.Fatalf("user chunk mismatch: %+v", userChunk)
	}
	if assistantChunk.Role != "assistant" || assistantChunk.Text != "new assistant" {
		t.Fatalf("assistant chunk mismatch: %+v", assistantChunk)
	}
	if assistantChunk.LLMTTFBMs == nil || *assistantChunk.LLMTTFBMs != 11.1 ||
		assistantChunk.TTSTTFBMs == nil || *assistantChunk.TTSTTFBMs != 22.2 ||
		assistantChunk.V2VLatencyMs == nil || *assistantChunk.V2VLatencyMs != 33.3 ||
		assistantChunk.TextAggregationMs == nil || *assistantChunk.TextAggregationMs != 44.4 {
		t.Fatalf("assistant metrics mismatch: %+v", assistantChunk)
	}

	eventAt := time.Date(2026, 5, 22, 2, 3, 4, 0, time.UTC)
	opts.CallEvents.OnBotJoined(eventAt)
	opts.CallEvents.OnUserJoined(eventAt.Add(time.Second))
	opts.CallEvents.OnUserFirstSpeech(eventAt.Add(2 * time.Second))
	opts.CallEvents.OnBotFirstSpeech(eventAt.Add(3 * time.Second))
	opts.CallEvents.OnCallEnded(voicepipelinecore.EndReasonClientDisconnect, voicepipelinecore.CallStats{
		TotalUserDurationSec:  12.9,
		FirstUserAudioFrameAt: eventAt.Add(4 * time.Second),
		EndedAt:               eventAt.Add(5 * time.Second),
	})

	requests := apiRecorder.snapshot()
	if len(requests) != 6 {
		t.Fatalf("request count = %d, want 6: %+v", len(requests), requests)
	}
	assertRequest(t, requests[0], http.MethodPatch, "/bot/update_conversation")
	if requests[0].Body["conversation_id"] != conversationID || requests[0].Body["bot_joined_at"] != eventAt.Format(time.RFC3339) {
		t.Fatalf("bot joined body mismatch: %+v", requests[0].Body)
	}
	assertRequest(t, requests[4], http.MethodPost, "/bot/run_post_call_operations")
	if value, ok := requests[4].Body["end_reason"]; !ok || value != nil {
		t.Fatalf("end_reason = %v (present=%v), want explicit null", value, ok)
	}
	if requests[4].Body["total_user_duration"] != float64(12) || requests[4].Body["log_data_s3_key"] != "" {
		t.Fatalf("post-call body mismatch: %+v", requests[4].Body)
	}
	assertRequest(t, requests[5], http.MethodPost, "/common/enqueue_job")
	kwargs, ok := requests[5].Body["kwargs"].(map[string]any)
	if !ok {
		t.Fatalf("kwargs = %#v, want object", requests[5].Body["kwargs"])
	}
	if requests[5].Body["module_name"] != "services.conversation_chunk_manager" ||
		requests[5].Body["func_name"] != "sync_conversation_chunks_to_db" ||
		requests[5].Body["sqs_queue"] != "p1-fast-l1" ||
		kwargs["user_id"] != userID ||
		kwargs["conversation_id"] != conversationID ||
		kwargs["bot_type"] != SalesCallBotType {
		t.Fatalf("enqueue body mismatch: %+v", requests[5].Body)
	}
}

func assertRequest(t *testing.T, got sessionAPIRequest, method, path string) {
	t.Helper()
	if got.Method != method || got.Path != path {
		t.Fatalf("request = %s %s, want %s %s", got.Method, got.Path, method, path)
	}
}

func TestBuildSalesCallOptionsSeedsHelloWhenNoPriorChunks(t *testing.T) {
	redisServer, redisClient := newRedisTestClient(t)
	apiServer, _ := newSessionAPIServer(t)
	conversationID := "fresh"
	seedConversationData(t, redisServer, conversationID, ConversationData{
		Conversation: ConversationRow{
			ID:      conversationID,
			UserID:  "user-1",
			BotType: SalesCallBotType,
		},
		Chunks: [][]any{
			{"debug", "user", "ignore", true, nil},
		},
		UserProfile: UserProfileData{UserID: "user-1"},
	})

	opts, err := BuildSalesCallOptions(context.Background(), conversationID, testDeps(redisClient, NewAPIClient(apiServer.URL, 10*time.Second, nil)))
	if err != nil {
		t.Fatalf("BuildSalesCallOptions: %v", err)
	}
	if len(opts.InitialMessages) != 2 ||
		opts.InitialMessages[0].Role != "system" ||
		opts.InitialMessages[1] != (voicepipelinecore.Message{Role: "user", Content: "hello?"}) {
		t.Fatalf("InitialMessages = %+v, want system + hello seed", opts.InitialMessages)
	}
	if opts.MaxTalkTime == nil || *opts.MaxTalkTime != lifetimeTalkTimeSeconds*time.Second {
		t.Fatalf("MaxTalkTime = %v, want lifetime default", opts.MaxTalkTime)
	}
}

func TestBuildSalesCallOptionsRejectsUnsupportedBotType(t *testing.T) {
	redisServer, redisClient := newRedisTestClient(t)
	apiServer, _ := newSessionAPIServer(t)
	seedConversationData(t, redisServer, "conv-1", ConversationData{
		Conversation: ConversationRow{
			ID:      "conv-1",
			UserID:  "user-1",
			BotType: "followup_call",
		},
		UserProfile: UserProfileData{UserID: "user-1"},
	})

	_, err := BuildSalesCallOptions(context.Background(), "conv-1", testDeps(redisClient, NewAPIClient(apiServer.URL, 10*time.Second, nil)))
	if err == nil {
		t.Fatal("BuildSalesCallOptions returned nil error for unsupported bot type")
	}
}

func TestSalesTalkTimeLimit(t *testing.T) {
	if got := salesTalkTimeLimit(nil); got != lifetimeTalkTimeSeconds*time.Second {
		t.Fatalf("nil talktime = %s, want default", got)
	}
	zero := 0.0
	if got := salesTalkTimeLimit(&zero); got != 0 {
		t.Fatalf("zero talktime = %s, want immediate timeout", got)
	}
	negative := -10.0
	if got := salesTalkTimeLimit(&negative); got != 0 {
		t.Fatalf("negative talktime = %s, want clamped zero", got)
	}
}

func TestMapEndReason(t *testing.T) {
	if got := mapEndReason(voicepipelinecore.EndReasonTalkTimeExhausted); got == nil || *got != "talktime_exhausted" {
		t.Fatalf("talktime reason = %v, want talktime_exhausted", got)
	}
	if got := mapEndReason(voicepipelinecore.EndReasonUserIdle); got == nil || *got != "user_idle" {
		t.Fatalf("idle reason = %v, want user_idle", got)
	}
	if got := mapEndReason(voicepipelinecore.EndReasonClientDisconnect); got != nil {
		t.Fatalf("client disconnect reason = %v, want nil", *got)
	}
	if got := mapEndReason(voicepipelinecore.EndReasonError); got != nil {
		t.Fatalf("error reason = %v, want nil", *got)
	}
}
