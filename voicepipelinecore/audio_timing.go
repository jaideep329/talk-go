package voicepipelinecore

import (
	"encoding/json"
	"sync"
	"time"
)

const audioTimingLogInterval = 10 * time.Second

type audioTimingAggregator struct {
	mu    sync.Mutex
	stats map[string]*audioTimingStat
}

type audioTimingStat struct {
	count   uint64
	totalNS uint64
	maxNS   uint64
}

type audioTimingEntry struct {
	Name    string  `json:"name"`
	Count   uint64  `json:"count"`
	AvgMS   float64 `json:"avg_ms"`
	MaxMS   float64 `json:"max_ms"`
	TotalMS float64 `json:"total_ms"`
}

type audioTimingLog struct {
	Event    string             `json:"event"`
	RoomName string             `json:"room_name,omitempty"`
	Timings  []audioTimingEntry `json:"timings"`
}

func newAudioTimingAggregator() *audioTimingAggregator {
	return &audioTimingAggregator{stats: make(map[string]*audioTimingStat)}
}

func (a *audioTimingAggregator) record(name string, elapsed time.Duration) {
	if a == nil || name == "" || elapsed < 0 {
		return
	}
	elapsedNS := uint64(elapsed.Nanoseconds())
	a.mu.Lock()
	defer a.mu.Unlock()
	stat := a.stats[name]
	if stat == nil {
		stat = &audioTimingStat{}
		a.stats[name] = stat
	}
	stat.count++
	stat.totalNS += elapsedNS
	if elapsedNS > stat.maxNS {
		stat.maxNS = elapsedNS
	}
}

func (a *audioTimingAggregator) snapshotAndReset() []audioTimingEntry {
	if a == nil {
		return nil
	}
	a.mu.Lock()
	stats := a.stats
	a.stats = make(map[string]*audioTimingStat)
	a.mu.Unlock()

	entries := make([]audioTimingEntry, 0, len(stats))
	for name, stat := range stats {
		if stat == nil || stat.count == 0 {
			continue
		}
		entries = append(entries, audioTimingEntry{
			Name:    name,
			Count:   stat.count,
			AvgMS:   durationMS(time.Duration(stat.totalNS / stat.count)),
			MaxMS:   durationMS(time.Duration(stat.maxNS)),
			TotalMS: durationMS(time.Duration(stat.totalNS)),
		})
	}
	return entries
}

func durationMS(d time.Duration) float64 {
	return float64(d.Microseconds()) / 1000
}

func (r *DailyRoom) recordAudioTiming(name string, elapsed time.Duration) {
	if r == nil || r.audioTiming == nil {
		return
	}
	r.audioTiming.record(name, elapsed)
}

func (r *DailyRoom) perfDiagnosticsEnabled() bool {
	return r != nil && r.perfDiag && r.audioTiming != nil
}

func (r *DailyRoom) monitorAudioTiming() {
	ticker := time.NewTicker(audioTimingLogInterval)
	defer ticker.Stop()

	done := taskDone(r.taskCtx)
	for {
		select {
		case <-ticker.C:
			r.emitAudioTiming()
		case <-r.waitDone:
			r.emitAudioTiming()
			return
		case <-done:
			r.emitAudioTiming()
			return
		}
	}
}

func (r *DailyRoom) emitAudioTiming() {
	if r == nil || r.audioTiming == nil {
		return
	}
	entries := r.audioTiming.snapshotAndReset()
	if len(entries) == 0 {
		return
	}
	raw, err := json.Marshal(audioTimingLog{
		Event:    "audio_timing",
		RoomName: r.roomName,
		Timings:  entries,
	})
	if err != nil {
		r.log("audio_timing marshal error: %v", err)
		return
	}
	r.log("audio_timing %s", raw)
}
