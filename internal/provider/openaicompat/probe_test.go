package openaicompat

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"testing"

	"holaclaw/internal/provider"
)

func TestProbeContextOllamaNumCtx(t *testing.T) {
	var body map[string]string
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/show" || r.Method != http.MethodPost {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		_, _ = io.WriteString(w, `{"parameters":"stop \"<eot>\"\nnum_ctx 8192","model_info":{"gemma.context_length":131072}}`)
	}), Options{})

	n, src, err := c.ProbeContext(context.Background(), provider.Model{ID: "gemma"})
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if n != 8192 || src != SourceOllamaNumCtx {
		t.Errorf("probe = (%d, %q)", n, src)
	}
	if body["model"] != "gemma" {
		t.Errorf("request body = %v", body)
	}
}

func TestProbeContextOllamaModelInfo(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/show" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = io.WriteString(w, `{"parameters":"temperature 0.7","model_info":{"llama.context_length":4096}}`)
	}), Options{})
	n, src, err := c.ProbeContext(context.Background(), provider.Model{ID: "llama"})
	if err != nil || n != 4096 || src != SourceOllamaModelInfo {
		t.Fatalf("probe = (%d, %q, %v)", n, src, err)
	}
}

func TestProbeContextLlamaCPPProps(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/props":
			_, _ = io.WriteString(w, `{"default_generation_settings":{"n_ctx":16384}}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}), Options{})
	n, src, err := c.ProbeContext(context.Background(), provider.Model{ID: "any"})
	if err != nil || n != 16384 || src != SourceLlamaCppProps {
		t.Fatalf("probe = (%d, %q, %v)", n, src, err)
	}
}

func TestProbeContextUnsupported(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}), Options{})
	_, _, err := c.ProbeContext(context.Background(), provider.Model{ID: "any"})
	if !errors.Is(err, ErrProbeUnsupported) {
		t.Fatalf("err = %v", err)
	}
}

func TestProbeContextIgnoresJunk(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `<html>not json</html>`)
	}), Options{})
	_, _, err := c.ProbeContext(context.Background(), provider.Model{ID: "any"})
	if !errors.Is(err, ErrProbeUnsupported) {
		t.Fatalf("err = %v", err)
	}
}
