package disha

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
)

func newRedisTestClient(t *testing.T) (*miniredis.Miniredis, RedisClient) {
	t.Helper()
	server := miniredis.RunT(t)
	client := NewRedisClient(server.Addr(), "", 0, nil)
	t.Cleanup(func() {
		if err := client.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		server.Close()
	})
	return server, client
}

func TestRedisClientGetConversationData(t *testing.T) {
	server, client := newRedisTestClient(t)
	remaining := 42.5
	payload := ConversationData{
		Conversation: ConversationRow{
			ID:      "conv-1",
			UserID:  "user-1",
			BotType: "sales_call",
		},
		Chunks: [][]any{{"chunk-1", "user", "hello", false, nil}},
		UserProfile: UserProfileData{
			UserID:                            "user-1",
			Phone:                             "+15551234567",
			RemainingSalesCallTalktimeSeconds: &remaining,
		},
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	server.Set(conversationDataKey("conv-1"), string(raw))

	got, err := client.GetConversationData(context.Background(), "conv-1")
	if err != nil {
		t.Fatalf("GetConversationData: %v", err)
	}
	if got.Conversation.ID != "conv-1" || got.Conversation.UserID != "user-1" {
		t.Fatalf("conversation mismatch: %+v", got.Conversation)
	}
	if got.UserProfile.RemainingSalesCallTalktimeSeconds == nil || *got.UserProfile.RemainingSalesCallTalktimeSeconds != remaining {
		t.Fatalf("remaining talktime mismatch: %+v", got.UserProfile.RemainingSalesCallTalktimeSeconds)
	}
	if len(got.Chunks) != 1 || got.Chunks[0][2] != "hello" {
		t.Fatalf("chunks mismatch: %+v", got.Chunks)
	}
}

func TestRedisClientGetConversationDataMissing(t *testing.T) {
	_, client := newRedisTestClient(t)

	_, err := client.GetConversationData(context.Background(), "missing")
	if !errors.Is(err, ErrConversationDataNotFound) {
		t.Fatalf("error = %v, want ErrConversationDataNotFound", err)
	}
}

func TestRedisClientGetConversationDataMalformed(t *testing.T) {
	server, client := newRedisTestClient(t)
	server.Set(conversationDataKey("bad"), "{not-json")

	_, err := client.GetConversationData(context.Background(), "bad")
	if err == nil || !strings.Contains(err.Error(), "malformed") {
		t.Fatalf("error = %v, want malformed error", err)
	}
}

func TestRedisClientAppendChunk(t *testing.T) {
	server, client := newRedisTestClient(t)
	llmTTFB := 12.3
	chunk := ConversationChunk{
		ID:             "chunk-1",
		Text:           "hello",
		Role:           "assistant",
		BotType:        "sales_call",
		ConversationID: "conv-1",
		UserID:         "user-1",
		LLMTTFBMs:      &llmTTFB,
		Created:        time.Date(2026, 5, 22, 1, 2, 3, 0, time.UTC).Format(time.RFC3339Nano),
		IsDebugLog:     false,
	}

	if err := client.AppendChunk(context.Background(), "user-1", "conv-1", chunk); err != nil {
		t.Fatalf("AppendChunk: %v", err)
	}

	items, err := server.List(conversationChunksKey("user-1", "conv-1"))
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("list length = %d, want 1", len(items))
	}
	var got ConversationChunk
	if err := json.Unmarshal([]byte(items[0]), &got); err != nil {
		t.Fatalf("Unmarshal chunk: %v", err)
	}
	if got.ID != chunk.ID || got.Text != chunk.Text || got.Role != chunk.Role {
		t.Fatalf("chunk mismatch: %+v", got)
	}
	if got.LLMTTFBMs == nil || *got.LLMTTFBMs != llmTTFB {
		t.Fatalf("LLMTTFBMs = %+v, want %.1f", got.LLMTTFBMs, llmTTFB)
	}
}

func TestRedisOptionsNormalizeAddr(t *testing.T) {
	if got := redisOptions("localhost", "", 0).Addr; got != "localhost:6379" {
		t.Fatalf("Addr = %q, want localhost:6379", got)
	}
	if got := redisOptions("localhost:6380", "", 0).Addr; got != "localhost:6380" {
		t.Fatalf("Addr = %q, want localhost:6380", got)
	}
}

type timeoutError struct{}

func (timeoutError) Error() string   { return "timeout" }
func (timeoutError) Timeout() bool   { return true }
func (timeoutError) Temporary() bool { return true }

func TestWithRedisTimeoutRetry(t *testing.T) {
	attempts := 0
	err := withRedisTimeoutRetry(context.Background(), func() error {
		attempts++
		if attempts < 3 {
			return timeoutError{}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("withRedisTimeoutRetry: %v", err)
	}
	if attempts != 3 {
		t.Fatalf("attempts = %d, want 3", attempts)
	}
}
