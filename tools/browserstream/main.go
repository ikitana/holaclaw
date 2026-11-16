// Command browserstream is a development-only live view of an agent's
// browser: it dials the sidecar's unix socket directly (bypassing the daemon,
// its tickets and the takeover lock entirely) and serves the screencast over
// plain HTTP/WebSocket so it can be pointed at from any browser on the
// network while the sidecar runs locally. It is not built into `holaclaw` and
// ships nothing: `go run ./tools/browserstream`.
//
// It has no authentication of its own — the same trust the sidecar's socket
// permissions already give a local developer — so bind it to an address you
// are comfortable exposing, and never run it anywhere but a dev machine.
//
// When the agent asked for has no browser open yet, or its browser goes away
// mid-stream, the viewer is told and the tool keeps polling until one
// appears. When more than one agent is running, the page lists every one as
// a tab to switch between.
//
// Beside the picture the page shows a timeline of what is going on, fed from
// two places at once. The sidecar's own account comes down the stream itself:
// one entry per observe and per act it served, with the shortlist it returned
// and how each action went — never the text an action typed. The agent's
// account comes from the daemon's event stream (/v1/events over its unix
// socket, which needs no token): the browser.* events, the moves the model
// decided on, the steps, what blocked a run and how it ended. --daemon
// overrides the address, which is otherwise the socket of the config
// directory, and --no-events leaves the second feed out; the viewer works
// without a daemon either way.
package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"holaclaw/internal/api"
	"holaclaw/internal/app"
	"holaclaw/internal/browser/client"
	"holaclaw/internal/browser/proto"
	"holaclaw/internal/config"
	"holaclaw/internal/daemon"
)

// errUsage marks an error as a usage error: main exits 2 for these (bad
// flags) and 1 for everything else, matching the old flag-based exit codes.
var errUsage = errors.New("usage")

func main() {
	os.Exit(execute(os.Args[1:], os.Stdout, os.Stderr))
}

// execute builds the command, runs it and returns the process exit code. It
// is the tool's testable entry point: main just forwards to it.
func execute(args []string, stdout, stderr io.Writer) int {
	cmd := newRootCmd()
	cmd.SetArgs(args)
	cmd.SetOut(stdout)
	cmd.SetErr(stderr)
	if err := cmd.Execute(); err != nil {
		_, _ = fmt.Fprintln(stderr, "browserstream:", err)
		if errors.Is(err, errUsage) {
			return 2
		}
		return 1
	}
	return 0
}

// newRootCmd builds the (single) browserstream command.
func newRootCmd() *cobra.Command {
	var (
		dir        string
		socket     string
		addr       string
		daemonAddr string
		noEvents   bool
	)
	cmd := &cobra.Command{
		Use:           "browserstream",
		Short:         "dev-only live view of an agent's browser, straight off the sidecar's socket",
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return run(cmd.Context(), dir, socket, addr, daemonAddr, noEvents)
		},
	}
	cmd.SetFlagErrorFunc(func(_ *cobra.Command, err error) error { return fmt.Errorf("%w: %v", errUsage, err) })
	cmd.Flags().StringVar(&dir, "config", app.DefaultConfigDir(), "config directory (read for browser.socket)")
	cmd.Flags().StringVar(&socket, "socket", "", "sidecar unix socket path (overrides the config directory)")
	cmd.Flags().StringVar(&addr, "addr", "127.0.0.1:8090", "address to listen on, e.g. 0.0.0.0:8090 to reach it from other machines")
	cmd.Flags().StringVar(&daemonAddr, "daemon", "", "daemon base URL or socket path for the event feed (default: the config directory's socket)")
	cmd.Flags().BoolVar(&noEvents, "no-events", false, "do not show the daemon's browser events, only the sidecar's own activity")
	return cmd
}

// run is the command's body: resolve the socket, start the event feed and
// serve the viewer until the context is cancelled or the server fails.
func run(ctx context.Context, dir, socket, addr, daemonAddr string, noEvents bool) error {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))

	sock := socket
	if sock == "" {
		cfg, err := config.Load(dir)
		if err != nil {
			return err
		}
		if !cfg.Browser.Enabled || cfg.Browser.Socket == "" {
			return fmt.Errorf("%w: no browser.socket in %s; pass --socket instead", errUsage, dir)
		}
		sock = cfg.Browser.Socket
	}

	c := client.New(sock, client.Options{})
	mgr := newManager(c, log)

	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	if !noEvents {
		if hub, err := dialEvents(daemonAddr, dir, log); err != nil {
			log.Warn("browserstream: no daemon event feed", "err", err)
		} else {
			mgr.events = hub
			go hub.run(ctx)
		}
	}

	srv := &http.Server{Addr: addr, Handler: newMux(mgr)}
	go func() {
		<-ctx.Done()
		_ = srv.Close()
	}()

	log.Warn("browserstream: unauthenticated dev viewer — do not expose this beyond a trusted network",
		"addr", addr, "socket", sock)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

// dialEvents builds the daemon client of the event feed. The address is the
// same one the CLI works out (cmd/holaclaw/client.go): the config directory's
// socket, which is admin by file permission and needs no token. Nothing is
// connected here — the hub dials, and keeps retrying, on its own goroutine.
func dialEvents(addr, dir string, log *slog.Logger) (*eventHub, error) {
	if addr == "" {
		a, err := app.Static(dir)
		if err != nil {
			return nil, err
		}
		addr = daemon.SocketPath(a)
	}
	c, err := api.Dial(addr, os.Getenv("HOLACLAW_TOKEN"))
	if err != nil {
		return nil, err
	}
	return newEventHub(c, log), nil
}

// newMux is the tool's whole routing table, split out from main so a test can
// drive it without a real listener or signal handling.
func newMux(mgr *manager) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", serveViewer)
	mux.HandleFunc("GET /sessions", mgr.handleSessions)
	mux.HandleFunc("GET /stream", mgr.handleStream)
	return mux
}

//go:embed viewer.html
var viewerHTML []byte

func serveViewer(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(viewerHTML)
}

// handleSessions lists the sidecar's running browsers as-is, for the page's
// agent tab bar.
func (m *manager) handleSessions(w http.ResponseWriter, r *http.Request) {
	sessions, err := m.sidecar.Sessions(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	if sessions == nil {
		sessions = []proto.Session{}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(sessions)
}
