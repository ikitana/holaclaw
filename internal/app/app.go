// Package app assembles the pieces — config, secrets, store, providers,
// tools, agent runners — for the CLI and the daemon. It holds no logic of its
// own beyond wiring.
package app

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"holaclaw/internal/agent"
	"holaclaw/internal/auth/secrets"
	"holaclaw/internal/config"
	"holaclaw/internal/connector"
	"holaclaw/internal/event"
	memsqlite "holaclaw/internal/memory/sqlite"
	"holaclaw/internal/provider"
	"holaclaw/internal/provider/codex"
	"holaclaw/internal/provider/decisions"
	"holaclaw/internal/provider/images"
	"holaclaw/internal/provider/openaicompat"
	"holaclaw/internal/store"
	"holaclaw/internal/tool"
	"holaclaw/internal/tool/tools"
)

// Default locations: the config directory when neither --config nor
// $HOLACLAW_CONFIG names one, the secrets directory when server.secrets_dir
// is unset, and the control socket when server.socket is.
const (
	defaultConfigDir  = "/etc/holaclaw"
	defaultSecretsDir = "/etc/holaclaw-secrets"
	defaultSocket     = "/run/holaclaw/holaclaw.sock"
)

// systemConfigDir and euid stand for the machine, so tests can replace them.
var (
	systemConfigDir = defaultConfigDir
	euid            = os.Geteuid
)

// ownUser reports whether the defaults are those of an install as one's own
// user: not root, and no system install in /etc/holaclaw.
func ownUser() bool {
	if euid() == 0 {
		return false
	}
	_, err := os.Stat(systemConfigDir)
	return errors.Is(err, fs.ErrNotExist)
}

// DefaultConfigDir returns $HOLACLAW_CONFIG, else /etc/holaclaw. For one's own
// user (ownUser) it is $XDG_CONFIG_HOME/holaclaw or ~/.config/holaclaw instead.
func DefaultConfigDir() string {
	if d := os.Getenv("HOLACLAW_CONFIG"); d != "" {
		return d
	}
	if ownUser() {
		if d, err := os.UserConfigDir(); err == nil {
			return filepath.Join(d, "holaclaw")
		}
	}
	return defaultConfigDir
}

// DefaultSocket is /run/holaclaw/holaclaw.sock, which only root can create.
// For one's own user (ownUser) it is $XDG_RUNTIME_DIR/holaclaw.sock instead,
// when that is set: a systemd user unit and a login session both set it.
func DefaultSocket() string {
	if d := os.Getenv("XDG_RUNTIME_DIR"); d != "" && ownUser() {
		return filepath.Join(d, "holaclaw.sock")
	}
	return defaultSocket
}

// SecretsPath is auth.toml inside server.secrets_dir, else inside
// /etc/holaclaw-secrets. Unlike data_dir, secrets_dir is always absolute: the
// config refuses a relative one.
func SecretsPath(configured string) string {
	if configured == "" {
		configured = defaultSecretsDir
	}
	return filepath.Join(configured, config.FileAuth)
}

// DataDir is server.data_dir, else the data directory inside configDir. A
// relative data_dir is relative to configDir, not to wherever the command
// runs, so `holaclaw init` and the daemon always agree on the database.
func DataDir(configDir, configured string) string {
	switch {
	case configured == "":
		return filepath.Join(configDir, "data")
	case filepath.IsAbs(configured):
		return configured
	}
	return filepath.Join(configDir, configured)
}

// App is the assembled application.
type App struct {
	Config    *config.Config
	Secrets   *secrets.File
	Store     *store.SQLite
	Registry  *tool.Registry
	Providers map[string]provider.Provider
	// Deciders and Imagers are the decision and image transports of the
	// providers that have them, by provider name (docs/models.md).
	Deciders map[string]provider.Decider
	Imagers  map[string]provider.ImageGenerator
	DataDir  string
	// Memory is the agent memory backend; Events is where tools and runners
	// emit. Open sets both (Events defaults to the store).
	Memory *memsqlite.Store
	Events event.Sink
	// Tasks is set by the daemon; task.create reaches it through a proxy.
	Tasks tools.TaskCreator
	// Connectors is set by the daemon; nil means no connector tools, as in
	// the Static apps of prompt-size, check and the CLI chat.
	Connectors *connector.Manager
	// Reacter is set by the daemon, which owns the channels; react reaches
	// it through a proxy.
	Reacter tools.Reacter
	// Codex holds the subscription providers by name, for the login flow.
	Codex map[string]*codex.Provider
	// Logger receives skill load problems; nil means slog.Default().
	Logger *slog.Logger
	// browser is the lazily built browser wiring (docs/browser.md); see browser.go.
	browser browserState
	// skills caches the eligible skills per run; see skills.go.
	skills skillState
}

// Static loads what needs no state on disk: config, secrets (read-only use)
// and the tool registry. `prompt-size` and `check` use it.
func Static(configDir string) (*App, error) {
	cfg, err := config.Load(configDir)
	if err != nil {
		return nil, err
	}
	a := &App{Config: cfg, Registry: tool.NewRegistry(), Providers: map[string]provider.Provider{}, Deciders: map[string]provider.Decider{}, Imagers: map[string]provider.ImageGenerator{}, Codex: map[string]*codex.Provider{}}
	a.DataDir = DataDir(configDir, cfg.Server.DataDir)
	if a.Secrets, err = secrets.Open(SecretsPath(cfg.Server.SecretsDir)); err != nil {
		return nil, err
	}
	if err := registerTools(a); err != nil {
		return nil, err
	}
	if err := config.Validate(cfg, a.Registry.Names()); err != nil {
		return nil, err
	}
	return a, nil
}

// Open is Static plus the store and the providers.
func Open(ctx context.Context, configDir string) (*App, error) {
	a, err := Static(configDir)
	if err != nil {
		return nil, err
	}
	if a.Store, err = store.Open(ctx, a.DataDir); err != nil {
		return nil, err
	}
	names := make([]string, 0, len(a.Config.Providers))
	for n := range a.Config.Providers {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		p, err := a.newProvider(a.Config.Providers[n])
		if err != nil {
			_ = a.Store.Close()
			return nil, err
		}
		a.Providers[n] = p
		if pc := a.Config.Providers[n]; pc.Type == "openai-compatible" {
			a.Deciders[n] = decisions.New(decisions.Options{Name: pc.Name, APIKey: a.apiKey(pc), MaxConcurrent: pc.MaxConcurrent})
			a.Imagers[n] = images.New(images.Options{Name: pc.Name, BaseURL: pc.BaseURL, APIKey: a.apiKey(pc), MaxConcurrent: pc.MaxConcurrent})
		}
	}
	a.Events = a.Store
	a.Memory = memsqlite.New(a.Store.DB(), a.memoryOptions())
	return a, nil
}

func (a *App) Close() error {
	a.closeBrowser()
	if a.Store != nil {
		return a.Store.Close()
	}
	return nil
}

func (a *App) newProvider(pc config.Provider) (provider.Provider, error) {
	switch pc.Type {
	case "openai-compatible":
		return openaicompat.New(openaicompat.Options{
			Name:          pc.Name,
			BaseURL:       pc.BaseURL,
			APIKey:        a.apiKey(pc),
			MaxConcurrent: pc.MaxConcurrent,
			Sort:          pc.Sort,
		}), nil
	case "codex":
		if pc.Auth == "api-key" {
			return codex.NewAPIKey(pc.Name, a.Secrets), nil
		}
		p := codex.New(codex.Options{Name: pc.Name, Secrets: a.Secrets, Events: sinkProxy{a}, MaxConcurrent: pc.MaxConcurrent})
		a.Codex[pc.Name] = p
		return p, nil
	default:
		return nil, fmt.Errorf("provider %s: unknown type %q", pc.Name, pc.Type)
	}
}

// apiKey resolves a provider key on every request: an `env:VAR` reference in
// config.toml wins, otherwise auth.toml. Rotated secrets work without rewiring.
func (a *App) apiKey(pc config.Provider) func() string {
	if pc.APIKey != "" {
		return func() string {
			v, _ := config.ResolveEnv(pc.APIKey)
			return v
		}
	}
	return a.Secrets.Lookup(secrets.Providers, pc.Name, "api_key")
}

// ModelRef resolves a configured model name to its profile and provider.
func (a *App) ModelRef(name string) (*agent.ModelRef, error) {
	m, err := a.Config.Model(name)
	if err != nil {
		return nil, err
	}
	p, ok := a.Providers[m.Provider]
	if !ok {
		return nil, fmt.Errorf("model %s: provider %q is not available", name, m.Provider)
	}
	return &agent.ModelRef{Model: m, Provider: p}, nil
}

// Definition returns the agent definition with the built-in core fragment.
func (a *App) Definition(name string) (agent.Definition, error) {
	if _, ok := a.Config.Agents[name]; !ok {
		return agent.Definition{}, fmt.Errorf("agent %q is not defined", name)
	}
	def := a.Config.Definition(name)
	def.Core = agent.CorePrompt()
	return def, nil
}

// Executor returns the agent's tool set (nil when it has no tools): the
// registry's selection and, with Connectors set, the tools of the agent's
// connectors. The policy gate wraps this in the daemon.
func (a *App) Executor(agentName string) (tool.Executor, error) {
	entries, err := a.toolEntries(agentName)
	if err != nil {
		return nil, err
	}
	var sel, conn tool.Executor
	if len(entries) > 0 {
		if sel, err = a.Registry.Select(entries); err != nil {
			return nil, err
		}
	}
	if ids := a.Config.Agents[agentName].Connectors; a.Connectors != nil && len(ids) > 0 {
		conn = a.Connectors.Executor(ids)
	}
	log := a.Logger
	if log == nil {
		log = slog.Default()
	}
	return liveMerge(sel, conn, log)
}

// liveMerge joins the registry's selection and the connector executor,
// either of which may be nil. The daemon caches one runner per agent and
// the loop reads Specs every turn, so the join is redone on every Specs
// call: the next turn sees a refreshed connector list (docs/connectors.md
// §10), which tool.Merge's snapshot alone would freeze.
func liveMerge(sel, conn tool.Executor, log *slog.Logger) (tool.Executor, error) {
	switch {
	case conn == nil:
		return sel, nil
	case sel == nil:
		return conn, nil
	}
	if _, err := tool.Merge(sel, conn); err != nil {
		return nil, err
	}
	return &merged{sel: sel, conn: conn, log: log}, nil
}

// merged is liveMerge's executor. Read-only after construction but for
// the once, so safe for the many sessions of one runner.
type merged struct {
	sel, conn tool.Executor
	log       *slog.Logger
	once      sync.Once // logs the first Merge failure of Specs
}

// Specs is tool.Merge's, redone. A name offered twice cannot happen (a
// connector id never names a built-in group); should it, the registry's
// specs alone are offered, logged once, and Lookup and Execute still
// prefer the registry for a name both offer.
func (m *merged) Specs() []provider.ToolSpec {
	all, err := tool.Merge(m.sel, m.conn)
	if err != nil {
		m.once.Do(func() { m.log.Error("connector tools left out", "err", err) })
		return m.sel.Specs()
	}
	return all.Specs()
}

func (m *merged) Lookup(name string) (tool.Tool, bool) {
	if t, ok := m.sel.(tool.Lookuper).Lookup(name); ok {
		return t, true
	}
	return m.conn.(tool.Lookuper).Lookup(name)
}

// Execute runs a name only through the executor that can look it up, as
// tool.Merge does.
func (m *merged) Execute(ctx context.Context, c tool.Call) (tool.Result, error) {
	if _, ok := m.sel.(tool.Lookuper).Lookup(c.Name); ok {
		return m.sel.Execute(ctx, c)
	}
	if _, ok := m.conn.(tool.Lookuper).Lookup(c.Name); ok {
		return m.conn.Execute(ctx, c)
	}
	return tool.Result{Text: fmt.Sprintf("unknown tool %q", c.Name), IsError: true}, nil
}

// Runner builds the agent loop for one agent. exec may be nil.
func (a *App) Runner(agentName string, exec tool.Executor, sink event.Sink) (*agent.Runner, error) {
	def, err := a.Definition(agentName)
	if err != nil {
		return nil, err
	}
	roles := a.Config.Agents[agentName].Models
	chat, err := a.ModelRef(roles.Chat)
	if err != nil {
		return nil, err
	}
	scorer, err := a.TaintScorer(agentName)
	if err != nil {
		return nil, err
	}
	pol := a.Config.Agents[agentName].Policy
	r := &agent.Runner{
		Def: def, Model: chat.Model, Provider: chat.Provider,
		Tools: exec, Builder: agent.NewBuilder(), Transcript: a.Store, Events: sink,
		TaintDecay: pol.TaintDecayCalls, Taint: scorer, TaintThreshold: pol.TaintThreshold,
	}
	if roles.Fallback != "" {
		if r.Fallback, err = a.ModelRef(roles.Fallback); err != nil {
			return nil, err
		}
	}
	return r, nil
}

// memoryOptions wires the embedder of the first agent (by name) that
// configures an `embed` role; without one, memory is FTS only.
func (a *App) memoryOptions() memsqlite.Options {
	names := make([]string, 0, len(a.Config.Agents))
	for n := range a.Config.Agents {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		role := a.Config.Agents[n].Models.Embed
		if role == "" {
			continue
		}
		ref, err := a.ModelRef(role)
		if err != nil {
			continue
		}
		if emb, ok := ref.Provider.(provider.Embedder); ok {
			return memsqlite.Options{Embedder: emb, EmbedModel: ref.Model, Events: sinkProxy{a}}
		}
	}
	return memsqlite.Options{}
}
