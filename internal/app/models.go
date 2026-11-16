package app

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"holaclaw/internal/guard"
	"holaclaw/internal/provider"
	"holaclaw/internal/taint"
	"holaclaw/internal/tool"
	"holaclaw/internal/tool/tools"
)

// imageCounter is the daily usage counter of image.generate.
const imageCounter = "images"

// Guard builds the agent's guard from its `guard` role, or nil when the role
// is unset and policy alone decides. A decision model gets the decisions
// transport, any other model the chat backend (docs/models.md).
func (a *App) Guard(agentName string) (tool.Guard, error) {
	role := a.Config.Agents[agentName].Models.Guard
	if role == "" {
		return nil, nil
	}
	m, err := a.Config.Model(role)
	if err != nil {
		return nil, err
	}
	if m.API == provider.APIDecisions {
		d, ok := a.Deciders[m.Provider]
		if !ok {
			return nil, fmt.Errorf("model %s: provider %q does not serve decision models", role, m.Provider)
		}
		return guard.New(guard.Options{Model: m, Decider: d}), nil
	}
	ref, err := a.ModelRef(role)
	if err != nil {
		return nil, err
	}
	return guard.New(guard.Options{Model: ref.Model, Provider: ref.Provider}), nil
}

// TaintScorer builds the agent's taint scorer, or nil when no decision model
// is available and every untrusted result counts as 1 (docs/security.md).
//
// The `guard` role first: the scorer asks the same kind of narrow question of
// the same endpoint, and an agent that already pays for a guard should not
// need a second model. A chat guard cannot answer it, so a `guard` role that
// is not a decisions model falls through to `decide`, which the browser loop
// already requires to be one.
func (a *App) TaintScorer(agentName string) (taint.Scorer, error) {
	roles := a.Config.Agents[agentName].Models
	for _, role := range []string{roles.Guard, roles.Decide} {
		if role == "" {
			continue
		}
		m, err := a.Config.Model(role)
		if err != nil {
			return nil, err
		}
		if m.API != provider.APIDecisions {
			continue
		}
		d, ok := a.Deciders[m.Provider]
		if !ok {
			return nil, fmt.Errorf("model %s: provider %q does not serve decision models", role, m.Provider)
		}
		return taint.New(taint.Options{Model: m, Decider: d}), nil
	}
	return nil, nil
}

// toolEntries is the agent's tool list. Setting the `image` role is what turns
// image.generate on; listing the tool without the role is a mistake worth
// stopping for, not a tool that fails on every call. The skill tools are
// always in it.
func (a *App) toolEntries(agentName string) ([]string, error) {
	ac := a.Config.Agents[agentName]
	entries := slices.Clone(ac.Tools)
	listed := slices.Contains(entries, "image") || slices.Contains(entries, "image.generate")
	switch {
	case ac.Models.Image == "" && listed:
		return nil, fmt.Errorf("agent %s: tools lists image.generate but models.image is not set", agentName)
	case ac.Models.Image != "" && !listed:
		entries = append(entries, "image.generate")
	}
	// The skill tools need no config key: they join every tool set unless
	// the agent lists some of them itself, and a run whose index is empty
	// never sees skill.load (agent.VisibleTools). skill.save stays, so a
	// first skill can always be saved.
	if !slices.ContainsFunc(entries, func(e string) bool { return e == "skill" || strings.HasPrefix(e, "skill.") }) {
		entries = append(entries, "skill")
	}
	// react joins every tool set the same way; the loop shows it only on a
	// channel that takes reactions.
	if !slices.Contains(entries, tool.ReactTool) {
		entries = append(entries, tool.ReactTool)
	}
	return entries, nil
}

// imageBackend resolves the agent's `image` role for the image.generate tool
// and keeps its daily cap in the store.
type imageBackend struct{ a *App }

func (b imageBackend) GenerateImage(ctx context.Context, agentName, prompt string) (*provider.ImageResponse, string, error) {
	ac, ok := b.a.Config.Agents[agentName]
	if !ok || ac.Models.Image == "" {
		return nil, "", tools.ErrNoImageModel
	}
	m, err := b.a.Config.Model(ac.Models.Image)
	if err != nil {
		return nil, "", err
	}
	g, ok := b.a.Imagers[m.Provider]
	if !ok {
		return nil, m.Name, tools.ErrNoImageModel
	}
	resp, err := g.GenerateImage(ctx, provider.ImageRequest{Model: m, Prompt: prompt})
	return resp, m.Name, err
}

func (b imageBackend) TakeImage(ctx context.Context, agentName string) (bool, int, error) {
	limit := b.a.Config.Agents[agentName].Limits.ImagesPerDay
	if b.a.Store == nil {
		return false, limit, errStoreClosed
	}
	ok, err := b.a.Store.TakeDaily(ctx, agentName, imageCounter, time.Now(), limit)
	return ok, limit, err
}

func (b imageBackend) ReturnImage(ctx context.Context, agentName string) {
	if b.a.Store != nil {
		_ = b.a.Store.ReturnDaily(context.WithoutCancel(ctx), agentName, imageCounter, time.Now())
	}
}
