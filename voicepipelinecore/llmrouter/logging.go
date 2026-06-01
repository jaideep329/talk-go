package llmrouter

import (
	"strings"
	"time"
)

// CallLog is the per-call record handed to Config.LogSink after every
// completion (success, error, or interruption). The sink (provided by
// the bot) decides how to persist it — e.g. enqueue Disha's
// llm_logging_service. The router stays free of S3/DB/usecase concerns.
type CallLog struct {
	Model            string
	ConfigKey        string
	Deployment       string
	Messages         []map[string]string
	ResponseContent  string
	PromptTokens     int
	CompletionTokens int
	TTFBMs           float64
	TotalMs          float64
	StatusCode       int
	Completed        bool
	Interrupted      bool
	ErrorType        string
	ErrorMessage     string
	FinishReason     string
	UsingFallback    bool
	SelectedGroup    string
}

// deploymentName mirrors OpenAIConfigHandler.get_deployment_name so the
// logged deployment matches what Python records.
func deploymentName(cfg endpointConfig) string {
	if cfg.APIKeyEnv != "" {
		if i := strings.Index(cfg.APIKeyEnv, "_API_KEY"); i >= 0 {
			return cfg.APIKeyEnv[:i]
		}
		return cfg.APIKeyEnv
	}
	if cfg.Provider == providerVertex {
		project := cfg.VertexProject
		if project == "gen-lang-client-0439239631" {
			project = "dishaai"
		}
		if project == "" {
			project = "curelinkai"
		}
		parts := []string{string(cfg.Provider), project}
		if cfg.VertexLocation != "" {
			parts = append(parts, cfg.VertexLocation)
		}
		return strings.Join(parts, "_")
	}
	return string(cfg.Provider)
}

func msFromDuration(d time.Duration) float64 {
	return float64(d.Microseconds()) / 1000.0
}
