package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"holaclaw/internal/agent"
	"holaclaw/internal/api/gen"
	"holaclaw/internal/app"
	"holaclaw/internal/check"
	"holaclaw/internal/connector"
	"holaclaw/internal/event"
	"holaclaw/internal/provider"
	"holaclaw/internal/tool"
)

func newPromptSizeCmd(c *cli) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "prompt-size",
		Short: "offline token breakdown of an agent's prompt",
		Args:  cobra.NoArgs,
	}
	agentName := cmd.Flags().String("agent", "main", "agent name")
	cmd.RunE = func(cmd *cobra.Command, _ []string) error { return cmdPromptSize(cmd.Context(), c, agentName) }
	return cmd
}

// cmdPromptSize prints the offline token breakdown per fragment and tool: the
// same numbers context.built records at runtime.
func cmdPromptSize(ctx context.Context, c *cli, agentName *string) error {
	a, err := app.Static(c.configDir)
	if err != nil {
		return err
	}
	def, err := a.Definition(*agentName)
	if err != nil {
		return err
	}
	model, err := a.Config.Model(a.Config.Agents[*agentName].Models.Chat)
	if err != nil {
		return err
	}
	def.Skills = a.PromptSkills(*agentName)
	in := agent.BuildInput{Def: def, Model: model}
	var specs []provider.ToolSpec
	if exec, err := a.Executor(*agentName); err != nil {
		return err
	} else if exec != nil {
		specs = exec.Specs()
	}
	// No daemon here: the connector tools are the cached lists.
	rows := cachedConnectors(ctx, c, a.Config, a.DataDir)
	specs = append(specs, connector.CachedSpecs(rows, def.Connectors)...)
	in.Tools = agent.VisibleTools(specs, def.Skills, tool.Call{Agent: *agentName, Role: tool.RoleOwner, RunKind: tool.RunInteractive})
	p, err := agent.NewBuilder().Build(ctx, in)
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(c.stdout, "agent %s · model %s (%s)\n\n", *agentName, model.Name, model.ID)
	agent.FormatBreakdown(c.stdout, p.Breakdown)
	if f, ok := check.ConnectorFinding(*agentName, a.Config.Agents[*agentName].Models.Chat, p.Breakdown); ok {
		_, _ = fmt.Fprintf(c.stdout, "\n%s\n", f)
	}
	return nil
}

// newTraceCmd shows one trace. With no id it opens the browser instead: the
// id is usually the thing that is missing, and walking the sessions is how it
// is found.
func newTraceCmd(c *cli) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "trace [<trace-id>]",
		Short: "browse traces, or show every hop of one",
		Args:  cobra.MaximumNArgs(1),
	}
	cn := c.connFlags(cmd)
	asJSON := cmd.Flags().Bool("json", false, "print the trace as JSON")
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		if len(args) == 0 {
			return cmdTraceBrowse(cmd.Context(), c, cn)
		}
		return cmdTrace(cmd.Context(), c, cn, args[0], *asJSON)
	}
	return cmd
}

// cmdTrace prints every event of one trace joined with its messages. It asks
// the daemon when one is running — that is the live view, including events
// still being written — and falls back to opening the store directly.
func cmdTrace(ctx context.Context, c *cli, cn *conn, id string, asJSON bool) error {
	client, err := cn.dial(ctx)
	switch {
	case err == nil:
		tr, err := client.GetTrace(ctx, id)
		if err != nil {
			return err
		}
		return writeTrace(c.stdout, tr, asJSON)
	case errors.Is(err, errNoDaemon):
		return traceFromStore(ctx, c, *cn.dir, event.ID(id), asJSON)
	default:
		return err
	}
}

// traceFromStore is the offline path: open the store read-only and join the
// events with their messages exactly as the API does.
func traceFromStore(ctx context.Context, c *cli, dir string, id event.ID, asJSON bool) error {
	a, err := app.Open(ctx, dir)
	if err != nil {
		return err
	}
	defer func() { _ = a.Close() }()

	events, err := a.Store.ListEvents(ctx, id)
	if err != nil {
		return err
	}
	msgs, err := a.Store.ListTraceMessages(ctx, id)
	if err != nil {
		return err
	}
	if len(events) == 0 && len(msgs) == 0 {
		return fmt.Errorf("no events for trace %s", id)
	}
	tr := gen.Trace{TraceId: string(id)}
	for _, e := range events {
		tr.Events = append(tr.Events, storeEvent(e))
	}
	for _, m := range msgs {
		tr.Messages = append(tr.Messages, storeMessage(m))
	}
	return writeTrace(c.stdout, tr, asJSON)
}

// writeTrace renders one trace: one line per hop, with the message it refers
// to underneath.
func writeTrace(w io.Writer, tr gen.Trace, asJSON bool) error {
	if asJSON {
		return writeJSON(w, tr)
	}
	if len(tr.Events) == 0 {
		return fmt.Errorf("no events for trace %s", tr.TraceId)
	}
	byID := map[string]gen.Message{}
	for _, m := range tr.Messages {
		byID[m.Id] = m
	}
	start := tr.Events[0].Time
	for _, e := range tr.Events {
		line := fmt.Sprintf("%8.3fs  %-18s", e.Time.Sub(start).Seconds(), e.Type)
		if e.DurationNs != nil && *e.DurationNs > 0 {
			line += fmt.Sprintf(" (%s)", time.Duration(*e.DurationNs).Round(time.Millisecond))
		}
		if e.Attrs != nil && len(*e.Attrs) > 0 {
			line += " " + clip(compactJSON(*e.Attrs), 160)
		}
		if _, err := fmt.Fprintln(w, line); err != nil {
			return err
		}
		if e.MessageId == nil {
			continue
		}
		if m, ok := byID[*e.MessageId]; ok && m.Text != nil && *m.Text != "" {
			_, _ = fmt.Fprintf(w, "%12s%s: %s\n", "", m.Role, clip(strings.ReplaceAll(*m.Text, "\n", " "), 140))
		}
	}
	return nil
}

func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

func newModelsCmd(c *cli) *cobra.Command {
	return &cobra.Command{
		Use:   "models <provider>",
		Short: "list the model ids a provider serves",
		Args:  cobra.ExactArgs(1),
		RunE:  func(cmd *cobra.Command, args []string) error { return cmdModels(cmd.Context(), c, args[0]) },
	}
}

// cmdModels prints the model ids a provider serves for its credentials, one
// per line: the values a `models.<name>.id` can take, and the list MOD003
// checks against. Listing only reads the credentials, which the running
// daemon owns.
func cmdModels(ctx context.Context, c *cli, name string) error {
	a, err := app.Static(c.configDir)
	if err != nil {
		return err
	}
	pc, ok := a.Config.Providers[name]
	if !ok {
		return fmt.Errorf("unknown provider %q", name)
	}
	p, err := app.NewProvider(pc, a.Secrets, nil)
	if err != nil {
		return err
	}
	lister, ok := p.(provider.ModelLister)
	if !ok {
		return fmt.Errorf("provider %s cannot list its models", name)
	}
	ctx, cancel := context.WithTimeout(ctx, check.ProbeTimeout)
	defer cancel()
	ids, err := lister.Models(ctx)
	if err != nil {
		return err
	}
	sort.Strings(ids)
	for _, id := range ids {
		_, _ = fmt.Fprintln(c.stdout, id)
	}
	return nil
}
