package disha

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestPostCallOperationsRequestIncludesNullEndReason(t *testing.T) {
	req := PostCallOperationsRequest{
		ConversationID:     "conv-1",
		TotalUserDuration:  12,
		EndedAt:            time.Date(2026, 5, 22, 1, 2, 3, 0, time.UTC),
		LogDataS3Key:       "",
		OnboardingCallDone: false,
	}
	raw, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if !strings.Contains(string(raw), `"end_reason":null`) {
		t.Fatalf("payload = %s, want explicit null end_reason", raw)
	}
}
