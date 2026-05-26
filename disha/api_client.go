package disha

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

const (
	defaultAPIBaseURL = "https://disha-ai.curelinktech.in"
	defaultAPITimeout = 10 * time.Second
)

type APIClient struct {
	baseURL    string
	httpClient *http.Client
	logger     *log.Logger
}

func NewAPIClient(baseURL string, timeout time.Duration, logger *log.Logger) *APIClient {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		baseURL = defaultAPIBaseURL
	}
	if timeout <= 0 {
		timeout = defaultAPITimeout
	}
	return &APIClient{
		baseURL:    baseURL,
		httpClient: &http.Client{Timeout: timeout},
		logger:     logger,
	}
}

func (c *APIClient) UpdateConversation(ctx context.Context, req UpdateConversationRequest) error {
	return c.send(ctx, http.MethodPatch, "/bot/update_conversation", req)
}

func (c *APIClient) RunPostCallOperations(ctx context.Context, req PostCallOperationsRequest) error {
	return c.send(ctx, http.MethodPost, "/bot/run_post_call_operations", req)
}

func (c *APIClient) EnqueueJob(ctx context.Context, req EnqueueJobRequest) error {
	return c.send(ctx, http.MethodPost, "/common/enqueue_job", req)
}

func (c *APIClient) send(ctx context.Context, method, path string, body any) error {
	payload, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("disha: marshal %s %s request: %w", method, path, err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("disha: build %s %s request: %w", method, path, err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return fmt.Errorf("disha: API %s %s failed: %w", method, path, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("disha: API %s %s returned %d: %s", method, path, resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	return nil
}
