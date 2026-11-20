package main

import (
	"net/http"
	"strings"
	"testing"
)

// `holaclaw models <provider>` prints the ids a provider serves.

func TestModelsCompat(t *testing.T) {
	srv, hits := modelsServer(t, http.StatusOK, `{"data":[{"id":"qwen3:8b"},{"id":"gemma4:e2b"}]}`)
	dir := compatConfig(t, srv.URL, "gemma4:e2b")
	code, out, _ := runCLI(t, "", "models", "main", "--config", dir)
	if code != 0 || out != "gemma4:e2b\nqwen3:8b\n" {
		t.Fatalf("exit = %d, want 0 and the sorted ids\n%s", code, out)
	}
	if hits.Load() != 1 {
		t.Errorf("model list requests = %d, want 1", hits.Load())
	}
}

func TestModelsCodex(t *testing.T) {
	hits := codexUpstream(t, http.StatusOK, codexLive)
	dir := codexConfig(t, "gpt-6-luna", true)
	code, out, _ := runCLI(t, "", "models", "main", "--config", dir)
	if code != 0 || out != "gpt-6-luna\ngpt-6.1-sol\n" {
		t.Fatalf("exit = %d, want 0 and the sorted ids\n%s", code, out)
	}
	if hits.Load() != 1 {
		t.Errorf("model list requests = %d, want 1", hits.Load())
	}
}

func TestModelsFailures(t *testing.T) {
	t.Run("unknown provider", func(t *testing.T) {
		srv, hits := modelsServer(t, http.StatusOK, servedModels)
		dir := compatConfig(t, srv.URL, "gemma4:e2b")
		code, out, errOut := runCLI(t, "", "models", "nope", "--config", dir)
		if code != 1 || out != "" || !strings.Contains(errOut, `unknown provider "nope"`) || hits.Load() != 0 {
			t.Fatalf("exit = %d, requests = %d, want 1 and 0\n%s%s", code, hits.Load(), out, errOut)
		}
	})

	t.Run("the endpoint refuses", func(t *testing.T) {
		srv, _ := modelsServer(t, http.StatusUnauthorized, `{"error":"no"}`)
		dir := compatConfig(t, srv.URL, "gemma4:e2b")
		code, out, errOut := runCLI(t, "", "models", "main", "--config", dir)
		if code != 1 || out != "" || errOut == "" {
			t.Fatalf("exit = %d, want 1 and an error\n%s%s", code, out, errOut)
		}
	})

	t.Run("codex not signed in: no request", func(t *testing.T) {
		hits := codexUpstream(t, http.StatusOK, codexLive)
		dir := codexConfig(t, "gpt-6.1-luna", false)
		code, out, errOut := runCLI(t, "", "models", "main", "--config", dir)
		if code != 1 || out != "" || !strings.Contains(errOut, "not logged in") || hits.Load() != 0 {
			t.Fatalf("exit = %d, requests = %d, want 1 and 0\n%s%s", code, hits.Load(), out, errOut)
		}
	})

	t.Run("no model list", func(t *testing.T) {
		hits := codexUpstream(t, http.StatusOK, codexLive)
		dir := modelsConfig(t, "type = \"codex\"\nauth = \"api-key\"", "gpt-6.1-luna")
		code, out, errOut := runCLI(t, "", "models", "main", "--config", dir)
		if code != 1 || out != "" || !strings.Contains(errOut, "cannot list its models") || hits.Load() != 0 {
			t.Fatalf("exit = %d, requests = %d, want 1 and 0\n%s%s", code, hits.Load(), out, errOut)
		}
	})

	t.Run("no provider named", func(t *testing.T) {
		if code, _, _ := runCLI(t, "", "models"); code != 2 {
			t.Fatalf("exit = %d, want 2", code)
		}
	})
}
