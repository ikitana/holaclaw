package main

import (
	"context"
	"fmt"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"holaclaw/internal/api/gen"
)

// newMemoryCmd is the memory group, through the API. Agent and partition are
// mandatory in the subcommands: the store is default-deny across partitions
// (docs/memory.md). The browser needs neither — it walks them. It comes
// first because it is what `holaclaw memory` on its own does; the subcommands
// stay for scripts, which is the only reason they still take the flags they
// do.
func newMemoryCmd(c *cli) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "memory",
		Short: "browse the agent memories, or search, add and forget one",
		Long: "browse the agent memories: pick the agent, then the partition, then the memory\n\n" +
			"holaclaw memory search|add|forget [flags] is the same memory for scripts;\n" +
			"each of those needs --agent and --partition",
		// A word that is not one of the three is a mistake worth naming
		// rather than a browser nobody asked for.
		Args: subcommands,
	}
	cn := c.connFlags(cmd)
	cmd.RunE = func(cmd *cobra.Command, _ []string) error { return cmdMemoryBrowse(cmd.Context(), c, cn) }

	// search and add take free text: flags go first, and the words after
	// them are the text as it was typed.
	search := &cobra.Command{
		Use:   "search <query>",
		Short: "search one partition of an agent's memory",
	}
	search.Flags().SetInterspersed(false)
	searchConn := c.connFlags(search)
	var so memorySearchOptions
	search.Flags().StringVar(&so.agent, "agent", "main", "agent the memory belongs to")
	search.Flags().StringVar(&so.partition, "partition", "", "memory partition, e.g. dm/<user id>")
	search.Flags().StringVar(&so.layer, "layer", "", "note | recall")
	search.Flags().IntVar(&so.limit, "limit", 0, "maximum hits")
	required(search, "partition")
	search.RunE = func(cmd *cobra.Command, args []string) error {
		return cmdMemorySearch(cmd.Context(), c, searchConn, so, strings.Join(args, " "))
	}

	add := &cobra.Command{
		Use:   "add <text>",
		Short: "save a note in one partition of an agent's memory",
	}
	add.Flags().SetInterspersed(false)
	addConn := c.connFlags(add)
	in := gen.MemoryWrite{}
	var about, visibility string
	add.Flags().StringVar(&in.Agent, "agent", "main", "agent the note belongs to")
	add.Flags().StringVar(&in.Partition, "partition", "", "memory partition, e.g. dm/<user id>")
	add.Flags().StringVar(&about, "about", "", "person the note is about")
	add.Flags().StringVar(&visibility, "visibility", "", "shared | origin")
	required(add, "partition")
	add.RunE = func(cmd *cobra.Command, args []string) error {
		in.Text = strings.Join(args, " ")
		if in.Text == "" {
			return usage(cmd)
		}
		if about != "" {
			in.About = &about
		}
		if visibility != "" {
			v := gen.MemoryWriteVisibility(visibility)
			in.Visibility = &v
		}
		return cmdMemoryAdd(cmd.Context(), c, addConn, in)
	}

	forget := &cobra.Command{
		Use:   "forget <id>",
		Short: "forget one memory",
		Args:  cobra.ExactArgs(1),
	}
	forgetConn := c.connFlags(forget)
	forgetAgent := forget.Flags().String("agent", "main", "agent the memory belongs to")
	forgetPartition := forget.Flags().String("partition", "", "memory partition, e.g. dm/<user id>")
	required(forget, "partition")
	forget.RunE = func(cmd *cobra.Command, args []string) error {
		return cmdMemoryForget(cmd.Context(), c, forgetConn, *forgetAgent, *forgetPartition, args[0])
	}

	cmd.AddCommand(search, add, forget)
	return cmd
}

// memorySearchOptions are the flags of `memory search`.
type memorySearchOptions struct {
	agent, partition, layer string
	limit                   int
}

func cmdMemorySearch(ctx context.Context, c *cli, cn *conn, o memorySearchOptions, q string) error {
	client, err := cn.dial(ctx)
	if err != nil {
		return err
	}
	agent, partition, layer, limit := &o.agent, &o.partition, &o.layer, &o.limit
	params := gen.SearchMemoryParams{Agent: *agent, Partition: *partition}
	if q != "" {
		params.Q = &q
	}
	if *layer != "" {
		l := gen.SearchMemoryParamsLayer(*layer)
		params.Layer = &l
	}
	if *limit > 0 {
		params.Limit = limit
	}
	items, err := client.SearchMemory(ctx, params)
	if err != nil {
		return err
	}
	if len(items) == 0 {
		_, _ = fmt.Fprintln(c.stdout, "no matches")
		return nil
	}
	w := tabwriter.NewWriter(c.stdout, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "ID\tLAYER\tCREATED\tTEXT")
	for _, it := range items {
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", it.Id, it.Layer, stamp(it.CreatedAt), oneLine(it.Text))
	}
	return w.Flush()
}

func cmdMemoryAdd(ctx context.Context, c *cli, cn *conn, in gen.MemoryWrite) error {
	client, err := cn.dial(ctx)
	if err != nil {
		return err
	}
	item, err := client.WriteMemory(ctx, in)
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(c.stdout, "saved %s (%s)\n", item.Id, item.Visibility)
	return nil
}

func cmdMemoryForget(ctx context.Context, c *cli, cn *conn, agent, partition, id string) error {
	client, err := cn.dial(ctx)
	if err != nil {
		return err
	}
	if err := client.ForgetMemory(ctx, id, agent, partition); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(c.stdout, "forgot %s\n", id)
	return nil
}

// oneLine folds a note onto one line for a table row.
func oneLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	return clip(s, 100)
}
