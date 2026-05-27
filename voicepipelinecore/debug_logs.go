package voicepipelinecore

import "time"

const rtviDebugLabel = "rtvi-ai"

// RTVIDebugLogEntry mirrors the JSON shape written by Disha's Python
// RTVI observer to debug_log_data/<conversation_id>/log_data.json.
type RTVIDebugLogEntry struct {
	Label     string  `json:"label"`
	Type      string  `json:"type"`
	Data      any     `json:"data,omitempty"`
	Timestamp float64 `json:"timestamp"`
}

func rtviTimestamp(t time.Time) string {
	if t.IsZero() {
		t = time.Now()
	}
	return t.UTC().Format("2006-01-02T15:04:05.000+00:00")
}
