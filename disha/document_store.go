package disha

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"
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
	redis  RedisClient
	logger *log.Logger
	env    string

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
	if redis == nil {
		return nil
	}
	return &DocumentStore{
		redis:  redis,
		logger: logger,
		env:    resolveDocumentEnvironment(),
		cache:  make(map[string]documentCacheEntry),
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
func (s *DocumentStore) GetDocument(ctx context.Context, name string, version int, variables map[string]string) (string, int, error) {
	if s == nil {
		return "", 0, errors.New("disha: document store is nil")
	}
	doc, err := s.resolve(ctx, name, version)
	if err != nil {
		return "", 0, err
	}
	rendered := renderTemplate(doc.PromptText, variables)
	// renderTemplate only understands bare `{{ var }}` references. If the
	// prompt carries Jinja2 logic (`{% if %}`, filters) or a variable we
	// didn't supply, those tokens survive untouched and would ship into
	// the LLM system prompt silently. Surface that instead of hiding it.
	if s.logger != nil {
		if leftover := unresolvedTemplateTokens(rendered); leftover != "" {
			s.logger.Printf("disha: document %q (version=%d) has unresolved template tokens after render: %s\n", name, doc.Version, leftover)
		}
	}
	return rendered, doc.Version, nil
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

// renderTemplate does a minimal `{{ var }}` substitution. Disha's
// prompts go through full Jinja2 in Python, but the sales-call surface
// only uses bare variable references (`{{ patient_info }}`,
// `{{ current_datetime }}`). If a prompt later starts using
// conditionals or filters we'll need to either swap to a richer engine
// or pre-render server-side and store the rendered prompt in Redis.
var templateVarRe = regexp.MustCompile(`\{\{\s*([A-Za-z_][A-Za-z0-9_]*)\s*\}\}`)

// templateLeftoverRe matches anything that still looks like a Jinja2
// tag after substitution: an unfilled `{{ ... }}` variable or a `{% %}`
// control block we don't render.
var templateLeftoverRe = regexp.MustCompile(`\{\{[^}]*\}\}|\{%[^%]*%\}`)

// unresolvedTemplateTokens returns a short, comma-joined sample of any
// surviving template tokens (capped so a pathological prompt can't
// flood the log), or "" when the render is clean.
func unresolvedTemplateTokens(text string) string {
	matches := templateLeftoverRe.FindAllString(text, 5)
	if len(matches) == 0 {
		return ""
	}
	return strings.Join(matches, ", ")
}

func renderTemplate(text string, variables map[string]string) string {
	if text == "" || len(variables) == 0 {
		return text
	}
	return templateVarRe.ReplaceAllStringFunc(text, func(match string) string {
		name := templateVarRe.FindStringSubmatch(match)[1]
		if val, ok := variables[name]; ok {
			return val
		}
		return match
	})
}
