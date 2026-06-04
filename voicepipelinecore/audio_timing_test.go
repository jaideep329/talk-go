package voicepipelinecore

import (
	"testing"
	"time"
)

func TestAudioTimingAggregatorSnapshotAndReset(t *testing.T) {
	agg := newAudioTimingAggregator()
	agg.record("write", 2*time.Millisecond)
	agg.record("write", 4*time.Millisecond)

	got := agg.snapshotAndReset()
	if len(got) != 1 {
		t.Fatalf("entry count = %d, want 1", len(got))
	}
	if got[0].Name != "write" || got[0].Count != 2 || got[0].AvgMS != 3 || got[0].MaxMS != 4 || got[0].TotalMS != 6 {
		t.Fatalf("snapshot mismatch: %+v", got[0])
	}
	if got := agg.snapshotAndReset(); len(got) != 0 {
		t.Fatalf("snapshot after reset = %+v, want empty", got)
	}
}
