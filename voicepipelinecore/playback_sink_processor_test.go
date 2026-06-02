package voicepipelinecore

import (
	"encoding/binary"
	"testing"
)

func TestPlaybackBotPCMFrameConvertsRawPCMBytes(t *testing.T) {
	fix := newTestFixture(t)
	p := &PlaybackSinkProcessor{taskCtx: fix.TaskCtx}

	input := make([]byte, framePCMBytes)
	want := []int16{-32768, -1234, 0, 1234, 32767}
	for i, sample := range want {
		binary.LittleEndian.PutUint16(input[i*2:], uint16(sample))
	}

	got := p.botPCMFrame(input)
	if len(got) != framePCM {
		t.Fatalf("expected %d samples, got %d", framePCM, len(got))
	}
	for i, sample := range want {
		if got[i] != sample {
			t.Fatalf("sample %d: expected %d, got %d", i, sample, got[i])
		}
	}
	for i := len(want); i < len(got); i++ {
		if got[i] != 0 {
			t.Fatalf("expected trailing sample %d to remain silent, got %d", i, got[i])
		}
	}
}
