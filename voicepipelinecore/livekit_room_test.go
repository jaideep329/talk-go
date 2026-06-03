package voicepipelinecore

import (
	"strings"
	"testing"

	lksdk "github.com/livekit/server-sdk-go/v2"
	"github.com/pion/webrtc/v4"
	"gopkg.in/hraban/opus.v2"
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

func TestLiveKitOpusEncoderConfigFromEnv(t *testing.T) {
	t.Setenv(liveKitOpusComplexityEnv, "3")
	t.Setenv(liveKitOpusBitrateEnv, "24000")
	t.Setenv(liveKitOpusMaxBandwidthEnv, "superwideband")

	room := &LiveKitRoom{}
	if err := room.resetOpusEncoder(); err != nil {
		t.Fatalf("resetOpusEncoder: %v", err)
	}

	complexity, err := room.opusEncoder.Complexity()
	if err != nil {
		t.Fatalf("Complexity: %v", err)
	}
	if complexity != 3 {
		t.Fatalf("complexity = %d, want 3", complexity)
	}

	bitrate, err := room.opusEncoder.Bitrate()
	if err != nil {
		t.Fatalf("Bitrate: %v", err)
	}
	if bitrate != 24000 {
		t.Fatalf("bitrate = %d, want 24000", bitrate)
	}

	bandwidth, err := room.opusEncoder.MaxBandwidth()
	if err != nil {
		t.Fatalf("MaxBandwidth: %v", err)
	}
	if bandwidth != opus.SuperWideband {
		t.Fatalf("max bandwidth = %v, want %v", bandwidth, opus.SuperWideband)
	}
}

func TestLiveKitOpusEncoderConfigRejectsInvalidEnv(t *testing.T) {
	t.Setenv(liveKitOpusComplexityEnv, "11")

	room := &LiveKitRoom{}
	err := room.resetOpusEncoder()
	if err == nil {
		t.Fatal("expected invalid opus complexity to fail")
	}
	if !strings.Contains(err.Error(), liveKitOpusComplexityEnv) {
		t.Fatalf("error %q does not mention %s", err, liveKitOpusComplexityEnv)
	}
}
