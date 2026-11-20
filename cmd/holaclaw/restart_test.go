package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// restartServer answers what `restart` asks of a daemon, and counts the
// requests and the restarts.
func restartServer(t *testing.T, status int) (addr string, requests, restarts *atomic.Int32) {
	t.Helper()
	requests, restarts = new(atomic.Int32), new(atomic.Int32)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		switch r.Method + " " + r.URL.Path {
		case "GET /v1/status":
			_, _ = w.Write([]byte(`{"version":"test","uptime_seconds":1,"sessions_active":0}`))
		case "POST /v1/restart":
			if status != http.StatusAccepted {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(status)
				_, _ = w.Write([]byte(`{"code":"forbidden","message":"admin scope required"}`))
				return
			}
			restarts.Add(1)
			w.WriteHeader(status)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL, requests, restarts
}

func TestRestart(t *testing.T) {
	addr, _, restarts := restartServer(t, http.StatusAccepted)
	code, out, errOut := runCLI(t, "", "restart", "--addr", addr, "--config", t.TempDir())
	if code != 0 || out != "the daemon is restarting\n" || errOut != "" || restarts.Load() != 1 {
		t.Fatalf("exit = %d, stdout = %q, stderr = %q, %d restarts", code, out, errOut, restarts.Load())
	}
}

func TestRestartFails(t *testing.T) {
	// The daemon refuses.
	addr, _, _ := restartServer(t, http.StatusForbidden)
	code, out, errOut := runCLI(t, "", "restart", "--addr", addr, "--config", t.TempDir())
	if code != 1 || out != "" || !strings.Contains(errOut, "forbidden") {
		t.Fatalf("refused: exit = %d, stdout = %q, stderr = %q", code, out, errOut)
	}

	// A daemon that `holaclaw run` did not start cannot restart itself.
	e := startDaemon(t, "", "")
	code, _, errOut = runCLI(t, "", "restart", "--config", e.dir)
	if code != 1 || !strings.Contains(errOut, "not_implemented") {
		t.Fatalf("no restart hook: exit = %d, stderr = %q", code, errOut)
	}

	// No daemon.
	down := &daemonEnv{dir: t.TempDir(), socket: shortSocket(t), model: e.model}
	writeDaemonConfig(t, down, "", "")
	code, out, errOut = runCLI(t, "", "restart", "--config", down.dir)
	if code != 1 || out != "" || !strings.Contains(errOut, "daemon is not running") {
		t.Fatalf("no daemon: exit = %d, stdout = %q, stderr = %q", code, out, errOut)
	}
}

// In the container image it is the container that restarts: the daemon is
// not asked.
func TestRestartInAContainer(t *testing.T) {
	t.Setenv("HOLACLAW_CONTAINER", "1")
	addr, requests, _ := restartServer(t, http.StatusAccepted)
	code, out, errOut := runCLI(t, "", "restart", "--addr", addr, "--config", t.TempDir())
	if code != 1 || out != "" || requests.Load() != 0 ||
		errOut != "holaclaw: this is a container: restart it from the host instead (`docker restart <container>`), so that its entrypoint runs again\n" {
		t.Fatalf("exit = %d, stdout = %q, stderr = %q, %d requests", code, out, errOut, requests.Load())
	}
}
