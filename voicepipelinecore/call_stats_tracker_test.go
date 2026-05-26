package voicepipelinecore

import (
	"testing"
	"time"
)

func TestCallStatsTrackerTracksTotalDurationAndFirstAudio(t *testing.T) {
	p := newCallStatsTracker()
	start := time.Unix(100, 0)

	p.MarkUserJoined(start)
	p.MarkUserLeft(start.Add(2 * time.Second))
	p.MarkUserJoined(start.Add(5 * time.Second))

	if ok := p.MarkFirstUserAudio(start.Add(6 * time.Second)); !ok {
		t.Fatal("first audio mark should return true")
	}
	if ok := p.MarkFirstUserAudio(start.Add(7 * time.Second)); ok {
		t.Fatal("second audio mark should return false")
	}

	got := p.TotalDurationSec(start.Add(8 * time.Second))
	if got != 5 {
		t.Fatalf("total duration = %.1f, want 5.0", got)
	}
	if got := p.FirstUserAudioFrameAt(); !got.Equal(start.Add(6 * time.Second)) {
		t.Fatalf("first audio at = %s, want %s", got, start.Add(6*time.Second))
	}
}
