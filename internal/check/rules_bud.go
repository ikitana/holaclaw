package check

import (
	"context"
	"errors"
	"fmt"
	"path"
	"strings"

	agentlib "holaclaw/internal/agent"
	"holaclaw/internal/config"
	"holaclaw/internal/provider"
)

// MaxFixedOverheadTokens is the warning threshold for BUD001: System +
// ToolSchemas above this is worth a look even if it still fits comfortably.
const MaxFixedOverheadTokens = 1500

// MaxFixedOverheadWindowPct is the error threshold: fixed overhead eating
// more than a quarter of the chat model's window leaves too little for an
// actual conversation.
const MaxFixedOverheadWindowNum, MaxFixedOverheadWindowDen = 1, 4

// MaxConnectorWindowNum/Den is the BUD002 threshold: connector tools over a
// quarter of the chat model's window. A warning only: granting them is
// allowed, and the builder's BudgetError stays the runtime guard.
const MaxConnectorWindowNum, MaxConnectorWindowDen = 1, 4

// bud001Rule builds each agent's prompt offline (no history, no injections)
// and flags its fixed overhead — the part of the prompt every turn pays no
// matter how short the conversation (docs/context.md). Connector tools are
// not part of it: BUD002 reports them.
func bud001Rule() Rule {
	return Rule{
		ID:      "BUD001",
		Summary: "agent prompt fixed overhead is too large",
		Run:     budRun(bud001Check),
	}
}

// bud002Rule flags an agent whose connector tools take more than a quarter
// of its chat model's window (docs/connectors.md §5).
func bud002Rule() Rule {
	return Rule{
		ID:      "BUD002",
		Summary: "agent connector tools take a large share of the window",
		Run:     budRun(bud002Check),
	}
}

// budRun runs fn over every agent, in name order.
func budRun(fn func(context.Context, *Input, string) (Finding, bool)) func(context.Context, *Input) []Finding {
	return func(ctx context.Context, in *Input) []Finding {
		if in.Config == nil {
			return nil
		}
		var out []Finding
		for _, name := range sortedKeys(in.Config.Agents) {
			if f, ok := fn(ctx, in, name); ok {
				out = append(out, f)
			}
		}
		return out
	}
}

// budBuild builds agent name's prompt offline with the tool schemas it
// would get. ok is false when the chat model is missing or dangling: a CFG
// finding already.
func budBuild(ctx context.Context, in *Input, name string) (p agentlib.Prompt, model provider.Model, ok bool, err error) {
	a := in.Config.Agents[name]
	if a.Models.Chat == "" {
		return p, model, false, nil // required key missing: already CFG005
	}
	model, err = in.Config.Model(a.Models.Chat)
	if err != nil {
		return p, model, false, nil // dangling ref: already CFG006
	}
	var toolSpecs []provider.ToolSpec
	if in.Specs != nil {
		toolSpecs = in.Specs(name)
	}
	def := agentlib.Definition{
		Name:         name,
		Core:         agentlib.CorePrompt(),
		Persona:      a.Prompt,
		Tools:        append([]string(nil), a.Tools...),
		Connectors:   GrantedConnectors(in.KnownTools, a.Connectors),
		MaxSteps:     a.Limits.MaxSteps,
		ReplyReserve: a.Limits.ReplyReserve,
	}
	p, err = agentlib.NewBuilder().Build(ctx, agentlib.BuildInput{Def: def, Model: model, Tools: toolSpecs})
	return p, model, true, err
}

// GrantedConnectors is ids without any that is a built-in tool or group
// name in known: such an id is a config error (CFG), and counting its
// tools as connector tools would let them past the schema cap.
func GrantedConnectors(known, ids []string) []string {
	builtin := map[string]bool{}
	for _, t := range known {
		builtin[t] = true
		if g, _, ok := strings.Cut(t, "."); ok {
			builtin[g] = true
		}
	}
	var out []string
	for _, id := range ids {
		if !builtin[id] {
			out = append(out, id)
		}
	}
	return out
}

func bud001Check(ctx context.Context, in *Input, name string) (Finding, bool) {
	a := in.Config.Agents[name]
	file := path.Join("agents", name, config.FileAgent)
	p, model, ok, err := budBuild(ctx, in, name)
	if !ok {
		return Finding{}, false
	}
	if err != nil {
		var be *agentlib.BudgetError
		if errors.As(err, &be) {
			return Finding{
				ID:       "BUD001",
				Severity: SeverityError,
				File:     file,
				Key:      "prompt",
				Message:  be.Error(),
				Fix:      "shorten the persona, or drop tools, until the prompt fits",
			}, true
		}
		return Finding{}, false
	}

	fixed := p.Breakdown.System + p.Breakdown.ToolSchemas
	window := model.Context
	if window > 0 && fixed*MaxFixedOverheadWindowDen > window*MaxFixedOverheadWindowNum {
		return Finding{
			ID:       "BUD001",
			Severity: SeverityError,
			File:     file,
			Key:      "prompt",
			Message: fmt.Sprintf("fixed overhead %d tokens is over %d/%d of the %s window (%d)",
				fixed, MaxFixedOverheadWindowNum, MaxFixedOverheadWindowDen, a.Models.Chat, window),
			Fix: "drop tools or shorten the persona in agents/" + name + "/" + config.FilePrompt,
		}, true
	}
	if fixed > MaxFixedOverheadTokens {
		return Finding{
			ID:       "BUD001",
			Severity: SeverityWarning,
			File:     file,
			Key:      "prompt",
			Message:  fmt.Sprintf("fixed overhead is %d tokens, over the %d target", fixed, MaxFixedOverheadTokens),
			Fix:      "drop tools or shorten the persona in agents/" + name + "/" + config.FilePrompt,
		}, true
	}
	return Finding{}, false
}

func bud002Check(ctx context.Context, in *Input, name string) (Finding, bool) {
	p, _, ok, err := budBuild(ctx, in, name)
	if !ok || err != nil {
		return Finding{}, false // a failed build is BUD001's
	}
	return ConnectorFinding(name, in.Config.Agents[name].Models.Chat, p.Breakdown)
}

// ConnectorFinding is BUD002 for one built prompt of agent name on the chat
// model named model: `check` and `prompt-size` report the same words.
func ConnectorFinding(name, model string, b agentlib.Breakdown) (Finding, bool) {
	if b.Window <= 0 || b.Connectors*MaxConnectorWindowDen <= b.Window*MaxConnectorWindowNum {
		return Finding{}, false
	}
	return Finding{
		ID:       "BUD002",
		Severity: SeverityWarning,
		File:     path.Join("agents", name, config.FileAgent),
		Key:      "connectors",
		Message: fmt.Sprintf("connector tools are %d tokens, over %d/%d of the %s window (%d)",
			b.Connectors, MaxConnectorWindowNum, MaxConnectorWindowDen, model, b.Window),
		Fix: "grant fewer connectors, or use a model with a larger window",
	}, true
}
