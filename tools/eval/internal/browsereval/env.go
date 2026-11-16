package browsereval

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http/httptest"
	"net/url"
	"os"

	"holaclaw/internal/browser/chromium"
	"holaclaw/internal/browser/page"
	"holaclaw/tools/eval/internal/sites"
)

// The world one eval run needs: the synthetic sites on a loopback listener,
// and one page engine (an in-process browser.Sidecar — no sidecar process is
// started, the eval is not testing the socket) driving a real Chromium.

// ErrNoBrowser says no browser could be resolved. The runner prints a skip
// message and exits 0, exactly like a model whose key_env is unset.
var ErrNoBrowser = errors.New("eval: no browser found")

// EnvOptions configures NewEnv.
type EnvOptions struct {
	// Binary is a browser path; "" searches PATH (chromium.Find).
	Binary string
	// Headless opts into --headless=new instead of the Xvfb display the
	// manager starts itself. Both are in the dev shell.
	Headless bool
	Logger   *slog.Logger
	// Pacing scales the page layer's human-like delays; nil is a person,
	// which is what a measured run wants. Tests set zero.
	Pacing *float64
	// NoSites skips the synthetic site server (--real): the run never
	// touches a Task.Site, only Task.Real entries with their own fixed URLs.
	NoSites bool
}

// Env is the site server plus the page engine, and where to reach them.
type Env struct {
	Server  *httptest.Server
	Engine  *page.Engine
	Browser chromium.Binary
	// Host is the loopback host without its port: the agent's allow-list.
	Host string

	stateDir string
}

// NewEnv starts the sites and builds the engine. It starts no browser: the
// first Open does. A missing or too-old browser comes back as ErrNoBrowser,
// with nothing left running.
func NewEnv(o EnvOptions) (*Env, error) {
	bin, err := chromium.Find(o.Binary)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrNoBrowser, err)
	}
	if bin.TooOld() {
		return nil, fmt.Errorf("%w: %s is older than major %d", ErrNoBrowser, bin, chromium.MinMajor)
	}
	dir, err := os.MkdirTemp("", "holaclaw-browsereval-")
	if err != nil {
		return nil, fmt.Errorf("eval: state dir: %w", err)
	}
	var srv *httptest.Server
	host := ""
	if !o.NoSites {
		srv = httptest.NewServer(sites.Handler())
		u, err := url.Parse(srv.URL)
		if err != nil { // httptest.Server.URL is always parseable; be explicit anyway
			srv.Close()
			_ = os.RemoveAll(dir)
			return nil, fmt.Errorf("eval: site server url: %w", err)
		}
		host = u.Hostname()
	}
	eng, err := page.New(page.Options{
		Binary:   bin,
		StateDir: dir,
		Headless: o.Headless,
		Logger:   o.Logger,
		Pacing:   o.Pacing,
	})
	if err != nil {
		if srv != nil {
			srv.Close()
		}
		_ = os.RemoveAll(dir)
		return nil, fmt.Errorf("eval: page engine: %w", err)
	}
	return &Env{Server: srv, Engine: eng, Browser: bin, Host: host, stateDir: dir}, nil
}

// StartURL is where one run of a task begins: the site's own path, scoped to
// the run id every handler keys its state by.
func (e *Env) StartURL(s sites.Site, run string) string {
	return e.Server.URL + s.Path + "?run=" + url.QueryEscape(run)
}

// Close stops every browser the engine started, the site server, and deletes
// the profiles and quarantined downloads with the state directory.
func (e *Env) Close(ctx context.Context) {
	if e.Engine != nil {
		_ = e.Engine.Shutdown(ctx)
	}
	if e.Server != nil {
		e.Server.Close()
	}
	if e.stateDir != "" {
		_ = os.RemoveAll(e.stateDir)
	}
}
