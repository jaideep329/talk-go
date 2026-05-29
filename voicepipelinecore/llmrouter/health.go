package llmrouter

import (
	"context"
	"encoding/json"
	"time"
)

// healthKeyPrefix matches HEALTH_KEY_PREFIX in llm_endpoint_health.py.
const healthKeyPrefix = "live_call_modal_health"

// selectionAverageWindow matches SELECTION_AVERAGE_WINDOW: the number of
// most-recent poll runs averaged to score an endpoint.
const selectionAverageWindow = 5

// healthTTL matches HEALTH_TTL_SECONDS (24h) used when writing health.
const healthTTL = 24 * time.Hour

// blacklistReasonError matches BLACKLIST_REASON_ERROR.
const blacklistReasonError = "error"

// RedisStore is the narrow Redis surface the router needs. Its method
// set is a subset of disha.RedisClient, so the existing Disha Redis
// client satisfies it structurally — the router never imports disha.
//
// GetCache/MGetCache return the raw JSON bytes Disha's Python set_cache
// stored (json.dumps(value)); ok=false means the key was absent.
// SetCache marshals value to JSON (matching set_cache) and sets a TTL.
// AcquireLock is a SET NX with expiry (acquire_redis_lock).
type RedisStore interface {
	GetCache(ctx context.Context, key string) ([]byte, bool, error)
	MGetCache(ctx context.Context, keys ...string) ([][]byte, error)
	SetCache(ctx context.Context, key string, value any, expiration time.Duration) error
	AcquireLock(ctx context.Context, key string, ttl time.Duration) (bool, error)
}

func healthKey(configKey string) string {
	return healthKeyPrefix + ":" + configKey
}

// pollRun mirrors PollRun in llm_endpoint_health.py. All fields are kept
// (and marshalled with their Python keys, null when unset) so a
// read-modify-write blacklist update preserves the poller's data.
type pollRun struct {
	At                string   `json:"at"`
	Success           bool     `json:"success"`
	LatencyMs         *float64 `json:"latency_ms"`
	AdjustedLatencyMs *float64 `json:"adjusted_latency_ms"`
	ErrorType         *string  `json:"error_type"`
	StatusCode        *int     `json:"status_code"`
	ExceptionText     *string  `json:"exception_text"`
}

// liveCallError mirrors LiveCallError in llm_endpoint_health.py.
type liveCallError struct {
	At            string  `json:"at"`
	ErrorType     string  `json:"error_type"`
	StatusCode    *int    `json:"status_code"`
	ExceptionText string  `json:"exception_text"`
}

// endpointHealth mirrors EndpointHealth.to_dict()/from_dict().
type endpointHealth struct {
	PollRuns          []pollRun      `json:"poll_runs"`
	Blacklisted       bool           `json:"blacklisted"`
	BlacklistReason   *string        `json:"blacklist_reason"`
	BlacklistedAt     *string        `json:"blacklisted_at"`
	UpdatedAt         *string        `json:"updated_at"`
	LastLiveCallError *liveCallError `json:"last_live_call_error"`
}

// selectionLatency mirrors EndpointHealth.selection_latency(): the mean
// of the adjusted latencies of the most recent selectionAverageWindow
// poll runs. Returns ok=false when there is no latency data.
func (h *endpointHealth) selectionLatency() (float64, bool) {
	values := make([]float64, 0, len(h.PollRuns))
	for _, run := range h.PollRuns {
		if run.AdjustedLatencyMs != nil {
			values = append(values, *run.AdjustedLatencyMs)
		}
	}
	if len(values) == 0 {
		return 0, false
	}
	if len(values) > selectionAverageWindow {
		values = values[len(values)-selectionAverageWindow:]
	}
	var sum float64
	for _, v := range values {
		sum += v
	}
	return sum / float64(len(values)), true
}

// parseHealth unmarshals raw cache bytes into endpointHealth. Absent or
// malformed values yield a zero-value health (not blacklisted, no
// latency), which is exactly how the Python from_dict treats them.
func parseHealth(raw []byte) endpointHealth {
	var h endpointHealth
	if len(raw) == 0 {
		return endpointHealth{}
	}
	if err := json.Unmarshal(raw, &h); err != nil {
		return endpointHealth{}
	}
	return h
}

// blacklistForLiveError mirrors blacklist_endpoint_for_live_error +
// EndpointHealth.record_live_call_error: read the current health,
// attach the live-call error, mark it blacklisted with reason "error",
// and write it back (preserving poll_runs) with the 24h TTL.
func blacklistForLiveError(ctx context.Context, store RedisStore, configKey, errType string, statusCode int, msg string) error {
	key := healthKey(configKey)
	raw, _, err := store.GetCache(ctx, key)
	if err != nil {
		return err
	}
	health := parseHealth(raw)
	now := utcNowISO()

	var sc *int
	if statusCode != 0 {
		sc = &statusCode
	}
	health.LastLiveCallError = &liveCallError{
		At:            now,
		ErrorType:     errType,
		StatusCode:    sc,
		ExceptionText: truncate(msg, 1000),
	}
	health.Blacklisted = true
	reason := blacklistReasonError
	health.BlacklistReason = &reason
	health.BlacklistedAt = &now
	health.UpdatedAt = &now

	return store.SetCache(ctx, key, health, healthTTL)
}

// utcNowISO mirrors utc_now_iso(): RFC3339 with microseconds, "Z" zone.
func utcNowISO() string {
	return time.Now().UTC().Format("2006-01-02T15:04:05.000000") + "Z"
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
