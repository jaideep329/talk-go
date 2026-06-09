package disha

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jaideep329/talk-go/internal/sentryutil"
)

const (
	documentCacheTTL  = 20 * time.Minute
	documentKeyPrefix = "document"
)

// DocumentVersion is the shape Disha stores under
// `document:{name}:{env}` (or `document:{name}:v{version}`).
type DocumentVersion struct {
	ID         string         `json:"id"`
	PromptText string         `json:"prompt_text"`
	ConfigJSON map[string]any `json:"config_json"`
	Version    int            `json:"version"`
}

// DocumentStore reads Disha's Langfuse-backed prompts from Redis. It
// mirrors documents/document_manager.py:_fetch_from_redis — same key
// shape, same JSON payload — but never falls through to Postgres
// (talk-go has no DB connection by design).
//
// The store keeps a per-process in-memory cache to avoid touching
// Redis once a prompt has been resolved. TTL matches Python (20 min).
type DocumentStore struct {
	redis    RedisClient
	logger   *log.Logger
	env      string
	renderer TemplateRenderer

	mu    sync.Mutex
	cache map[string]documentCacheEntry
}

type documentCacheEntry struct {
	doc       DocumentVersion
	expiresAt time.Time
}

// NewDocumentStore wires the store to the shared Disha Redis client.
// Environment selection follows Python's _resolve_environment:
//   - ENVIRONMENT in {"staging", "dev"} → key suffix "latest"
//   - anything else                     → key suffix "production"
func NewDocumentStore(redis RedisClient, logger *log.Logger) *DocumentStore {
	return newDocumentStore(redis, logger, NewPythonJinjaRenderer(logger))
}

func newDocumentStore(redis RedisClient, logger *log.Logger, renderer TemplateRenderer) *DocumentStore {
	if redis == nil {
		return nil
	}
	return &DocumentStore{
		redis:    redis,
		logger:   logger,
		env:      resolveDocumentEnvironment(),
		renderer: renderer,
		cache:    make(map[string]documentCacheEntry),
	}
}

func resolveDocumentEnvironment() string {
	switch strings.TrimSpace(strings.ToLower(os.Getenv("ENVIRONMENT"))) {
	case "staging", "dev":
		return "latest"
	default:
		return "production"
	}
}

// GetDocument fetches the prompt for `name` (or a specific version
// when version != 0) and renders it with the provided variables.
// Returns the rendered text and the resolved version (useful for
// pinning system_message_prompt_key on chunks).
func (s *DocumentStore) GetDocument(ctx context.Context, name string, version int, variables DocumentVariables) (string, int, error) {
	if s == nil {
		return "", 0, errors.New("disha: document store is nil")
	}
	if s.renderer == nil {
		return "", 0, errors.New("disha: document renderer is nil")
	}
	doc, err := s.resolve(ctx, name, version)
	if err != nil {
		return "", 0, err
	}
	rendered, err := s.renderer.Render(ctx, TemplateRenderRequest{
		DocumentName:    name,
		DocumentVersion: doc.Version,
		Text:            doc.PromptText,
		Variables:       variables,
	})
	if err != nil {
		return "", 0, err
	}
	if s.logger != nil {
		if rendered.UndefinedError != "" {
			s.logger.Printf("disha: document %q (version=%d) has undefined jinja variables after strict validation: %s missing=%v\n", name, doc.Version, rendered.UndefinedError, rendered.CompileTimeMissingVars)
		} else if len(rendered.CompileTimeMissingVars) > 0 {
			s.logger.Printf("disha: document %q (version=%d) has compile-time missing jinja variables: %v\n", name, doc.Version, rendered.CompileTimeMissingVars)
		}
		if rendered.StrictValidationError != "" {
			s.logger.Printf("disha: document %q (version=%d) strict jinja validation warning: %s\n", name, doc.Version, rendered.StrictValidationError)
		}
	}
	reportMissingJinjaVariables(name, doc.Version, rendered, variables)
	return rendered.Output, doc.Version, nil
}

func reportMissingJinjaVariables(name string, version int, rendered TemplateRenderResult, variables DocumentVariables) {
	if rendered.UndefinedError == "" && len(rendered.CompileTimeMissingVars) == 0 {
		return
	}
	supplied := make([]string, 0, len(variables))
	for key := range variables {
		supplied = append(supplied, key)
	}
	sort.Strings(supplied)
	sentryutil.Capture(sentryutil.Event{
		Message: fmt.Sprintf("Undefined Jinja variables in document %q v%d", name, version),
		Tags: map[string]string{
			"component":        "disha_document_store",
			"operation":        "render_jinja",
			"document_name":    name,
			"document_version": fmt.Sprintf("%d", version),
		},
		Details: map[string]any{
			"document_name":                  name,
			"document_version":               version,
			"runtime_missing_variable":       rendered.UndefinedError,
			"compile_time_missing_variables": rendered.CompileTimeMissingVars,
			"strict_validation_error":        rendered.StrictValidationError,
			"supplied_variables":             supplied,
		},
	})
}

func (s *DocumentStore) resolve(ctx context.Context, name string, version int) (DocumentVersion, error) {
	cacheKey := documentCacheKey(name, version)
	if cached, ok := s.lookupCache(cacheKey); ok {
		return cached, nil
	}
	doc, err := s.fetchFromRedis(ctx, name, version)
	if err != nil {
		return DocumentVersion{}, err
	}
	s.storeCache(cacheKey, doc)
	return doc, nil
}

func (s *DocumentStore) fetchFromRedis(ctx context.Context, name string, version int) (DocumentVersion, error) {
	key := s.redisKey(name, version)
	raw, ok, err := s.redis.GetCache(ctx, key)
	if err != nil {
		return DocumentVersion{}, fmt.Errorf("disha: document %q redis GET failed: %w", name, err)
	}
	if !ok {
		return DocumentVersion{}, fmt.Errorf("disha: document %q (version=%d) not in redis key %s", name, version, key)
	}
	var doc DocumentVersion
	if err := json.Unmarshal(raw, &doc); err != nil {
		return DocumentVersion{}, fmt.Errorf("disha: document %q payload malformed: %w", name, err)
	}
	if s.logger != nil {
		s.logger.Printf("disha: document loaded name=%s version=%d key=%s\n", name, doc.Version, key)
	}
	return doc, nil
}

func (s *DocumentStore) lookupCache(key string) (DocumentVersion, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.cache[key]
	if !ok || time.Now().After(entry.expiresAt) {
		if ok {
			delete(s.cache, key)
		}
		return DocumentVersion{}, false
	}
	return entry.doc, true
}

func (s *DocumentStore) storeCache(key string, doc DocumentVersion) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cache[key] = documentCacheEntry{doc: doc, expiresAt: time.Now().Add(documentCacheTTL)}
}

func (s *DocumentStore) redisKey(name string, version int) string {
	if version > 0 {
		return fmt.Sprintf("%s:%s:v%d", documentKeyPrefix, name, version)
	}
	return fmt.Sprintf("%s:%s:%s", documentKeyPrefix, name, s.env)
}

func documentCacheKey(name string, version int) string {
	if version > 0 {
		return fmt.Sprintf("%s:v%d", name, version)
	}
	return name
}

// PromptKey is the canonical "system_message_prompt_key" we persist on
// chunks (matches Python's `{name}_v{version}` format).
func PromptKey(name string, version int) string {
	return fmt.Sprintf("%s_v%d", name, version)
}

func (s *DocumentStore) Close() error {
	if s == nil || s.renderer == nil {
		return nil
	}
	return s.renderer.Close()
}
