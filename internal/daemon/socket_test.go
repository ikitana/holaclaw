package daemon

import (
	"testing"

	"holaclaw/internal/app"
	"holaclaw/internal/config"
)

func TestSocketPath(t *testing.T) {
	a := &app.App{Config: &config.Config{}}
	if got := SocketPath(a); got != app.DefaultSocket() {
		t.Errorf("SocketPath() = %q, want the default %q", got, app.DefaultSocket())
	}

	a.Config.Server.Socket = "/tmp/custom.sock"
	if got := SocketPath(a); got != "/tmp/custom.sock" {
		t.Errorf("SocketPath() = %q, want server.socket", got)
	}
}
