package voicepipelinecore

import (
	"io"
	"log"
	"sync"
	"testing"
	"time"
)

func TestLifecycleCallbacksAreOnceAndTracked(t *testing.T) {
	var wg sync.WaitGroup
	logger := log.New(io.Discard, "", 0)
	calls := make(chan string, 4)
	l := newCallEventDispatcher(logger, &wg, CallEvents{
		OnBotJoined:       func(time.Time) { calls <- "bot_joined" },
		OnUserJoined:      func(time.Time) { calls <- "user_joined" },
		OnBotFirstSpeech:  func(time.Time) { calls <- "bot_first_speech" },
		OnUserFirstSpeech: func(time.Time) { calls <- "user_first_speech" },
	})

	l.fireBotJoined(time.Now())
	l.fireBotJoined(time.Now())
	l.fireUserJoined(time.Now())
	l.fireUserJoined(time.Now())
	l.fireBotFirstSpeech(time.Now())
	l.fireBotFirstSpeech(time.Now())
	l.fireUserFirstSpeech(time.Now())
	l.fireUserFirstSpeech(time.Now())

	if err := waitForWG(&wg, 2*time.Second); err != nil {
		t.Fatalf("waitForWG: %v", err)
	}
	close(calls)
	got := map[string]int{}
	for name := range calls {
		got[name]++
	}
	for _, name := range []string{"bot_joined", "user_joined", "bot_first_speech", "user_first_speech"} {
		if got[name] != 1 {
			t.Fatalf("%s callback count = %d, want 1 (all counts: %+v)", name, got[name], got)
		}
	}
}
