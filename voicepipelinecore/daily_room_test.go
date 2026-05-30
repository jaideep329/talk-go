package voicepipelinecore

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type testWriteCloser struct {
	bytes.Buffer
}

func (w *testWriteCloser) Close() error { return nil }

func TestDailyRoomRespondsToRTVIPing(t *testing.T) {
	var out testWriteCloser
	room := &DailyRoom{
		roomName: "room-1",
		stdin:    &out,
	}

	room.handleAppMessage(json.RawMessage(`{
		"label":"rtvi-ai",
		"type":"client-message",
		"id":"msg-1",
		"data":{"t":"ping"}
	}`))

	var cmd dailyBridgeCommand
	if err := json.NewDecoder(&out).Decode(&cmd); err != nil {
		t.Fatalf("Decode command: %v", err)
	}
	if cmd.Type != "message" {
		t.Fatalf("command type = %q, want message", cmd.Type)
	}
	msg, ok := cmd.Data.(map[string]any)
	if !ok {
		t.Fatalf("command data = %#v, want object", cmd.Data)
	}
	if msg["label"] != "rtvi-ai" || msg["type"] != "server-response" || msg["id"] != "msg-1" {
		t.Fatalf("RTVI response mismatch: %+v", msg)
	}
	data, ok := msg["data"].(map[string]any)
	if !ok {
		t.Fatalf("response data = %#v, want object", msg["data"])
	}
	if data["t"] != "ping" || data["d"] != "pong" {
		t.Fatalf("response payload = %+v, want ping/pong", data)
	}
}

func TestJoinDailyRoomRetriesBridgeJoin(t *testing.T) {
	fix := newTestFixture(t)
	tmp := t.TempDir()
	countFile := filepath.Join(tmp, "count")
	script := filepath.Join(tmp, "bridge.sh")
	body := `#!/bin/sh
count=0
if [ -f "$DAILY_BRIDGE_TEST_COUNT" ]; then
  count=$(cat "$DAILY_BRIDGE_TEST_COUNT")
fi
count=$((count + 1))
echo "$count" > "$DAILY_BRIDGE_TEST_COUNT"
if [ "$count" -lt 3 ]; then
  printf '%s\n' '{"event":"error","message":"join failed"}'
  exit 0
fi
printf '%s\n' '{"event":"joined","participant_id":"bot","meeting_id":"meeting-1"}'
while IFS= read -r line; do
  case "$line" in
    *'"type":"leave"'*) break ;;
  esac
done
printf '%s\n' '{"event":"left"}'
`
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatalf("WriteFile script: %v", err)
	}
	t.Setenv("DAILY_BRIDGE_PYTHON", "/bin/sh")
	t.Setenv("DAILY_BRIDGE_SCRIPT", script)
	t.Setenv("DAILY_BRIDGE_TEST_COUNT", countFile)

	oldRetryDelay := dailyJoinRetryDelay
	oldJoinTimeout := dailyJoinTimeout
	dailyJoinRetryDelay = 10 * time.Millisecond
	dailyJoinTimeout = time.Second
	t.Cleanup(func() {
		dailyJoinRetryDelay = oldRetryDelay
		dailyJoinTimeout = oldJoinTimeout
	})

	audioSource := NewAudioSourceProcessor(fix.TaskCtx)
	room, err := JoinDailyRoom("https://example.daily.co/test-room", "token", fix.TaskCtx, audioSource)
	if err != nil {
		t.Fatalf("JoinDailyRoom: %v", err)
	}
	room.Disconnect()
	if err := waitForWG(fix.WG, 2*time.Second); err != nil {
		t.Fatalf("waitForWG: %v", err)
	}
	raw, err := os.ReadFile(countFile)
	if err != nil {
		t.Fatalf("ReadFile count: %v", err)
	}
	if got := string(bytes.TrimSpace(raw)); got != "3" {
		t.Fatalf("join attempts = %s, want 3", got)
	}
}
