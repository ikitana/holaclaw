package daemon

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"holaclaw/internal/api"
	"holaclaw/internal/api/gen"
	"holaclaw/internal/config"
	"holaclaw/internal/connector/mcp/mcptest"
	"holaclaw/internal/event"
	"holaclaw/internal/testmodel"
)

// The daemon wires the manager: a create over the API connects an open
// server, writes connectors.toml, streams connector.status and hands the
// manager to the agents.
func TestConnectorCreateIsWired(t *testing.T) {
	srv := mcptest.New(t, mcptest.Options{Tools: []mcptest.Tool{mcptest.TextTool("echo", "hi")}})
	e := start(t, opts{})
	s := e.watch("")
	got, err := e.client.CreateConnector(e.ctx, gen.ConnectorCreate{Id: "docs", Url: srv.URL})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if got.Connector.State != "connected" || got.AuthorizationUrl != nil {
		t.Fatalf("create = %+v", got)
	}
	if ev := s.await(event.ConnectorStatus); attr(t, ev, "id") != "docs" {
		t.Errorf("event = %+v", ev)
	}
	body, err := os.ReadFile(filepath.Join(e.dir, config.FileConnectors))
	if err != nil || !strings.Contains(string(body), "[connectors.docs]") {
		t.Errorf("connectors.toml = %q, %v", body, err)
	}
	if e.daemon.App.Connectors == nil {
		t.Error("the agents have no connector manager")
	}
}

// An id that is a built-in tool or group is refused before any network
// call: its tools would shadow the built-ins.
func TestConnectorBuiltinIDRefused(t *testing.T) {
	e := start(t, opts{})
	for _, id := range []string{"memory", "exec"} {
		_, err := e.client.CreateConnector(e.ctx, gen.ConnectorCreate{Id: id, Url: "http://127.0.0.1:1/mcp"})
		var ce *api.ClientError
		if !errors.As(err, &ce) || ce.Status != http.StatusBadRequest || !strings.Contains(ce.Message, "built-in") {
			t.Errorf("create %s: %v", id, err)
		}
	}
}

// A connector in connectors.toml is checked at start: an open server
// leaves pending for connected with no API call.
func TestConnectorCheckedAtStart(t *testing.T) {
	srv := mcptest.New(t, mcptest.Options{Tools: []mcptest.Tool{mcptest.TextTool("echo", "hi")}})
	model := testmodel.New(t)
	dir := t.TempDir()
	e := &env{t: t, dir: dir, dataDir: filepath.Join(dir, "data"), socket: shortSocket(t), model: model, ctx: context.Background()}
	writeConfig(t, e, opts{}, model.URL())
	write(t, filepath.Join(dir, config.FileConnectors),
		fmt.Sprintf("version = 1\n\n[connectors.docs]\nauth = 'none'\nurl = %q\n", srv.URL))
	e.launch()

	// Replayed from the start: the check may well have finished already.
	stream, err := e.client.Events(e.ctx, "", "0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stream.Close() })
	got := make(chan string, 1)
	go func() {
		for {
			ev, err := stream.Next()
			if err != nil {
				close(got)
				return
			}
			if ev.Type == string(event.ConnectorStatus) && ev.Attrs != nil && (*ev.Attrs)["id"] == "docs" && (*ev.Attrs)["state"] == "connected" {
				got <- "connected"
				return
			}
		}
	}()
	select {
	case s, ok := <-got:
		if !ok || s != "connected" {
			t.Fatal("the stream ended before docs was connected")
		}
	case <-time.After(wait):
		t.Fatal("docs was not checked at start")
	}
}

func TestBuiltinNames(t *testing.T) {
	got := builtinNames([]string{"exec", "memory.save", "memory.search"})
	for _, n := range []string{"exec", "memory", "memory.save"} {
		if !got[n] {
			t.Errorf("%s not reserved", n)
		}
	}
	if got["docs"] {
		t.Error("docs reserved")
	}
}
