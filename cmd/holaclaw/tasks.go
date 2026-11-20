package main

import (
	"context"
	"fmt"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"holaclaw/internal/api/gen"
)

// newTasksCmd is the tasks group, through the API (docs/tasks.md §7).
func newTasksCmd(c *cli) *cobra.Command {
	cmd := &cobra.Command{Use: "tasks", Short: "inspect the scheduled tasks"}

	list := &cobra.Command{Use: "list", Short: "list every task, newest first", Args: cobra.NoArgs}
	cn := c.connFlags(list)
	list.RunE = func(cmd *cobra.Command, _ []string) error { return cmdTasksList(cmd.Context(), c, cn) }

	cmd.AddCommand(list)
	return cmd
}

func cmdTasksList(ctx context.Context, c *cli, cn *conn) error {
	client, err := cn.dial(ctx)
	if err != nil {
		return err
	}
	tasks, err := client.ListTasks(ctx)
	if err != nil {
		return err
	}
	if len(tasks) == 0 {
		_, _ = fmt.Fprintln(c.stdout, "no tasks")
		return nil
	}
	w := tabwriter.NewWriter(c.stdout, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "ID\tAGENT\tNAME\tSCHEDULE\tSTATUS\tNEXT RUN\tLAST RUN\tPROMPT")
	for _, t := range tasks {
		name := deref(t.Name)
		if name == "" {
			name = "-"
		}
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", t.Id, t.Agent, name, taskSchedule(t),
			t.Status, stampOrDash(t.NextRun), stampOrDash(t.LastRun), oneLine(t.Prompt))
	}
	return w.Flush()
}

// taskSchedule is the schedule of a task on one line: the cron expression,
// the time of an `at`, or the kind alone.
func taskSchedule(t gen.Task) string {
	kind, _ := t.Schedule["kind"].(string)
	switch kind {
	case "cron":
		if expr, ok := t.Schedule["cron"].(string); ok {
			return expr
		}
	case "at":
		if s, ok := t.Schedule["at"].(string); ok {
			if at, err := time.Parse(time.RFC3339, s); err == nil {
				s = stamp(at)
			}
			return "at " + s
		}
	}
	return kind
}

func stampOrDash(t *time.Time) string {
	if t == nil {
		return "-"
	}
	return stamp(*t)
}
