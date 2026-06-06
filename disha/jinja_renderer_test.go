package disha

import (
	"context"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestPythonJinjaRendererSmoke(t *testing.T) {
	python := os.Getenv("PYTHON_JINJA_TEST_PYTHON")
	if python == "" {
		var err error
		python, err = exec.LookPath("python3")
		if err != nil {
			t.Skip("python3 not available")
		}
	}
	if err := exec.Command(python, "-c", "import jinja2").Run(); err != nil {
		t.Skipf("%s cannot import jinja2: %v", python, err)
	}
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	t.Setenv("JINJA_RENDERER_PYTHON", python)
	t.Setenv("JINJA_RENDERER_SCRIPT", filepath.Join(wd, "..", "jinja_renderer.py"))

	renderer := NewPythonJinjaRenderer(log.New(io.Discard, "", 0))
	t.Cleanup(func() { _ = renderer.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	result, err := renderer.Render(ctx, TemplateRenderRequest{
		DocumentName:    "test",
		DocumentVersion: 1,
		Text:            "{{ x or 'fallback' }} | {{ missing or 'fallback' }} | {% if y is none %}none{% endif %}",
		Variables:       DocumentVariables{"x": "abc", "y": nil},
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if want := "abc | fallback | none"; result.Output != want {
		t.Fatalf("rendered = %q, want %q", result.Output, want)
	}
}
