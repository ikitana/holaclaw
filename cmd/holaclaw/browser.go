package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/spf13/cobra"

	"holaclaw/internal/browser/chromium"
	"holaclaw/internal/browser/page"
	"holaclaw/internal/browser/proto"
	"holaclaw/internal/browser/sidecar"
	"holaclaw/internal/config"
	"holaclaw/internal/daemon"
)

// newBrowserCmd runs the browser sidecar (docs/browser.md): its own process
// and system user, one Chromium per agent, a unix socket and nothing else. It
// is deliberately store-less and secret-less — the config is read statically,
// so this process never opens the database or auth.toml.
func newBrowserCmd(c *cli) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "browser",
		Short: "start the browser sidecar",
		Args:  cobra.NoArgs,
	}
	state := cmd.Flags().String("state", "", "state directory (default $STATE_DIRECTORY, else <data_dir>/browser)")
	debug := cmd.Flags().Bool("debug", false, "verbose logging")
	cmd.RunE = func(cmd *cobra.Command, _ []string) error { return cmdBrowser(cmd, c, state, debug) }
	return cmd
}

func cmdBrowser(cmd *cobra.Command, c *cli, state *string, debug *bool) error {
	dir := &c.configDir
	// config.Load, not app.Static: Static opens auth.toml, and the sidecar
	// must never read a secret it has no use for.
	cfg, err := config.Load(*dir)
	if err != nil {
		return err
	}
	if !cfg.Browser.Enabled {
		return usagef(cmd, "the browser is off in %s (no [browser] table, or browser.enabled = false); nothing to serve", filepath.Join(*dir, config.FileConfig))
	}
	if cfg.Browser.Socket == "" {
		return fmt.Errorf("browser.socket is not set in %s", filepath.Join(*dir, config.FileConfig))
	}

	stateDir := stateDirOf(*state, cfg, *dir)
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return fmt.Errorf("browser state directory: %w", err)
	}

	level := slog.LevelInfo
	if *debug {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(c.stderr, &slog.HandlerOptions{Level: level}))

	engine, err := newEngine(engineOptions{Config: cfg.Browser, StateDir: stateDir, Logger: log})
	if err != nil {
		return err
	}
	srv := sidecar.New(engine, sidecar.Options{
		Logger:   log,
		OnListen: func(addr net.Addr) { log.Info("browser sidecar listening", "socket", addr.String()) },
	})
	return srv.Serve(cmd.Context(), cfg.Browser.Socket)
}

// stateDirOf resolves where the profiles and the download quarantine live:
// --state, else the directory systemd hands us, else under the data dir.
func stateDirOf(flagValue string, cfg *config.Config, configDir string) string {
	if flagValue != "" {
		return flagValue
	}
	if d := os.Getenv("STATE_DIRECTORY"); d != "" {
		return d
	}
	return filepath.Join(dataDirOf(cfg, configDir), "browser")
}

// engineOptions is what the page layer needs to own browsers.
type engineOptions struct {
	Config   config.Browser
	StateDir string
	Logger   *slog.Logger
}

// newEngine builds the engine the sidecar serves: the page layer when a
// browser was found, else a stub whose Health is honest (it really does
// resolve the binary and look for Xvfb) and whose every other method fails
// with `browser`.
func newEngine(opts engineOptions) (sidecar.Engine, error) {
	bin, err := chromium.Find(opts.Config.Binary)
	if err != nil {
		// No browser is not a start failure: /health says what is missing
		// and check reports it; every other call answers `browser`.
		return &stubEngine{cfg: opts.Config}, nil
	}
	return page.New(page.Options{Binary: bin, StateDir: opts.StateDir, Headless: opts.Config.Headless, Logger: opts.Logger})
}

// stubEngine answers health and refuses everything else.
type stubEngine struct{ cfg config.Browser }

var _ sidecar.Engine = (*stubEngine)(nil)

const stubDetail = "page layer not built"

func (e *stubEngine) Health(context.Context) proto.Health {
	h := proto.Health{Version: daemon.Version, Headless: e.cfg.Headless}
	bin, err := chromium.Find(e.cfg.Binary)
	switch {
	case err != nil:
		h.Problem = err.Error()
	case bin.TooOld():
		h.Binary, h.Browser = bin.Path, bin.Version
		h.Problem = fmt.Sprintf("%s is too old: major %d, holaclaw needs %d or newer", bin.Path, bin.Major, chromium.MinMajor)
	default:
		h.Binary, h.Browser = bin.Path, bin.Version
	}
	// Visible is the default, and a visible browser needs the virtual display
	// the sidecar starts itself.
	if h.Problem == "" && !e.cfg.Headless {
		if _, err := exec.LookPath("Xvfb"); err != nil {
			h.Problem = chromium.ErrNoXvfb.Error()
		}
	}
	return h
}

func (e *stubEngine) Sessions(context.Context) []proto.Session { return nil }

func (e *stubEngine) Open(context.Context, string, proto.OpenRequest) (proto.Session, error) {
	return proto.Session{}, sidecar.Errorf(proto.ErrBrowser, stubDetail)
}

func (e *stubEngine) Close(context.Context, string) error {
	return sidecar.Errorf(proto.ErrBrowser, stubDetail)
}

func (e *stubEngine) Observe(context.Context, string, proto.ObserveRequest) (proto.Observation, error) {
	return proto.Observation{}, sidecar.Errorf(proto.ErrBrowser, stubDetail)
}

func (e *stubEngine) Act(context.Context, string, proto.Action) (proto.ActResult, error) {
	return proto.ActResult{}, sidecar.Errorf(proto.ErrBrowser, stubDetail)
}

func (e *stubEngine) Screenshot(context.Context, string) ([]byte, error) {
	return nil, sidecar.Errorf(proto.ErrBrowser, stubDetail)
}

func (e *stubEngine) Download(context.Context, string, string) (io.ReadCloser, proto.Download, error) {
	return nil, proto.Download{}, sidecar.Errorf(proto.ErrBrowser, stubDetail)
}

func (e *stubEngine) Stream(context.Context, string, <-chan proto.StreamMessage, sidecar.Viewer) error {
	return sidecar.Errorf(proto.ErrBrowser, stubDetail)
}
