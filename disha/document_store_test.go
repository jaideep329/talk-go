package disha

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"strings"
	"testing"

	"github.com/alicebob/miniredis/v2"
)

type simpleTemplateRenderer struct{}

func (simpleTemplateRenderer) Render(_ context.Context, req TemplateRenderRequest) (TemplateRenderResult, error) {
	out := req.Text
	for key, val := range req.Variables {
		rendered := fmt.Sprint(val)
		out = strings.ReplaceAll(out, "{{ "+key+" }}", rendered)
		out = strings.ReplaceAll(out, "{{"+key+"}}", rendered)
	}
	return TemplateRenderResult{Output: out}, nil
}

func (simpleTemplateRenderer) Close() error {
	return nil
}

func TestDocumentStoreUsesInjectedTemplateEngine(t *testing.T) {
	t.Setenv("ENVIRONMENT", "prod")
	redisServer := miniredis.RunT(t)
	redisClient := NewRedisClient(redisServer.Addr(), "", 0, log.New(io.Discard, "", 0))
	t.Cleanup(func() { _ = redisClient.Close() })

	doc := DocumentVersion{
		ID:         "doc-1",
		PromptText: "Hi {{ name }}",
		Version:    3,
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("Marshal document: %v", err)
	}
	redisServer.Set("document:test/prompt:production", string(raw))

	store := newDocumentStore(redisClient, log.New(io.Discard, "", 0), simpleTemplateRenderer{})
	got, version, err := store.GetDocument(context.Background(), "test/prompt", 0, DocumentVariables{"name": "Riya"})
	if err != nil {
		t.Fatalf("GetDocument: %v", err)
	}
	if version != 3 {
		t.Fatalf("version = %d, want 3", version)
	}
	if want := "Hi Riya"; got != want {
		t.Fatalf("rendered prompt = %q, want %q", got, want)
	}
}
