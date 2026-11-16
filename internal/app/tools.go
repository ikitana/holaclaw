package app

import (
	"context"
	"errors"
	"io"
	"os"
	"time"

	"holaclaw/internal/event"
	"holaclaw/internal/memory"
	"holaclaw/internal/tool"
	"holaclaw/internal/tool/tools"
)

// registerTools adds the compiled-in tools to the registry. The store and the
// memory backend open later, so tools reach them through small proxies.
func registerTools(a *App) error {
	if err := tools.Register(a.Registry, tools.Deps{Blobs: blobs{a}, ExecPath: execPath()}); err != nil {
		return err
	}
	if err := tools.RegisterMemory(a.Registry, lazyMemory{a}, sinkProxy{a}); err != nil {
		return err
	}
	if err := tools.RegisterTask(a.Registry, lazyTasks{a}); err != nil {
		return err
	}
	if err := tools.RegisterImage(a.Registry, imageBackend{a}, blobs{a}, sinkProxy{a}); err != nil {
		return err
	}
	// The skill tools are always registered and selected (toolEntries); the
	// loop hides skill.load from a run with no eligible skill.
	if err := tools.RegisterSkill(a.Registry, a, sinkProxy{a}); err != nil {
		return err
	}
	// react likewise; the loop shows it only on a channel that takes
	// reactions (tool.Call.CanReact).
	if err := tools.RegisterReact(a.Registry, lazyReacter{a}); err != nil {
		return err
	}
	// browser.do is always registered; the backend refuses every call with
	// ErrNoBrowser when config.toml has no [browser] table (docs/browser.md).
	return tools.RegisterBrowser(a.Registry, browserBackend{a}, blobs{a}, sinkProxy{a})
}

// execPath is the fixed PATH of exec children. NixOS has no /usr/bin, so its
// system profile is used there; nothing is inherited from the daemon's env.
func execPath() string {
	const nixos = "/run/current-system/sw/bin"
	if _, err := os.Stat(nixos); err == nil {
		return nixos
	}
	return tools.DefaultExecPath
}

// blobs adapts the store to the attach tool.
type blobs struct{ a *App }

func (b blobs) PutBlob(ctx context.Context, r io.Reader, mime string) (string, int64, error) {
	if b.a.Store == nil {
		return "", 0, errors.New("blob store is not open")
	}
	bl, err := b.a.Store.PutBlob(ctx, r, mime)
	return bl.ID, bl.Size, err
}

// sinkProxy forwards to the app's event sink once it exists.
type sinkProxy struct{ a *App }

func (s sinkProxy) Emit(ctx context.Context, e event.Event) {
	if s.a.Events != nil {
		s.a.Events.Emit(ctx, e)
	}
}

var (
	errMemoryClosed = errors.New("memory store is not open")
	errStoreClosed  = errors.New("store is not open")
)

// lazyMemory forwards to the memory backend once it is open.
type lazyMemory struct{ a *App }

func (l lazyMemory) Write(ctx context.Context, it memory.Item) (event.ID, error) {
	if l.a.Memory == nil {
		return "", errMemoryClosed
	}
	return l.a.Memory.Write(ctx, it)
}

func (l lazyMemory) Search(ctx context.Context, q memory.Query) ([]memory.Hit, error) {
	if l.a.Memory == nil {
		return nil, errMemoryClosed
	}
	return l.a.Memory.Search(ctx, q)
}

func (l lazyMemory) Get(ctx context.Context, agent, partition string, id event.ID) (memory.Item, error) {
	if l.a.Memory == nil {
		return memory.Item{}, errMemoryClosed
	}
	return l.a.Memory.Get(ctx, agent, partition, id)
}

func (l lazyMemory) Forget(ctx context.Context, agent, partition string, id event.ID) error {
	if l.a.Memory == nil {
		return errMemoryClosed
	}
	return l.a.Memory.Forget(ctx, agent, partition, id)
}

func (l lazyMemory) Reindex(ctx context.Context, agent string) error {
	if l.a.Memory == nil {
		return errMemoryClosed
	}
	return l.a.Memory.Reindex(ctx, agent)
}

func (l lazyMemory) Status(ctx context.Context, agent string) (memory.Status, error) {
	if l.a.Memory == nil {
		return memory.Status{}, errMemoryClosed
	}
	return l.a.Memory.Status(ctx, agent)
}

// Partitions forwards the optional lister, so a caller that type-asserts on
// this wrapper reaches the backend's own implementation rather than being
// told the API cannot list partitions.
func (l lazyMemory) Partitions(ctx context.Context, agent string) ([]memory.Partition, error) {
	if l.a.Memory == nil {
		return nil, errMemoryClosed
	}
	return l.a.Memory.Partitions(ctx, agent)
}

// Policy converts an agent's configured policy to the gate's form.
func (a *App) Policy(agentName string) tool.Policy {
	p := a.Config.Agents[agentName].Policy
	classes := func(c struct{ Read, Write, External, Exec string }) tool.Classes {
		return tool.Classes{
			Read: tool.Decision(c.Read), Write: tool.Decision(c.Write),
			External: tool.Decision(c.External), Exec: tool.Decision(c.Exec),
		}
	}
	out := tool.Policy{
		Default:         classes(struct{ Read, Write, External, Exec string }(p.Classes)),
		Guest:           classes(struct{ Read, Write, External, Exec string }(p.Guest)),
		Task:            classes(struct{ Read, Write, External, Exec string }(p.Task)),
		Tools:           map[string]tool.Decision{},
		ExecAllow:       p.ExecAllow,
		TaintEscalates:  p.TaintEscalates,
		GuardMode:       tool.GuardMode(p.GuardMode),
		GuardThreshold:  p.GuardThreshold,
		TaintDecayCalls: p.TaintDecayCalls,
	}
	for name, d := range p.Tools {
		out.Tools[name] = tool.Decision(d)
	}
	return out
}

// lazyTasks forwards task.create to the task service; only the daemon has
// one (a task needs a scheduler to ever run).
type lazyTasks struct{ a *App }

func (l lazyTasks) CreateFromTool(ctx context.Context, c tool.Call, name, prompt, cron string, at time.Time) (string, time.Time, error) {
	if l.a.Tasks == nil {
		return "", time.Time{}, errors.New("tasks need the daemon: start it with `holaclaw run`")
	}
	return l.a.Tasks.CreateFromTool(ctx, c, name, prompt, cron, at)
}

// lazyReacter forwards react to the daemon's channels.
type lazyReacter struct{ a *App }

func (l lazyReacter) React(ctx context.Context, c tool.Call, emoji string) error {
	if l.a.Reacter == nil {
		return errors.New("reactions need the daemon's channels")
	}
	return l.a.Reacter.React(ctx, c, emoji)
}

// NewRegistry returns the compiled-in tool set without any backing state, for
// commands that only need names and schemas (`check`, `prompt-size`). It is
// the same registration the daemon uses, so the two can never disagree.
func NewRegistry() (*tool.Registry, error) {
	a := &App{Registry: tool.NewRegistry()}
	if err := registerTools(a); err != nil {
		return nil, err
	}
	return a.Registry, nil
}
