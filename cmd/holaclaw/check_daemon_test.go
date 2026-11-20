package main

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// `check` and `models` ask the running daemon for a provider's model ids,
// and the provider itself only when no daemon can say (docs/models.md §9).

func compatTOML(baseURL string) string {
	return fmt.Sprintf("type = \"openai-compatible\"\nbase_url = %q", baseURL+"/v1")
}

// modelsDaemon runs a daemon whose one model, gemma, is on the
// OpenAI-compatible provider main at baseURL.
func modelsDaemon(t *testing.T, baseURL, id string) *daemonEnv {
	t.Helper()
	e := &daemonEnv{dir: fixture(t, "minimal"), socket: shortSocket(t)}
	writeModelsConfig(t, e.dir, e.socket, "main", compatTOML(baseURL), id)
	return runDaemon(t, e, true)
}

// deadURL is an endpoint nothing listens on.
func deadURL(t *testing.T) string {
	t.Helper()
	srv, _ := modelsServer(t, http.StatusOK, servedModels)
	srv.Close()
	return srv.URL
}

// The config on disk now names a dead endpoint: only the daemon, which
// started on the live one, can answer.
func TestCheckModelThroughTheDaemon(t *testing.T) {
	srv, _ := modelsServer(t, http.StatusOK, servedModels)
	e := modelsDaemon(t, srv.URL, "gemma4:e4b")
	writeModelsConfig(t, e.dir, e.socket, "main", compatTOML(deadURL(t)), "gemma4:e4b")

	code, out, _ := runCLI(t, "", "check", "--config", e.dir, "--only", "MOD003")
	if code != 2 {
		t.Fatalf("exit = %d, want 2\n%s", code, out)
	}
	want := `MOD003 error: config.toml: models.gemma.id: main does not serve "gemma4:e4b"; it serves: gemma4:e2b, qwen3:8b`
	if !strings.Contains(out, want) {
		t.Errorf("no MOD003 from the daemon's list:\n%s", out)
	}

	writeModelsConfig(t, e.dir, e.socket, "main", compatTOML(deadURL(t)), "gemma4:e2b")
	code, out, _ = runCLI(t, "", "check", "--config", e.dir, "--only", "MOD003")
	if code != 0 || strings.Contains(out, "MOD003") {
		t.Fatalf("exit = %d, want 0 and no MOD003\n%s", code, out)
	}

	code, out, _ = runCLI(t, "", "models", "main", "--config", e.dir)
	if code != 0 || out != "gemma4:e2b\nqwen3:8b\n" {
		t.Fatalf("models: exit = %d, want 0 and the sorted ids\n%s", code, out)
	}
}

// A list the daemon cannot read is never an error, and the provider is not
// asked a second time from here.
func TestCheckModelDaemonCannotList(t *testing.T) {
	e := modelsDaemon(t, deadURL(t), "gemma4:e4b")
	srv, hits := modelsServer(t, http.StatusOK, servedModels)
	writeModelsConfig(t, e.dir, e.socket, "main", compatTOML(srv.URL), "gemma4:e4b")

	code, out, _ := runCLI(t, "", "check", "--config", e.dir, "--only", "MOD003")
	if code != 0 {
		t.Fatalf("exit = %d, want 0\n%s", code, out)
	}
	if !strings.Contains(out, "MOD003 info: config.toml: providers.main: could not list the models of main: provider main:") {
		t.Errorf("no MOD003 info with the provider's error:\n%s", out)
	}
	code, out, errOut := runCLI(t, "", "models", "main", "--config", e.dir)
	if code != 1 || out != "" || !strings.Contains(errOut, "provider main:") {
		t.Fatalf("models: exit = %d, want 1 and the provider's error\n%s%s", code, out, errOut)
	}
	if hits.Load() != 0 {
		t.Errorf("the provider was asked directly %d times beside a running daemon", hits.Load())
	}
}

// A daemon that started before the config had the provider cannot say: the
// provider is asked from here.
func TestCheckModelDaemonLacksTheProvider(t *testing.T) {
	srv, _ := modelsServer(t, http.StatusOK, servedModels)
	e := modelsDaemon(t, deadURL(t), "gemma4:e4b")
	writeModelsConfig(t, e.dir, e.socket, "late", compatTOML(srv.URL), "gemma4:e4b")

	code, out, _ := runCLI(t, "", "check", "--config", e.dir, "--only", "MOD003")
	if code != 2 {
		t.Fatalf("exit = %d, want 2\n%s", code, out)
	}
	if !strings.Contains(out, `models.gemma.id: late does not serve "gemma4:e4b"; it serves: gemma4:e2b, qwen3:8b`) {
		t.Errorf("no MOD003 from the provider's own list:\n%s", out)
	}
	code, out, _ = runCLI(t, "", "models", "late", "--config", e.dir)
	if code != 0 || out != "gemma4:e2b\nqwen3:8b\n" {
		t.Fatalf("models: exit = %d, want 0 and the sorted ids\n%s", code, out)
	}
}

// A provider that cannot list its models has no finding through the daemon
// either, and `models` says so.
func TestCheckModelDaemonNoModelList(t *testing.T) {
	e := &daemonEnv{dir: fixture(t, "minimal"), socket: shortSocket(t)}
	writeModelsConfig(t, e.dir, e.socket, "main", "type = \"codex\"\nauth = \"api-key\"", "gpt-6.1-luna")
	runDaemon(t, e, true)

	code, out, _ := runCLI(t, "", "check", "--config", e.dir, "--only", "MOD003")
	if code != 0 || strings.Contains(out, "MOD003") {
		t.Fatalf("exit = %d, want 0 and no MOD003\n%s", code, out)
	}
	code, out, errOut := runCLI(t, "", "models", "main", "--config", e.dir)
	if code != 1 || out != "" || !strings.Contains(errOut, "cannot list its models") {
		t.Fatalf("models: exit = %d, want 1\n%s%s", code, out, errOut)
	}
}

// --offline asks nobody, the daemon included.
func TestCheckModelOfflineBesideTheDaemon(t *testing.T) {
	srv, _ := modelsServer(t, http.StatusOK, servedModels)
	e := modelsDaemon(t, srv.URL, "gemma4:e4b")
	code, out, _ := runCLI(t, "", "check", "--config", e.dir, "--only", "MOD003", "--offline")
	if code != 0 || strings.Contains(out, "MOD003") {
		t.Fatalf("exit = %d, want 0 and no MOD003\n%s", code, out)
	}
}
