package voicepipelinecore

import (
	"testing"

	lksdk "github.com/livekit/server-sdk-go/v2"
	"github.com/pion/webrtc/v4"
)

func TestLiveKitWriteAudioPCMEncodesOpusLocally(t *testing.T) {
	track, err := lksdk.NewLocalTrack(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus})
	if err != nil {
		t.Fatalf("NewLocalTrack: %v", err)
	}
	room := &LiveKitRoom{outTrack: track}
	if err := room.resetOpusEncoder(); err != nil {
		t.Fatalf("resetOpusEncoder: %v", err)
	}

	if err := room.WriteAudioPCM(make([]byte, framePCMBytes)); err != nil {
		t.Fatalf("WriteAudioPCM: %v", err)
	}
}

func TestLiveKitClearAudioBufferResetsOpusEncoder(t *testing.T) {
	room := &LiveKitRoom{}
	if err := room.resetOpusEncoder(); err != nil {
		t.Fatalf("resetOpusEncoder: %v", err)
	}
	oldEncoder := room.opusEncoder

	room.ClearAudioBuffer()

	if room.opusEncoder == nil {
		t.Fatal("opus encoder was not recreated")
	}
	if room.opusEncoder == oldEncoder {
		t.Fatal("expected ClearAudioBuffer to reset encoder state")
	}
}
