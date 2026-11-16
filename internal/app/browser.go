package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"holaclaw/internal/browser"
	"holaclaw/internal/browser/client"
	"holaclaw/internal/browser/proto"
	"holaclaw/internal/provider"
	"holaclaw/internal/tool/tools"
)

// The daemon half of docs/browser.md as the app assembles it: one sidecar
// client for the whole process and one executor per agent, both built on
// first use. Approvals and the live view live in the daemon, so it injects
// them through SetBrowserHumans; without them there is no browser, because a
// run that cannot ask a person is not the browser this design describes.

// browserCloseTimeout bounds the shutdown call that stops the agent browsers
// this process opened. A sidecar that does not answer is not worth waiting
// for: it idle-stops them on its own.
const browserCloseTimeout = 5 * time.Second

// errBrowserNeedsDaemon is what a browser call gets outside the daemon. It is
// not ErrNoBrowser: the browser is configured, there is simply nobody to
// approve a purchase or take over a login.
var errBrowserNeedsDaemon = errors.New("the browser needs the daemon: start it with `holaclaw run`")

// browserState is the lazily built wiring. It is a field of App rather than a
// package variable so tests and `holaclaw check` can hold several apps.
type browserState struct {
	mu     sync.Mutex
	client *client.Client
	execs  map[string]*browser.Executor
	humans browser.Humans
}

// SetBrowserHumans installs the approval and takeover side of the executor.
// Only the daemon has one; it must be set before the first browser.do call.
func (a *App) SetBrowserHumans(h browser.Humans) {
	a.browser.mu.Lock()
	defer a.browser.mu.Unlock()
	a.browser.humans = h
}

// BrowserClient returns the one sidecar client, or nil when no [browser]
// table is configured. Nothing is dialled until the first call.
func (a *App) BrowserClient() *client.Client {
	if a.Config == nil || !a.Config.Browser.Enabled {
		return nil
	}
	a.browser.mu.Lock()
	defer a.browser.mu.Unlock()
	return a.browserClientLocked()
}

func (a *App) browserClientLocked() *client.Client {
	if a.browser.client == nil {
		a.browser.client = client.New(a.Config.Browser.Socket, client.Options{})
	}
	return a.browser.client
}

// closeBrowser stops the agent browsers this process opened. Close calls it,
// so a daemon shutdown does not leave a Chromium up holding a takeover lock.
func (a *App) closeBrowser() {
	a.browser.mu.Lock()
	c, execs := a.browser.client, a.browser.execs
	a.browser.client, a.browser.execs = nil, nil
	a.browser.mu.Unlock()
	if c == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), browserCloseTimeout)
	defer cancel()
	for agentName := range execs {
		_ = c.Close(ctx, agentName)
	}
}

// browserBackend is tools.BrowserBackend: the tool's view of the executor,
// the sidecar client and the agent's configuration.
type browserBackend struct{ a *App }

func (b browserBackend) Run(ctx context.Context, t browser.Task) (browser.Result, error) {
	e, err := b.a.browserExecutor(t.Agent)
	if err != nil {
		return browser.Result{}, err
	}
	return e.Run(ctx, t)
}

func (b browserBackend) Download(ctx context.Context, agentName string, d proto.Download) (io.ReadCloser, error) {
	c := b.a.BrowserClient()
	if c == nil {
		return nil, tools.ErrNoBrowser
	}
	return c.Download(ctx, agentName, d.ID)
}

// Allow is the agent's navigation allow-list; empty means any site, which the
// sidecar reads as "no list" (docs/browser.md).
func (b browserBackend) Allow(agentName string) []string {
	if b.a.Config == nil {
		return nil
	}
	return b.a.Config.Agents[agentName].Browser.Allow
}

// browserExecutor returns the agent's executor, building it once. The roles are
// the ones docs/browser.md names: `plan` (M11) is the model that chooses the moves, falling
// back to `chat` when unset; `chat` also extracts prose answers; `decide` — a
// decision model, which the config schema already enforces for that role —
// binds elements and scores what each move did, and without it the chat model
// does (slower, works); `vision` is the late rung.
func (a *App) browserExecutor(agentName string) (*browser.Executor, error) {
	if a.Config == nil || !a.Config.Browser.Enabled {
		return nil, tools.ErrNoBrowser
	}
	ac, ok := a.Config.Agents[agentName]
	if !ok {
		return nil, tools.ErrNoBrowser
	}
	a.browser.mu.Lock()
	defer a.browser.mu.Unlock()
	if e, ok := a.browser.execs[agentName]; ok {
		return e, nil
	}
	if a.browser.humans == nil {
		return nil, errBrowserNeedsDaemon
	}
	chat, err := a.ModelRef(ac.Models.Chat)
	if err != nil {
		return nil, err
	}
	o := browser.Options{
		Sidecar:   a.browserClientLocked(),
		Humans:    a.browser.humans,
		Chat:      chat.Provider,
		ChatModel: chat.Model,
		Events:    sinkProxy{a},
	}
	if role := ac.Models.Decide; role != "" {
		m, err := a.Config.Model(role)
		if err != nil {
			return nil, err
		}
		if m.API == provider.APIDecisions {
			d, ok := a.Deciders[m.Provider]
			if !ok {
				return nil, fmt.Errorf("model %s: provider %q does not serve decision models", role, m.Provider)
			}
			o.Decider, o.DecideModel = d, m
		}
	}
	if role := ac.Models.Vision; role != "" {
		ref, err := a.ModelRef(role)
		if err != nil {
			return nil, err
		}
		o.Vision, o.VisionModel = ref.Provider, ref.Model
	}
	if role := ac.Models.Plan; role != "" {
		ref, err := a.ModelRef(role)
		if err != nil {
			return nil, err
		}
		o.Plan, o.PlanModel = ref.Provider, ref.Model
	}
	e, err := browser.NewExecutor(o)
	if err != nil {
		return nil, err
	}
	if a.browser.execs == nil {
		a.browser.execs = map[string]*browser.Executor{}
	}
	a.browser.execs[agentName] = e
	return e, nil
}
