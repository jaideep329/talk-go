package voicepipelinecore

import (
	"io"
	"log"
	"testing"
	"time"
)

func TestUIEventSenderStoresRTVIStreamForDebugLogs(t *testing.T) {
	events := NewUIEventSender(log.New(io.Discard, "", 0))
	at := time.Date(2026, 5, 13, 11, 11, 32, 892_000_000, time.UTC)

	events.SetDailyMeeting("meeting-1", "bot-session-1")
	events.SetUserSessionID("user-session-1")
	events.BotTranscription("Hi Jai", at)
	events.BotStartedSpeaking(at.Add(time.Second))
	events.ServerMessage(map[string]any{
		"type":     "llm_call_result",
		"status":   "completed",
		"model":    "test-model",
		"ttfb_ms":  123.4,
		"total_ms": 567.8,
	}, at.Add(2*time.Second))
	events.BotStoppedSpeaking(at.Add(3 * time.Second))
	events.UserTranscription("hello?", true, at.Add(4*time.Second))

	meetingID, botSessionID, userSessionID := events.DailySession()
	if meetingID != "meeting-1" || botSessionID != "bot-session-1" || userSessionID != "user-session-1" {
		t.Fatalf("daily session = %q %q %q", meetingID, botSessionID, userSessionID)
	}

	logs := events.Snapshot()
	wantTypes := []string{
		"bot-transcription",
		"bot-started-speaking",
		"server-message",
		"bot-stopped-speaking",
		"user-transcription",
	}
	if len(logs) != len(wantTypes) {
		t.Fatalf("log count = %d, want %d", len(logs), len(wantTypes))
	}
	for i, want := range wantTypes {
		if logs[i].Label != "rtvi-ai" || logs[i].Type != want || logs[i].Timestamp == 0 {
			t.Fatalf("logs[%d] = %+v, want label rtvi-ai type %s", i, logs[i], want)
		}
	}
	userData, ok := logs[4].Data.(map[string]any)
	if !ok {
		t.Fatalf("user data = %#v", logs[4].Data)
	}
	if userData["text"] != "hello?" || userData["user_id"] != "user-session-1" ||
		userData["timestamp"] != "2026-05-13T11:11:36.892+00:00" ||
		userData["final"] != true {
		t.Fatalf("user transcription data mismatch: %+v", userData)
	}
}
